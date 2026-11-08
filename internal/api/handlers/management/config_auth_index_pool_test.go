package management

import (
	"context"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/watcher/synthesizer"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// TestConfigAuthIndexDistinguishesConnectionPools pins that two credentials that
// differ only by their pool settings report distinct auth indexes. Before the pool
// block joined the dedup key, both collapsed onto one index (gemini/vertex were
// deduplicated outright), so an operator could not address either credential.
// Upstream CLIProxyAPI 8643ea56d520.
func TestConfigAuthIndexDistinguishesConnectionPools(t *testing.T) {
	enabled := true
	two := 2
	five := 5
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "shared-gemini-key", BaseURL: "https://example.invalid", ConnectionPool: &config.ProxyConnectionPoolConfig{Enabled: &enabled, MaxIdleConnsPerHost: &two}},
			{APIKey: "shared-gemini-key", BaseURL: "https://example.invalid", ConnectionPool: &config.ProxyConnectionPoolConfig{Enabled: &enabled, MaxIdleConnsPerHost: &five}},
		},
		VertexCompatAPIKey: []config.VertexCompatKey{
			{APIKey: "shared-vertex-key", BaseURL: "https://vertex.example.invalid", ConnectionPool: &config.ProxyConnectionPoolConfig{Enabled: &enabled, MaxIdleConnsPerHost: &two}},
			{APIKey: "shared-vertex-key", BaseURL: "https://vertex.example.invalid", ConnectionPool: &config.ProxyConnectionPoolConfig{Enabled: &enabled, MaxIdleConnsPerHost: &five}},
		},
	}
	auths, errSynth := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynth != nil {
		t.Fatalf("synthesize: %v", errSynth)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, a := range auths {
		if _, errRegister := manager.Register(context.Background(), a); errRegister != nil {
			t.Fatalf("register %s: %v", a.ID, errRegister)
		}
	}
	h := &Handler{cfg: cfg, authManager: manager}

	gemini := h.geminiKeysWithAuthIndex()
	if len(gemini) != 2 {
		t.Fatalf("gemini entries = %d, want 2", len(gemini))
	}
	if gemini[0].AuthIndex == "" || gemini[1].AuthIndex == "" || gemini[0].AuthIndex == gemini[1].AuthIndex {
		t.Fatalf("gemini auth_index values = (%q, %q), want two distinct non-empty indexes", gemini[0].AuthIndex, gemini[1].AuthIndex)
	}
	vertex := h.vertexCompatKeysWithAuthIndex()
	if len(vertex) != 2 {
		t.Fatalf("vertex entries = %d, want 2", len(vertex))
	}
	if vertex[0].AuthIndex == "" || vertex[1].AuthIndex == "" || vertex[0].AuthIndex == vertex[1].AuthIndex {
		t.Fatalf("vertex auth_index values = (%q, %q), want two distinct non-empty indexes", vertex[0].AuthIndex, vertex[1].AuthIndex)
	}
}
