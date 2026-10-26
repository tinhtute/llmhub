package logging

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestGinLogrusRecoveryRepanicsErrAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/abort", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	recorder := httptest.NewRecorder()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic, got nil")
		}
		err, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T", recovered)
		}
		if !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("expected ErrAbortHandler, got %v", err)
		}
		if err != http.ErrAbortHandler {
			t.Fatalf("expected exact ErrAbortHandler sentinel, got %v", err)
		}
	}()

	engine.ServeHTTP(recorder, req)
}

func TestGinLogrusRecoveryHandlesRegularPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
}

func TestIsAIAPIPathIncludesImages(t *testing.T) {
	if !isAIAPIPath("/v1/images/generations") {
		t.Fatalf("expected /v1/images/generations to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/images/edits") {
		t.Fatalf("expected /v1/images/edits to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/videos") {
		t.Fatalf("expected /v1/videos to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/videos/video_123") {
		t.Fatalf("expected /v1/videos/video_123 to be treated as AI API path")
	}
}

// TestGinLogrusLoggerHealthProbeStatus ports upstream a163c5e7e: successful
// GET/HEAD /healthz probes stay silent while failed probes remain visible.
func TestGinLogrusLoggerHealthProbeStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })
	for _, tc := range []struct {
		name, method, path string
		status             int
		wantLog            bool
	}{
		{"get_ok", "GET", "/healthz", 200, false},
		{"head_ok", "HEAD", "/healthz", 200, false},
		{"success_boundary", "GET", "/healthz", 299, false},
		{"redirect", "GET", "/healthz", 300, true},
		{"client_error", "GET", "/healthz", 400, true},
		{"server_error", "HEAD", "/healthz", 503, true},
		{"similar_path", "GET", "/healthz-extra", 200, true},
		{"other_method", "POST", "/healthz", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := gin.New()
			engine.Use(GinLogrusLogger())
			// Simulate an early global middleware response before route handlers run.
			engine.Use(func(c *gin.Context) { c.AbortWithStatus(tc.status) })
			engine.Handle(tc.method, tc.path, func(c *gin.Context) { t.Error("aborted request reached route handler") })
			hook.Reset()
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.status)
			}
			if got := len(hook.AllEntries()) > 0; got != tc.wantLog {
				t.Errorf("access log present = %v, want %v", got, tc.wantLog)
			}
		})
	}
}

func TestGinLogrusLoggerEscapesPathControlCharacters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })

	engine := gin.New()
	engine.Use(GinLogrusLogger())

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x%0A200%20forged%01", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}

	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	message := entries[0].Message
	if strings.ContainsAny(message, "\r\n") || strings.Contains(message, "\x01") {
		t.Errorf("access log message carries a raw control character: %q", message)
	}
	if !strings.HasSuffix(message, `GET     "/x\n200 forged\x01"`) {
		t.Errorf("access log message = %q, want suffix %q", message, `GET     "/x\n200 forged\x01"`)
	}
}

func TestGinLogrusRecoveryEscapesPathControlCharacters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.NoRoute(func(c *gin.Context) { panic("boom") })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic%0Aforged%01", nil))

	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	path, _ := entries[0].Data["path"].(string)
	if strings.ContainsAny(path, "\r\n") || strings.Contains(path, "\x01") {
		t.Errorf("recovery path field carries a raw control character: %q", path)
	}
	if path != `"/panic\nforged\x01"` {
		t.Errorf("recovery path field = %q, want %q", path, `"/panic\nforged\x01"`)
	}
}
