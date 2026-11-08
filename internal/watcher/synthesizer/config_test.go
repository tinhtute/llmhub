package synthesizer

import (
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

func TestNewConfigSynthesizer(t *testing.T) {
	synth := NewConfigSynthesizer()
	if synth == nil {
		t.Fatal("expected non-nil synthesizer")
	}
}

func TestConfigSynthesizer_Synthesize_NilContext(t *testing.T) {
	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_Synthesize_NilConfig(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config:      nil,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_GeminiKeys(t *testing.T) {
	tests := []struct {
		name       string
		geminiKeys []config.GeminiKey
		wantLen    int
		validate   func(*testing.T, []*coreauth.Auth)
	}{
		{
			name: "single gemini key",
			geminiKeys: []config.GeminiKey{
				{APIKey: "test-key-123", Prefix: "team-a"},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Provider != "gemini" {
					t.Errorf("expected provider gemini, got %s", auths[0].Provider)
				}
				if auths[0].Prefix != "team-a" {
					t.Errorf("expected prefix team-a, got %s", auths[0].Prefix)
				}
				if auths[0].Label != "gemini-apikey" {
					t.Errorf("expected label gemini-apikey, got %s", auths[0].Label)
				}
				if auths[0].Attributes["api_key"] != "test-key-123" {
					t.Errorf("expected api_key test-key-123, got %s", auths[0].Attributes["api_key"])
				}
				if auths[0].Metadata != nil {
					t.Errorf("expected metadata to be nil when disable_cooling not set, got %v", auths[0].Metadata)
				}
				if auths[0].Status != coreauth.StatusActive {
					t.Errorf("expected status active, got %s", auths[0].Status)
				}
			},
		},
		{
			name: "gemini key disable cooling",
			geminiKeys: []config.GeminiKey{
				{APIKey: "test-key-123", Prefix: "team-a", DisableCooling: boolPointer(true)},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
					t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
				}
			},
		},
		{
			name: "gemini key with base url and proxy",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:   "api-key",
					BaseURL:  "https://custom.api.com",
					ProxyURL: "http://proxy.local:8080",
					Prefix:   "custom",
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["base_url"] != "https://custom.api.com" {
					t.Errorf("expected base_url https://custom.api.com, got %s", auths[0].Attributes["base_url"])
				}
				if auths[0].ProxyURL != "http://proxy.local:8080" {
					t.Errorf("expected proxy_url http://proxy.local:8080, got %s", auths[0].ProxyURL)
				}
			},
		},
		{
			name: "gemini key with headers",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:  "api-key",
					Headers: map[string]string{"X-Custom": "value"},
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["header:X-Custom"] != "value" {
					t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
				}
			},
		},
		{
			name: "empty api key skipped",
			geminiKeys: []config.GeminiKey{
				{APIKey: ""},
				{APIKey: "  "},
				{APIKey: "valid-key"},
			},
			wantLen: 1,
		},
		{
			name: "multiple gemini keys",
			geminiKeys: []config.GeminiKey{
				{APIKey: "key-1", Prefix: "a"},
				{APIKey: "key-2", Prefix: "b"},
				{APIKey: "key-3", Prefix: "c"},
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synth := NewConfigSynthesizer()
			ctx := &SynthesisContext{
				Config: &config.Config{
					GeminiKey: tt.geminiKeys,
				},
				Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != tt.wantLen {
				t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
			}

			if tt.validate != nil && len(auths) > 0 {
				tt.validate(t, auths)
			}
		})
	}
}

func TestConfigSynthesizer_ClaudeKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:         "sk-ant-api-xxx",
					Prefix:         "main",
					BaseURL:        "https://api.anthropic.com",
					DisableCooling: boolPointer(true),
					Models: []config.ClaudeModel{
						{Name: "claude-3-opus"},
						{Name: "claude-3-sonnet"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "claude" {
		t.Errorf("expected provider claude, got %s", auths[0].Provider)
	}
	if auths[0].Label != "claude-apikey" {
		t.Errorf("expected label claude-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "main" {
		t.Errorf("expected prefix main, got %s", auths[0].Prefix)
	}
	if auths[0].Attributes["api_key"] != "sk-ant-api-xxx" {
		t.Errorf("expected api_key sk-ant-api-xxx, got %s", auths[0].Attributes["api_key"])
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
	}
}

func TestConfigSynthesizer_ClaudeKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: ""},    // empty, should be skipped
				{APIKey: "   "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"X-Custom": "value"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:X-Custom"] != "value" {
		t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
	}
}

func TestConfigSynthesizer_CodexKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{
					APIKey:               "codex-key-123",
					Prefix:               "dev",
					BaseURL:              "https://api.openai.com",
					ProxyURL:             "http://proxy.local",
					Websockets:           true,
					DisableCooling:       boolPointer(true),
					DisableCodexCloaking: boolPointer(true),
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "codex" {
		t.Errorf("expected provider codex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "codex-apikey" {
		t.Errorf("expected label codex-apikey, got %s", auths[0].Label)
	}
	if auths[0].ProxyURL != "http://proxy.local" {
		t.Errorf("expected proxy_url http://proxy.local, got %s", auths[0].ProxyURL)
	}
	if auths[0].Attributes["websockets"] != "true" {
		t.Errorf("expected websockets=true, got %s", auths[0].Attributes["websockets"])
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
	}
	if auths[0].Attributes[coreauth.AttributeCodexDisableCloaking] != "true" {
		t.Errorf("expected disable_codex_cloaking=true, got %s", auths[0].Attributes[coreauth.AttributeCodexDisableCloaking])
	}
}

func TestConfigSynthesizer_CodexKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{APIKey: ""},   // empty, should be skipped
				{APIKey: "  "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"Authorization": "Bearer xyz"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:Authorization"] != "Bearer xyz" {
		t.Errorf("expected header:Authorization=Bearer xyz, got %s", auths[0].Attributes["header:Authorization"])
	}
}

// Ported from upstream CLIProxyAPI commit 54d4f4c0
// (internal/watcher/synthesizer/config_test.go TestConfigSynthesizer_MetaKeys).
func TestConfigSynthesizer_MetaKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	disableCooling := true
	ctx := &SynthesisContext{
		Config: &config.Config{
			MetaKey: []config.MetaKey{
				{
					APIKey:         "meta-secret",
					BaseURL:        "https://api.meta.ai/v1",
					ProxyURL:       "http://proxy.local",
					DisableCooling: &disableCooling,
					Models: []config.CodexModel{
						{Name: "muse-spark-1.3", Alias: "muse-spark-1.3"},
					},
					Headers: map[string]string{"X-Custom": "value"},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := synth.Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "meta" {
		t.Fatalf("provider = %q, want meta", auth.Provider)
	}
	if auth.Label != "meta-apikey" {
		t.Fatalf("label = %q, want meta-apikey", auth.Label)
	}
	if auth.Attributes["base_url"] != "https://api.meta.ai/v1" {
		t.Fatalf("base_url = %q, want https://api.meta.ai/v1", auth.Attributes["base_url"])
	}
	if auth.Attributes["api_key"] != "meta-secret" {
		t.Fatalf("api_key = %q, want meta-secret", auth.Attributes["api_key"])
	}
	if auth.Attributes["auth_kind"] != "apikey" {
		t.Fatalf("auth_kind = %q, want apikey", auth.Attributes["auth_kind"])
	}
	if auth.Attributes["header:X-Custom"] != "value" {
		t.Fatalf("custom header = %q, want value", auth.Attributes["header:X-Custom"])
	}
	if auth.ProxyURL != "http://proxy.local" {
		t.Fatalf("proxy URL = %q, want http://proxy.local", auth.ProxyURL)
	}
	if v, ok := auth.Metadata["disable_cooling"].(bool); !ok || !v {
		t.Fatalf("disable_cooling = %v, want true", auth.Metadata["disable_cooling"])
	}
	if auth.Attributes["models_hash"] == "" {
		t.Fatal("expected models_hash attribute to be set for configured models")
	}
}

func TestConfigSynthesizer_OpenAICompat(t *testing.T) {
	tests := []struct {
		name    string
		compat  []config.OpenAICompatibility
		wantLen int
	}{
		{
			name: "with APIKeyEntries",
			compat: []config.OpenAICompatibility{
				{
					Name:           "CustomProvider",
					BaseURL:        "https://custom.api.com",
					DisableCooling: boolPointer(true),
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-1"},
						{APIKey: "key-2"},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "empty APIKeyEntries included (legacy)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "EmptyKeys",
					BaseURL: "https://empty.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: ""},
						{APIKey: "   "},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "without APIKeyEntries (fallback)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "NoKeyProvider",
					BaseURL: "https://no-key.api.com",
				},
			},
			wantLen: 1,
		},
		{
			name: "empty name defaults",
			compat: []config.OpenAICompatibility{
				{
					Name:    "",
					BaseURL: "https://default.api.com",
				},
			},
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synth := NewConfigSynthesizer()
			ctx := &SynthesisContext{
				Config: &config.Config{
					OpenAICompatibility: tt.compat,
				},
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != tt.wantLen {
				t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
			}
			if tt.name == "with APIKeyEntries" {
				for i := range auths {
					if v, ok := auths[i].Metadata["disable_cooling"].(bool); !ok || !v {
						t.Fatalf("expected auth[%d].disable_cooling=true, got %v", i, auths[i].Metadata["disable_cooling"])
					}
				}
			}
		})
	}
}

func TestConfigSynthesizer_VertexCompat(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key-123",
					BaseURL: "https://vertex.googleapis.com",
					Prefix:  "vertex-prod",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "vertex" {
		t.Errorf("expected provider vertex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "vertex-apikey" {
		t.Errorf("expected label vertex-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "vertex-prod" {
		t.Errorf("expected prefix vertex-prod, got %s", auths[0].Prefix)
	}
}

func TestConfigSynthesizer_VertexCompat_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "", BaseURL: "https://vertex.api"},   // empty key creates auth without api_key attr
				{APIKey: "  ", BaseURL: "https://vertex.api"}, // whitespace key creates auth without api_key attr
				{APIKey: "valid-key", BaseURL: "https://vertex.api", Headers: map[string]string{"X-Vertex": "test"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Vertex compat doesn't skip empty keys - it creates auths without api_key attribute
	if len(auths) != 3 {
		t.Fatalf("expected 3 auths, got %d", len(auths))
	}
	// First two should not have api_key attribute
	if _, ok := auths[0].Attributes["api_key"]; ok {
		t.Error("expected first auth to not have api_key attribute")
	}
	if _, ok := auths[1].Attributes["api_key"]; ok {
		t.Error("expected second auth to not have api_key attribute")
	}
	// Third should have headers
	if auths[2].Attributes["header:X-Vertex"] != "test" {
		t.Errorf("expected header:X-Vertex=test, got %s", auths[2].Attributes["header:X-Vertex"])
	}
}

func TestConfigSynthesizer_OpenAICompat_WithModelsHash(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "TestProvider",
					BaseURL: "https://test.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-a"},
						{Name: "model-b"},
					},
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-with-models"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if auths[0].Attributes["api_key"] != "key-with-models" {
		t.Errorf("expected api_key key-with-models, got %s", auths[0].Attributes["api_key"])
	}
}

func TestConfigSynthesizer_OpenAICompat_FallbackWithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "NoKeyWithModels",
					BaseURL: "https://nokey.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-x"},
					},
					Headers: map[string]string{"X-API": "header-value"},
					// No APIKeyEntries - should use fallback path
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in fallback path")
	}
	if auths[0].Attributes["header:X-API"] != "header-value" {
		t.Errorf("expected header:X-API=header-value, got %s", auths[0].Attributes["header:X-API"])
	}
}

func TestConfigSynthesizer_VertexCompat_WithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key",
					BaseURL: "https://vertex.api",
					Models: []config.VertexCompatModel{
						{Name: "gemini-pro", Alias: "pro"},
						{Name: "gemini-ultra", Alias: "ultra"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in vertex auth with models")
	}
}

func TestConfigSynthesizer_IDStability(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "stable-key", Prefix: "test"},
		},
	}

	// Generate IDs twice with fresh generators
	synth1 := NewConfigSynthesizer()
	ctx1 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths1, _ := synth1.Synthesize(ctx1)

	synth2 := NewConfigSynthesizer()
	ctx2 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths2, _ := synth2.Synthesize(ctx2)

	if auths1[0].ID != auths2[0].ID {
		t.Errorf("same config should produce same ID: got %q and %q", auths1[0].ID, auths2[0].ID)
	}
}

func TestConfigSynthesizer_AllProviders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey: []config.GeminiKey{
				{APIKey: "gemini-key"},
			},
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "claude-key"},
			},
			CodexKey: []config.CodexKey{
				{APIKey: "codex-key"},
			},
			MetaKey: []config.MetaKey{
				{APIKey: "meta-key", BaseURL: "https://api.meta.ai/v1"},
			},
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "compat", BaseURL: "https://compat.api"},
			},
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "vertex-key", BaseURL: "https://vertex.api"},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 6 {
		t.Fatalf("expected 6 auths, got %d", len(auths))
	}

	providers := make(map[string]bool)
	for _, a := range auths {
		providers[a.Provider] = true
	}

	expected := []string{"gemini", "claude", "codex", "meta", "compat", "vertex"}
	for _, p := range expected {
		if !providers[p] {
			t.Errorf("expected provider %s not found", p)
		}
	}
}

func TestConfigSynthesizer_StaticProviderWeights(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey: []config.GeminiKey{{APIKey: "gemini-key", Weight: 2}},
			ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", Weight: 3}},
			CodexKey:  []config.CodexKey{{APIKey: "codex-key", Weight: 4}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "compat",
				BaseURL: "https://compat.api",
				Weight:  5,
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "compat-key"},
				},
			}},
			VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "vertex-key", BaseURL: "https://vertex.api", Weight: 6}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"gemini": "2",
		"claude": "3",
		"codex":  "4",
		"compat": "5",
		"vertex": "6",
	}
	for _, auth := range auths {
		if got, ok := want[auth.Provider]; ok && auth.Attributes["weight"] != got {
			t.Fatalf("expected %s weight %s, got %q", auth.Provider, got, auth.Attributes["weight"])
		}
	}
}

func TestConfigSynthesizer_DefaultStaticProviderWeightsAreOmitted(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey: []config.GeminiKey{{APIKey: "gemini-key", Weight: coreauth.DefaultCredentialWeight}},
			ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key"}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, auth := range auths {
		if _, ok := auth.Attributes["weight"]; ok {
			t.Fatalf("expected default weight to be omitted for %s, got %v", auth.Provider, auth.Attributes)
		}
	}
}

func TestConfigSynthesizer_RequestScopedErrors(t *testing.T) {
	synth := NewConfigSynthesizer()
	rules := []config.RequestScopedErrorRule{
		{
			Status: 400,
			Match:  []string{"maximum_context_length"},
			Action: "stop",
		},
	}

	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey: []config.GeminiKey{
				{APIKey: "gemini-key", RequestScopedErrors: rules},
			},
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "claude-key", RequestScopedErrors: rules},
			},
			CodexKey: []config.CodexKey{
				{APIKey: "codex-key", BaseURL: "https://codex.api", RequestScopedErrors: rules},
			},
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:                "compat",
					BaseURL:             "https://compat.api",
					RequestScopedErrors: rules,
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "compat-key"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := synth.Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}

	for _, auth := range auths {
		if auth.Metadata == nil {
			t.Fatalf("auth %s has nil metadata", auth.ID)
		}
		val, exists := auth.Metadata["request_scoped_errors"]
		if !exists {
			t.Fatalf("auth %s missing request_scoped_errors in metadata", auth.ID)
		}
		extracted, ok := val.([]config.RequestScopedErrorRule)
		if !ok || len(extracted) != 1 || extracted[0].Action != "stop" {
			t.Fatalf("auth %s unexpected request_scoped_errors: %#v", auth.ID, val)
		}
	}
}

// TestConfigSynthesizer_ConnectionPoolOverride pins the per-credential pool opt-in:
// the override declared on an API-key entry must reach the auth record so the
// generic proxy client can pool just that credential.
// Ported from upstream CLIProxyAPI internal/watcher/synthesizer/config_test.go (bf834311e6d6).
func TestConfigSynthesizer_ConnectionPoolOverride(t *testing.T) {
	enabled := true
	maxIdle := 4
	cfg := &config.Config{
		CodexKey: []config.CodexKey{{
			APIKey:   "ollama-key",
			BaseURL:  "https://ollama.com/v1",
			ProxyURL: "direct",
			ConnectionPool: &config.ProxyConnectionPoolConfig{
				Enabled:             &enabled,
				IdleConnTimeout:     "300s",
				MaxIdleConnsPerHost: &maxIdle,
			},
		}},
		ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", BaseURL: "https://api.anthropic.com"}},
	}
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var codexAuth, claudeAuth *coreauth.Auth
	for _, a := range auths {
		switch {
		case a.Provider == "codex":
			codexAuth = a
		case a.Provider == "claude":
			claudeAuth = a
		}
	}
	if codexAuth == nil || claudeAuth == nil {
		t.Fatalf("missing synthesised auths: codex=%v claude=%v", codexAuth, claudeAuth)
	}
	pool := codexAuth.ProxyConnectionPool
	if pool == nil {
		t.Fatal("codex override did not reach the auth record")
	}
	if pool.Enabled == nil || !*pool.Enabled || pool.IdleConnTimeout != "300s" || pool.MaxIdleConnsPerHost == nil || *pool.MaxIdleConnsPerHost != 4 {
		t.Fatalf("unexpected override: %+v", pool)
	}
	if claudeAuth.ProxyConnectionPool != nil {
		t.Fatal("entry without a connection-pool block must stay unpooled")
	}
	// The override must be copied, never aliased to live config values.
	*pool.Enabled = false
	if !enabled {
		t.Fatal("auth override aliases the config pointer")
	}
}

// TestConfigSynthesizer_EmptyConnectionPoolBlockOptsIn pins that writing the block
// without fields requests the pool defaults. Dropping it during synthesis disabled
// pooling for `connection-pool: {}` and for keys inheriting an empty group block.
// Upstream CLIProxyAPI 638f5f55e0d9.
func TestConfigSynthesizer_EmptyConnectionPoolBlockOptsIn(t *testing.T) {
	cfg := &config.Config{CodexKey: []config.CodexKey{{
		APIKey:         "empty-block",
		BaseURL:        "https://ollama.com/v1",
		ProxyURL:       "direct",
		ConnectionPool: &config.ProxyConnectionPoolConfig{},
	}}}
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auths = %d, want 1", len(auths))
	}
	if auths[0].ProxyConnectionPool == nil {
		t.Fatal("an empty connection-pool block is still an opt-in and must reach the auth record")
	}
	if enabled := auths[0].ProxyConnectionPool.Enabled; enabled != nil {
		t.Fatalf("empty block must leave enabled unset (defaults to on), got %v", *enabled)
	}
}

// TestConfigSynthesizer_OpenAICompatConnectionPoolMerge pins field-level inheritance
// for OpenAI-compatible keys: a per-key block only replaces the fields it sets, and a
// keyless provider inherits the group block.
// Ported from upstream CLIProxyAPI internal/watcher/synthesizer/config_test.go (bf834311e6d6, 8643ea56d520).
func TestConfigSynthesizer_OpenAICompatConnectionPoolMerge(t *testing.T) {
	groupEnabled := true
	groupMax := 2
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "compat",
			BaseURL: "https://compat.example.com",
			ConnectionPool: &config.ProxyConnectionPoolConfig{
				Enabled:             &groupEnabled,
				IdleConnTimeout:     "300s",
				MaxIdleConnsPerHost: &groupMax,
			},
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{
				{APIKey: "inherits"},
				{APIKey: "partial", ConnectionPool: &config.ProxyConnectionPoolConfig{IdleConnTimeout: "60s"}},
			},
		}},
	}
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byKey := map[string]*coreauth.ProxyConnectionPoolOverride{}
	for _, a := range auths {
		byKey[a.Attributes["api_key"]] = a.ProxyConnectionPool
	}
	inherits := byKey["inherits"]
	if inherits == nil || inherits.Enabled == nil || !*inherits.Enabled || inherits.IdleConnTimeout != "300s" {
		t.Fatalf("key without an override must inherit the group block: %+v", inherits)
	}
	if inherits.MaxIdleConnsPerHost == nil || *inherits.MaxIdleConnsPerHost != 2 {
		t.Fatalf("inheriting key lost max-idle: %+v", inherits)
	}
	partial := byKey["partial"]
	if partial == nil || partial.Enabled == nil || !*partial.Enabled {
		t.Fatalf("partial override must keep the group's enabled value: %+v", partial)
	}
	if partial.IdleConnTimeout != "60s" {
		t.Fatalf("partial override idle = %q, want 60s", partial.IdleConnTimeout)
	}
	if partial.MaxIdleConnsPerHost == nil || *partial.MaxIdleConnsPerHost != 2 {
		t.Fatalf("partial override must inherit max-idle-conns-per-host: %+v", partial)
	}
}
