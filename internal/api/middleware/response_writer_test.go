package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/interfaces"
	"github.com/therealtinhtute/llmhub/internal/logging"
)

func TestExtractRequestBodyPrefersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{
		requestInfo: &RequestInfo{Body: []byte("original-body")},
	}

	body := wrapper.extractRequestBody(c)
	if string(body) != "original-body" {
		t.Fatalf("request body = %q, want %q", string(body), "original-body")
	}

	c.Set(requestBodyOverrideContextKey, []byte("override-body"))
	body = wrapper.extractRequestBody(c)
	if string(body) != "override-body" {
		t.Fatalf("request body = %q, want %q", string(body), "override-body")
	}
}

func TestExtractRequestBodySupportsStringOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{body: &bytes.Buffer{}}
	c.Set(requestBodyOverrideContextKey, "override-as-string")

	body := wrapper.extractRequestBody(c)
	if string(body) != "override-as-string" {
		t.Fatalf("request body = %q, want %q", string(body), "override-as-string")
	}
}

func TestExtractResponseBodyPrefersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{body: &bytes.Buffer{}}
	wrapper.body.WriteString("original-response")

	body := wrapper.extractResponseBody(c)
	if string(body) != "original-response" {
		t.Fatalf("response body = %q, want %q", string(body), "original-response")
	}

	c.Set(responseBodyOverrideContextKey, []byte("override-response"))
	body = wrapper.extractResponseBody(c)
	if string(body) != "override-response" {
		t.Fatalf("response body = %q, want %q", string(body), "override-response")
	}

	body[0] = 'X'
	if got := wrapper.extractResponseBody(c); string(got) != "override-response" {
		t.Fatalf("response override should be cloned, got %q", string(got))
	}
}

func TestExtractResponseBodySupportsStringOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{}
	c.Set(responseBodyOverrideContextKey, "override-response-as-string")

	body := wrapper.extractResponseBody(c)
	if string(body) != "override-response-as-string" {
		t.Fatalf("response body = %q, want %q", string(body), "override-response-as-string")
	}
}

func TestExtractBodyOverrideClonesBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	override := []byte("body-override")
	c.Set(requestBodyOverrideContextKey, override)

	body := extractBodyOverride(c, requestBodyOverrideContextKey)
	if !bytes.Equal(body, override) {
		t.Fatalf("body override = %q, want %q", string(body), string(override))
	}

	body[0] = 'X'
	if !bytes.Equal(override, []byte("body-override")) {
		t.Fatalf("override mutated: %q", string(override))
	}
}

func TestExtractWebsocketTimelineUsesOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{}
	if got := wrapper.extractWebsocketTimeline(c); got != nil {
		t.Fatalf("expected nil websocket timeline, got %q", string(got))
	}

	c.Set(websocketTimelineOverrideContextKey, []byte("timeline"))
	body := wrapper.extractWebsocketTimeline(c)
	if string(body) != "timeline" {
		t.Fatalf("websocket timeline = %q, want %q", string(body), "timeline")
	}
}

func TestFinalizeStreamingWritesAPIWebsocketTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	streamWriter := &testStreamingLogWriter{}
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		logger:         &testRequestLogger{enabled: true},
		requestInfo: &RequestInfo{
			URL:       "/v1/responses",
			Method:    "POST",
			Headers:   map[string][]string{"Content-Type": {"application/json"}},
			RequestID: "req-1",
			Timestamp: time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
		},
		isStreaming:  true,
		streamWriter: streamWriter,
	}

	c.Set("API_WEBSOCKET_TIMELINE", []byte("Timestamp: 2026-04-01T12:00:00Z\nEvent: api.websocket.request\n{}"))

	if err := wrapper.Finalize(c); err != nil {
		t.Fatalf("Finalize error: %v", err)
	}
	if string(streamWriter.apiWebsocketTimeline) != "Timestamp: 2026-04-01T12:00:00Z\nEvent: api.websocket.request\n{}" {
		t.Fatalf("stream writer websocket timeline = %q", string(streamWriter.apiWebsocketTimeline))
	}
	if !streamWriter.closed {
		t.Fatal("expected stream writer to be closed")
	}
}

type testRequestLogger struct {
	enabled bool
}

func (l *testRequestLogger) LogRequest(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []byte, []byte, []*interfaces.ErrorMessage, string, time.Time, time.Time) error {
	return nil
}

func (l *testRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return &testStreamingLogWriter{}, nil
}

func (l *testRequestLogger) IsEnabled() bool {
	return l.enabled
}

type testStreamingLogWriter struct {
	apiWebsocketTimeline []byte
	apiResponseErrors    []*interfaces.ErrorMessage
	statuses             []int
	closed               bool
}

func (w *testStreamingLogWriter) WriteChunkAsync([]byte) {}

func (w *testStreamingLogWriter) WriteStatus(status int, _ map[string][]string) error {
	w.statuses = append(w.statuses, status)
	return nil
}

func (w *testStreamingLogWriter) WriteAPIRequest([]byte) error {
	return nil
}

func (w *testStreamingLogWriter) WriteAPIResponse([]byte) error {
	return nil
}

func (w *testStreamingLogWriter) WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error {
	w.apiWebsocketTimeline = bytes.Clone(apiWebsocketTimeline)
	return nil
}

func (w *testStreamingLogWriter) WriteAPIResponseErrors(errors []*interfaces.ErrorMessage) error {
	w.apiResponseErrors = append([]*interfaces.ErrorMessage(nil), errors...)
	return nil
}

func (w *testStreamingLogWriter) SetFirstChunkTimestamp(time.Time) {}

func (w *testStreamingLogWriter) Close() error {
	w.closed = true
	return nil
}

func TestShouldBufferResponseBodyExcludesClientCancellation(t *testing.T) {
	wrapper := &ResponseWriterWrapper{
		logOnErrorOnly: true,
		statusCode:     statusClientClosedRequest,
	}
	if wrapper.shouldBufferResponseBody() {
		t.Fatal("expected 499 response body not to be buffered in error-only mode")
	}

	wrapper.statusCode = http.StatusInternalServerError
	if !wrapper.shouldBufferResponseBody() {
		t.Fatal("expected 500 response body to be buffered in error-only mode")
	}
}

func TestHasActionableError(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		statusCode int
		ctx        context.Context
		apiErrors  []*interfaces.ErrorMessage
		want       bool
	}{
		{
			name:       "499 client closed request",
			statusCode: statusClientClosedRequest,
		},
		{
			name:       "200 with canceled context",
			statusCode: http.StatusOK,
			ctx:        canceledCtx,
		},
		{
			name:       "400 bad request",
			statusCode: http.StatusBadRequest,
			want:       true,
		},
		{
			name:       "503 with canceled context remains actionable",
			statusCode: http.StatusServiceUnavailable,
			ctx:        canceledCtx,
			want:       true,
		},
		{
			name:       "200 with wrapped canceled API error",
			statusCode: http.StatusOK,
			apiErrors: []*interfaces.ErrorMessage{{
				Error: fmt.Errorf("read: %w", context.Canceled),
			}},
		},
		{
			name:       "200 with actionable API error",
			statusCode: http.StatusOK,
			apiErrors: []*interfaces.ErrorMessage{{
				StatusCode: http.StatusBadGateway,
				Error:      errors.New("upstream failed"),
			}},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tc.ctx != nil {
				request = request.WithContext(tc.ctx)
			}
			c.Request = request

			if got := hasActionableError(c, tc.statusCode, tc.apiErrors); got != tc.want {
				t.Fatalf("hasActionableError(status=%d) = %t, want %t", tc.statusCode, got, tc.want)
			}
		})
	}
}

type recordingRequestLogger struct {
	loggedCalls []int
	enabled     bool
}

func (l *recordingRequestLogger) LogRequest(_ string, _ string, _ map[string][]string, _ []byte, statusCode int, _ map[string][]string, _ []byte, _ []byte, _ []byte, _ []byte, _ []byte, _ []*interfaces.ErrorMessage, _ string, _ time.Time, _ time.Time) error {
	l.loggedCalls = append(l.loggedCalls, statusCode)
	return nil
}

func (l *recordingRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return &testStreamingLogWriter{}, nil
}

func (l *recordingRequestLogger) IsEnabled() bool {
	return l.enabled
}

func (l *recordingRequestLogger) LogRequestWithOptions(_ string, _ string, _ map[string][]string, _ []byte, statusCode int, _ map[string][]string, _ []byte, _ []byte, _ []byte, _ []byte, _ []byte, _ []*interfaces.ErrorMessage, force bool, _ string, _ time.Time, _ time.Time) error {
	if force || l.enabled {
		l.loggedCalls = append(l.loggedCalls, statusCode)
	}
	return nil
}

func TestFinalizeExcludesClientCancellationFromForceLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	logger := &recordingRequestLogger{}
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		logger:         logger,
		logOnErrorOnly: true,
		statusCode:     statusClientClosedRequest,
		requestInfo: &RequestInfo{
			URL:       "/v1/responses",
			Method:    http.MethodPost,
			RequestID: "req-499",
		},
	}

	if err := wrapper.Finalize(c); err != nil {
		t.Fatalf("Finalize error: %v", err)
	}
	if len(logger.loggedCalls) != 0 {
		t.Fatalf("expected no forced log for 499, got %v", logger.loggedCalls)
	}
}

func TestFinalizeIncludesServerFailureInForceLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	logger := &recordingRequestLogger{}
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		logger:         logger,
		logOnErrorOnly: true,
		statusCode:     http.StatusInternalServerError,
		requestInfo: &RequestInfo{
			URL:       "/v1/responses",
			Method:    http.MethodPost,
			RequestID: "req-500",
		},
	}

	if err := wrapper.Finalize(c); err != nil {
		t.Fatalf("Finalize error: %v", err)
	}
	if len(logger.loggedCalls) != 1 || logger.loggedCalls[0] != http.StatusInternalServerError {
		t.Fatalf("expected one forced 500 log, got %v", logger.loggedCalls)
	}
}

// TestResponseWriterLogOnErrorOnlyZeroBufferUntilActionableError pins the
// no-200-OK-buffering rule (upstream 69a032dea368): error-only mode must not
// allocate for a successful response, and must start buffering once a
// mid-stream error has been recorded.
func TestResponseWriterLogOnErrorOnlyZeroBufferUntilActionableError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	wrapper := NewResponseWriterWrapper(c.Writer, &testRequestLogger{}, &RequestInfo{
		URL:    "/v1/messages",
		Method: http.MethodPost,
	})
	wrapper.logOnErrorOnly = true
	wrapper.ginContext = c

	// Normal 200 OK writes (even 4 MiB) must not allocate/buffer any bytes when request-log is disabled.
	largePayload := bytes.Repeat([]byte("a"), 4<<20)
	if _, err := wrapper.Write(largePayload); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if wrapper.body.Len() != 0 {
		t.Fatalf("wrapper.body.Len() for 200 OK = %d, want 0", wrapper.body.Len())
	}

	// Once an actionable mid-stream error is recorded, subsequent error frame writes are buffered.
	c.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{
		{
			StatusCode: http.StatusBadRequest,
			Error:      errors.New("claude refusal (cyber): blocked"),
		},
	})
	const errFrame = "event: error\ndata: {\"type\":\"error\"}\n\n"
	if _, err := wrapper.WriteString(errFrame); err != nil {
		t.Fatalf("WriteString failed: %v", err)
	}
	if got := wrapper.body.String(); got != errFrame {
		t.Fatalf("wrapper.body.String() = %q, want %q", got, errFrame)
	}
}
