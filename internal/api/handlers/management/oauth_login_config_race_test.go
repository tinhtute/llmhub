package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/config"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// OAuth login handlers and token save helpers run outside the request that
// mutates Handler.cfg, so they must take a snapshot under h.mu. Run with -race:
// the concurrent config writer below mutates the very fields those paths read.
func TestOAuthLoginAndTokenSaveDoNotRaceConfigSave(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir(), Port: 8317, Debug: false}, manager)
	h.tokenStore = store

	const iterations = 20
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Config writer: the same in-place mutation a management save performs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.mu.Lock()
			h.cfg.AuthDir = t.TempDir()
			h.cfg.Port = 8317 + i%2
			h.cfg.Debug = i%2 == 0
			h.mu.Unlock()
		}
	}()

	// Login path: builds the provider auth service from the handler config.
	for range iterations {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/anthropic-auth-url", nil)
		h.RequestAnthropicToken(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("anthropic login status = %d, body %s", rec.Code, rec.Body.String())
		}
		var payload struct {
			State string `json:"state"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
			t.Fatalf("decode login response: %v", errDecode)
		}
		// The waiter goroutine reads the config too; stop it before the next round.
		CancelOAuthSession(payload.State)
	}

	// Token save path: resolves the token store and its base dir from the config.
	for i := range iterations {
		record := &coreauth.Auth{
			ID:       "raced-token.json",
			FileName: "raced-token.json",
			Provider: "claude",
			Metadata: map[string]any{"type": "claude", "email": "race@example.com"},
		}
		if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
			t.Fatalf("saveTokenRecord() error = %v", errSave)
		}
		_ = i
	}

	close(stop)
	wg.Wait()

	// The handler must still own a usable config after the storm.
	if snapshot := h.configSnapshot(); snapshot == nil {
		t.Fatal("configSnapshot() returned nil")
	}
}
