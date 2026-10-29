// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the request logging middleware that captures comprehensive
// request and response data when enabled through configuration.
package middleware

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/therealtinhtute/llmhub/internal/logging"
	"github.com/therealtinhtute/llmhub/internal/util"
)

const (
	maxErrorOnlyCapturedRequestBodyBytes int64 = 1 << 20  // 1 MiB
	maxDeferredErrorRequestBodyBytes     int64 = 32 << 20 // 32 MiB
	maxCapturedZstdWindowBytes                 = 8 << 20  // 8 MiB: covers every zstd level up to 19
)

// RequestLoggingMiddleware creates a Gin middleware that logs HTTP requests and responses.
// It captures detailed information about the request and response, including headers and body,
// and uses the provided RequestLogger to record this data. When full request logging is disabled,
// body capture is limited to small known-size payloads to avoid large per-request memory spikes.
func RequestLoggingMiddleware(logger logging.RequestLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logger == nil {
			c.Next()
			return
		}

		if shouldSkipMethodForRequestLogging(c.Request) {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldLogRequest(path) {
			c.Next()
			return
		}

		loggerEnabled := logger.IsEnabled()

		// Capture request metadata only. The body is captured lazily, as the
		// handlers read it, so nothing is read before authentication and an
		// unauthenticated client cannot pin a descriptor with an incomplete body.
		requestInfo, err := captureRequestInfo(c)
		if err != nil {
			// Log error but continue processing
			// In a real implementation, you might want to use a proper logger here
			c.Next()
			return
		}
		capture := attachRequestBodyCapture(c.Request, shouldCaptureRequestBody(loggerEnabled, c.Request))

		// Create response writer wrapper
		wrapper := NewResponseWriterWrapper(c.Writer, logger, requestInfo)
		if !loggerEnabled {
			wrapper.logOnErrorOnly = true
		}
		c.Writer = wrapper
		attachWebsocketLogSources(c, logger, loggerEnabled)

		// Process the request
		c.Next()

		if capture != nil {
			requestInfo.Body = capture.capturedBody(c, c.Request.Header.Get("Content-Encoding"))
		}

		// Finalize logging after request processing
		if err = wrapper.Finalize(c); err != nil {
			// Log error but don't interrupt the response
			// In a real implementation, you might want to use a proper logger here
		}
	}
}

type fileBodySourceFactory interface {
	NewFileBodySource(prefix string) (*logging.FileBodySource, error)
}

func attachWebsocketLogSources(c *gin.Context, logger logging.RequestLogger, loggerEnabled bool) {
	if c == nil || !loggerEnabled || !isResponsesWebsocketUpgrade(c.Request) {
		return
	}
	factory, ok := logger.(fileBodySourceFactory)
	if !ok || factory == nil {
		return
	}
	if source, errSource := factory.NewFileBodySource("websocket-timeline"); errSource == nil {
		c.Set(logging.WebsocketTimelineSourceContextKey, source)
	}
	if source, errSource := factory.NewFileBodySource("api-websocket-timeline"); errSource == nil {
		c.Set(logging.APIWebsocketTimelineSourceContextKey, source)
	}
}

func shouldSkipMethodForRequestLogging(req *http.Request) bool {
	if req == nil {
		return true
	}
	if req.Method != http.MethodGet {
		return false
	}
	return !isResponsesWebsocketUpgrade(req)
}

func isResponsesWebsocketUpgrade(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if req.URL.Path != "/v1/responses" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

func shouldCaptureRequestBody(loggerEnabled bool, req *http.Request) bool {
	if loggerEnabled {
		return true
	}
	if req == nil || req.Body == nil {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		return false
	}
	if req.ContentLength <= 0 {
		return false
	}
	return req.ContentLength <= maxErrorOnlyCapturedRequestBodyBytes
}

// captureRequestInfo extracts request metadata: URL, method and headers. It does
// not touch the request body; body capture is installed separately so that the
// body is only read once a handler asks for it.
func captureRequestInfo(c *gin.Context) (*RequestInfo, error) {
	// Capture URL with sensitive query parameters masked
	maskedQuery := util.MaskSensitiveQuery(c.Request.URL.RawQuery)
	url := c.Request.URL.Path
	if maskedQuery != "" {
		url += "?" + maskedQuery
	}

	// Capture method
	method := c.Request.Method

	// Capture headers
	headers := make(map[string][]string)
	for key, values := range c.Request.Header {
		headers[key] = values
	}

	return &RequestInfo{
		URL:       url,
		Method:    method,
		Headers:   headers,
		RequestID: logging.GetGinRequestID(c),
		Timestamp: time.Now(),
	}, nil
}

// capturedRequestBody tees the request body into a bounded in-memory buffer as
// the handlers read it. Bytes past maxErrorOnlyCapturedRequestBodyBytes are
// passed through untouched but not captured.
type capturedRequestBody struct {
	source io.ReadCloser
	buf    bytes.Buffer
	read   int64
}

// attachRequestBodyCapture replaces req.Body with a tee that records at most
// maxErrorOnlyCapturedRequestBodyBytes. It returns nil when the body must not be
// captured, in which case the request is left exactly as it was.
func attachRequestBodyCapture(req *http.Request, capture bool) *capturedRequestBody {
	if !capture || req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	captureBody := &capturedRequestBody{source: req.Body}
	req.Body = captureBody
	return captureBody
}

func (c *capturedRequestBody) Read(p []byte) (int, error) {
	if c == nil || c.source == nil {
		return 0, io.EOF
	}
	n, errRead := c.source.Read(p)
	if n > 0 {
		c.read += int64(n)
		if remaining := maxErrorOnlyCapturedRequestBodyBytes - int64(c.buf.Len()); remaining > 0 {
			chunk := p[:n]
			if int64(len(chunk)) > remaining {
				chunk = chunk[:remaining]
			}
			_, _ = c.buf.Write(chunk)
		}
	}
	return n, errRead
}

func (c *capturedRequestBody) Close() error {
	if c == nil || c.source == nil {
		return nil
	}
	return c.source.Close()
}

// capturedBody returns the captured bytes decoded for logging. It is empty when
// the handlers never read the body, for example when the request was rejected
// before authentication.
func (c *capturedRequestBody) capturedBody(ctx *gin.Context, contentEncoding string) []byte {
	if c == nil || c.buf.Len() == 0 {
		return nil
	}
	return decodeCapturedRequestBodyForLogWithLimit(c.buf.Bytes(), contentEncoding, capturedBodyLogLimit(ctx))
}

// capturedBodyLogLimit bounds the decoded body even when no max-request-body-bytes
// is configured: a small zstd body can expand to gigabytes.
func capturedBodyLogLimit(ctx *gin.Context) int64 {
	if ctx == nil {
		return maxDeferredErrorRequestBodyBytes
	}
	maxBytes := ctx.GetInt64("maxRequestBodyBytes")
	if maxBytes <= 0 || maxBytes > maxDeferredErrorRequestBodyBytes {
		return maxDeferredErrorRequestBodyBytes
	}
	return maxBytes
}

func decodeCapturedRequestBodyForLogWithLimit(raw []byte, encoding string, limit int64) []byte {
	if len(raw) == 0 || limit <= 0 {
		return raw
	}
	encoding = strings.TrimSpace(encoding)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw
	}

	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, truncated, errDecode := decodeCapturedZstdRequestBodyWithLimit(body, limit)
			if errDecode != nil {
				return raw
			}
			body = decoded
			if truncated {
				if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
					body = append(body, '\n')
				}
				return append(body, "[DECOMPRESSED REQUEST BODY TRUNCATED]"...)
			}
		default:
			return raw
		}
	}
	return body
}

func decodeCapturedZstdRequestBodyWithLimit(raw []byte, limit int64) ([]byte, bool, error) {
	// One decoder goroutine and a bounded window: a frame that declares a larger window is not decoded (the raw bytes are logged).
	decoder, errNewReader := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxCapturedZstdWindowBytes))
	if errNewReader != nil {
		return nil, false, fmt.Errorf("failed to create zstd request decoder: %w", errNewReader)
	}
	defer decoder.Close()

	var reader io.Reader = decoder
	if limit > 0 && limit < math.MaxInt64 {
		reader = io.LimitReader(decoder, limit+1)
	}
	decoded, errRead := io.ReadAll(reader)
	if errRead != nil {
		return nil, false, fmt.Errorf("failed to decode zstd request body: %w", errRead)
	}
	if int64(len(decoded)) > limit {
		return decoded[:limit], true, nil
	}
	return decoded, false, nil
}

// shouldLogRequest determines whether the request should be logged.
// It skips management endpoints to avoid leaking secrets but allows
// all other routes, including module-provided ones, to honor request-log.
func shouldLogRequest(path string) bool {
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management") {
		return false
	}

	if strings.HasPrefix(path, "/api") {
		return strings.HasPrefix(path, "/api/provider")
	}

	return true
}
