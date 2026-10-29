// Package handlers provides core API handler functionality for the CLI Proxy API server.
// It includes common types, client management, load balancing, and error handling
// shared across all API endpoint handlers (OpenAI, Claude, Gemini).
package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/clienterror"
	"github.com/therealtinhtute/llmhub/internal/interfaces"
	"github.com/therealtinhtute/llmhub/internal/logging"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/therealtinhtute/llmhub/internal/util"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	coreexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	coresession "github.com/therealtinhtute/llmhub/sdk/cliproxy/session"
	"github.com/therealtinhtute/llmhub/sdk/config"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"golang.org/x/net/context"
)

// ErrorResponse represents a standard error response format for the API.
// It contains a single ErrorDetail field.
type ErrorResponse struct {
	// Error contains detailed information about the error that occurred.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides specific information about an error that occurred.
// It includes a human-readable message, an error type, and an optional error code.
type ErrorDetail struct {
	// Message is a human-readable message providing more details about the error.
	Message string `json:"message"`

	// Type is the category of error that occurred (e.g., "invalid_request_error").
	Type string `json:"type"`

	// Code is a short code identifying the error, if applicable.
	Code string `json:"code,omitempty"`

	// Retryable optionally indicates whether a retry might fix the issue automatically.
	Retryable *bool `json:"retryable,omitempty"`
}

const idempotencyKeyMetadataKey = "idempotency_key"

const (
	defaultStreamingKeepAliveSeconds = 0
	defaultStreamingBootstrapRetries = 0
)

type pinnedAuthContextKey struct{}
type selectedAuthCallbackContextKey struct{}
type executionSessionContextKey struct{}
type disallowFreeAuthContextKey struct{}

// WithPinnedAuthID returns a child context that requests execution on a specific auth ID.
func WithPinnedAuthID(ctx context.Context, authID string) context.Context {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pinnedAuthContextKey{}, authID)
}

// WithSelectedAuthIDCallback returns a child context that receives the selected auth ID.
func WithSelectedAuthIDCallback(ctx context.Context, callback func(string)) context.Context {
	if callback == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, selectedAuthCallbackContextKey{}, callback)
}

// WithExecutionSessionID returns a child context tagged with a long-lived execution session ID.
func WithExecutionSessionID(ctx context.Context, sessionID string) context.Context {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionSessionContextKey{}, sessionID)
}

// WithDisallowFreeAuth returns a child context that requests skipping known free-tier credentials.
func WithDisallowFreeAuth(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, disallowFreeAuthContextKey{}, true)
}

// BuildErrorResponseBody builds an OpenAI-compatible JSON error response body.
// If errText is already valid JSON, it is returned as-is to preserve upstream error payloads.
func BuildErrorResponseBody(status int, errText string) []byte {
	return BuildErrorResponseBodyWithError(status, errText, nil)
}

// BuildErrorResponseBodyWithError builds an OpenAI-compatible JSON error response body,
// preserving structured classifications (such as terminal upstream auth failures and retryable flags)
// present in err.
// Ported from upstream CLIProxyAPI handlers.go (nested error details).
func BuildErrorResponseBodyWithError(status int, errText string, err error) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)

	if coreauth.IsTerminalAuthError(err) {
		message := errText
		if trimmed != "" && json.Valid([]byte(trimmed)) {
			var parsed map[string]any
			if errUnmarshal := json.Unmarshal([]byte(trimmed), &parsed); errUnmarshal == nil {
				if msg, ok := parsed["message"].(string); ok && msg != "" {
					message = msg
				} else if errMap, ok := parsed["error"].(map[string]any); ok {
					if msg, ok := errMap["message"].(string); ok && msg != "" {
						message = msg
					}
				}
			}
		}
		r := false
		payload, errMarshal := json.Marshal(ErrorResponse{
			Error: ErrorDetail{
				Message:   message,
				Type:      "authentication_error",
				Code:      "upstream_authentication_required",
				Retryable: &r,
			},
		})
		if errMarshal != nil {
			return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"authentication_error","code":"upstream_authentication_required","retryable":false}}`, message))
		}
		return payload
	}
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: errText,
			Type:    errType,
			Code:    code,
		},
	})
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText))
	}
	return payload
}

// StreamingKeepAliveInterval returns the SSE keep-alive interval for this server.
// Returning 0 disables keep-alives (default when unset).
func StreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := defaultStreamingKeepAliveSeconds
	if cfg != nil {
		seconds = cfg.Streaming.KeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// NonStreamingKeepAliveInterval returns the keep-alive interval for non-streaming responses.
// Returning 0 disables keep-alives (default when unset).
func NonStreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.NonStreamKeepAliveInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// StreamingBootstrapRetries returns how many times a streaming request may be retried before any bytes are sent.
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil {
		retries = cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

// PassthroughHeadersEnabled returns whether upstream response headers should be forwarded to clients.
// Default is false.
func PassthroughHeadersEnabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.PassthroughHeaders
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	var ginCtx *gin.Context
	if ctx != nil {
		ginCtx, _ = ctx.Value("gin").(*gin.Context)
	}

	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// Only include it if the client explicitly provides it.
	key := ""
	requestPath := ""
	if ginCtx != nil && ginCtx.Request != nil {
		key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
		requestPath = strings.TrimSpace(ginCtx.FullPath())
		if requestPath == "" && ginCtx.Request.URL != nil {
			requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
		}
	}

	meta := make(map[string]any)
	if key != "" {
		meta[idempotencyKeyMetadataKey] = key
	}
	if requestPath != "" {
		meta[coreexecutor.RequestPathMetadataKey] = requestPath
	}
	if pinnedAuthID := pinnedAuthIDFromContext(ctx); pinnedAuthID != "" {
		meta[coreexecutor.PinnedAuthMetadataKey] = pinnedAuthID
	}
	if selectedCallback := selectedAuthIDCallbackFromContext(ctx); selectedCallback != nil {
		meta[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	if executionSessionID := executionSessionIDFromContext(ctx); executionSessionID != "" {
		meta[coreexecutor.ExecutionSessionMetadataKey] = executionSessionID
	}
	if callerScope := requestCallerScope(ginCtx); callerScope != "" {
		meta[coreexecutor.CallerScopeMetadataKey] = callerScope
	}
	if disallowFreeAuthFromContext(ctx) {
		meta[coreexecutor.DisallowFreeAuthMetadataKey] = true
	}
	return meta
}

// requestCallerScope namespaces inferred session identities by the downstream
// caller that produced them, so two credentials sharing a session alias never
// share a replay ledger. It hashes the authenticated principal and never keeps
// the raw credential.
func requestCallerScope(ginCtx *gin.Context) string {
	if ginCtx == nil {
		return ""
	}
	value, exists := ginCtx.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	return coresession.CallerScope(fmt.Sprint(value))
}

func setReasoningEffortMetadata(meta map[string]any, handlerType, model string, rawJSON []byte) {
	if meta == nil {
		return
	}
	effort := thinking.ExtractReasoningEffort(rawJSON, handlerType, model)
	if effort == "" {
		return
	}
	meta[coreexecutor.ReasoningEffortMetadataKey] = effort
}

// headersFromContext extracts the original HTTP request headers from the gin context
// embedded in the provided context. This allows session affinity selectors to read
// client-provided session headers.
func headersFromContext(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.Request.Header.Clone()
	}
	return nil
}

func pinnedAuthIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(pinnedAuthContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func selectedAuthIDCallbackFromContext(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(selectedAuthCallbackContextKey{})
	if callback, ok := raw.(func(string)); ok && callback != nil {
		return callback
	}
	return nil
}

func executionSessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(executionSessionContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func disallowFreeAuthFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw, ok := ctx.Value(disallowFreeAuthContextKey{}).(bool)
	return ok && raw
}

// BaseAPIHandler contains the handlers for API endpoints.
// It holds a pool of clients to interact with the backend service and manages
// load balancing, client selection, and configuration.
type BaseAPIHandler struct {
	// runtimeMu guards Cfg against concurrent UpdateClients swaps while
	// request handlers read the configuration. Read the config only via
	// CurrentConfig; never mutate the returned pointer.
	runtimeMu sync.RWMutex

	// AuthManager manages auth lifecycle and execution in the new architecture.
	AuthManager *coreauth.Manager

	// Cfg holds the current application configuration.
	Cfg *config.SDKConfig
}

// NewBaseAPIHandlers creates a new API handlers instance.
// It takes a slice of clients and configuration as input.
//
// Parameters:
//   - cliClients: A slice of AI service clients
//   - cfg: The application configuration
//
// Returns:
//   - *BaseAPIHandler: A new API handlers instance
func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler {
	return &BaseAPIHandler{
		Cfg:         cfg,
		AuthManager: authManager,
	}
}

// UpdateClients updates the handlers' client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) {
	if h == nil {
		return
	}
	h.runtimeMu.Lock()
	h.Cfg = cfg
	h.runtimeMu.Unlock()
}

// CurrentConfig returns the current application configuration snapshot.
// The returned pointer is owned by the handler and must not be mutated.
func (h *BaseAPIHandler) CurrentConfig() *config.SDKConfig {
	if h == nil {
		return nil
	}
	h.runtimeMu.RLock()
	cfg := h.Cfg
	h.runtimeMu.RUnlock()
	return cfg
}

// GetAlt extracts the 'alt' parameter from the request query string.
// It checks both 'alt' and '$alt' parameters and returns the appropriate value.
//
// Parameters:
//   - c: The Gin context containing the HTTP request
//
// Returns:
//   - string: The alt parameter value, or empty string if it's "sse"
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string {
	var alt string
	var hasAlt bool
	alt, hasAlt = c.GetQuery("alt")
	if !hasAlt {
		alt, _ = c.GetQuery("$alt")
	}
	if alt == "sse" {
		return ""
	}
	return alt
}

// GetContextWithCancel creates a new context with cancellation capabilities.
// It embeds the Gin context and the API handler into the new context for later use.
// The returned cancel function also handles logging the API response if request logging is enabled.
//
// Parameters:
//   - handler: The API handler associated with the request.
//   - c: The Gin context of the current request.
//   - ctx: The parent context (caller values/deadlines are preserved; request context adds cancellation and request ID).
//
// Returns:
//   - context.Context: The new context with cancellation and embedded values.
//   - APIHandlerCancelFunc: A function to cancel the context and log the response.
func (h *BaseAPIHandler) GetContextWithCancel(handler interfaces.APIHandler, c *gin.Context, ctx context.Context) (context.Context, APIHandlerCancelFunc) {
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	var requestCtx context.Context
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}

	if requestCtx != nil && logging.GetRequestID(parentCtx) == "" {
		if requestID := logging.GetRequestID(requestCtx); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		} else if requestID = logging.GetGinRequestID(c); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		}
	}
	newCtx, cancel := context.WithCancel(parentCtx)
	if requestCtx != nil && coreexecutor.CodexCloakingDisabled(requestCtx) {
		newCtx = coreexecutor.WithCodexCloakingDisabled(newCtx)
	}

	endpoint := ""
	if c != nil && c.Request != nil {
		path := strings.TrimSpace(c.FullPath())
		if path == "" && c.Request.URL != nil {
			path = strings.TrimSpace(c.Request.URL.Path)
		}
		if path != "" {
			method := strings.TrimSpace(c.Request.Method)
			if method != "" {
				endpoint = method + " " + path
			} else {
				endpoint = path
			}
		}
	}
	if endpoint != "" {
		newCtx = logging.WithEndpoint(newCtx, endpoint)
	}
	if c != nil && c.Request != nil {
		clientMeta := logging.GetClientRequestMetadata(newCtx)
		clientMeta.ResolvedClientIP = strings.TrimSpace(c.ClientIP())
		newCtx = logging.WithClientRequestMetadata(newCtx, clientMeta)
	}
	newCtx = logging.WithResponseStatusHolder(newCtx)
	newCtx = logging.WithResponseHeadersHolder(newCtx)

	cancelCtx := newCtx
	if requestCtx != nil && requestCtx != parentCtx {
		go func() {
			select {
			case <-requestCtx.Done():
				cancel()
			case <-cancelCtx.Done():
			}
		}()
	}
	newCtx = context.WithValue(newCtx, "gin", c)
	newCtx = context.WithValue(newCtx, "handler", handler)
	return newCtx, func(params ...interface{}) {
		if c != nil {
			logging.SetResponseStatus(cancelCtx, c.Writer.Status())
		}
		if cfg := h.CurrentConfig(); cfg != nil && cfg.RequestLog && len(params) == 1 {
			if existing, exists := c.Get("API_RESPONSE"); exists {
				if existingBytes, ok := existing.([]byte); ok && len(bytes.TrimSpace(existingBytes)) > 0 {
					switch params[0].(type) {
					case error, string:
						cancel()
						return
					}
				}
			}

			var payload []byte
			switch data := params[0].(type) {
			case []byte:
				payload = data
			case error:
				if data != nil {
					payload = []byte(data.Error())
				}
			case string:
				payload = []byte(data)
			}
			if len(payload) > 0 {
				if existing, exists := c.Get("API_RESPONSE"); exists {
					if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
						trimmedPayload := bytes.TrimSpace(payload)
						if len(trimmedPayload) > 0 && bytes.Contains(existingBytes, trimmedPayload) {
							cancel()
							return
						}
					}
				}
				appendAPIResponse(c, payload)
			}
		}

		cancel()
	}
}

// StartNonStreamingKeepAlive emits blank lines every 5 seconds while waiting for a non-streaming response.
// It returns a stop function that must be called before writing the final response.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	if h == nil || c == nil {
		return func() {}
	}
	interval := NonStreamingKeepAliveInterval(h.CurrentConfig())
	if interval <= 0 {
		return func() {}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	stopChan := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
		wg.Wait()
	}
}

// appendAPIResponse preserves any previously captured API response and appends new data.
func appendAPIResponse(c *gin.Context, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}

	// Capture timestamp on first API response
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); !exists {
		c.Set("API_RESPONSE_TIMESTAMP", time.Now())
	}

	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			combined := make([]byte, 0, len(existingBytes)+len(data)+1)
			combined = append(combined, existingBytes...)
			if existingBytes[len(existingBytes)-1] != '\n' {
				combined = append(combined, '\n')
			}
			combined = append(combined, data...)
			c.Set("API_RESPONSE", combined)
			return
		}
	}

	c.Set("API_RESPONSE", bytes.Clone(data))
}

// ExecuteWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, false)
}

// ExecuteImageWithAuthManager executes an OpenAI-compatible image endpoint request.
func (h *BaseAPIHandler) ExecuteImageWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, true)
}

func (h *BaseAPIHandler) executeWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, allowImageModel bool) ([]byte, http.Header, *interfaces.ErrorMessage) {
	comboCandidates, isCombo := h.comboOrder(modelName)
	var providers []string
	var normalizedModel string
	var errMsg *interfaces.ErrorMessage
	if isCombo {
		// Combo models are virtual: their candidates define the providers and
		// upstream models, so the model-to-provider lookup is skipped.
		normalizedModel = modelName
	} else {
		providers, normalizedModel, errMsg = h.getRequestDetailsWithOptions(modelName, allowImageModel)
	}
	if errMsg != nil {
		return nil, nil, errMsg
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	headers := headersFromContext(ctx)
	lifecycleReq := &RequestLifecycleRequest{
		HandlerType:     handlerType,
		Model:           modelName,
		NormalizedModel: normalizedModel,
		Alt:             alt,
		Stream:          false,
		Payload:         payload,
		Headers:         cloneHeader(headers),
		Metadata:        cloneLifecycleMetadata(reqMeta),
	}
	plugin := requestLifecyclePlugin(ctx)
	if plugin != nil {
		decision, err := plugin.InterceptBefore(ctx, lifecycleReq)
		if err != nil {
			return nil, nil, lifecycleErrorMessage(err)
		}
		if decision != nil && decision.Termination != nil {
			if errMsg := lifecycleTerminationError(decision.Termination); errMsg != nil {
				return nil, nil, errMsg
			}
			body, addon := lifecycleTerminationPayload(decision.Termination)
			return body, addon, nil
		}
		payload, headers = applyLifecycleDecision(payload, headers, decision)
		lifecycleReq.Payload = payload
		lifecycleReq.Headers = cloneHeader(headers)
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
		Headers:         headers,
	}
	opts.Metadata = reqMeta
	var resp coreexecutor.Response
	var err error
	if isCombo {
		resp, err = h.executeWithComboFallback(ctx, req, opts, comboCandidates)
	} else {
		resp, err = h.AuthManager.Execute(ctx, providers, req, opts)
	}
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		errMsg := executionErrorMessage(err)
		if plugin != nil {
			_, hookErr := plugin.InterceptAfter(ctx, lifecycleReq, &RequestLifecycleResponse{Stream: false, Error: errMsg})
			if hookErr != nil {
				return nil, nil, lifecycleErrorMessage(hookErr)
			}
		}
		return nil, nil, errMsg
	}
	upstreamHeaders := http.Header(nil)
	if PassthroughHeadersEnabled(h.CurrentConfig()) {
		upstreamHeaders = FilterUpstreamHeaders(resp.Headers)
	}
	payload = resp.Payload
	if plugin != nil {
		decision, hookErr := plugin.InterceptAfter(ctx, lifecycleReq, &RequestLifecycleResponse{Stream: false, Payload: cloneBytes(payload), Headers: cloneHeader(upstreamHeaders)})
		if hookErr != nil {
			return nil, nil, lifecycleErrorMessage(hookErr)
		}
		if decision != nil && decision.Termination != nil {
			if errMsg := lifecycleTerminationError(decision.Termination); errMsg != nil {
				return nil, nil, errMsg
			}
			body, addon := lifecycleTerminationPayload(decision.Termination)
			return body, addon, nil
		}
		payload, upstreamHeaders = applyLifecycleDecision(payload, upstreamHeaders, decision)
	}
	return payload, upstreamHeaders, nil
}

// ExecuteCountWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
		Headers:         headersFromContext(ctx),
	}
	opts.Metadata = reqMeta
	resp, err := h.AuthManager.ExecuteCount(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		return nil, nil, executionErrorMessage(err)
	}
	if !PassthroughHeadersEnabled(h.CurrentConfig()) {
		return resp.Payload, nil, nil
	}
	return resp.Payload, FilterUpstreamHeaders(resp.Headers), nil
}

// ExecuteStreamWithAuthManager executes a streaming request via the core auth manager.
// This path is the only supported execution route.
// The returned http.Header carries upstream response headers captured before streaming begins.
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, false)
}

// ExecuteImageStreamWithAuthManager executes a streaming OpenAI-compatible image endpoint request.
func (h *BaseAPIHandler) ExecuteImageStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, true)
}

// nextStreamChunk reads the next executor chunk. canceled reports that the read was cut by the request
// context: either it was already done, or the channel closed together with it. A close on a live context
// is the end of the stream; a close that races the cancellation must not be, because the select picks at
// random when both are ready and the bare close would otherwise be reported as a completed stream
// (upstream CLIProxyAPI commits 90fe5b23d350, 712e4970bc98).
func nextStreamChunk(ctx context.Context, chunks <-chan coreexecutor.StreamChunk) (coreexecutor.StreamChunk, bool, bool) {
	var chunk coreexecutor.StreamChunk
	var ok bool
	if ctx != nil {
		select {
		case <-ctx.Done():
			return coreexecutor.StreamChunk{}, false, true
		case chunk, ok = <-chunks:
		}
	} else {
		chunk, ok = <-chunks
	}
	if !ok && ctx != nil && ctx.Err() != nil {
		return coreexecutor.StreamChunk{}, false, true
	}
	return chunk, ok, false
}

// canceledStreamResponse is the lifecycle response for a stream the request context cut short.
// executionErrorMessage maps context.Canceled to 499 (clienterror.HTTPStatusFromError), so a canceled
// stream is classified as a client cancellation rather than an actionable failure.
func canceledStreamResponse(ctx context.Context, headers http.Header) *RequestLifecycleResponse {
	err := context.Canceled
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
	}
	return &RequestLifecycleResponse{Stream: true, Headers: cloneHeader(headers), Error: executionErrorMessage(err)}
}

func (h *BaseAPIHandler) executeStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, allowImageModel bool) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	comboCandidates, isCombo := h.comboOrder(modelName)
	var providers []string
	var normalizedModel string
	var errMsg *interfaces.ErrorMessage
	if isCombo {
		// Combo models are virtual: their candidates define the providers and
		// upstream models, so the model-to-provider lookup is skipped.
		normalizedModel = modelName
	} else {
		providers, normalizedModel, errMsg = h.getRequestDetailsWithOptions(modelName, allowImageModel)
	}
	if errMsg != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	headers := headersFromContext(ctx)
	lifecycleReq := &RequestLifecycleRequest{
		HandlerType:     handlerType,
		Model:           modelName,
		NormalizedModel: normalizedModel,
		Alt:             alt,
		Stream:          true,
		Payload:         payload,
		Headers:         cloneHeader(headers),
		Metadata:        cloneLifecycleMetadata(reqMeta),
	}
	plugin := requestLifecyclePlugin(ctx)
	if plugin != nil {
		decision, hookErr := plugin.InterceptBefore(ctx, lifecycleReq)
		if hookErr != nil {
			errChan := make(chan *interfaces.ErrorMessage, 1)
			errChan <- lifecycleErrorMessage(hookErr)
			close(errChan)
			return nil, nil, errChan
		}
		if decision != nil && decision.Termination != nil {
			errChan := make(chan *interfaces.ErrorMessage, 1)
			if errMsg := lifecycleTerminationError(decision.Termination); errMsg != nil {
				errChan <- errMsg
			} else {
				body, _ := lifecycleTerminationPayload(decision.Termination)
				if len(body) > 0 {
					dataChan := make(chan []byte, 1)
					dataChan <- body
					close(dataChan)
					close(errChan)
					return dataChan, cloneHeader(decision.Termination.Headers), errChan
				}
			}
			close(errChan)
			return nil, cloneHeader(decision.Termination.Headers), errChan
		}
		payload, headers = applyLifecycleDecision(payload, headers, decision)
		lifecycleReq.Payload = payload
		lifecycleReq.Headers = cloneHeader(headers)
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          true,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
		Headers:         headers,
	}
	opts.Metadata = reqMeta
	// Combo requests start on the first candidate; the goroutine advances the
	// cursor on bootstrap failures below. Call-level failures of the initial
	// candidate switch here, before any bytes can reach the client.
	execProviders, execReq := providers, req
	comboIndex := 0
	if isCombo && len(comboCandidates) > 0 {
		execProviders = []string{comboCandidates[0].Provider}
		execReq.Model = comboCandidates[0].Model
	}
	streamResult, err := h.AuthManager.ExecuteStream(ctx, execProviders, execReq, opts)
	if err != nil && isCombo {
		for comboIndex+1 < len(comboCandidates) && bootstrapEligible(err) {
			comboIndex++
			execProviders = []string{comboCandidates[comboIndex].Provider}
			execReq.Model = comboCandidates[comboIndex].Model
			streamResult, err = h.AuthManager.ExecuteStream(ctx, execProviders, execReq, opts)
			if err == nil {
				break
			}
		}
		if err != nil && comboIndex+1 >= len(comboCandidates) && bootstrapEligible(err) {
			err = h.comboExhaustedError(comboCandidates)
		}
	}
	if err != nil {
		err = enrichAuthSelectionError(err, execProviders, normalizedModel)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errMsg := executionErrorMessage(err)
		if plugin != nil {
			_, hookErr := plugin.InterceptAfter(ctx, lifecycleReq, &RequestLifecycleResponse{Stream: true, Error: errMsg})
			if hookErr != nil {
				errChan <- lifecycleErrorMessage(hookErr)
				close(errChan)
				return nil, nil, errChan
			}
		}
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	passthroughHeadersEnabled := PassthroughHeadersEnabled(h.CurrentConfig())
	// Capture upstream headers from the initial connection synchronously before the goroutine starts.
	// Keep a mutable map so bootstrap retries can replace it before first payload is sent.
	var upstreamHeaders http.Header
	if passthroughHeadersEnabled {
		upstreamHeaders = cloneHeader(FilterUpstreamHeaders(streamResult.Headers))
		if upstreamHeaders == nil {
			upstreamHeaders = make(http.Header)
		}
	}
	chunks := streamResult.Chunks
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		defer close(dataChan)
		defer close(errChan)
		sentPayload := false
		bootstrapRetries := 0
		maxBootstrapRetries := StreamingBootstrapRetries(h.CurrentConfig())
		emitter := NewSafeStreamEmitter(func(chunk []byte) bool {
			if ctx == nil {
				dataChan <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case dataChan <- chunk:
				return true
			}
		})
		defer func() { _ = emitter.Close() }()

		sendErr := func(msg *interfaces.ErrorMessage) bool {
			if ctx == nil {
				errChan <- msg
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case errChan <- msg:
				return true
			}
		}

		sendData := func(chunk []byte) bool {
			result := emitter.Emit(chunk)
			if result.Err != nil {
				_ = sendErr(&interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: result.Err})
				return false
			}
			return result.Accepted
		}

		notifyAfter := func(resp *RequestLifecycleResponse) bool {
			if plugin == nil {
				return true
			}
			decision, hookErr := plugin.InterceptAfter(ctx, lifecycleReq, resp)
			if hookErr != nil {
				return sendErr(lifecycleErrorMessage(hookErr))
			}
			if decision == nil || decision.Termination == nil {
				return true
			}
			if errMsg := lifecycleTerminationError(decision.Termination); errMsg != nil {
				return sendErr(errMsg)
			}
			body, _ := lifecycleTerminationPayload(decision.Termination)
			if len(body) == 0 {
				return true
			}
			return sendData(body)
		}

	outer:
		for {
			for {
				chunk, ok, canceled := nextStreamChunk(ctx, chunks)
				if canceled {
					_ = notifyAfter(canceledStreamResponse(ctx, upstreamHeaders))
					return
				}
				if !ok {
					_ = notifyAfter(&RequestLifecycleResponse{Stream: true, Headers: cloneHeader(upstreamHeaders)})
					return
				}
				if chunk.Err != nil {
					streamErr := chunk.Err
					// Safe bootstrap recovery: if the upstream fails before any payload bytes are sent,
					// retry a few times (to allow auth rotation / transient recovery) and then switch to
					// the next combo candidate. A post-first-chunk failure never switches.
					if !sentPayload {
						for bootstrapEligible(streamErr) {
							if bootstrapRetries < maxBootstrapRetries {
								bootstrapRetries++
								retryResult, retryErr := h.AuthManager.ExecuteStream(ctx, execProviders, execReq, opts)
								if retryErr == nil {
									if passthroughHeadersEnabled {
										replaceHeader(upstreamHeaders, FilterUpstreamHeaders(retryResult.Headers))
									}
									chunks = retryResult.Chunks
									continue outer
								}
								streamErr = enrichAuthSelectionError(retryErr, execProviders, normalizedModel)
								continue
							}
							if isCombo && comboIndex+1 < len(comboCandidates) {
								comboIndex++
								execProviders = []string{comboCandidates[comboIndex].Provider}
								execReq.Model = comboCandidates[comboIndex].Model
								bootstrapRetries = 0
								retryResult, retryErr := h.AuthManager.ExecuteStream(ctx, execProviders, execReq, opts)
								if retryErr == nil {
									if passthroughHeadersEnabled {
										replaceHeader(upstreamHeaders, FilterUpstreamHeaders(retryResult.Headers))
									}
									chunks = retryResult.Chunks
									continue outer
								}
								streamErr = enrichAuthSelectionError(retryErr, execProviders, normalizedModel)
								continue
							}
							break
						}
						if isCombo && bootstrapEligible(streamErr) && comboIndex+1 >= len(comboCandidates) {
							streamErr = h.comboExhaustedError(comboCandidates)
						}
					}

					errMsg := executionErrorMessage(streamErr)
					if plugin != nil {
						decision, hookErr := plugin.InterceptAfter(ctx, lifecycleReq, &RequestLifecycleResponse{Stream: true, Headers: cloneHeader(upstreamHeaders), Error: errMsg})
						if hookErr != nil {
							_ = sendErr(lifecycleErrorMessage(hookErr))
							return
						}
						if decision != nil && decision.Termination != nil {
							if hookErrMsg := lifecycleTerminationError(decision.Termination); hookErrMsg != nil {
								_ = sendErr(hookErrMsg)
								return
							}
							body, _ := lifecycleTerminationPayload(decision.Termination)
							if len(body) > 0 {
								_ = sendData(body)
							}
							return
						}
					}
					_ = sendErr(errMsg)
					return
				}
				if len(chunk.Payload) > 0 {
					if handlerType == "openai-response" {
						if err := validateSSEDataJSON(chunk.Payload); err != nil {
							_ = sendErr(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
							return
						}
					}
					sentPayload = true
					if okSendData := sendData(cloneBytes(chunk.Payload)); !okSendData {
						return
					}
				}
			}
		}
	}()
	return dataChan, upstreamHeaders, errChan
}

func validateSSEDataJSON(chunk []byte) error {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if json.Valid(data) {
			continue
		}
		const max = 512
		preview := data
		if len(preview) > max {
			preview = preview[:max]
		}
		return fmt.Errorf("invalid SSE data JSON (len=%d): %q", len(data), preview)
	}
	return nil
}

func statusFromError(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
		if code := se.StatusCode(); code > 0 {
			return code
		}
	}
	return 0
}

// bootstrapEligible reports whether an upstream failure may be retried before
// any payload bytes were sent: auth-level failures (credential rotation) and
// transient server errors. A client error like 400 is never eligible.
func bootstrapEligible(err error) bool {
	status := statusFromError(err)
	if status == 0 {
		return true
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired,
		http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return status >= http.StatusInternalServerError
	}
}

// comboOrder returns the candidate list for a request when the model is a combo.
func (h *BaseAPIHandler) comboOrder(modelName string) ([]coreauth.ComboCandidate, bool) {
	if h == nil || h.AuthManager == nil || h.AuthManager.ComboResolver() == nil {
		return nil, false
	}
	baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
	return h.AuthManager.ComboResolver().Order(baseModel)
}

// executeWithComboFallback runs the request against each combo candidate in
// order. Retryable failures (auth-level or transient 5xx) move to the next
// candidate; 5xx failures with a cooldown of at most 5s sleep before switching.
// When every candidate fails, the returned error is a *coreauth.ComboExhaustedError.
func (h *BaseAPIHandler) executeWithComboFallback(ctx context.Context, req coreexecutor.Request, opts coreexecutor.Options, candidates []coreauth.ComboCandidate) (coreexecutor.Response, error) {
	for i, candidate := range candidates {
		req.Model = candidate.Model
		resp, err := h.AuthManager.Execute(ctx, []string{candidate.Provider}, req, opts)
		if err == nil {
			return resp, nil
		}
		status := statusFromError(err)
		if !bootstrapEligible(err) {
			return resp, err
		}
		if i+1 < len(candidates) && status >= http.StatusBadGateway && status <= http.StatusGatewayTimeout {
			if wait := comboCooldown(err); wait > 0 && wait <= 5*time.Second {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return resp, ctx.Err()
				case <-timer.C:
				}
			}
		}
	}
	return coreexecutor.Response{}, h.comboExhaustedError(candidates)
}

// comboCooldown extracts a provider-supplied retry hint from the error, when present.
func comboCooldown(err error) time.Duration {
	if ra, ok := err.(interface{ RetryAfter() *time.Duration }); ok {
		if d := ra.RetryAfter(); d != nil && *d > 0 {
			return *d
		}
	}
	return 0
}

// comboExhaustedError builds the 503 exhaustion error carrying the earliest
// cooldown reset across the candidates plus a human-readable reset string.
func (h *BaseAPIHandler) comboExhaustedError(candidates []coreauth.ComboCandidate) error {
	exhausted := &coreauth.ComboExhaustedError{Candidates: len(candidates)}
	if h != nil && h.AuthManager != nil {
		now := time.Now()
		if resetAt, found := h.AuthManager.EarliestComboReset(candidates, now); found {
			exhausted.ResetAt = resetAt
			exhausted.Reset = resetAt.Sub(now)
		}
	}
	return exhausted
}

func (h *BaseAPIHandler) getRequestDetails(modelName string) (providers []string, normalizedModel string, err *interfaces.ErrorMessage) {
	return h.getRequestDetailsWithOptions(modelName, false)
}

func (h *BaseAPIHandler) getRequestDetailsWithOptions(modelName string, allowImageModel bool) (providers []string, normalizedModel string, err *interfaces.ErrorMessage) {
	resolvedModelName := modelName
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			resolvedModelName = modelName
		} else {
			resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
			if initialSuffix.HasSuffix {
				resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
			} else {
				resolvedModelName = resolvedBase
			}
		}
	} else {
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			resolvedModelName = modelName
		} else {
			resolvedModelName = util.ResolveAutoModel(modelName)
		}
	}

	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)

	if strings.EqualFold(routeModelBaseName(baseModel), "gpt-image-2") && !allowImageModel {
		return nil, "", &interfaces.ErrorMessage{
			StatusCode: http.StatusServiceUnavailable,
			Error:      fmt.Errorf("model %s is only supported on /v1/images/generations and /v1/images/edits", routeModelBaseName(baseModel)),
		}
	}

	if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return []string{"home"}, resolvedModelName, nil
	}

	providers = util.GetProviderName(baseModel)
	// Fallback: if baseModel has no provider but differs from resolvedModelName,
	// try using the full model name. This handles edge cases where custom models
	// may be registered with their full suffixed name (e.g., "my-model(8192)").
	// Evaluated in Story 11.8: This fallback is intentionally preserved to support
	// custom model registrations that include thinking suffixes.
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}

	if len(providers) == 0 {
		return nil, "", &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("unknown provider for model %s", modelName)}
	}

	// The thinking suffix is preserved in the model name itself, so no
	// metadata-based configuration passing is needed.
	return providers, resolvedModelName, nil
}

func routeModelBaseName(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return strings.TrimSpace(model[idx+1:])
	}
	return model
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func replaceHeader(dst http.Header, src http.Header) {
	for key := range dst {
		delete(dst, key)
	}
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

func enrichAuthSelectionError(err error, providers []string, model string) error {
	if err == nil {
		return nil
	}

	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return err
	}

	code := strings.TrimSpace(authErr.Code)
	if code != "auth_not_found" && code != "auth_unavailable" {
		return err
	}

	providerText := strings.Join(providers, ",")
	if providerText == "" {
		providerText = "unknown"
	}
	modelText := strings.TrimSpace(model)
	if modelText == "" {
		modelText = "unknown"
	}

	baseMessage := strings.TrimSpace(authErr.Message)
	if baseMessage == "" {
		baseMessage = "no auth available"
	}
	detail := fmt.Sprintf("%s (providers=%s, model=%s)", baseMessage, providerText, modelText)

	// Clarify the most common alias confusion between Anthropic route names and internal provider keys.
	if strings.Contains(","+providerText+",", ",claude,") {
		detail += "; check Claude auth/key session and cooldown state via /v0/management/auth-files"
	}

	status := authErr.HTTPStatus
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}

	cause := errors.Unwrap(err)

	enriched := &coreauth.Error{
		Code:       authErr.Code,
		Message:    detail,
		Retryable:  authErr.Retryable,
		HTTPStatus: status,
	}
	// Ported from upstream CLIProxyAPI commit 6724a95851f9: typed error carriers
	// (e.g. authUnavailableError) keep their trusted retry deadline while the
	// enriched message replaces the public face.
	var carrier interface{ WithAuthError(*coreauth.Error) error }
	if errors.As(err, &carrier) && carrier != nil {
		return carrier.WithAuthError(enriched)
	}
	// Ported from upstream CLIProxyAPI commit aedc9e6a3987: keep the terminal
	// upstream-auth classification (and the wrapped cause) through enrichment so
	// response writers can emit the non-retryable contract.
	if coreauth.IsTerminalAuthError(err) {
		return coreauth.NewTerminalAuthError(enriched, cause)
	}
	if cause != nil {
		return coreauth.WithCause(enriched, cause)
	}
	return enriched
}

// ExecutionErrorMessage converts an execution error into an ErrorMessage,
// preserving marked direct-response bodies supplied by trusted components.
// Ported from upstream CLIProxyAPI sdk/api/handlers/handlers_execution.go.
func ExecutionErrorMessage(err error) *interfaces.ErrorMessage {
	return executionErrorMessage(err)
}

func executionErrorMessage(err error) *interfaces.ErrorMessage {
	status := http.StatusInternalServerError
	if code := clienterror.HTTPStatusFromError(err); code > 0 {
		status = code
	}
	type directResponseError interface {
		DirectResponse() bool
		ResponseBody() []byte
	}
	var direct directResponseError
	if errors.As(err, &direct) && direct != nil && direct.DirectResponse() {
		body := direct.ResponseBody()
		var headers http.Header
		if len(body) > 0 {
			contentType := http.DetectContentType(body)
			if json.Valid(body) {
				contentType = "application/json"
			}
			headers = http.Header{"Content-Type": []string{contentType}}
		}
		return &interfaces.ErrorMessage{
			StatusCode:     status,
			Error:          err,
			DirectResponse: true,
			Body:           body,
			Headers:        headers,
		}
	}
	var addon http.Header
	if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
		if hdr := he.Headers(); hdr != nil {
			addon = hdr.Clone()
		}
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
}

// WriteErrorResponse writes an error message to the response writer using the HTTP status embedded in the message.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.DirectResponse {
		writeDirectErrorResponse(c, status, msg)
		return
	}
	if msg != nil && msg.Error != nil {
		for _, value := range coreauth.SafeResponseHeaders(msg.Error).Values("Retry-After") {
			c.Writer.Header().Add("Retry-After", value)
		}
	}
	if msg != nil && msg.Addon != nil && PassthroughHeadersEnabled(h.CurrentConfig()) {
		for key, values := range msg.Addon {
			if len(values) == 0 {
				continue
			}
			c.Writer.Header().Del(key)
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
	}

	errText := http.StatusText(status)
	if msg != nil && msg.Error != nil {
		if v := strings.TrimSpace(msg.Error.Error()); v != "" {
			errText = v
		}
	}

	// Ported from upstream CLIProxyAPI commit aedc9e6a3987: propagate the
	// structured error (terminal auth classification, retryable flag) into the
	// response envelope instead of flattening to a generic body.
	var errCause error
	if msg != nil {
		errCause = msg.Error
	}
	body := BuildErrorResponseBodyWithError(status, errText, errCause)
	// Append first to preserve upstream response logs, then drop duplicate payloads if already recorded.
	var previous []byte
	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			previous = existingBytes
		}
	}
	appendAPIResponse(c, body)
	trimmedErrText := strings.TrimSpace(errText)
	trimmedBody := bytes.TrimSpace(body)
	if len(previous) > 0 {
		if (trimmedErrText != "" && bytes.Contains(previous, []byte(trimmedErrText))) ||
			(len(trimmedBody) > 0 && bytes.Contains(previous, trimmedBody)) {
			c.Set("API_RESPONSE", previous)
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

// writeDirectErrorResponse emits a preformatted downstream response supplied by
// a trusted in-process component (e.g. a marked Home refresh upstream error).
// Ported from upstream CLIProxyAPI sdk/api/handlers/handlers_errors.go.
func writeDirectErrorResponse(c *gin.Context, status int, msg *interfaces.ErrorMessage) {
	for key, values := range FilterUpstreamHeaders(msg.Headers) {
		if len(values) == 0 {
			continue
		}
		c.Writer.Header().Del(key)
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	body := bytes.Clone(msg.Body)
	appendAPIResponse(c, body)
	if !c.Writer.Written() && c.Writer.Header().Get("Content-Type") == "" {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage) {
	if cfg := h.CurrentConfig(); cfg != nil && cfg.RequestLog {
		if ginContext, ok := ctx.Value("gin").(*gin.Context); ok {
			if apiResponseErrors, isExist := ginContext.Get("API_RESPONSE_ERROR"); isExist {
				if slicesAPIResponseError, isOk := apiResponseErrors.([]*interfaces.ErrorMessage); isOk {
					slicesAPIResponseError = append(slicesAPIResponseError, err)
					ginContext.Set("API_RESPONSE_ERROR", slicesAPIResponseError)
				}
			} else {
				// Create new response data entry
				ginContext.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{err})
			}
		}
	}
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...interface{})
