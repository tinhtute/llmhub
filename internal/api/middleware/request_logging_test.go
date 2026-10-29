package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/therealtinhtute/llmhub/internal/interfaces"
	"github.com/therealtinhtute/llmhub/internal/logging"
)

func TestShouldSkipMethodForRequestLogging(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		skip bool
	}{
		{
			name: "nil request",
			req:  nil,
			skip: true,
		},
		{
			name: "post request should not skip",
			req: &http.Request{
				Method: http.MethodPost,
				URL:    &url.URL{Path: "/v1/responses"},
			},
			skip: false,
		},
		{
			name: "plain get should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/models"},
				Header: http.Header{},
			},
			skip: true,
		},
		{
			name: "responses websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "responses get without upgrade should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{},
			},
			skip: true,
		},
	}

	for i := range tests {
		got := shouldSkipMethodForRequestLogging(tests[i].req)
		if got != tests[i].skip {
			t.Fatalf("%s: got skip=%t, want %t", tests[i].name, got, tests[i].skip)
		}
	}
}

func TestShouldCaptureRequestBody(t *testing.T) {
	tests := []struct {
		name          string
		loggerEnabled bool
		req           *http.Request
		want          bool
	}{
		{
			name:          "logger enabled always captures",
			loggerEnabled: true,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: true,
		},
		{
			name:          "nil request",
			loggerEnabled: false,
			req:           nil,
			want:          false,
		},
		{
			name:          "small known size json in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: 2,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: true,
		},
		{
			name:          "large known size skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: maxErrorOnlyCapturedRequestBodyBytes + 1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "unknown size skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "multipart skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: 1,
				Header:        http.Header{"Content-Type": []string{"multipart/form-data; boundary=abc"}},
			},
			want: false,
		},
	}

	for i := range tests {
		got := shouldCaptureRequestBody(tests[i].loggerEnabled, tests[i].req)
		if got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func TestAttachWebsocketLogSourcesUsesLoggerLogsDir(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 0)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("Upgrade", "websocket")

	attachWebsocketLogSources(c, logger, true)
	defer cleanupFileBodySourcesFromContext(c)

	for _, key := range []string{
		logging.WebsocketTimelineSourceContextKey,
		logging.APIWebsocketTimelineSourceContextKey,
	} {
		value, exists := c.Get(key)
		if !exists {
			t.Fatalf("expected %s source to be attached", key)
		}
		source, ok := value.(*logging.FileBodySource)
		if !ok || source == nil {
			t.Fatalf("%s source type = %T", key, value)
		}
		file, errPart := source.CreatePart("probe")
		if errPart != nil {
			t.Fatalf("CreatePart(%s): %v", key, errPart)
		}
		path := file.Name()
		if errClose := file.Close(); errClose != nil {
			t.Fatalf("close part: %v", errClose)
		}
		if !strings.HasPrefix(path, logsDir+string(os.PathSeparator)) {
			t.Fatalf("%s part path %s is not under logs dir %s", key, path, logsDir)
		}
	}
}

func cleanupFileBodySourcesFromContext(c *gin.Context) {
	if c == nil {
		return
	}
	for _, key := range []string{
		logging.WebsocketTimelineSourceContextKey,
		logging.APIWebsocketTimelineSourceContextKey,
	} {
		value, exists := c.Get(key)
		if !exists {
			continue
		}
		if source, ok := value.(*logging.FileBodySource); ok && source != nil {
			_ = source.Cleanup()
		}
	}
}

func TestRequestLoggingMiddlewareClientCancellationExclusion(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("499 is not forced when request logging is disabled", func(t *testing.T) {
		logsDir := t.TempDir()
		logger := logging.NewFileRequestLogger(false, logsDir, "", 10)

		router := gin.New()
		router.Use(RequestLoggingMiddleware(logger))
		router.POST("/v1/responses", func(c *gin.Context) {
			c.AbortWithStatus(statusClientClosedRequest)
		})

		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code != statusClientClosedRequest {
			t.Fatalf("status = %d, want %d", response.Code, statusClientClosedRequest)
		}
		entries, errRead := os.ReadDir(logsDir)
		if errRead != nil {
			t.Fatalf("read logs dir: %v", errRead)
		}
		if len(entries) != 0 {
			t.Fatalf("expected no log files for 499 cancellation, got %d", len(entries))
		}
	})

	t.Run("canceled context is not forced when request logging is disabled", func(t *testing.T) {
		logsDir := t.TempDir()
		logger := logging.NewFileRequestLogger(false, logsDir, "", 10)

		router := gin.New()
		router.Use(RequestLoggingMiddleware(logger))
		router.POST("/v1/responses", func(c *gin.Context) {
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
			c.Status(http.StatusOK)
		})

		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		entries, errRead := os.ReadDir(logsDir)
		if errRead != nil {
			t.Fatalf("read logs dir: %v", errRead)
		}
		if len(entries) != 0 {
			t.Fatalf("expected no log files for canceled context, got %d", len(entries))
		}
	})

	t.Run("ordinary bad request remains forced", func(t *testing.T) {
		logsDir := t.TempDir()
		logger := logging.NewFileRequestLogger(false, logsDir, "", 10)

		router := gin.New()
		router.Use(RequestLoggingMiddleware(logger))
		router.POST("/v1/responses", func(c *gin.Context) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid parameter"})
		})

		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"bad":"param"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
		}
		entries, errRead := os.ReadDir(logsDir)
		if errRead != nil {
			t.Fatalf("read logs dir: %v", errRead)
		}
		var errorLogCount int
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
				errorLogCount++
			}
		}
		if errorLogCount != 1 {
			t.Fatalf("expected one error log for 400, got %d", errorLogCount)
		}
	})
}

type spyRequestLogger struct {
	lastLogRequestID       string
	lastStreamingRequestID string
	underlying             logging.RequestLogger
}

func (s *spyRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	s.lastLogRequestID = requestID
	if s.underlying != nil {
		return s.underlying.LogRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, requestID, requestTimestamp, apiResponseTimestamp)
	}
	return nil
}

func (s *spyRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (logging.StreamingLogWriter, error) {
	s.lastStreamingRequestID = requestID
	if s.underlying != nil {
		return s.underlying.LogStreamingRequest(url, method, headers, body, requestID)
	}
	return &testStreamingLogWriter{}, nil
}

func (s *spyRequestLogger) IsEnabled() bool {
	return true
}

func TestRequestLoggingMiddleware_PreservesFullUUIDForLoggerAndTruncatesFilename(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	fileLogger := logging.NewFileRequestLogger(true, logsDir, "", 10)
	spy := &spyRequestLogger{underlying: fileLogger}

	router := gin.New()
	router.Use(logging.GinLogrusLogger())
	router.Use(RequestLoggingMiddleware(spy))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"choices": []string{"hello"}})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"input":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}

	// 1. Verify logger received full UUIDv7
	if spy.lastLogRequestID == "" {
		t.Fatalf("expected non-empty request ID passed to logger")
	}
	parsed, errParse := uuid.Parse(spy.lastLogRequestID)
	if errParse != nil {
		t.Fatalf("request ID passed to logger is not a valid UUID: %q (%v)", spy.lastLogRequestID, errParse)
	}
	if parsed.Version() != 7 {
		t.Fatalf("request ID version = %d, want 7", parsed.Version())
	}

	// 2. Verify on-disk log filename uses the trailing 8 chars, not full UUID
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("ReadDir logsDir failed: %v", errRead)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 log file on disk, got %d", len(entries))
	}
	fileName := entries[0].Name()
	expectedShortID := spy.lastLogRequestID[len(spy.lastLogRequestID)-8:]
	expectedSuffix := "-" + expectedShortID + ".log"
	if !strings.HasSuffix(fileName, expectedSuffix) {
		t.Fatalf("filename %q does not end with expected short suffix %q", fileName, expectedSuffix)
	}
	if strings.Contains(fileName, spy.lastLogRequestID) {
		t.Fatalf("filename %q should not contain the full UUID", fileName)
	}
}

func TestRequestLoggingMiddleware_StreamingPreservesFullUUIDForLogger(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	fileLogger := logging.NewFileRequestLogger(true, logsDir, "", 10)
	spy := &spyRequestLogger{underlying: fileLogger}

	router := gin.New()
	router.Use(logging.GinLogrusLogger())
	router.Use(RequestLoggingMiddleware(spy))
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write([]byte("data: chunk\n\n"))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}

	if spy.lastStreamingRequestID == "" {
		t.Fatalf("expected non-empty streaming request ID passed to logger")
	}
	parsed, errParse := uuid.Parse(spy.lastStreamingRequestID)
	if errParse != nil {
		t.Fatalf("streaming request ID passed to logger is not a valid UUID: %q (%v)", spy.lastStreamingRequestID, errParse)
	}
	if parsed.Version() != 7 {
		t.Fatalf("streaming request ID version = %d, want 7", parsed.Version())
	}
}
