package executor

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexIncompleteStreamMessage = "stream error: stream disconnected before completion: stream closed before response.completed"

// codexIncompleteStreamError marks a clean upstream end that never produced a terminal
// response event. It is request-scoped: the conductor stops the attempt but must not
// penalize the credential. Ported from upstream codex_executor_terminal.go.
type codexIncompleteStreamError struct {
	statusErr
}

func newCodexIncompleteStreamError() codexIncompleteStreamError {
	return codexIncompleteStreamError{statusErr: statusErr{
		code: http.StatusRequestTimeout,
		msg:  codexIncompleteStreamMessage,
	}}
}

func (codexIncompleteStreamError) IsRequestScoped() bool {
	return true
}

// codexEmptyIncompleteStreamError marks a response.incomplete with zero output tokens
// and zero produced content (upstream silent abort). Delivered in-stream.
type codexEmptyIncompleteStreamError struct {
	statusErr
}

func newCodexEmptyIncompleteStreamError() codexEmptyIncompleteStreamError {
	return codexEmptyIncompleteStreamError{statusErr: statusErr{
		code: http.StatusBadGateway,
		msg:  helps.CodexEmptyIncompleteStreamMessage,
	}}
}

func (codexEmptyIncompleteStreamError) IsRequestScoped() bool {
	return true
}

// codexBootstrapNowMu protects codexBootstrapNow across concurrent tests and goroutines.
// Ported from upstream CLIProxyAPI (6e307553f43f).
var (
	codexBootstrapNowMu sync.RWMutex
	codexBootstrapNow   = time.Now
)

func nowCodexBootstrap() time.Time {
	codexBootstrapNowMu.RLock()
	fn := codexBootstrapNow
	codexBootstrapNowMu.RUnlock()
	if fn != nil {
		return fn()
	}
	return time.Now()
}

func setCodexBootstrapNowForTest(fn func() time.Time) func() {
	codexBootstrapNowMu.Lock()
	orig := codexBootstrapNow
	codexBootstrapNow = fn
	codexBootstrapNowMu.Unlock()
	return func() {
		codexBootstrapNowMu.Lock()
		codexBootstrapNow = orig
		codexBootstrapNowMu.Unlock()
	}
}

// codexBootstrapMaxBufferedFrames bounds how many upstream frames may be held back while probing
// for a rejection embedded in an HTTP 200 stream. It counts frames read from the upstream, not
// chunks handed downstream: a frame the downstream translator does not recognise renders as zero
// chunks, so a chunk count is a bound only for the formats that happen to render every frame.
//
// The two transports spend the budget differently: the SSE executor charges one unit per line it
// holds, the websocket executor one per message it reads whether or not it holds it. So the same
// number protects 15 heartbeats under the three-line event:/data:/blank shape a keepalive arrives
// in - 45 lines, with the rejection frame's own event: line spending a 46th - and more under terser
// framings. It is sized for that worst case rather than for a fixed event count, because deriving
// the unit from the framing is what lets an upstream evade the bound.
// Ported from upstream CLIProxyAPI (6e307553f43f range).
const codexBootstrapMaxBufferedFrames = 48

// codexBootstrapMaxBufferedBytes caps what a single bootstrap retains, counted over the upstream
// frames and the chunks they translate into. A frame budget alone would not bound that: on SSE
// scanner.Buffer allows 50MB per line, and the websocket dialer sets no read limit at all. The check
// runs before the frame is taken, so one oversized frame cannot be admitted on the strength of an
// empty buffer - but it bounds what is retained, not the peak: the transport has already
// materialised the frame by the time it is consulted.
const codexBootstrapMaxBufferedBytes = 1 << 20

// isCodexBootstrapBufferableEvent reports whether a frame may be held back before the downstream
// response headers are committed, i.e. whether nothing observable has happened yet.
//
// The list is closed on purpose. "Nothing has happened yet" cannot be derived from the absence of a
// TTFT token: TTFT deliberately ignores server-side tool traffic such as
// response.shell_call_output_content.delta and its .done counterpart, and holding one of those back
// would let a later rejection replay a tool call, and its side effects, on another credential. An
// unrecognised frame therefore releases the stream.
//
// isCodexHandshakeMetadataEvent reports whether an event type belongs to the
// handshake preamble - metadata frames that always precede generated content
// and are therefore unconditionally safe to buffer.
func isCodexHandshakeMetadataEvent(eventType string) bool {
	switch eventType {
	// response.metadata carries prompt-moderation flags, not generated output.
	// Upstream sends it between response.in_progress and a server_is_overloaded
	// rejection, so releasing on it leaked the rejection to the client instead of
	// failing the attempt over.
	case "response.created", "response.in_progress", "codex.rate_limits", "codex.response.metadata", "response.metadata", "keepalive":
		return true
	default:
		return false
	}
}

// Beyond the handshake preamble the list covers what upstream interleaves before the first token:
// keepalive heartbeats, and the *.added frames that announce an item or part with no content yet.
func isCodexBootstrapBufferableEvent(eventType string, payload []byte) bool {
	// An empty data: frame is the SSE heartbeat idiom and carries nothing at all, which is how the
	// websocket loop already treats an empty message. Without this it would fall through to the
	// default and release the stream, turning the feature off for any upstream that sends one.
	if len(bytes.TrimSpace(payload)) == 0 {
		return true
	}
	if isCodexHandshakeMetadataEvent(eventType) {
		return true
	}
	switch eventType {
	case "response.output_item.added":
		return isCodexBufferableOutputItem(payload)
	case "response.content_part.added":
		return isCodexEmptyPart(payload)
	case "response.reasoning_summary_part.added":
		return isCodexEmptyPart(payload)
	default:
		return false
	}
}

// isCodexBufferableOutputItem reports whether an announced output item is one the model produces by
// itself and has not started producing, so nothing is running upstream yet. The emptiness checks
// cover message and reasoning text, tool arguments, and encrypted reasoning. Every other item type
// is released, which covers the server-side operations that may already have been dispatched - a
// web_search_call is announced with status "in_progress" and its searching event follows
// immediately, and failing the attempt over after one would run it again on another credential - and
// errs the same way for anything else this list has not been taught about.
func isCodexBufferableOutputItem(payload []byte) bool {
	item := gjson.GetBytes(payload, "item")
	switch item.Get("type").String() {
	case "message":
		return isCodexEmptyContentList(item.Get("content"))
	case "reasoning":
		if item.Get("encrypted_content").String() != "" {
			return false
		}
		return isCodexEmptyContentList(item.Get("summary")) && isCodexEmptyContentList(item.Get("content"))
	case "function_call":
		return item.Get("arguments").String() == ""
	case "custom_tool_call":
		return item.Get("input").String() == ""
	default:
		return false
	}
}

// isCodexEmptyContentList reports whether every entry of an item's content or summary array is a
// textual shape this list knows about and is still empty. An entry whose type is not on the list may
// carry content in a field this check cannot see - an output_audio entry keeps it in "audio" - so it
// is treated as already produced.
func isCodexEmptyContentList(list gjson.Result) bool {
	for _, entry := range list.Array() {
		switch entry.Get("type").String() {
		case "output_text", "summary_text", "text", "reasoning_text":
			if entry.Get("text").String() != "" {
				return false
			}
		case "refusal":
			if entry.Get("refusal").String() != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isCodexEmptyPart reports whether an announced part is a textual one that is still empty. The part
// type is matched against a closed list for the same reason the event type is: a part shape this
// list has not been taught about may carry content in a field the emptiness check cannot see, so it
// releases the stream instead.
func isCodexEmptyPart(payload []byte) bool {
	part := gjson.GetBytes(payload, "part")
	switch part.Get("type").String() {
	case "output_text", "summary_text", "text", "reasoning_text":
		return part.Get("text").String() == ""
	case "refusal":
		return part.Get("refusal").String() == ""
	default:
		return false
	}
}

// newCodexBootstrapOverloadErr reports a buffered overload rejection with its real status.
//
// The status is deliberately produced here instead of in the shared terminal-failure mapping:
// that mapping serves the unbuffered path, where the rejection is delivered in-stream and a
// status change would alter cooldown classification and retry-after parsing for everyone.
// Keeping 503 scoped to this path means disabling the feature restores previous behaviour exactly.
func newCodexBootstrapOverloadErr(body []byte) statusErr {
	return newCodexStatusErr(http.StatusServiceUnavailable, body)
}

// isCodexOverloadBootstrapFailure reports whether a terminal failure delivered inside an HTTP 200
// stream is a transient capacity rejection that a different credential may be able to serve.
// Only these failures justify replacing the whole attempt during bootstrap; every other terminal
// failure keeps the original in-stream delivery semantics so downstream behaviour is unchanged.
func isCodexOverloadBootstrapFailure(body []byte) bool {
	// Model-capacity rejections are transient capacity failures too (upstream 3ae9093d).
	if isCodexModelCapacityError(body) {
		return true
	}
	errorType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.type").String()))
	errorCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	errorMessage := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()))
	if errorMessage == "" {
		errorMessage = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "message").String()))
	}
	switch {
	case errorType == "service_unavailable_error", errorCode == "server_is_overloaded":
		return true
	case errorType == "rate_limit_error", errorCode == "rate_limit_exceeded":
		return true
	case (errorType == "server_error" || errorCode == "server_error") && strings.Contains(errorMessage, "you can retry your request"):
		// Retryable server errors: upstream says the request can be retried, which a
		// different credential may serve (upstream range classification row).
		return true
	default:
		return false
	}
}

// codexPreOutputFailoverEligible reports whether an upstream failure that reached the downstream
// turn before any payload chunk can still be retried on another credential. A usage-limit or
// transient capacity rejection qualifies, and so does any other 429; those are exactly the
// failures worth keeping the downstream websocket open for (upstream 86a1aabf0178). Every other
// failure stays terminal so an in-stream error still ends the session as before.
func codexPreOutputFailoverEligible(body []byte, status int) bool {
	if status == 429 {
		return true
	}
	return isCodexUsageLimitError(body) || isCodexOverloadBootstrapFailure(body)
}

// statusCodeOfError reads the HTTP status an executor failure carries, or 0 when it has none.
func statusCodeOfError(err error) int {
	if err == nil {
		return 0
	}
	if coded, ok := err.(interface{ StatusCode() int }); ok && coded != nil {
		return coded.StatusCode()
	}
	return 0
}

// codexTerminalFailureBody extracts a terminal error payload from a stream event, preserving the
// upstream sequence_number so error telemetry stays aligned with the wire ordering.
// Ported from upstream codex_executor_terminal.go (sequence-number row).
func codexTerminalFailureBody(eventData []byte) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()
	var body []byte
	switch eventType {
	case "error":
		body = codexTerminalErrorBody(eventData, "error")
		if len(body) == 0 {
			body = codexTerminalTopLevelErrorBody(eventData)
		}
	case "response.failed":
		body = codexTerminalErrorBody(eventData, "response.error")
		if len(body) == 0 {
			body = codexTerminalErrorBody(eventData, "error")
		}
	default:
		return nil, false
	}
	if len(body) == 0 {
		body = []byte(`{"error":{"message":"upstream stream failed without error details"}}`)
	}
	if seq := gjson.GetBytes(eventData, "sequence_number"); seq.Exists() {
		body, _ = sjson.SetBytes(body, "sequence_number", seq.Int())
	}
	return body, true
}

// codexTerminalFailureStatus maps a terminal failure body to the status a client would have
// received had upstream returned it on the wire.
// Ported from upstream codex_executor_terminal.go.
func codexTerminalFailureStatus(body []byte) int {
	for _, path := range []string{"error.status_code", "error.status"} {
		if status := int(gjson.GetBytes(body, path).Int()); status >= 400 && status <= 599 {
			return status
		}
	}

	errorCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	errorType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.type").String()))
	switch {
	case errorCode == "cyber_policy":
		return http.StatusBadRequest
	case errorType == "not_found_error", errorCode == "not_found", errorCode == "model_not_found":
		return http.StatusNotFound
	case errorType == "authentication_error", errorCode == "invalid_api_key", errorCode == "unauthorized":
		return http.StatusUnauthorized
	case errorType == "permission_error", errorCode == "forbidden", errorCode == "permission_denied":
		return http.StatusForbidden
	case errorType == "rate_limit_error", errorCode == "rate_limit_exceeded":
		return http.StatusTooManyRequests
	case errorType == "invalid_request_error", errorType == "bad_request_error":
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// codexTerminalStreamErrShouldHandle reports whether a terminal failure body belongs to the set
// that is delivered as a 400-class request-scoped error rather than the mapped wire status.
func codexTerminalStreamErrShouldHandle(body []byte) bool {
	if codexTerminalErrorIsContextLength(body) {
		return true
	}
	if isCodexUsageLimitError(body) || isCodexModelCapacityError(body) {
		return true
	}
	code, _, ok := codexStatusErrorClassification(http.StatusBadRequest, body)
	return ok && code == "thinking_signature_invalid"
}

// codexTerminalStreamErr maps a should-handle terminal failure to a request-scoped 400 error.
func codexTerminalStreamErr(eventData []byte) (statusErr, []byte, bool) {
	return codexTerminalStreamErrWithCooling(eventData, false)
}

func codexTerminalStreamErrWithCooling(eventData []byte, modelLevelCooling bool) (statusErr, []byte, bool) {
	body, ok := codexTerminalFailureBody(eventData)
	if !ok || !codexTerminalStreamErrShouldHandle(body) {
		return statusErr{}, nil, false
	}
	return newCodexStatusErrWithCooling(http.StatusBadRequest, body, modelLevelCooling), body, true
}

// codexTerminalFailureErr extracts a terminal error from a buffered stream event, mirroring the
// unbuffered path's classification so buffered and unbuffered deliveries classify identically.
// Returns ok=false for non-terminal events.
func codexTerminalFailureErr(eventData []byte) (statusErr, []byte, bool) {
	return codexTerminalFailureErrWithCooling(eventData, false)
}

// codexTerminalFailureErrWithCooling passes the executor's model-level-cooling
// setting through to the mapped status error so usage_limit_reached failures
// can be marked credential-scoped (upstream b064b832e242).
func codexTerminalFailureErrWithCooling(eventData []byte, modelLevelCooling bool) (statusErr, []byte, bool) {
	if streamErr, body, ok := codexTerminalStreamErrWithCooling(eventData, modelLevelCooling); ok {
		return streamErr, body, true
	}
	body, ok := codexTerminalFailureBody(eventData)
	if !ok {
		return statusErr{}, nil, false
	}
	return newCodexStatusErrWithCooling(codexTerminalFailureStatus(body), body, modelLevelCooling), body, true
}

// codexBootstrapTerminalFailure extracts a terminal error payload from a buffered stream event.
// Mirrors the local unbuffered handling of "error" and "response.failed" events so buffered and
// unbuffered paths classify identically; returns ok=false for non-terminal events.
func codexBootstrapTerminalFailure(eventData []byte) (statusErr, []byte, bool) {
	return codexTerminalFailureErr(eventData)
}
