// Package executor provides runtime execution capabilities for various AI service providers.
// It includes stateless executors that handle API requests, streaming responses,
// token counting, and authentication refresh for different AI service providers.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	internalsignature "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/therealtinhtute/llmhub/internal/util"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// glEndpoint is the base URL for the Google Generative Language API.
	glEndpoint = "https://generativelanguage.googleapis.com"

	// glAPIVersion is the API version used for Gemini requests.
	glAPIVersion = "v1beta"

	// streamScannerBuffer is the buffer size for SSE stream scanning.
	streamScannerBuffer = 52_428_800
)

// GeminiExecutor is a stateless executor for the official Gemini API using API keys.
// It handles both API key and OAuth bearer token authentication, supporting both
// regular and streaming requests to the Google Generative Language API.
type GeminiExecutor struct {
	// cfg holds the application configuration.
	cfg *config.Config
}

// NewGeminiExecutor creates a new Gemini executor instance.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *GeminiExecutor: A new Gemini executor instance
func NewGeminiExecutor(cfg *config.Config) *GeminiExecutor {
	return &GeminiExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *GeminiExecutor) Identifier() string { return "gemini" }

// PrepareRequest injects Gemini credentials into the outgoing HTTP request.
func (e *GeminiExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, bearer := geminiCreds(auth)
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
		req.Header.Del("Authorization")
	} else if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Del("x-goog-api-key")
	} else {
		// base_URL-only credential: clear any stale auth headers so unrelated
		// state from a reused connection cannot leak upstream.
		req.Header.Del("x-goog-api-key")
		req.Header.Del("Authorization")
	}
	applyGeminiHeaders(req, auth)
	return nil
}

// HttpRequest injects Gemini credentials into the request and executes it.
func (e *GeminiExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("gemini executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming request to the Gemini API.
// It translates the request to Gemini format, sends it to the API, and translates
// the response back to the requested format.
//
// Parameters:
//   - ctx: The context for the request
//   - auth: The authentication information
//   - req: The request to execute
//   - opts: Additional execution options
//
// Returns:
//   - cliproxyexecutor.Response: The response from the API
//   - error: An error if the request fails
func (e *GeminiExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, bearer := geminiCreds(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	// Official Gemini API via API key or OAuth bearer
	from := opts.SourceFormat
	to := sdktranslator.FromString("gemini")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	body = fixGeminiImageAspectRatio(baseModel, body)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	// The usage record names the tier the upstream is actually asked to serve,
	// so capture it after the last payload rule has run.
	reporter.SetEffectiveServiceTier(body)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body = capGeminiMaxOutputTokens(body, baseModel)
	body = internalsignature.SanitizeGeminiRequestThoughtSignatures(body, "contents")

	action := "generateContent"
	if req.Metadata != nil {
		if a, _ := req.Metadata["action"].(string); a == "countTokens" {
			action = "countTokens"
		}
	}
	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, action)
	if opts.Alt != "" && action != "countTokens" {
		url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
	}

	body, _ = sjson.DeleteBytes(body, "session_id")
	body = helps.EnsureGeminiLeadingUserContent(body, "contents")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	} else if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	applyGeminiHeaders(httpReq, auth, opts.Headers)
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

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	reporter.ObserveResponseModel(data)
	if blockMsg, blocked := geminiPromptBlockErrorMessage(data); blocked {
		err = statusErr{code: http.StatusBadGateway, msg: blockMsg}
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		// Publish as a failure before the success path publishes: the reporter
		// emits exactly once, so the blocked prompt's tokens would otherwise be
		// recorded as a successful completion.
		reporter.PublishFailureWithDetail(ctx, helps.ParseGeminiUsage(data), err)
		return cliproxyexecutor.Response{}, err
	}
	reporter.Publish(ctx, helps.ParseGeminiUsage(data))
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, data, &param)
	if from == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming request to the Gemini API.
func (e *GeminiExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, bearer := geminiCreds(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("gemini")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	body = fixGeminiImageAspectRatio(baseModel, body)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	// The usage record names the tier the upstream is actually asked to serve,
	// so capture it after the last payload rule has run.
	reporter.SetEffectiveServiceTier(body)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body = capGeminiMaxOutputTokens(body, baseModel)
	body = internalsignature.SanitizeGeminiRequestThoughtSignatures(body, "contents")

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, "streamGenerateContent")
	if opts.Alt == "" {
		url = url + "?alt=sse"
	} else {
		url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
	}

	body, _ = sjson.DeleteBytes(body, "session_id")
	body = helps.EnsureGeminiLeadingUserContent(body, "contents")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	applyGeminiHeaders(httpReq, auth, opts.Headers)
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

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("gemini executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, from, originalPayload)
		var param any
		var sawFinishReason bool
		var terminalDelivered bool
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			reporter.ObserveResponseModel(line)
			rawPayload := helps.JSONPayload(line)
			if !sawFinishReason && geminiStreamHasFinishSignal(rawPayload) {
				sawFinishReason = true
			}
			// A candidate-less prompt block is a failed request, not an empty
			// completion: surface it before any usage is published as a success.
			if blockMsg, blocked := geminiPromptBlockErrorMessage(rawPayload); blocked {
				blockErr := statusErr{code: http.StatusBadGateway, msg: blockMsg}
				helps.RecordAPIResponseError(ctx, e.cfg, blockErr)
				reporter.PublishFailureWithDetail(ctx, helps.ParseGeminiUsage(rawPayload), blockErr)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: blockErr}:
				case <-ctx.Done():
				}
				return
			}
			filtered := helps.FilterSSEUsageMetadata(line)
			payload := helps.JSONPayload(filtered)
			if len(payload) == 0 {
				continue
			}
			if detail, ok := helps.ParseGeminiStreamUsage(payload); ok {
				reporter.Publish(ctx, detail)
			}
			lines := helps.TranslateStreamWithClaudeInputTokens(ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(payload), &param, claudeInputTokens)
			for i := range lines {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: lines[i]}:
					if helps.IsGeminiTerminalStreamChunk(lines[i]) {
						terminalDelivered = true
					}
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			// A cancellation after a delivered terminal frame is a completed
			// stream: the client already has everything, so the observed usage
			// must survive as a success rather than be replaced by a failure.
			if terminalDelivered && errors.Is(errScan, context.Canceled) {
				reporter.EnsurePublished(ctx)
				return
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if (from == sdktranslator.FormatClaude || from == sdktranslator.FormatOpenAI) && !sawFinishReason {
			truncErr := statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before finishReason"}
			helps.RecordAPIResponseError(ctx, e.cfg, truncErr)
			reporter.PublishFailure(ctx, truncErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: truncErr}:
			case <-ctx.Done():
			}
			return
		}
		lines := helps.TranslateStreamWithClaudeInputTokens(ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param, claudeInputTokens)
		for i := range lines {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: lines[i]}:
				if helps.IsGeminiTerminalStreamChunk(lines[i]) {
					terminalDelivered = true
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// geminiPromptBlockErrorMessage reports an upstream prompt block: a payload with
// no candidate at all plus a promptFeedback.blockReason (v8.0.41 3f5d3a16). The
// message keeps the upstream reason and its detail so the client can tell the
// block apart from a truncation. A response with any candidate is never a block,
// even when it also carries promptFeedback.
func geminiPromptBlockErrorMessage(payload []byte) (string, bool) {
	if len(payload) == 0 {
		return "", false
	}
	for _, candidatesPath := range []string{"candidates", "response.candidates"} {
		if candidates := gjson.GetBytes(payload, candidatesPath); candidates.IsArray() && len(candidates.Array()) > 0 {
			return "", false
		}
	}
	for _, prefix := range []string{"promptFeedback", "response.promptFeedback"} {
		reason := strings.TrimSpace(gjson.GetBytes(payload, prefix+".blockReason").String())
		if reason == "" {
			continue
		}
		if detail := strings.TrimSpace(gjson.GetBytes(payload, prefix+".blockReasonMessage").String()); detail != "" {
			return fmt.Sprintf("upstream blocked the prompt: %s - %s", reason, detail), true
		}
		return fmt.Sprintf("upstream blocked the prompt: %s", reason), true
	}
	return "", false
}

func geminiStreamHasFinishSignal(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	for _, path := range []string{
		"candidates.#.finishReason",
		"response.candidates.#.finishReason",
		"promptFeedback.blockReason",
		"response.promptFeedback.blockReason",
	} {
		res := gjson.GetBytes(payload, path)
		if res.IsArray() {
			for _, item := range res.Array() {
				if strings.TrimSpace(item.String()) != "" {
					return true
				}
			}
		} else if res.Exists() && strings.TrimSpace(res.String()) != "" {
			return true
		}
	}
	return false
}


// CountTokens counts tokens for the given request using the Gemini API.
func (e *GeminiExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, bearer := geminiCreds(auth)

	from := opts.SourceFormat
	to := sdktranslator.FromString("gemini")
	translatedReq := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	translatedReq, err := thinking.ApplyThinking(translatedReq, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translatedReq = fixGeminiImageAspectRatio(baseModel, translatedReq)
	respCtx := context.WithValue(ctx, "alt", opts.Alt)
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "tools")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "generationConfig")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "safetySettings")
	translatedReq, _ = sjson.SetBytes(translatedReq, "model", baseModel)
	translatedReq = internalsignature.SanitizeGeminiRequestThoughtSignatures(translatedReq, "contents")
	translatedReq = helps.EnsureGeminiLeadingUserContent(translatedReq, "contents")

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, "countTokens")

	requestBody := bytes.NewReader(translatedReq)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, requestBody)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	applyGeminiHeaders(httpReq, auth, opts.Headers)
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
		Body:      translatedReq,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			helps.LogWithRequestID(ctx).Errorf("response body close error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", resp.StatusCode, helps.SummarizeErrorBody(resp.Header.Get("Content-Type"), data))
		return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: string(data)}
	}

	count := gjson.GetBytes(data, "totalTokens").Int()
	translated := sdktranslator.TranslateTokenCount(respCtx, to, from, count, data)
	return cliproxyexecutor.Response{Payload: translated, Headers: resp.Header.Clone()}, nil
}

// Refresh refreshes the authentication credentials (no-op for Gemini API key).
func (e *GeminiExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

func geminiCreds(a *cliproxyauth.Auth) (apiKey, bearer string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			apiKey = v
		}
	}
	if a.Metadata != nil {
		// GeminiTokenStorage.Token is a map that may contain access_token
		if v, ok := a.Metadata["access_token"].(string); ok && v != "" {
			bearer = v
		}
		if token, ok := a.Metadata["token"].(map[string]any); ok && token != nil {
			if v, ok2 := token["access_token"].(string); ok2 && v != "" {
				bearer = v
			}
		}
	}
	return
}

func resolveGeminiBaseURL(auth *cliproxyauth.Auth) string {
	base := glEndpoint
	if auth != nil && auth.Attributes != nil {
		if custom := strings.TrimSpace(auth.Attributes["base_url"]); custom != "" {
			base = strings.TrimRight(custom, "/")
		}
	}
	if base == "" {
		return glEndpoint
	}
	return base
}

func (e *GeminiExecutor) resolveGeminiConfig(auth *cliproxyauth.Auth) *config.GeminiKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range e.cfg.GeminiKey {
		entry := &e.cfg.GeminiKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range e.cfg.GeminiKey {
			entry := &e.cfg.GeminiKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func applyGeminiHeaders(req *http.Request, auth *cliproxyauth.Auth, clientHeaders ...http.Header) {
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs, clientHeaders...)
}

func capGeminiMaxOutputTokens(body []byte, modelName string) []byte {
	maxOut := gjson.GetBytes(body, "generationConfig.maxOutputTokens")
	if !maxOut.Exists() || maxOut.Type != gjson.Number {
		return body
	}
	modelInfo := registry.LookupModelInfo(modelName, "gemini")
	if modelInfo == nil {
		return body
	}
	limit := modelInfo.OutputTokenLimit
	if limit <= 0 {
		limit = modelInfo.MaxCompletionTokens
	}
	if limit <= 0 || maxOut.Int() <= int64(limit) {
		return body
	}
	body, _ = sjson.SetBytes(body, "generationConfig.maxOutputTokens", limit)
	return body
}

func fixGeminiImageAspectRatio(modelName string, rawJSON []byte) []byte {
	if modelName == "gemini-2.5-flash-image-preview" {
		aspectRatioResult := gjson.GetBytes(rawJSON, "generationConfig.imageConfig.aspectRatio")
		if aspectRatioResult.Exists() {
			contents := gjson.GetBytes(rawJSON, "contents")
			contentArray := contents.Array()
			if len(contentArray) > 0 {
				hasInlineData := false
			loopContent:
				for i := 0; i < len(contentArray); i++ {
					parts := contentArray[i].Get("parts").Array()
					for j := 0; j < len(parts); j++ {
						if parts[j].Get("inlineData").Exists() {
							hasInlineData = true
							break loopContent
						}
					}
				}

				if !hasInlineData {
					emptyImageBase64ed, _ := util.CreateWhiteImageBase64(aspectRatioResult.String())
					emptyImagePart := []byte(`{"inlineData":{"mime_type":"image/png","data":""}}`)
					emptyImagePart, _ = sjson.SetBytes(emptyImagePart, "inlineData.data", emptyImageBase64ed)
					newPartsJson := []byte(`[]`)
					newPartsJson, _ = sjson.SetRawBytes(newPartsJson, "-1", []byte(`{"text": "Based on the following requirements, create an image within the uploaded picture. The new content *MUST* completely cover the entire area of the original picture, maintaining its exact proportions, and *NO* blank areas should appear."}`))
					newPartsJson, _ = sjson.SetRawBytes(newPartsJson, "-1", emptyImagePart)

					parts := contentArray[0].Get("parts").Array()
					for j := 0; j < len(parts); j++ {
						newPartsJson, _ = sjson.SetRawBytes(newPartsJson, "-1", []byte(parts[j].Raw))
					}

					rawJSON, _ = sjson.SetRawBytes(rawJSON, "contents.0.parts", newPartsJson)
					rawJSON, _ = sjson.SetRawBytes(rawJSON, "generationConfig.responseModalities", []byte(`["IMAGE", "TEXT"]`))
				}
			}
			rawJSON, _ = sjson.DeleteBytes(rawJSON, "generationConfig.imageConfig")
		}
	}
	return rawJSON
}

// sanitizeGeminiInteractionsUnsupportedInputIDs aligns input step IDs with the
// official Gemini Interactions API schema:
// - `function_call` (FunctionCallStep) requires `id` and rejects `call_id`
// - `function_result` (FunctionResultStep) requires `call_id` and rejects `id`
// - other steps and content parts do not support `id`
//
// Ported from upstream f51d3ae93b87. Upstream wires this at the native
// Gemini Interactions request sites (gemini_executor.go Execute/ExecuteStream);
// llmhub has no Gemini Interactions executor path yet, so the helper lands
// unwired until that surface is ported.
func sanitizeGeminiInteractionsUnsupportedInputIDs(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	for i, item := range input.Array() {
		stepType := item.Get("type").String()
		if stepType == "function_call" {
			if !item.Get("id").Exists() && item.Get("call_id").Exists() {
				body, _ = sjson.SetBytes(body, fmt.Sprintf("input.%d.id", i), item.Get("call_id").String())
			}
			if item.Get("call_id").Exists() {
				body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.call_id", i))
			}
		} else {
			if item.Get("id").Exists() {
				body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.id", i))
			}
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for j, part := range content.Array() {
			if part.Get("id").Exists() {
				body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.content.%d.id", i, j))
			}
		}
	}
	return body
}
