package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/clienterror"
	"github.com/therealtinhtute/llmhub/internal/interfaces"
	requestlogging "github.com/therealtinhtute/llmhub/internal/logging"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsRequestTypeCreate  = "response.create"
	wsRequestTypeAppend  = "response.append"
	wsEventTypeError     = "error"
	wsEventTypeCompleted = "response.completed"
	wsDoneMarker         = "[DONE]"
	wsTurnStateHeader    = "x-codex-turn-state"
	wsTimelineBodyKey    = "WEBSOCKET_TIMELINE_OVERRIDE"
	// wsHTTPReplayRequiredCloseReason mirrors the upstream close reason sent when
	// a duplex continuation must be replayed over HTTP (upstream 42c9680eee55).
	wsHTTPReplayRequiredCloseReason = "upstream requires HTTP replay"
)

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type websocketTimelineAppender interface {
	Append(eventType string, payload []byte, timestamp time.Time)
}

type responsesWebsocketToolCacheTransaction struct {
	sessionKey     string
	outputCache    *websocketToolOutputCache
	callCache      *websocketToolOutputCache
	outputBaseline map[string]json.RawMessage
	callBaseline   map[string]json.RawMessage
}

func beginResponsesWebsocketToolCacheTransaction(sessionKey string) *responsesWebsocketToolCacheTransaction {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return nil
	}
	outputCache, outputBaseline := cloneResponsesWebsocketToolCacheSession(defaultWebsocketToolOutputCache, sessionKey)
	callCache, callBaseline := cloneResponsesWebsocketToolCacheSession(defaultWebsocketToolCallCache, sessionKey)
	return &responsesWebsocketToolCacheTransaction{
		sessionKey:     sessionKey,
		outputCache:    outputCache,
		callCache:      callCache,
		outputBaseline: outputBaseline,
		callBaseline:   callBaseline,
	}
}

func cloneResponsesWebsocketToolCacheSession(source *websocketToolOutputCache, sessionKey string) (*websocketToolOutputCache, map[string]json.RawMessage) {
	if source == nil {
		return nil, nil
	}
	local := newWebsocketToolOutputCache(source.ttl, source.maxPerSession)
	baseline := make(map[string]json.RawMessage)
	source.mu.Lock()
	source.cleanupLocked(time.Now())
	if session := source.sessions[sessionKey]; session != nil {
		cloned := &websocketToolOutputSession{
			lastSeen: session.lastSeen,
			outputs:  make(map[string]json.RawMessage, len(session.outputs)),
			order:    append([]string(nil), session.order...),
		}
		for callID, item := range session.outputs {
			clonedItem := append(json.RawMessage(nil), item...)
			cloned.outputs[callID] = clonedItem
			baseline[callID] = append(json.RawMessage(nil), item...)
		}
		local.sessions[sessionKey] = cloned
	}
	source.mu.Unlock()
	return local, baseline
}

func commitResponsesWebsocketToolCacheChanges(target, local *websocketToolOutputCache, sessionKey string, baseline map[string]json.RawMessage) {
	if target == nil || local == nil || sessionKey == "" {
		return
	}
	type cacheEntry struct {
		callID string
		item   json.RawMessage
	}
	var changed []cacheEntry
	local.mu.Lock()
	if session := local.sessions[sessionKey]; session != nil {
		changed = make([]cacheEntry, 0, len(session.order))
		for _, callID := range session.order {
			item := session.outputs[callID]
			if bytes.Equal(baseline[callID], item) {
				continue
			}
			changed = append(changed, cacheEntry{callID: callID, item: append(json.RawMessage(nil), item...)})
		}
	}
	local.mu.Unlock()
	for _, entry := range changed {
		target.record(sessionKey, entry.callID, entry.item)
	}
	if len(baseline) > 0 {
		target.mu.Lock()
		if session := target.sessions[sessionKey]; session != nil {
			session.lastSeen = time.Now()
		}
		target.mu.Unlock()
	}
}

func (t *responsesWebsocketToolCacheTransaction) repair(payload []byte) []byte {
	if t == nil {
		return repairResponsesWebsocketToolCalls("", payload)
	}
	return repairResponsesWebsocketToolCallsWithCaches(t.outputCache, t.callCache, t.sessionKey, payload)
}

func (t *responsesWebsocketToolCacheTransaction) record(payload []byte) {
	if t == nil {
		return
	}
	recordResponsesWebsocketToolCallsFromPayloadWithCache(t.callCache, t.sessionKey, payload)
}

func (t *responsesWebsocketToolCacheTransaction) commit() {
	if t == nil {
		return
	}
	commitResponsesWebsocketToolCacheChanges(defaultWebsocketToolOutputCache, t.outputCache, t.sessionKey, t.outputBaseline)
	commitResponsesWebsocketToolCacheChanges(defaultWebsocketToolCallCache, t.callCache, t.sessionKey, t.callBaseline)
}

type websocketTimelineLog struct {
	enabled bool
	source  *requestlogging.FileBodySource
	builder *strings.Builder

	currentPart       io.WriteCloser
	currentPartHasLog bool
}

func newWebsocketTimelineLog(enabled bool, source *requestlogging.FileBodySource) *websocketTimelineLog {
	if !enabled {
		return &websocketTimelineLog{}
	}
	if source == nil {
		return newInMemoryWebsocketTimelineLog()
	}
	return &websocketTimelineLog{
		enabled: true,
		source:  source,
	}
}

func newInMemoryWebsocketTimelineLog() *websocketTimelineLog {
	return &websocketTimelineLog{
		enabled: true,
		builder: &strings.Builder{},
	}
}

func websocketTimelineSourceFromContext(c *gin.Context) *requestlogging.FileBodySource {
	if c == nil {
		return nil
	}
	value, exists := c.Get(requestlogging.WebsocketTimelineSourceContextKey)
	if !exists {
		return nil
	}
	source, ok := value.(*requestlogging.FileBodySource)
	if !ok {
		return nil
	}
	return source
}

func (l *websocketTimelineLog) BeginRequest() {
	if l == nil || !l.enabled || l.source == nil {
		return
	}
	l.closeCurrentPart()
	part, errCreate := l.source.CreatePart("request")
	if errCreate != nil {
		log.WithError(errCreate).Warn("failed to create websocket request detail log")
		return
	}
	l.currentPart = part
	l.currentPartHasLog = false
}

func (l *websocketTimelineLog) Append(eventType string, payload []byte, timestamp time.Time) {
	if l == nil || !l.enabled {
		return
	}
	data := formatWebsocketTimelineEvent(eventType, payload, timestamp)
	if len(data) == 0 {
		return
	}
	if l.source != nil {
		if l.currentPart == nil {
			l.BeginRequest()
		}
		if l.currentPart == nil {
			return
		}
		if errWrite := writeWebsocketTimelinePart(l.currentPart, data, l.currentPartHasLog); errWrite != nil {
			log.WithError(errWrite).Warn("failed to write websocket request detail log")
			return
		}
		l.currentPartHasLog = true
		return
	}
	if l.builder != nil {
		writeWebsocketTimelineBuilder(l.builder, data)
	}
}

func (l *websocketTimelineLog) SetContext(c *gin.Context) {
	if l == nil || !l.enabled {
		return
	}
	l.closeCurrentPart()
	if l.source != nil {
		if l.source.HasPayload() {
			c.Set(requestlogging.WebsocketTimelineSourceContextKey, l.source)
			return
		}
		if errCleanup := l.source.Cleanup(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up empty websocket timeline log parts")
		}
	}
	if l.builder != nil {
		setWebsocketTimelineBody(c, l.builder.String())
	}
}

func (l *websocketTimelineLog) String() string {
	if l == nil || !l.enabled {
		return ""
	}
	l.closeCurrentPart()
	if l.source != nil {
		data, errRead := l.source.Bytes()
		if errRead != nil {
			return ""
		}
		return string(data)
	}
	if l.builder == nil {
		return ""
	}
	return l.builder.String()
}

func (l *websocketTimelineLog) closeCurrentPart() {
	if l == nil || l.currentPart == nil {
		return
	}
	if errClose := l.currentPart.Close(); errClose != nil {
		log.WithError(errClose).Warn("failed to close websocket request detail log")
	}
	l.currentPart = nil
	l.currentPartHasLog = false
}

func writeWebsocketTimelinePart(w io.Writer, data []byte, prependNewline bool) error {
	if w == nil || len(data) == 0 {
		return nil
	}
	if prependNewline {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	_, errWrite := w.Write(data)
	return errWrite
}

func writeWebsocketTimelineBuilder(builder *strings.Builder, data []byte) {
	if builder == nil || len(data) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.Write(data)
}

// cloneWebsocketTurnAuthRequest clones the upgrade request into a plain POST
// carrying the turn payload, with the websocket handshake headers stripped, so
// access providers evaluate the same request shape they see on the HTTP path.
func cloneWebsocketTurnAuthRequest(baseReq *http.Request, body []byte) *http.Request {
	req := baseReq.Clone(baseReq.Context())
	req.Method = http.MethodPost
	for key := range req.Header {
		lower := strings.ToLower(key)
		if lower == "upgrade" || lower == "connection" || lower == "content-length" || strings.HasPrefix(lower, "sec-websocket-") {
			delete(req.Header, key)
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return req
}

// authenticateResponsesWebsocketTurn re-runs the frontend access providers for
// one downstream turn on an already upgraded socket. A revoked or denied key
// therefore rejects the turn instead of retaining the handshake decision for
// the lifetime of the socket. An empty turn model inherits requestModelName so
// a provider that gates on the model cannot be bypassed by omitting it
// (upstream CLIProxyAPI commit b9bb031dcbb0).
func (h *OpenAIResponsesAPIHandler) authenticateResponsesWebsocketTurn(c *gin.Context, baseReq *http.Request, payload []byte, requestModelName string) *interfaces.ErrorMessage {
	if h == nil || h.AccessManager == nil {
		return nil
	}
	sourceReq := baseReq
	if sourceReq == nil && c != nil {
		sourceReq = c.Request
	}
	if sourceReq == nil {
		return nil
	}
	authBody := payload
	if requestModelName != "" && json.Valid(payload) && strings.TrimSpace(gjson.GetBytes(payload, "model").String()) == "" {
		if updated, errSet := sjson.SetBytes(bytes.Clone(payload), "model", requestModelName); errSet == nil {
			authBody = updated
		}
	}
	result, authErr := h.AccessManager.Authenticate(sourceReq.Context(), cloneWebsocketTurnAuthRequest(sourceReq, authBody))
	if authErr != nil {
		message := strings.TrimSpace(authErr.Message)
		if message == "" {
			message = authErr.Error()
		}
		return &interfaces.ErrorMessage{
			StatusCode: authErr.HTTPStatusCode(),
			Error:      errors.New(message),
		}
	}
	if result != nil && c != nil {
		// The last turn's principal wins: usage and downstream scope for this
		// turn must describe the credential that actually authorized it.
		c.Set("userApiKey", result.Principal)
		c.Set("accessProvider", result.Provider)
		if len(result.Metadata) > 0 {
			c.Set("accessMetadata", result.Metadata)
		} else {
			c.Set("accessMetadata", nil)
		}
	}
	return nil
}

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	// Per-turn reauthorization replays the handshake's credential material, not
	// the socket's framed input, so the upgrade request is kept as the base.
	upgradeRequest := c.Request.Clone(c.Request.Context())
	// In duplex mode a single goroutine owns conn.ReadMessage and forwards frames
	// through a bounded channel so the executor can keep consuming client input
	// while an upstream response is still streaming (upstream CLIProxyAPI
	// commit 42c9680eee55).
	var duplexInput <-chan cliproxyexecutor.WebsocketInput
	if h != nil && h.Cfg != nil && h.Cfg.CodexResponseSteering {
		socketCtx, cancelSocket := context.WithCancel(c.Request.Context())
		defer cancelSocket()
		c.Request = c.Request.WithContext(socketCtx)
		duplexInput = readResponsesWebsocketInput(socketCtx, cancelSocket, conn)
	}
	passthroughSessionID := uuid.NewString()
	downstreamSessionKey := websocketDownstreamSessionKey(c.Request)
	retainResponsesWebsocketToolCaches(downstreamSessionKey)
	clientIP := websocketClientAddress(c)
	log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientIP)

	requestLogEnabled := h != nil && h.Cfg != nil && h.Cfg.RequestLog
	wsTimelineLog := newWebsocketTimelineLog(requestLogEnabled, websocketTimelineSourceFromContext(c))

	wsDone := make(chan struct{})
	defer close(wsDone)

	// wsWriteMu serializes writes so the disconnect goroutine below can emit a
	// terminal error frame without racing the main goroutine's writes
	// (upstream responsesWebsocketWriter.writeMu equivalent).
	var wsWriteMu sync.Mutex
	// wsForwardedErr records the last upstream failure already delivered as an
	// error frame so the disconnect path below does not write it twice
	// (upstream responsesWebsocketWriter.closing dedupe, commit 42c9680eee55).
	var wsForwardedErr atomic.Value

	// In duplex mode the executor owns the socket until its ordered event stream
	// ends; an out-of-band disconnect close could discard an already received
	// steering acknowledgement before it reaches the client (upstream 42c9680eee55).
	if h != nil && h.AuthManager != nil && duplexInput == nil {
		if exec, ok := h.AuthManager.Executor("codex"); ok && exec != nil {
			type upstreamDisconnectSubscriber interface {
				UpstreamDisconnectChan(sessionID string) <-chan error
			}
			if subscriber, ok := exec.(upstreamDisconnectSubscriber); ok && subscriber != nil {
				disconnectCh := subscriber.UpstreamDisconnectChan(passthroughSessionID)
				if disconnectCh != nil {
					go func() {
						select {
						case <-wsDone:
							return
						case disconnectErr := <-disconnectCh:
							if isRequestScopedResponsesWebsocketError(disconnectErr) {
								return
							}
							// Upstream closeForUpstreamDisconnect: client-facing
							// faults are exposed as an error event before the
							// socket closes; anything else closes silently
							// (upstream shouldExposeResponsesUpstreamError).
							status := clienterror.HTTPStatusFromError(disconnectErr)
							if status <= 0 {
								status = http.StatusInternalServerError
							}
							errMsg := &interfaces.ErrorMessage{StatusCode: status, Error: disconnectErr}
							unlockWebsocketWriter := lockWebsocketWriter(&wsWriteMu)
							alreadyForwarded := false
							if forwarded, ok := wsForwardedErr.Load().(*interfaces.ErrorMessage); ok && forwarded != nil && forwarded.Error != nil && disconnectErr != nil {
								alreadyForwarded = forwarded.Error.Error() == disconnectErr.Error()
							}
							if !alreadyForwarded && shouldExposeResponsesUpstreamError(errMsg) {
								_, _ = writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
							}
							// Hold the write lock across Close so no in-flight
							// frame write can interleave with the teardown.
							_ = conn.Close()
							unlockWebsocketWriter()
						}
					}()
				}
			}
		}
	}

	var wsTerminateErr error
	defer func() {
		releaseResponsesWebsocketToolCaches(downstreamSessionKey)
		if wsTerminateErr != nil {
			appendWebsocketTimelineDisconnect(wsTimelineLog, wsTerminateErr, time.Now())
			// log.Infof("responses websocket: session closing id=%s reason=%v", passthroughSessionID, wsTerminateErr)
		} else {
			log.Infof("responses websocket: session closing id=%s", passthroughSessionID)
		}
		if h != nil && h.AuthManager != nil {
			h.AuthManager.CloseExecutionSession(passthroughSessionID)
			log.Infof("responses websocket: upstream execution session closed id=%s", passthroughSessionID)
		}
		wsTimelineLog.SetContext(c)
		if errClose := conn.Close(); errClose != nil {
			log.Warnf("responses websocket: close connection error: %v", errClose)
		}
	}()

	var lastRequest []byte
	lastResponseOutput := []byte("[]")
	var observedCompaction responsesWebsocketObservedCompactionState
	// Remains pending until a generating request commits successfully.
	pendingPrewarmID := ""
	pinnedAuthID := ""
	sessionAuthByID := func(authID string) (*coreauth.Auth, bool) {
		if h == nil || h.AuthManager == nil {
			return nil, false
		}
		if auth, ok := h.AuthManager.GetExecutionSessionAuthByID(passthroughSessionID, authID); ok {
			return auth, true
		}
		return h.AuthManager.GetByID(authID)
	}
	forceTranscriptReplayNextRequest := false

	for {
		var msgType int
		var payload []byte
		var errReadMessage error
		if duplexInput == nil {
			msgType, payload, errReadMessage = conn.ReadMessage()
		} else {
			select {
			case message, ok := <-duplexInput:
				if !ok {
					return
				}
				msgType, payload, errReadMessage = websocket.TextMessage, message.Payload, message.Err
			case <-c.Request.Context().Done():
				return
			}
		}
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			if websocket.IsCloseError(errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				log.Infof("responses websocket: client disconnected id=%s error=%v", passthroughSessionID, errReadMessage)
			} else {
				// log.Warnf("responses websocket: read message failed id=%s error=%v", passthroughSessionID, errReadMessage)
			}
			return
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		// log.Infof(
		// 	"responses websocket: downstream_in id=%s type=%d event=%s payload=%s",
		// 	passthroughSessionID,
		// 	msgType,
		// 	websocketPayloadEventType(payload),
		// 	websocketPayloadPreview(payload),
		// )
		wsTimelineLog.BeginRequest()
		wsTimelineLog.Append("request", payload, time.Now())

		allowIncrementalInputWithPreviousResponseID := false
		if pinnedAuthID != "" {
			if pinnedAuth, ok := sessionAuthByID(pinnedAuthID); ok && pinnedAuth != nil {
				allowIncrementalInputWithPreviousResponseID = websocketUpstreamSupportsIncrementalInput(pinnedAuth.Attributes, pinnedAuth.Metadata)
			}
		} else {
			requestModelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
			if requestModelName == "" {
				requestModelName = strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
			}
			allowIncrementalInputWithPreviousResponseID = h.websocketUpstreamSupportsIncrementalInputForModel(requestModelName)
		}
		if forceTranscriptReplayNextRequest {
			allowIncrementalInputWithPreviousResponseID = false
		}

		// A completed compaction response is direct evidence that the auth that
		// produced it supports compaction replay (upstream observed-compaction
		// state; plugin-executor/provider routes do not exist in this tree).
		requestModelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
		if requestModelName == "" {
			requestModelName = strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		}
		observedCompactionReplayAuthID := ""
		if observedCompaction.modelName != "" && observedCompaction.authID != "" &&
			observedCompaction.modelName == requestModelName &&
			(pinnedAuthID == "" || pinnedAuthID == observedCompaction.authID) {
			if compactionAuth, ok := sessionAuthByID(observedCompaction.authID); ok && compactionAuth != nil && compactionAuth.Status == coreauth.StatusActive {
				observedCompactionReplayAuthID = observedCompaction.authID
			}
		}
		allowCompactionReplayBypass := observedCompactionReplayAuthID != ""
		if pinnedAuthID != "" {
			if pinnedAuth, ok := sessionAuthByID(pinnedAuthID); ok && pinnedAuth != nil {
				allowCompactionReplayBypass = allowCompactionReplayBypass || responsesWebsocketAuthSupportsCompactionReplay(pinnedAuth)
			}
		} else {
			allowCompactionReplayBypass = allowCompactionReplayBypass || h.websocketUpstreamSupportsCompactionReplayForModel(requestModelName)
		}

		// Each turn re-presents the handshake credential to the access providers.
		// A denial rejects this turn and keeps the socket open: the client may
		// reauthorize and continue on the same connection (upstream b9bb031dcbb0).
		if errAuth := h.authenticateResponsesWebsocketTurn(c, upgradeRequest, payload, requestModelName); errAuth != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errAuth)
			markAPIResponseTimestamp(c)
			unlockWebsocketWriter := lockWebsocketWriter(&wsWriteMu)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errAuth)
			unlockWebsocketWriter()
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					passthroughSessionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				return
			}
			continue
		}

		var requestJSON []byte
		var updatedLastRequest []byte
		var errMsg *interfaces.ErrorMessage
		previousResponseID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
		if pendingPrewarmID != "" && previousResponseID != "" {
			if previousResponseID != pendingPrewarmID {
				errMsg = responsesWebsocketPreviousResponseNotFoundError()
			} else {
				// The synthetic warm-up acknowledged input that never reached
				// upstream; materialize it before merging the client's delta.
				requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketPrewarmFollowup(payload, lastRequest)
			}
		} else if pendingPrewarmID != "" && gjson.GetBytes(payload, "type").String() == wsRequestTypeCreate {
			input := gjson.GetBytes(payload, "input")
			if input.Exists() && !input.IsArray() {
				errMsg = &interfaces.ErrorMessage{
					StatusCode: http.StatusBadRequest,
					Error:      fmt.Errorf("websocket request requires array field: input"),
				}
			} else {
				// No parent reference means a self-contained replacement, not a delta.
				requestJSON, updatedLastRequest, errMsg = normalizeResponseCreateRequest(normalizeResponseTranscriptReplacement(payload, lastRequest))
			}
		} else {
			requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketRequestWithMode(
				payload,
				lastRequest,
				lastResponseOutput,
				allowIncrementalInputWithPreviousResponseID,
				allowCompactionReplayBypass,
			)
		}
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
			markAPIResponseTimestamp(c)
			unlockWebsocketWriter := lockWebsocketWriter(&wsWriteMu)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
			unlockWebsocketWriter()
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					passthroughSessionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				return
			}
			continue
		}
		requestJSON = h.prepareCodexOrphanDelegation(c, requestJSON)

		if shouldHandleResponsesWebsocketPrewarmLocally(payload, lastRequest, allowIncrementalInputWithPreviousResponseID) {
			if updated, errDelete := sjson.DeleteBytes(requestJSON, "generate"); errDelete == nil {
				requestJSON = updated
			}
			if updated, errDelete := sjson.DeleteBytes(updatedLastRequest, "generate"); errDelete == nil {
				updatedLastRequest = updated
			}
			lastRequest = updatedLastRequest
			lastResponseOutput = []byte("[]")
			observedCompaction.clear()
			unlockWebsocketWriter := lockWebsocketWriter(&wsWriteMu)
			prewarmID, errWrite := writeResponsesWebsocketSyntheticPrewarm(c, conn, requestJSON, wsTimelineLog, passthroughSessionID)
			unlockWebsocketWriter()
			if errWrite != nil {
				wsTerminateErr = errWrite
				return
			}
			pendingPrewarmID = prewarmID
			continue
		}

		toolCacheTransaction := beginResponsesWebsocketToolCacheTransaction(downstreamSessionKey)
		if toolCacheTransaction != nil {
			requestJSON = toolCacheTransaction.repair(requestJSON)
		} else {
			requestJSON = repairResponsesWebsocketToolCalls(downstreamSessionKey, requestJSON)
		}
		updatedLastRequest = bytes.Clone(requestJSON)
		previousLastRequest := bytes.Clone(lastRequest)
		previousLastResponseOutput := bytes.Clone(lastResponseOutput)
		previousForceTranscriptReplay := forceTranscriptReplayNextRequest
		lastRequest = updatedLastRequest
		if previousForceTranscriptReplay {
			forceTranscriptReplayNextRequest = false
		}

		modelName := gjson.GetBytes(requestJSON, "model").String()
		lastAttemptedAuthID := pinnedAuthID
		var codexDuplexStream atomic.Bool
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		if duplexInput != nil {
			cliCtx = cliproxyexecutor.WithWebsocketInput(cliCtx, duplexInput)
			// Live account-state check: later frames must not be sent once the
			// selected credential is disabled (upstream CLIProxyAPI commit 42c9680eee55).
			cliCtx = cliproxyexecutor.WithWebsocketAuthCheck(cliCtx, func(authID string) bool {
				current, ok := sessionAuthByID(authID)
				return ok && current != nil && !current.Disabled && current.Status != coreauth.StatusDisabled
			})
			// Follow-up continuations never pass the loop above, so the same
			// per-turn access check runs where the duplex executor consumes them.
			cliCtx = cliproxyexecutor.WithWebsocketTurnAuthorizer(cliCtx, func(turnPayload []byte) ([]byte, error) {
				errAuth := h.authenticateResponsesWebsocketTurn(c, upgradeRequest, turnPayload, modelName)
				if errAuth == nil {
					return nil, nil
				}
				return buildResponsesWebsocketErrorPayload(errAuth)
			})
		}
		cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
		executionAuthID := pinnedAuthID
		if executionAuthID == "" {
			executionAuthID = observedCompactionReplayAuthID
		}
		if executionAuthID != "" {
			cliCtx = handlers.WithPinnedAuthID(cliCtx, executionAuthID)
		}
		cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
			codexDuplexStream.Store(false)
			authID = strings.TrimSpace(authID)
			if authID == "" || h == nil || h.AuthManager == nil {
				return
			}
			lastAttemptedAuthID = authID
			selectedAuth, ok := sessionAuthByID(authID)
			if !ok || selectedAuth == nil {
				return
			}
			if websocketUpstreamSupportsIncrementalInput(selectedAuth.Attributes, selectedAuth.Metadata) {
				pinnedAuthID = authID
				codexDuplexStream.Store(duplexInput != nil && strings.EqualFold(strings.TrimSpace(selectedAuth.Provider), "codex"))
			}
		})
		dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")

		completedOutput, forwardErrMsg, errForward := h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, wsTimelineLog, passthroughSessionID, toolCacheTransaction, &wsWriteMu, codexDuplexStream.Load, &wsForwardedErr)
		if errForward != nil {
			wsTerminateErr = errForward
			log.Warnf("responses websocket: forward failed id=%s error=%v", passthroughSessionID, errForward)
			return
		}
		releasePinnedAuth := shouldReleaseResponsesWebsocketPinnedAuth(forwardErrMsg)
		rollbackRequest := shouldRollbackResponsesWebsocketRequest(forwardErrMsg)
		if releasePinnedAuth {
			pinnedAuthID = ""
			forceTranscriptReplayNextRequest = true
			if observedCompaction.authID != "" && observedCompaction.authID == lastAttemptedAuthID {
				observedCompaction.clear()
			}
		} else if rollbackRequest {
			forceTranscriptReplayNextRequest = previousForceTranscriptReplay
		}
		if releasePinnedAuth || rollbackRequest {
			if !rollbackRequest {
				toolCacheTransaction.commit()
			}
			lastRequest = previousLastRequest
			lastResponseOutput = previousLastResponseOutput
			continue
		}
		toolCacheTransaction.commit()
		pendingPrewarmID = ""
		lastResponseOutput = completedOutput
		if inputContainsFullTranscript(gjson.ParseBytes(completedOutput)) {
			// Compaction markers in the completed output prove this auth handled
			// a compaction replay; remember it for the next turn.
			observedCompaction = responsesWebsocketObservedCompactionState{
				modelName: strings.TrimSpace(modelName),
				authID:    lastAttemptedAuthID,
			}
		} else if observedCompaction.modelName != "" &&
			(observedCompaction.modelName != strings.TrimSpace(modelName) ||
				(observedCompaction.authID != "" && lastAttemptedAuthID != "" && observedCompaction.authID != lastAttemptedAuthID)) {
			observedCompaction.clear()
		}
	}
}

type responsesWebsocketObservedCompactionState struct {
	modelName string
	authID    string
}

func (s *responsesWebsocketObservedCompactionState) clear() {
	*s = responsesWebsocketObservedCompactionState{}
}

func websocketClientAddress(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return strings.TrimSpace(c.ClientIP())
}

func isRequestScopedResponsesWebsocketError(err error) bool {
	if err == nil {
		return false
	}
	type requestScopedError interface {
		IsRequestScoped() bool
	}
	var requestErr requestScopedError
	return errors.As(err, &requestErr) && requestErr != nil && requestErr.IsRequestScoped()
}

func websocketUpgradeHeaders(req *http.Request) http.Header {
	headers := http.Header{}
	if req == nil {
		return headers
	}

	// Keep the same sticky turn-state across reconnects when provided by the client.
	turnState := strings.TrimSpace(req.Header.Get(wsTurnStateHeader))
	if turnState != "" {
		headers.Set(wsTurnStateHeader, turnState)
	}
	return headers
}

func normalizeResponsesWebsocketRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithMode(rawJSON, lastRequest, lastResponseOutput, true, true)
}

func normalizeResponsesWebsocketRequestWithMode(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		// log.Infof("responses websocket: response.create request")
		if len(lastRequest) == 0 {
			return normalizeResponseCreateRequest(rawJSON)
		}
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
	case wsRequestTypeAppend:
		// log.Infof("responses websocket: response.append request")
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
	default:
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
}

func normalizeResponseCreateRequest(rawJSON []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	input := gjson.GetBytes(rawJSON, "input")
	if input.Exists() && !input.IsArray() {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	if !gjson.GetBytes(normalized, "input").Exists() {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	}

	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if modelName == "" {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("missing model in response.create request"),
		}
	}
	return normalized, bytes.Clone(normalized), nil
}

// responsesWebsocketItemMetadata extracts a named field from a single input item
// object, matching the key case-insensitively like the upstream websocket merge
// input classifier. Duplicate keys within an item resolve last-wins.
func responsesWebsocketItemMetadata(item []byte, name string) string {
	value := ""
	gjson.ParseBytes(item).ForEach(func(key, field gjson.Result) bool {
		if strings.EqualFold(key.String(), name) {
			value = strings.TrimSpace(field.String())
		}
		return true
	})
	return value
}

// responsesWebsocketInputField extracts the top-level "input" field following
// encoding/json duplicate-field semantics: the key is matched case-insensitively,
// the last duplicate wins, and any incompatible (non-null, non-array) duplicate
// marks the field invalid. It does not copy the selected array out of the
// caller-owned request buffer.
func responsesWebsocketInputField(rawJSON []byte) (last gjson.Result, exists bool, invalid bool) {
	gjson.ParseBytes(rawJSON).ForEach(func(key, matched gjson.Result) bool {
		if !strings.EqualFold(key.String(), "input") {
			return true
		}
		exists = true
		last = matched
		if matched.Type != gjson.Null && !matched.IsArray() {
			invalid = true
		}
		return true
	})
	return last, exists, invalid
}

func normalizeResponseSubsequentRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	if len(lastRequest) == 0 {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request received before response.create"),
		}
	}

	nextInput, nextInputExists, nextInputInvalid := responsesWebsocketInputField(rawJSON)
	if nextInputInvalid || !nextInputExists || !nextInput.IsArray() {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}

	// Compaction can cause clients to replace local websocket history with a new
	// compact transcript on the next `response.create`. When the input already
	// contains historical model output items, treating it as an incremental append
	// duplicates stale turn-state and can leave late orphaned function_call items.
	if shouldReplaceWebsocketTranscript(rawJSON, nextInput) {
		normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
		return normalized, bytes.Clone(normalized), nil
	}

	// Websocket v2 mode uses response.create with previous_response_id + incremental input.
	// Do not expand it into a full input transcript; upstream expects the incremental payload.
	if allowIncrementalInputWithPreviousResponseID {
		if prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()); prev != "" {
			normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
			if errDelete != nil {
				normalized = bytes.Clone(rawJSON)
			}
			if !gjson.GetBytes(normalized, "model").Exists() {
				modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
				if modelName != "" {
					normalized, _ = sjson.SetBytes(normalized, "model", modelName)
				}
			}
			if !gjson.GetBytes(normalized, "instructions").Exists() {
				instructions := gjson.GetBytes(lastRequest, "instructions")
				if instructions.Exists() {
					normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
				}
			}
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			return normalized, bytes.Clone(normalized), nil
		}
	}

	// When the client sends a compact replay for a downstream that can consume it
	// directly, the input already carries the canonical history. In that case,
	// skip merging with stale lastRequest/lastResponseOutput to avoid breaking
	// function_call / function_call_output pairings.
	// See: https://github.com/therealtinhtute/llmhub/issues/2207
	var mergedInput string
	if allowCompactionReplayBypass && inputContainsFullTranscript(nextInput) {
		log.Infof("responses websocket: full transcript detected, skipping stale merge (input items=%d)", len(nextInput.Array()))
		mergedInput = nextInput.Raw
	} else {
		appendInputRaw := nextInput.Raw
		if inputContainsFullTranscript(nextInput) {
			appendInputRaw = inputWithoutCompactionItems(nextInput)
		}

		existingInput, existingInputExists, existingInputInvalid := responsesWebsocketInputField(lastRequest)
		if existingInputInvalid {
			return nil, lastRequest, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("invalid previous request input: duplicate input field is not an array"),
			}
		}
		existingInputRaw := "[]"
		if existingInputExists && existingInput.Type != gjson.Null {
			existingInputRaw = existingInput.Raw
		}
		var errMerge error
		mergedInput, errMerge = mergeJSONArrayRaw(existingInputRaw, normalizeJSONArrayRaw(lastResponseOutput))
		if errMerge != nil {
			return nil, lastRequest, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("invalid previous response output: %w", errMerge),
			}
		}

		mergedInput, errMerge = mergeJSONArrayRaw(mergedInput, appendInputRaw)
		if errMerge != nil {
			return nil, lastRequest, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("invalid request input: %w", errMerge),
			}
		}
	}
	dedupedInput, errDedupeFunctionCalls := dedupeFunctionCallsByCallID(mergedInput)
	if errDedupeFunctionCalls == nil {
		mergedInput = dedupedInput
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("failed to merge websocket input: %w", errSet),
		}
	}
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return normalized, bytes.Clone(normalized), nil
}

func shouldReplaceWebsocketTranscript(rawJSON []byte, nextInput gjson.Result) bool {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) != "" {
		return false
	}
	if !nextInput.Exists() || !nextInput.IsArray() {
		return false
	}

	for _, item := range nextInput.Array() {
		rawItem := []byte(item.Raw)
		switch responsesWebsocketItemMetadata(rawItem, "type") {
		case "function_call", "custom_tool_call":
			return true
		case "message":
			if responsesWebsocketItemMetadata(rawItem, "role") == "assistant" {
				return true
			}
		}
	}

	return false
}

func normalizeResponseTranscriptReplacement(rawJSON []byte, lastRequest []byte) []byte {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return bytes.Clone(normalized)
}

func dedupeFunctionCallsByCallID(rawArray string) (string, error) {
	rawArray = strings.TrimSpace(rawArray)
	if rawArray == "" {
		return "[]", nil
	}
	var items []json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(rawArray), &items); errUnmarshal != nil {
		return "", errUnmarshal
	}

	seenCallIDs := make(map[string]struct{}, len(items))
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		itemType := responsesWebsocketItemMetadata(item, "type")
		if isResponsesToolCallType(itemType) {
			callID := responsesWebsocketItemMetadata(item, "call_id")
			if callID != "" {
				if _, ok := seenCallIDs[callID]; ok {
					continue
				}
				seenCallIDs[callID] = struct{}{}
			}
		}
		filtered = append(filtered, item)
	}

	out, errMarshal := json.Marshal(filtered)
	if errMarshal != nil {
		return "", errMarshal
	}
	return string(out), nil
}

func websocketUpstreamSupportsIncrementalInput(attributes map[string]string, metadata map[string]any) bool {
	if len(attributes) > 0 {
		if raw := strings.TrimSpace(attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(metadata) == 0 {
		return false
	}
	raw, ok := metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsIncrementalInputForModel(modelName string) bool {
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	for _, auth := range auths {
		if websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata) {
			return true
		}
	}
	return false
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsCompactionReplayForModel(modelName string) bool {
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	if len(auths) == 0 {
		return false
	}
	for _, auth := range auths {
		if !responsesWebsocketAuthSupportsCompactionReplay(auth) {
			return false
		}
	}
	return true
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketAvailableAuthsForModel(modelName string) ([]*coreauth.Auth, string) {
	if h == nil || h.AuthManager == nil {
		return nil, ""
	}
	resolvedModelName := responsesWebsocketResolvedModelName(modelName)
	providerSet, modelKey := responsesWebsocketProviderSetForModel(resolvedModelName)
	if len(providerSet) == 0 {
		return nil, modelKey
	}

	registryRef := registry.GetGlobalRegistry()
	now := time.Now()
	auths := h.AuthManager.List()
	available := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if !responsesWebsocketAuthMatchesModel(auth, providerSet, modelKey, registryRef, now) {
			continue
		}
		available = append(available, auth)
	}
	return available, modelKey
}

func responsesWebsocketResolvedModelName(modelName string) string {
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			return fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		}
		return resolvedBase
	}
	return util.ResolveAutoModel(modelName)
}

func responsesWebsocketProviderSetForModel(resolvedModelName string) (map[string]struct{}, string) {
	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)
	providers := util.GetProviderName(baseModel)
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		providerSet[providerKey] = struct{}{}
	}
	modelKey := baseModel
	if modelKey == "" {
		modelKey = strings.TrimSpace(resolvedModelName)
	}
	return providerSet, modelKey
}

func responsesWebsocketAuthMatchesModel(auth *coreauth.Auth, providerSet map[string]struct{}, modelKey string, registryRef *registry.ModelRegistry, now time.Time) bool {
	if auth == nil {
		return false
	}
	providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
	if _, ok := providerSet[providerKey]; !ok {
		return false
	}
	if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(auth.ID, modelKey) {
		return false
	}
	return responsesWebsocketAuthAvailableForModel(auth, modelKey, now)
}

func responsesWebsocketAuthSupportsCompactionReplay(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

func responsesWebsocketAuthAvailableForModel(auth *coreauth.Auth, modelName string, now time.Time) bool {
	if auth == nil {
		return false
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if modelName != "" && len(auth.ModelStates) > 0 {
		state, ok := auth.ModelStates[modelName]
		if (!ok || state == nil) && modelName != "" {
			baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
			if baseModel != "" && baseModel != modelName {
				state, ok = auth.ModelStates[baseModel]
			}
		}
		if ok && state != nil {
			if state.Status == coreauth.StatusDisabled {
				return false
			}
			if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
				return false
			}
			return true
		}
	}
	if auth.Unavailable && !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return false
	}
	return true
}

func shouldHandleResponsesWebsocketPrewarmLocally(rawJSON []byte, lastRequest []byte, allowIncrementalInputWithPreviousResponseID bool) bool {
	if allowIncrementalInputWithPreviousResponseID || len(lastRequest) != 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String()) != wsRequestTypeCreate {
		return false
	}
	generateResult := gjson.GetBytes(rawJSON, "generate")
	return generateResult.Exists() && !generateResult.Bool()
}

func writeResponsesWebsocketSyntheticPrewarm(
	c *gin.Context,
	conn *websocket.Conn,
	requestJSON []byte,
	wsTimelineLog websocketTimelineAppender,
	sessionID string,
) (string, error) {
	payloads, errPayloads := syntheticResponsesWebsocketPrewarmPayloads(requestJSON)
	if errPayloads != nil {
		return "", errPayloads
	}
	for i := 0; i < len(payloads); i++ {
		markAPIResponseTimestamp(c)
		// log.Infof(
		// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
		// 	sessionID,
		// 	websocket.TextMessage,
		// 	websocketPayloadEventType(payloads[i]),
		// 	websocketPayloadPreview(payloads[i]),
		// )
		if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now()); errWrite != nil {
			log.Warnf(
				"responses websocket: downstream_out write failed id=%s event=%s error=%v",
				sessionID,
				websocketPayloadEventType(payloads[i]),
				errWrite,
			)
			return "", errWrite
		}
	}
	return gjson.GetBytes(payloads[0], "response.id").String(), nil
}

// normalizeResponsesWebsocketPrewarmFollowup materializes input that was
// acknowledged by a synthetic warm-up but never reached upstream, then merges
// the client's next delta against it (upstream
// normalizeResponsesWebsocketPrewarmFollowup, adapted to the local merge
// helpers).
func normalizeResponsesWebsocketPrewarmFollowup(rawJSON, warmupRequest []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return nil, warmupRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("unsupported websocket request type: %s", requestType)}
	}
	input := gjson.GetBytes(rawJSON, "input")
	if !input.IsArray() {
		return nil, warmupRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("websocket request requires array field: input")}
	}
	warmupInput, warmupExists, warmupInvalid := responsesWebsocketInputField(warmupRequest)
	if warmupInvalid {
		return nil, warmupRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("invalid previous request input: duplicate input field is not an array")}
	}
	warmupInputRaw := "[]"
	if warmupExists && warmupInput.Type != gjson.Null {
		warmupInputRaw = warmupInput.Raw
	}
	merged, errMerge := mergeJSONArrayRaw(warmupInputRaw, input.Raw)
	if errMerge != nil {
		return nil, warmupRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errMerge}
	}
	if deduped, errDedupe := dedupeFunctionCallsByCallID(merged); errDedupe == nil {
		merged = deduped
	}
	normalized := normalizeResponseTranscriptReplacement(rawJSON, warmupRequest)
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(merged))
	if errSet != nil {
		return nil, warmupRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errSet}
	}
	return normalized, normalized, nil
}

func responsesWebsocketPreviousResponseNotFoundError() *interfaces.ErrorMessage {
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusConflict,
		Error: errors.New(
			`{"error":{"message":"Previous response is not available on this websocket; resend the full conversation input without previous_response_id","type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id"}}`,
		),
	}
}

func syntheticResponsesWebsocketPrewarmPayloads(requestJSON []byte) ([][]byte, error) {
	responseID := "resp_prewarm_" + uuid.NewString()
	createdAt := time.Now().Unix()
	modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String())

	createdPayload := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
	var errSet error
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		createdPayload, errSet = sjson.SetBytes(createdPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	completedPayload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}}`)
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		completedPayload, errSet = sjson.SetBytes(completedPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	return [][]byte{createdPayload, completedPayload}, nil
}

func mergeJSONArrayRaw(existingRaw, appendRaw string) (string, error) {
	existingRaw = strings.TrimSpace(existingRaw)
	appendRaw = strings.TrimSpace(appendRaw)
	if existingRaw == "" {
		existingRaw = "[]"
	}
	if appendRaw == "" {
		appendRaw = "[]"
	}

	var existing []json.RawMessage
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return "", err
	}
	var appendItems []json.RawMessage
	if err := json.Unmarshal([]byte(appendRaw), &appendItems); err != nil {
		return "", err
	}

	merged := append(existing, appendItems...)
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// inputContainsFullTranscript returns true when the input array carries compact
// replay markers that indicate the client already sent the full conversation
// transcript. Merging that input with stale lastRequest/lastResponseOutput
// would duplicate or break function_call/function_call_output pairings, so the
// caller should use the input as-is.
//
// Assistant messages alone are not enough to classify the payload as a replay:
// incremental websocket requests may legitimately append assistant items.
func inputContainsFullTranscript(input gjson.Result) bool {
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		t := item.Get("type").String()
		if t == "compaction" || t == "compaction_summary" {
			return true
		}
	}
	return false
}

func inputWithoutCompactionItems(input gjson.Result) string {
	if !input.IsArray() {
		return normalizeJSONArrayRaw([]byte(input.Raw))
	}
	filtered := make([]string, 0, len(input.Array()))
	for _, item := range input.Array() {
		t := item.Get("type").String()
		if t == "compaction" || t == "compaction_summary" {
			continue
		}
		filtered = append(filtered, item.Raw)
	}
	return "[" + strings.Join(filtered, ",") + "]"
}

func normalizeJSONArrayRaw(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "[]"
	}
	result := gjson.Parse(trimmed)
	if result.Type == gjson.JSON && result.IsArray() {
		return trimmed
	}
	return "[]"
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsTimelineLog websocketTimelineAppender,
	sessionID string,
	toolCacheTransaction *responsesWebsocketToolCacheTransaction,
	writeMu *sync.Mutex,
	duplexStream func() bool,
	forwardedErr *atomic.Value,
) ([]byte, *interfaces.ErrorMessage, error) {
	completed := false
	completedOutput := []byte("[]")
	downstreamSessionKey := ""
	if c != nil && c.Request != nil {
		downstreamSessionKey = websocketDownstreamSessionKey(c.Request)
	}

	// Downstream ping control frames keep intermediaries alive during long
	// upstream stalls; text writes go through writeMu so the disconnect
	// goroutine can interleave a terminal error frame safely.
	// (Upstream responsesWebsocketWriter.writePing + keepAlive ticker.)
	keepAliveInterval := time.Duration(0)
	if h != nil {
		keepAliveInterval = handlers.StreamingKeepAliveInterval(h.Cfg)
	}
	var keepAliveTicker *time.Ticker
	var keepAliveC <-chan time.Time
	if keepAliveInterval > 0 {
		keepAliveTicker = time.NewTicker(keepAliveInterval)
		defer keepAliveTicker.Stop()
		keepAliveC = keepAliveTicker.C
	}
	forwardError := func(errMsg *interfaces.ErrorMessage) error {
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
			markAPIResponseTimestamp(c)
			unlockWebsocketWriter := lockWebsocketWriter(writeMu)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
			if errWrite == nil && forwardedErr != nil {
				// Stored while the write lock is held so a disconnect racing this
				// frame cannot emit a duplicate terminal error
				// (upstream responsesWebsocketWriter.closing, commit 42c9680eee55).
				forwardedErr.Store(errMsg)
			}
			unlockWebsocketWriter()
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				sessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				cancel(errMsg.Error)
				return errWrite
			}
			cancel(errMsg.Error)
			return nil
		}
		cancel(nil)
		return nil
	}

	// finishDuplex ends a duplex turn on a terminal stream error: a duplex executor only reports
	// errors at connection termination, so the socket always ends. A replay-required failure asks
	// the client to continue over HTTP via a service-restart close frame (upstream
	// websocketClosePayloadForUpstreamError, commit 42c9680eee55).
	finishDuplex := func(errMsg *interfaces.ErrorMessage) ([]byte, *interfaces.ErrorMessage, error) {
		if errMsg != nil && cliproxyexecutor.IsUpstreamWebsocketReplayRequired(errMsg.Error) {
			unlockWebsocketWriter := lockWebsocketWriter(writeMu)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, wsHTTPReplayRequiredCloseReason), time.Time{})
			unlockWebsocketWriter()
			cancel(errMsg.Error)
			return completedOutput, errMsg, websocket.ErrCloseSent
		}
		// Expose client-facing faults as an error frame, then end the
		// socket either way (upstream writeResponsesWebsocketTerminalError,
		// CLIProxyAPI commit 42c9680eee55).
		if shouldExposeResponsesUpstreamError(errMsg) {
			if errWrite := forwardError(errMsg); errWrite != nil {
				return completedOutput, errMsg, errWrite
			}
		} else {
			var cause error
			if errMsg != nil {
				cause = errMsg.Error
			}
			if wsTimelineLog != nil && cause != nil {
				appendWebsocketTimelineDisconnect(wsTimelineLog, cause, time.Now())
			}
			cancel(cause)
		}
		return completedOutput, errMsg, websocket.ErrCloseSent
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return completedOutput, nil, c.Request.Context().Err()
		case <-keepAliveC:
			if errPing := conn.WriteControl(websocket.PingMessage, nil, time.Time{}); errPing != nil {
				cancel(errPing)
				return completedOutput, nil, errPing
			}
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if duplexStream != nil && duplexStream() {
				return finishDuplex(errMsg)
			}
			return completedOutput, errMsg, forwardError(errMsg)
		case chunk, ok := <-data:
			if !ok {
				data = nil
				duplex := duplexStream != nil && duplexStream()
				if duplex || !completed {
					// The producers send a terminal error before they close errs and then data, so it
					// is already buffered here. The select picks between the closed data channel and
					// that error at random; take the error (upstream CLIProxyAPI commit 0fe4cc2cfa80).
					// Otherwise a duplex stream loses the error its executor reported and a failure
					// that needs a replay close ends as a plain disconnect.
					if pendingErr, hasPending := handlers.PendingStreamError(errs); hasPending {
						if duplex {
							// The same terminal policy the errs branch applies, but the error
							// is reported to the caller instead of being dropped with the close.
							return finishDuplex(pendingErr)
						}
						return completedOutput, pendingErr, forwardError(pendingErr)
					}
				}
				if duplex {
					// A duplex stream ends with its socket, not an individual
					// response. The data channel may close before select observes
					// its final error; report connection termination instead of
					// "stream closed before response.completed"
					// (upstream CLIProxyAPI commit 42c9680eee55).
					cancel(nil)
					return completedOutput, nil, websocket.ErrCloseSent
				}
				if !completed {
					// A close that races the client's cancel is a cancellation, not a truncated
					// upstream read (upstream CLIProxyAPI commit 90fe5b23d350 makes the same choice).
					if errCtx := c.Request.Context().Err(); errCtx != nil {
						cancel(errCtx)
						return completedOutput, nil, errCtx
					}
					errMsg := &interfaces.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					return completedOutput, errMsg, forwardError(errMsg)
				}
				cancel(nil)
				return completedOutput, nil, nil
			}
			if keepAliveTicker != nil && keepAliveInterval > 0 {
				keepAliveTicker.Reset(keepAliveInterval)
			}

			payloads := websocketJSONPayloadsFromChunk(chunk)
			for i := range payloads {
				if gjson.GetBytes(payloads[i], "type").String() == "response.created" {
					// Each new response starts fresh per-response tracking; in
					// duplex mode multiple responses share one stream
					// (upstream CLIProxyAPI commit 42c9680eee55).
					completed = false
				}
				if toolCacheTransaction != nil {
					toolCacheTransaction.record(payloads[i])
				} else {
					recordResponsesWebsocketToolCallsFromPayload(downstreamSessionKey, payloads[i])
				}
				eventType := gjson.GetBytes(payloads[i], "type").String()
				if eventType == wsEventTypeCompleted {
					completed = true
					completedOutput = responseCompletedOutputFromPayload(payloads[i])
				}
				markAPIResponseTimestamp(c)
				// log.Infof(
				// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				// 	sessionID,
				// 	websocket.TextMessage,
				// 	websocketPayloadEventType(payloads[i]),
				// 	websocketPayloadPreview(payloads[i]),
				// )
				unlockWebsocketWriter := lockWebsocketWriter(writeMu)
				errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now())
				unlockWebsocketWriter()
				if errWrite != nil {
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s error=%v",
						sessionID,
						websocketPayloadEventType(payloads[i]),
						errWrite,
					)
					cancel(errWrite)
					return completedOutput, nil, errWrite
				}
			}
		}
	}
}

// shouldExposeResponsesUpstreamError reports whether a terminal upstream
// failure is written to the client as an error event; non-exposable failures
// just close the socket (upstream CLIProxyAPI commit 42c9680eee55).
func shouldExposeResponsesUpstreamError(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	if coreauth.IsTerminalAuthError(errMsg.Error) {
		return true
	}
	return clienterror.IsRequestFault(responsesWebsocketErrorStatus(errMsg), errMsg.Error)
}

func responsesWebsocketErrorStatus(errMsg *interfaces.ErrorMessage) int {
	if errMsg == nil {
		return 0
	}
	status := errMsg.StatusCode
	if status <= 0 && errMsg.Error != nil {
		if se, ok := errMsg.Error.(interface{ StatusCode() int }); ok && se != nil {
			status = se.StatusCode()
		}
	}
	return status
}

func shouldReleaseResponsesWebsocketPinnedAuth(errMsg *interfaces.ErrorMessage) bool {
	switch responsesWebsocketErrorStatus(errMsg) {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func shouldRollbackResponsesWebsocketRequest(errMsg *interfaces.ErrorMessage) bool {
	return responsesWebsocketErrorStatus(errMsg) == http.StatusRequestEntityTooLarge &&
		errMsg != nil && isRequestScopedResponsesWebsocketError(errMsg.Error)
}

func responseCompletedOutputFromPayload(payload []byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() {
		return bytes.Clone([]byte(output.Raw))
	}
	return []byte("[]")
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	payloads := make([][]byte, 0, 2)
	lines := bytes.Split(chunk, []byte("\n"))
	for i := range lines {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte(wsDoneMarker)) {
			continue
		}
		if json.Valid(line) {
			payloads = append(payloads, bytes.Clone(line))
		}
	}

	if len(payloads) > 0 {
		return payloads
	}

	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte(wsDoneMarker)) && json.Valid(trimmed) {
		payloads = append(payloads, bytes.Clone(trimmed))
	}
	return payloads
}

func writeResponsesWebsocketError(conn *websocket.Conn, wsTimelineLog websocketTimelineAppender, errMsg *interfaces.ErrorMessage) ([]byte, error) {
	payload, errBuild := buildResponsesWebsocketErrorPayload(errMsg)
	if errBuild != nil {
		return nil, errBuild
	}
	return payload, writeResponsesWebsocketPayload(conn, wsTimelineLog, payload, time.Now())
}

// buildResponsesWebsocketErrorPayload renders the error event for an
// ErrorMessage. It is transport-free so a rejected duplex turn can hand the
// same payload back to the executor's ordered output stream.
func buildResponsesWebsocketErrorPayload(errMsg *interfaces.ErrorMessage) ([]byte, error) {
	status := http.StatusInternalServerError
	errText := http.StatusText(status)
	if errMsg != nil {
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
			errText = http.StatusText(status)
		}
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
	}

	var errCause error
	if errMsg != nil {
		errCause = errMsg.Error
	}
	body := handlers.BuildErrorResponseBodyWithError(status, errText, errCause)
	payload := []byte(`{}`)
	var errSet error
	payload, errSet = sjson.SetBytes(payload, "type", wsEventTypeError)
	if errSet != nil {
		return nil, errSet
	}
	payload, errSet = sjson.SetBytes(payload, "status", status)
	if errSet != nil {
		return nil, errSet
	}

	if errMsg != nil && errMsg.Addon != nil {
		headers := []byte(`{}`)
		hasHeaders := false
		for key, values := range errMsg.Addon {
			if len(values) == 0 {
				continue
			}
			headerPath := strings.ReplaceAll(strings.ReplaceAll(key, `\\`, `\\\\`), ".", `\\.`)
			headers, errSet = sjson.SetBytes(headers, headerPath, values[0])
			if errSet != nil {
				return nil, errSet
			}
			hasHeaders = true
		}
		if hasHeaders {
			payload, errSet = sjson.SetRawBytes(payload, "headers", headers)
			if errSet != nil {
				return nil, errSet
			}
		}
	}

	if len(body) > 0 && json.Valid(body) {
		errorNode := gjson.GetBytes(body, "error")
		if errorNode.Exists() {
			payload, errSet = sjson.SetRawBytes(payload, "error", []byte(errorNode.Raw))
		} else {
			payload, errSet = sjson.SetRawBytes(payload, "error", body)
		}
		if errSet != nil {
			return nil, errSet
		}
	}

	if !gjson.GetBytes(payload, "error").Exists() {
		payload, errSet = sjson.SetBytes(payload, "error.type", "server_error")
		if errSet != nil {
			return nil, errSet
		}
		payload, errSet = sjson.SetBytes(payload, "error.message", errText)
		if errSet != nil {
			return nil, errSet
		}
	}

	return payload, nil
}

func appendWebsocketEvent(builder *strings.Builder, eventType string, payload []byte) {
	if builder == nil {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString("websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(trimmedPayload)
	builder.WriteString("\n")
}

func websocketPayloadEventType(payload []byte) string {
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType == "" {
		return "-"
	}
	return eventType
}

func websocketPayloadPreview(payload []byte) string {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return "<empty>"
	}
	previewText := strings.ReplaceAll(string(trimmedPayload), "\n", "\\n")
	previewText = strings.ReplaceAll(previewText, "\r", "\\r")
	return previewText
}

func setWebsocketTimelineBody(c *gin.Context, body string) {
	setWebsocketBody(c, wsTimelineBodyKey, body)
}

func setWebsocketBody(c *gin.Context, key string, body string) {
	if c == nil {
		return
	}
	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		return
	}
	c.Set(key, []byte(trimmedBody))
}

// lockWebsocketWriter serializes websocket text writes when writeMu is
// provided, so the upstream-disconnect goroutine can emit a terminal error
// frame without racing in-flight writes (upstream responsesWebsocketWriter.writeMu).
// Callers on the main handler goroutine may pass nil when no sharing is needed.
func lockWebsocketWriter(writeMu *sync.Mutex) func() {
	if writeMu == nil {
		return func() {}
	}
	writeMu.Lock()
	return writeMu.Unlock
}

func writeResponsesWebsocketPayload(conn *websocket.Conn, wsTimelineLog websocketTimelineAppender, payload []byte, timestamp time.Time) error {
	if wsTimelineLog != nil {
		wsTimelineLog.Append("response", payload, timestamp)
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func appendWebsocketTimelineDisconnect(timeline websocketTimelineAppender, err error, timestamp time.Time) {
	if err == nil {
		return
	}
	if timeline != nil {
		timeline.Append("disconnect", []byte(err.Error()), timestamp)
	}
}

func appendWebsocketTimelineEvent(builder *strings.Builder, eventType string, payload []byte, timestamp time.Time) {
	if builder == nil {
		return
	}
	writeWebsocketTimelineBuilder(builder, formatWebsocketTimelineEvent(eventType, payload, timestamp))
}

func formatWebsocketTimelineEvent(eventType string, payload []byte, timestamp time.Time) []byte {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return nil
	}
	var builder strings.Builder
	builder.WriteString("Timestamp: ")
	builder.WriteString(timestamp.Format(time.RFC3339Nano))
	builder.WriteString("\n")
	builder.WriteString("Event: websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(trimmedPayload)
	builder.WriteString("\n")
	return []byte(builder.String())
}

func markAPIResponseTimestamp(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	c.Set("API_RESPONSE_TIMESTAMP", time.Now())
}
