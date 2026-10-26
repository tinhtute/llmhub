package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	xaiauth "github.com/therealtinhtute/llmhub/internal/auth/xai"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/misc"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/therealtinhtute/llmhub/internal/util"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"
)

var (
	xaiDataTag  = []byte("data:")
	xaiEventTag = []byte("event:")
)

const (
	xaiImageHandlerType         = "openai-image"
	xaiVideoHandlerType         = "openai-video"
	xaiCustomToolType           = "custom"
	xaiFunctionToolType         = "function"
	xaiImageGenerationToolType  = "image_generation"
	xaiNamespaceToolType        = "namespace"
	xaiToolSearchType           = "tool_search"
	xaiWebSearchToolType        = "web_search"
	xaiClientWebSearchAlias     = "clientfn_web_search"
	xaiXSearchToolType          = "x_search"
	xaiMaxTools                 = 200
	xaiCodexAppNamespaceName    = "codex_app"
	xaiAutomationUpdateToolName = "automation_update"
	xaiSafeFunctionParameters   = `{"type":"object","properties":{},"additionalProperties":true}`
	xaiImagesGenerationsPath    = "/images/generations"
	xaiImagesEditsPath          = "/images/edits"
	xaiDefaultImageEndpointPath = xaiImagesGenerationsPath
	xaiVideosGenerationsPath    = "/videos/generations"
	xaiVideosEditsPath          = "/videos/edits"
	xaiVideosExtensionsPath     = "/videos/extensions"
	xaiVideosPath               = "/videos"
	xaiIdempotencyKeyMetaKey    = "idempotency_key"
	xaiCLIChatProxyBaseURL      = "https://cli-chat-proxy.grok.com/v1"
	xaiUsingAPIAttr             = "using_api"
)

var xaiXSearchToolJSON = []byte(`{"type":"x_search"}`)

// XAIExecutor is a stateless executor for xAI Grok's Responses API.
type XAIExecutor struct {
	cfg *config.Config
}

// NewXAIExecutor creates a new xAI executor.
func NewXAIExecutor(cfg *config.Config) *XAIExecutor {
	return &XAIExecutor{cfg: cfg}
}

// Identifier returns the provider identifier.
func (e *XAIExecutor) Identifier() string {
	return "xai"
}

// PrepareRequest injects xAI credentials into the outgoing HTTP request.
func (e *XAIExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := xaiCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Del("Authorization")
	}
	ensureXAIUserAgent(req)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects xAI credentials into the request and executes it.
func (e *XAIExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("xai executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *XAIExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if endpointPath := xaiImageEndpointPath(opts); endpointPath != "" {
		return e.executeImages(ctx, auth, req, opts, endpointPath)
	}
	if xaiIsVideoRequest(opts) {
		return e.executeVideos(ctx, auth, req, opts)
	}

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)

	prepared, err := e.prepareResponsesRequest(ctx, auth, req, opts, true)
	if err != nil {
		return resp, err
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	// prepareResponsesRequest already applied the payload rules, so the prepared
	// body carries the effective outbound tier.
	reporter.SetEffectiveServiceTier(prepared.body)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.body))
	if err != nil {
		return resp, err
	}
	applyXAIHeaders(httpReq, auth, token, true, prepared.sessionID, opts.Headers)
	e.recordXAIRequest(ctx, auth, url, httpReq.Header.Clone(), prepared.body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := io.ReadAll(httpResp.Body)
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return resp, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		return resp, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)

	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	responseFilter := newXAIInternalXSearchResponseFilter(prepared.declarations)
	responseRestorer := newXAIToolResponseRestorer(prepared.declarations)
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.HasPrefix(line, xaiDataTag) {
			continue
		}
		eventData := xaiNormalizeReasoningSummaryData(bytes.TrimSpace(line[len(xaiDataTag):]))
		eventData = responseFilter.apply(eventData)
		if len(eventData) == 0 {
			continue
		}
		reporter.ObserveResponseModel(eventData)
		eventData = responseRestorer.apply(eventData)
		if prepared.webSearchAlias != "" {
			eventData = restoreXAIClientWebSearchName(eventData, prepared.webSearchAlias)
		}
		switch gjson.GetBytes(eventData, "type").String() {
		case "response.output_item.done":
			xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
		case "response.completed", "response.incomplete":
			if detail, ok := helps.ParseCodexUsage(eventData); ok {
				reporter.Publish(ctx, detail)
			}
			completedData := xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
			completedData = xaiNormalizeReasoningSummaryData(completedData)
			var param any
			out := sdktranslator.TranslateNonStream(ctx, prepared.to, prepared.from, req.Model, prepared.originalPayload, prepared.body, completedData, &param)
			if prepared.from == sdktranslator.FormatOpenAIResponse {
				out = helps.EnsureResponsesUsageDetails(out)
			}
			return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
		}
	}

	return resp, statusErr{code: http.StatusRequestTimeout, msg: "xai stream error: stream disconnected before response.completed or response.incomplete"}
}

func (e *XAIExecutor) executeImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	model := strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)
	logXAIResolvedBaseURL(ctx, baseURL)
	if endpointPath == "" {
		endpointPath = xaiDefaultImageEndpointPath
	}

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Payload))
	if err != nil {
		return resp, err
	}
	applyXAIHeaders(httpReq, auth, token, false, "", opts.Headers)
	e.recordXAIRequest(ctx, auth, url, httpReq.Header.Clone(), req.Payload)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
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
		return resp, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	reporter.ObserveResponseModel(data)
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

func (e *XAIExecutor) executeVideos(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	model := strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)
	logXAIResolvedBaseURL(ctx, baseURL)

	method := http.MethodPost
	endpointPath := xaiVideosGenerationsPath
	var body io.Reader = bytes.NewReader(req.Payload)

	switch path := xaiVideoEndpointPath(opts); path {
	case xaiVideosGenerationsPath, xaiVideosEditsPath, xaiVideosExtensionsPath:
		endpointPath = path
	default:
		if requestID := strings.TrimSpace(gjson.GetBytes(req.Payload, "request_id").String()); requestID != "" {
			method = http.MethodGet
			endpointPath = xaiVideosPath + "/" + url.PathEscape(requestID)
			body = nil
		}
	}
	requestURL := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return resp, err
	}
	applyXAIHeaders(httpReq, auth, token, false, "", opts.Headers)
	if method == http.MethodPost {
		key := xaiMetadataString(opts.Metadata, xaiIdempotencyKeyMetaKey)
		if key == "" && opts.Headers != nil {
			key = strings.TrimSpace(opts.Headers.Get("x-idempotency-key"))
		}
		if key != "" {
			httpReq.Header.Set("x-idempotency-key", key)
		}
	}
	e.recordXAIRequest(ctx, auth, requestURL, httpReq.Header.Clone(), req.Payload)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
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
		return resp, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	reporter.ObserveResponseModel(data)
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

func (e *XAIExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)

	prepared, err := e.prepareResponsesRequest(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	// prepareResponsesRequest already applied the payload rules, so the prepared
	// body carries the effective outbound tier.
	reporter.SetEffectiveServiceTier(prepared.body)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.body))
	if err != nil {
		return nil, err
	}
	applyXAIHeaders(httpReq, auth, token, true, prepared.sessionID, opts.Headers)
	e.recordXAIRequest(ctx, auth, url, httpReq.Header.Clone(), prepared.body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		return nil, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("xai executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		claudeInputTokens := helps.NewClaudeInputTokenState(prepared.from, prepared.to, prepared.from, prepared.originalPayload)
		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		responseFilter := newXAIInternalXSearchResponseFilter(prepared.declarations)
		responseRestorer := newXAIToolResponseRestorer(prepared.declarations)
		var pendingEventLine []byte
		emitTranslatedLine := func(translatedLine []byte) bool {
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, prepared.to, prepared.from, req.Model, prepared.originalPayload, prepared.body, translatedLine, &param, claudeInputTokens)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)

			if bytes.HasPrefix(line, xaiEventTag) {
				if pendingEventLine != nil && !emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")) {
					return
				}
				pendingEventLine = bytes.Clone(line)
				continue
			}

			if bytes.HasPrefix(line, xaiDataTag) {
				eventDataList := xaiNormalizeReasoningSummaryDataEvents(bytes.TrimSpace(line[len(xaiDataTag):]))
				hasPendingEventLine := pendingEventLine != nil
				for i, eventData := range eventDataList {
					eventData = responseFilter.apply(eventData)
					if len(eventData) == 0 {
						if hasPendingEventLine && i == 0 {
							pendingEventLine = nil
						}
						continue
					}
					reporter.ObserveResponseModel(eventData)
					eventData = responseRestorer.apply(eventData)
					if prepared.webSearchAlias != "" {
						eventData = restoreXAIClientWebSearchName(eventData, prepared.webSearchAlias)
					}
					normalizedEventName := gjson.GetBytes(eventData, "type").String()
					switch normalizedEventName {
					case "response.output_item.done":
						xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
					case "response.completed", "response.incomplete":
						if detail, ok := helps.ParseCodexUsage(eventData); ok {
							reporter.Publish(ctx, detail)
						}
						eventData = xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
						eventData = xaiNormalizeReasoningSummaryData(eventData)
						normalizedEventName = gjson.GetBytes(eventData, "type").String()
					}

					if hasPendingEventLine {
						eventLine := []byte("event: " + normalizedEventName)
						if i == 0 {
							eventLine = xaiNormalizeReasoningSummaryEventLine(pendingEventLine, normalizedEventName)
							pendingEventLine = nil
						}
						if !emitTranslatedLine(eventLine) {
							return
						}
					}
					if !emitTranslatedLine(append([]byte("data: "), eventData...)) {
						return
					}
				}
				continue
			}

			if pendingEventLine != nil {
				if !emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")) {
					return
				}
				pendingEventLine = nil
			}
			if !emitTranslatedLine(bytes.Clone(line)) {
				return
			}
		}
		if pendingEventLine != nil {
			emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, ""))
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates token count for xAI Responses requests.
func (e *XAIExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	prepared, err := e.prepareResponsesRequest(ctx, auth, req, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai executor: tokenizer init failed: %w", err)
	}
	count, err := countXAIInputTokens(enc, prepared.body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai executor: token counting failed: %w", err)
	}
	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	translated := sdktranslator.TranslateTokenCount(ctx, prepared.to, prepared.from, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func countXAIInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(body) == 0 {
		return 0, nil
	}

	root := gjson.ParseBytes(body)
	segments := make([]string, 0, 32)
	xaiAppendTokenString(&segments, root.Get("instructions"))
	xaiCollectInputTokenSegments(root.Get("input"), &segments)
	xaiCollectToolTokenSegments(root.Get("tools"), &segments)
	if textFormat := root.Get("text.format"); textFormat.Exists() {
		xaiAppendTokenString(&segments, textFormat.Get("name"))
		xaiAppendTokenJSON(&segments, textFormat.Get("schema"))
	}
	if len(segments) == 0 {
		return 0, nil
	}
	count, err := enc.Count(strings.Join(segments, "\n"))
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func xaiCollectInputTokenSegments(input gjson.Result, segments *[]string) {
	if input.Type == gjson.String {
		xaiAppendTokenString(segments, input)
		return
	}
	if !input.IsArray() {
		return
	}
	for _, item := range input.Array() {
		switch item.Get("type").String() {
		case "message":
			xaiCollectContentTokenSegments(item.Get("content"), segments)
		case "function_call":
			xaiAppendTokenString(segments, item.Get("name"))
			xaiAppendTokenJSON(segments, item.Get("arguments"))
		case "function_call_output":
			xaiAppendTokenJSON(segments, item.Get("output"))
		case "reasoning":
			for _, part := range item.Get("summary").Array() {
				xaiAppendTokenString(segments, part.Get("text"))
			}
		}
	}
}

func xaiCollectContentTokenSegments(content gjson.Result, segments *[]string) {
	if content.Type == gjson.String {
		xaiAppendTokenString(segments, content)
		return
	}
	if !content.IsArray() {
		return
	}
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "text", "input_text", "output_text":
			xaiAppendTokenString(segments, part.Get("text"))
		case "refusal":
			xaiAppendTokenString(segments, part.Get("refusal"))
		case "input_image":
			xaiAppendTokenString(segments, part.Get("image_url"))
			xaiAppendTokenString(segments, part.Get("file_id"))
		case "input_file":
			xaiAppendTokenString(segments, part.Get("file_data"))
			xaiAppendTokenString(segments, part.Get("file_url"))
			xaiAppendTokenString(segments, part.Get("file_id"))
			xaiAppendTokenString(segments, part.Get("filename"))
		case "input_audio":
			xaiAppendTokenString(segments, part.Get("data"))
			xaiAppendTokenString(segments, part.Get("input_audio.data"))
		}
	}
}

func xaiCollectToolTokenSegments(tools gjson.Result, segments *[]string) {
	if !tools.IsArray() {
		return
	}
	for _, tool := range tools.Array() {
		if tool.Get("type").String() != xaiFunctionToolType {
			continue
		}
		xaiAppendTokenString(segments, tool.Get("name"))
		xaiAppendTokenString(segments, tool.Get("description"))
		xaiAppendTokenJSON(segments, tool.Get("parameters"))
	}
}

func xaiAppendTokenString(segments *[]string, value gjson.Result) {
	if text := strings.TrimSpace(value.String()); text != "" {
		*segments = append(*segments, text)
	}
}

func xaiAppendTokenJSON(segments *[]string, value gjson.Result) {
	if !value.Exists() {
		return
	}
	if value.Type == gjson.String {
		xaiAppendTokenString(segments, value)
		return
	}
	if text := strings.TrimSpace(value.Raw); text != "" {
		*segments = append(*segments, text)
	}
}

// Refresh refreshes xAI OAuth credentials using the stored refresh token.
func (e *XAIExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("xai executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "xai executor: auth is nil"}
	}
	refreshToken := xaiMetadataString(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}
	tokenEndpoint := xaiMetadataString(auth.Metadata, "token_endpoint")
	svc := xaiauth.NewXAIAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokens(ctx, refreshToken, tokenEndpoint)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["type"] = "xai"
	auth.Metadata["auth_kind"] = "oauth"
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.IDToken != "" {
		auth.Metadata["id_token"] = td.IDToken
	}
	if td.TokenType != "" {
		auth.Metadata["token_type"] = td.TokenType
	}
	if td.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = td.ExpiresIn
	}
	if td.Expire != "" {
		auth.Metadata["expired"] = td.Expire
	}
	if td.Email != "" {
		auth.Metadata["email"] = td.Email
	}
	if td.Subject != "" {
		auth.Metadata["sub"] = td.Subject
	}
	if tokenEndpoint != "" {
		auth.Metadata["token_endpoint"] = tokenEndpoint
	}
	if xaiMetadataString(auth.Metadata, "base_url") == "" {
		auth.Metadata["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["auth_kind"] = "oauth"
	if strings.TrimSpace(auth.Attributes["base_url"]) == "" {
		auth.Attributes["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	return auth, nil
}

type xaiPreparedRequest struct {
	baseModel       string
	from            sdktranslator.Format
	to              sdktranslator.Format
	originalPayload []byte
	body            []byte
	declarations    *xaiToolDeclarations
	sessionID       string
	webSearchAlias  string
}

func (e *XAIExecutor) prepareResponsesRequest(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*xaiPreparedRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, stream)
	body, updatesChanged := helps.TranslateRequestWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, bytes.Clone(req.Payload), stream, helps.APIKeyModelIsCompat(req))

	var err error
	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), e.Identifier(), e.Identifier(), updatesChanged)
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", stream)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	declarationSource := body
	if from == sdktranslator.FormatOpenAIResponse {
		declarationSource = originalPayload
	}
	willInjectXSearch := e.cfg != nil && e.cfg.XAI.InjectXSearch
	shouldFold := xaiShouldFoldNamespaceTools(declarationSource, willInjectXSearch)
	declarations, err := buildXAIToolDeclarationsWithFold(declarationSource, shouldFold)
	if err != nil {
		return nil, err
	}
	if from == sdktranslator.FormatOpenAIResponse {
		shouldFoldBody := xaiShouldFoldNamespaceTools(body, willInjectXSearch)
		translatedDeclarations, errTranslated := buildXAIToolDeclarationsWithFold(body, shouldFoldBody)
		if errTranslated != nil {
			return nil, errTranslated
		}
		declarations = mergeXAIToolDeclarations(declarations, translatedDeclarations)
	}
	body = normalizeXAIToolsWithDeclarations(body, declarations, shouldFold)
	if shouldFold && from == sdktranslator.FormatOpenAIResponse {
		tools := gjson.GetBytes(originalPayload, "tools")
		if tools.IsArray() {
			filtered, _, ok := normalizeXAIToolArray(tools, "", declarations, xaiSupportsNativeImageGeneration(baseModel), shouldFold)
			if ok && len(filtered) > 0 {
				body, _ = sjson.SetRawBytes(body, "tools", filtered)
			}
		}
	}
	body = promoteXAIAdditionalTools(body)
	var webSearchAlias string
	if xaiHasClientWebSearchFunction(body, declarations) {
		webSearchAlias = xaiResolveClientWebSearchAlias(body)
		body = aliasXAIClientWebSearchFunction(body, webSearchAlias, declarations)
	}
	body = normalizeXAINamespaceToolChoiceWithFold(body, shouldFold)
	body = normalizeXAIToolChoices(body, declarations)
	// Prune before rewriting hosted tool choices so older models that still
	// strip the tool do not keep a leftover "required" selection.
	body = pruneXAIOrphanedToolChoice(body)
	body = normalizeXAIForcedWebSearchToolChoice(body)
	body = normalizeXAIForcedImageGenerationToolChoice(body)
	body = normalizeXAIToolChoiceForTools(body)
	// Skip x_search injection when the request was forced to a hosted tool and
	// the remaining tools list is only that hosted tool. "required" plus extra
	// tools would let Grok call x_search instead of the forced hosted tool.
	if willInjectXSearch && !xaiToolChoiceRequiresHostedToolOnlyAny(body) {
		body = ensureXAINativeXSearchTool(body)
	}
	body = clampXAIToolsLimit(body, xaiMaxTools, declarations)
	body = normalizeXAIInputNamespaceToolCallsWithFold(body, shouldFold)
	if webSearchAlias != "" {
		body = aliasXAIClientWebSearchInput(body, webSearchAlias, declarations)
	}
	// Only OpenRouter mints and accepts the private reasoning router suffix, so
	// replaying a routed Grok blob to any other upstream must strip it before the
	// request leaves (upstream CLIProxyAPI a68c7d8ed7c2).
	body = normalizeXAIInputReasoningItems(body, isOpenRouterUpstreamURL(xaiChatBaseURL(auth)))
	body = normalizeCodexInstructions(body)
	body = removeXAIEncryptedReasoningInclude(body)
	// stop is supported by Chat Completions but not by xAI's Responses API.
	// Thinking was handled before payload overrides and must not be revalidated here.
	body, _ = sjson.DeleteBytes(body, "stop")
	body = normalizeXAIToolChoiceForTools(body)
	sessionID := xaiExecutionSessionID(req, opts)
	if sessionID != "" {
		body, _ = sjson.SetBytes(body, "prompt_cache_key", sessionID)
	}

	return &xaiPreparedRequest{
		baseModel:       baseModel,
		from:            from,
		to:              to,
		originalPayload: originalPayload,
		body:            body,
		declarations:    declarations,
		sessionID:       sessionID,
		webSearchAlias:  webSearchAlias,
	}, nil
}

func (e *XAIExecutor) recordXAIRequest(ctx context.Context, auth *cliproxyauth.Auth, url string, headers http.Header, body []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   headers,
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

func xaiCreds(auth *cliproxyauth.Auth) (token, baseURL string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		token = strings.TrimSpace(auth.Attributes["api_key"])
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if auth.Metadata != nil {
		if token == "" {
			token = xaiMetadataString(auth.Metadata, "access_token")
		}
		if baseURL == "" {
			baseURL = xaiMetadataString(auth.Metadata, "base_url")
		}
	}
	return token, baseURL
}
func xaiUsingAPI(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return true
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes[xaiUsingAPIAttr]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) > 0 {
		raw, ok := auth.Metadata[xaiUsingAPIAttr]
		if ok && raw != nil {
			switch v := raw.(type) {
			case bool:
				return v
			case string:
				parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
				if errParse == nil {
					return parsed
				}
			default:
			}
		}
	}
	if raw := strings.TrimSpace(auth.Attributes["auth_kind"]); raw != "" {
		return !strings.EqualFold(raw, "oauth")
	}
	return !strings.EqualFold(xaiMetadataString(auth.Metadata, "auth_kind"), "oauth")
}

func xaiNormalizeBaseURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

func xaiIsDefaultAPIBaseURL(baseURL string) bool {
	return xaiNormalizeBaseURL(baseURL) == xaiNormalizeBaseURL(xaiauth.DefaultAPIBaseURL)
}

func xaiIsCLIChatProxyBaseURL(baseURL string) bool {
	return xaiNormalizeBaseURL(baseURL) == xaiNormalizeBaseURL(xaiCLIChatProxyBaseURL)
}

func xaiChatBaseURL(auth *cliproxyauth.Auth) string {
	_, baseURL := xaiCreds(auth)
	if xaiUsingAPI(auth) {
		if baseURL == "" {
			return xaiauth.DefaultAPIBaseURL
		}
		return baseURL
	}
	if baseURL != "" && !xaiIsDefaultAPIBaseURL(baseURL) {
		return baseURL
	}
	return xaiCLIChatProxyBaseURL
}

func xaiBaseURLSource(baseURL string) string {
	switch {
	case xaiIsDefaultAPIBaseURL(baseURL):
		return "DefaultAPIBaseURL"
	case xaiIsCLIChatProxyBaseURL(baseURL):
		return "CLIChatProxyBaseURL"
	default:
		return "custom"
	}
}

func logXAIResolvedBaseURL(ctx context.Context, baseURL string) {
	helps.LogWithRequestID(ctx).Infof("xai: using base_url=%s source=%s", baseURL, xaiBaseURLSource(baseURL))
}

// xaiDefaultUserAgent is the Grok CLI workspace identity sent to xAI when the
// downstream client supplied no User-Agent, standing in for Go's default HTTP
// client identity. The version is upstream CLIProxyAPI's
// DefaultXAIFallbackClientVersion.
const xaiDefaultUserAgent = "xai-grok-workspace/1.0.46"

// ensureXAIUserAgent forwards the downstream User-Agent, falling back to the
// Grok workspace User-Agent instead of Go's default HTTP client identity.
func ensureXAIUserAgent(r *http.Request, clientHeaders ...http.Header) {
	if r == nil {
		return
	}
	misc.EnsureHeader(r.Header, util.ResolveClientHeaders(r, clientHeaders...), "User-Agent", xaiDefaultUserAgent)
}

func applyXAIHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, sessionID string, clientHeaders ...http.Header) {
	r.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")
	if sessionID != "" {
		r.Header.Set("x-grok-conv-id", sessionID)
	}
	ensureXAIUserAgent(r, clientHeaders...)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, clientHeaders...)
}

func xaiExecutionSessionID(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if value := xaiMetadataString(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return value
	}
	if value := xaiMetadataString(req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return value
	}
	if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
		return strings.TrimSpace(promptCacheKey.String())
	}
	return helps.DerivedSessionUUID("xai", opts.Metadata, req.Metadata)
}

func xaiImageEndpointPath(opts cliproxyexecutor.Options) string {
	if opts.SourceFormat.String() != xaiImageHandlerType {
		return ""
	}

	path := xaiMetadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey)
	if strings.HasSuffix(path, "/images/edits") {
		return xaiImagesEditsPath
	}
	if strings.HasSuffix(path, "/images/generations") {
		return xaiImagesGenerationsPath
	}
	return xaiDefaultImageEndpointPath
}

func xaiIsVideoRequest(opts cliproxyexecutor.Options) bool {
	return opts.SourceFormat.String() == xaiVideoHandlerType
}

func xaiVideoEndpointPath(opts cliproxyexecutor.Options) string {
	if !xaiIsVideoRequest(opts) {
		return ""
	}
	path := xaiMetadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey)
	if strings.HasSuffix(path, "/videos/edits") {
		return xaiVideosEditsPath
	}
	if strings.HasSuffix(path, "/videos/extensions") {
		return xaiVideosExtensionsPath
	}
	if strings.HasSuffix(path, "/videos/generations") {
		return xaiVideosGenerationsPath
	}
	return ""
}

func xaiMetadataString(meta map[string]any, key string) string {
	if len(meta) == 0 || key == "" {
		return ""
	}
	value, ok := meta[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

type xaiToolIdentity struct {
	namespace string
	name      string
	toolType  string
}

type xaiToolDeclaration struct {
	original      xaiToolIdentity
	effectiveName string
	effectiveType string
	isDispatcher  bool
}

type xaiToolDeclarations struct {
	byOriginal      map[xaiToolIdentity]xaiToolDeclaration
	byEffectiveName map[string]xaiToolDeclaration
}

func xaiCountFlattenedTools(tools gjson.Result) int {
	if !tools.Exists() || !tools.IsArray() {
		return 0
	}
	count := 0
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case xaiNamespaceToolType:
			if nestedTools := tool.Get("tools"); nestedTools.IsArray() {
				count += len(nestedTools.Array())
			} else {
				count++
			}
		case xaiToolSearchType:
			// Tool search is stripped by normalizeXAITool
		default:
			count++
		}
	}
	return count
}

func xaiTotalFlattenedToolsCount(body []byte, willInjectXSearch bool) int {
	count := xaiCountFlattenedTools(gjson.GetBytes(body, "tools"))
	input := gjson.GetBytes(body, "input")
	if input.Exists() && input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() == "additional_tools" {
				count += xaiCountFlattenedTools(item.Get("tools"))
			}
		}
	}
	if willInjectXSearch && !xaiRequestHasNativeXSearch(body) && !xaiToolChoiceRequiresHostedToolOnlyAny(body) {
		count++
	}
	return count
}

func xaiShouldFoldNamespaceTools(body []byte, willInjectXSearch bool) bool {
	return xaiTotalFlattenedToolsCount(body, willInjectXSearch) > xaiMaxTools
}

func buildXAINamespaceDispatcherTool(tool gjson.Result) []byte {
	namespaceName := strings.TrimSpace(tool.Get("name").String())
	if namespaceName == "" {
		return nil
	}
	description := strings.TrimSpace(tool.Get("description").String())

	var toolNames []string
	var toolDescriptions []string
	if nestedTools := tool.Get("tools"); nestedTools.IsArray() {
		for _, child := range nestedTools.Array() {
			childName := strings.TrimSpace(child.Get("name").String())
			if childName == "" {
				continue
			}
			toolNames = append(toolNames, childName)
			childDesc := strings.TrimSpace(child.Get("description").String())

			params := child.Get("parameters")
			if !params.Exists() {
				params = child.Get("input_schema")
			}

			var paramStr string
			if params.Exists() && params.Raw != "" {
				rawParams := strings.TrimSpace(params.Raw)
				if rawParams != "" && rawParams != "{}" && rawParams != `{"type":"object","properties":{}}` {
					inlined := xaiInlineLocalRefs(rawParams)
					if gjson.Valid(inlined) {
						cleaned := []byte(inlined)
						if gjson.GetBytes(cleaned, "$defs").Exists() {
							cleaned, _ = sjson.DeleteBytes(cleaned, "$defs")
						}
						if gjson.GetBytes(cleaned, "definitions").Exists() {
							cleaned, _ = sjson.DeleteBytes(cleaned, "definitions")
						}
						paramStr = string(cleaned)
					} else {
						paramStr = inlined
					}
				}
			}

			var entry string
			if childDesc != "" {
				if paramStr != "" {
					entry = fmt.Sprintf("- %s: %s\n  Parameters: %s", childName, childDesc, paramStr)
				} else {
					entry = fmt.Sprintf("- %s: %s", childName, childDesc)
				}
			} else {
				if paramStr != "" {
					entry = fmt.Sprintf("- %s\n  Parameters: %s", childName, paramStr)
				} else {
					entry = fmt.Sprintf("- %s", childName)
				}
			}
			toolDescriptions = append(toolDescriptions, entry)
		}
	}

	fullDescription := description
	if len(toolDescriptions) > 0 {
		catalog := "Available tools in this namespace:\n" + strings.Join(toolDescriptions, "\n")
		if fullDescription != "" {
			fullDescription += "\n\n" + catalog
		} else {
			fullDescription = fmt.Sprintf("Tools in namespace %s.\n\n%s", namespaceName, catalog)
		}
	} else if fullDescription == "" {
		fullDescription = fmt.Sprintf("Tools in namespace %s.", namespaceName)
	}

	nameProp := map[string]any{
		"type":        "string",
		"description": fmt.Sprintf("Child tool name to execute in namespace %s", namespaceName),
	}
	if len(toolNames) > 0 {
		nameProp["enum"] = toolNames
	}

	dispatcher := map[string]any{
		"type":        xaiFunctionToolType,
		"name":        namespaceName,
		"description": fullDescription,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": nameProp,
				"arguments": map[string]any{
					"type":                 "object",
					"description":          "Arguments object matching the parameter schema of the selected child tool",
					"additionalProperties": true,
				},
			},
			"required": []string{"name"},
		},
	}

	raw, errMarshal := json.Marshal(dispatcher)
	if errMarshal != nil {
		return nil
	}
	return raw
}

func buildXAIToolDeclarations(body []byte) (*xaiToolDeclarations, error) {
	return buildXAIToolDeclarationsWithFold(body, xaiShouldFoldNamespaceTools(body, false))
}

func buildXAIToolDeclarationsWithFold(body []byte, shouldFold bool) (*xaiToolDeclarations, error) {
	declarations := &xaiToolDeclarations{
		byOriginal:      make(map[xaiToolIdentity]xaiToolDeclaration),
		byEffectiveName: make(map[string]xaiToolDeclaration),
	}
	collect := func(tools gjson.Result, namespace string) error {
		return collectXAIToolDeclarations(tools, namespace, declarations, shouldFold)
	}
	if err := collect(gjson.GetBytes(body, "tools"), ""); err != nil {
		return nil, err
	}
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() != "additional_tools" {
				continue
			}
			if err := collect(item.Get("tools"), ""); err != nil {
				return nil, err
			}
		}
	}
	return declarations, nil
}

func mergeXAIToolDeclarations(primary, fallback *xaiToolDeclarations) *xaiToolDeclarations {
	if primary == nil {
		return fallback
	}
	if fallback == nil {
		return primary
	}
	for effectiveName, declaration := range fallback.byEffectiveName {
		if _, exists := primary.byEffectiveName[effectiveName]; exists {
			continue
		}
		primary.byEffectiveName[effectiveName] = declaration
		primary.byOriginal[declaration.original] = declaration
	}
	for identity, declaration := range fallback.byOriginal {
		if _, exists := primary.byOriginal[identity]; exists {
			continue
		}
		primary.byOriginal[identity] = declaration
	}
	return primary
}

func collectXAIToolDeclarations(tools gjson.Result, namespace string, declarations *xaiToolDeclarations, shouldFold bool) error {
	if declarations == nil || !tools.IsArray() {
		return nil
	}
	for _, tool := range tools.Array() {
		toolType := tool.Get("type").String()
		if toolType == xaiNamespaceToolType {
			namespaceName := tool.Get("name").String()
			if shouldFold {
				identity := xaiToolIdentity{namespace: namespaceName, name: "", toolType: xaiFunctionToolType}
				decl := xaiToolDeclaration{
					original:      identity,
					effectiveName: namespaceName,
					effectiveType: xaiFunctionToolType,
					isDispatcher:  true,
				}
				declarations.byOriginal[identity] = decl
				declarations.byEffectiveName[namespaceName] = decl
			}
			if err := collectXAIToolDeclarations(tool.Get("tools"), namespaceName, declarations, shouldFold); err != nil {
				return err
			}
			continue
		}
		if toolType != xaiFunctionToolType && toolType != xaiCustomToolType {
			continue
		}
		name := tool.Get("name").String()
		if name == "" || (toolType == xaiCustomToolType && name == "apply_patch") {
			continue
		}
		identity := xaiToolIdentity{namespace: namespace, name: name, toolType: toolType}
		effectiveName := name
		if namespace != "" && !shouldFold {
			effectiveName = qualifyXAINamespaceToolName(namespace, name)
		}
		declaration := xaiToolDeclaration{
			original:      identity,
			effectiveName: effectiveName,
			effectiveType: xaiFunctionToolType,
			isDispatcher:  false,
		}
		if existing, ok := declarations.byEffectiveName[effectiveName]; ok && existing.original != identity && !shouldFold {
			return newXAIToolNameCollisionError(existing, declaration)
		}
		declarations.byOriginal[identity] = declaration
		if !shouldFold {
			declarations.byEffectiveName[effectiveName] = declaration
		}
	}
	return nil
}

func newXAIToolNameCollisionError(existing, incoming xaiToolDeclaration) error {
	message := fmt.Sprintf("tool declarations %s and %s resolve to the same outbound name %q", xaiToolIdentityLabel(existing.original), xaiToolIdentityLabel(incoming.original), incoming.effectiveName)
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "invalid_request_error",
			"code":    "tool_name_collision",
		},
	})
	return statusErr{code: http.StatusBadRequest, msg: string(payload)}
}

func xaiToolIdentityLabel(identity xaiToolIdentity) string {
	name := identity.name
	if identity.namespace != "" {
		name = identity.namespace + "." + name
	}
	return fmt.Sprintf("%q (%s)", name, identity.toolType)
}

func qualifyXAINamespaceToolName(namespace, name string) string {
	if namespace == "" || name == "" || strings.HasPrefix(name, "mcp__") {
		return name
	}
	prefix := namespace
	if !strings.HasSuffix(prefix, "__") {
		prefix += "__"
	}
	if strings.HasPrefix(name, prefix) {
		return name
	}
	return prefix + name
}

func normalizeXAITools(body []byte) []byte {
	shouldFold := xaiShouldFoldNamespaceTools(body, false)
	declarations, _ := buildXAIToolDeclarationsWithFold(body, shouldFold)
	return normalizeXAIToolsWithDeclarations(body, declarations, shouldFold)
}

func normalizeXAIToolsWithDeclarations(body []byte, declarations *xaiToolDeclarations, shouldFold bool) []byte {
	keepImageGeneration := xaiSupportsNativeImageGeneration(gjson.GetBytes(body, "model").String())
	original := body
	normalizeAtPath := func(path string) bool {
		tools := gjson.GetBytes(body, path)
		if !tools.IsArray() {
			return true
		}
		filtered, changed, ok := normalizeXAIToolArray(tools, "", declarations, keepImageGeneration, shouldFold)
		if !ok {
			return false
		}
		if !changed {
			return true
		}
		updated, errSet := sjson.SetRawBytes(body, path, filtered)
		if errSet != nil {
			return false
		}
		body = updated
		return true
	}
	if !normalizeAtPath("tools") {
		return original
	}
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for index, item := range input.Array() {
			if item.Get("type").String() != "additional_tools" {
				continue
			}
			if !normalizeAtPath(fmt.Sprintf("input.%d.tools", index)) {
				return original
			}
		}
	}
	return body
}

func normalizeXAIToolArray(tools gjson.Result, namespace string, declarations *xaiToolDeclarations, keepImageGeneration bool, shouldFold bool) ([]byte, bool, bool) {
	filtered := make([][]byte, 0, len(tools.Array()))
	changed := false
	for _, tool := range tools.Array() {
		if tool.Get("type").String() == xaiNamespaceToolType {
			changed = true
			if shouldFold {
				if dispatcher := buildXAINamespaceDispatcherTool(tool); len(dispatcher) > 0 {
					filtered = append(filtered, dispatcher)
				}
				continue
			}
			nestedNamespace := tool.Get("name").String()
			for _, nestedTool := range tool.Get("tools").Array() {
				raw, nestedChanged, ok := normalizeXAIToolWithDeclaration(nestedTool, nestedNamespace, declarations, keepImageGeneration)
				if !ok {
					return nil, false, false
				}
				changed = changed || nestedChanged
				if len(raw) > 0 {
					filtered = append(filtered, raw)
				}
			}
			continue
		}
		raw, toolChanged, ok := normalizeXAIToolWithDeclaration(tool, namespace, declarations, keepImageGeneration)
		if !ok {
			return nil, false, false
		}
		changed = changed || toolChanged
		if len(raw) > 0 {
			filtered = append(filtered, raw)
		}
	}
	if !changed {
		return nil, false, true
	}
	return xaiJoinRawJSONArray(filtered), true, true
}

func xaiJoinRawJSONArray(items [][]byte) []byte {
	var joined bytes.Buffer
	joined.WriteByte('[')
	for index, item := range items {
		if index > 0 {
			joined.WriteByte(',')
		}
		joined.Write(item)
	}
	joined.WriteByte(']')
	return joined.Bytes()
}

func normalizeXAITool(tool gjson.Result) ([]byte, bool, bool) {
	return normalizeXAIToolWithDeclaration(tool, "", nil, false)
}

// xaiUnsupportedWebSearchFields are OpenAI Responses web_search options that the
// xAI Responses API rejects with "Argument not supported". Options xAI accepts,
// such as filters, user_location and search_content_types, are forwarded as is.
var xaiUnsupportedWebSearchFields = [...]string{"external_web_access", "search_context_size"}

// stripUnsupportedXAIWebSearchFields removes web_search options that xAI rejects
// and reports which ones were removed.
func stripUnsupportedXAIWebSearchFields(raw []byte) ([]byte, []string, bool) {
	var removed []string
	for _, field := range xaiUnsupportedWebSearchFields {
		if !gjson.GetBytes(raw, field).Exists() {
			continue
		}
		updated, errDel := sjson.DeleteBytes(raw, field)
		if errDel != nil {
			return nil, nil, false
		}
		raw = updated
		removed = append(removed, field)
	}
	return raw, removed, true
}

func normalizeXAIToolWithDeclaration(tool gjson.Result, namespace string, declarations *xaiToolDeclarations, keepImageGeneration bool) ([]byte, bool, bool) {
	toolType := tool.Get("type").String()
	changed := false
	if toolType == xaiToolSearchType {
		return nil, true, true
	}
	if toolType == xaiImageGenerationToolType && !keepImageGeneration {
		return nil, true, true
	}
	raw := []byte(tool.Raw)
	schemaTool := tool
	if toolType == xaiCustomToolType {
		if tool.Get("name").String() == "apply_patch" {
			return nil, true, true
		}
		updatedTool, errSet := sjson.SetBytes(raw, "type", xaiFunctionToolType)
		if errSet != nil {
			return nil, false, false
		}
		raw = updatedTool
		schemaTool = gjson.ParseBytes(raw)
		toolType = xaiFunctionToolType
		changed = true
	}
	if toolType == xaiWebSearchToolType {
		updatedTool, removed, ok := stripUnsupportedXAIWebSearchFields(raw)
		if !ok {
			return nil, false, false
		}
		if len(removed) > 0 {
			raw = updatedTool
			changed = true
			log.Debugf("xai: removed unsupported web_search fields %v", removed)
		}
	}
	if toolType == xaiFunctionToolType && !tool.Get("parameters").Exists() {
		updatedTool, errSet := sjson.SetRawBytes(raw, "parameters", []byte(`{"type":"object","properties":{}}`))
		if errSet != nil {
			return nil, false, false
		}
		raw = updatedTool
		schemaTool = gjson.ParseBytes(raw)
		changed = true
	}
	if toolType == xaiFunctionToolType || toolType == xaiCustomToolType {
		if rawParams := schemaTool.Get("parameters"); rawParams.Exists() {
			inlinedParams := xaiInlineLocalRefs(rawParams.Raw)
			if inlinedParams != rawParams.Raw {
				if updated, errSet := sjson.SetRawBytes(raw, "parameters", []byte(inlinedParams)); errSet == nil {
					if inlinedDefs := gjson.GetBytes(updated, "parameters.$defs"); inlinedDefs.Exists() {
						updated, _ = sjson.DeleteBytes(updated, "parameters.$defs")
					}
					if inlinedDefinitions := gjson.GetBytes(updated, "parameters.definitions"); inlinedDefinitions.Exists() {
						updated, _ = sjson.DeleteBytes(updated, "parameters.definitions")
					}
					raw = updated
					schemaTool = gjson.ParseBytes(raw)
					changed = true
				}
			}
		}
		updatedTool, schemaChanged, ok := normalizeXAIObjectRootUnionBranchTypes(raw)
		if !ok {
			return nil, false, false
		}
		if schemaChanged {
			raw = updatedTool
			schemaTool = gjson.ParseBytes(raw)
			changed = true
		}
		if xaiFunctionParametersNeedSimplification(schemaTool, namespace) {
			updatedTool, errSet := sjson.SetRawBytes(raw, "parameters", []byte(xaiSafeFunctionParameters))
			if errSet != nil {
				return nil, false, false
			}
			raw = updatedTool
			changed = true
		}
	}
	originalType := tool.Get("type").String()
	if declarations != nil && (originalType == xaiFunctionToolType || originalType == xaiCustomToolType) {
		identity := xaiToolIdentity{namespace: namespace, name: tool.Get("name").String(), toolType: originalType}
		if declaration, ok := declarations.byOriginal[identity]; ok && declaration.effectiveName != identity.name {
			updatedTool, errSet := sjson.SetBytes(raw, "name", declaration.effectiveName)
			if errSet != nil {
				return nil, false, false
			}
			raw = updatedTool
			changed = true
		}
	}
	return raw, changed, true
}

func promoteXAIAdditionalTools(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	inputItems := input.Array()
	remainingInput := make([]json.RawMessage, 0, len(inputItems))
	promotedTools := make([]json.RawMessage, 0)
	for _, item := range inputItems {
		if item.Get("type").String() != "additional_tools" {
			remainingInput = append(remainingInput, json.RawMessage(item.Raw))
			continue
		}
		for _, tool := range item.Get("tools").Array() {
			promotedTools = append(promotedTools, json.RawMessage(tool.Raw))
		}
	}
	if len(remainingInput) == len(inputItems) {
		return body
	}
	rawInput, errMarshal := json.Marshal(remainingInput)
	if errMarshal != nil {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "input", rawInput)
	if errSet != nil || len(promotedTools) == 0 {
		return updated
	}
	tools := make([]json.RawMessage, 0, len(gjson.GetBytes(updated, "tools").Array())+len(promotedTools))
	for _, tool := range gjson.GetBytes(updated, "tools").Array() {
		tools = append(tools, json.RawMessage(tool.Raw))
	}
	tools = append(tools, promotedTools...)
	rawTools, errMarshal := json.Marshal(tools)
	if errMarshal != nil {
		return body
	}
	updated, errSet = sjson.SetRawBytes(updated, "tools", rawTools)
	if errSet != nil {
		return body
	}
	return updated
}

// normalizeXAIForcedWebSearchToolChoice rewrites web_search choices into a
// ModelToolChoice variant accepted by xAI chat-proxy.
func normalizeXAIForcedWebSearchToolChoice(body []byte) []byte {
	return normalizeXAIForcedHostedToolChoice(body, xaiWebSearchToolType)
}

// normalizeXAIForcedImageGenerationToolChoice rewrites image_generation choices
// into a ModelToolChoice variant accepted by xAI chat-proxy.
func normalizeXAIForcedImageGenerationToolChoice(body []byte) []byte {
	return normalizeXAIForcedHostedToolChoice(body, xaiImageGenerationToolType)
}

// normalizeXAIForcedHostedToolChoice rewrites choices for a hosted tool (web_search
// or image_generation) into a ModelToolChoice variant accepted by xAI chat-proxy.
// `{type: <toolType>}` becomes the string "required" and the tools list is reduced
// to that hosted tool so later x_search injection cannot broaden the restriction.
// An allowed_tools list that only names that hosted tool becomes the original mode
// ("auto" or "required") and is likewise reduced to that hosted tool. Mixed lists
// drop the hosted tool entry so the remaining hosted/function choices can still
// deserialize.
func normalizeXAIForcedHostedToolChoice(body []byte, toolType string) []byte {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() {
		return body
	}
	choiceType := strings.TrimSpace(choice.Get("type").String())
	if choiceType == toolType {
		body = xaiKeepOnlyHostedTools(body, toolType)
		return xaiSetToolChoiceString(body, "required")
	}
	if choiceType != "allowed_tools" {
		return body
	}
	allowed := choice.Get("tools")
	if !allowed.IsArray() {
		return body
	}
	filtered := make([][]byte, 0, len(allowed.Array()))
	stripped := false
	for _, tool := range allowed.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == toolType {
			stripped = true
			continue
		}
		filtered = append(filtered, []byte(tool.Raw))
	}
	if !stripped {
		return body
	}
	if len(filtered) == 0 {
		mode := strings.TrimSpace(choice.Get("mode").String())
		if mode != "auto" {
			mode = "required"
		}
		body = xaiKeepOnlyHostedTools(body, toolType)
		return xaiSetToolChoiceString(body, mode)
	}
	updated, errSet := sjson.SetRawBytes(body, "tool_choice.tools", xaiJoinRawJSONArray(filtered))
	if errSet != nil {
		return body
	}
	return updated
}

func xaiKeepOnlyHostedTools(body []byte, toolType string) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	kept := make([][]byte, 0, 1)
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == toolType {
			kept = append(kept, []byte(tool.Raw))
		}
	}
	if len(kept) == 0 || len(kept) == len(tools.Array()) {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "tools", xaiJoinRawJSONArray(kept))
	if errSet != nil {
		return body
	}
	return updated
}

func xaiToolChoiceRequiresHostedToolOnly(body []byte, toolType string) bool {
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.Type != gjson.String {
		return false
	}
	switch choice.String() {
	case "required", "auto":
	default:
		return false
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) != toolType {
			return false
		}
	}
	return true
}

func xaiToolChoiceRequiresImageGenerationOnly(body []byte) bool {
	return xaiToolChoiceRequiresHostedToolOnly(body, xaiImageGenerationToolType)
}

func xaiToolChoiceRequiresWebSearchOnly(body []byte) bool {
	return xaiToolChoiceRequiresHostedToolOnly(body, xaiWebSearchToolType)
}

func xaiToolChoiceRequiresHostedToolOnlyAny(body []byte) bool {
	return xaiToolChoiceRequiresImageGenerationOnly(body) || xaiToolChoiceRequiresWebSearchOnly(body)
}

func xaiSetToolChoiceString(body []byte, value string) []byte {
	updated, errSet := sjson.SetBytes(body, "tool_choice", value)
	if errSet != nil {
		return body
	}
	return updated
}

func pruneXAIOrphanedToolChoice(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.Exists() {
		return body
	}
	available := collectXAIAvailableToolChoiceKeys(body)
	if choice.Type == gjson.String {
		return body
	}
	if !choice.IsObject() {
		return body
	}
	choiceType := strings.TrimSpace(choice.Get("type").String())
	switch choiceType {
	case "allowed_tools":
		return pruneXAIAllowedToolsChoice(body, available)
	default:
		if choiceType == "" {
			return body
		}
		if xaiToolChoiceMatchesAvailable(choice, available) {
			return body
		}
		body, _ = sjson.DeleteBytes(body, "tool_choice")
		return body
	}
}

func pruneXAIAllowedToolsChoice(body []byte, available map[xaiToolChoiceKey]struct{}) []byte {
	allowed := gjson.GetBytes(body, "tool_choice.tools")
	if !allowed.Exists() || !allowed.IsArray() {
		body, _ = sjson.DeleteBytes(body, "tool_choice")
		return body
	}
	allowedItems := allowed.Array()
	filtered := make([][]byte, 0, len(allowedItems))
	changed := false
	for _, tool := range allowedItems {
		if !xaiToolChoiceMatchesAvailable(tool, available) {
			changed = true
			continue
		}
		filtered = append(filtered, []byte(tool.Raw))
	}
	if !changed {
		return body
	}
	if len(filtered) == 0 {
		body, _ = sjson.DeleteBytes(body, "tool_choice")
		return body
	}
	body, _ = sjson.SetRawBytes(body, "tool_choice.tools", xaiJoinRawJSONArray(filtered))
	return body
}

type xaiToolChoiceKey struct {
	toolType string
	name     string
}

func collectXAIAvailableToolChoiceKeys(body []byte) map[xaiToolChoiceKey]struct{} {
	keys := make(map[xaiToolChoiceKey]struct{})
	collect := func(tools gjson.Result) {
		if !tools.IsArray() {
			return
		}
		for _, tool := range tools.Array() {
			toolType := strings.TrimSpace(tool.Get("type").String())
			if toolType == "" {
				continue
			}
			key := xaiToolChoiceKey{toolType: toolType}
			if toolType == xaiFunctionToolType || toolType == xaiCustomToolType {
				key.name = strings.TrimSpace(tool.Get("name").String())
				if key.name == "" {
					continue
				}
			}
			keys[key] = struct{}{}
		}
	}
	collect(gjson.GetBytes(body, "tools"))
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() == "additional_tools" {
				collect(item.Get("tools"))
			}
		}
	}
	return keys
}

func xaiToolChoiceMatchesAvailable(choice gjson.Result, available map[xaiToolChoiceKey]struct{}) bool {
	toolType := strings.TrimSpace(choice.Get("type").String())
	if toolType == "" {
		return false
	}
	key := xaiToolChoiceKey{toolType: toolType}
	if toolType == xaiFunctionToolType || toolType == xaiCustomToolType {
		key.name = strings.TrimSpace(choice.Get("name").String())
		if key.name == "" {
			return false
		}
	}
	_, ok := available[key]
	return ok
}

func ensureXAINativeXSearchTool(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	if !xaiRequestHasNativeXSearch(body) {
		tools := gjson.GetBytes(body, "tools")
		if !tools.Exists() || !tools.IsArray() {
			body, _ = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"x_search"}]`))
		} else {
			body, _ = sjson.SetRawBytes(body, "tools.-1", xaiXSearchToolJSON)
		}
	}
	return ensureXAINativeXSearchAllowedTools(body)
}

func ensureXAINativeXSearchAllowedTools(body []byte) []byte {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() || choice.Get("type").String() != "allowed_tools" {
		return body
	}
	allowed := choice.Get("tools")
	if !allowed.Exists() || !allowed.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tool_choice.tools", []byte(`[{"type":"x_search"}]`))
		return body
	}
	for _, tool := range allowed.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == xaiXSearchToolType {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tool_choice.tools.-1", xaiXSearchToolJSON)
	return body
}

// xaiDeclarationIsDispatcher reports whether declarations track name as a folded
// namespace dispatcher — the local analogue of upstream's namespaceTools[name]
// membership check, which only ever matches dispatcher entries for a bare name.
func xaiDeclarationIsDispatcher(declarations *xaiToolDeclarations, name string) bool {
	if declarations == nil {
		return false
	}
	declaration, ok := declarations.byEffectiveName[name]
	return ok && declaration.isDispatcher
}

// xaiHasClientWebSearchFunction reports whether body declares a client function
// or custom tool named "web_search" (without namespace), excluding folded namespace dispatchers.
func xaiHasClientWebSearchFunction(body []byte, declarations *xaiToolDeclarations) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		toolType := strings.TrimSpace(tool.Get("type").String())
		name := strings.TrimSpace(tool.Get("name").String())
		if (toolType == xaiFunctionToolType || toolType == xaiCustomToolType) && name == xaiWebSearchToolType {
			if xaiDeclarationIsDispatcher(declarations, name) {
				continue
			}
			return true
		}
	}
	return false
}

// xaiBodyHasToolNamed reports whether any tool in tools or input has the given name.
func xaiBodyHasToolNamed(body []byte, name string) bool {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("name").String()) == name {
				return true
			}
			if nested := tool.Get("tools"); nested.IsArray() {
				for _, child := range nested.Array() {
					if strings.TrimSpace(child.Get("name").String()) == name {
						return true
					}
				}
			}
		}
	}
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			if strings.TrimSpace(item.Get("name").String()) == name {
				return true
			}
		}
	}
	return false
}

// xaiResolveClientWebSearchAlias picks a candidate alias that does not collide
// with any client tool already present in the request.
func xaiResolveClientWebSearchAlias(body []byte) string {
	candidate := xaiClientWebSearchAlias
	if !xaiBodyHasToolNamed(body, candidate) {
		return candidate
	}
	for i := 1; ; i++ {
		next := fmt.Sprintf("%s_%d", xaiClientWebSearchAlias, i)
		if !xaiBodyHasToolNamed(body, next) {
			return next
		}
	}
}

// aliasXAIClientWebSearchInput renames client-declared function calls named
// "web_search" to alias in replayed input history.
func aliasXAIClientWebSearchInput(body []byte, alias string, declarations *xaiToolDeclarations) []byte {
	if !gjson.ValidBytes(body) || alias == "" {
		return body
	}
	if xaiDeclarationIsDispatcher(declarations, xaiWebSearchToolType) {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	for idx, item := range input.Array() {
		itemType := strings.TrimSpace(item.Get("type").String())
		itemName := strings.TrimSpace(item.Get("name").String())
		itemNamespace := strings.TrimSpace(item.Get("namespace").String())
		if (itemType == "function_call" || itemType == "custom_tool_call" || itemType == "function_call_output") &&
			itemName == xaiWebSearchToolType && itemNamespace == "" {
			body, _ = sjson.SetBytes(body, fmt.Sprintf("input.%d.name", idx), alias)
		}
	}
	return body
}

// aliasXAIClientWebSearchFunction renames client-declared function tools named
// "web_search" to a non-colliding alias in tools declarations, tool_choice,
// and replayed input history to prevent xAI from hijacking them into hosted
// server-side search. Namespace dispatcher tools named "web_search" are untouched.
func aliasXAIClientWebSearchFunction(body []byte, alias string, declarations *xaiToolDeclarations) []byte {
	if !gjson.ValidBytes(body) || alias == "" {
		return body
	}
	// 1. Alias in tools (excluding namespace dispatchers)
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for idx, tool := range tools.Array() {
			toolType := strings.TrimSpace(tool.Get("type").String())
			name := strings.TrimSpace(tool.Get("name").String())
			if (toolType == xaiFunctionToolType || toolType == xaiCustomToolType) && name == xaiWebSearchToolType {
				if xaiDeclarationIsDispatcher(declarations, name) {
					continue
				}
				body, _ = sjson.SetBytes(body, fmt.Sprintf("tools.%d.name", idx), alias)
			}
		}
	}

	// 2. Alias in tool_choice (only when unnamespaced and not a namespace dispatcher)
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.IsObject() {
		if fnName := choice.Get("function.name"); fnName.Exists() && strings.TrimSpace(fnName.String()) == xaiWebSearchToolType {
			if strings.TrimSpace(choice.Get("function.namespace").String()) == "" {
				if !xaiDeclarationIsDispatcher(declarations, xaiWebSearchToolType) {
					body, _ = sjson.SetBytes(body, "tool_choice.function.name", alias)
				}
			}
		}
		if name := choice.Get("name"); name.Exists() && strings.TrimSpace(name.String()) == xaiWebSearchToolType {
			if strings.TrimSpace(choice.Get("namespace").String()) == "" {
				choiceType := strings.TrimSpace(choice.Get("type").String())
				if choiceType == xaiFunctionToolType || choiceType == "tool" {
					if !xaiDeclarationIsDispatcher(declarations, xaiWebSearchToolType) {
						body, _ = sjson.SetBytes(body, "tool_choice.name", alias)
					}
				}
			}
		}
		if allowed := choice.Get("tools"); allowed.IsArray() {
			for idx, allowedTool := range allowed.Array() {
				if strings.TrimSpace(allowedTool.Get("namespace").String()) != "" {
					continue
				}
				if xaiDeclarationIsDispatcher(declarations, xaiWebSearchToolType) {
					continue
				}
				allowedType := strings.TrimSpace(allowedTool.Get("type").String())
				allowedName := strings.TrimSpace(allowedTool.Get("name").String())
				if (allowedType == xaiFunctionToolType || allowedType == "tool") && allowedName == xaiWebSearchToolType {
					body, _ = sjson.SetBytes(body, fmt.Sprintf("tool_choice.tools.%d.name", idx), alias)
				} else if allowedName == xaiWebSearchToolType && allowedType != xaiWebSearchToolType {
					body, _ = sjson.SetBytes(body, fmt.Sprintf("tool_choice.tools.%d.name", idx), alias)
				}
			}
		}
	}

	// 3. Alias in input (conversation history, only when unnamespaced)
	return aliasXAIClientWebSearchInput(body, alias, declarations)
}

func xaiRequestHasNativeXSearch(body []byte) bool {
	if gjson.GetBytes(body, `tools.#(type=="x_search")`).Exists() {
		return true
	}
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() == "additional_tools" {
				if item.Get(`tools.#(type=="x_search")`).Exists() {
					return true
				}
			}
		}
	}
	return false
}

func normalizeXAIToolChoiceForTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if !hasTools {
		input := gjson.GetBytes(body, "input")
		if input.Exists() && input.IsArray() {
			for _, item := range input.Array() {
				additionalTools := item.Get("tools")
				if item.Get("type").String() == "additional_tools" && additionalTools.IsArray() && len(additionalTools.Array()) > 0 {
					hasTools = true
					break
				}
			}
		}
	}
	if hasTools {
		return body
	}
	if tools.Exists() {
		body, _ = sjson.DeleteBytes(body, "tools")
	}
	if gjson.GetBytes(body, "tool_choice").Exists() {
		body, _ = sjson.DeleteBytes(body, "tool_choice")
	}
	if gjson.GetBytes(body, "parallel_tool_calls").Exists() {
		body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
	}
	return body
}

func normalizeXAIToolChoices(body []byte, declarations *xaiToolDeclarations) []byte {
	body = normalizeXAIToolChoiceAtPath(body, "tool_choice", declarations)
	for index := range gjson.GetBytes(body, "tool_choice.tools").Array() {
		body = normalizeXAIToolChoiceAtPath(body, fmt.Sprintf("tool_choice.tools.%d", index), declarations)
	}
	return body
}

func normalizeXAIToolChoiceAtPath(body []byte, path string, declarations *xaiToolDeclarations) []byte {
	if declarations == nil {
		return body
	}
	choice := gjson.GetBytes(body, path)
	if !choice.IsObject() {
		return body
	}
	identity := xaiToolIdentity{
		namespace: choice.Get("namespace").String(),
		name:      choice.Get("name").String(),
		toolType:  choice.Get("type").String(),
	}
	declaration, ok := declarations.byOriginal[identity]
	if !ok && identity.namespace == "" {
		candidate, found := declarations.byEffectiveName[identity.name]
		if found && (identity.toolType == candidate.original.toolType || identity.toolType == candidate.effectiveType) {
			declaration, ok = candidate, true
		}
	}
	if !ok {
		return body
	}
	updated, errSet := sjson.SetBytes(body, path+".name", declaration.effectiveName)
	if errSet != nil {
		return body
	}
	updated, errSet = sjson.SetBytes(updated, path+".type", declaration.effectiveType)
	if errSet != nil {
		return body
	}
	updated, errSet = sjson.DeleteBytes(updated, path+".namespace")
	if errSet != nil {
		return body
	}
	return updated
}

func xaiHasFunctionToolNamed(body []byte, name string) bool {
	if name == "" {
		return false
	}
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == xaiFunctionToolType && tool.Get("name").String() == name {
				return true
			}
		}
	}
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() == "additional_tools" {
				for _, tool := range item.Get("tools").Array() {
					if tool.Get("type").String() == xaiFunctionToolType && tool.Get("name").String() == name {
						return true
					}
				}
			}
		}
	}
	return false
}

func clampXAIToolsLimit(body []byte, maxTools int, declarations *xaiToolDeclarations) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || len(tools.Array()) <= maxTools {
		return body
	}
	allTools := tools.Array()
	var dispatcherTools []json.RawMessage
	var regularTools []json.RawMessage
	for _, tool := range allTools {
		name := strings.TrimSpace(tool.Get("name").String())
		if declarations != nil {
			if decl, ok := declarations.byEffectiveName[name]; ok && decl.isDispatcher {
				dispatcherTools = append(dispatcherTools, json.RawMessage(tool.Raw))
				continue
			}
		}
		regularTools = append(regularTools, json.RawMessage(tool.Raw))
	}

	capped := make([]json.RawMessage, 0, maxTools)
	if len(dispatcherTools) >= maxTools {
		capped = append(capped, dispatcherTools[:maxTools]...)
	} else {
		capped = append(capped, dispatcherTools...)
		remainingSlots := maxTools - len(dispatcherTools)
		if len(regularTools) > remainingSlots {
			capped = append(capped, regularTools[:remainingSlots]...)
		} else {
			capped = append(capped, regularTools...)
		}
	}

	raw, errMarshal := json.Marshal(capped)
	if errMarshal != nil {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "tools", raw)
	if errSet != nil {
		return body
	}
	return normalizeXAIToolChoiceForTools(updated)
}

func normalizeXAINamespaceToolChoice(body []byte) []byte {
	return normalizeXAINamespaceToolChoiceWithFold(body, xaiShouldFoldNamespaceTools(body, false))
}

func normalizeXAINamespaceToolChoiceWithFold(body []byte, shouldFold bool) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	mutatePath := func(path string) bool {
		toolChoice := gjson.GetBytes(body, path)
		if !toolChoice.IsObject() || toolChoice.Get("type").String() != xaiFunctionToolType {
			return true
		}
		namespaceName := strings.TrimSpace(toolChoice.Get("namespace").String())
		toolName := strings.TrimSpace(toolChoice.Get("name").String())
		if namespaceName == "" {
			return true
		}
		qualifiedName := qualifyXAINamespaceToolName(namespaceName, toolName)
		var targetName string
		if xaiHasFunctionToolNamed(body, namespaceName) {
			targetName = namespaceName
		} else if xaiHasFunctionToolNamed(body, qualifiedName) {
			targetName = qualifiedName
		} else if shouldFold {
			targetName = namespaceName
		} else {
			targetName = qualifiedName
		}
		if targetName == "" {
			return true
		}
		updated, errSet := sjson.SetBytes(body, path+".name", targetName)
		if errSet != nil {
			return false
		}
		updated, errDelete := sjson.DeleteBytes(updated, path+".namespace")
		if errDelete != nil {
			return false
		}
		body = updated
		return true
	}
	if !mutatePath("tool_choice") {
		return body
	}
	for index := range gjson.GetBytes(body, "tool_choice.tools").Array() {
		if !mutatePath(fmt.Sprintf("tool_choice.tools.%d", index)) {
			return body
		}
	}
	return body
}

func normalizeXAIInputNamespaceToolCalls(body []byte) []byte {
	return normalizeXAIInputNamespaceToolCallsWithFold(body, xaiShouldFoldNamespaceTools(body, false))
}

func normalizeXAIInputNamespaceToolCallsWithFold(body []byte, shouldFold bool) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	for index, item := range input.Array() {
		if item.Get("type").String() != "function_call" {
			continue
		}
		namespaceName := strings.TrimSpace(item.Get("namespace").String())
		toolName := strings.TrimSpace(item.Get("name").String())
		if namespaceName == "" {
			continue
		}
		qualifiedName := qualifyXAINamespaceToolName(namespaceName, toolName)
		var isFolded bool
		if xaiHasFunctionToolNamed(body, namespaceName) {
			isFolded = true
		} else if xaiHasFunctionToolNamed(body, qualifiedName) {
			isFolded = false
		} else {
			isFolded = shouldFold
		}
		if isFolded {
			namePath := fmt.Sprintf("input.%d.name", index)
			namespacePath := fmt.Sprintf("input.%d.namespace", index)
			argsPath := fmt.Sprintf("input.%d.arguments", index)

			dispatcherArgs := map[string]any{
				"name": toolName,
			}
			if rawArgs := item.Get("arguments").String(); rawArgs != "" {
				if gjson.Valid(rawArgs) {
					dispatcherArgs["arguments"] = json.RawMessage(rawArgs)
				} else {
					dispatcherArgs["arguments"] = rawArgs
				}
			}
			encodedArgs, errMarshal := json.Marshal(dispatcherArgs)
			if errMarshal != nil {
				continue
			}

			updated, errSet := sjson.SetBytes(body, namePath, namespaceName)
			if errSet != nil {
				continue
			}
			updated, errSet = sjson.SetBytes(updated, argsPath, string(encodedArgs))
			if errSet != nil {
				continue
			}
			updated, errDelete := sjson.DeleteBytes(updated, namespacePath)
			if errDelete != nil {
				continue
			}
			body = updated
			continue
		}

		if qualifiedName == "" {
			continue
		}
		namePath := fmt.Sprintf("input.%d.name", index)
		namespacePath := fmt.Sprintf("input.%d.namespace", index)
		updated, errSet := sjson.SetBytes(body, namePath, qualifiedName)
		if errSet != nil {
			continue
		}
		updated, errDelete := sjson.DeleteBytes(updated, namespacePath)
		if errDelete != nil {
			continue
		}
		body = updated
	}
	return body
}

type xaiToolResponseRestorer struct {
	declarations      *xaiToolDeclarations
	declarationByID   map[string]xaiToolDeclaration
	dispatcherItemIDs map[string]string
}

func newXAIToolResponseRestorer(declarations *xaiToolDeclarations) *xaiToolResponseRestorer {
	return &xaiToolResponseRestorer{
		declarations:      declarations,
		declarationByID:   make(map[string]xaiToolDeclaration),
		dispatcherItemIDs: make(map[string]string),
	}
}

func (r *xaiToolResponseRestorer) apply(eventData []byte) []byte {
	if r == nil || r.declarations == nil || len(eventData) == 0 || !gjson.ValidBytes(eventData) {
		return eventData
	}
	switch eventType := gjson.GetBytes(eventData, "type").String(); eventType {
	case "response.output_item.added":
		item := gjson.GetBytes(eventData, "item")
		if item.Get("type").String() == "function_call" {
			name := strings.TrimSpace(item.Get("name").String())
			itemID := strings.TrimSpace(item.Get("id").String())
			if decl, ok := r.declarations.byEffectiveName[name]; ok && decl.isDispatcher {
				if itemID != "" {
					r.dispatcherItemIDs[itemID] = decl.original.namespace
				}
				eventData, _ = sjson.SetBytes(eventData, "item.namespace", decl.original.namespace)
			}
		}
		return r.restoreItemAtPath(eventData, "item")
	case "response.function_call_arguments.done":
		itemID := strings.TrimSpace(gjson.GetBytes(eventData, "item_id").String())
		if namespaceName, isDisp := r.dispatcherItemIDs[itemID]; isDisp {
			rawArgs := gjson.GetBytes(eventData, "arguments").String()
			if _, childArgs, ok := unwrapXAIDispatcherArguments(rawArgs, namespaceName, r.declarations); ok {
				updated, errSet := sjson.SetBytes(eventData, "arguments", string(childArgs))
				if errSet == nil {
					eventData = updated
				}
			}
		}
		return r.restoreCustomToolInputEvent(eventData)

	default:
		if item := gjson.GetBytes(eventData, "item"); item.IsObject() {
			eventData = r.restoreItemAtPath(eventData, "item")
		}
		for index := range gjson.GetBytes(eventData, "response.output").Array() {
			eventData = r.restoreItemAtPath(eventData, fmt.Sprintf("response.output.%d", index))
		}
		return r.restoreCustomToolInputEvent(eventData)
	}
}

func (r *xaiToolResponseRestorer) restoreItemAtPath(eventData []byte, path string) []byte {
	item := gjson.GetBytes(eventData, path)
	declaration, ok := r.declarations.matchResponseItem(item)
	if !ok {
		return eventData
	}
	if id := item.Get("id").String(); id != "" {
		r.declarationByID[id] = declaration
	}
	if declaration.isDispatcher {
		rawArgs := gjson.GetBytes(eventData, path+".arguments").String()
		childName, childArgs, unwrapped := unwrapXAIDispatcherArguments(rawArgs, declaration.original.namespace, r.declarations)
		if !unwrapped && childName == "" {
			childName = declaration.original.name
		}
		updated, errSet := sjson.SetBytes(eventData, path+".namespace", declaration.original.namespace)
		if errSet != nil {
			return eventData
		}
		if childName != "" {
			if updatedName, errSetName := sjson.SetBytes(updated, path+".name", childName); errSetName == nil {
				updated = updatedName
			}
		}
		if len(childArgs) > 0 {
			if updatedArgs, errSetArgs := sjson.SetBytes(updated, path+".arguments", string(childArgs)); errSetArgs == nil {
				updated = updatedArgs
			}
		}
		return updated
	}

	updated, errSet := sjson.SetBytes(eventData, path+".name", declaration.original.name)
	if errSet != nil {
		return eventData
	}
	if declaration.original.namespace != "" {
		updated, errSet = sjson.SetBytes(updated, path+".namespace", declaration.original.namespace)
		if errSet != nil {
			return eventData
		}
	}
	if declaration.original.toolType != xaiCustomToolType {
		return updated
	}
	updated, errSet = sjson.SetBytes(updated, path+".type", "custom_tool_call")
	if errSet != nil {
		return eventData
	}
	if arguments := gjson.GetBytes(updated, path+".arguments"); arguments.Exists() {
		updated, errSet = sjson.SetBytes(updated, path+".input", arguments.String())
		if errSet != nil {
			return eventData
		}
		updated, errSet = sjson.DeleteBytes(updated, path+".arguments")
		if errSet != nil {
			return eventData
		}
	}
	return updated
}
func (r *xaiToolResponseRestorer) restoreCustomToolInputEvent(eventData []byte) []byte {
	eventType := gjson.GetBytes(eventData, "type").String()
	if eventType != "response.function_call_arguments.delta" && eventType != "response.function_call_arguments.done" {
		return eventData
	}
	declaration, ok := r.declarationByID[gjson.GetBytes(eventData, "item_id").String()]
	if !ok || declaration.original.toolType != xaiCustomToolType {
		return eventData
	}
	if eventType == "response.function_call_arguments.delta" {
		updated, errSet := sjson.SetBytes(eventData, "type", "response.custom_tool_call_input.delta")
		if errSet != nil {
			return eventData
		}
		return updated
	}
	updated, errSet := sjson.SetBytes(eventData, "type", "response.custom_tool_call_input.done")
	if errSet != nil {
		return eventData
	}
	if arguments := gjson.GetBytes(updated, "arguments"); arguments.Exists() {
		updated, errSet = sjson.SetBytes(updated, "input", arguments.String())
		if errSet != nil {
			return eventData
		}
		updated, errSet = sjson.DeleteBytes(updated, "arguments")
		if errSet != nil {
			return eventData
		}
	}
	return updated
}

// restoreXAIClientWebSearchName rewrites the given alias
// back to the original client tool name "web_search" across event items and
// completed outputs. Only unnamespaced tool calls are restored so namespaced
// tools (e.g. acme.clientfn_web_search) are preserved untouched.
func restoreXAIClientWebSearchName(data []byte, alias string) []byte {
	if alias == "" || !bytes.Contains(data, []byte(alias)) {
		return data
	}
	if !gjson.ValidBytes(data) {
		return data
	}

	// Check item (e.g. response.output_item.added / response.output_item.done)
	if strings.TrimSpace(gjson.GetBytes(data, "item.namespace").String()) == "" {
		if strings.TrimSpace(gjson.GetBytes(data, "item.name").String()) == alias {
			data, _ = sjson.SetBytes(data, "item.name", xaiWebSearchToolType)
		}
		if strings.TrimSpace(gjson.GetBytes(data, "item.function.name").String()) == alias {
			data, _ = sjson.SetBytes(data, "item.function.name", xaiWebSearchToolType)
		}
	}

	// Check response.output array (e.g. response.completed / response.incomplete)
	respOutput := gjson.GetBytes(data, "response.output")
	if respOutput.IsArray() {
		for idx, item := range respOutput.Array() {
			if strings.TrimSpace(item.Get("namespace").String()) != "" {
				continue
			}
			if strings.TrimSpace(item.Get("name").String()) == alias {
				data, _ = sjson.SetBytes(data, fmt.Sprintf("response.output.%d.name", idx), xaiWebSearchToolType)
			}
			if strings.TrimSpace(item.Get("function.name").String()) == alias {
				data, _ = sjson.SetBytes(data, fmt.Sprintf("response.output.%d.function.name", idx), xaiWebSearchToolType)
			}
		}
	}

	// Check top-level output array (non-stream responses translated)
	topOutput := gjson.GetBytes(data, "output")
	if topOutput.IsArray() {
		for idx, item := range topOutput.Array() {
			if strings.TrimSpace(item.Get("namespace").String()) != "" {
				continue
			}
			if strings.TrimSpace(item.Get("name").String()) == alias {
				data, _ = sjson.SetBytes(data, fmt.Sprintf("output.%d.name", idx), xaiWebSearchToolType)
			}
			if strings.TrimSpace(item.Get("function.name").String()) == alias {
				data, _ = sjson.SetBytes(data, fmt.Sprintf("output.%d.function.name", idx), xaiWebSearchToolType)
			}
		}
	}

	// Check top-level name
	if strings.TrimSpace(gjson.GetBytes(data, "namespace").String()) == "" {
		if strings.TrimSpace(gjson.GetBytes(data, "name").String()) == alias {
			data, _ = sjson.SetBytes(data, "name", xaiWebSearchToolType)
		}
	}

	return data
}

func unwrapXAIDispatcherArguments(rawArgs string, namespaceName string, declarations *xaiToolDeclarations) (string, []byte, bool) {
	if !gjson.Valid(rawArgs) {
		return "", nil, false
	}
	argsParsed := gjson.Parse(rawArgs)
	nameField := argsParsed.Get("name")
	if !nameField.Exists() || nameField.Type != gjson.String {
		return "", nil, false
	}
	childName := strings.TrimSpace(nameField.String())
	if childName == "" {
		return "", nil, false
	}

	if namespaceName != "" {
		if declaration, exists := declarations.byEffectiveName[qualifyXAINamespaceToolName(namespaceName, childName)]; exists && declaration.isDispatcher {
			return "", nil, false
		}
	} else {
		isChildOfDispatcher := false
		if declarations != nil {
			for _, decl := range declarations.byEffectiveName {
				if decl.isDispatcher && (decl.original.name == childName || decl.original.namespace == childName) {
					isChildOfDispatcher = true
					break
				}
			}
		}
		if !isChildOfDispatcher && !argsParsed.Get("arguments").Exists() {
			return "", nil, false
		}
	}

	var childArgs []byte
	if argsField := argsParsed.Get("arguments"); argsField.Exists() {
		if argsField.Type == gjson.String {
			childArgs = []byte(argsField.String())
		} else {
			childArgs = []byte(argsField.Raw)
		}
	} else {
		cleaned, errDel := sjson.DeleteBytes([]byte(rawArgs), "name")
		if errDel == nil && len(cleaned) > 0 && string(cleaned) != "{}" {
			childArgs = cleaned
		} else {
			childArgs = []byte("{}")
		}
	}
	if len(childArgs) == 0 {
		childArgs = []byte("{}")
	}
	return childName, childArgs, true
}

func normalizeXAIObjectRootUnionBranchTypes(tool []byte) ([]byte, bool, bool) {
	parameters := gjson.GetBytes(tool, "parameters")
	if !parameters.IsObject() {
		return tool, false, true
	}
	if schemaType := parameters.Get("type"); !xaiSchemaTypeIsObjectOnly(schemaType) {
		return tool, false, true
	}

	changed := false
	for _, unionName := range []string{"oneOf", "anyOf"} {
		union := parameters.Get(unionName)
		if !union.IsArray() {
			continue
		}
		for index, branch := range union.Array() {
			if !branch.IsObject() || branch.Get("type").Exists() || branch.Get("$ref").Exists() {
				continue
			}
			updated, errSet := sjson.SetBytes(tool, fmt.Sprintf("parameters.%s.%d.type", unionName, index), "object")
			if errSet != nil {
				return nil, false, false
			}
			tool = updated
			changed = true
		}
	}
	return tool, changed, true
}

func xaiSchemaTypeIsObjectOnly(schemaType gjson.Result) bool {
	if schemaType.Type == gjson.String {
		return schemaType.String() == "object"
	}
	if schemaType.IsArray() {
		for _, item := range schemaType.Array() {
			if item.String() != "object" {
				return false
			}
		}
		return len(schemaType.Array()) > 0
	}
	return true
}

func isXAICodexAppAutomationUpdate(toolName, namespaceName string) bool {
	cleanNamespace := strings.TrimPrefix(strings.TrimSpace(namespaceName), "mcp__")
	cleanTool := strings.TrimPrefix(strings.TrimSpace(toolName), "mcp__")
	if strings.EqualFold(cleanTool, xaiAutomationUpdateToolName) && (strings.EqualFold(cleanNamespace, xaiCodexAppNamespaceName) || strings.EqualFold(cleanNamespace, "codex_apps")) {
		return true
	}
	if strings.EqualFold(cleanTool, xaiCodexAppNamespaceName+"__"+xaiAutomationUpdateToolName) || strings.EqualFold(cleanTool, "codex_apps__"+xaiAutomationUpdateToolName) {
		return true
	}
	return false
}

func xaiFunctionParametersNeedSimplification(tool gjson.Result, namespaceName string) bool {
	toolType := tool.Get("type").String()
	isFunction := toolType == xaiFunctionToolType || toolType == xaiCustomToolType
	if !isFunction {
		return false
	}

	toolName := strings.TrimSpace(tool.Get("name").String())
	if isFunction && isXAICodexAppAutomationUpdate(toolName, namespaceName) {
		return true
	}

	parameters := tool.Get("parameters")
	if !parameters.IsObject() {
		return false
	}

	for _, unionName := range []string{"oneOf", "anyOf"} {
		union := parameters.Get(unionName)
		if !union.IsArray() {
			continue
		}
		for _, branch := range union.Array() {
			if branch.Get("$ref").Exists() || !xaiSchemaTypeIsObjectOnly(branch.Get("type")) {
				return true
			}
		}
	}

	return false
}

func xaiInlineLocalRefs(jsonStr string) string {
	if !strings.Contains(jsonStr, `"$ref"`) {
		return jsonStr
	}

	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return jsonStr
	}

	resolved := xaiResolveLocalRefs(root, root, make(map[string]bool))
	out, err := json.Marshal(resolved)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func xaiResolveLocalRefs(root, value any, active map[string]bool) any {
	switch node := value.(type) {
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = xaiResolveLocalRefs(root, item, active)
		}
		return out
	case map[string]any:
		ref, hasRef := node["$ref"].(string)
		if hasRef && strings.HasPrefix(ref, "#/") {
			if target, ok := xaiResolveJSONPointer(root, ref); ok {
				if active[ref] {
					return xaiCyclicRefFallback(node, target, ref)
				}
				active[ref] = true
				resolvedTarget := xaiResolveLocalRefs(root, target, active)
				delete(active, ref)
				if targetMap, okTarget := resolvedTarget.(map[string]any); okTarget {
					out := make(map[string]any, len(targetMap)+len(node))
					for key, item := range targetMap {
						out[key] = item
					}
					for key, item := range node {
						if key == "$ref" {
							continue
						}
						out[key] = xaiResolveLocalRefs(root, item, active)
					}
					return out
				}
			}
		}

		out := make(map[string]any, len(node))
		for key, item := range node {
			out[key] = xaiResolveLocalRefs(root, item, active)
		}
		return out
	default:
		return value
	}
}

func xaiResolveJSONPointer(root any, ref string) (any, bool) {
	current := root
	for _, rawPart := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(rawPart, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[part]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func xaiCyclicRefFallback(node map[string]any, target any, ref string) map[string]any {
	out := make(map[string]any, len(node)+2)
	if targetMap, ok := target.(map[string]any); ok {
		for _, key := range []string{"type", "nullable", "description"} {
			if value, exists := targetMap[key]; exists {
				out[key] = value
			}
		}
	}
	for key, value := range node {
		if key != "$ref" {
			out[key] = value
		}
	}
	name := xaiRefName(ref)
	hint := "See: " + name
	if description, _ := out["description"].(string); description != "" {
		out["description"] = description + " (" + hint + ")"
	} else {
		out["description"] = hint
	}
	return out
}

func xaiRefName(ref string) string {
	if index := strings.LastIndex(ref, "/"); index >= 0 && index+1 < len(ref) {
		return strings.ReplaceAll(strings.ReplaceAll(ref[index+1:], "~1", "/"), "~0", "~")
	}
	return ref
}

func (d *xaiToolDeclarations) matchResponseItem(item gjson.Result) (xaiToolDeclaration, bool) {
	if d == nil || item.Get("type").String() != "function_call" {
		return xaiToolDeclaration{}, false
	}
	name := item.Get("name").String()
	if namespace := item.Get("namespace").String(); namespace != "" {
		for _, toolType := range []string{xaiFunctionToolType, xaiCustomToolType} {
			if declaration, ok := d.byOriginal[xaiToolIdentity{namespace: namespace, name: name, toolType: toolType}]; ok {
				return declaration, true
			}
		}
		if declaration, ok := d.byEffectiveName[namespace]; ok && declaration.isDispatcher {
			return declaration, true
		}
		return xaiToolDeclaration{}, false
	}
	declaration, ok := d.byEffectiveName[name]
	if !ok || declaration.effectiveType != xaiFunctionToolType {
		return xaiToolDeclaration{}, false
	}
	return declaration, true
}

type xaiInternalXSearchResponseFilter struct {
	declarations         *xaiToolDeclarations
	droppedOutputIndexes map[int64]struct{}
	droppedItemIDs       map[string]struct{}
}

func newXAIInternalXSearchResponseFilter(declarations *xaiToolDeclarations) *xaiInternalXSearchResponseFilter {
	return &xaiInternalXSearchResponseFilter{
		declarations:         declarations,
		droppedOutputIndexes: make(map[int64]struct{}),
		droppedItemIDs:       make(map[string]struct{}),
	}
}

func xaiIsInternalXSearchToolName(name string) bool {
	switch name {
	case "x_user_search", "x_semantic_search", "x_keyword_search", "x_thread_fetch":
		return true
	default:
		return false
	}
}

func xaiIsInternalXSearchCallID(callID string) bool {
	return strings.HasPrefix(callID, "xs_call")
}

func (f *xaiInternalXSearchResponseFilter) isInternalCall(item gjson.Result) bool {
	itemType := item.Get("type").String()
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return false
	}
	if !xaiIsInternalXSearchToolName(item.Get("name").String()) {
		return false
	}
	if f != nil && f.declarations != nil && item.Get("namespace").String() != "" {
		if _, declared := f.declarations.matchResponseItem(item); declared {
			return false
		}
	}
	if xaiIsInternalXSearchCallID(item.Get("call_id").String()) {
		return true
	}
	if f != nil && f.declarations != nil {
		if _, declared := f.declarations.matchResponseItem(item); declared {
			return false
		}
	}
	return true
}

func (f *xaiInternalXSearchResponseFilter) apply(eventData []byte) []byte {
	if f == nil || len(eventData) == 0 || !gjson.ValidBytes(eventData) {
		return eventData
	}
	if item := gjson.GetBytes(eventData, "item"); f.isInternalCall(item) {
		f.recordDroppedItem(eventData, item)
		return nil
	}
	eventData = f.filterCompletedOutput(eventData)
	if f.referencesDroppedItem(eventData) {
		return nil
	}
	return f.compactOutputIndex(eventData)
}

func (f *xaiInternalXSearchResponseFilter) recordDroppedItem(eventData []byte, item gjson.Result) {
	if outputIndex := gjson.GetBytes(eventData, "output_index"); outputIndex.Exists() {
		f.droppedOutputIndexes[outputIndex.Int()] = struct{}{}
	}
	for _, path := range []string{"id", "call_id"} {
		if id := item.Get(path).String(); id != "" {
			f.droppedItemIDs[id] = struct{}{}
		}
	}
}

func (f *xaiInternalXSearchResponseFilter) referencesDroppedItem(eventData []byte) bool {
	if outputIndex := gjson.GetBytes(eventData, "output_index"); outputIndex.Exists() {
		if _, dropped := f.droppedOutputIndexes[outputIndex.Int()]; dropped {
			return true
		}
	}
	for _, path := range []string{"item_id", "call_id"} {
		id := gjson.GetBytes(eventData, path).String()
		if _, dropped := f.droppedItemIDs[id]; id != "" && dropped {
			return true
		}
	}
	return false
}

func (f *xaiInternalXSearchResponseFilter) compactOutputIndex(eventData []byte) []byte {
	outputIndex := gjson.GetBytes(eventData, "output_index")
	if !outputIndex.Exists() {
		return eventData
	}
	original := outputIndex.Int()
	removedBefore := int64(0)
	for dropped := range f.droppedOutputIndexes {
		if dropped < original {
			removedBefore++
		}
	}
	if removedBefore == 0 {
		return eventData
	}
	updated, errSet := sjson.SetBytes(eventData, "output_index", original-removedBefore)
	if errSet != nil {
		return eventData
	}
	return updated
}

func (f *xaiInternalXSearchResponseFilter) filterCompletedOutput(eventData []byte) []byte {
	output := gjson.GetBytes(eventData, "response.output")
	if !output.IsArray() {
		return eventData
	}
	items := make([]json.RawMessage, 0, len(output.Array()))
	changed := false
	for _, item := range output.Array() {
		if f.isInternalCall(item) {
			changed = true
			continue
		}
		items = append(items, json.RawMessage(item.Raw))
	}
	if !changed {
		return eventData
	}
	rawOutput, errMarshal := json.Marshal(items)
	if errMarshal != nil {
		return eventData
	}
	updated, errSet := sjson.SetRawBytes(eventData, "response.output", rawOutput)
	if errSet != nil {
		return eventData
	}
	return updated
}

func normalizeXAIInputReasoningItems(body []byte, allowRouterSuffix bool) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}

	updated := body
	for i, item := range input.Array() {
		if item.Get("type").String() != "reasoning" {
			continue
		}
		// A router-served conversation replays values that carry the router's
		// private suffix. xAI decrypts encrypted_content and answers 400
		// "Could not decrypt" for a mutated value, so the suffix leaves before the
		// request does unless this upstream is the router that minted it.
		encryptedContentPath := fmt.Sprintf("input.%d.encrypted_content", i)
		if encryptedContent := gjson.GetBytes(updated, encryptedContentPath); encryptedContent.Type == gjson.String && !allowRouterSuffix {
			if info, errInspect := signature.InspectGrokEncryptedContent(encryptedContent.String()); errInspect == nil && info.RouterSuffix != "" {
				if stripped, errSet := sjson.SetBytes(updated, encryptedContentPath, stripRouterSuffix(encryptedContent.String(), info.RouterSuffix)); errSet == nil {
					updated = stripped
				}
			}
		}
		contentPath := fmt.Sprintf("input.%d.content", i)
		if content := gjson.GetBytes(updated, contentPath); content.Exists() && content.Type == gjson.Null {
			updatedBody, errDel := sjson.DeleteBytes(updated, contentPath)
			if errDel != nil {
				return body
			}
			updated = updatedBody
		}
		if encryptedContent := gjson.GetBytes(updated, encryptedContentPath); encryptedContent.Exists() && encryptedContent.Type == gjson.Null {
			updatedBody, errDel := sjson.DeleteBytes(updated, encryptedContentPath)
			if errDel != nil {
				return body
			}
			updated = updatedBody
		}
	}
	return mergeAdjacentXAIInputReasoningSummaries(updated)
}

func mergeAdjacentXAIInputReasoningSummaries(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}

	changed := false
	items := make([]json.RawMessage, 0, len(input.Array()))
	for _, item := range input.Array() {
		if len(items) > 0 && canMergeXAIReasoningSummary(items[len(items)-1], item) {
			merged, ok := appendXAIReasoningSummary(items[len(items)-1], item.Get("summary").Array())
			if ok {
				items[len(items)-1] = json.RawMessage(merged)
				changed = true
				continue
			}
		}
		items = append(items, json.RawMessage(item.Raw))
	}
	if !changed {
		return body
	}

	rawInput, errMarshal := json.Marshal(items)
	if errMarshal != nil {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "input", rawInput)
	if errSet != nil {
		return body
	}
	return updated
}

func canMergeXAIReasoningSummary(previous json.RawMessage, current gjson.Result) bool {
	previousItem := gjson.ParseBytes(previous)
	if previousItem.Get("type").String() != "reasoning" || current.Get("type").String() != "reasoning" {
		return false
	}
	if !previousItem.Get("summary").IsArray() || !current.Get("summary").IsArray() {
		return false
	}
	if len(current.Get("summary").Array()) == 0 {
		return false
	}
	for name := range current.Map() {
		if name != "type" && name != "summary" {
			return false
		}
	}
	return true
}

func appendXAIReasoningSummary(previous json.RawMessage, currentSummary []gjson.Result) ([]byte, bool) {
	updated := []byte(previous)
	summary := gjson.GetBytes(updated, "summary")
	if !summary.IsArray() {
		return previous, false
	}
	nextIndex := len(summary.Array())
	for i, item := range currentSummary {
		updatedItem, errSet := sjson.SetRawBytes(updated, fmt.Sprintf("summary.%d", nextIndex+i), []byte(item.Raw))
		if errSet != nil {
			return previous, false
		}
		updated = updatedItem
	}
	return updated, true
}

func xaiNormalizeReasoningSummaryEventLine(line []byte, eventName string) []byte {
	if eventName == "" && bytes.HasPrefix(line, xaiEventTag) {
		eventName = strings.TrimSpace(string(line[len(xaiEventTag):]))
	}
	eventName = xaiNormalizeReasoningSummaryEventName(eventName)
	if eventName == "" {
		return bytes.Clone(line)
	}
	return []byte("event: " + eventName)
}

func xaiNormalizeReasoningSummaryEventName(eventName string) string {
	switch eventName {
	case "response.reasoning_text.delta":
		return "response.reasoning_summary_text.delta"
	case "response.reasoning_text.done":
		return "response.reasoning_summary_part.done"
	default:
		return eventName
	}
}

func xaiNormalizeReasoningSummaryData(eventData []byte) []byte {
	if len(eventData) == 0 || !gjson.ValidBytes(eventData) {
		return eventData
	}

	normalized := eventData
	switch gjson.GetBytes(normalized, "type").String() {
	case "response.reasoning_text.delta":
		normalized, _ = sjson.SetBytes(normalized, "type", "response.reasoning_summary_text.delta")
		normalized = xaiNormalizeReasoningSummaryIndex(normalized)
	case "response.reasoning_text.done":
		normalized, _ = sjson.SetBytes(normalized, "type", "response.reasoning_summary_part.done")
		normalized, _ = sjson.SetBytes(normalized, "part.type", "summary_text")
		if text := gjson.GetBytes(normalized, "text"); text.Exists() {
			normalized, _ = sjson.SetBytes(normalized, "part.text", text.String())
		}
		normalized, _ = sjson.DeleteBytes(normalized, "text")
		normalized = xaiNormalizeReasoningSummaryIndex(normalized)
	case "response.content_part.added":
		if gjson.GetBytes(normalized, "part.type").String() == "reasoning_text" {
			normalized, _ = sjson.SetBytes(normalized, "type", "response.reasoning_summary_part.added")
			normalized, _ = sjson.SetBytes(normalized, "part.type", "summary_text")
			normalized = xaiNormalizeReasoningSummaryIndex(normalized)
		}
	case "response.content_part.done":
		if gjson.GetBytes(normalized, "part.type").String() == "reasoning_text" {
			normalized, _ = sjson.SetBytes(normalized, "type", "response.reasoning_summary_part.done")
			normalized, _ = sjson.SetBytes(normalized, "part.type", "summary_text")
			normalized = xaiNormalizeReasoningSummaryIndex(normalized)
		}
	}

	if item := gjson.GetBytes(normalized, "item"); item.Exists() && item.Type == gjson.JSON {
		updatedItem := xaiNormalizeReasoningOutputItem([]byte(item.Raw))
		if !bytes.Equal(updatedItem, []byte(item.Raw)) {
			normalized, _ = sjson.SetRawBytes(normalized, "item", updatedItem)
		}
	}
	if output := gjson.GetBytes(normalized, "response.output"); output.IsArray() {
		updatedOutput, changed := xaiNormalizeReasoningOutputItems(output.Array())
		if changed {
			normalized, _ = sjson.SetRawBytes(normalized, "response.output", updatedOutput)
		}
	}
	return normalized
}

func xaiNormalizeReasoningSummaryDataEvents(eventData []byte) [][]byte {
	if len(eventData) == 0 || !gjson.ValidBytes(eventData) {
		return [][]byte{eventData}
	}
	if gjson.GetBytes(eventData, "type").String() != "response.reasoning_text.done" {
		return [][]byte{xaiNormalizeReasoningSummaryData(eventData)}
	}

	textDone, _ := sjson.SetBytes(eventData, "type", "response.reasoning_summary_text.done")
	textDone = xaiNormalizeReasoningSummaryIndex(textDone)
	partDone := xaiNormalizeReasoningSummaryData(eventData)
	return [][]byte{textDone, partDone}
}

func xaiNormalizeReasoningSummaryIndex(eventData []byte) []byte {
	contentIndex := gjson.GetBytes(eventData, "content_index")
	if contentIndex.Exists() && contentIndex.Raw != "" && !gjson.GetBytes(eventData, "summary_index").Exists() {
		eventData, _ = sjson.SetRawBytes(eventData, "summary_index", []byte(contentIndex.Raw))
	}
	eventData, _ = sjson.DeleteBytes(eventData, "content_index")
	return eventData
}

func xaiNormalizeReasoningOutputItems(items []gjson.Result) ([]byte, bool) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	changed := false
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		updatedItem := xaiNormalizeReasoningOutputItem([]byte(item.Raw))
		if !bytes.Equal(updatedItem, []byte(item.Raw)) {
			changed = true
		}
		buf.Write(updatedItem)
	}
	buf.WriteByte(']')
	return buf.Bytes(), changed
}

func xaiNormalizeReasoningOutputItem(item []byte) []byte {
	if !gjson.ValidBytes(item) || gjson.GetBytes(item, "type").String() != "reasoning" {
		return item
	}

	normalized := item
	if summary := gjson.GetBytes(normalized, "summary"); summary.IsArray() {
		updatedSummary, changed := xaiNormalizeReasoningSummaryItems(summary.Array())
		if changed {
			normalized, _ = sjson.SetRawBytes(normalized, "summary", updatedSummary)
		}
	}

	content := gjson.GetBytes(normalized, "content")
	if !content.IsArray() {
		return normalized
	}

	summaryItems := make([]gjson.Result, 0, len(content.Array()))
	for _, part := range content.Array() {
		if part.Get("type").String() == "reasoning_text" {
			summaryItems = append(summaryItems, part)
		}
	}
	if len(summaryItems) == 0 {
		return normalized
	}

	updatedSummary, _ := xaiNormalizeReasoningSummaryItems(summaryItems)
	normalized, _ = sjson.SetRawBytes(normalized, "summary", updatedSummary)
	normalized, _ = sjson.DeleteBytes(normalized, "content")
	return normalized
}

func xaiNormalizeReasoningSummaryItems(items []gjson.Result) ([]byte, bool) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	changed := false
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		itemRaw := []byte(item.Raw)
		if item.Get("type").String() == "reasoning_text" {
			var errSet error
			itemRaw, errSet = sjson.SetBytes(itemRaw, "type", "summary_text")
			if errSet == nil {
				changed = true
			}
		}
		buf.Write(itemRaw)
	}
	buf.WriteByte(']')
	return buf.Bytes(), changed
}

func removeXAIEncryptedReasoningInclude(body []byte) []byte {
	include := gjson.GetBytes(body, "include")
	if !include.Exists() || !include.IsArray() {
		return body
	}
	kept := make([]string, 0, len(include.Array()))
	for _, item := range include.Array() {
		value := strings.TrimSpace(item.String())
		if value == "" || value == "reasoning.encrypted_content" {
			continue
		}
		kept = append(kept, value)
	}
	body, _ = sjson.SetBytes(body, "include", kept)
	return body
}

// xaiGrokImageGenerationMinVersion is the first Grok line that accepts xAI's
// native Responses image_generation tool. Older conversation models still
// reject that hosted type, so the executor keeps stripping it there.
var xaiGrokImageGenerationMinVersion = xaiGrokVersion{major: 4, minor: 6}

type xaiGrokVersion struct {
	major int
	minor int
}

// xaiSupportsNativeImageGeneration reports whether the Grok model accepts
// xAI's native Responses image_generation tool. grok-4.20-* is an older
// product line whose dotted minor is not comparable to grok-4.6.
func xaiSupportsNativeImageGeneration(model string) bool {
	name := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" || !strings.HasPrefix(name, "grok-") {
		return false
	}
	rest := strings.TrimPrefix(name, "grok-")
	if rest == "4.20" || strings.HasPrefix(rest, "4.20-") {
		return false
	}
	ver, ok := xaiParseGrokVersionPrefix(rest)
	if !ok {
		return false
	}
	return xaiCompareGrokVersion(ver, xaiGrokImageGenerationMinVersion) >= 0
}

func xaiParseGrokVersionPrefix(rest string) (xaiGrokVersion, bool) {
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return xaiGrokVersion{}, false
	}
	major, err := strconv.Atoi(rest[:i])
	if err != nil {
		return xaiGrokVersion{}, false
	}
	if i == len(rest) || rest[i] != '.' {
		return xaiGrokVersion{major: major, minor: -1}, true
	}
	j := i + 1
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == i+1 {
		return xaiGrokVersion{major: major, minor: -1}, true
	}
	minor, err := strconv.Atoi(rest[i+1 : j])
	if err != nil {
		return xaiGrokVersion{}, false
	}
	return xaiGrokVersion{major: major, minor: minor}, true
}

func xaiCompareGrokVersion(a, b xaiGrokVersion) int {
	if a.major != b.major {
		if a.major < b.major {
			return -1
		}
		return 1
	}
	aMinor := a.minor
	if aMinor < 0 {
		aMinor = 0
	}
	bMinor := b.minor
	if bMinor < 0 {
		bMinor = 0
	}
	if aMinor < bMinor {
		return -1
	}
	if aMinor > bMinor {
		return 1
	}
	return 0
}

func xaiCollectOutputItemDone(eventData []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback *[][]byte) {
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

func xaiPatchCompletedOutput(eventData []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback [][]byte) []byte {
	eventData = helps.EnsureResponsesUsageDetails(eventData)
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

	outputArray := []byte("[]")
	var buf bytes.Buffer
	buf.WriteByte('[')
	wrote := false
	for _, idx := range indexes {
		if wrote {
			buf.WriteByte(',')
		}
		buf.Write(outputItemsByIndex[idx])
		wrote = true
	}
	for _, item := range outputItemsFallback {
		if wrote {
			buf.WriteByte(',')
		}
		buf.Write(item)
		wrote = true
	}
	buf.WriteByte(']')
	if wrote {
		outputArray = buf.Bytes()
	}

	patched, _ := sjson.SetRawBytes(eventData, "response.output", outputArray)
	return patched
}
