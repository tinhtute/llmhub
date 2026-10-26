package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	codexauth "github.com/therealtinhtute/llmhub/internal/auth/codex"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/misc"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	openairesponses "github.com/therealtinhtute/llmhub/internal/translator/openai/openai/responses"
	"github.com/therealtinhtute/llmhub/internal/util"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	codexUserAgent             = "codex_cli_rs/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9"
	codexOriginator            = "codex_cli_rs"
	codexDefaultImageToolModel = "gpt-image-2"
)

var dataTag = []byte("data:")

func codexUserAgentForCloaking(disabled bool) string {
	if disabled {
		return ""
	}
	return codexUserAgent
}

// Streamed Codex responses may emit response.output_item.done events while leaving
// response.completed.response.output empty. Keep the stream path aligned with the
// already-patched non-stream path by reconstructing response.output from those items.
func collectCodexOutputItemDone(eventData []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback *[][]byte) {
	itemResult := gjson.GetBytes(eventData, "item")
	if !itemResult.Exists() || itemResult.Type != gjson.JSON {
		return
	}
	outputIndexResult := gjson.GetBytes(eventData, "output_index")
	if outputIndexResult.Exists() {
		outputItemsByIndex[outputIndexResult.Int()] = []byte(itemResult.Raw)
		return
	}
	*outputItemsFallback = append(*outputItemsFallback, []byte(itemResult.Raw))
}

func patchCodexCompletedOutput(eventData []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback [][]byte) []byte {
	outputResult := gjson.GetBytes(eventData, "response.output")
	shouldPatchOutput := (!outputResult.Exists() || !outputResult.IsArray() || len(outputResult.Array()) == 0) && (len(outputItemsByIndex) > 0 || len(outputItemsFallback) > 0)
	if !shouldPatchOutput {
		return eventData
	}

	indexes := make([]int64, 0, len(outputItemsByIndex))
	for idx := range outputItemsByIndex {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool {
		return indexes[i] < indexes[j]
	})

	items := make([][]byte, 0, len(outputItemsByIndex)+len(outputItemsFallback))
	for _, idx := range indexes {
		items = append(items, outputItemsByIndex[idx])
	}
	items = append(items, outputItemsFallback...)

	outputArray := []byte("[]")
	if len(items) > 0 {
		var buf bytes.Buffer
		totalLen := 2
		for _, item := range items {
			totalLen += len(item)
		}
		if len(items) > 1 {
			totalLen += len(items) - 1
		}
		buf.Grow(totalLen)
		buf.WriteByte('[')
		for i, item := range items {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(item)
		}
		buf.WriteByte(']')
		outputArray = buf.Bytes()
	}

	completedDataPatched, _ := sjson.SetRawBytes(eventData, "response.output", outputArray)
	return completedDataPatched
}

func codexTerminalStreamContextLengthErr(eventData []byte) (statusErr, bool) {
	streamErr, body, ok := codexTerminalStreamErr(eventData)
	if !ok || !codexTerminalErrorIsContextLength(body) {
		return statusErr{}, false
	}
	return streamErr, true
}

func codexTerminalErrorBody(eventData []byte, path string) []byte {
	errorResult := gjson.GetBytes(eventData, path)
	if !errorResult.Exists() {
		return nil
	}
	body := []byte(`{"error":{}}`)
	if errorResult.Type == gjson.JSON {
		body, _ = sjson.SetRawBytes(body, "error", []byte(errorResult.Raw))
	} else if message := strings.TrimSpace(errorResult.String()); message != "" {
		body, _ = sjson.SetBytes(body, "error.message", message)
	}
	if strings.TrimSpace(gjson.GetBytes(body, "error.message").String()) == "" {
		if message := strings.TrimSpace(gjson.GetBytes(eventData, "response.error.message").String()); message != "" {
			body, _ = sjson.SetBytes(body, "error.message", message)
		}
	}
	if strings.TrimSpace(gjson.GetBytes(body, "error.message").String()) == "" {
		if code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String()); code != "" {
			body, _ = sjson.SetBytes(body, "error.message", code)
		}
	}
	if strings.TrimSpace(gjson.GetBytes(body, "error.message").String()) == "" {
		if errorType := strings.TrimSpace(gjson.GetBytes(body, "error.type").String()); errorType != "" {
			body, _ = sjson.SetBytes(body, "error.message", errorType)
		}
	}
	return body
}

func codexTerminalTopLevelErrorBody(eventData []byte) []byte {
	message := strings.TrimSpace(gjson.GetBytes(eventData, "message").String())
	code := strings.TrimSpace(gjson.GetBytes(eventData, "code").String())
	errorType := strings.TrimSpace(gjson.GetBytes(eventData, "error_type").String())
	param := strings.TrimSpace(gjson.GetBytes(eventData, "param").String())
	if message == "" && code == "" && errorType == "" && param == "" {
		return nil
	}

	body := []byte(`{"error":{}}`)
	if message != "" {
		body, _ = sjson.SetBytes(body, "error.message", message)
	}
	if code != "" {
		body, _ = sjson.SetBytes(body, "error.code", code)
	}
	if errorType != "" {
		body, _ = sjson.SetBytes(body, "error.type", errorType)
	}
	if param != "" {
		body, _ = sjson.SetBytes(body, "error.param", param)
	}
	if strings.TrimSpace(gjson.GetBytes(body, "error.message").String()) == "" {
		if code != "" {
			body, _ = sjson.SetBytes(body, "error.message", code)
		} else if errorType != "" {
			body, _ = sjson.SetBytes(body, "error.message", errorType)
		}
	}
	return body
}

func codexTerminalErrorIsContextLength(body []byte) bool {
	errorCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()))
	return errorCode == "context_length_exceeded" ||
		errorCode == "context_too_large" ||
		strings.Contains(message, "context window") ||
		strings.Contains(message, "context length") ||
		strings.Contains(message, "too many tokens")
}

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg *config.Config
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func buildCodexResponsesDeclarationTable(from sdktranslator.Format, requestRawJSON []byte) (*openairesponses.ResponsesToolDeclarationTable, error) {
	if from != "openai-response" {
		return nil, nil
	}
	return openairesponses.BuildResponsesToolDeclarationTable(requestRawJSON)
}

func codexToolDeclarationStatusErr(err error) statusErr {
	body := []byte(`{"error":{}}`)
	body, _ = sjson.SetBytes(body, "error.message", err.Error())
	body, _ = sjson.SetBytes(body, "error.type", "invalid_request_error")
	body, _ = sjson.SetBytes(body, "error.code", "tool_name_collision")
	return statusErr{code: http.StatusBadRequest, msg: string(body)}
}

func (e *CodexExecutor) Identifier() string { return "codex" }

// modelLevelCooling reports whether Codex usage_limit_reached quota cooldowns
// are scoped to the requested model only, rather than the whole credential.
func (e *CodexExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.CodexModelLevelCooling
}

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Del("Authorization")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
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

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.SourceFormat.String() == "codex-alpha-search" {
		return e.executeAlphaSearch(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImage(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	isCompat := e.resolveCodexModelIsCompat(auth, req, baseModel)
	declarationTable, declarationErr := buildCodexResponsesDeclarationTable(from, originalPayload)
	if declarationErr != nil {
		return resp, codexToolDeclarationStatusErr(declarationErr)
	}
	originalTranslated, body, updatesChanged := translateCodexRequestPairWithUpdateIntent(from, to, baseModel, originalPayload, req.Payload, false, isCompat)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body = ensureWebSearchToolForHistory(body, opts.Headers)
	body = normalizeCodexInstructions(body, helps.IsNativeCodexRequest(req.Payload, opts))
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth, opts.Headers)
	}
	// Ported from upstream CLIProxyAPI commit 81d6ba774621: compat models keep
	// reasoning.content and reasoning ids; others get the standard sanitize.
	body = sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(ctx, "codex executor", body, isCompat, resolveResponsesSignatureTargetModel(baseModel, req, opts), baseURL)
	body = normalizeCodexParallelToolCalls(body, opts.Headers)
	body = helps.NormalizeCodexToolSchemas(body)
	// Optimize official Codex multi_agent v2 requests: refresh spawn_agent model
	// details, strip message encryption, and (for is-compat models) convert
	// agent_message input into portable message/user items.
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2RequestForAuth(ctx, opts.Headers, body, e.cfg, auth, isCompat)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg, opts.Headers)
	applyCodexRoutingHint(ctx, httpReq.Header, auth, baseModel, body, opts.Headers)
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
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newCodexStatusErrWithCooling(httpResp.StatusCode, b, e.modelLevelCooling())
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)

	lines := bytes.Split(data, []byte("\n"))
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	for _, line := range lines {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}

		eventData := bytes.TrimSpace(line[5:])
		eventData = helps.RestoreCodexMultiAgentV2Response(eventData, optimizeMultiAgentV2)
		reporter.ObserveCodexResponseModel(eventData)
		eventType := gjson.GetBytes(eventData, "type").String()

		if streamErr, ok := codexTerminalStreamContextLengthErr(eventData); ok {
			err = streamErr
			return resp, err
		}

		if eventType == "response.output_item.done" {
			itemResult := gjson.GetBytes(eventData, "item")
			if !itemResult.Exists() || itemResult.Type != gjson.JSON {
				continue
			}
			outputIndexResult := gjson.GetBytes(eventData, "output_index")
			if outputIndexResult.Exists() {
				outputItemsByIndex[outputIndexResult.Int()] = []byte(itemResult.Raw)
			} else {
				outputItemsFallback = append(outputItemsFallback, []byte(itemResult.Raw))
			}
			continue
		}

		if eventType != "response.completed" {
			continue
		}

		if detail, ok := helps.ParseCodexUsage(eventData); ok {
			reporter.Publish(ctx, detail)
		}
		publishCodexImageToolUsage(ctx, reporter, body, eventData)

		completedData := eventData
		outputResult := gjson.GetBytes(completedData, "response.output")
		shouldPatchOutput := (!outputResult.Exists() || !outputResult.IsArray() || len(outputResult.Array()) == 0) && (len(outputItemsByIndex) > 0 || len(outputItemsFallback) > 0)
		if shouldPatchOutput {
			completedDataPatched := completedData
			completedDataPatched, _ = sjson.SetRawBytes(completedDataPatched, "response.output", []byte(`[]`))

			indexes := make([]int64, 0, len(outputItemsByIndex))
			for idx := range outputItemsByIndex {
				indexes = append(indexes, idx)
			}
			sort.Slice(indexes, func(i, j int) bool {
				return indexes[i] < indexes[j]
			})
			for _, idx := range indexes {
				completedDataPatched, _ = sjson.SetRawBytes(completedDataPatched, "response.output.-1", outputItemsByIndex[idx])
			}
			for _, item := range outputItemsFallback {
				completedDataPatched, _ = sjson.SetRawBytes(completedDataPatched, "response.output.-1", item)
			}
			completedData = completedDataPatched
		}

		if declarationTable != nil {
			completedData = declarationTable.RestoreResponsesToolCalls(completedData)
		}
		var param any
		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, completedData, &param)
		if from == sdktranslator.FormatOpenAIResponse {
			out = helps.EnsureResponsesUsageDetails(out)
		}
		resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
		return resp, nil
	}
	err = statusErr{code: 408, msg: "stream error: stream disconnected before completion: stream closed before response.completed"}
	return resp, err
}

func (e *CodexExecutor) executeAlphaSearch(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if strings.TrimSpace(baseModel) == "" {
		baseModel = strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	body := append([]byte(nil), req.Payload...)
	if baseModel != "" {
		body, _ = sjson.SetBytes(body, "model", baseModel)
	}
	body, _ = sjson.DeleteBytes(body, "prompt_cache_key")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")

	url := strings.TrimSuffix(baseURL, "/") + "/alpha/search"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, false, e.cfg, opts.Headers)
	applyCodexRoutingHint(ctx, httpReq.Header, auth, baseModel, body, opts.Headers)

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
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErrWithCooling(httpResp.StatusCode, data, e.modelLevelCooling())
		return resp, err
	}
	return cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	isCompat := e.resolveCodexModelIsCompat(auth, req, baseModel)
	declarationTable, declarationErr := buildCodexResponsesDeclarationTable(from, originalPayload)
	if declarationErr != nil {
		return resp, codexToolDeclarationStatusErr(declarationErr)
	}
	originalTranslated, body, updatesChanged := translateCodexRequestPairWithUpdateIntent(from, to, baseModel, originalPayload, req.Payload, false, isCompat)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "stream")
	body = ensureWebSearchToolForHistory(body, opts.Headers)
	body = normalizeCodexInstructions(body, helps.IsNativeCodexRequest(req.Payload, opts))
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth, opts.Headers)
	}
	// Ported from upstream CLIProxyAPI commit 81d6ba774621 (compact flow).
	body = sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(ctx, "codex executor", body, isCompat, resolveResponsesSignatureTargetModel(baseModel, req, opts), baseURL)
	body = normalizeCodexParallelToolCalls(body, opts.Headers)
	body = helps.NormalizeCodexToolSchemas(body)
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2RequestForAuth(ctx, opts.Headers, body, e.cfg, auth, isCompat)

	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, false, e.cfg, opts.Headers)
	applyCodexRoutingHint(ctx, httpReq.Header, auth, baseModel, body, opts.Headers)
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
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newCodexStatusErrWithCooling(httpResp.StatusCode, b, e.modelLevelCooling())
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	data = helps.RestoreCodexMultiAgentV2Response(data, optimizeMultiAgentV2)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	reporter.EnsurePublished(ctx)
	if declarationTable != nil {
		data = declarationTable.RestoreResponsesToolCalls(data)
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, data, &param)
	if from == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImageStream(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	preserveNativeOutput := helps.IsNativeCodexRequest(req.Payload, opts)
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	isCompat := e.resolveCodexModelIsCompat(auth, req, baseModel)
	declarationTable, declarationErr := buildCodexResponsesDeclarationTable(from, originalPayload)
	if declarationErr != nil {
		return nil, codexToolDeclarationStatusErr(declarationErr)
	}
	originalTranslated, body, updatesChanged := translateCodexRequestPairWithUpdateIntent(from, to, baseModel, originalPayload, req.Payload, true, isCompat)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	reasoningSummaryDelivery := gjson.GetBytes(body, "stream_options.reasoning_summary_delivery")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	if reasoningSummaryDelivery.Exists() {
		body, _ = sjson.SetBytes(body, "stream_options.reasoning_summary_delivery", reasoningSummaryDelivery.Value())
	}
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body = ensureWebSearchToolForHistory(body, opts.Headers)
	body = normalizeCodexInstructions(body, preserveNativeOutput)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth, opts.Headers)
	}
	// Ported from upstream CLIProxyAPI commit 81d6ba774621 (streaming flow).
	body = sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(ctx, "codex executor", body, isCompat, resolveResponsesSignatureTargetModel(baseModel, req, opts), baseURL)
	body = normalizeCodexParallelToolCalls(body, opts.Headers)
	body = helps.NormalizeCodexToolSchemas(body)
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2RequestForAuth(ctx, opts.Headers, body, e.cfg, auth, isCompat)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg, opts.Headers)
	applyCodexRoutingHint(ctx, httpReq.Header, auth, baseModel, body, opts.Headers)
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
		data, readErr := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErrWithCooling(httpResp.StatusCode, data, e.modelLevelCooling())
		return nil, err
	}
	buffering := e.cfg != nil && e.cfg.CodexStreamBootstrapBuffering
	var bootstrapTimeout time.Duration
	var bootstrapStart time.Time
	if buffering {
		bootstrapTimeout = e.cfg.CodexStreamBootstrapTimeoutDuration()
		bootstrapStart = nowCodexBootstrap()
	}

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800) // 50MB
	claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
	var param any
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte

	var bufferedChunks [][]byte
	// bufferedFrames counts the scanned lines this loop holds and bufferedBytes sums each line
	// together with the chunks it translates into. Every iteration either holds the line or leaves
	// the loop, so this is one unit per line read. In addition to the frame and byte budgets,
	// bootstrapTimeout bounds how long trickled frames may hold the downstream headers
	// (upstream 6e307553f43f).
	bufferedFrames := 0
	bufferedBytes := 0
	var initialChunks [][]byte
	streamStarted := false
	immediateTerminal := false
	// bootstrapTerminalErr holds a non-overload terminal failure seen while buffering. It is
	// delivered as an in-stream chunk after the buffered handshake so downstream behaviour stays
	// identical to the unbuffered path instead of silently turning into a credential failover.
	var bootstrapTerminalErr error
	sawOutputDelta := false

	closeBootstrapBody := func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}

	if buffering {
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			translatedLines := [][]byte{bytes.Clone(line)}
			isHandshake := true
			terminalSuccess := false
			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				data = helps.RestoreCodexMultiAgentV2Response(data, optimizeMultiAgentV2)
				observeCodexTokenEvent(reporter, data)
				eventType := gjson.GetBytes(data, "type").String()
				if streamErr, terminalBody, ok := codexTerminalFailureErrWithCooling(data, e.modelLevelCooling()); ok {
					closeBootstrapBody()
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					if isCodexOverloadBootstrapFailure(terminalBody) {
						timeSinceStart := nowCodexBootstrap().Sub(bootstrapStart)
						timeoutReached := bootstrapTimeout > 0 && timeSinceStart >= bootstrapTimeout
						if !timeoutReached {
							// Transient capacity rejection smuggled into an HTTP 200 stream. Fail the
							// attempt before downstream headers commit so the conductor can retry on
							// another credential, reporting the status upstream refused to put on the wire.
							helps.LogWithRequestID(ctx).Debugf("codex executor: bootstrap overload rejection after %d buffered lines, failing over", bufferedFrames)
							return nil, newCodexBootstrapOverloadErr(terminalBody)
						}
						helps.LogWithRequestID(ctx).Debugf("codex executor: bootstrap overload rejection after %d lines / %v, time budget exhausted; delivering in-stream", bufferedFrames, timeSinceStart)
					}
					// Non-overload terminal failure (or post-timeout overload): keep the original
					// in-stream delivery semantics.
					bootstrapTerminalErr = streamErr
					break
				}
				if helps.HasMeaningfulCodexOutputDelta(data) {
					sawOutputDelta = true
				}
				if helps.IsCodexTerminalEmptyIncomplete(data, len(outputItemsByIndex)+len(outputItemsFallback), sawOutputDelta) {
					closeBootstrapBody()
					streamErr := newCodexEmptyIncompleteStreamError()
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					// Not an overload rejection, so it keeps its in-stream delivery: flush what is
					// held and hand the error downstream, matching the websocket executor and the
					// contract that only overload and rate-limit rejections fail the attempt over.
					bootstrapTerminalErr = streamErr
					break
				}
				isHandshake = isCodexBootstrapBufferableEvent(eventType, data)
				switch eventType {
				case "response.output_item.done":
					collectCodexOutputItemDone(data, outputItemsByIndex, &outputItemsFallback)
				case "response.completed", "response.incomplete":
					terminalSuccess = true
					if detail, ok := helps.ParseCodexUsage(data); ok {
						reporter.Publish(ctx, detail)
					}
					publishCodexImageToolUsage(ctx, reporter, body, data)
					if !preserveNativeOutput {
						data = patchCodexCompletedOutput(data, outputItemsByIndex, outputItemsFallback)
					}
				}
				restoredEvents := [][]byte{data}
				if declarationTable != nil {
					restoredEvents = declarationTable.RestoreResponsesToolCallEvents(data)
				}
				translatedLines = translatedLines[:0]
				for _, restoredEvent := range restoredEvents {
					translatedLines = append(translatedLines, append([]byte("data: "), restoredEvent...))
				}
			} else {
				// Lines that are not data: frames - SSE comments, event:, id:, retry: and the blank
				// separator - carry no event to check against the allow-list, so they are held with
				// the frame they belong to. They spend the same budgets.
				isHandshake = true
			}

			var lineChunks [][]byte
			for _, translatedLine := range translatedLines {
				lineChunks = append(lineChunks, helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, originalPayload, body, translatedLine, &param, claudeInputTokens)...)
			}
			if isHandshake && !terminalSuccess {
				frameBytes := len(line)
				for i := range lineChunks {
					frameBytes += len(lineChunks[i])
				}
				timeSinceStart := nowCodexBootstrap().Sub(bootstrapStart)
				timeoutReached := bootstrapTimeout > 0 && timeSinceStart >= bootstrapTimeout
				if !timeoutReached && bufferedFrames < codexBootstrapMaxBufferedFrames && bufferedBytes+frameBytes <= codexBootstrapMaxBufferedBytes {
					bufferedFrames++
					bufferedBytes += frameBytes
					bufferedChunks = append(bufferedChunks, lineChunks...)
					continue
				}
				exhausted := "frame budget"
				if timeoutReached {
					exhausted = "time budget"
				} else if bufferedFrames < codexBootstrapMaxBufferedFrames {
					exhausted = "byte budget"
				}
				helps.LogWithRequestID(ctx).Debugf("codex executor: bootstrap %s exhausted after %d lines / %d bytes / %v, releasing stream without overload probing", exhausted, bufferedFrames, bufferedBytes, timeSinceStart)
			}

			initialChunks = lineChunks
			streamStarted = true
			if terminalSuccess {
				immediateTerminal = true
			}
			break
		}

		if !streamStarted && bootstrapTerminalErr == nil {
			closeBootstrapBody()
			if errScan := scanner.Err(); errScan != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				reporter.PublishFailure(ctx, errScan)
				return nil, errScan
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			streamErr := newCodexIncompleteStreamError()
			helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
			reporter.PublishFailure(ctx, streamErr)
			return nil, streamErr
		}
	}

	chanCapacity := len(bufferedChunks) + len(initialChunks)
	if bootstrapTerminalErr != nil {
		chanCapacity++
	}
	out := make(chan cliproxyexecutor.StreamChunk, chanCapacity)
	for _, chunk := range bufferedChunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	for _, chunk := range initialChunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	if bootstrapTerminalErr != nil {
		// Context-length and other non-overload terminal failures keep unbuffered semantics:
		// delivered as an in-stream error chunk after any flushed handshake payloads.
		out <- cliproxyexecutor.StreamChunk{Err: bootstrapTerminalErr}
		close(out)
		return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
	}
	if immediateTerminal {
		closeBootstrapBody()
		close(out)
		return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
	}

	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			translatedLines := [][]byte{bytes.Clone(line)}
			terminalSuccess := false

			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				data = helps.RestoreCodexMultiAgentV2Response(data, optimizeMultiAgentV2)
				observeCodexTokenEvent(reporter, data)
				if streamErr, _, ok := codexTerminalFailureErrWithCooling(data, e.modelLevelCooling()); ok {
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
					case <-ctx.Done():
					}
					return
				}
				if helps.HasMeaningfulCodexOutputDelta(data) {
					sawOutputDelta = true
				}
				if helps.IsCodexTerminalEmptyIncomplete(data, len(outputItemsByIndex)+len(outputItemsFallback), sawOutputDelta) {
					streamErr := newCodexEmptyIncompleteStreamError()
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
					case <-ctx.Done():
					}
					return
				}
				switch gjson.GetBytes(data, "type").String() {
				case "response.output_item.done":
					collectCodexOutputItemDone(data, outputItemsByIndex, &outputItemsFallback)
				case "response.completed", "response.incomplete", "response.done":
					terminalSuccess = true
					data = normalizeCodexWebsocketCompletion(data)
					if detail, ok := helps.ParseCodexUsage(data); ok {
						reporter.Publish(ctx, detail)
					}
					publishCodexImageToolUsage(ctx, reporter, body, data)
					if !preserveNativeOutput {
						data = patchCodexCompletedOutput(data, outputItemsByIndex, outputItemsFallback)
					}
				}
				restoredEvents := [][]byte{data}
				if declarationTable != nil {
					restoredEvents = declarationTable.RestoreResponsesToolCallEvents(data)
				}
				translatedLines = translatedLines[:0]
				for _, restoredEvent := range restoredEvents {
					translatedLines = append(translatedLines, append([]byte("data: "), restoredEvent...))
				}
			}

			for _, translatedLine := range translatedLines {
				chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, originalPayload, body, translatedLine, &param, claudeInputTokens)
				for i := range chunks {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
					case <-ctx.Done():
						return
					}
				}
			}
			if terminalSuccess {
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if ctx.Err() != nil {
				return
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
		}
		// The stream ended without a terminal event: surface the request-scoped
		// incomplete-stream error in-band (upstream codex_executor_stream.go).
		streamErr := newCodexIncompleteStreamError()
		helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
		reporter.PublishFailure(ctx, streamErr)
		select {
		case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
		case <-ctx.Done():
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *CodexExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	body, updatesChanged := helps.TranslateRequestWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, false, helps.APIKeyModelIsCompat(req))

	body, err := helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body, _ = sjson.SetBytes(body, "stream", false)
	body = normalizeCodexInstructions(body, helps.IsNativeCodexRequest(req.Payload, opts))

	enc, err := tokenizerForCodexModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: tokenizer init failed: %w", err)
	}

	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func tokenizerForCodexModel(model string) (tokenizer.Codec, error) {
	sanitized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case sanitized == "":
		return tokenizer.Get(tokenizer.Cl100kBase)
	case strings.HasPrefix(sanitized, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(sanitized, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(sanitized, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(sanitized, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(sanitized, "gpt-3.5"), strings.HasPrefix(sanitized, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	default:
		return tokenizer.Get(tokenizer.Cl100kBase)
	}
}

func countCodexInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(body) == 0 {
		return 0, nil
	}

	root := gjson.ParseBytes(body)
	var segments []string

	if inst := strings.TrimSpace(root.Get("instructions").String()); inst != "" {
		segments = append(segments, inst)
	}

	inputItems := root.Get("input")
	if inputItems.IsArray() {
		arr := inputItems.Array()
		for i := range arr {
			item := arr[i]
			switch item.Get("type").String() {
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					parts := content.Array()
					for j := range parts {
						part := parts[j]
						if text := strings.TrimSpace(part.Get("text").String()); text != "" {
							segments = append(segments, text)
						}
					}
				}
			case "function_call":
				if name := strings.TrimSpace(item.Get("name").String()); name != "" {
					segments = append(segments, name)
				}
				if args := strings.TrimSpace(item.Get("arguments").String()); args != "" {
					segments = append(segments, args)
				}
			case "function_call_output":
				if out := strings.TrimSpace(item.Get("output").String()); out != "" {
					segments = append(segments, out)
				}
			default:
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					segments = append(segments, text)
				}
			}
		}
	}

	tools := root.Get("tools")
	if tools.IsArray() {
		tarr := tools.Array()
		for i := range tarr {
			tool := tarr[i]
			if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
				segments = append(segments, name)
			}
			if desc := strings.TrimSpace(tool.Get("description").String()); desc != "" {
				segments = append(segments, desc)
			}
			if params := tool.Get("parameters"); params.Exists() {
				val := params.Raw
				if params.Type == gjson.String {
					val = params.String()
				}
				if trimmed := strings.TrimSpace(val); trimmed != "" {
					segments = append(segments, trimmed)
				}
			}
		}
	}

	textFormat := root.Get("text.format")
	if textFormat.Exists() {
		if name := strings.TrimSpace(textFormat.Get("name").String()); name != "" {
			segments = append(segments, name)
		}
		if schema := textFormat.Get("schema"); schema.Exists() {
			val := schema.Raw
			if schema.Type == gjson.String {
				val = schema.String()
			}
			if trimmed := strings.TrimSpace(val); trimmed != "" {
				segments = append(segments, trimmed)
			}
		}
	}

	text := strings.Join(segments, "\n")
	if text == "" {
		return 0, nil
	}

	count, err := enc.Count(text)
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
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
	svc := codexauth.NewCodexAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	// Use unified key in files
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now

	planType := strings.TrimSpace(td.PlanType)
	if planType == "" && td.IDToken != "" {
		if claims, errParse := codexauth.ParseJWTToken(td.IDToken); errParse == nil && claims != nil {
			planType = claims.GetPlanType()
		}
	}
	if planType == "" {
		planType = codexauth.DefaultPlanType
	}
	auth.Metadata["plan_type"] = planType
	clonedAttributes := make(map[string]string, len(auth.Attributes)+1)
	for k, v := range auth.Attributes {
		clonedAttributes[k] = v
	}
	clonedAttributes["plan_type"] = planType
	auth.Attributes = clonedAttributes
	if storage, ok := auth.Storage.(*codexauth.CodexTokenStorage); ok && storage != nil {
		clonedStorage := *storage
		svc.UpdateTokenStorage(&clonedStorage, td)
		clonedStorage.PlanType = planType
		auth.Storage = &clonedStorage
	}
	return auth, nil
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte) (*http.Request, error) {
	var cache helps.CodexCache
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := fmt.Sprintf("%s-%s", req.Model, userIDResult.String())
			var ok bool
			if cache, ok = helps.GetCodexCache(key); !ok {
				cache = helps.CodexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				helps.SetCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	} else if from == "openai" {
		if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
			cache.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String()
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
	}
	rawJSON = helps.SanitizeCodexInputItemIDs(rawJSON)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, err
	}
	if cache.ID != "" {
		httpReq.Header.Set("Session-Id", cache.ID)
	}
	return httpReq, nil
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0]
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	if ginHeaders.Get("X-Codex-Beta-Features") != "" {
		r.Header.Set("X-Codex-Beta-Features", ginHeaders.Get("X-Codex-Beta-Features"))
	}
	misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	// Forward the client's turn-state token so Codex can resume the same turn
	// across credentials (upstream e696ea47c5ee).
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-State", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Window-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Thread-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Session-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Openai-Internal-Codex-Responses-Lite", "")
	disableCloaking := isCodexCloakingDisabled(r.Context(), cfg, auth)
	cfgUserAgent := ""
	if !disableCloaking {
		cfgUserAgent, _ = codexHeaderDefaults(cfg, auth)
	}
	ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgentForCloaking(disableCloaking))

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		r.Header.Set("Originator", originator)
	} else if !isAPIKey && !disableCloaking {
		r.Header.Set("Originator", codexOriginator)
	}
	if !isAPIKey && !disableCloaking {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, ginHeaders)
}

func newCodexStatusErr(statusCode int, body []byte) statusErr {
	return newCodexStatusErrWithCooling(statusCode, body, false)
}

// newCodexStatusErrWithCooling maps an upstream error status/body onto a
// statusErr. When modelLevelCooling is disabled, usage_limit_reached quota
// failures are marked credential-scoped so the conductor cools the entire
// credential instead of only the requested model (upstream b064b832e242).
func newCodexStatusErrWithCooling(statusCode int, body []byte, modelLevelCooling bool) statusErr {
	errCode := statusCode
	isUsageLimit := isCodexUsageLimitError(body)
	credentialScoped := isUsageLimit && !modelLevelCooling
	if isCodexModelCapacityError(body) || isUsageLimit {
		errCode = http.StatusTooManyRequests
	}
	body = classifyCodexStatusError(errCode, body)
	err := statusErr{code: errCode, msg: string(body), credentialScoped: credentialScoped}
	if retryAfter := parseCodexRetryAfter(errCode, body, time.Now()); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	return err
}

func classifyCodexStatusError(statusCode int, body []byte) []byte {
	code, errType, ok := codexStatusErrorClassification(statusCode, body)
	if !ok {
		return body
	}
	message := gjson.GetBytes(body, "error.message").String()
	if message == "" {
		message = gjson.GetBytes(body, "message").String()
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(statusCode)
	}
	out := []byte(`{"error":{}}`)
	out, _ = sjson.SetBytes(out, "error.message", message)
	out, _ = sjson.SetBytes(out, "error.type", errType)
	out, _ = sjson.SetBytes(out, "error.code", code)
	return out
}

func codexStatusErrorClassification(statusCode int, body []byte) (code string, errType string, ok bool) {
	errorMessage := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()))
	if errorMessage == "" {
		errorMessage = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "message").String()))
	}
	lower := strings.ToLower(strings.TrimSpace(string(body)))
	upstreamCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	upstreamType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.type").String()))
	isInvalidRequest := upstreamType == "" || upstreamType == "invalid_request_error"

	switch {
	case statusCode == http.StatusRequestEntityTooLarge || upstreamCode == "context_length_exceeded" || upstreamCode == "context_too_large" || isInvalidRequest && (strings.Contains(errorMessage, "context length") || strings.Contains(errorMessage, "context_length") || strings.Contains(errorMessage, "maximum context") || strings.Contains(errorMessage, "too many tokens")):
		return "context_too_large", "invalid_request_error", true
	case strings.Contains(lower, "invalid signature in thinking block") || strings.Contains(lower, "invalid_encrypted_content"):
		return "thinking_signature_invalid", "invalid_request_error", true
	case upstreamCode == "previous_response_not_found" || strings.Contains(lower, "previous_response_not_found") || strings.Contains(lower, "previous_response_id") && strings.Contains(lower, "not found"):
		return "previous_response_not_found", "invalid_request_error", true
	case statusCode == http.StatusUnauthorized || upstreamType == "authentication_error" || upstreamCode == "invalid_api_key" || strings.Contains(lower, "invalid or expired token") || strings.Contains(lower, "refresh_token_reused"):
		return "auth_unavailable", "authentication_error", true
	default:
		return "", "", false
	}
}

// normalizeCodexInstructions fills a missing instructions field so upstream accepts the
// request. Native responses-lite requests are passed through untouched so their payload
// is preserved verbatim (upstream f702bc1ac263).
func normalizeCodexInstructions(body []byte, nativeRequest ...bool) []byte {
	if len(nativeRequest) > 0 && nativeRequest[0] {
		return body
	}
	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() || instructions.Type == gjson.Null {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	return body
}

const codexResponsesLiteHeader = "X-OpenAI-Internal-Codex-Responses-Lite"

var imageGenToolJSON = []byte(`{"type":"image_generation","output_format":"png"}`)
var imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func ensureImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header) []byte {
	if util.IsCodexResponsesLiteRequest(body, headers) {
		return body
	}
	if strings.HasSuffix(baseModel, "spark") {
		return body
	}
	if isCodexFreePlanAuth(auth) {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", imageGenToolArrayJSON)
		return body
	}
	for _, t := range tools.Array() {
		if t.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(t) {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tools.-1", imageGenToolJSON)
	return body
}

// isImageGenerationFunctionTool reports whether a tool entry already provides
// image generation through the image_gen.imagegen function or image_gen
// namespace shape, so a synthetic image_generation tool must not be injected
// (upstream codex_executor_request.go).
func isImageGenerationFunctionTool(tool gjson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		return tool.Get("name").String() == "image_gen.imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		tools := tool.Get("tools")
		if !tools.IsArray() {
			return false
		}
		for _, nestedTool := range tools.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

var webSearchToolJSON = []byte(`{"type":"web_search"}`)
var webSearchToolArrayJSON = []byte(`[{"type":"web_search"}]`)

func isCodexWebSearchToolType(toolType string) bool {
	switch strings.TrimSpace(toolType) {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
		return true
	default:
		return false
	}
}

// ensureWebSearchToolForHistory reconciles replayed web_search_call input items
// with the request's own tool declarations. A standard Responses request that
// replays a hosted search without declaring the tool is rejected upstream with
// "response protection is unavailable", so the declaration is restored; a
// responses-lite request omits top-level hosted tools by design, so the
// replayed items are stripped instead. Repeated runs converge: the tool is
// present (or the items are gone) and the payload is returned untouched.
// Ported from upstream CLIProxyAPI commit e7d4c7316fca.
func ensureWebSearchToolForHistory(body []byte, headers http.Header) []byte {
	if !bytes.Contains(body, []byte("web_search_call")) {
		return body
	}
	input := util.GetGJSONBytesNoCopy(body, "input")
	if !input.IsArray() {
		return body
	}
	hasWebSearchCall := false
	input.ForEach(func(_, item gjson.Result) bool {
		if strings.TrimSpace(item.Get("type").String()) == "web_search_call" {
			hasWebSearchCall = true
			return false
		}
		return true
	})
	if !hasWebSearchCall {
		return body
	}

	tools := util.GetGJSONBytesNoCopy(body, "tools")
	if tools.IsArray() {
		for _, t := range tools.Array() {
			if isCodexWebSearchToolType(t.Get("type").String()) {
				return body
			}
		}
	}

	if util.IsCodexResponsesLiteRequest(body, headers) {
		items := input.Array()
		rebuilt := make([]string, 0, len(items))
		for _, item := range items {
			if strings.TrimSpace(item.Get("type").String()) == "web_search_call" {
				continue
			}
			rebuilt = append(rebuilt, item.Raw)
		}
		updated, errSet := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(rebuilt, ",")+"]"))
		if errSet != nil {
			return body
		}
		return updated
	}

	if !tools.Exists() || !tools.IsArray() {
		updated, errSet := sjson.SetRawBytes(body, "tools", webSearchToolArrayJSON)
		if errSet != nil {
			return body
		}
		return updated
	}
	updated, errSet := sjson.SetRawBytes(body, "tools.-1", webSearchToolJSON)
	if errSet != nil {
		return body
	}
	return updated
}

// normalizeCodexParallelToolCalls forces parallel_tool_calls=false for native
// responses-lite requests and drops the field when no tools are declared
// (upstream codex_executor_request.go).
func normalizeCodexParallelToolCalls(body []byte, headers http.Header) []byte {
	if util.IsCodexResponsesLiteRequest(body, headers) {
		body = helps.SetBoolIfDifferent(body, "parallel_tool_calls", false)
		return body
	}
	return normalizeCodexParallelToolCallsForTools(body)
}

func normalizeCodexParallelToolCallsForTools(body []byte) []byte {
	if !gjson.GetBytes(body, "parallel_tool_calls").Exists() {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if hasTools {
		return body
	}

	body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
	return body
}

// observeCodexTokenEvent inspects a stream payload, marks TTFT on the first substantive
// token event, and records the model the upstream reports serving.
func observeCodexTokenEvent(reporter *helps.UsageReporter, payload []byte) {
	helps.ObserveResponsesTokenEvent(reporter, payload)
	reporter.ObserveCodexResponseModel(payload)
}

func publishCodexImageToolUsage(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte) {
	detail, ok := helps.ParseCodexImageToolUsage(completedData)
	if !ok {
		return
	}
	reporter.EnsurePublished(ctx)
	reporter.PublishAdditionalModel(ctx, codexImageGenerationToolModel(body), detail)
}

func codexImageGenerationToolModel(body []byte) string {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "image_generation" {
				continue
			}
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				return model
			}
			break
		}
	}
	return codexDefaultImageToolModel
}

func isCodexModelCapacityError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	candidates := []string{
		gjson.GetBytes(errorBody, "error.message").String(),
		gjson.GetBytes(errorBody, "message").String(),
		string(errorBody),
	}
	for _, candidate := range candidates {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if lower == "" {
			continue
		}
		if strings.Contains(lower, "model is at capacity") ||
			strings.Contains(lower, "model_at_capacity") ||
			strings.Contains(lower, "model_is_at_capacity") ||
			(strings.Contains(lower, "model") && strings.Contains(lower, "at capacity")) {
			return true
		}
	}
	return false
}

// isCodexUsageLimitError reports whether the error body represents a Codex
// quota/plan-limit exhaustion (error.type == "usage_limit_reached"). This is the
// signal Codex emits when a credential's usage quota is depleted, and it carries
// reset timing (resets_at/resets_in_seconds) parsed by parseCodexRetryAfter.
// Transient per-minute rate limits (rate_limit_error/rate_limit_exceeded) are
// intentionally excluded, as they should be retried rather than cooled down.
// Ported from upstream codex_executor_terminal.go.
func isCodexUsageLimitError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	candidates := []string{
		gjson.GetBytes(errorBody, "error.type").String(),
		gjson.GetBytes(errorBody, "type").String(),
	}
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate), "usage_limit_reached") {
			return true
		}
	}
	return false
}

func parseCodexRetryAfter(statusCode int, errorBody []byte, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests || len(errorBody) == 0 {
		return nil
	}
	for _, quota := range []gjson.Result{gjson.GetBytes(errorBody, "error"), gjson.ParseBytes(errorBody)} {
		if !strings.EqualFold(strings.TrimSpace(quota.Get("type").String()), "usage_limit_reached") {
			continue
		}
		if resetsAt := quota.Get("resets_at").Int(); resetsAt > 0 {
			resetAtTime := time.Unix(resetsAt, 0)
			if resetAtTime.After(now) {
				retryAfter := resetAtTime.Sub(now)
				return &retryAfter
			}
		}
		if resetsInSeconds := quota.Get("resets_in_seconds").Int(); resetsInSeconds > 0 {
			retryAfter := time.Duration(resetsInSeconds) * time.Second
			return &retryAfter
		}
	}
	return nil
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
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

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if e == nil {
		return nil
	}
	return resolveCodexKeyConfig(e.cfg, auth)
}

func resolveCodexKeyConfig(cfg *config.Config, auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
		// Ported from upstream CLIProxyAPI commit 81d6ba774621
		// (codex_executor_auth.go): the auth may carry the index of the config
		// entry it was synthesized from ("config_index"; upstream
		// cliproxyauth.AttributeConfigIndex — the fork uses the literal key).
		if index, errIndex := strconv.Atoi(strings.TrimSpace(auth.Attributes["config_index"])); errIndex == nil && index >= 0 && index < len(cfg.CodexKey) {
			entry := &cfg.CodexKey[index]
			cfgKey := strings.TrimSpace(entry.APIKey)
			cfgBase := strings.TrimSpace(entry.BaseURL)
			if (attrKey == "" || strings.EqualFold(cfgKey, attrKey)) && (attrBase == "" || strings.EqualFold(cfgBase, attrBase)) {
				return entry
			}
		}
	}
	for i := range cfg.CodexKey {
		entry := &cfg.CodexKey[i]
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
		for i := range cfg.CodexKey {
			entry := &cfg.CodexKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

// isCodexCloakingDisabled resolves the cloaking override for a credential in
// precedence order: auth attribute (synthesized from config or set on the auth
// file), then the matching credential's disable-codex-cloaking entry, then the
// global cloaking runtime control carried on the request context.
// Ported from upstream CLIProxyAPI commit f351924f42cb.
func isCodexCloakingDisabled(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth != nil && len(auth.Attributes) > 0 {
		if val, ok := auth.Attributes[cliproxyauth.AttributeCodexDisableCloaking]; ok {
			if parsed, errParse := strconv.ParseBool(strings.TrimSpace(val)); errParse == nil {
				return parsed
			}
		}
	}
	if entry := resolveCodexKeyConfig(cfg, auth); entry != nil && entry.DisableCodexCloaking != nil {
		return *entry.DisableCodexCloaking
	}
	return cliproxyexecutor.CodexCloakingDisabled(ctx)
}

// resolveCodexModelIsCompat reports whether the requested model runs against a
// third-party Responses-compatible endpoint that must receive cleartext
// reasoning content and reasoning item ids unmodified. The runtime-bound model
// info wins over configuration so an explicitly resolved non-compat model
// cannot be overridden by a matching config entry.
//
// Ported from upstream CLIProxyAPI commit 81d6ba774621
// (codex_executor_auth.go).
func (e *CodexExecutor) resolveCodexModelIsCompat(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, baseModel string) bool {
	if modelInfo, ok := cliproxyauth.ResolvedModelInfo(req); ok && modelInfo != nil {
		return modelInfo.IsCompat
	}
	entry := e.resolveCodexConfig(auth)
	if entry != nil && len(entry.Models) > 0 {
		requested := strings.TrimSpace(req.Model)
		target := strings.TrimSpace(baseModel)
		for i := range entry.Models {
			name := strings.TrimSpace(entry.Models[i].Name)
			alias := strings.TrimSpace(entry.Models[i].Alias)
			if (target != "" && (strings.EqualFold(name, target) || strings.EqualFold(alias, target))) ||
				(requested != "" && (strings.EqualFold(name, requested) || strings.EqualFold(alias, requested))) {
				return entry.Models[i].IsCompat
			}
		}
		return false
	}
	if cliproxyauth.CodexAPIKeyModelIsCompat(e.cfg, auth, baseModel) || cliproxyauth.CodexAPIKeyModelIsCompat(e.cfg, auth, req.Model) {
		return true
	}
	return false
}

// translateCodexRequestPair translates the baseline and working payloads with
// the compatibility-aware Claude->Codex translator when the resolved API-key
// model has is-compat enabled. Ported from upstream CLIProxyAPI
// (codex_executor_request.go).
func translateCodexRequestPair(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte) {
	original, body, _ := translateCodexRequestPairWithUpdateIntent(from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
	return original, body
}

// translateCodexRequestPairWithUpdateIntent additionally reports whether a
// plugin request normalizer modified Responses configuration_update items in
// the working payload.
func translateCodexRequestPairWithUpdateIntent(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool) {
	isCompat := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	ctx := context.Background()
	translate := func(raw []byte) ([]byte, bool) {
		if isCompat && from == sdktranslator.FormatClaude && to == sdktranslator.FormatCodex {
			return helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, nil, nil, from, to, model, raw, stream, true), false
		}
		translated := sdktranslator.TranslateRequestEnvelope(ctx, from, to, sdktranslator.RequestEnvelope{Format: from, Model: model, Stream: stream, Body: raw})
		return translated.Body, translated.ConfigurationUpdatesChanged
	}
	if bytes.Equal(originalPayload, payload) {
		body, changed := translate(payload)
		return body, body, changed
	}
	originalTranslated, _ := translate(originalPayload)
	body, changed := translate(payload)
	return originalTranslated, body, changed
}
