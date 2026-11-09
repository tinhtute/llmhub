package middleware

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
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
	lastLogBody            []byte
	lastStreamWriter       *testStreamingLogWriter
	streamingRequests      int
	underlying             logging.RequestLogger
}

func (s *spyRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	s.lastLogRequestID = requestID
	s.lastLogBody = body
	if s.underlying != nil {
		return s.underlying.LogRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, requestID, requestTimestamp, apiResponseTimestamp)
	}
	return nil
}

func (s *spyRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (logging.StreamingLogWriter, error) {
	s.lastStreamingRequestID = requestID
	s.streamingRequests++
	if s.underlying != nil {
		return s.underlying.LogStreamingRequest(url, method, headers, body, requestID)
	}
	w := &testStreamingLogWriter{}
	s.lastStreamWriter = w
	return w, nil
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

// fallbackCapturedBodyBound is the decoded-body cap the advisory requires when no smaller configured limit exists (32 MiB).
const fallbackCapturedBodyBound int64 = 32 << 20

// streamingZstdBomb is a small zstd body (a stream frame with a 1 MiB window, as zstd makes from a pipe) that expands to
// size bytes of zeros.
func streamingZstdBomb(t *testing.T, size int) []byte {
	t.Helper()
	var out bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(1<<20))
	if errNewWriter != nil {
		t.Fatal(errNewWriter)
	}
	chunk := make([]byte, 1<<20)
	for written := 0; written < size; written += len(chunk) {
		if _, errWrite := encoder.Write(chunk); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return out.Bytes()
}

func captureZstdOf(t *testing.T, body []byte, limit int64) (captured, restored []byte) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Encoding", "zstd")
	if limit != 0 {
		c.Set("maxRequestBodyBytes", limit)
	}
	capture := attachRequestBodyCapture(c.Request, true)
	if capture == nil {
		t.Fatal("expected a request body capture")
	}
	restored, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return capture.capturedBody(c, c.Request.Header.Get("Content-Encoding")), restored
}

// A small zstd body that expands past the capture bound is decoded to at most the bound when no max-request-body-bytes is
// configured (the capture runs before authentication), or to the configured limit when that is smaller; the request body is
// restored for the handler either way. 40 MiB is enough to pass the 32 MiB bound without a heavy fixture.
func TestCaptureRequestInfoBoundsZstdExpansionWithoutAConfiguredLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bomb := streamingZstdBomb(t, 40<<20)
	if int64(len(bomb)) > maxErrorOnlyCapturedRequestBodyBytes {
		t.Fatalf("the probe body is %d bytes, over the 1 MiB capture bound", len(bomb))
	}
	marker := int64(len("\n[DECOMPRESSED REQUEST BODY TRUNCATED]"))
	for _, c := range []struct{ limit, bound int64 }{{0, fallbackCapturedBodyBound}, {-1, fallbackCapturedBodyBound}, {1 << 40, fallbackCapturedBodyBound}, {4096, 4096}} {
		captured, restored := captureZstdOf(t, bomb, c.limit)
		if int64(len(captured)) > c.bound+marker || int64(len(captured)) < c.bound {
			t.Fatalf("limit %d: captured %d bytes, want the bound %d plus the marker", c.limit, len(captured), c.bound)
		}
		if !bytes.HasSuffix(captured, []byte("[DECOMPRESSED REQUEST BODY TRUNCATED]")) {
			t.Fatalf("limit %d: no truncation marker", c.limit)
		}
		if !bytes.Equal(restored, bomb) {
			t.Fatalf("limit %d: the request body was not restored for the handler (%d bytes)", c.limit, len(restored))
		}
	}
}

// The classic bomb (one segment frame that declares its whole content as its window, 49 KB for 512 MiB) is not decoded at all: its
// window is past the bound, so the raw bytes are logged and nothing large is allocated.
func TestCaptureRequestInfoDoesNotDecodeASingleSegmentBomb(t *testing.T) {
	gin.SetMode(gin.TestMode)
	encoder, errNewWriter := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if errNewWriter != nil {
		t.Fatal(errNewWriter)
	}
	bomb := encoder.EncodeAll(make([]byte, 64<<20), nil)
	if int64(len(bomb)) > maxErrorOnlyCapturedRequestBodyBytes {
		t.Fatalf("the probe body is %d bytes", len(bomb))
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	captured, restored := captureZstdOf(t, bomb, 0)
	runtime.ReadMemStats(&after)
	if !bytes.Equal(captured, bomb) || !bytes.Equal(restored, bomb) {
		t.Fatalf("the raw bytes must be logged and restored, captured %d restored %d", len(captured), len(restored))
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 16<<20 {
		t.Fatalf("decoding a window-bomb allocated %d MiB", grown>>20)
	}
}

// A frame that declares a window past the bound is not decoded: the raw bytes are logged and nothing large is allocated.
func TestCaptureRequestInfoRefusesAnOversizedZstdWindow(t *testing.T) {
	// `printf '{"model":"gpt-5"}' | zstd --long=24`-style frame declaring a 16 MiB window.
	raw, errDecode := hex.DecodeString("28b52ffd0470b900007b226d6f64656c223a226770742d362e312d736f6c227d2a82afeb")
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	gin.SetMode(gin.TestMode)
	captured, restored := captureZstdOf(t, raw, 1<<20)
	if !bytes.Equal(captured, raw) {
		t.Fatalf("an oversized window must leave the raw bytes, got %q", captured)
	}
	if !bytes.Equal(restored, raw) {
		t.Fatalf("the request body was not restored: %q", restored)
	}
}

// The bound holds on the live middleware path too: a compressed body that expands past the cap reaches the logger truncated, and
// the handler still receives the original compressed bytes.
func TestRequestLoggingMiddlewareBoundsZstdCapturedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bomb := streamingZstdBomb(t, 40<<20)
	spy := &spyRequestLogger{}
	var handlerBody []byte

	router := gin.New()
	router.Use(RequestLoggingMiddleware(spy))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		handlerBody, _ = io.ReadAll(c.Request.Body)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bomb))
	req.Header.Set("Content-Encoding", "zstd")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	if int64(len(spy.lastLogBody)) > fallbackCapturedBodyBound+int64(len("\n[DECOMPRESSED REQUEST BODY TRUNCATED]")) {
		t.Fatalf("logged body = %d bytes, want at most the %d byte bound", len(spy.lastLogBody), fallbackCapturedBodyBound)
	}
	if !bytes.HasSuffix(spy.lastLogBody, []byte("[DECOMPRESSED REQUEST BODY TRUNCATED]")) {
		t.Fatal("logged body is not marked truncated")
	}
	if !bytes.Equal(handlerBody, bomb) {
		t.Fatalf("the handler body was not restored (%d bytes)", len(handlerBody))
	}
}

// TestRequestLoggingMiddleware_FinalizesStreamingWriterOnHandlerPanic pins the
// panic-safe finalize path (upstream a5795047bfb4): a handler that commits a 200
// SSE response and then panics must still close the streaming log writer, and a
// handler that panics before committing anything must be logged as a 500.
func TestRequestLoggingMiddleware_FinalizesStreamingWriterOnHandlerPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	spy := &spyRequestLogger{}

	router := gin.New()
	router.Use(logging.GinLogrusRecovery())
	router.Use(RequestLoggingMiddleware(spy))
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write([]byte("data: chunk\n\n"))
		panic("handler boom")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if spy.lastStreamWriter == nil {
		t.Fatal("expected streaming log writer to be created before panic")
	}
	if !spy.lastStreamWriter.closed {
		t.Fatal("expected streaming log writer to be closed when handler panics")
	}
	if spy.streamingRequests != 1 {
		t.Fatalf("streaming log writers created = %d, want exactly 1", spy.streamingRequests)
	}

	// A non-streaming handler panicking before writing a status is finalized as
	// 500 so error-only request logging persists the error log.
	logsDir := t.TempDir()
	errorOnlyLogger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	errorRouter := gin.New()
	errorRouter.Use(logging.GinLogrusRecovery())
	errorRouter.Use(RequestLoggingMiddleware(errorOnlyLogger))
	errorRouter.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Status(http.StatusOK)
		panic("pre-status boom")
	})
	reqPreStatus := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	reqPreStatus.Header.Set("Content-Type", "application/json")
	respPreStatus := httptest.NewRecorder()
	errorRouter.ServeHTTP(respPreStatus, reqPreStatus)
	if respPreStatus.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d, want 500", respPreStatus.Code)
	}
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("ReadDir logsDir: %v", errRead)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "error-") {
		t.Fatalf("expected 1 error log file on pre-status panic, got %+v", entries)
	}

	// A panic before any write must not let the outer recovery reopen a streaming
	// log writer after the middleware already finalized the request.
	lateSpy := &spyRequestLogger{}
	lateRouter := gin.New()
	lateRouter.Use(logging.GinLogrusRecovery())
	lateRouter.Use(RequestLoggingMiddleware(lateSpy))
	lateRouter.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		panic("pre-write boom")
	})
	reqLate := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`))
	reqLate.Header.Set("Content-Type", "application/json")
	lateRouter.ServeHTTP(httptest.NewRecorder(), reqLate)
	if lateSpy.streamingRequests != 0 {
		t.Fatalf("streaming log writers created after finalize = %d, want 0", lateSpy.streamingRequests)
	}

	// http.ErrAbortHandler must not stamp a synthetic 500 error log in error-only mode.
	abortLogsDir := t.TempDir()
	abortLogger := logging.NewFileRequestLogger(false, abortLogsDir, "", 10)
	abortRouter := gin.New()
	abortRouter.Use(RequestLoggingMiddleware(abortLogger))
	abortRouter.POST("/v1/chat/completions", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})
	func() {
		defer func() { _ = recover() }()
		reqAbort := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
		reqAbort.Header.Set("Content-Type", "application/json")
		abortRouter.ServeHTTP(httptest.NewRecorder(), reqAbort)
	}()
	abortEntries, errReadAbort := os.ReadDir(abortLogsDir)
	if errReadAbort != nil {
		t.Fatalf("ReadDir abortLogsDir: %v", errReadAbort)
	}
	if len(abortEntries) != 0 {
		t.Fatalf("expected no error log for http.ErrAbortHandler, got %+v", abortEntries)
	}
}

// TestRequestLoggingMiddleware_MidStreamErrorClassifiesStatus pins the mid-stream
// classification path (upstream c3938b72bdaa): an SSE response that already
// committed 200 but recorded a 400 API error must land in an error-*.log whose
// RESPONSE status is the classified 400.
func TestRequestLoggingMiddleware_MidStreamErrorClassifiesStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, requestLogEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("request-log=%v", requestLogEnabled), func(t *testing.T) {
			logsDir := t.TempDir()
			logger := logging.NewFileRequestLogger(requestLogEnabled, logsDir, "", 10)

			router := gin.New()
			router.Use(RequestLoggingMiddleware(logger))
			router.POST("/v1beta/models/gemini-4-argon-high:streamGenerateContent", func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				c.Writer.WriteHeader(http.StatusOK)
				_, _ = c.Writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"thought\":true,\"text\":\"thinking\"}]}}]}\n\n"))
				c.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{
					{
						StatusCode: http.StatusBadRequest,
						Error:      errors.New("This request was blocked by Gemini's filters. You can read more about our policies here (https://policies.google.com/terms/generative-ai/use-policy)."),
					},
				})
			})

			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon-high:streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[]}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(httptest.NewRecorder(), req)

			entries, errRead := os.ReadDir(logsDir)
			if errRead != nil {
				t.Fatalf("read logs dir: %v", errRead)
			}
			var errorLogPath string
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
					errorLogPath = filepath.Join(logsDir, entry.Name())
				}
			}
			if errorLogPath == "" {
				t.Fatalf("requestLog=%v: expected error-*.log file, got entries=%v", requestLogEnabled, entries)
			}
			content, errReadFile := os.ReadFile(errorLogPath)
			if errReadFile != nil {
				t.Fatalf("read error log: %v", errReadFile)
			}
			if !bytes.Contains(content, []byte("=== API ERROR RESPONSE ===\nHTTP Status: 400")) {
				t.Fatalf("requestLog=%v: missing API ERROR RESPONSE 400 in log:\n%s", requestLogEnabled, string(content))
			}
			if !bytes.Contains(content, []byte("=== RESPONSE ===\nStatus: 400\n")) {
				t.Fatalf("requestLog=%v: expected RESPONSE Status: 400 in log:\n%s", requestLogEnabled, string(content))
			}
		})
	}
}
