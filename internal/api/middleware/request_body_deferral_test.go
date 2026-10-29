package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
