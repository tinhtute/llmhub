package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/therealtinhtute/llmhub/internal/interfaces"
	"github.com/therealtinhtute/llmhub/internal/logging"
)

// bodyRecordLogger records the request body handed to the logger.
type bodyRecordLogger struct {
	calls      int
	loggedBody []byte
}

func (l *bodyRecordLogger) LogRequest(_ string, _ string, _ map[string][]string, body []byte, _ int, _ map[string][]string, _ []byte, _ []byte, _ []byte, _ []byte, _ []byte, _ []*interfaces.ErrorMessage, _ string, _ time.Time, _ time.Time) error {
	l.calls++
	l.loggedBody = append([]byte(nil), body...)
	return nil
}

func (l *bodyRecordLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return &testStreamingLogWriter{}, nil
}

func (l *bodyRecordLogger) IsEnabled() bool { return true }

// unreadableBody blocks every Read: it models a client that sent request headers
// and then stalled in the middle of the body.
type unreadableBody struct {
	reads atomic.Int32
	block chan struct{}
}

func (b *unreadableBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	<-b.block
	return 0, io.EOF
}

func (b *unreadableBody) Close() error { return nil }

func newLoggingEngine(logger logging.RequestLogger, handler gin.HandlerFunc) *gin.Engine {
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST("/v1/chat/completions", handler)
	engine.POST("/v1/responses", handler)
	return engine
}

// A request rejected before any handler reads its body must not make the logging
// middleware read the body: otherwise one unauthenticated connection with an
// incomplete body holds a descriptor until the client gives up.
func TestRequestLoggingMiddlewareDoesNotReadBodyBeforeHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := newLoggingEngine(&bodyRecordLogger{}, func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing api key"})
	})

	body := &unreadableBody{block: make(chan struct{})}
	defer close(body.block)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 1 << 20
	recorder := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(recorder, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request logging middleware blocked on an unread request body")
	}

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if reads := body.reads.Load(); reads != 0 {
		t.Fatalf("request body read %d times before the handler asked for it, want 0", reads)
	}
}

// An authenticated request must reach its handler intact however large its body
// is, while the captured log body stays bounded.
func TestRequestLoggingMiddlewarePassesLargeBodyToHandlerIntact(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logger := &bodyRecordLogger{}
	const bodySize = 3 << 20 // 3 MiB, past the 1 MiB capture threshold
	payload := bytes.Repeat([]byte("a"), bodySize)
	var handlerRead int64
	engine := newLoggingEngine(logger, func(c *gin.Context) {
		n, errRead := io.Copy(io.Discard, c.Request.Body)
		if errRead != nil {
			t.Errorf("handler read body: %v", errRead)
		}
		handlerRead = n
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if handlerRead != bodySize {
		t.Fatalf("handler received %d body bytes, want %d", handlerRead, bodySize)
	}
	if int64(len(logger.loggedBody)) != maxErrorOnlyCapturedRequestBodyBytes {
		t.Fatalf("captured log body = %d bytes, want the %d byte bound", len(logger.loggedBody), maxErrorOnlyCapturedRequestBodyBytes)
	}
	if !bytes.Equal(logger.loggedBody, payload[:maxErrorOnlyCapturedRequestBodyBytes]) {
		t.Fatal("captured log body does not match the start of the request body")
	}
}

// A zstd-encoded body is still logged decoded, and the handler still receives
// the original compressed bytes.
func TestRequestLoggingMiddlewareDecodesZstdBodyForLog(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte(`{"model":"test-model","stream":true}`)
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter: %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}
	compressedBytes := compressed.Bytes()

	logger := &bodyRecordLogger{}
	handlerBody := make([]byte, 0, len(compressedBytes))
	engine := newLoggingEngine(logger, func(c *gin.Context) {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			t.Errorf("handler read body: %v", errRead)
		}
		handlerBody = append(handlerBody, body...)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedBytes))
	req.Header.Set("Content-Encoding", "zstd")
	engine.ServeHTTP(httptest.NewRecorder(), req)

	if !bytes.Equal(handlerBody, compressedBytes) {
		t.Fatal("handler did not receive the original compressed request body")
	}
	if logger.calls != 1 {
		t.Fatalf("logger calls = %d, want 1", logger.calls)
	}
	if !bytes.Equal(logger.loggedBody, payload) {
		t.Fatalf("logged request body = %q, want %q", string(logger.loggedBody), string(payload))
	}
}

// The tee must propagate handler read errors instead of hiding them.
func TestCapturedRequestBodyPropagatesReadErrors(t *testing.T) {
	wantErr := errors.New("body read failed")
	capture := attachRequestBodyCapture(&http.Request{Body: errorBody{err: wantErr}}, true)
	if capture == nil {
		t.Fatal("expected a body capture")
	}
	if _, errRead := capture.Read(make([]byte, 8)); !errors.Is(errRead, wantErr) {
		t.Fatalf("read error = %v, want %v", errRead, wantErr)
	}
}

type errorBody struct{ err error }

func (b errorBody) Read([]byte) (int, error) { return 0, b.err }
func (b errorBody) Close() error             { return nil }
