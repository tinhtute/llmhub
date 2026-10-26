package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
	claudeauth "github.com/therealtinhtute/llmhub/internal/auth/claude"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/misc"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/therealtinhtute/llmhub/internal/util"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

// ClaudeExecutor is a stateless executor for Anthropic Claude over the messages API.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg *config.Config
}

// claudeToolPrefix is empty to match real Claude Code behavior (no tool name prefix).
// Previously "proxy_" was used but this is a detectable fingerprint difference.
const claudeToolPrefix = ""

// oauthToolRenameMap maps OpenCode-style (lowercase) tool names to Claude Code-style
// (TitleCase) names. Anthropic uses tool name fingerprinting to detect third-party
// clients on OAuth traffic. Renaming to official names avoids extra-usage billing.
// All tools are mapped to TitleCase equivalents to match Claude Code naming patterns.
var oauthToolRenameMap = map[string]string{
	"bash":         "Bash",
	"read":         "Read",
	"write":        "Write",
	"edit":         "Edit",
	"glob":         "Glob",
	"grep":         "Grep",
	"task":         "Task",
	"webfetch":     "WebFetch",
	"todowrite":    "TodoWrite",
	"question":     "Question",
	"skill":        "Skill",
	"ls":           "LS",
	"todoread":     "TodoRead",
	"notebookedit": "NotebookEdit",
}

// The reverse map is now computed per-request in remapOAuthToolNames so that
// only names the client actually caused us to rewrite are restored on the
// response. A global reverse map — as used previously — corrupted responses
// for clients that sent mixed casing (for example, `Bash` TitleCase
// alongside `glob` lowercase; the request flagged renames via `glob→Glob`,
// then the global reverse map incorrectly rewrote every `Bash` in the
// response to `bash`, causing the client to reject the tool_use as unknown).

// oauthToolsToRemove lists tool names that must be stripped from OAuth requests
// even after remapping. Currently empty — all tools are mapped instead of removed.
var oauthToolsToRemove = map[string]bool{}

// Anthropic-compatible upstreams may reject or even crash when Claude models
// omit max_tokens. Prefer registered model metadata before using a fallback.
const defaultModelMaxTokens = 1024

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

func (e *ClaudeExecutor) Identifier() string { return "claude" }

// modelLevelCooling reports whether quota cooldowns are scoped to the requested
// model rather than the whole credential. Upstream reads
// cfg.Claude.ModelLevelCooling (44eaef0009f8); this repository's flat schema
// exposes it as cfg.ClaudeModelLevelCooling.
func (e *ClaudeExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.ClaudeModelLevelCooling
}

func (e *ClaudeExecutor) injectDiagnosticsEnabled() bool {
	return e == nil || e.cfg == nil || e.cfg.ClaudeHeaderDefaults.InjectDiagnostics == nil || *e.cfg.ClaudeHeaderDefaults.InjectDiagnostics
}

// isClaudeCallerOwned reports whether the request preserves caller-supplied
// headers and betas instead of synthesizing the full Claude Code CLI profile.
// Real Claude OAuth tokens, credentials with fingerprint_profile="claude-code-cli",
// and credentials with cloak_mode="always" use the CLI profile. An explicit
// caller_owned attribute ("true" / "false") or cloak_mode="never" on an API-key
// credential explicitly marks the credential as caller-owned or CLI-profile.
func isClaudeCallerOwned(auth *cliproxyauth.Auth, apiKey string) bool {
	if isClaudeOAuthToken(apiKey) {
		return false
	}
	if auth != nil && auth.Attributes != nil {
		if v, ok := auth.Attributes["caller_owned"]; ok {
			return strings.EqualFold(strings.TrimSpace(v), "true")
		}
		if strings.EqualFold(strings.TrimSpace(auth.Attributes["cloak_mode"]), "never") {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(auth.Attributes["fingerprint_profile"]), "claude-code-cli") {
			return false
		}
		if strings.EqualFold(strings.TrimSpace(auth.Attributes["cloak_mode"]), "always") {
			return false
		}
	}
	return false
}

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := claudeCreds(auth)
	isConfigAPIKey := auth != nil && auth.Attributes != nil && (strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "apikey") || strings.TrimSpace(auth.Attributes["api_key"]) != "")
	useAPIKey := auth != nil && isConfigAPIKey
	isAnthropicBase := req.URL != nil && strings.EqualFold(req.URL.Scheme, "https") && strings.EqualFold(req.URL.Host, "api.anthropic.com")
	if strings.TrimSpace(apiKey) != "" {
		if isAnthropicBase && useAPIKey {
			req.Header.Del("Authorization")
			req.Header.Set("x-api-key", apiKey)
		} else {
			req.Header.Del("x-api-key")
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else {
		// base_URL-only credential: drop both auth headers instead of leaving
		// whatever the inbound request carried.
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewUtlsHTTPClient(e.cfg, auth, 0)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}
	return httpClient.Do(httpReq)
}

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	// Upstream request continuity context (cc_prev_req/cc_prompt_id) and the
	// resolved incoming headers travel on ctx so cloaking can read and fill
	// them (upstream 086ad91bd970).
	incomingHeaders := resolveIncomingClaudeHeaders(ctx, opts.Headers)
	claudeSessionID := helps.ExtractClaudeCodeSessionID(ctx, originalPayload, incomingHeaders)

	continuityCtx := &helps.ClaudeContinuityContext{}
	ctx = helps.WithClaudeContinuityContext(ctx, continuityCtx)
	ctx = helps.WithIncomingHeaders(ctx, incomingHeaders)
	ctx = helps.WithClaudeExecutionMetadata(ctx, helps.ClaudeRequestHasExecutionMetadata(opts.Metadata, req.Metadata))
	if claudeSessionID != "" {
		ctx = helps.WithClaudeSessionID(ctx, claudeSessionID)
	}

	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, stream)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	bodyBeforeCloaking := body
	isProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(bodyBeforeCloaking)
	var cloaked bool
	body, cloaked = applyCloaking(ctx, e.cfg, auth, body, baseModel, apiKey, baseURL)
	fableState := captureClaudeCodeFableState(bodyBeforeCloaking, body, cloaked)

	// Diagnostics + upstream request continuity state: only committed after a
	// complete successful upstream response (upstream 086ad91bd970,
	// 4a5ab534f827).
	diagnosticsState := claudeDiagnosticsRequestState{}
	if !isProbeOrHelper {
		isProbeOrHelper = helps.IsClaudeProbeOrHelperRequest(body)
	}
	if continuityCtx.Initialized {
		diagnosticsState = claudeDiagnosticsRequestState{
			key:      continuityCtx.Key,
			sequence: continuityCtx.Sequence,
			promptID: continuityCtx.PromptID,
		}
	}
	diagnosticsInjectedByCPA := false
	oauthToken := isClaudeOAuthToken(apiKey)
	if cloaked && oauthToken && isAnthropicUpstreamBase(baseURL) && !isProbeOrHelper && e.injectDiagnosticsEnabled() {
		diagnosticsInjectedByCPA = true
		if continuityCtx.Initialized {
			body, diagnosticsState = injectClaudeDiagnosticsWithState(body, continuityCtx.Key, continuityCtx.Sequence, continuityCtx.PreviousMessageID, continuityCtx.PromptID)
		} else {
			body, diagnosticsState = injectClaudeDiagnostics(body, auth, claudeSessionID)
		}
	}

	body = applyClaudePreservedThinkingBinding(body)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	var touchedPayloadPaths map[string]bool
	body, touchedPayloadPaths = helps.ApplyPayloadConfigWithTrackedPaths(
		e.cfg,
		baseModel,
		to.String(),
		from.String(),
		"",
		body,
		originalTranslated,
		requestedModel,
		requestPath,
		opts.Headers,
		"fallbacks",
		"thinking.display",
		"diagnostics",
	)

	// Post-payload probe/helper reclassification (upstream 4a5ab534f827): a
	// request that became a probe drops CPA continuity and diagnostics; one
	// that stopped being a probe restores them.
	wasProbeOrHelper := isProbeOrHelper
	isProbeOrHelper = helps.IsClaudeProbeOrHelperRequest(body)
	if isProbeOrHelper {
		diagnosticsState = claudeDiagnosticsRequestState{}
		if diagnosticsInjectedByCPA && !touchedPayloadPaths["diagnostics"] {
			body, _ = sjson.DeleteBytes(body, "diagnostics")
		}
		if cloaked {
			body = helps.StripClaudeBillingTags(body)
		}
		if continuityCtx != nil {
			*continuityCtx = helps.ClaudeContinuityContext{}
		}
	} else if wasProbeOrHelper {
		if cloaked {
			existingPrevReq, existingPromptID := helps.ExtractClaudeBillingTags(body)
			prevReq, promptID, cCtx, ok := resolveClaudeContinuityTags(ctx, auth, incomingHeaders, body, false, existingPrevReq, existingPromptID)
			if ok {
				if continuityCtx != nil {
					*continuityCtx = cCtx
				}
				body = helps.InjectClaudeBillingTags(body, prevReq, promptID)
				if oauthToken && isAnthropicUpstreamBase(baseURL) && e.injectDiagnosticsEnabled() {
					body, diagnosticsState = injectClaudeDiagnosticsWithState(body, cCtx.Key, cCtx.Sequence, cCtx.PreviousMessageID, promptID)
				}
			}
		}
	}
	body = reconcileClaudeCodeFableModelAfterPayload(
		body,
		fableState,
		touchedPayloadPaths["fallbacks"],
		touchedPayloadPaths["thinking.display"],
		cloaked,
		isProbeOrHelper,
	)
	body = ensureModelMaxTokens(body, baseModel)

	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeTemperatureForThinking(body)

	// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
	cpaOwnsCacheControl := cloaked || countCacheControls(body) == 0
	if cpaOwnsCacheControl {
		body = ensureCacheControl(body)
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	// Cloaking and ensureCacheControl may push the total over 4 when the client
	// already sends multiple cache_control blocks.
	body = enforceCacheControlLimit(body, 4)

	// Native selects the 1h cache pool only for OAuth credentials and pairs it
	// with extended-cache-ttl-2025-04-11, which claudeCodeCLIBetas emits on the
	// same credential condition. Subagents default to 5m unless 1h is
	// explicitly requested; probes omit both (upstream d7052c96af78).
	isSubagent := helps.IsClaudeSubagentRequest(incomingHeaders, body)
	subagent1h := isSubagent && helps.ClaudeSubagentRequests1h(incomingHeaders, body)
	if cpaOwnsCacheControl && oauthToken && (!isSubagent || subagent1h) && !isProbeOrHelper {
		body = upgradeClaudeCacheControlTTL(body, claudeCacheControlTTL1h)
	} else if isProbeOrHelper || (isSubagent && !subagent1h) {
		body = stripClaudeCacheControlTTL(body)
	}

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	// A 1h-TTL block must not appear after a 5m-TTL block in evaluation order (tools→system→messages).
	body = normalizeCacheControlTTL(body)

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	var oauthToolNamesReverseMap map[string]string
	if oauthToken {
		bodyForUpstream, oauthToolNamesReverseMap = prepareClaudeOAuthToolNamesForUpstream(ctx, bodyForUpstream, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	// Enable cch signing by default for OAuth tokens (not just experimental flag).
	// Claude Code always computes cch; missing or invalid cch is a detectable fingerprint.
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth, baseURL) {
		// A cloaked request whose system survived without a billing header (for
		// example after payload rules replaced system) still needs the chain
		// for the signature to attach to (upstream 086ad91bd970).
		if claudeBodyNeedsBillingFallback(bodyForUpstream) {
			billing := gjson.GetBytes(bodyForUpstream, "system.0.text")
			if billing.Type != gjson.String || !strings.HasPrefix(billing.String(), "x-anthropic-billing-header:") {
				fallback := claudeCCHFallbackBillingHeader(ctx, e.cfg, bodyForUpstream, parseEntrypointFromUA(getClientUserAgent(ctx)))
				if fallback != "" {
					if updated, errPrepend := prependClaudeBillingSystemBlock(bodyForUpstream, fallback); errPrepend == nil {
						bodyForUpstream = updated
					}
				}
			}
		}
		bodyForUpstream = signAnthropicMessagesBody(bodyForUpstream)
	}

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return resp, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, bodyForUpstream, e.cfg, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      bodyForUpstream,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(e.cfg, auth, 0)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return resp, classifyClaudeUpstreamErrorWithCooling(httpResp.StatusCode, httpResp.Header, []byte(msg), e.modelLevelCooling())
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = classifyClaudeUpstreamErrorWithCooling(httpResp.StatusCode, httpResp.Header, b, e.modelLevelCooling())
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if stream {
		if errValidate := validateClaudeStreamingResponse(data); errValidate != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errValidate)
			return resp, errValidate
		}
		// Only a stream that reached message_stop advances continuity, so a
		// truncated response cannot corrupt upstream request tracking
		// (upstream 086ad91bd970).
		if msgID := claudeMessageIDFromSSE(data); msgID != "" {
			commitClaudeContinuity(diagnosticsState, msgID, helps.HeaderValueCaseInsensitive(httpResp.Header, "request-id"))
		}
		lines := bytes.Split(data, []byte("\n"))
		for i, line := range lines {
			reporter.ObserveResponseModel(line)
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			restoredLine, errRestore := restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
			if errRestore != nil {
				errRestore = fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore)
				helps.RecordAPIResponseError(ctx, e.cfg, errRestore)
				reporter.PublishFailure(ctx, errRestore)
				return resp, errRestore
			}
			lines[i] = restoredLine
		}
		data = bytes.Join(lines, []byte("\n"))
	} else {
		commitClaudeContinuity(diagnosticsState, claudeMessageIDFromResponse(data), helps.HeaderValueCaseInsensitive(httpResp.Header, "request-id"))
		reporter.ObserveResponseModel(data)
		reporter.Publish(ctx, helps.ParseClaudeUsage(data))
		var errRestore error
		data, errRestore = restoreClaudeOAuthToolNamesFromResponse(data, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
		if errRestore != nil {
			errRestore = fmt.Errorf("restore Claude OAuth tool name from response: %w", errRestore)
			helps.RecordAPIResponseError(ctx, e.cfg, errRestore)
			return resp, errRestore
		}
	}
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		to,
		from,
		req.Model,
		opts.OriginalRequest,
		bodyForTranslation,
		data,
		&param,
	)
	if from == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	incomingHeaders := resolveIncomingClaudeHeaders(ctx, opts.Headers)
	claudeSessionID := helps.ExtractClaudeCodeSessionID(ctx, originalPayload, incomingHeaders)

	continuityCtx := &helps.ClaudeContinuityContext{}
	ctx = helps.WithClaudeContinuityContext(ctx, continuityCtx)
	ctx = helps.WithIncomingHeaders(ctx, incomingHeaders)
	ctx = helps.WithClaudeExecutionMetadata(ctx, helps.ClaudeRequestHasExecutionMetadata(opts.Metadata, req.Metadata))
	if claudeSessionID != "" {
		ctx = helps.WithClaudeSessionID(ctx, claudeSessionID)
	}

	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	bodyBeforeCloaking := body
	isProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(bodyBeforeCloaking)
	var cloaked bool
	body, cloaked = applyCloaking(ctx, e.cfg, auth, body, baseModel, apiKey, baseURL)
	fableState := captureClaudeCodeFableState(bodyBeforeCloaking, body, cloaked)

	diagnosticsState := claudeDiagnosticsRequestState{}
	if !isProbeOrHelper {
		isProbeOrHelper = helps.IsClaudeProbeOrHelperRequest(body)
	}
	if continuityCtx.Initialized {
		diagnosticsState = claudeDiagnosticsRequestState{
			key:      continuityCtx.Key,
			sequence: continuityCtx.Sequence,
			promptID: continuityCtx.PromptID,
		}
	}
	diagnosticsInjectedByCPA := false
	oauthToken := isClaudeOAuthToken(apiKey)
	if cloaked && oauthToken && isAnthropicUpstreamBase(baseURL) && !isProbeOrHelper && e.injectDiagnosticsEnabled() {
		diagnosticsInjectedByCPA = true
		if continuityCtx.Initialized {
			body, diagnosticsState = injectClaudeDiagnosticsWithState(body, continuityCtx.Key, continuityCtx.Sequence, continuityCtx.PreviousMessageID, continuityCtx.PromptID)
		} else {
			body, diagnosticsState = injectClaudeDiagnostics(body, auth, claudeSessionID)
		}
	}

	body = applyClaudePreservedThinkingBinding(body)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	var touchedPayloadPaths map[string]bool
	body, touchedPayloadPaths = helps.ApplyPayloadConfigWithTrackedPaths(
		e.cfg,
		baseModel,
		to.String(),
		from.String(),
		"",
		body,
		originalTranslated,
		requestedModel,
		requestPath,
		opts.Headers,
		"fallbacks",
		"thinking.display",
		"diagnostics",
	)

	// Post-payload probe/helper reclassification (upstream 4a5ab534f827).
	wasProbeOrHelper := isProbeOrHelper
	isProbeOrHelper = helps.IsClaudeProbeOrHelperRequest(body)
	if isProbeOrHelper {
		diagnosticsState = claudeDiagnosticsRequestState{}
		if diagnosticsInjectedByCPA && !touchedPayloadPaths["diagnostics"] {
			body, _ = sjson.DeleteBytes(body, "diagnostics")
		}
		if cloaked {
			body = helps.StripClaudeBillingTags(body)
		}
		if continuityCtx != nil {
			*continuityCtx = helps.ClaudeContinuityContext{}
		}
	} else if wasProbeOrHelper {
		if cloaked {
			existingPrevReq, existingPromptID := helps.ExtractClaudeBillingTags(body)
			prevReq, promptID, cCtx, ok := resolveClaudeContinuityTags(ctx, auth, incomingHeaders, body, false, existingPrevReq, existingPromptID)
			if ok {
				if continuityCtx != nil {
					*continuityCtx = cCtx
				}
				body = helps.InjectClaudeBillingTags(body, prevReq, promptID)
				if oauthToken && isAnthropicUpstreamBase(baseURL) && e.injectDiagnosticsEnabled() {
					body, diagnosticsState = injectClaudeDiagnosticsWithState(body, cCtx.Key, cCtx.Sequence, cCtx.PreviousMessageID, promptID)
				}
			}
		}
	}
	body = reconcileClaudeCodeFableModelAfterPayload(
		body,
		fableState,
		touchedPayloadPaths["fallbacks"],
		touchedPayloadPaths["thinking.display"],
		cloaked,
		isProbeOrHelper,
	)
	body = ensureModelMaxTokens(body, baseModel)

	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeTemperatureForThinking(body)

	// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
	cpaOwnsCacheControl := cloaked || countCacheControls(body) == 0
	if cpaOwnsCacheControl {
		body = ensureCacheControl(body)
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	body = enforceCacheControlLimit(body, 4)

	// Pair body cache TTL with the extended-cache-ttl beta (upstream d7052c96af78).
	isSubagent := helps.IsClaudeSubagentRequest(incomingHeaders, body)
	subagent1h := isSubagent && helps.ClaudeSubagentRequests1h(incomingHeaders, body)
	if cpaOwnsCacheControl && oauthToken && (!isSubagent || subagent1h) && !isProbeOrHelper {
		body = upgradeClaudeCacheControlTTL(body, claudeCacheControlTTL1h)
	} else if isProbeOrHelper || (isSubagent && !subagent1h) {
		body = stripClaudeCacheControlTTL(body)
	}

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	body = normalizeCacheControlTTL(body)

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	var oauthToolNamesReverseMap map[string]string
	if oauthToken {
		bodyForUpstream, oauthToolNamesReverseMap = prepareClaudeOAuthToolNamesForUpstream(ctx, bodyForUpstream, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	// Enable cch signing by default for OAuth tokens (not just experimental flag).
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth, baseURL) {
		if claudeBodyNeedsBillingFallback(bodyForUpstream) {
			billing := gjson.GetBytes(bodyForUpstream, "system.0.text")
			if billing.Type != gjson.String || !strings.HasPrefix(billing.String(), "x-anthropic-billing-header:") {
				fallback := claudeCCHFallbackBillingHeader(ctx, e.cfg, bodyForUpstream, parseEntrypointFromUA(getClientUserAgent(ctx)))
				if fallback != "" {
					if updated, errPrepend := prependClaudeBillingSystemBlock(bodyForUpstream, fallback); errPrepend == nil {
						bodyForUpstream = updated
					}
				}
			}
		}
		bodyForUpstream = signAnthropicMessagesBody(bodyForUpstream)
	}

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return nil, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, true, extraBetas, bodyForUpstream, e.cfg, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      bodyForUpstream,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(e.cfg, auth, 0)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return nil, classifyClaudeUpstreamErrorWithCooling(httpResp.StatusCode, httpResp.Header, []byte(msg), e.modelLevelCooling())
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		err = classifyClaudeUpstreamErrorWithCooling(httpResp.StatusCode, httpResp.Header, b, e.modelLevelCooling())
		return nil, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()

		// If from == to (Claude → Claude), directly forward the SSE stream without translation
		if from == to {
			scanner := bufio.NewScanner(decodedBody)
			scanner.Buffer(nil, 52_428_800) // 50MB
			var upstreamMessageID string
			var upstreamStreamErr error
			var eventBuf bytes.Buffer
			upstreamCompleted := false
			for scanner.Scan() {
				line := scanner.Bytes()
				eventBuf.Write(line)
				eventBuf.WriteByte('\n')
				if len(bytes.TrimSpace(line)) == 0 {
					observeClaudeSSEEvent(eventBuf.Bytes(), &upstreamMessageID, &upstreamCompleted, &upstreamStreamErr)
					eventBuf.Reset()
				}
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				reporter.ObserveResponseModel(line)
				if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
				}
				restoredLine, errRestore := restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
				if errRestore != nil {
					errRestore = fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore)
					helps.RecordAPIResponseError(ctx, e.cfg, errRestore)
					reporter.PublishFailure(ctx, errRestore)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errRestore}:
					case <-ctx.Done():
					}
					return
				}
				line = restoredLine
				// Forward the line as-is to preserve SSE format
				cloned := make([]byte, len(line)+1)
				copy(cloned, line)
				cloned[len(line)] = '\n'
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: cloned}:
				case <-ctx.Done():
					return
				}
				// Stop reading once the terminal event (message_stop) has been
				// fully forwarded — a client disconnect after upstream completion
				// must not surface as a stream failure (upstream 7c32971b91c8).
				if (upstreamCompleted || upstreamStreamErr != nil) && len(bytes.TrimSpace(line)) == 0 {
					break
				}
			}
			if eventBuf.Len() > 0 {
				observeClaudeSSEEvent(eventBuf.Bytes(), &upstreamMessageID, &upstreamCompleted, &upstreamStreamErr)
			}
			if !upstreamCompleted {
				errScan := scanner.Err()
				if errScan == nil {
					errScan = upstreamStreamErr
				}
				if errScan == nil && ctx.Err() == nil {
					errScan = statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before message_stop"}
				}
				if errScan != nil {
					helps.RecordAPIResponseError(ctx, e.cfg, errScan)
					reporter.PublishFailure(ctx, errScan)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
					case <-ctx.Done():
					}
					return
				}
			}
			// Only a stream that reached message_stop advances continuity, so a
			// truncated response cannot corrupt upstream request tracking
			// (upstream 086ad91bd970).
			if upstreamCompleted {
				commitClaudeContinuity(diagnosticsState, upstreamMessageID, helps.HeaderValueCaseInsensitive(httpResp.Header, "request-id"))
			}
			return
		}

		// For other formats, use translation
		scanner := bufio.NewScanner(decodedBody)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		var upstreamMessageID string
		var upstreamStreamErr error
		var eventBuf bytes.Buffer
		upstreamCompleted := false
		for scanner.Scan() {
			line := scanner.Bytes()
			eventBuf.Write(line)
			eventBuf.WriteByte('\n')
			if len(bytes.TrimSpace(line)) == 0 {
				observeClaudeSSEEvent(eventBuf.Bytes(), &upstreamMessageID, &upstreamCompleted, &upstreamStreamErr)
				eventBuf.Reset()
			}
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			reporter.ObserveResponseModel(line)
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			restoredLine, errRestore := restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
			if errRestore != nil {
				errRestore = fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore)
				helps.RecordAPIResponseError(ctx, e.cfg, errRestore)
				reporter.PublishFailure(ctx, errRestore)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errRestore}:
				case <-ctx.Done():
				}
				return
			}
			line = restoredLine
			chunks := sdktranslator.TranslateStream(
				ctx,
				to,
				from,
				req.Model,
				opts.OriginalRequest,
				bodyForTranslation,
				bytes.Clone(line),
				&param,
			)
			if from == sdktranslator.FormatOpenAIResponse {
				for i, chunk := range chunks {
					chunks[i] = helps.EnsureResponsesUsageDetails(chunk)
				}
			}
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
			// Stop reading once upstream completion is reached — a client
			// disconnect after completion must not surface as a stream failure
			// (upstream 7c32971b91c8).
			if upstreamCompleted || upstreamStreamErr != nil {
				break
			}
		}
		if eventBuf.Len() > 0 {
			observeClaudeSSEEvent(eventBuf.Bytes(), &upstreamMessageID, &upstreamCompleted, &upstreamStreamErr)
		}
		if !upstreamCompleted {
			errScan := scanner.Err()
			if errScan == nil {
				errScan = upstreamStreamErr
			}
			if errScan == nil && ctx.Err() == nil {
				errScan = statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before message_stop"}
			}
			if errScan != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				reporter.PublishFailure(ctx, errScan)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
				case <-ctx.Done():
				}
				return
			}
		}
		if upstreamCompleted {
			commitClaudeContinuity(diagnosticsState, upstreamMessageID, helps.HeaderValueCaseInsensitive(httpResp.Header, "request-id"))
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func validateClaudeStreamingResponse(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 52_428_800)

	hasData := false
	hasMessageStart := false
	hasMessageDelta := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		hasData = true
		if !gjson.ValidBytes(payload) {
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned malformed stream data"}
		}

		root := gjson.ParseBytes(payload)
		switch root.Get("type").String() {
		case "error":
			message := strings.TrimSpace(root.Get("error.message").String())
			if message == "" {
				message = strings.TrimSpace(root.Get("error.type").String())
			}
			if message == "" {
				message = "unknown upstream error"
			}
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned error event: " + message}
		case "message_start":
			message := root.Get("message")
			if strings.TrimSpace(message.Get("id").String()) == "" || strings.TrimSpace(message.Get("model").String()) == "" {
				return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream message_start is missing id or model"}
			}
			hasMessageStart = true
		case "message_delta":
			hasMessageDelta = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	if !hasData {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned empty stream response"}
	}
	if !hasMessageStart {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response is missing message_start"}
	}
	if !hasMessageDelta {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response ended before message completion"}
	}
	return nil
}

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	if !strings.HasPrefix(baseModel, "claude-3-5-haiku") {
		body = checkSystemInstructions(body)
	}

	// Keep count_tokens requests compatible with Anthropic cache-control constraints too.
	body = enforceCacheControlLimit(body, 4)
	body = normalizeCacheControlTTL(body)

	body = applyClaudePreservedThinkingBinding(body)

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	if isClaudeOAuthToken(apiKey) {
		body, _ = prepareClaudeOAuthToolNamesForUpstream(ctx, body, claudeToolPrefix, auth.ToolPrefixDisabled())
	}

	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, body, e.cfg, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(e.cfg, auth, 0)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return cliproxyexecutor.Response{}, classifyClaudeUpstreamErrorWithCooling(resp.StatusCode, resp.Header, []byte(msg), e.modelLevelCooling())
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, classifyClaudeUpstreamErrorWithCooling(resp.StatusCode, resp.Header, b, e.modelLevelCooling())
	}
	decodedBody, err := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	count := gjson.GetBytes(data, "input_tokens").Int()
	out := sdktranslator.TranslateTokenCount(ctx, to, from, count, data)
	return cliproxyexecutor.Response{Payload: out, Headers: resp.Header.Clone()}, nil
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

// extractAndRemoveBetas extracts the "betas" array from the body and removes it.
// Returns the extracted betas as a string slice and the modified body.
func extractAndRemoveBetas(body []byte) ([]string, []byte) {
	betasResult := gjson.GetBytes(body, "betas")
	if !betasResult.Exists() {
		return nil, body
	}
	var betas []string
	if betasResult.IsArray() {
		for _, item := range betasResult.Array() {
			if s := strings.TrimSpace(item.String()); s != "" {
				betas = append(betas, s)
			}
		}
	} else if s := strings.TrimSpace(betasResult.String()); s != "" {
		betas = append(betas, s)
	}
	body, _ = sjson.DeleteBytes(body, "betas")
	return betas, body
}

// disableThinkingIfToolChoiceForced checks if tool_choice forces tool use and disables thinking.
// Anthropic API does not allow thinking when tool_choice is set to "any" or a specific tool.
// See: https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking#important-considerations
func disableThinkingIfToolChoiceForced(body []byte) []byte {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		// Remove thinking configuration entirely to avoid API error
		body, _ = sjson.DeleteBytes(body, "thinking")
		// Adaptive thinking may also set output_config.effort; remove it to avoid
		// leaking thinking controls when tool_choice forces tool use.
		body, _ = sjson.DeleteBytes(body, "output_config.effort")
		if oc := gjson.GetBytes(body, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, "output_config")
		}
	}
	return body
}

// normalizeClaudeTemperatureForThinking keeps Anthropic message requests valid when
// thinking is enabled. Anthropic rejects temperatures other than 1 when
// thinking.type is enabled/adaptive/auto.
func normalizeClaudeTemperatureForThinking(body []byte) []byte {
	if !gjson.GetBytes(body, "temperature").Exists() {
		return body
	}

	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "enabled", "adaptive", "auto":
		if temp := gjson.GetBytes(body, "temperature"); temp.Exists() && temp.Type == gjson.Number && temp.Float() == 1 {
			return body
		}
		body, _ = sjson.SetBytes(body, "temperature", 1)
	}
	return body
}

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for i := range c.closers {
		if c.closers[i] == nil {
			continue
		}
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// peekableBody wraps a bufio.Reader around the original ReadCloser so that
// magic bytes can be inspected without consuming them from the stream.
type peekableBody struct {
	*bufio.Reader
	closer io.Closer
}

func (p *peekableBody) Close() error {
	return p.closer.Close()
}

func decodeResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	if contentEncoding == "" {
		// No Content-Encoding header.  Attempt best-effort magic-byte detection to
		// handle misbehaving upstreams that compress without setting the header.
		// Only gzip (1f 8b) and zstd (28 b5 2f fd) have reliable magic sequences;
		// br and deflate have none and are left as-is.
		// The bufio wrapper preserves unread bytes so callers always see the full
		// stream regardless of whether decompression was applied.
		pb := &peekableBody{Reader: bufio.NewReader(body), closer: body}
		magic, peekErr := pb.Peek(4)
		if peekErr == nil || (peekErr == io.EOF && len(magic) >= 2) {
			switch {
			case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
				gzipReader, gzErr := gzip.NewReader(pb)
				if gzErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte gzip: failed to create reader: %w", gzErr)
				}
				return &compositeReadCloser{
					Reader: gzipReader,
					closers: []func() error{
						gzipReader.Close,
						pb.Close,
					},
				}, nil
			case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
				decoder, zdErr := zstd.NewReader(pb)
				if zdErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte zstd: failed to create reader: %w", zdErr)
				}
				return &compositeReadCloser{
					Reader: decoder,
					closers: []func() error{
						func() error { decoder.Close(); return nil },
						pb.Close,
					},
				}, nil
			}
		}
		return pb, nil
	}
	// Encodings are listed in application order, so decoding chains in reverse:
	// each listed encoding wraps the previous one's bytes. The old forward loop
	// decoded only the first encoding and silently dropped the rest.
	encodings := strings.Split(contentEncoding, ",")
	reader := io.Reader(body)
	decoderClosers := make([]func() error, 0, len(encodings))
	cleanup := func() {
		for i := len(decoderClosers) - 1; i >= 0; i-- {
			_ = decoderClosers[i]()
		}
		_ = body.Close()
	}
	for index := len(encodings) - 1; index >= 0; index-- {
		encoding := strings.TrimSpace(strings.ToLower(encodings[index]))
		switch encoding {
		case "", "identity":
			continue
		case "gzip":
			gzipReader, errGzip := gzip.NewReader(reader)
			if errGzip != nil {
				cleanup()
				return nil, fmt.Errorf("failed to create gzip reader: %w", errGzip)
			}
			reader = gzipReader
			decoderClosers = append(decoderClosers, gzipReader.Close)
		case "deflate":
			deflateReader, errDeflate := newClaudeDeflateReader(reader)
			if errDeflate != nil {
				cleanup()
				return nil, errDeflate
			}
			reader = deflateReader
			decoderClosers = append(decoderClosers, deflateReader.Close)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoder, errZstd := zstd.NewReader(reader)
			if errZstd != nil {
				cleanup()
				return nil, fmt.Errorf("failed to create zstd reader: %w", errZstd)
			}
			reader = decoder
			decoderClosers = append(decoderClosers, func() error { decoder.Close(); return nil })
		default:
			continue
		}
	}
	closers := make([]func() error, 0, len(decoderClosers)+1)
	for index := len(decoderClosers) - 1; index >= 0; index-- {
		closers = append(closers, decoderClosers[index])
	}
	closers = append(closers, body.Close)
	return &compositeReadCloser{Reader: reader, closers: closers}, nil
}

// newClaudeDeflateReader builds a deflate reader that handles both raw DEFLATE
// and zlib-wrapped streams; real upstreams send either and raw flate.NewReader
// misparses zlib framing.
func newClaudeDeflateReader(reader io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(reader)
	header, errPeek := buffered.Peek(2)
	if errPeek == nil && isZlibHeader(header) {
		zlibReader, errZlib := zlib.NewReader(buffered)
		if errZlib != nil {
			return nil, fmt.Errorf("failed to create zlib deflate reader: %w", errZlib)
		}
		return zlibReader, nil
	}
	return flate.NewReader(buffered), nil
}

func isZlibHeader(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf, flg := header[0], header[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}

// Anthropic-Beta composition follows Claude Code 2.1.280's per-request assembly
// rather than a fixed string. The captured wire order (verified against
// api.anthropic.com on both the API-key and OAuth paths, and against the
// 2.1.280 binary 80abbfe measured 2026-09-23) is:
//
//	 1 claude-code-20250219
//	 2 oauth-2025-04-20                  OAuth credentials only
//	 3 context-1m-2025-08-07             [1m] model variants only
//	 4 interleaved-thinking-2025-05-14
//	 5 redact-thinking-2026-02-12        cli entrypoint only
//	 6 thinking-token-count-2026-05-13
//	 7 context-management-2025-06-27
//	 8 prompt-caching-scope-2026-01-05
//	 9 mid-conversation-system-2026-04-07  models accepting a role=system turn
//	10 per-turn-control-2026-07-01        opus-5-5 and fable-5-1, or requested
//	11 timing-2026-09-09                  per-turn timing body, or requested
//	12 mid-conversation-tool-changes-2026-07-01  same models as mid-conversation-system
//	13 inline-tools-2026-09-15            inline tool_addition blocks, or requested
//	14 advisor-tool-2026-03-01            requests declaring advisor tools or requesting advisor beta
//	15 advanced-tool-use-2025-11-20       requests using tool search or another advanced tool-use feature
//	16 mid-conversation-system-clear-at-2026-08-21  messages with clear_at, or requested
//	17 dangerous-tool-use-2026-09-03      safeguards body, or requested
//	18 effort-2025-11-24                  effort-supporting models with active thinking
//	19 server-side-fallback-2026-06-01    requests with fallbacks or requested
//	20 fallback-credit-2026-06-01         OAuth credentials
//	21 structured-outputs-2025-12-15      structured output requests
//	22 thinking-binding-controls-2026-08-01  thinking.block_binding, or requested
//	23 thinking-display-updates-2026-08-18 requests with thinking.display=updates
//	24 thinking-resumption-2026-07-17     requested only; the 2.1.280 flag defaults off
//	25 fast-mode-2026-02-01               speed:fast requests only
//	26 afk-mode-2026-01-31                auto-mode sessions, forwarded when the caller sends it
//	27 extended-cache-ttl-2025-04-11      OAuth credentials (omitted on subagent & probe)
//	28 prompt-caching-evict-2026-05-12    evict_on_complete, or requested
//	29 cache-diagnosis-2026-04-07         requests with diagnostics only
const (
	claudeTokenCountingBeta          = "token-counting-2024-11-01"
	claudeFastModeBeta               = "fast-mode-2026-02-01"
	claudeOAuthBeta                  = "oauth-2025-04-20"
	claudeCodeBeta                   = "claude-code-20250219"
	claudeContext1MBeta              = "context-1m-2025-08-07"
	claudeMidConvSystemBeta          = "mid-conversation-system-2026-04-07"
	claudePerTurnControlBeta         = "per-turn-control-2026-07-01"
	claudePerTurnTimingBeta          = "timing-2026-09-09"
	claudeMidConvToolChangesBeta     = "mid-conversation-tool-changes-2026-07-01"
	claudeInlineToolsBeta            = "inline-tools-2026-09-15"
	claudeMidConvSystemClearAtBeta   = "mid-conversation-system-clear-at-2026-08-21"
	claudeDangerousToolUseBeta       = "dangerous-tool-use-2026-09-03"
	claudeAdvisorToolBeta            = "advisor-tool-2026-03-01"
	claudeAdvancedToolUseBeta        = "advanced-tool-use-2025-11-20"
	claudeEffortBeta                 = "effort-2025-11-24"
	claudeServerSideFallbackBeta     = "server-side-fallback-2026-06-01"
	claudeFallbackCreditBeta         = "fallback-credit-2026-06-01"
	claudeStructuredOutputsBeta      = "structured-outputs-2025-12-15"
	claudeThinkingDisplayUpdatesBeta = "thinking-display-updates-2026-08-18"
	claudeThinkingBindingBeta        = "thinking-binding-controls-2026-08-01"
	claudeThinkingResumptionBeta     = "thinking-resumption-2026-07-17"
	claudeExtendedCacheTTLBeta       = "extended-cache-ttl-2025-04-11"
	claudePromptCachingEvictBeta     = "prompt-caching-evict-2026-05-12"
	claudeCacheDiagnosisBeta         = "cache-diagnosis-2026-04-07"
	claudeRedactThinkingBeta         = "redact-thinking-2026-02-12"
	claudeAFKModeBeta                = "afk-mode-2026-01-31"
)

// claudeCodeCLIConstantBetas are the betas Claude Code 2.1.280 sends on every
// /v1/messages request from the "cli" entrypoint, in wire order, excluding the
// leading claude-code-20250219.
//
// redact-thinking-2026-02-12 belongs here because cloaked requests always claim
// cc_entrypoint=cli; the "sdk-cli" entrypoint omits it.
var claudeCodeCLIConstantBetas = []string{
	"interleaved-thinking-2025-05-14",
	claudeRedactThinkingBeta,
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
}

// claudeCodeTrailingBetas are caller-supplied betas that real Claude Code emits
// after effort-2025-11-24, in that relative order. They are forwarded when the
// caller asks for them and dropped otherwise.
var claudeCodeTrailingBetas = []string{
	claudeServerSideFallbackBeta,
	claudeFallbackCreditBeta,
	claudeStructuredOutputsBeta,
}

// claudeManagedBetaSet holds every beta the proxy itself assembles or gates.
// Caller betas outside this set are unknown to the pinned Claude Code profile —
// newer client releases ship betas past it — and are forwarded verbatim so
// their features keep working (upstream #5738, bd584a752329).
var claudeManagedBetaSet = func() map[string]bool {
	managed := []string{
		claudeTokenCountingBeta,
		claudeFastModeBeta,
		claudeOAuthBeta,
		claudeCodeBeta,
		claudeContext1MBeta,
		claudeMidConvSystemBeta,
		claudePerTurnControlBeta,
		claudePerTurnTimingBeta,
		claudeMidConvToolChangesBeta,
		claudeInlineToolsBeta,
		claudeMidConvSystemClearAtBeta,
		claudeDangerousToolUseBeta,
		claudeAdvisorToolBeta,
		claudeAdvancedToolUseBeta,
		claudeEffortBeta,
		claudeServerSideFallbackBeta,
		claudeFallbackCreditBeta,
		claudeStructuredOutputsBeta,
		claudeThinkingDisplayUpdatesBeta,
		claudeThinkingBindingBeta,
		claudeThinkingResumptionBeta,
		claudeExtendedCacheTTLBeta,
		claudePromptCachingEvictBeta,
		claudeCacheDiagnosisBeta,
		claudeRedactThinkingBeta,
		claudeAFKModeBeta,
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
	}
	set := make(map[string]bool, len(managed))
	for _, beta := range managed {
		set[beta] = true
	}
	return set
}()

func isManagedClaudeBeta(beta string) bool {
	return claudeManagedBetaSet[strings.TrimSpace(beta)]
}

// claudeCodeCLIBetas assembles the Anthropic-Beta baseline the way Claude Code
// 2.1.280 does: the list is per-request, not a fixed string. requested holds the
// betas the caller asked for, which decide the capability flags below.
func claudeCodeCLIBetas(body []byte, requested map[string]bool, oauthToken bool) string {
	betas := make([]string, 0, len(claudeCodeCLIConstantBetas)+len(claudeCodeTrailingBetas)+14)
	betas = append(betas, claudeCodeBeta)
	if oauthToken {
		betas = append(betas, claudeOAuthBeta)
	}
	if requested[claudeContext1MBeta] {
		betas = append(betas, claudeContext1MBeta)
	}
	redactThinking := !claudeThinkingDisplaySet(body)
	for _, beta := range claudeCodeCLIConstantBetas {
		if beta == claudeRedactThinkingBeta && !redactThinking {
			continue
		}
		betas = append(betas, beta)
	}
	if !claudeUsesLegacySystemReminder(body) {
		betas = append(betas, claudeMidConvSystemBeta)
		if claudeIncludePerTurnControl(body, requested) {
			betas = append(betas, claudePerTurnControlBeta)
		}
		if claudeIncludePerTurnTiming(body, requested) {
			betas = append(betas, claudePerTurnTimingBeta)
		}
		if !isClaudeSonnet5Model(gjson.GetBytes(body, "model").String()) {
			betas = append(betas, claudeMidConvToolChangesBeta)
		}
		if claudeIncludeInlineTools(body, requested) {
			betas = append(betas, claudeInlineToolsBeta)
		}
	} else {
		// Legacy models have no mid-conversation slot. A caller that still names
		// these betas keeps them, in the same relative order, ahead of effort.
		if claudeIncludePerTurnControl(body, requested) {
			betas = append(betas, claudePerTurnControlBeta)
		}
		if claudeIncludePerTurnTiming(body, requested) {
			betas = append(betas, claudePerTurnTimingBeta)
		}
	}
	if requested[claudeAdvisorToolBeta] || claudeBodyHasAdvisorTool(body) {
		betas = append(betas, claudeAdvisorToolBeta)
	}
	if requested[claudeAdvancedToolUseBeta] || claudeBodyUsesAdvancedToolUse(body) {
		betas = append(betas, claudeAdvancedToolUseBeta)
	}
	if !claudeUsesLegacySystemReminder(body) && claudeIncludeMidConvClearAt(body, requested) {
		betas = append(betas, claudeMidConvSystemClearAtBeta)
	}
	if requested[claudeDangerousToolUseBeta] || gjson.GetBytes(body, "safeguards").Exists() {
		betas = append(betas, claudeDangerousToolUseBeta)
	}
	if claudeRequestSupportsEffort(body, requested) {
		betas = append(betas, claudeEffortBeta)
	}
	isProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(body)
	if !isProbeOrHelper && (requested[claudeServerSideFallbackBeta] || gjson.GetBytes(body, "fallbacks").Exists()) {
		betas = append(betas, claudeServerSideFallbackBeta)
	}
	if requested[claudeFallbackCreditBeta] || oauthToken {
		betas = append(betas, claudeFallbackCreditBeta)
	}
	for _, beta := range claudeCodeTrailingBetas {
		if beta == claudeServerSideFallbackBeta || beta == claudeFallbackCreditBeta {
			continue
		}
		if requested[beta] {
			betas = append(betas, beta)
		}
	}
	thinkingType := gjson.GetBytes(body, "thinking.type").String()
	if requested[claudeThinkingBindingBeta] || gjson.GetBytes(body, "thinking.block_binding").Exists() ||
		(claudeModelUsesProgressDisplay(gjson.GetBytes(body, "model").String()) && thinkingType == "adaptive") {
		betas = append(betas, claudeThinkingBindingBeta)
	}
	if !isProbeOrHelper && thinkingType != "disabled" && (requested[claudeThinkingDisplayUpdatesBeta] || claudeThinkingDisplayUpdates(body)) {
		betas = append(betas, claudeThinkingDisplayUpdatesBeta)
	}
	if requested[claudeThinkingResumptionBeta] {
		betas = append(betas, claudeThinkingResumptionBeta)
	}
	if claudeRequestUsesFastMode(body, requested) {
		betas = append(betas, claudeFastModeBeta)
	}
	if requested[claudeAFKModeBeta] {
		betas = append(betas, claudeAFKModeBeta)
	}
	if !isProbeOrHelper {
		includeExtended := (oauthToken && !helps.IsClaudeSubagentRequest(nil, body)) ||
			requested[claudeExtendedCacheTTLBeta] ||
			helps.ClaudePayloadHas1hTTL(body)
		if includeExtended {
			betas = append(betas, claudeExtendedCacheTTLBeta)
		}
	}
	if requested[claudePromptCachingEvictBeta] || bytes.Contains(body, []byte(`"evict_on_complete"`)) {
		betas = append(betas, claudePromptCachingEvictBeta)
	}
	if diagnostics := gjson.GetBytes(body, "diagnostics"); diagnostics.IsObject() {
		betas = append(betas, claudeCacheDiagnosisBeta)
	}
	return strings.Join(betas, ",")
}

// claudeBodyUsesAdvancedToolUse reports whether the request uses an advanced
// tool-use feature (tool search, deferred loading, input examples, or
// allowed callers) that requires the advanced-tool-use beta
// (upstream d7052c96af78).
func claudeBodyUsesAdvancedToolUse(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
		if strings.HasPrefix(toolType, "tool_search_tool_") {
			return true
		}
		if tool.Get("defer_loading").Bool() || tool.Get("input_examples").Exists() || tool.Get("allowed_callers").Exists() {
			return true
		}
	}
	return false
}

// claudeBodyHasAdvisorTool reports whether the request declares an advisor
// server tool, which requires the advisor-tool beta (upstream d7052c96af78).
func claudeBodyHasAdvisorTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
		if strings.HasPrefix(toolType, "advisor_") {
			return true
		}
	}
	return false
}

// claudeThinkingDisplaySet reports whether thinking.display is explicitly set;
// when it is, native Claude Code 2.1.280 omits redact-thinking-2026-02-12.
func claudeThinkingDisplaySet(body []byte) bool {
	display := gjson.GetBytes(body, "thinking.display")
	return display.Type == gjson.String && strings.TrimSpace(display.String()) != ""
}

// isClaudeHaikuModel reports whether the request model is a Haiku variant;
// native Claude Code does not emit effort-2025-11-24 for it
// (upstream d7052c96af78).
func isClaudeHaikuModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "haiku")
}

func claudeCanonicalModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	return model
}

func isClaudeOpus55Model(model string) bool {
	model = claudeCanonicalModel(model)
	return model == "claude-opus-5-5" || strings.HasPrefix(model, "claude-opus-5-5[")
}

func isClaudeSonnet55Model(model string) bool {
	model = claudeCanonicalModel(model)
	return model == "claude-sonnet-5-5" || strings.HasPrefix(model, "claude-sonnet-5-5-") || strings.HasPrefix(model, "claude-sonnet-5-5[")
}

func isClaudeSonnet5Model(model string) bool {
	model = claudeCanonicalModel(model)
	if isClaudeSonnet55Model(model) {
		return false
	}
	return model == "claude-sonnet-5" || strings.HasPrefix(model, "claude-sonnet-5-") || strings.HasPrefix(model, "claude-sonnet-5[")
}

// claudeModelUsesProgressDisplay reports the 2.1.280 interactive CLI models that
// send thinking.display=updates unless the caller already chose a display mode.
func claudeModelUsesProgressDisplay(model string) bool {
	return isClaudeOpus55Model(model) || isClaudeFable51Model(model) || isClaudeSonnet5Model(model) || isClaudeSonnet55Model(model)
}

// applyClaudeCloakThinkingDisplay fills the latest CLI display only when the
// translated caller did not choose one. An explicit summarized or omitted value
// from Responses, Chat, or Gemini stays untouched.
func applyClaudeCloakThinkingDisplay(body []byte, callerOwned bool) []byte {
	if callerOwned || len(body) == 0 || gjson.GetBytes(body, "thinking.display").Exists() {
		return body
	}
	if !claudeModelUsesProgressDisplay(gjson.GetBytes(body, "model").String()) {
		return body
	}
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "adaptive", "enabled":
	default:
		return body
	}
	body, _ = sjson.SetBytes(body, "thinking.display", "updates")
	return body
}

// claudeModelHasPerTurnEffort reports models whose 2.1.280 catalog capability
// per_turn_effort puts per-turn-control-2026-07-01 on every first-party request
// (upstream bd584a752329).
func claudeModelHasPerTurnEffort(model string) bool {
	return isClaudeOpus55Model(model) || strings.HasPrefix(claudeCanonicalModel(model), "claude-fable-5-1")
}

// claudeModelHasPerTurnTiming reports models whose catalog lists per_turn_timing.
// Claude Code still withholds timing-2026-09-09 unless CLAUDE_CODE_PER_TURN_TIMING
// is set, so the beta follows the body or an explicit caller request.
func claudeModelHasPerTurnTiming(model string) bool {
	model = claudeCanonicalModel(model)
	return claudeModelHasPerTurnEffort(model) || strings.HasPrefix(model, "claude-mythos-5-1")
}

func claudeIncludePerTurnControl(body []byte, requested map[string]bool) bool {
	if requested[claudePerTurnControlBeta] {
		return true
	}
	return claudeModelHasPerTurnEffort(gjson.GetBytes(body, "model").String())
}

func claudeIncludePerTurnTiming(body []byte, requested map[string]bool) bool {
	if requested[claudePerTurnTimingBeta] {
		return true
	}
	if !claudeModelHasPerTurnTiming(gjson.GetBytes(body, "model").String()) {
		return false
	}
	if gjson.GetBytes(body, "output_config.timing").Exists() {
		return true
	}
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("output_config.timing").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
}

func claudeIncludeInlineTools(body []byte, requested map[string]bool) bool {
	if requested[claudeInlineToolsBeta] {
		return true
	}
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			if strings.EqualFold(strings.TrimSpace(block.Get("type").String()), "tool_addition") && block.Get("tool.definition").Exists() {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

func claudeIncludeMidConvClearAt(body []byte, requested map[string]bool) bool {
	if requested[claudeMidConvSystemClearAtBeta] || claudeModelUsesProgressDisplay(gjson.GetBytes(body, "model").String()) {
		return true
	}
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("clear_at").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
}

// claudeRequestSupportsEffort reports whether the request may carry the
// effort-2025-11-24 beta: native Claude Code omits it on Haiku models, on
// probe/helper turns, and when thinking is disabled (upstream d7052c96af78).
func claudeRequestSupportsEffort(body []byte, requested map[string]bool) bool {
	if len(body) > 0 {
		if helps.IsClaudeProbeOrHelperRequest(body) {
			return false
		}
		model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
		if isClaudeHaikuModel(model) {
			return false
		}
		thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
		if thinkingType == "disabled" {
			return false
		}
	}
	if requested[claudeEffortBeta] {
		return true
	}
	return true
}

// claudeThinkingDisplayUpdates reports whether the body asks for adaptive
// thinking display updates (thinking.display = "updates").
func claudeThinkingDisplayUpdates(body []byte) bool {
	display := gjson.GetBytes(body, "thinking.display")
	return display.Type == gjson.String && strings.EqualFold(strings.TrimSpace(display.String()), "updates")
}

// withoutClaudeBeta removes every occurrence of removeBeta from a
// comma-separated beta list (upstream d7052c96af78).
func withoutClaudeBeta(betas, removeBeta string) string {
	parts := strings.Split(betas, ",")
	res := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && p != removeBeta {
			res = append(res, p)
		}
	}
	return strings.Join(res, ",")
}

// withClaudeBetaAppended appends beta at the end unless it is already present.
func withClaudeBetaAppended(betas, beta string) string {
	beta = strings.TrimSpace(beta)
	if beta == "" {
		return betas
	}
	for _, existing := range strings.Split(betas, ",") {
		if strings.TrimSpace(existing) == beta {
			return betas
		}
	}
	if strings.TrimSpace(betas) == "" {
		return beta
	}
	return betas + "," + beta
}

// withClaudeExtendedCacheTTLBeta appends extended-cache-ttl-2025-04-11 to the
// assembled list at its captured trailer position when absent
// (upstream d7052c96af78: a body carrying 1h cache TTL pairs with the beta).
func withClaudeExtendedCacheTTLBeta(betas string) string {
	parts := make([]string, 0, 16)
	seen := make(map[string]bool)
	for _, beta := range strings.Split(betas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" && !seen[beta] {
			parts = append(parts, beta)
			seen[beta] = true
		}
	}
	if !seen[claudeExtendedCacheTTLBeta] {
		parts = append(parts, claudeExtendedCacheTTLBeta)
	}
	return strings.Join(parts, ",")
}

// claudeRequestUsesFastMode reports whether the request selects the fast service
// tier. Anthropic rejects the body's speed field with "Extra inputs are not
// permitted" unless fast-mode-2026-02-01 is declared, so the beta has to follow
// the body.
func claudeRequestUsesFastMode(body []byte, requested map[string]bool) bool {
	if requested[claudeFastModeBeta] {
		return true
	}
	speed := gjson.GetBytes(body, "speed")
	return speed.Type == gjson.String && strings.EqualFold(strings.TrimSpace(speed.String()), "fast")
}

// claudeCountTokensBetas is the fixed profile Claude Code 2.1.280 sends to
// /v1/messages/count_tokens. It is far smaller than the inference baseline:
// redact-thinking, thinking-token-count, prompt-caching-scope, effort and every
// conditional beta are absent.
var claudeCountTokensBetas = []string{
	claudeCodeBeta,
	"interleaved-thinking-2025-05-14",
	"context-management-2025-06-27",
	claudeTokenCountingBeta,
}

// claudeEntitlementError marks an upstream refusal that is a property of the
// request shape combined with the account's entitlements, not of the credential's
// health. The auth manager must neither rotate nor cool down on these.
// (ported from upstream claude_executor_request.go)
type claudeEntitlementError struct {
	statusErr
}

func (claudeEntitlementError) IsRequestScoped() bool {
	return true
}

func (claudeEntitlementError) IsCredentialScoped() bool {
	return false
}

// claudeRateLimitError carries the credential scope of an upstream 429:
// credential-scoped rejections cool down every sibling model while
// model-scoped ones only cool down the requested model state
// (ported from upstream claude_executor_request.go).
type claudeRateLimitError struct {
	statusErr
	credentialScoped bool
}

func (e claudeRateLimitError) IsCredentialScoped() bool {
	return e.credentialScoped
}

func (e claudeRateLimitError) IsRequestScoped() bool {
	return false
}

// classifyClaudeUpstreamError promotes upstream refusals that no other credential
// can satisfy into request-scoped errors.
//
// Anthropic answers a fast-mode request from an account without the matching
// usage credits with 429 rate_limit_error "Usage credits are required for fast
// mode". The generic pipeline reads 429 as quota exhaustion: it marks the
// credential Quota.Exceeded, applies an exponential cooldown and rotates to the
// next one, which returns the same 429. A single speed:"fast" request would walk
// the whole Claude pool and cool down every credential, all of which remain
// perfectly healthy for ordinary traffic. The refusal belongs to the request.
// (ported from upstream claude_executor_request.go)
func classifyClaudeUpstreamError(statusCode int, headers http.Header, body []byte) error {
	return classifyClaudeUpstreamErrorWithCooling(statusCode, headers, body, false)
}

// classifyClaudeUpstreamErrorWithCooling is classifyClaudeUpstreamError with an
// explicit model-level cooling switch: when modelLevelCooling is true, even an
// explicit shared-window (5h/7d) rate-limit rejection stays model-scoped so
// sibling models on the same credential remain usable
// (upstream 44eaef0009f8).
func classifyClaudeUpstreamErrorWithCooling(statusCode int, headers http.Header, body []byte, modelLevelCooling bool) error {
	var retryAfter *time.Duration
	if statusCode == http.StatusTooManyRequests || (statusCode >= 400 && statusCode < 600) {
		retryAfter = helps.ParseClaudeRateLimitResetForError(headers, body, time.Now())
	}
	err := statusErr{code: statusCode, msg: string(body), retryAfter: retryAfter}
	if statusCode == http.StatusTooManyRequests {
		if !modelLevelCooling && helps.ClaudeHeadersIndicateUnifiedRateLimitRejection(headers) {
			return claudeRateLimitError{statusErr: err, credentialScoped: true}
		}
		if claudeBodyIndicatesFastModeCredits(body) {
			return claudeEntitlementError{err}
		}
		// Ordinary model-level Claude 429 (not a unified 5h/7d rejection)
		return claudeRateLimitError{statusErr: err, credentialScoped: false}
	}
	return err
}

// claudeBodyIndicatesFastModeCredits matches Anthropic's fast-mode entitlement
// refusal without matching a genuine rate limit, which never mentions fast mode.
// (ported from upstream claude_executor_request.go)
func claudeBodyIndicatesFastModeCredits(body []byte) bool {
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.ToLower(string(body))
	}
	return strings.Contains(message, "fast request rejected") ||
		(strings.Contains(message, "fast") &&
			(strings.Contains(message, "usage credits") || strings.Contains(message, "credits are required")))
}

// claudeRequestedBetas collects every beta the caller asked for, from the
// Anthropic-Beta header and from betas lifted out of the request body.
func claudeRequestedBetas(incomingBetas string, extraBetas []string) map[string]bool {
	requested := make(map[string]bool)
	for _, beta := range strings.Split(incomingBetas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	for _, beta := range extraBetas {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	return requested
}

// claudeLegacySystemReminderModels lists the official Anthropic model IDs and
// aliases that reject a mid-conversation role=system message. Entries mirror the
// "claude" provider registry plus Anthropic's own bare and "-latest" aliases.
var claudeLegacySystemReminderModels = map[string]struct{}{
	"claude-3-5-haiku-20241022":  {},
	"claude-3-5-haiku-latest":    {},
	"claude-3-7-sonnet-20250219": {},
	"claude-3-7-sonnet-latest":   {},
	"claude-haiku-4-5":           {},
	"claude-haiku-4-5-20251001":  {},
	"claude-opus-4":              {},
	"claude-opus-4-20250514":     {},
	"claude-opus-4-1":            {},
	"claude-opus-4-1-20250805":   {},
	"claude-opus-4-5":            {},
	"claude-opus-4-5-20251101":   {},
	"claude-opus-4-6":            {},
	"claude-opus-4-7":            {},
	"claude-sonnet-4":            {},
	"claude-sonnet-4-20250514":   {},
	"claude-sonnet-4-5":          {},
	"claude-sonnet-4-5-20250929": {},
	"claude-sonnet-4-6":          {},
}

// claudeUsesLegacySystemReminder reports whether the request model rejects a
// mid-conversation role=system message (the pre-2026-04-07 system reminder
// shape). It gates the mid-conversation-system beta and the cloaking path.
func claudeUsesLegacySystemReminder(payload []byte) bool {
	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "model").String()))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	_, legacy := claudeLegacySystemReminderModels[model]
	return legacy
}

// isAnthropicUpstreamURL reports whether a resolved request targets Anthropic's
// first-party API. Every rule that reconstructs Claude Code's identity must key
// on this rather than on the cloaked flag: Kimi rewrites base_url to
// api.kimi.com and custom gateways set their own host, yet both delegate to
// ClaudeExecutor and are therefore cloaked.
func isAnthropicUpstreamURL(u *url.URL) bool {
	return u != nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Host, "api.anthropic.com")
}

func isAnthropicUpstreamBase(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return isAnthropicUpstreamURL(parsed)
}

// resolveIncomingClaudeHeaders merges the gin request headers (when present)
// with the headers forwarded on opts so subagent/session/beta detection sees
// everything the caller sent (upstream 086ad91bd970).
func resolveIncomingClaudeHeaders(ctx context.Context, incoming http.Header) http.Header {
	resolved := make(http.Header)
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		resolved = ginCtx.Request.Header.Clone()
	}
	for key, values := range incoming {
		resolved[key] = append([]string(nil), values...)
	}
	return resolved
}

func claudeIncomingHeaderValue(headers http.Header, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return value
			}
		}
	}
	return ""
}

func applyClaudeHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool, extraBetas []string, body []byte, cfg *config.Config, clientHeaders ...http.Header) {
	hdrDefault := func(cfgVal, fallback string) string {
		if cfgVal != "" {
			return cfgVal
		}
		return fallback
	}

	var hd config.ClaudeHeaderDefaults
	if cfg != nil {
		hd = cfg.ClaudeHeaderDefaults
	}

	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	oauthToken := isClaudeOAuthToken(apiKey) || !useAPIKey
	isAnthropicBase := isAnthropicUpstreamURL(r.URL)
	if isAnthropicBase && useAPIKey {
		r.Header.Del("Authorization")
		r.Header.Set("x-api-key", apiKey)
	} else {
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}
	r.Header.Set("Content-Type", "application/json")

	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0]
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}
	stabilizeDeviceProfile := helps.ClaudeDeviceProfileStabilizationEnabled(cfg)
	var deviceProfile helps.ClaudeDeviceProfile
	if stabilizeDeviceProfile {
		deviceProfile = helps.ResolveClaudeDeviceProfile(auth, apiKey, ginHeaders, cfg)
	}

	incomingBetas := strings.TrimSpace(strings.Join(helps.HeaderValuesCaseInsensitive(ginHeaders, "Anthropic-Beta"), ","))
	countTokens := r.URL != nil && strings.HasSuffix(r.URL.Path, "/count_tokens")
	isCallerOwned := isClaudeCallerOwned(auth, apiKey)
	var baseBetas string
	if isCallerOwned {
		baseBetas = incomingBetas
	} else {
		baseBetas = claudeCodeCLIBetas(body, claudeRequestedBetas(incomingBetas, extraBetas), oauthToken)
		if countTokens {
			baseBetas = strings.Join(claudeCountTokensBetas, ",")
		}
	}
	existingSet := make(map[string]bool)
	appendBeta := func(beta string) {
		beta = strings.TrimSpace(beta)
		if beta != "" && !existingSet[beta] {
			baseBetas += "," + beta
			existingSet[beta] = true
		}
	}
	for _, beta := range strings.Split(baseBetas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			existingSet[beta] = true
		}
	}
	// On direct Anthropic the caller's managed betas are dropped: appending them
	// to the measured baseline produces a combination real Claude Code never
	// sends — they reach the wire through claudeCodeCLIBetas at their captured
	// positions instead. Caller betas the proxy does not manage are newer-client
	// features the pinned profile predates; dropping them fails those requests
	// outright, so they are forwarded (upstream #5738, bd584a752329). Other
	// Anthropic-compatible upstreams (Kimi, custom gateways) keep all caller
	// extensions.
	if !isCallerOwned && incomingBetas != "" {
		for _, beta := range strings.Split(incomingBetas, ",") {
			beta = strings.TrimSpace(beta)
			if beta == "" {
				continue
			}
			if isAnthropicBase && isManagedClaudeBeta(beta) {
				continue
			}
			appendBeta(beta)
		}
	}
	// The OAuth betas have known positions on /v1/messages and are placed by
	// claudeCodeCLIBetas. count_tokens was only captured over an API key, so its
	// OAuth shape keeps the previous appended form until it can be measured.
	if oauthToken && countTokens {
		appendBeta(claudeOAuthBeta)
	}
	// Betas lifted out of the body follow the same policy as header-supplied
	// ones. Known betas already reached the assembled baseline through the
	// requested map, which places them at their captured positions; anything
	// left over is unknown to Claude Code and Anthropic rejects it outright.
	if !isAnthropicBase {
		for _, beta := range extraBetas {
			appendBeta(beta)
		}
	}
	// Enforce native Claude Code 2.1.280 model & turn beta gating
	// (upstream d7052c96af78): effort is pruned on Haiku, probes, and disabled
	// thinking; probe/helper turns drop fallback, display-update, and 1h-cache
	// betas; subagents drop the 1h-cache beta unless they explicitly opt in;
	// a body carrying 1h cache TTL always pairs with the beta.
	if !claudeRequestSupportsEffort(body, nil) {
		baseBetas = withoutClaudeBeta(baseBetas, claudeEffortBeta)
	}
	reqProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(body)
	if reqProbeOrHelper {
		baseBetas = withoutClaudeBeta(baseBetas, claudeServerSideFallbackBeta)
		baseBetas = withoutClaudeBeta(baseBetas, claudeThinkingDisplayUpdatesBeta)
		baseBetas = withoutClaudeBeta(baseBetas, claudeExtendedCacheTTLBeta)
	}
	if gjson.GetBytes(body, "thinking.type").String() == "disabled" {
		baseBetas = withoutClaudeBeta(baseBetas, claudeThinkingDisplayUpdatesBeta)
	}
	if helps.IsClaudeSubagentRequest(ginHeaders, body) && !helps.ClaudeSubagentRequests1h(ginHeaders, body) {
		baseBetas = withoutClaudeBeta(baseBetas, claudeExtendedCacheTTLBeta)
	}
	if !reqProbeOrHelper && !countTokens && helps.ClaudePayloadHas1hTTL(body) {
		baseBetas = withClaudeExtendedCacheTTLBeta(baseBetas)
	}
	reqModel := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	if isClaudeHaikuModel(reqModel) && !gjson.GetBytes(body, "fallbacks").Exists() {
		baseBetas = withoutClaudeBeta(baseBetas, claudeServerSideFallbackBeta)
	}
	if gjson.GetBytes(body, "thinking.block_binding").Exists() {
		baseBetas = withClaudeBetaAppended(baseBetas, claudeThinkingBindingBeta)
	}

	applyBetaHeader := func() {
		emittedBetas := baseBetas
		for _, beta := range hd.AnthropicBetaStrip {
			emittedBetas = withoutClaudeBeta(emittedBetas, beta)
		}
		for _, beta := range hd.AnthropicBetaAppend {
			emittedBetas = withClaudeBetaAppended(emittedBetas, beta)
		}
		if strings.TrimSpace(emittedBetas) == "" {
			r.Header.Del("Anthropic-Beta")
			return
		}
		r.Header.Set("Anthropic-Beta", emittedBetas)
	}
	applyBetaHeader()
	misc.EnsureHeader(r.Header, ginHeaders, "Anthropic-Version", "2023-06-01")
	// Only set browser access header for API key mode; real Claude Code CLI does not send it.
	if useAPIKey {
		misc.EnsureHeader(r.Header, ginHeaders, "Anthropic-Dangerous-Direct-Browser-Access", "true")
	}
	misc.EnsureHeader(r.Header, ginHeaders, "X-App", "cli")
	// Values below match Claude Code 2.1.280 / @anthropic-ai/sdk 0.112.1.
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Retry-Count", "0")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Runtime", "node")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Lang", "js")
	// Claude Code omits X-Stainless-Timeout on count_tokens.
	if !countTokens {
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Timeout", hdrDefault(hd.Timeout, "600"))
	}
	// Session ID: stable per auth/apiKey, matches Claude Code's X-Claude-Code-Session-Id header.
	misc.EnsureHeader(r.Header, ginHeaders, "X-Claude-Code-Session-Id", helps.CachedSessionID(apiKey))
	// Per-request UUID, matches Claude Code's x-client-request-id for first-party API.
	// On api.anthropic.com, synthesize a fresh UUID unless passthrough mode is configured.
	// On non-Anthropic bases (third-party gateways), unsolicited UUIDs are never synthesized,
	// but when the client explicitly supplies an x-client-request-id, it is forwarded in both
	// "on" and "passthrough" modes to preserve caller trace continuity across proxies (pinned
	// by TestClaudeP4_HelperRequestIDFollowsUpstreamBase and TestApplyClaudeHeaders_GatewayClientRequestIDModes).
	// Setting client-request-id="off" removes the header on both first-party and gateway endpoints.
	incomingClientRequestID := claudeIncomingHeaderValue(ginHeaders, "x-client-request-id")
	if hd.ClientRequestID != config.ClaudeClientRequestIDOff {
		if hd.ClientRequestID == config.ClaudeClientRequestIDPassthrough {
			if incomingClientRequestID != "" {
				r.Header.Set("x-client-request-id", incomingClientRequestID)
			} else {
				r.Header.Del("X-Client-Request-Id")
				r.Header.Del("x-client-request-id")
			}
		} else if isAnthropicBase {
			r.Header.Set("x-client-request-id", uuid.New().String())
		} else if incomingClientRequestID != "" {
			r.Header.Set("x-client-request-id", incomingClientRequestID)
		}
	} else {
		r.Header.Del("X-Client-Request-Id")
		r.Header.Del("x-client-request-id")
	}
	r.Header.Set("Connection", "keep-alive")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		// SSE streams must not be compressed: the downstream scanner reads
		// line-delimited text and cannot parse compressed bytes.  Using
		// "identity" tells the upstream to send an uncompressed stream.
		r.Header.Set("Accept-Encoding", "identity")
	} else {
		r.Header.Set("Accept", "application/json")
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	// Legacy mode keeps OS/Arch runtime-derived; stabilized mode pins OS/Arch
	// to the configured baseline while still allowing newer official
	// User-Agent/package/runtime tuples to upgrade the software fingerprint.
	if stabilizeDeviceProfile {
		helps.ApplyClaudeDeviceProfileHeaders(r, deviceProfile)
	} else {
		helps.ApplyClaudeLegacyDeviceHeaders(r, ginHeaders, cfg)
	}
	for _, headerName := range []string{
		"X-Claude-Code-Agent-Id",
		"X-Claude-Code-Parent-Agent-Id",
		"X-Claude-Remote-Container-Id",
		"X-Claude-Remote-Session-Id",
		"X-Client-App",
		"X-Anthropic-Additional-Protection",
	} {
		if value := claudeIncomingHeaderValue(ginHeaders, headerName); value != "" {
			r.Header.Set(headerName, value)
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, ginHeaders)
	if isAnthropicBase {
		applyBetaHeader()
	}
	// Re-enforce Accept-Encoding: identity after ApplyCustomHeadersFromAttrs, which
	// may override it with a user-configured value.  Compressed SSE breaks the line
	// scanner regardless of user preference, so this is non-negotiable for streams.
	if stream {
		r.Header.Set("Accept-Encoding", "identity")
	}
}

func claudeCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func checkSystemInstructions(payload []byte) []byte {
	return checkSystemInstructionsWithSigningMode(payload, false, false, false, "2.1.280", "cli", "", false, "", "")
}

func isClaudeOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

// prepareClaudeOAuthToolNamesForUpstream applies the Claude OAuth tool-name
// transforms in the same order across request paths. Remap runs before prefixing
// so any future non-empty prefix still composes correctly with the per-request
// reverse map.
func prepareClaudeOAuthToolNamesForUpstream(ctx context.Context, body []byte, prefix string, prefixDisabled bool) ([]byte, map[string]string) {
	body, reverseMap := remapOAuthToolNames(body, ctx)
	if !prefixDisabled {
		body = applyClaudeToolPrefix(body, prefix)
	}
	return body, reverseMap
}

// restoreClaudeOAuthToolNamesFromResponse undoes the Claude OAuth tool-name
// transforms for non-stream responses in reverse order. A drifted or ambiguous
// MCP alias that cannot be restored unambiguously is a request-scoped error
// (upstream 22392c537d95).
func restoreClaudeOAuthToolNamesFromResponse(body []byte, prefix string, prefixDisabled bool, reverseMap map[string]string) ([]byte, error) {
	if !prefixDisabled {
		body = stripClaudeToolPrefixFromResponse(body, prefix)
	}
	return reverseRemapOAuthToolNames(body, reverseMap)
}

// restoreClaudeOAuthToolNamesFromStreamLine undoes the Claude OAuth tool-name
// transforms for SSE lines in reverse order.
func restoreClaudeOAuthToolNamesFromStreamLine(line []byte, prefix string, prefixDisabled bool, reverseMap map[string]string) ([]byte, error) {
	if !prefixDisabled {
		line = stripClaudeToolPrefixFromStreamLine(line, prefix)
	}
	return reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
}

// remapOAuthToolNames renames third-party tool names to Claude Code equivalents
// and removes tools without an official counterpart. This prevents Anthropic from
// fingerprinting the request as a third-party client via tool naming patterns.
//
// It operates on: tools[].name, tool_choice.name, and all tool_use/tool_reference
// references in messages. Removed tools' corresponding tool_result blocks are preserved
// (they just become orphaned, which is safe for Claude).
//
// The returned map is keyed on the upstream (TitleCase) name and maps to the
// client-supplied original name. Callers MUST pass this map to the reverse
// functions so only names the client actually caused us to rewrite are restored
// on the response. A global reverse map (the previous implementation) incorrectly
// rewrote names the client originally sent in TitleCase (e.g. a client's `Bash`)
// when any OTHER tool in the same request triggered a forward rename (e.g.
// another tool's `glob`→`Glob`), because the global reverse map contained `Bash`→`bash`
// regardless of what the client originally sent.
func remapOAuthToolNames(body []byte, ctx context.Context) ([]byte, map[string]string) {
	reverseMap := make(map[string]string, len(oauthToolRenameMap))
	recordRename := func(original, renamed string) {
		// Preserve the first-seen original name if the same upstream name is
		// produced from multiple call sites; they all map back identically.
		if _, exists := reverseMap[renamed]; !exists {
			reverseMap[renamed] = original
		}
	}

	// Third-party tools are aliased to Claude Code-style MCP names, matching real
	// Claude Code 2.1.220 naming. Alias identity belongs to the downstream caller
	// (the API key presented to the proxy), so aliases stay stable across OAuth
	// refresh and auth failover while each caller keeps one shared virtual MCP
	// server. The official-name map below still wins for OpenCode builtins so
	// their TitleCase equivalents keep avoiding Anthropic's tool-name fingerprint.
	secret := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	if secret == "" {
		secret = "cpa-claude-mcp-default-caller"
	}
	aliasMap := make(map[string]string)
	protectedNames := helps.AugmentClaudeBuiltinToolRegistry(body, nil)
	for name, official := range oauthToolRenameMap {
		protectedNames[name] = true
		protectedNames[official] = true
	}

	// reservedNames guards alias allocation: declared tool names, protected
	// names, and previously allocated aliases can never collide.
	reservedNames := make(map[string]bool, len(protectedNames)+8)
	for name := range protectedNames {
		reservedNames[name] = true
	}

	// renameRef resolves one reference through the official-name map first, then
	// the request-local MCP alias map.
	renameRef := func(name string) (string, bool) {
		if newName, ok := oauthToolRenameMap[name]; ok && newName != name {
			return newName, true
		}
		if newName, ok := aliasMap[name]; ok && newName != name {
			return newName, true
		}
		return name, false
	}

	// Build the alias map from tool declarations before rewriting anything, so
	// every historical reference resolves through the same request-local map.
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if name := tool.Get("name").String(); name != "" {
				reservedNames[name] = true
			}
			return true
		})
		passthroughMCPTools := make([]string, 0, 4)
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				return true
			}
			name := tool.Get("name").String()
			if name == "" {
				return true
			}
			if helps.IsClaudeMCPToolName(name) {
				// Caller-owned MCP tool: forwarded untouched, but tracked so the
				// response resolver can recover hybrid names when the model mixes
				// the virtual server prefix with the caller's tool name
				// (upstream 22392c537d95).
				passthroughMCPTools = append(passthroughMCPTools, name)
				return true
			}
			if protectedNames[name] {
				return true
			}
			if _, exists := aliasMap[name]; exists {
				return true
			}
			alias, allocated := helps.AllocateClaudeMCPToolAlias(secret, name, reservedNames)
			if !allocated {
				return true
			}
			aliasMap[name] = alias
			reservedNames[alias] = true
			return true
		})
		recordPassthroughMCPTools(recordRename, aliasMap, passthroughMCPTools)
	}

	// 1. Rewrite tools array in a single pass (if present).
	// IMPORTANT: do not mutate names first and then rebuild from an older gjson
	// snapshot. gjson results are snapshots of the original bytes; rebuilding from a
	// stale snapshot will preserve removals but overwrite renamed names back to their
	// original lowercase values.
	if tools.Exists() && tools.IsArray() {

		var toolsJSON strings.Builder
		toolsJSON.WriteByte('[')
		toolCount := 0
		tools.ForEach(func(_, tool gjson.Result) bool {
			// Keep Anthropic built-in tools (web_search, code_execution, etc.) unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if toolCount > 0 {
					toolsJSON.WriteByte(',')
				}
				toolsJSON.WriteString(tool.Raw)
				toolCount++
				return true
			}

			name := tool.Get("name").String()
			if oauthToolsToRemove[name] {
				return true
			}

			toolJSON := tool.Raw
			if newName, ok := renameRef(name); ok {
				updatedTool, err := sjson.Set(toolJSON, "name", newName)
				if err == nil {
					toolJSON = updatedTool
					recordRename(name, newName)
				}
			}

			if toolCount > 0 {
				toolsJSON.WriteByte(',')
			}
			toolsJSON.WriteString(toolJSON)
			toolCount++
			return true
		})
		toolsJSON.WriteByte(']')
		body, _ = sjson.SetRawBytes(body, "tools", []byte(toolsJSON.String()))
	}

	// 2. Rename tool_choice if it references a known tool
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	if toolChoiceType == "tool" {
		tcName := gjson.GetBytes(body, "tool_choice.name").String()
		if oauthToolsToRemove[tcName] {
			// The chosen tool was removed from the tools array, so drop tool_choice to
			// keep the payload internally consistent and fall back to normal auto tool use.
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		} else if newName, ok := renameRef(tcName); ok {
			body, _ = sjson.SetBytes(body, "tool_choice.name", newName)
			recordRename(tcName, newName)
		}
	}

	// 3. Rename tool references in messages
	messages := gjson.GetBytes(body, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if newName, ok := renameRef(name); ok {
						path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(name, newName)
					}
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if newName, ok := renameRef(toolName); ok {
						path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(toolName, newName)
					}
				case "tool_addition", "tool_removal":
					if namePath := claudeToolChangeNamePath(part); namePath != "" {
						changeToolName := part.Get(namePath).String()
						if newName, ok := renameRef(changeToolName); ok {
							changePath := fmt.Sprintf("messages.%d.content.%d.%s", msgIndex.Int(), contentIndex.Int(), namePath)
							body, _ = sjson.SetBytes(body, changePath, newName)
							recordRename(changeToolName, newName)
						}
					}
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					toolID := part.Get("tool_use_id").String()
					_ = toolID // tool_use_id stays as-is
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if newName, ok := renameRef(nestedToolName); ok {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, newName)
									recordRename(nestedToolName, newName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body, reverseMap
}

// The MCP alias resolver below is ported verbatim-adapted from upstream
// claude_executor_request.go (end state incl. 22392c537d95 hybrid passthrough
// recovery). It restores the exact caller-supplied tool name for aliases the
// model may have mangled, while passing through names that were never remapped.

type claudeMCPAliasParts struct {
	server   string
	toolID   string
	semantic string
}

type claudeMCPAliasEntry struct {
	alias    string
	original string
	parts    claudeMCPAliasParts
}

type claudeMCPAliasResolver struct {
	exact        map[string]string
	aliases      []claudeMCPAliasEntry
	servers      map[string]struct{}
	passthroughs []string
}

// claudeMCPAliasRestoreError fails the request when an upstream tool name
// cannot be restored to exactly one caller-declared tool. Silently forwarding
// a drifted alias would break the client's own tool dispatch, so the refusal
// is request-scoped: the credential is not at fault (upstream 22392c537d95).
type claudeMCPAliasRestoreError struct {
	error
}

func (e claudeMCPAliasRestoreError) Unwrap() error {
	return e.error
}

func (claudeMCPAliasRestoreError) IsRequestScoped() bool {
	return true
}

func newClaudeMCPAliasResolver(reverseMap map[string]string) claudeMCPAliasResolver {
	resolver := claudeMCPAliasResolver{
		exact:        reverseMap,
		aliases:      make([]claudeMCPAliasEntry, 0, len(reverseMap)),
		servers:      make(map[string]struct{}),
		passthroughs: make([]string, 0),
	}
	for alias, original := range reverseMap {
		if alias == original {
			// Caller-owned MCP tool recorded for exact passthrough and fallback hybrid
			// recovery. It must not register a virtual server or take part in fuzzy
			// client-tool alias recovery.
			resolver.passthroughs = append(resolver.passthroughs, original)
			continue
		}
		parts, ok := parseClaudeMCPAlias(alias)
		if !ok {
			continue
		}
		resolver.aliases = append(resolver.aliases, claudeMCPAliasEntry{
			alias:    alias,
			original: original,
			parts:    parts,
		})
		resolver.servers[parts.server] = struct{}{}
	}
	return resolver
}

func parseClaudeMCPAlias(name string) (claudeMCPAliasParts, bool) {
	if !helps.IsClaudeMCPToolName(name) {
		return claudeMCPAliasParts{}, false
	}
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return claudeMCPAliasParts{}, false
	}
	server, tool, ok := strings.Cut(rest, "__")
	if !ok || server == "" {
		return claudeMCPAliasParts{}, false
	}
	toolID, semantic, ok := strings.Cut(tool, "_")
	if !ok || toolID == "" || semantic == "" {
		return claudeMCPAliasParts{}, false
	}
	return claudeMCPAliasParts{server: server, toolID: toolID, semantic: semantic}, true
}

func claudeMCPAliasServer(name string) string {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return ""
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return ""
	}
	return server
}

// recordPassthroughMCPTools remembers caller-owned MCP tool names that were left
// untouched. Without this the response resolver would treat such a name as a
// drifted alias whenever the derived two-word virtual server happens to equal a
// real MCP server name, and would either restore the wrong tool or fail the
// request. Recording is skipped when nothing was aliased so an untouched request
// keeps an empty reverse map and the restore path stays a no-op.
// (upstream 22392c537d95)
func recordPassthroughMCPTools(recordRename func(original, renamed string), forwardMap map[string]string, passthrough []string) {
	if len(forwardMap) == 0 {
		return
	}
	for _, name := range passthrough {
		recordRename(name, name)
	}
}

// claudeToolChangeNamePath returns the path, relative to a mid-conversation
// tool_addition or tool_removal block, of the tool name that must carry the
// same MCP alias as tools[]. A tool_reference names a tool declared in tools[];
// upstream rejects a reference to an undeclared name. A tool_addition can
// instead carry a tool_definition (inline-tools-2026-09-15) whose definition is
// a tools[] entry; redefining a declared custom tool replaces it only under the
// same upstream name. Server tool definitions and MCP connector references keep
// their names, matching the tools[] rewrite.
// Ported from upstream CLIProxyAPI commit 958eeca7.
func claudeToolChangeNamePath(part gjson.Result) string {
	switch part.Get("tool.type").String() {
	case "tool_reference":
		return "tool.name"
	case "tool_definition":
		if part.Get("type").String() != "tool_addition" || helps.IsClaudeServerToolType(part.Get("tool.definition.type").String()) {
			return ""
		}
		return "tool.definition.name"
	}
	return ""
}

func (resolver claudeMCPAliasResolver) resolve(name string) (string, bool, error) {
	if original, ok := resolver.exact[name]; ok {
		if original == name {
			// Caller-owned MCP tool: forward it exactly as the client declared it.
			return "", false, nil
		}
		return original, true, nil
	}

	server := claudeMCPAliasServer(name)
	if _, known := resolver.servers[server]; !known {
		return "", false, nil
	}

	canonicalServerPrefix := "mcp__" + server + "__"
	normalizedName := name
	suffix := strings.TrimPrefix(name, canonicalServerPrefix)
	for {
		strippedSuffix, repeatedServer := strings.CutPrefix(suffix, server+"__")
		if !repeatedServer {
			break
		}
		suffix = strippedSuffix
		normalizedName = canonicalServerPrefix + suffix
		if original, exact := resolver.exact[normalizedName]; exact {
			return original, true, nil
		}
	}

	matchedOriginal := ""
	matchCount := 0
	for _, entry := range resolver.aliases {
		if entry.parts.server == server && strings.HasSuffix(name, entry.alias) {
			matchedOriginal = entry.original
			matchCount++
		}
	}
	if matchCount == 1 {
		return matchedOriginal, true, nil
	}
	if matchCount > 1 {
		return "", false, claudeMCPAliasRestoreError{fmt.Errorf("cannot restore Claude OAuth MCP tool alias %q: matched multiple declared aliases", name)}
	}

	parts, validAlias := parseClaudeMCPAlias(normalizedName)
	if validAlias {
		for _, entry := range resolver.aliases {
			if entry.parts.server == parts.server && entry.parts.semantic == parts.semantic {
				matchedOriginal = entry.original
				matchCount++
			}
		}
	}
	// Extra words in the tool component still parse, but the semantic field
	// is then wrong. Fall through to an unambiguous suffix match so word-level
	// repeats do not become restore 500s.
	if matchCount == 0 {
		var suffixMatches []claudeMCPAliasEntry
		for _, entry := range resolver.aliases {
			if entry.parts.server == server && strings.HasSuffix(normalizedName, "_"+entry.parts.semantic) {
				suffixMatches = append(suffixMatches, entry)
			}
		}
		if len(suffixMatches) == 1 {
			matchedOriginal = suffixMatches[0].original
			matchCount = 1
		} else if len(suffixMatches) > 1 {
			// If multiple candidates match (e.g. "_file" and "_read_file"),
			// choose the strictly longest semantic match when unambiguous.
			longest := suffixMatches[0]
			tie := false
			for _, candidate := range suffixMatches[1:] {
				if len(candidate.parts.semantic) > len(longest.parts.semantic) {
					longest = candidate
					tie = false
				} else if len(candidate.parts.semantic) == len(longest.parts.semantic) {
					tie = true
				}
			}
			if !tie {
				matchedOriginal = longest.original
				matchCount = 1
			} else {
				matchCount = len(suffixMatches)
			}
		}
		if matchCount == 1 {
			// This path guesses instead of failing, so leave a trace: it is the only
			// way to tell a silent wrong-tool restore from a healthy request.
			log.Debugf("claude oauth mcp alias: recovered drifted tool name %q as %q via semantic suffix", name, matchedOriginal)
		}
	}
	if matchCount == 1 {
		return matchedOriginal, true, nil
	}
	if matchCount > 1 {
		return "", false, claudeMCPAliasRestoreError{fmt.Errorf("cannot restore Claude OAuth MCP tool alias %q: semantic suffix matches multiple declared tools", name)}
	}

	if len(resolver.passthroughs) > 0 {
		// Recovery 1: The model prepended the virtual server to the full caller MCP tool name
		// (e.g. "mcp__<virtual>__<real_server>__<tool>"). After stripping the virtual server prefix,
		// re-prefixing suffix with "mcp__" produces the original caller tool name.
		reprefixed := "mcp__" + suffix
		if original, exact := resolver.exact[reprefixed]; exact && original == reprefixed {
			log.Debugf("claude oauth mcp alias: recovered hybrid passthrough tool name %q as %q via exact prefix", name, original)
			return original, true, nil
		}

		// Recovery 2: The model replaced the caller's server with the virtual server
		// (e.g. "mcp__<virtual>__<tool>"). Match suffix against the tool component of
		// declared passthrough tools. This runs strictly after client-tool alias recovery
		// so that a client tool (e.g. "Bash") is never eclipsed by a passthrough tool
		// with the same suffix (e.g. "mcp__shell__Bash").
		matchedPassthrough := ""
		passthroughMatches := 0
		for _, pt := range resolver.passthroughs {
			toolPart := pt
			if rest, ok := strings.CutPrefix(pt, "mcp__"); ok {
				if _, tool, ok := strings.Cut(rest, "__"); ok {
					toolPart = tool
				}
			}
			if toolPart == suffix {
				matchedPassthrough = pt
				passthroughMatches++
			}
		}
		if passthroughMatches == 1 {
			log.Debugf("claude oauth mcp alias: recovered hybrid passthrough tool name %q as %q via unique tool suffix", name, matchedPassthrough)
			return matchedPassthrough, true, nil
		}
		if passthroughMatches > 1 {
			return "", false, claudeMCPAliasRestoreError{fmt.Errorf("cannot restore Claude OAuth MCP tool alias %q: passthrough tool suffix matches multiple declared tools", name)}
		}
	}

	return "", false, claudeMCPAliasRestoreError{fmt.Errorf("cannot restore Claude OAuth MCP tool alias %q: no unique request-local match", name)}
}

// reverseRemapOAuthToolNames reverses the tool name mapping for non-stream responses
// using the per-request map produced by remapOAuthToolNames. Names outside the
// request-local generated MCP server are passed through unchanged
// (resolver ported from upstream claude_executor_request.go, incl. 22392c537d95).
func reverseRemapOAuthToolNames(body []byte, reverseMap map[string]string) ([]byte, error) {
	if len(reverseMap) == 0 {
		return body, nil
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body, nil
	}
	resolver := newClaudeMCPAliasResolver(reverseMap)
	var resolveErr error
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			origName, matched, errResolve := resolver.resolve(name)
			if errResolve != nil {
				resolveErr = errResolve
				return false
			}
			if matched {
				path := fmt.Sprintf("content.%d.name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			origName, matched, errResolve := resolver.resolve(toolName)
			if errResolve != nil {
				resolveErr = errResolve
				return false
			}
			if matched {
				path := fmt.Sprintf("content.%d.tool_name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		case "tool_result":
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() != "tool_reference" {
						return true
					}
					toolName := nestedPart.Get("tool_name").String()
					origName, matched, errResolve := resolver.resolve(toolName)
					if errResolve != nil {
						resolveErr = errResolve
						return false
					}
					if matched {
						path := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
						body, _ = sjson.SetBytes(body, path, origName)
					}
					return true
				})
			}
		case "tool_search_tool_result":
			toolRefs := part.Get("content.tool_references")
			if toolRefs.Exists() && toolRefs.IsArray() {
				toolRefs.ForEach(func(refIndex, refPart gjson.Result) bool {
					if refPart.Get("type").String() != "tool_reference" {
						return true
					}
					toolName := refPart.Get("tool_name").String()
					origName, matched, errResolve := resolver.resolve(toolName)
					if errResolve != nil {
						resolveErr = errResolve
						return false
					}
					if matched {
						path := fmt.Sprintf("content.%d.content.tool_references.%d.tool_name", index.Int(), refIndex.Int())
						body, _ = sjson.SetBytes(body, path, origName)
					}
					return true
				})
			}
		}
		return resolveErr == nil
	})
	return body, resolveErr
}

// reverseRemapOAuthToolNamesFromStreamLine reverses the tool name mapping for SSE
// stream lines, using the per-request reverseMap produced by remapOAuthToolNames.
func reverseRemapOAuthToolNamesFromStreamLine(line []byte, reverseMap map[string]string) ([]byte, error) {
	if len(reverseMap) == 0 {
		return line, nil
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line, nil
	}

	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line, nil
	}

	resolver := newClaudeMCPAliasResolver(reverseMap)
	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		origName, matched, errResolve := resolver.resolve(name)
		if errResolve != nil {
			return line, errResolve
		}
		if !matched {
			return line, nil
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", origName)
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		origName, matched, errResolve := resolver.resolve(toolName)
		if errResolve != nil {
			return line, errResolve
		}
		if !matched {
			return line, nil
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", origName)
	case "tool_search_tool_result":
		toolRefs := contentBlock.Get("content.tool_references")
		if !toolRefs.Exists() || !toolRefs.IsArray() {
			return line, nil
		}
		updatedPayload := payload
		var resolveErr error
		hasChange := false
		toolRefs.ForEach(func(refIndex, refPart gjson.Result) bool {
			if refPart.Get("type").String() != "tool_reference" {
				return true
			}
			toolName := refPart.Get("tool_name").String()
			origName, matched, errResolve := resolver.resolve(toolName)
			if errResolve != nil {
				resolveErr = errResolve
				return false
			}
			if matched {
				path := fmt.Sprintf("content_block.content.tool_references.%d.tool_name", refIndex.Int())
				updatedPayload, err = sjson.SetBytes(updatedPayload, path, origName)
				if err != nil {
					return false
				}
				hasChange = true
			}
			return true
		})
		if resolveErr != nil {
			return line, resolveErr
		}
		if err != nil {
			return line, fmt.Errorf("rewrite Claude OAuth MCP tool alias: %w", err)
		}
		if !hasChange {
			return line, nil
		}
		updated = updatedPayload
	default:
		return line, nil
	}
	if err != nil {
		return line, fmt.Errorf("rewrite Claude OAuth MCP tool alias: %w", err)
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...), nil
	}
	return updated, nil
}

func applyClaudeToolPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	// Collect built-in tool names from the authoritative fallback seed list and
	// augment it with any typed built-ins present in the current request body.
	builtinTools := helps.AugmentClaudeBuiltinToolRegistry(body, nil)

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(index, tool gjson.Result) bool {
			// Skip built-in tools (web_search, code_execution, etc.) which have
			// a "type" field and require their name to remain unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if n := tool.Get("name").String(); n != "" {
					builtinTools[n] = true
				}
				return true
			}
			name := tool.Get("name").String()
			if name == "" || strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("tools.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, prefix+name)
			return true
		})
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) && !builtinTools[name] {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if name == "" || strings.HasPrefix(name, prefix) || builtinTools[name] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+name)
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if toolName == "" || strings.HasPrefix(toolName, prefix) || builtinTools[toolName] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+toolName)
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if nestedToolName != "" && !strings.HasPrefix(nestedToolName, prefix) && !builtinTools[nestedToolName] {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, prefix+nestedToolName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body
}

func stripClaudeToolPrefixFromResponse(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if !strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(name, prefix))
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if !strings.HasPrefix(toolName, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.tool_name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(toolName, prefix))
		case "tool_result":
			// Handle nested tool_reference blocks inside tool_result.content[]
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() == "tool_reference" {
						nestedToolName := nestedPart.Get("tool_name").String()
						if strings.HasPrefix(nestedToolName, prefix) {
							nestedPath := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
							body, _ = sjson.SetBytes(body, nestedPath, strings.TrimPrefix(nestedToolName, prefix))
						}
					}
					return true
				})
			}
		}
		return true
	})
	return body
}

func stripClaudeToolPrefixFromStreamLine(line []byte, prefix string) []byte {
	if prefix == "" {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if !strings.HasPrefix(name, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", strings.TrimPrefix(name, prefix))
		if err != nil {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if !strings.HasPrefix(toolName, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", strings.TrimPrefix(toolName, prefix))
		if err != nil {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

// getClientUserAgent extracts the client User-Agent from the gin context.
func getClientUserAgent(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.GetHeader("User-Agent")
	}
	return ""
}

// parseEntrypointFromUA extracts the entrypoint from a Claude Code User-Agent.
// Format: "claude-cli/x.y.z (external, cli)" → "cli"
// Format: "claude-cli/x.y.z (external, vscode)" → "vscode"
// Returns "cli" if parsing fails or UA is not Claude Code.
func parseEntrypointFromUA(userAgent string) string {
	// Find content inside parentheses
	start := strings.Index(userAgent, "(")
	end := strings.LastIndex(userAgent, ")")
	if start < 0 || end <= start {
		return "cli"
	}
	inner := userAgent[start+1 : end]
	// Split by comma, take the second part (entrypoint is at index 1, after USER_TYPE)
	// Format: "(USER_TYPE, ENTRYPOINT[, extra...])"
	parts := strings.Split(inner, ",")
	if len(parts) >= 2 {
		ep := strings.TrimSpace(parts[1])
		if ep != "" {
			return ep
		}
	}
	return "cli"
}

// getWorkloadFromContext extracts workload identifier from the gin request headers.
func getWorkloadFromContext(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return strings.TrimSpace(ginCtx.GetHeader("X-CPA-Claude-Workload"))
	}
	return ""
}

// getCloakConfigFromAuth extracts cloak configuration from auth attributes.
// Returns (cloakMode, strictMode, sensitiveWords, cacheUserID).
func getCloakConfigFromAuth(auth *cliproxyauth.Auth) (string, bool, []string, bool) {
	if auth == nil || auth.Attributes == nil {
		return "auto", false, nil, false
	}

	cloakMode := auth.Attributes["cloak_mode"]
	if cloakMode == "" {
		cloakMode = "auto"
	}

	strictMode := strings.ToLower(auth.Attributes["cloak_strict_mode"]) == "true"

	var sensitiveWords []string
	if wordsStr := auth.Attributes["cloak_sensitive_words"]; wordsStr != "" {
		sensitiveWords = strings.Split(wordsStr, ",")
		for i := range sensitiveWords {
			sensitiveWords[i] = strings.TrimSpace(sensitiveWords[i])
		}
	}

	cacheUserID := strings.EqualFold(strings.TrimSpace(auth.Attributes["cloak_cache_user_id"]), "true")

	return cloakMode, strictMode, sensitiveWords, cacheUserID
}

// injectFakeUserID generates and injects a fake user ID into the request metadata.
// When useCache is false, a new user ID is generated for every call.
func injectFakeUserID(payload []byte, apiKey string, useCache bool) []byte {
	generateID := func() string {
		if useCache {
			return helps.CachedUserID(apiKey)
		}
		return helps.GenerateFakeUserID()
	}

	metadata := gjson.GetBytes(payload, "metadata")
	if !metadata.Exists() {
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", generateID())
		return payload
	}

	existingUserID := gjson.GetBytes(payload, "metadata.user_id").String()
	if existingUserID == "" || !helps.IsValidUserID(existingUserID) {
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", generateID())
	}
	return payload
}

// fingerprintSalt is the salt used by Claude Code to compute the 3-char build fingerprint.
const fingerprintSalt = "59cf53e54c78"

// claudeCodeFableReportingOutcomes is the system text block Claude Code 2.1.280
// injects for Fable 5.1 / Mythos 5.1 models (upstream de4aa600280e).
const claudeCodeFableReportingOutcomes = `# Reporting outcomes

Report what actually happened, not what you intended. When you say something is done, sent, saved, fixed, or verified, that claim must rest on a result you observed in this session — tool output, the file as it now reads, the page as it now loads — not on what the step should have produced. If you did not check, say you did not check. If any step failed, was skipped, or came back different from what you expected, say so in the first sentence of your report, before anything else, even when the rest of the work succeeded. Never quietly work around a failure in a way that makes it look resolved; a problem the user can see is recoverable, one your summary hides is not. When you stop before the task is complete, your first line says so plainly and names what is left. Do not describe partial work as done, and do not let a summary read as more certain than the evidence behind it.`

// isClaudeFable51Model reports whether the model is specifically Fable 5.1 /
// Mythos 5.1, matching native Claude Code 2.1.280 family/major/minor checks
// (upstream de4aa600280e).
func isClaudeFable51Model(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, target := range []string{"fable-5-1", "fable-5.1", "mythos-5-1", "mythos-5.1"} {
		idx := strings.Index(m, target)
		if idx != -1 {
			nextIdx := idx + len(target)
			if nextIdx >= len(m) || m[nextIdx] < '0' || m[nextIdx] > '9' {
				return true
			}
		}
	}
	return false
}

// firstClaudeUserMessageIndex returns the index of the first role=user message.
func firstClaudeUserMessageIndex(payload []byte) int {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return -1
	}

	firstUserIdx := -1
	messages.ForEach(func(idx, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			firstUserIdx = int(idx.Int())
			return false
		}
		return true
	})
	return firstUserIdx
}

func isClaudeCodeContextReminder(text string) bool {
	return strings.HasPrefix(text, "<system-reminder>") && strings.Contains(text, "</system-reminder>")
}

func isClaudeCodeCurrentDateReminder(text string) bool {
	return strings.HasPrefix(text, "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is ")
}

// marshalJSONStringWithoutHTMLEscape marshals s as a JSON string without HTML
// escaping, matching upstream's billing/fingerprint serialization
// (upstream 086ad91bd970).
func marshalJSONStringWithoutHTMLEscape(value string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	return strings.TrimRight(buf.String(), "\n")
}

// computeFingerprint computes the 3-char build fingerprint that Claude Code embeds in cc_version.
// Algorithm: SHA256(salt + messageText[4] + messageText[7] + messageText[20] + version)[:3]
func computeFingerprint(messageText, version string) string {
	indices := [3]int{4, 7, 20}
	runes := []rune(messageText)
	var sb strings.Builder
	for _, idx := range indices {
		if idx < len(runes) {
			sb.WriteRune(runes[idx])
		} else {
			sb.WriteRune('0')
		}
	}
	input := fingerprintSalt + sb.String() + version
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])[:3]
}

// generateBillingHeader creates the x-anthropic-billing-header text block that
// Claude Code prepends to its system prompt. The tag chain follows the
// upstream order (086ad91bd970):
//
//	x-anthropic-billing-header: cc_version=<ver>.<build>; cc_entrypoint=<ep>; [cch=00000;] [cc_workload=<wl>;] [cc_is_subagent=true;] [cc_prev_req=<id>;] [cc_prompt_id=<uuid>;]
//
// cch is present only on signed paths; cc_is_subagent is emitted only for
// detected subagent requests; cc_prev_req and cc_prompt_id carry upstream
// request continuity and appear only on signed paths.
func generateBillingHeader(payload []byte, experimentalCCHSigning bool, version, messageText, entrypoint, workload string, isSubagent bool, prevReq, promptID string) string {
	if entrypoint == "" {
		entrypoint = "cli"
	}
	buildHash := computeFingerprint(messageText, version)
	var b strings.Builder
	b.WriteString("x-anthropic-billing-header: cc_version=")
	b.WriteString(version)
	b.WriteByte('.')
	b.WriteString(buildHash)
	b.WriteString("; cc_entrypoint=")
	b.WriteString(entrypoint)
	b.WriteByte(';')

	if experimentalCCHSigning {
		b.WriteString(" cch=00000;")
	}
	if workload != "" {
		b.WriteString(" cc_workload=")
		b.WriteString(workload)
		b.WriteByte(';')
	}
	if isSubagent {
		b.WriteString(" cc_is_subagent=true;")
	}
	if experimentalCCHSigning {
		if prevReq != "" {
			b.WriteString(" cc_prev_req=")
			b.WriteString(prevReq)
			b.WriteByte(';')
		}
		if promptID != "" {
			b.WriteString(" cc_prompt_id=")
			b.WriteString(promptID)
			b.WriteByte(';')
		}
	}
	return b.String()
}

// resolveClaudeContinuityTags resolves cc_prev_req and cc_prompt_id for the
// billing header from caller-supplied billing tags and the persisted upstream
// request continuity state (ported from upstream 086ad91bd970).
func resolveClaudeContinuityTags(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	incomingHeaders http.Header,
	payload []byte,
	confirmedClaudeCode bool,
	existingPrevReq, existingPromptID string,
) (prevReq, promptID string, cCtx helps.ClaudeContinuityContext, ok bool) {
	hasExecutionMetadata := helps.ClaudeExecutionMetadataFromContext(ctx)
	sessionID := helps.ClaudeSessionIDFromContext(ctx)
	if sessionID == "" && auth != nil {
		sessionID = helps.ClaudeAgentSessionUUIDForRequest(incomingHeaders, payload, payload, confirmedClaudeCode)
	}
	if sessionID == "" || auth == nil {
		return "", "", helps.ClaudeContinuityContext{}, false
	}

	credIdentity := claudeDiagnosticsCredentialIdentity(auth)
	isNewTurn := helps.IsClaudeNewPromptTurn(payload)
	continuityKey, seq, prevMsgID, storedPrevReq, storedPromptID := helps.BeginClaudeContinuity(credIdentity, sessionID, isNewTurn, existingPromptID)

	if existingPromptID != "" {
		promptID = existingPromptID
	} else if prevMsgID != "" && storedPromptID != "" && (hasExecutionMetadata || !isNewTurn) {
		promptID = storedPromptID
	} else if !hasExecutionMetadata {
		promptID = helps.ClaudeDeterministicPromptID("cpa:prompt:" + claudeBillingFingerprintMessageText(payload))
	} else {
		promptID = storedPromptID
	}

	if (hasExecutionMetadata || existingPrevReq != "") && storedPrevReq != "" {
		prevReq = storedPrevReq
	} else {
		prevReq = existingPrevReq
	}

	cCtx = helps.ClaudeContinuityContext{
		Key:         continuityKey,
		Sequence:    seq,
		PromptID:    promptID,
		Initialized: true,
	}
	if hasExecutionMetadata || existingPrevReq != "" {
		cCtx.PreviousMessageID = prevMsgID
		cCtx.PreviousRequestID = storedPrevReq
	} else {
		cCtx.PreviousMessageID = ""
		cCtx.PreviousRequestID = ""
	}
	return prevReq, promptID, cCtx, true
}

// claudeBillingFingerprintMessageText returns the text Claude Code feeds the
// billing fingerprint: the first user message's text, skipping system-reminder
// wrappers (ported from upstream 086ad91bd970).
func claudeBillingFingerprintMessageText(payload []byte) string {
	idx := firstClaudeUserMessageIndex(payload)
	if idx < 0 {
		return ""
	}
	content := gjson.GetBytes(payload, fmt.Sprintf("messages.%d.content", idx))
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		messageText := ""
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				text := part.Get("text").String()
				if !isClaudeCodeCurrentDateReminder(text) && !isClaudeCodeContextReminder(text) {
					messageText = text
				}
			}
			return true
		})
		return messageText
	}
	return ""
}

// claudeCCHFallbackBillingHeader rebuilds the signed billing header for a
// request whose system blocks survived without a billing header (for example
// after payload rules replaced system), so the CCH signing path still has a
// chain to sign (ported from upstream 086ad91bd970).
func claudeCCHFallbackBillingHeader(ctx context.Context, cfg *config.Config, payload []byte, entrypoint string) string {
	isProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(payload)
	prevReq, promptID := helps.ExtractClaudeBillingTags(payload)
	if !isProbeOrHelper {
		continuityCtx := helps.ClaudeContinuityContextFromContext(ctx)
		if prevReq == "" && continuityCtx != nil {
			prevReq = continuityCtx.PreviousRequestID
		}
		if promptID == "" && continuityCtx != nil {
			promptID = continuityCtx.PromptID
		}
	}
	incomingHeaders := helps.IncomingHeadersFromContext(ctx)
	isSubagent := helps.IsClaudeSubagentRequest(incomingHeaders, payload)
	return generateBillingHeader(
		payload,
		true,
		helps.DefaultClaudeVersion(cfg),
		claudeBillingFingerprintMessageText(payload),
		entrypoint,
		getWorkloadFromContext(ctx),
		isSubagent,
		prevReq,
		promptID,
	)
}

func checkSystemInstructionsWithMode(payload []byte, strictMode bool) []byte {
	return checkSystemInstructionsWithSigningMode(payload, strictMode, false, false, "2.1.280", "cli", "", false, "", "")
}

// checkSystemInstructionsWithSigningMode injects Claude Code-style system blocks:
//
//	system[0]: billing header (no cache_control)
//	system[1]: agent identifier
//	system[2]: Fable/Mythos 5.1 reporting outcomes block (Fable models only)
//	system[3]: core static prompt (intro + instructions + tone)
//	user system messages moved to first user message (non-strict mode)
func checkSystemInstructionsWithSigningMode(payload []byte, strictMode bool, experimentalCCHSigning bool, oauthMode bool, version, entrypoint, workload string, isSubagent bool, prevReq, promptID string) []byte {
	system := gjson.GetBytes(payload, "system")

	// Extract the fingerprint source the way Claude Code does: the first user
	// message's text, skipping system-reminder wrappers (upstream 086ad91bd970).
	messageText := claudeBillingFingerprintMessageText(payload)

	// Skip if already injected
	firstText := gjson.GetBytes(payload, "system.0.text").String()
	if strings.HasPrefix(firstText, "x-anthropic-billing-header:") {
		return payload
	}

	billingText := generateBillingHeader(payload, experimentalCCHSigning, version, messageText, entrypoint, workload, isSubagent, prevReq, promptID)
	billingBlock := buildTextBlock(billingText, nil)

	// Build system blocks matching real Claude Code structure.
	// Important: Claude Code's internal cacheScope='org' does NOT serialize to
	// scope='org' in the API request. Only scope='global' is sent explicitly.
	// The system prompt prefix block is sent without cache_control.
	agentBlock := buildTextBlock("You are Claude Code, Anthropic's official CLI for Claude.", nil)
	staticPrompt := strings.Join([]string{
		helps.ClaudeCodeIntro,
		helps.ClaudeCodeSystem,
		helps.ClaudeCodeDoingTasks,
		helps.ClaudeCodeToneAndStyle,
		helps.ClaudeCodeOutputEfficiency,
	}, "\n\n")
	staticBlock := buildTextBlock(staticPrompt, nil)

	systemBlocks := []string{billingBlock, agentBlock}
	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "model").String()))
	if isClaudeFable51Model(model) && !helps.IsClaudeProbeOrHelperRequest(payload) {
		systemBlocks = append(systemBlocks, buildTextBlock(claudeCodeFableReportingOutcomes, nil))
	}
	systemBlocks = append(systemBlocks, staticBlock)

	// Collect user system instructions before serializing system so histories
	// bound to an advisor call/result keep them at top level instead of a
	// mid-conversation splice (upstream 7c2f6ce0; advisor detection restricted
	// to server_tool_use by f86a33f72175).
	var userSystemParts []string
	if !strictMode {
		if system.IsArray() {
			system.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					txt := strings.TrimSpace(part.Get("text").String())
					if txt != "" {
						userSystemParts = append(userSystemParts, txt)
					}
				}
				return true
			})
		} else if system.Type == gjson.String && strings.TrimSpace(system.String()) != "" {
			userSystemParts = append(userSystemParts, strings.TrimSpace(system.String()))
		}
	}

	advisorHistory := len(userSystemParts) > 0 && claudeHistoryHasAdvisorCallOrResult(payload)
	if advisorHistory {
		// The encrypted advisor result is bound to the message layout; keep the
		// caller's system blocks in top-level system so messages[] is untouched.
		if oauthMode {
			if combined := sanitizeForwardedSystemPrompt(strings.Join(userSystemParts, "\n\n")); combined != "" {
				systemBlocks = append(systemBlocks, buildTextBlock(combined, nil))
			}
		} else {
			for _, part := range userSystemParts {
				systemBlocks = append(systemBlocks, buildTextBlock(part, nil))
			}
		}
	}

	systemResult := "[" + strings.Join(systemBlocks, ",") + "]"
	payload, _ = sjson.SetRawBytes(payload, "system", []byte(systemResult))

	// Move user system instructions into the first user message (non-strict mode)
	if !strictMode && !advisorHistory {
		if len(userSystemParts) > 0 {
			combined := strings.Join(userSystemParts, "\n\n")
			if oauthMode {
				combined = sanitizeForwardedSystemPrompt(combined)
			}
			if strings.TrimSpace(combined) != "" {
				payload = prependToFirstUserMessage(payload, combined)
			}
		}
	}

	return payload
}

// claudeHistoryHasAdvisorCallOrResult reports whether messages contains an
// advisor tool invocation (server_tool_use) or advisor result
// (advisor_tool_result / advisor_redacted_result). Anthropic cryptographically
// binds the encrypted advisor result to the conversation layout; any
// mid-conversation system splice shifts message indices and causes upstream
// 400 errors.
//
// Ported in its upstream end state: only server_tool_use blocks count as
// advisor invocations — a client-side tool_use merely named "advisor" must not
// trigger advisor cloaking restrictions (upstream f86a33f72175).
func claudeHistoryHasAdvisorCallOrResult(payload []byte) bool {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}
	for _, msg := range messages.Array() {
		content := msg.Get("content")
		if content.IsArray() {
			for _, block := range content.Array() {
				blockType := block.Get("type").String()
				switch blockType {
				case "advisor_tool_result", "advisor_redacted_result":
					return true
				case "server_tool_use":
					if block.Get("name").String() == "advisor" {
						return true
					}
				case "tool_result":
					inner := block.Get("content")
					if inner.IsArray() {
						for _, b := range inner.Array() {
							if b.Get("type").String() == "advisor_redacted_result" {
								return true
							}
						}
					} else if inner.IsObject() {
						if inner.Get("type").String() == "advisor_redacted_result" {
							return true
						}
					}
				}
			}
		} else if content.IsObject() {
			blockType := content.Get("type").String()
			if blockType == "advisor_tool_result" || blockType == "advisor_redacted_result" {
				return true
			}
		}
	}
	return false
}

// sanitizeForwardedSystemPrompt reduces forwarded third-party system context to a
// tiny neutral reminder for Claude OAuth cloaking. The goal is to preserve only
// the minimum tool/task guidance while removing virtually all client-specific
// prompt structure that Anthropic may classify as third-party agent traffic.
func sanitizeForwardedSystemPrompt(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return strings.TrimSpace(`Use the available tools when needed to help with software engineering tasks.
Keep responses concise and focused on the user's request.
Prefer acting on the user's task over describing product-specific workflows.`)
}

// buildTextBlock constructs a JSON text block object with proper escaping.
// Uses sjson.SetBytes to handle multi-line text, quotes, and control characters.
// cacheControl is optional; pass nil to omit cache_control.
func buildTextBlock(text string, cacheControl map[string]string) string {
	block := []byte(`{"type":"text"}`)
	block, _ = sjson.SetBytes(block, "text", text)
	if cacheControl != nil && len(cacheControl) > 0 {
		// Build cache_control JSON manually to avoid sjson map marshaling issues.
		// sjson.SetBytes with map[string]string may not produce expected structure.
		cc := `{"type":"ephemeral"`
		if t, ok := cacheControl["ttl"]; ok {
			cc += fmt.Sprintf(`,"ttl":"%s"`, t)
		}
		cc += "}"
		block, _ = sjson.SetRawBytes(block, "cache_control", []byte(cc))
	}
	return string(block)
}

// prependToFirstUserMessage prepends text content to the first user message.
// This avoids putting non-Claude-Code system instructions in system[] which
// triggers Anthropic's extra usage billing for OAuth-proxied requests.
func leadsWithToolResult(content gjson.Result) bool {
	first := content.Get("0")
	return first.Exists() && first.Get("type").String() == "tool_result"
}

func prependToFirstUserMessage(payload []byte, text string) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	// Find the first user message index
	firstUserIdx := -1
	messages.ForEach(func(idx, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			firstUserIdx = int(idx.Int())
			return false
		}
		return true
	})

	if firstUserIdx < 0 {
		return payload
	}

	prefixBlock := fmt.Sprintf(`<system-reminder>
As you answer the user's questions, you can use the following context from the system:
%s

IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>
`, text)

	contentPath := fmt.Sprintf("messages.%d.content", firstUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		newBlock := fmt.Sprintf(`{"type":"text","text":%q}`, prefixBlock)
		var newArray string
		switch {
		case content.Raw == "[]" || content.Raw == "":
			newArray = "[" + newBlock + "]"
		case leadsWithToolResult(content):
			trimmed := strings.TrimRight(content.Raw, " \t\r\n")
			newArray = trimmed[:len(trimmed)-1] + "," + newBlock + "]"
		default:
			newArray = "[" + newBlock + "," + content.Raw[1:]
		}
		payload, _ = sjson.SetRawBytes(payload, contentPath, []byte(newArray))
	} else if content.Type == gjson.String {
		newText := prefixBlock + content.String()
		payload, _ = sjson.SetBytes(payload, contentPath, newText)
	}

	return payload
}

// applyCloaking applies cloaking transformations to the payload based on config and client.
// Cloaking includes: system prompt injection, fake user ID, and sensitive word obfuscation.
// The returned boolean reports whether cloaking ran (upstream 4a5ab534f827).
func applyCloaking(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, payload []byte, model string, apiKey string, baseURL string) ([]byte, bool) {
	clientUserAgent := getClientUserAgent(ctx)
	// Enable cch signing for OAuth tokens by default (not just experimental flag).
	oauthToken := isClaudeOAuthToken(apiKey)
	useCCHSigning := oauthToken || experimentalCCHSigningEnabled(cfg, auth, baseURL)

	// Get cloak config from ClaudeKey configuration
	cloakCfg := resolveClaudeKeyCloakConfig(cfg, auth)
	attrMode, attrStrict, attrWords, attrCache := getCloakConfigFromAuth(auth)

	// Determine cloak settings
	cloakMode := attrMode
	strictMode := attrStrict
	sensitiveWords := attrWords
	cacheUserID := attrCache

	if cloakCfg != nil {
		if mode := strings.TrimSpace(cloakCfg.Mode); mode != "" {
			cloakMode = mode
		}
		if cloakCfg.StrictMode {
			strictMode = true
		}
		if len(cloakCfg.SensitiveWords) > 0 {
			sensitiveWords = cloakCfg.SensitiveWords
		}
		if cloakCfg.CacheUserID != nil {
			cacheUserID = *cloakCfg.CacheUserID
		}
	}

	// Determine if cloaking should be applied
	if !helps.ShouldCloak(cloakMode, clientUserAgent) {
		return payload, false
	}

	// Resolve upstream request continuity (cc_prev_req / cc_prompt_id) and the
	// subagent flag before the billing header is written (upstream 086ad91bd970).
	isProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(payload)
	isSubagent := false
	prevReq := ""
	promptID := ""
	var incomingHeaders http.Header
	if !isProbeOrHelper {
		incomingHeaders = helps.IncomingHeadersFromContext(ctx)
		isSubagent = helps.IsClaudeSubagentRequest(incomingHeaders, payload)
		existingPrevReq, existingPromptID := helps.ExtractClaudeBillingTags(payload)

		var cCtx helps.ClaudeContinuityContext
		var ok bool
		prevReq, promptID, cCtx, ok = resolveClaudeContinuityTags(ctx, auth, incomingHeaders, payload, false, existingPrevReq, existingPromptID)
		if ok {
			if continuityCtx := helps.ClaudeContinuityContextFromContext(ctx); continuityCtx != nil {
				*continuityCtx = cCtx
			}
		}
	}

	// Skip system instructions for claude-3-5-haiku models
	if !strings.HasPrefix(model, "claude-3-5-haiku") {
		billingVersion := helps.DefaultClaudeVersion(cfg)
		entrypoint := parseEntrypointFromUA(clientUserAgent)
		workload := getWorkloadFromContext(ctx)
		payload = checkSystemInstructionsWithSigningMode(payload, strictMode, useCCHSigning, oauthToken, billingVersion, entrypoint, workload, isSubagent, prevReq, promptID)
	}

	// In native Claude Code 2.1.280, claude-fable-5-1 requests carry:
	// "fallbacks": [{"model": "claude-opus-5"}] (upstream de4aa600280e).
	cloakModel := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "model").String()))
	if isClaudeOpus55Model(cloakModel) && !isProbeOrHelper && !gjson.GetBytes(payload, "fallbacks").Exists() {
		payload, _ = sjson.SetRawBytes(payload, "fallbacks", []byte(`[{"model":"claude-opus-4-8"}]`))
	}
	if isClaudeFable51Model(cloakModel) && !isProbeOrHelper {
		if !gjson.GetBytes(payload, "fallbacks").Exists() {
			payload, _ = sjson.SetRawBytes(payload, "fallbacks", []byte(`[{"model":"claude-opus-5"}]`))
		}
	}
	if !isProbeOrHelper {
		payload = applyClaudeCloakThinkingDisplay(payload, false)
	}

	// Probes never use 1h cache in native Claude Code; ensure any caller-supplied
	// 1h ttl is stripped to match extended-cache-ttl beta suppression. Subagents
	// preserve caller-requested 1h cache TTL (e.g. subagentPromptCacheTtl: 1h).
	if isProbeOrHelper || (isSubagent && !helps.ClaudeSubagentRequests1h(incomingHeaders, payload)) {
		payload = stripClaudeCacheControlTTL(payload)
	}

	// Inject fake user ID
	payload = injectFakeUserID(payload, apiKey, cacheUserID)

	// Apply sensitive word obfuscation
	if len(sensitiveWords) > 0 {
		matcher := helps.BuildSensitiveWordMatcher(sensitiveWords)
		payload = helps.ObfuscateSensitiveWords(payload, matcher)
	}

	return payload, true
}

// upgradeClaudeCacheControlTTL upgrades cache_control breakpoints without an
// explicit ttl to the given ttl, preserving the native {type, ttl, scope} key
// order (upstream d7052c96af78).
func upgradeClaudeCacheControlTTL(payload []byte, ttl string) []byte {
	if ttl == "" || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	upgrade := func(path string, block gjson.Result) {
		cacheControl := block.Get("cache_control")
		if !cacheControl.IsObject() || cacheControl.Get("ttl").Exists() {
			return
		}
		blockType := cacheControl.Get("type")
		if blockType.Type != gjson.String {
			return
		}
		upgraded := `{"type":` + marshalJSONStringWithoutHTMLEscape(blockType.String()) +
			`,"ttl":` + marshalJSONStringWithoutHTMLEscape(ttl)
		if scope := cacheControl.Get("scope"); scope.Exists() {
			upgraded += `,"scope":` + scope.Raw
		}
		upgraded += "}"
		updated, errSet := sjson.SetRawBytes(payload, path+".cache_control", []byte(upgraded))
		if errSet != nil {
			return
		}
		payload = updated
	}

	forEachClaudeCacheControlBlock(payload, upgrade)
	return payload
}

// stripClaudeCacheControlTTL removes any ttl field from cache_control blocks in payload,
// downgrading {"type":"ephemeral","ttl":"..."} to {"type":"ephemeral"}.
// This ensures that when extended-cache-ttl-2025-04-11 is stripped (e.g. on probes,
// subagents, or non-OAuth credentials), the body does not retain a ttl field that would
// trigger Anthropic 400 errors or fingerprint mismatch (upstream d7052c96af78).
func stripClaudeCacheControlTTL(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	strip := func(path string, block gjson.Result) {
		cacheControl := block.Get("cache_control")
		if !cacheControl.IsObject() || !cacheControl.Get("ttl").Exists() {
			return
		}
		updated, errDel := sjson.DeleteBytes(payload, path+".cache_control.ttl")
		if errDel != nil {
			return
		}
		payload = updated
	}

	forEachClaudeCacheControlBlock(payload, strip)
	return payload
}

// forEachClaudeCacheControlBlock walks every block that can carry cache_control
// in Anthropic's evaluation order: tools, then system, then messages.
func forEachClaudeCacheControlBlock(payload []byte, visit func(path string, block gjson.Result)) {
	if tools := gjson.GetBytes(payload, "tools"); tools.IsArray() {
		tools.ForEach(func(idx, item gjson.Result) bool {
			visit(fmt.Sprintf("tools.%d", int(idx.Int())), item)
			return true
		})
	}
	if system := gjson.GetBytes(payload, "system"); system.IsArray() {
		system.ForEach(func(idx, item gjson.Result) bool {
			visit(fmt.Sprintf("system.%d", int(idx.Int())), item)
			return true
		})
	}
	if messages := gjson.GetBytes(payload, "messages"); messages.IsArray() {
		messages.ForEach(func(msgIdx, message gjson.Result) bool {
			content := message.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item gjson.Result) bool {
				visit(fmt.Sprintf("messages.%d.content.%d", int(msgIdx.Int()), int(itemIdx.Int())), item)
				return true
			})
			return true
		})
	}
}

// claudeCodeFableState records which Fable/Mythos 5.1 additions cloaking
// injected, so post-payload reconciliation only removes what CPA added and
// never caller-owned fields (upstream de4aa600280e).
type claudeCodeFableState struct {
	injectedFallbacks bool
	injectedDisplay   bool
	injectedReporting bool
}

func hasFableReportingBlock(body []byte) bool {
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() {
		str := strings.ReplaceAll(system.String(), "​", "")
		return str == claudeCodeFableReportingOutcomes || strings.Contains(str, claudeCodeFableReportingOutcomes)
	}
	for _, blk := range system.Array() {
		text := strings.ReplaceAll(blk.Get("text").String(), "​", "")
		if text == claudeCodeFableReportingOutcomes {
			return true
		}
	}
	return false
}

func captureClaudeCodeFableState(before, after []byte, cloaked bool) claudeCodeFableState {
	if !cloaked || len(before) == 0 || len(after) == 0 {
		return claudeCodeFableState{}
	}
	return claudeCodeFableState{
		injectedFallbacks: !gjson.GetBytes(before, "fallbacks").Exists() && gjson.GetBytes(after, "fallbacks").Exists(),
		injectedDisplay:   !gjson.GetBytes(before, "thinking.display").Exists() && gjson.GetBytes(after, "thinking.display").Exists(),
		injectedReporting: !hasFableReportingBlock(before) && hasFableReportingBlock(after),
	}
}

// reconcileClaudeCodeFableModelAfterPayload reconciles model-specific additions
// (Opus fallback, thinking.display=updates, and # Reporting outcomes system block)
// if payload rules rewrite the request model between Fable 5.1 and non-Fable models
// (upstream de4aa600280e).
func reconcileClaudeCodeFableModelAfterPayload(
	body []byte,
	fableState claudeCodeFableState,
	payloadTouchedFallbacks bool,
	payloadTouchedDisplay bool,
	cloaked bool,
	isProbeOrHelper bool,
) []byte {
	if !cloaked || len(body) == 0 {
		return body
	}

	// Probes and helpers must never carry Fable additions (Opus fallback, display=updates, reporting block)
	if isProbeOrHelper {
		if fableState.injectedFallbacks && !payloadTouchedFallbacks {
			body, _ = sjson.DeleteBytes(body, "fallbacks")
		}
		if fableState.injectedDisplay && !payloadTouchedDisplay {
			body, _ = sjson.DeleteBytes(body, "thinking.display")
		}
		if fableState.injectedReporting {
			system := gjson.GetBytes(body, "system")
			if system.IsArray() {
				blocks := make([]string, 0, len(system.Array()))
				removed := false
				for _, blk := range system.Array() {
					if strings.ReplaceAll(blk.Get("text").String(), "​", "") == claudeCodeFableReportingOutcomes {
						removed = true
						continue
					}
					blocks = append(blocks, blk.Raw)
				}
				if removed {
					body, _ = sjson.SetRawBytes(body, "system", []byte("["+strings.Join(blocks, ",")+"]"))
				}
			} else if strings.ReplaceAll(system.String(), "​", "") == claudeCodeFableReportingOutcomes {
				body, _ = sjson.DeleteBytes(body, "system")
			}
		}
		return body
	}
	currentModel := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	// A model rewrite may change the native fallback target. Never override a
	// caller or payload-rule fallback; only replace one inserted by the cloak.
	if fableState.injectedFallbacks && !payloadTouchedFallbacks {
		wantFallback := ""
		switch {
		case isClaudeFable51Model(currentModel):
			wantFallback = "claude-opus-5"
		case isClaudeOpus55Model(currentModel):
			wantFallback = "claude-opus-4-8"
		}
		if gjson.GetBytes(body, "fallbacks.0.model").String() != wantFallback {
			body, _ = sjson.DeleteBytes(body, "fallbacks")
		}
	}

	if isClaudeFable51Model(currentModel) {
		// Non-Fable rewritten to Fable 5.1 (or original Fable 5.1): attach Fable additions
		// unless matching payload rules explicitly configured or filtered them.
		if !gjson.GetBytes(body, "fallbacks").Exists() && !payloadTouchedFallbacks {
			body, _ = sjson.SetRawBytes(body, "fallbacks", []byte(`[{"model":"claude-opus-5"}]`))
		}
		if gjson.GetBytes(body, "thinking").Exists() {
			thinkingType := gjson.GetBytes(body, "thinking.type").String()
			if thinkingType == "adaptive" && !gjson.GetBytes(body, "thinking.display").Exists() && !payloadTouchedDisplay {
				body, _ = sjson.SetBytes(body, "thinking.display", "updates")
			} else if thinkingType != "adaptive" && fableState.injectedDisplay && !payloadTouchedDisplay {
				body, _ = sjson.DeleteBytes(body, "thinking.display")
			}
		} else if fableState.injectedDisplay && !payloadTouchedDisplay {
			body, _ = sjson.DeleteBytes(body, "thinking.display")
		}
		if !hasFableReportingBlock(body) {
			system := gjson.GetBytes(body, "system")
			if system.IsArray() {
				blocks := make([]string, 0, len(system.Array())+1)
				for _, blk := range system.Array() {
					blocks = append(blocks, blk.Raw)
				}
				blocks = append(blocks, buildTextBlock(claudeCodeFableReportingOutcomes, nil))
				body, _ = sjson.SetRawBytes(body, "system", []byte("["+strings.Join(blocks, ",")+"]"))
			} else if system.Type == gjson.String {
				str := system.String()
				blocks := []string{
					buildTextBlock(str, nil),
					buildTextBlock(claudeCodeFableReportingOutcomes, nil),
				}
				body, _ = sjson.SetRawBytes(body, "system", []byte("["+strings.Join(blocks, ",")+"]"))
			} else if !system.Exists() {
				blocks := []string{
					buildTextBlock(claudeCodeFableReportingOutcomes, nil),
				}
				body, _ = sjson.SetRawBytes(body, "system", []byte("["+strings.Join(blocks, ",")+"]"))
			}
		}
		if !isProbeOrHelper {
			body = applyClaudeCloakThinkingDisplay(body, payloadTouchedDisplay)
		}
		return body
	}

	if isClaudeOpus55Model(currentModel) && !gjson.GetBytes(body, "fallbacks").Exists() && !payloadTouchedFallbacks {
		body, _ = sjson.SetRawBytes(body, "fallbacks", []byte(`[{"model":"claude-opus-4-8"}]`))
	}
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	thinkingActive := thinkingType == "adaptive" || thinkingType == "enabled"
	// Payload rules can disable thinking without touching display. Remove only
	// CPA's injected value; explicit caller and operator choices remain owned.
	if fableState.injectedDisplay && !payloadTouchedDisplay && (!thinkingActive || !claudeModelUsesProgressDisplay(currentModel)) {
		body, _ = sjson.DeleteBytes(body, "thinking.display")
	}
	if !isProbeOrHelper {
		body = applyClaudeCloakThinkingDisplay(body, payloadTouchedDisplay)
	}

	// Remove Reporting outcomes if CPA automatically injected it
	if fableState.injectedReporting {
		system := gjson.GetBytes(body, "system")
		if system.IsArray() {
			blocks := make([]string, 0, len(system.Array()))
			removed := false
			for _, blk := range system.Array() {
				if strings.ReplaceAll(blk.Get("text").String(), "​", "") == claudeCodeFableReportingOutcomes {
					removed = true
					continue
				}
				blocks = append(blocks, blk.Raw)
			}
			if removed {
				body, _ = sjson.SetRawBytes(body, "system", []byte("["+strings.Join(blocks, ",")+"]"))
			}
		} else if strings.ReplaceAll(system.String(), "​", "") == claudeCodeFableReportingOutcomes {
			body, _ = sjson.DeleteBytes(body, "system")
		}
	}
	return body
}

// claudeCacheControlTTL1h is the only non-default ttl native ever selects.
const claudeCacheControlTTL1h = "1h"

// claudeBodyNeedsBillingFallback reports whether a signed request still needs
// CPA's billing-header fallback: a request whose system field is absent matches
// the measured minimal native wire shape, so no billing header is injected
// (upstream 086ad91bd970).
func claudeBodyNeedsBillingFallback(body []byte) bool {
	return gjson.GetBytes(body, "system").Exists()
}

// prependClaudeBillingSystemBlock inserts a billing-header text block at
// system[0], preserving a caller string-system or an existing array
// (upstream 086ad91bd970).
func prependClaudeBillingSystemBlock(body []byte, billingText string) ([]byte, error) {
	billingBlock := []byte(buildTextBlock(billingText, nil))
	system := gjson.GetBytes(body, "system")
	var systemArray []byte
	switch {
	case system.Type == gjson.String:
		originalBlock := []byte(buildTextBlock(system.String(), nil))
		systemArray = make([]byte, 0, len(billingBlock)+len(originalBlock)+3)
		systemArray = append(systemArray, '[')
		systemArray = append(systemArray, billingBlock...)
		systemArray = append(systemArray, ',')
		systemArray = append(systemArray, originalBlock...)
		systemArray = append(systemArray, ']')
	case system.IsArray():
		rawSystem := bytes.TrimSpace([]byte(system.Raw))
		if bytes.Equal(rawSystem, []byte("[]")) {
			systemArray = make([]byte, 0, len(billingBlock)+2)
			systemArray = append(systemArray, '[')
			systemArray = append(systemArray, billingBlock...)
			systemArray = append(systemArray, ']')
		} else {
			systemArray = make([]byte, 0, len(billingBlock)+len(rawSystem)+1)
			systemArray = append(systemArray, '[')
			systemArray = append(systemArray, billingBlock...)
			systemArray = append(systemArray, ',')
			systemArray = append(systemArray, rawSystem[1:]...)
		}
	default:
		systemArray = make([]byte, 0, len(billingBlock)+2)
		systemArray = append(systemArray, '[')
		systemArray = append(systemArray, billingBlock...)
		systemArray = append(systemArray, ']')
	}

	updated, err := sjson.SetRawBytes(body, "system", systemArray)
	if err != nil {
		return nil, fmt.Errorf("prepend Claude CCH billing block: %w", err)
	}
	return updated, nil
}

// ensureCacheControl injects cache_control breakpoints into the payload for optimal prompt caching.
// According to Anthropic's documentation, cache prefixes are created in order: tools -> system -> messages.
// This function adds cache_control to:
// 1. The LAST tool in the tools array (caches all tool definitions)
// 2. The LAST system prompt element
// 3. The SECOND-TO-LAST user turn (caches conversation history for multi-turn)
//
// Up to 4 cache breakpoints are allowed per request. Tools, System, and Messages are INDEPENDENT breakpoints.
// This enables up to 90% cost reduction on cached tokens (cache read = 0.1x base price).
// See: https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
func ensureCacheControl(payload []byte) []byte {
	// 1. Inject cache_control into the LAST tool (caches all tool definitions)
	// Tools are cached first in the hierarchy, so this is the most important breakpoint.
	payload = injectToolsCacheControl(payload)

	// 2. Inject cache_control into the LAST system prompt element
	// System is the second level in the cache hierarchy.
	payload = injectSystemCacheControl(payload)

	// 3. Inject cache_control into messages for multi-turn conversation caching
	// This caches the conversation history up to the second-to-last user turn.
	payload = injectMessagesCacheControl(payload)

	return payload
}

func countCacheControls(payload []byte) int {
	count := 0

	// Check system
	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	// Check tools
	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	// Check messages
	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					if item.Get("cache_control").Exists() {
						count++
					}
					return true
				})
			}
			return true
		})
	}

	return count
}

// normalizeCacheControlTTL ensures cache_control TTL values don't violate the
// prompt-caching-scope-2026-01-05 ordering constraint: a 1h-TTL block must not
// appear after a 5m-TTL block anywhere in the evaluation order.
//
// Anthropic evaluates blocks in order: tools → system (index 0..N) → messages.
// Within each section, blocks are evaluated in array order. A 5m (default) block
// followed by a 1h block at ANY later position is an error — including within
// the same section (e.g. system[1]=5m then system[3]=1h).
//
// Strategy: walk all cache_control blocks in evaluation order. Once a 5m block
// is seen, strip ttl from ALL subsequent 1h blocks (downgrading them to 5m).
func normalizeCacheControlTTL(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	original := payload
	seen5m := false
	modified := false

	processBlock := func(path string, obj gjson.Result) {
		cc := obj.Get("cache_control")
		if !cc.Exists() {
			return
		}
		if !cc.IsObject() {
			seen5m = true
			return
		}
		ttl := cc.Get("ttl")
		if ttl.Type != gjson.String || ttl.String() != "1h" {
			seen5m = true
			return
		}
		if !seen5m {
			return
		}
		ttlPath := path + ".cache_control.ttl"
		updated, errDel := sjson.DeleteBytes(payload, ttlPath)
		if errDel != nil {
			return
		}
		payload = updated
		modified = true
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(idx, item gjson.Result) bool {
			processBlock(fmt.Sprintf("tools.%d", int(idx.Int())), item)
			return true
		})
	}

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(idx, item gjson.Result) bool {
			processBlock(fmt.Sprintf("system.%d", int(idx.Int())), item)
			return true
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(msgIdx, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item gjson.Result) bool {
				processBlock(fmt.Sprintf("messages.%d.content.%d", int(msgIdx.Int()), int(itemIdx.Int())), item)
				return true
			})
			return true
		})
	}

	if !modified {
		return original
	}
	return payload
}

// enforceCacheControlLimit removes excess cache_control blocks from a payload
// so the total does not exceed the Anthropic API limit (currently 4).
//
// Anthropic evaluates cache breakpoints in order: tools → system → messages.
// The most valuable breakpoints are:
//  1. Last tool         — caches ALL tool definitions
//  2. Last system block — caches ALL system content
//  3. Recent messages   — cache conversation context
//
// Removal priority (strip lowest-value first):
//
//	Phase 1: system blocks earliest-first, preserving the last one.
//	Phase 2: tool blocks earliest-first, preserving the last one.
//	Phase 3: message content blocks earliest-first.
//	Phase 4: remaining system blocks (last system).
//	Phase 5: remaining tool blocks (last tool).
func enforceCacheControlLimit(payload []byte, maxBlocks int) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	total := countCacheControls(payload)
	if total <= maxBlocks {
		return payload
	}

	excess := total - maxBlocks

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		lastIdx := -1
		system.ForEach(func(idx, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				lastIdx = int(idx.Int())
			}
			return true
		})
		if lastIdx >= 0 {
			system.ForEach(func(idx, item gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				i := int(idx.Int())
				if i == lastIdx {
					return true
				}
				if !item.Get("cache_control").Exists() {
					return true
				}
				path := fmt.Sprintf("system.%d.cache_control", i)
				updated, errDel := sjson.DeleteBytes(payload, path)
				if errDel != nil {
					return true
				}
				payload = updated
				excess--
				return true
			})
		}
	}
	if excess <= 0 {
		return payload
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		lastIdx := -1
		tools.ForEach(func(idx, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				lastIdx = int(idx.Int())
			}
			return true
		})
		if lastIdx >= 0 {
			tools.ForEach(func(idx, item gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				i := int(idx.Int())
				if i == lastIdx {
					return true
				}
				if !item.Get("cache_control").Exists() {
					return true
				}
				path := fmt.Sprintf("tools.%d.cache_control", i)
				updated, errDel := sjson.DeleteBytes(payload, path)
				if errDel != nil {
					return true
				}
				payload = updated
				excess--
				return true
			})
		}
	}
	if excess <= 0 {
		return payload
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(msgIdx, msg gjson.Result) bool {
			if excess <= 0 {
				return false
			}
			content := msg.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				if !item.Get("cache_control").Exists() {
					return true
				}
				path := fmt.Sprintf("messages.%d.content.%d.cache_control", int(msgIdx.Int()), int(itemIdx.Int()))
				updated, errDel := sjson.DeleteBytes(payload, path)
				if errDel != nil {
					return true
				}
				payload = updated
				excess--
				return true
			})
			return true
		})
	}
	if excess <= 0 {
		return payload
	}

	system = gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(idx, item gjson.Result) bool {
			if excess <= 0 {
				return false
			}
			if !item.Get("cache_control").Exists() {
				return true
			}
			path := fmt.Sprintf("system.%d.cache_control", int(idx.Int()))
			updated, errDel := sjson.DeleteBytes(payload, path)
			if errDel != nil {
				return true
			}
			payload = updated
			excess--
			return true
		})
	}
	if excess <= 0 {
		return payload
	}

	tools = gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(idx, item gjson.Result) bool {
			if excess <= 0 {
				return false
			}
			if !item.Get("cache_control").Exists() {
				return true
			}
			path := fmt.Sprintf("tools.%d.cache_control", int(idx.Int()))
			updated, errDel := sjson.DeleteBytes(payload, path)
			if errDel != nil {
				return true
			}
			payload = updated
			excess--
			return true
		})
	}

	return payload
}

// injectMessagesCacheControl adds cache_control to the second-to-last user turn for multi-turn caching.
// Per Anthropic docs: "Place cache_control on the second-to-last User message to let the model reuse the earlier cache."
// This enables caching of conversation history, which is especially beneficial for long multi-turn conversations.
// Only adds cache_control if:
// - There are at least 2 user turns in the conversation
// - No message content already has cache_control
func injectMessagesCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	// Check if ANY message content already has cache_control
	hasCacheControlInMessages := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					hasCacheControlInMessages = true
					return false
				}
				return true
			})
		}
		return !hasCacheControlInMessages
	})
	if hasCacheControlInMessages {
		return payload
	}

	// Find all user message indices
	var userMsgIndices []int
	messages.ForEach(func(index gjson.Result, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			userMsgIndices = append(userMsgIndices, int(index.Int()))
		}
		return true
	})

	// Need at least 2 user turns to cache the second-to-last
	if len(userMsgIndices) < 2 {
		return payload
	}

	// Get the second-to-last user message index
	secondToLastUserIdx := userMsgIndices[len(userMsgIndices)-2]

	// Get the content of this message
	contentPath := fmt.Sprintf("messages.%d.content", secondToLastUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		// Add cache_control to the last eligible content block of this message.
		// Anthropic rejects cache_control on an empty text block, so an empty
		// trailing block moves the breakpoint back instead of hosting it.
		targetBlockIndex := lastCacheableClaudeMessageContentBlockIndex(messages.Get(fmt.Sprintf("%d", secondToLastUserIdx)))
		if targetBlockIndex >= 0 {
			cacheControlPath := fmt.Sprintf("messages.%d.content.%d.cache_control", secondToLastUserIdx, targetBlockIndex)
			result, err := sjson.SetBytes(payload, cacheControlPath, map[string]string{"type": "ephemeral"})
			if err != nil {
				log.Warnf("failed to inject cache_control into messages: %v", err)
				return payload
			}
			payload = result
		}
	} else if content.Type == gjson.String && strings.TrimSpace(content.String()) != "" {
		// Convert string content to array with cache_control
		text := content.String()
		newContent := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, contentPath, newContent)
		if err != nil {
			log.Warnf("failed to inject cache_control into message string content: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

// injectToolsCacheControl adds cache_control to the last non-deferred tool in the tools array.
// Deferred tools cannot use prompt caching, so trailing deferred tools are skipped.
// This only adds cache_control if NO tool in the array already has it.
func injectToolsCacheControl(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}

	// Check if ANY tool already has cache_control and find the last eligible tool.
	hasCacheControlInTools := false
	lastEligibleToolIndex := -1
	tools.ForEach(func(index, tool gjson.Result) bool {
		if tool.Get("cache_control").Exists() {
			hasCacheControlInTools = true
			return false
		}
		if !tool.Get("defer_loading").Bool() {
			lastEligibleToolIndex = int(index.Int())
		}
		return true
	})
	if hasCacheControlInTools || lastEligibleToolIndex < 0 {
		return payload
	}

	lastToolPath := fmt.Sprintf("tools.%d.cache_control", lastEligibleToolIndex)
	result, err := sjson.SetBytes(payload, lastToolPath, map[string]string{"type": "ephemeral"})
	if err != nil {
		log.Warnf("failed to inject cache_control into tools array: %v", err)
		return payload
	}

	return result
}

// isEmptyClaudeTextBlock reports whether a content block is a text block with no
// printable text: Anthropic rejects cache_control on one.
func isEmptyClaudeTextBlock(block gjson.Result) bool {
	return block.IsObject() &&
		block.Get("type").String() == "text" &&
		strings.TrimSpace(block.Get("text").String()) == ""
}

// claudeContentBlockCanHostCacheControl reports whether a message content block
// may carry a cache_control breakpoint.
func claudeContentBlockCanHostCacheControl(block gjson.Result) bool {
	if !block.IsObject() {
		return false
	}
	switch block.Get("type").String() {
	case "", "thinking", "redacted_thinking":
		return false
	case "text":
		return strings.TrimSpace(block.Get("text").String()) != ""
	default:
		return true
	}
}

// lastCacheableClaudeMessageContentBlockIndex returns the index of the last
// content block a message-level cache_control may stamp, or -1 when none can
// host one. An assistant turn whose last visible block is thinking-like hosts
// no breakpoint, and empty text blocks are skipped either way.
func lastCacheableClaudeMessageContentBlockIndex(message gjson.Result) int {
	content := message.Get("content")
	if !content.IsArray() {
		return -1
	}
	blocks := content.Array()
	if len(blocks) == 0 {
		return -1
	}
	isAssistant := message.Get("role").String() == "assistant"
	for i := len(blocks) - 1; i >= 0; i-- {
		block := blocks[i]
		if isEmptyClaudeTextBlock(block) {
			continue
		}
		if isAssistant {
			switch block.Get("type").String() {
			case "thinking", "redacted_thinking":
				return -1
			}
		}
		if claudeContentBlockCanHostCacheControl(block) {
			return i
		}
	}
	return -1
}

// claudeSystemBlockCanHostCacheControl reports whether a system block has text a
// cache_control breakpoint can cover.
func claudeSystemBlockCanHostCacheControl(item gjson.Result) bool {
	if !item.IsObject() {
		return false
	}
	if typ := item.Get("type").String(); typ != "" && typ != "text" {
		return false
	}
	return strings.TrimSpace(item.Get("text").String()) != ""
}

// injectSystemCacheControl adds cache_control to the last element in the system prompt.
// Converts string system prompts to array format if needed.
// This only adds cache_control if NO system element already has it.
func injectSystemCacheControl(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		count := int(system.Get("#").Int())
		if count == 0 {
			return payload
		}

		// Check if ANY system element already has cache_control and find the last
		// element that can host one.
		hasCacheControlInSystem := false
		lastEligibleSystemIndex := -1
		system.ForEach(func(index, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				hasCacheControlInSystem = true
				return false
			}
			if claudeSystemBlockCanHostCacheControl(item) {
				lastEligibleSystemIndex = int(index.Int())
			}
			return true
		})
		if hasCacheControlInSystem || lastEligibleSystemIndex < 0 {
			return payload
		}

		// Add cache_control to the last cacheable system element
		lastSystemPath := fmt.Sprintf("system.%d.cache_control", lastEligibleSystemIndex)
		result, err := sjson.SetBytes(payload, lastSystemPath, map[string]string{"type": "ephemeral"})
		if err != nil {
			log.Warnf("failed to inject cache_control into system array: %v", err)
			return payload
		}
		payload = result
	} else if system.Type == gjson.String && strings.TrimSpace(system.String()) != "" {
		// Convert string system prompt to array with cache_control
		// "system": "text" -> "system": [{"type": "text", "text": "text", "cache_control": {"type": "ephemeral"}}]
		text := system.String()
		newSystem := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, "system", newSystem)
		if err != nil {
			log.Warnf("failed to inject cache_control into system string: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

func ensureModelMaxTokens(body []byte, modelID string) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	if maxTokens := gjson.GetBytes(body, "max_tokens"); maxTokens.Exists() {
		return body
	}

	for _, provider := range registry.GetGlobalRegistry().GetModelProviders(strings.TrimSpace(modelID)) {
		if strings.EqualFold(provider, "claude") {
			maxTokens := defaultModelMaxTokens
			if info := registry.GetGlobalRegistry().GetModelInfo(strings.TrimSpace(modelID), "claude"); info != nil && info.MaxCompletionTokens > 0 {
				maxTokens = info.MaxCompletionTokens
			}
			body, _ = sjson.SetBytes(body, "max_tokens", maxTokens)
			return body
		}
	}

	return body
}
