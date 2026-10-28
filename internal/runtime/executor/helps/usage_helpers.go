package helps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	internallogging "github.com/therealtinhtute/llmhub/internal/logging"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type UsageReporter struct {
	requestID            string
	traceID              string
	provider             string
	executorType         string
	model                string
	alias                string
	authID               string
	authIndex            string
	accessToken          string
	authType             string
	apiKey               string
	sessionID            string
	parentSessionID      string
	source               string
	reasoning            string
	effectiveServiceTier string
	stream               bool
	requestedAt          time.Time
	ttftMu               sync.RWMutex
	ttft                 time.Duration
	firstPacketDuration  time.Duration
	firstPacketSet       bool
	ttftStart            time.Time
	ttftSet              bool
	once                 sync.Once

	responseModelMu sync.RWMutex
	// responseModel holds the latest model name reported by the upstream response.
	responseModel string
	// responseModelFinal marks that a terminal event already reported the served
	// model, so later frames skip parsing entirely.
	responseModelFinal atomic.Bool

	upstreamModelMu sync.RWMutex
	// upstreamModel holds the canonical upstream model expected to be served when
	// it differs from the requested model (e.g. local Kimi model mappings).
	upstreamModel string
}

type usageExecutor interface {
	Identifier() string
}

// NewExecutorUsageReporter builds a reporter whose provider is taken from the
// executor's Identifier, mirroring the upstream constructor (upstream 959067ed).
func NewExecutorUsageReporter(ctx context.Context, executor usageExecutor, model string, auth *cliproxyauth.Auth) *UsageReporter {
	provider := ""
	if executor != nil {
		provider = executor.Identifier()
	}
	reporter := NewUsageReporter(ctx, provider, model, auth)
	reporter.executorType = ExecutorTypeName(executor)
	return reporter
}

// ExecutorTypeName returns the concrete executor type name for usage records.
func ExecutorTypeName(executor any) string {
	if executor == nil {
		return ""
	}
	executorType := reflect.TypeOf(executor)
	for executorType.Kind() == reflect.Pointer {
		executorType = executorType.Elem()
	}
	return strings.TrimSpace(executorType.Name())
}

func NewUsageReporter(ctx context.Context, provider, model string, auth *cliproxyauth.Auth) *UsageReporter {
	apiKey := APIKeyFromContext(ctx)
	alias := usage.RequestedModelAliasFromContext(ctx)
	if alias == "" {
		alias = model
	}
	sessionID := ""
	parentSessionID := ""
	clientMeta := internallogging.GetClientRequestMetadata(ctx)
	if clientMeta.SessionID != "" {
		sessionID = clientMeta.SessionID
		parentSessionID = clientMeta.ParentSessionID
		if sessionID == parentSessionID || !isUsageHierarchyParent(sessionID, parentSessionID) {
			parentSessionID = ""
		}
	}
	traceID := usage.TraceIDFromContext(ctx)
	if traceID == "" {
		traceID = internallogging.GetRequestID(ctx)
	}
	reporter := &UsageReporter{
		requestID:       uuid.NewString(),
		traceID:         traceID,
		provider:        provider,
		model:           model,
		alias:           strings.TrimSpace(alias),
		requestedAt:     time.Now(),
		apiKey:          apiKey,
		sessionID:       sessionID,
		parentSessionID: parentSessionID,
		source:          resolveUsageSource(auth, apiKey),
		authType:        resolveUsageAuthType(auth),
		reasoning:       usage.ReasoningEffortFromContext(ctx),
		stream:          usage.StreamFromContext(ctx),
	}
	if auth != nil {
		reporter.authID = auth.ID
		reporter.authIndex = auth.EnsureIndex()
		reporter.accessToken = authAccessTokenSHA256(auth)
	}
	return reporter
}

// SetStream records whether the request was executed in streaming mode.
func (r *UsageReporter) SetStream(stream bool) {
	if r == nil {
		return
	}
	r.stream = stream
}

// SetSessionHierarchy sets the explicit session and parent session identifiers.
// Callers should invoke this method before Publish or EnsurePublished on the request thread.
func (r *UsageReporter) SetSessionHierarchy(sessionID, parentSessionID string) {
	if r == nil {
		return
	}
	r.sessionID = strings.TrimSpace(sessionID)
	r.parentSessionID = strings.TrimSpace(parentSessionID)
	if r.sessionID == "" || r.sessionID == r.parentSessionID || !isUsageHierarchyParent(r.sessionID, r.parentSessionID) {
		r.parentSessionID = ""
	}
}

// isUsageHierarchyParent reports whether parent is a plausible lineage ancestor
// of primary: same-namespace identifiers and :agent: subagent paths qualify,
// while empty, equal, or cross-namespace pairs do not.
func isUsageHierarchyParent(primary, parent string) bool {
	if parent == "" || primary == "" || primary == parent {
		return false
	}
	if strings.Contains(primary, ":agent:") {
		return true
	}
	idx1 := strings.Index(primary, ":")
	idx2 := strings.Index(parent, ":")
	if idx1 > 0 && idx2 > 0 && primary[:idx1] == parent[:idx2] {
		return true
	}
	return false
}

// SetEffectiveServiceTier captures the service tier of the final outbound payload,
// after protocol translation and payload defaults or overrides, so the usage
// record names the tier the upstream was actually asked to serve. The
// client-requested tier is not recorded separately: the local reporter has no
// such field and no sink consumes one.
func (r *UsageReporter) SetEffectiveServiceTier(payload []byte) {
	if r == nil {
		return
	}
	r.effectiveServiceTier = extractEffectiveServiceTier(payload)
}

// extractEffectiveServiceTier reads the service tier from the outbound request
// shapes a translated payload can take: a Responses/Chat Completions top-level
// field, a Gemini-family request wrapper, and an Interactions request.
func extractEffectiveServiceTier(payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	for _, path := range []string{"service_tier", "request.service_tier", "interaction.service_tier"} {
		if node := gjson.GetBytes(payload, path); node.Type == gjson.String {
			if tier := strings.TrimSpace(node.String()); tier != "" {
				return tier
			}
		}
	}
	return ""
}

func (r *UsageReporter) TrackHTTPClient(client *http.Client) *http.Client {
	return r.trackHTTPClient(client, false)
}

// TrackHTTPClientRoundTripOnly records the TTFT start time upon sending the request
// and captures first-packet arrival fallback on initial body reads, while keeping
// effective TTFT unset. This allows protocol-aware streaming executors (like Codex SSE)
// to mark effective TTFT explicitly upon receiving substantive token events, while
// preserving first-packet fallback metrics for non-2xx error bodies or keepalive streams.
func (r *UsageReporter) TrackHTTPClientRoundTripOnly(client *http.Client) *http.Client {
	return r.trackHTTPClient(client, true)
}

func (r *UsageReporter) trackHTTPClient(client *http.Client, packetOnly bool) *http.Client {
	if r == nil || client == nil {
		return client
	}
	tracked := *client
	transport := tracked.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	tracked.Transport = usageTTFTRoundTripper{
		base:       transport,
		reporter:   r,
		packetOnly: packetOnly,
	}
	return &tracked
}

func (r *UsageReporter) ObserveResponse(resp *http.Response) {
	if r == nil || resp == nil || resp.Body == nil {
		return
	}
	r.StartResponseTTFT()
	resp.Body = &usageTTFTReadCloser{
		ReadCloser: resp.Body,
		mark: func() {
			r.MarkFirstResponseByte()
		},
	}
}

func (r *UsageReporter) ObserveResponsePacketOnly(resp *http.Response) {
	if r == nil || resp == nil || resp.Body == nil {
		return
	}
	r.StartResponseTTFT()
	resp.Body = &usageTTFTReadCloser{
		ReadCloser: resp.Body,
		mark: func() {
			r.RecordFirstPacket()
		},
	}
}

func (r *UsageReporter) StartResponseTTFT() {
	if r == nil {
		return
	}
	r.ttftMu.Lock()
	if !r.ttftSet && r.ttftStart.IsZero() {
		r.ttftStart = time.Now()
	}
	r.ttftMu.Unlock()
}

func (r *UsageReporter) IsTTFTSet() bool {
	if r == nil {
		return false
	}
	r.ttftMu.RLock()
	defer r.ttftMu.RUnlock()
	return r.ttftSet
}

func (r *UsageReporter) IsFirstPacketSet() bool {
	if r == nil {
		return false
	}
	r.ttftMu.RLock()
	defer r.ttftMu.RUnlock()
	return r.firstPacketSet
}

// RecordFirstPacket records the arrival time of the first packet/chunk from upstream as a fallback.
func (r *UsageReporter) RecordFirstPacket() {
	r.ObserveTokenEvent(false)
}

// ObserveTokenEvent records the first packet fallback time on the first frame,
// and if isToken is true, records the effective TTFT. It uses a fast-path
// read check to return immediately with zero write lock contention once TTFT is set
// or when subsequent non-token metadata frames arrive after the first packet.
func (r *UsageReporter) ObserveTokenEvent(isToken bool) {
	if r == nil {
		return
	}
	r.ttftMu.RLock()
	if r.ttftSet {
		r.ttftMu.RUnlock()
		return
	}
	start := r.ttftStart
	alreadyRecordedPacket := r.firstPacketSet
	r.ttftMu.RUnlock()

	if start.IsZero() {
		return
	}

	if !isToken && alreadyRecordedPacket {
		return
	}

	r.ttftMu.Lock()
	defer r.ttftMu.Unlock()
	if r.ttftSet {
		return
	}
	if !r.firstPacketSet {
		r.firstPacketDuration = time.Since(start)
		r.firstPacketSet = true
	}
	if isToken {
		r.ttft = time.Since(start)
		r.ttftSet = true
		r.ttftStart = time.Time{}
	}
}

func (r *UsageReporter) MarkFirstResponseByte() {
	if r == nil {
		return
	}
	r.ttftMu.Lock()
	if r.ttftSet {
		r.ttftMu.Unlock()
		return
	}
	start := r.ttftStart
	r.ttftStart = time.Time{}
	r.ttftMu.Unlock()
	if start.IsZero() {
		return
	}
	r.setTTFT(time.Since(start))
}

func (r *UsageReporter) setTTFT(ttft time.Duration) {
	if r == nil {
		return
	}
	if ttft < 0 {
		ttft = 0
	}
	r.ttftMu.Lock()
	if r.ttftSet {
		r.ttftMu.Unlock()
		return
	}
	r.ttft = ttft
	r.ttftSet = true
	r.ttftStart = time.Time{}
	r.ttftMu.Unlock()
}

func (r *UsageReporter) ttftDuration() time.Duration {
	if r == nil {
		return 0
	}
	r.ttftMu.RLock()
	defer r.ttftMu.RUnlock()
	if r.ttftSet {
		return r.ttft
	}
	if r.firstPacketSet {
		return r.firstPacketDuration
	}
	return 0
}

// ObserveResponseModel stores the model reported by an upstream response or event and
// ignores payloads without one; the substitution warning is emitted at publish time.
// Ported from upstream e9463ff5a795.
func (r *UsageReporter) ObserveResponseModel(payload []byte) {
	if r == nil || r.responseModelFinal.Load() {
		return
	}
	provider := ""
	if r != nil {
		provider = r.provider
	}
	served, terminal := extractResponseModelEvent(payload, provider)
	if served == "" {
		if terminal {
			r.responseModelFinal.Store(true)
		}
		return
	}
	r.responseModelMu.Lock()
	r.responseModel = served
	r.responseModelMu.Unlock()
	if terminal {
		r.responseModelFinal.Store(true)
	}
}

// ObserveCodexResponseModel stores the model reported by a codex upstream event and
// ignores payloads without one; the substitution warning is emitted at publish time.
func (r *UsageReporter) ObserveCodexResponseModel(payload []byte) {
	r.ObserveResponseModel(payload)
}

// SetResponseModel sets the reported model directly if valid and not already marked final.
func (r *UsageReporter) SetResponseModel(model string) {
	if r == nil || r.responseModelFinal.Load() {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" || len(model) > maxResponseModelLength {
		return
	}
	r.responseModelMu.Lock()
	r.responseModel = model
	r.responseModelMu.Unlock()
}

// SetUpstreamModel records the upstream model expected to be served when it differs
// from the requested model (e.g. due to provider-specific mapping or canonicalization).
// Model substitution detection compares the response against this upstream model,
// while usage accounting preserves the client's requested model.
func (r *UsageReporter) SetUpstreamModel(model string) {
	if r == nil {
		return
	}
	r.upstreamModelMu.Lock()
	r.upstreamModel = strings.TrimSpace(model)
	r.upstreamModelMu.Unlock()
}

// UpstreamModel returns the expected upstream model, or an empty string if not explicitly set.
func (r *UsageReporter) UpstreamModel() string {
	if r == nil {
		return ""
	}
	r.upstreamModelMu.RLock()
	defer r.upstreamModelMu.RUnlock()
	return r.upstreamModel
}

// IsResponseModelFinal reports whether the response model was already finalized by a terminal event.
func (r *UsageReporter) IsResponseModelFinal() bool {
	return r != nil && r.responseModelFinal.Load()
}

// warnModelSubstitution warns about a silent upstream model swap, throttled per
// credential and model pair, and labels the credential by index only, never by account.
func (r *UsageReporter) warnModelSubstitution(ctx context.Context) {
	if r == nil {
		return
	}
	served := r.ResponseModel()
	expectedModel := r.UpstreamModel()
	if expectedModel == "" {
		expectedModel = r.model
	}
	if served == "" || !IsModelSubstituted(expectedModel, served) {
		return
	}
	if r.model != "" && !IsModelSubstituted(r.model, served) {
		return
	}
	// The throttle key uses the same normalized names as the substitution check, so
	// aliases of one pair share a window instead of each warning on its own.
	requested := normalizeModelName(expectedModel)
	servedNormalized := normalizeModelName(served)
	providerName := r.provider
	if providerName == "" {
		providerName = "codex"
	}
	if !codexModelSubstitutionWarns.allow(codexModelSubstitutionKey{
		provider:  providerName,
		authID:    r.authID,
		requested: requested,
		served:    servedNormalized,
	}) {
		return
	}
	LogWithRequestID(ctx).Warnf("%s executor: upstream served model %q for requested model %q (auth_index=%s)", providerName, served, r.model, r.authIndexForLog())
}

// warnCodexModelSubstitution warns about a silent upstream model swap, throttled per
// credential and model pair, and labels the credential by index only, never by account.
func (r *UsageReporter) warnCodexModelSubstitution(ctx context.Context) {
	r.warnModelSubstitution(ctx)
}

// authIndexForLog labels the credential without exposing its file name or account.
func (r *UsageReporter) authIndexForLog() string {
	if r == nil {
		return "nil"
	}
	if authIndex := strings.TrimSpace(r.authIndex); authIndex != "" {
		return authIndex
	}
	return "nil"
}

// ResponseModel returns the latest model reported by the upstream response.
func (r *UsageReporter) ResponseModel() string {
	if r == nil {
		return ""
	}
	r.responseModelMu.RLock()
	defer r.responseModelMu.RUnlock()
	return r.responseModel
}

func (r *UsageReporter) Publish(ctx context.Context, detail usage.Detail) {
	r.publishWithOutcome(ctx, detail, false, usage.Failure{})
}

func (r *UsageReporter) PublishAdditionalModel(ctx context.Context, model string, detail usage.Detail) {
	record, ok := r.buildAdditionalModelRecord(model, detail)
	if !ok {
		return
	}
	r.publishRecord(ctx, record)
}

func (r *UsageReporter) buildAdditionalModelRecord(model string, detail usage.Detail) (usage.Record, bool) {
	if r == nil {
		return usage.Record{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return usage.Record{}, false
	}
	detail = normalizeUsageDetailTotal(detail)
	if !hasNonZeroTokenUsage(detail) {
		return usage.Record{}, false
	}
	rec := r.buildRecordForModel(model, detail, false, usage.Failure{})
	rec.RequestID = uuid.NewString()
	return rec, true
}

func (r *UsageReporter) PublishFailure(ctx context.Context, errs ...error) {
	r.publishWithOutcome(ctx, usage.Detail{}, true, failFromErrors(errs...))
}

func (r *UsageReporter) TrackFailure(ctx context.Context, errPtr *error) {
	if r == nil || errPtr == nil {
		return
	}
	if *errPtr != nil {
		r.PublishFailure(ctx, *errPtr)
	}
}

func (r *UsageReporter) publishWithOutcome(ctx context.Context, detail usage.Detail, failed bool, fail usage.Failure) {
	if r == nil {
		return
	}
	detail = normalizeUsageDetailTotal(detail)
	r.once.Do(func() {
		r.publishAttemptRecord(ctx, r.buildRecord(detail, failed, fail))
	})
}

func normalizeUsageDetailTotal(detail usage.Detail) usage.Detail {
	if detail.TotalTokens == 0 {
		total := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
		if total > 0 {
			detail.TotalTokens = total
		}
	}
	return detail
}

func hasNonZeroTokenUsage(detail usage.Detail) bool {
	return detail.InputTokens != 0 ||
		detail.OutputTokens != 0 ||
		detail.ReasoningTokens != 0 ||
		detail.CachedTokens != 0 ||
		detail.CacheReadTokens != 0 ||
		detail.CacheCreationTokens != 0 ||
		detail.TotalTokens != 0
}

// ensurePublished guarantees that a usage record is emitted exactly once.
// It is safe to call multiple times; only the first call wins due to once.Do.
// This is used to ensure request counting even when upstream responses do not
// include any usage fields (tokens), especially for streaming paths.
func (r *UsageReporter) EnsurePublished(ctx context.Context) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.publishAttemptRecord(ctx, r.buildRecord(usage.Detail{}, false, usage.Failure{}))
	})
}

// publishAttemptRecord emits the record for one upstream attempt and the
// observability warnings that belong to the attempt rather than to a single event.
func (r *UsageReporter) publishAttemptRecord(ctx context.Context, record usage.Record) {
	r.publishRecord(ctx, record)
	r.warnModelSubstitution(ctx)
}

func (r *UsageReporter) publishRecord(ctx context.Context, record usage.Record) {
	record.ResponseHeaders = internallogging.GetResponseHeaders(ctx)
	usage.PublishRecord(ctx, record)
}

// RequestID returns the execution instance request ID for this reporter.
func (r *UsageReporter) RequestID() string {
	if r == nil {
		return ""
	}
	return r.requestID
}

// TraceID returns the parent inbound request ID for this reporter.
func (r *UsageReporter) TraceID() string {
	if r == nil {
		return ""
	}
	return r.traceID
}

func (r *UsageReporter) buildRecord(detail usage.Detail, failed bool, failures ...usage.Failure) usage.Record {
	var fail usage.Failure
	if len(failures) > 0 {
		fail = failures[0]
	}
	if r == nil {
		return usage.Record{Detail: detail, Failed: failed, Fail: fail}
	}
	return r.buildRecordForModel(r.model, detail, failed, fail)
}

func (r *UsageReporter) buildRecordForModel(model string, detail usage.Detail, failed bool, fail usage.Failure) usage.Record {
	if r == nil {
		return usage.Record{Model: model, Detail: detail, Failed: failed, Fail: fail}
	}
	// Additional-model records describe a side model (image generation tool usage) that
	// the upstream response model never refers to, so they must stay empty.
	responseModel := ""
	if model == r.model {
		responseModel = r.ResponseModel()
	}
	return usage.Record{
		RequestID:            r.requestID,
		TraceID:              r.traceID,
		Provider:             r.provider,
		ExecutorType:         r.executorType,
		Model:                model,
		Alias:                r.alias,
		Source:               r.source,
		APIKey:               r.apiKey,
		SessionID:            r.sessionID,
		ParentSessionID:      r.parentSessionID,
		AuthID:               r.authID,
		AuthIndex:            r.authIndex,
		AccessTokenSHA256:    r.accessToken,
		AuthType:             r.authType,
		ReasoningEffort:      r.reasoning,
		EffectiveServiceTier: r.effectiveServiceTier,
		ResponseServiceTier:  strings.TrimSpace(detail.ResponseServiceTier),
		ResponseModel:        responseModel,
		Stream:               r.stream,
		RequestedAt:          r.requestedAt,
		Latency:              r.latency(),
		TTFT:                 r.ttftDuration(),
		Failed:               failed,
		Fail:                 fail,
		Detail:               detail,
	}
}

func failFromErrors(errs ...error) usage.Failure {
	for _, err := range errs {
		if err == nil {
			continue
		}
		body := strings.TrimSpace(err.Error())
		// Ported from upstream 9a2201c36a0a: prefer a marked upstream response
		// body over the flattened error text for usage failure records.
		type responseBodyProvider interface {
			ResponseBody() []byte
		}
		var responseErr responseBodyProvider
		if errors.As(err, &responseErr) && responseErr != nil {
			if responseBody := responseErr.ResponseBody(); len(responseBody) > 0 {
				body = string(responseBody)
			}
		}
		fail := usage.Failure{
			Body: body,
		}
		var se interface{ StatusCode() int }
		if errors.As(err, &se) && se != nil {
			fail.StatusCode = se.StatusCode()
		}
		if truncatedErr, ok := errors.AsType[cliproxyexecutor.StreamTruncatedError](err); ok && truncatedErr != nil && truncatedErr.IsStreamTruncated() {
			fail.Code = usage.FailureStreamTruncatedCode
		}
		return fail
	}
	return usage.Failure{}
}

func (r *UsageReporter) latency() time.Duration {
	if r == nil || r.requestedAt.IsZero() {
		return 0
	}
	latency := time.Since(r.requestedAt)
	if latency < 0 {
		return 0
	}
	return latency
}

type usageTTFTRoundTripper struct {
	base       http.RoundTripper
	reporter   *UsageReporter
	packetOnly bool
}

func (t usageTTFTRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.reporter.StartResponseTTFT()
	resp, errRoundTrip := t.base.RoundTrip(req)
	if errRoundTrip != nil {
		return resp, errRoundTrip
	}
	if t.packetOnly {
		t.reporter.ObserveResponsePacketOnly(resp)
	} else {
		t.reporter.ObserveResponse(resp)
	}
	return resp, nil
}

type usageTTFTReadCloser struct {
	io.ReadCloser
	once sync.Once
	mark func()
}

func (r *usageTTFTReadCloser) Read(p []byte) (int, error) {
	if r == nil || r.ReadCloser == nil {
		return 0, io.ErrClosedPipe
	}
	n, errRead := r.ReadCloser.Read(p)
	if n > 0 && r.mark != nil {
		r.once.Do(r.mark)
	}
	return n, errRead
}

func APIKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return ""
	}
	if v, exists := ginCtx.Get("userApiKey"); exists {
		switch value := v.(type) {
		case string:
			return value
		case fmt.Stringer:
			return value.String()
		default:
			return fmt.Sprintf("%v", value)
		}
	}
	return ""
}

func resolveUsageSource(auth *cliproxyauth.Auth, ctxAPIKey string) string {
	if auth != nil {
		provider := strings.TrimSpace(auth.Provider)
		if strings.EqualFold(provider, "gemini-cli") {
			if id := strings.TrimSpace(auth.ID); id != "" {
				return id
			}
		}
		if strings.EqualFold(provider, "vertex") {
			if auth.Metadata != nil {
				if projectID, ok := auth.Metadata["project_id"].(string); ok {
					if trimmed := strings.TrimSpace(projectID); trimmed != "" {
						return trimmed
					}
				}
				if project, ok := auth.Metadata["project"].(string); ok {
					if trimmed := strings.TrimSpace(project); trimmed != "" {
						return trimmed
					}
				}
			}
		}
		if _, value := auth.AccountInfo(); value != "" {
			return strings.TrimSpace(value)
		}
		if auth.Metadata != nil {
			if email, ok := auth.Metadata["email"].(string); ok {
				if trimmed := strings.TrimSpace(email); trimmed != "" {
					return trimmed
				}
			}
		}
		if auth.Attributes != nil {
			if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
				return key
			}
		}
	}
	if trimmed := strings.TrimSpace(ctxAPIKey); trimmed != "" {
		return trimmed
	}
	return ""
}

func resolveUsageAuthType(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	kind, _ := auth.AccountInfo()
	kind = strings.TrimSpace(kind)
	if kind == "api_key" {
		return "apikey"
	}
	return kind
}

func ParseCodexUsage(data []byte) (usage.Detail, bool) {
	usageNode := gjson.ParseBytes(data).Get("response.usage")
	if !hasOpenAIStyleUsageTokenFields(usageNode) {
		return usage.Detail{}, false
	}
	return parseOpenAIStyleUsageNode(usageNode), true
}

func ParseCodexImageToolUsage(data []byte) (usage.Detail, bool) {
	usageNode := gjson.ParseBytes(data).Get("response.tool_usage.image_gen")
	if !hasOpenAIStyleUsageTokenFields(usageNode) {
		return usage.Detail{}, false
	}
	return parseOpenAIStyleUsageNode(usageNode), true
}

func ParseOpenAIUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !hasOpenAIStyleUsageTokenFields(usageNode) {
		return usage.Detail{}
	}
	return parseOpenAIStyleUsageNode(usageNode)
}

func hasOpenAIStyleUsageTokenFields(usageNode gjson.Result) bool {
	if !usageNode.Exists() || !usageNode.IsObject() {
		return false
	}
	return usageNode.Get("prompt_tokens").Exists() ||
		usageNode.Get("input_tokens").Exists() ||
		usageNode.Get("completion_tokens").Exists() ||
		usageNode.Get("output_tokens").Exists() ||
		usageNode.Get("total_tokens").Exists() ||
		usageNode.Get("prompt_tokens_details.cached_tokens").Exists() ||
		usageNode.Get("input_tokens_details.cached_tokens").Exists() ||
		usageNode.Get("completion_tokens_details.reasoning_tokens").Exists() ||
		usageNode.Get("output_tokens_details.reasoning_tokens").Exists() ||
		usageNode.Get("reasoning_tokens").Exists()
}

func parseOpenAIStyleUsageNode(usageNode gjson.Result) usage.Detail {
	inputNode := usageNode.Get("prompt_tokens")
	if !inputNode.Exists() {
		inputNode = usageNode.Get("input_tokens")
	}
	outputNode := usageNode.Get("completion_tokens")
	if !outputNode.Exists() {
		outputNode = usageNode.Get("output_tokens")
	}
	detail := usage.Detail{
		InputTokens:  inputNode.Int(),
		OutputTokens: outputNode.Int(),
		TotalTokens:  usageNode.Get("total_tokens").Int(),
	}
	cached := usageNode.Get("prompt_tokens_details.cached_tokens")
	if !cached.Exists() {
		cached = usageNode.Get("input_tokens_details.cached_tokens")
	}
	if cached.Exists() {
		detail.CachedTokens = cached.Int()
	}
	reasoning := usageNode.Get("completion_tokens_details.reasoning_tokens")
	if !reasoning.Exists() {
		reasoning = usageNode.Get("output_tokens_details.reasoning_tokens")
	}
	if !reasoning.Exists() {
		// Flat dialect (e.g. SGLang): usage.reasoning_tokens at top level.
		reasoning = usageNode.Get("reasoning_tokens")
	}
	if reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	return detail
}

func ParseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !hasOpenAIStyleUsageTokenFields(usageNode) {
		return usage.Detail{}, false
	}
	return parseOpenAIStyleUsageNode(usageNode), true
}

func ParseClaudeUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	return parseClaudeUsageNode(usageNode)
}

func ParseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	return parseClaudeUsageNode(usageNode), true
}

func parseClaudeUsageNode(usageNode gjson.Result) usage.Detail {
	cacheReadTokens := usageNode.Get("cache_read_input_tokens").Int()
	cacheCreationTokens := usageNode.Get("cache_creation_input_tokens").Int()
	detail := usage.Detail{
		InputTokens:         usageNode.Get("input_tokens").Int(),
		OutputTokens:        usageNode.Get("output_tokens").Int(),
		CachedTokens:        cacheReadTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: cacheCreationTokens,
	}
	if detail.CachedTokens == 0 {
		detail.CachedTokens = detail.CacheCreationTokens
	}
	detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	// Anthropic reports the speed a response was served and billed at in
	// usage.speed ("fast" or "standard"): fast mode costs more, and a fast
	// request on a model without it runs, and is billed, at standard. Carried as
	// the response tier so usage sinks can price it.
	if speed := strings.TrimSpace(usageNode.Get("speed").String()); speed != "" {
		detail.ResponseServiceTier = speed
	}
	return detail
}

func parseGeminiFamilyUsageDetail(node gjson.Result) usage.Detail {
	detail := usage.Detail{
		InputTokens:     node.Get("promptTokenCount").Int(),
		OutputTokens:    node.Get("candidatesTokenCount").Int(),
		ReasoningTokens: node.Get("thoughtsTokenCount").Int(),
		TotalTokens:     node.Get("totalTokenCount").Int(),
		CachedTokens:    node.Get("cachedContentTokenCount").Int(),
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	return detail
}

func hasGeminiFamilyUsageTokenFields(node gjson.Result) bool {
	return node.Get("promptTokenCount").Exists() ||
		node.Get("candidatesTokenCount").Exists() ||
		node.Get("thoughtsTokenCount").Exists() ||
		node.Get("totalTokenCount").Exists() ||
		node.Get("cachedContentTokenCount").Exists()
}

func ParseGeminiCLIUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := firstExistingUsageNode(usageNode,
		"response.usageMetadata",
		"response.usage_metadata",
		"usageMetadata",
		"usage_metadata",
	)
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func ParseGeminiUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func ParseGeminiStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func ParseGeminiCLIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	root := gjson.ParseBytes(payload)
	node := firstExistingUsageNode(root,
		"response.usageMetadata",
		"response.usage_metadata",
		"usageMetadata",
		"usage_metadata",
	)
	if !node.Exists() {
		return usage.Detail{}, false
	}
	if !hasGeminiFamilyUsageTokenFields(node) {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func firstExistingUsageNode(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		node := root.Get(path)
		if node.Exists() {
			return node
		}
	}
	return gjson.Result{}
}

// ParseInteractionsUsage extracts a usage.Detail from an intermediate interactions
// JSON payload (non-streaming interaction document or a streaming interaction.completed event).
// Ported from upstream CLIProxyAPI helps/usage_helpers.go (f94752762bb9 at v7.3.3);
// adapted to the local usage.Detail, which has no TokenBreakdown field.
func ParseInteractionsUsage(data []byte) usage.Detail {
	root := gjson.ParseBytes(data)
	node := firstExistingUsageNode(root, "usage", "total_usage", "metadata.total_usage", "metadata.usage", "usageMetadata", "usage_metadata", "interaction.usage", "interaction.total_usage", "interaction.metadata.total_usage")
	if !node.Exists() {
		return usage.Detail{}
	}
	if node.Get("promptTokenCount").Exists() || node.Get("candidatesTokenCount").Exists() {
		return parseGeminiFamilyUsageDetail(node)
	}
	return parseInteractionsUsageDetail(node)
}

func parseInteractionsUsageDetail(node gjson.Result) usage.Detail {
	cacheRead := firstExistingUsageNode(node, "cache_read_input_tokens", "cache_read_tokens", "cacheReadTokens")
	cacheWrite := firstExistingUsageNode(node, "cache_creation_input_tokens", "cache_creation_tokens", "cacheCreationTokens", "cache_write_tokens", "cacheWriteTokens")
	toolUseTokens := firstExistingUsageNode(node, "tool_use_tokens", "total_tool_use_tokens", "toolUseTokens", "totalToolUseTokens").Int()
	detail := usage.Detail{
		OutputTokens:        firstExistingUsageNode(node, "output_tokens", "completion_tokens", "total_output_tokens").Int(),
		ReasoningTokens:     firstExistingUsageNode(node, "reasoning_tokens", "thoughtsTokenCount", "total_thought_tokens").Int(),
		TotalTokens:         firstExistingUsageNode(node, "total_tokens", "totalTokenCount").Int(),
		CachedTokens:        firstExistingUsageNode(node, "cached_tokens", "cachedContentTokenCount", "total_cached_tokens").Int(),
		CacheReadTokens:     cacheRead.Int(),
		CacheCreationTokens: cacheWrite.Int(),
	}
	if !cacheRead.Exists() && detail.CachedTokens > 0 {
		detail.CacheReadTokens = detail.CachedTokens
	}
	// total_input_tokens/prompt_tokens are cache-inclusive totals, so the uncached
	// input portion is the total minus the effective cache volume (cache_read +
	// cache_creation). A verbatim input_tokens field is already uncached and is
	// preferred when present. Mirrors upstream e54a8e97 setClaudeUsageFromInteractions.
	totalCache := detail.CacheReadTokens + detail.CacheCreationTokens
	if inNode := node.Get("input_tokens"); inNode.Exists() {
		detail.InputTokens = inNode.Int() + toolUseTokens
	} else if totalNode := firstExistingUsageNode(node, "total_input_tokens", "prompt_tokens"); totalNode.Exists() {
		if total := totalNode.Int(); total >= totalCache {
			detail.InputTokens = total - totalCache + toolUseTokens
		} else {
			detail.InputTokens = toolUseTokens
		}
	} else {
		detail.InputTokens = toolUseTokens
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	return detail
}

// ParseInteractionsStreamUsage extracts a usage.Detail from a single interactions stream
// event payload (SSE data line or raw JSON), reporting false when no token fields are present.
// Ported from upstream CLIProxyAPI helps/usage_helpers.go (f94752762bb9 at v7.3.3).
func ParseInteractionsStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 {
		payload = line
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	detail := ParseInteractionsUsage(payload)
	if !hasNonZeroTokenUsage(detail) {
		return usage.Detail{}, false
	}
	return detail, true
}

func ParseAntigravityUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("response.usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("usageMetadata")
	}
	if !node.Exists() {
		node = usageNode.Get("usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func ParseAntigravityStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "response.usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usageMetadata")
	}
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

var stopChunkWithoutUsage sync.Map

func rememberStopWithoutUsage(traceID string) {
	stopChunkWithoutUsage.Store(traceID, struct{}{})
	time.AfterFunc(10*time.Minute, func() { stopChunkWithoutUsage.Delete(traceID) })
}

// FilterSSEUsageMetadata removes usageMetadata from SSE events that are not
// terminal (finishReason != "stop"). Stop chunks are left untouched. This
// function is shared between aistudio and antigravity executors.
func FilterSSEUsageMetadata(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}

	lines := bytes.Split(payload, []byte("\n"))
	modified := false
	foundData := false
	for idx, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		foundData = true
		dataIdx := bytes.Index(line, []byte("data:"))
		if dataIdx < 0 {
			continue
		}
		rawJSON := bytes.TrimSpace(line[dataIdx+5:])
		traceID := gjson.GetBytes(rawJSON, "traceId").String()
		if isStopChunkWithoutUsage(rawJSON) && traceID != "" {
			rememberStopWithoutUsage(traceID)
			continue
		}
		if traceID != "" {
			if _, ok := stopChunkWithoutUsage.Load(traceID); ok && hasUsageMetadata(rawJSON) {
				stopChunkWithoutUsage.Delete(traceID)
				continue
			}
		}

		cleaned, changed := StripUsageMetadataFromJSON(rawJSON)
		if !changed {
			continue
		}
		var rebuilt []byte
		rebuilt = append(rebuilt, line[:dataIdx]...)
		rebuilt = append(rebuilt, []byte("data:")...)
		if len(cleaned) > 0 {
			rebuilt = append(rebuilt, ' ')
			rebuilt = append(rebuilt, cleaned...)
		}
		lines[idx] = rebuilt
		modified = true
	}
	if !modified {
		if !foundData {
			// Handle payloads that are raw JSON without SSE data: prefix.
			trimmed := bytes.TrimSpace(payload)
			cleaned, changed := StripUsageMetadataFromJSON(trimmed)
			if !changed {
				return payload
			}
			return cleaned
		}
		return payload
	}
	return bytes.Join(lines, []byte("\n"))
}

// StripUsageMetadataFromJSON drops usageMetadata unless finishReason is present (terminal).
// It handles both formats:
// - Aistudio: candidates.0.finishReason
// - Antigravity: response.candidates.0.finishReason
func StripUsageMetadataFromJSON(rawJSON []byte) ([]byte, bool) {
	jsonBytes := bytes.TrimSpace(rawJSON)
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return rawJSON, false
	}

	// Check for finishReason in both aistudio and antigravity formats
	finishReason := gjson.GetBytes(jsonBytes, "candidates.0.finishReason")
	if !finishReason.Exists() {
		finishReason = gjson.GetBytes(jsonBytes, "response.candidates.0.finishReason")
	}
	terminalReason := finishReason.Exists() && strings.TrimSpace(finishReason.String()) != ""

	usageMetadata := gjson.GetBytes(jsonBytes, "usageMetadata")
	if !usageMetadata.Exists() {
		usageMetadata = gjson.GetBytes(jsonBytes, "response.usageMetadata")
	}

	// Terminal chunk: keep as-is.
	if terminalReason {
		return rawJSON, false
	}

	// Nothing to strip
	if !usageMetadata.Exists() {
		return rawJSON, false
	}

	// Remove usageMetadata from both possible locations
	cleaned := jsonBytes
	var changed bool

	if usageMetadata = gjson.GetBytes(cleaned, "usageMetadata"); usageMetadata.Exists() {
		// Rename usageMetadata to cpaUsageMetadata in the message_start event of Claude
		cleaned, _ = sjson.SetRawBytes(cleaned, "cpaUsageMetadata", []byte(usageMetadata.Raw))
		cleaned, _ = sjson.DeleteBytes(cleaned, "usageMetadata")
		changed = true
	}

	if usageMetadata = gjson.GetBytes(cleaned, "response.usageMetadata"); usageMetadata.Exists() {
		// Rename usageMetadata to cpaUsageMetadata in the message_start event of Claude
		cleaned, _ = sjson.SetRawBytes(cleaned, "response.cpaUsageMetadata", []byte(usageMetadata.Raw))
		cleaned, _ = sjson.DeleteBytes(cleaned, "response.usageMetadata")
		changed = true
	}

	return cleaned, changed
}

func hasUsageMetadata(jsonBytes []byte) bool {
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return false
	}
	if gjson.GetBytes(jsonBytes, "usageMetadata").Exists() {
		return true
	}
	if gjson.GetBytes(jsonBytes, "response.usageMetadata").Exists() {
		return true
	}
	return false
}

func isStopChunkWithoutUsage(jsonBytes []byte) bool {
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return false
	}
	finishReason := gjson.GetBytes(jsonBytes, "candidates.0.finishReason")
	if !finishReason.Exists() {
		finishReason = gjson.GetBytes(jsonBytes, "response.candidates.0.finishReason")
	}
	trimmed := strings.TrimSpace(finishReason.String())
	if !finishReason.Exists() || trimmed == "" {
		return false
	}
	return !hasUsageMetadata(jsonBytes)
}

func JSONPayload(line []byte) []byte {
	return jsonPayload(line)
}

func jsonPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	return trimmed
}
