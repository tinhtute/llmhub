package cliproxy

import (
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
)

func TestConfiguredModelsUseDisplayNameWithoutChangingID(t *testing.T) {
	tests := []struct {
		name        string
		modelID     string
		displayName string
		models      []*ModelInfo
	}{
		{
			name:        "claude",
			modelID:     "claude-alias",
			displayName: "Claude Friendly",
			models: buildClaudeConfigModels(&config.ClaudeKey{Models: []config.ClaudeModel{{
				Name: "claude-upstream", Alias: "claude-alias", DisplayName: " Claude Friendly ",
			}}}),
		},
		{
			name:        "gemini",
			modelID:     "gemini-alias",
			displayName: "Gemini Friendly",
			models: buildGeminiConfigModels(&config.GeminiKey{Models: []config.GeminiModel{{
				Name: "gemini-upstream", Alias: "gemini-alias", DisplayName: " Gemini Friendly ",
			}}}),
		},
		{
			name:        "vertex",
			modelID:     "vertex-alias",
			displayName: "Vertex Friendly",
			models: buildVertexCompatConfigModels(&config.VertexCompatKey{Models: []config.VertexCompatModel{{
				Name: "vertex-upstream", Alias: "vertex-alias", DisplayName: " Vertex Friendly ",
			}}}),
		},
		{
			name:        "openai compatibility",
			modelID:     "compat-alias",
			displayName: "Compat Friendly",
			models: buildOpenAICompatibilityConfigModels(&config.OpenAICompatibility{
				Name: "compat",
				Models: []config.OpenAICompatibilityModel{{
					Name: "compat-upstream", Alias: "compat-alias", DisplayName: " Compat Friendly ",
				}},
			}),
		},
		{
			name:        "codex builtin replacement",
			modelID:     "gpt-image-2",
			displayName: "Configured Image Two",
			models: buildCodexConfigModels(&config.CodexKey{Models: []config.CodexModel{{
				Name: "gpt-image-2", Alias: "gpt-image-2", DisplayName: " Configured Image Two ",
			}}}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := findModelInfoByID(tt.models, tt.modelID)
			if model == nil {
				t.Fatalf("model %q not found", tt.modelID)
			}
			if model.ID != tt.modelID {
				t.Fatalf("model id = %q, want %q", model.ID, tt.modelID)
			}
			if model.DisplayName != tt.displayName {
				t.Fatalf("display name = %q, want %q", model.DisplayName, tt.displayName)
			}
		})
	}
}

func TestConfiguredModelDisplayNameSurvivesPrefixClone(t *testing.T) {
	models := buildGeminiConfigModels(&config.GeminiKey{Models: []config.GeminiModel{{
		Name: "gemini-upstream", Alias: "gemini-alias", DisplayName: "Gemini Friendly",
	}}})

	prefixed := applyModelPrefixes(models, "team", false)
	original := findModelInfoByID(prefixed, "gemini-alias")
	alias := findModelInfoByID(prefixed, "team/gemini-alias")
	if original == nil || alias == nil {
		t.Fatalf("expected original and prefixed models, got %+v", prefixed)
	}
	if original.DisplayName != "Gemini Friendly" || alias.DisplayName != "Gemini Friendly" {
		t.Fatalf("display names = %q and %q", original.DisplayName, alias.DisplayName)
	}
}

func TestOAuthModelAliasDisplayNameOnlyChangesPresentation(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{
				Name: "gpt-upstream", Alias: "gpt-alias", Fork: true, DisplayName: "Alias Friendly",
			}},
		},
	}
	models := []*ModelInfo{{
		ID: "gpt-upstream", Name: "models/gpt-upstream", DisplayName: "Upstream Friendly", OwnedBy: "openai",
	}}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 2 {
		t.Fatalf("expected original and alias, got %d models", len(out))
	}
	original := findModelInfoByID(out, "gpt-upstream")
	alias := findModelInfoByID(out, "gpt-alias")
	if original == nil || alias == nil {
		t.Fatalf("expected original and alias models, got %+v", out)
	}
	if original.Name != "models/gpt-upstream" || original.DisplayName != "Upstream Friendly" {
		t.Fatalf("original model changed: %+v", original)
	}
	if alias.Name != "models/gpt-alias" || alias.DisplayName != "Alias Friendly" || alias.OwnedBy != "openai" {
		t.Fatalf("alias model metadata = %+v", alias)
	}
}

func findModelInfoByID(models []*ModelInfo, id string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

// TestConfiguredModelsInheritNativeCapabilities verifies that config-declared
// models pick up per-model NativeCapabilities from the static catalog via the
// metadata channel (ported from upstream 4311ae874774).
func TestConfiguredModelsInheritNativeCapabilities(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		models  []*ModelInfo
	}{
		{
			name:    "claude",
			channel: "claude",
			models: buildClaudeConfigModels(&config.ClaudeKey{Models: []config.ClaudeModel{{
				Name: "claude-opus-5", Alias: "claude-opus-5",
			}}}),
		},
		{
			name:    "codex",
			channel: "codex",
			models: buildCodexConfigModels(&config.CodexKey{Models: []config.CodexModel{{
				Name: "gpt-5.6-terra", Alias: "gpt-5.6-terra",
			}}}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := findModelInfoByID(tt.models, tt.models[0].ID)
			if model == nil {
				t.Fatalf("configured model not found")
			}
			static := registry.LookupStaticModelInfoByChannel(model.ID, tt.channel)
			if static == nil || static.NativeCapabilities == nil {
				t.Fatalf("static catalog has no NativeCapabilities for %s", model.ID)
			}
			if model.NativeCapabilities == nil {
				t.Fatalf("configured model %s did not inherit NativeCapabilities", model.ID)
			}
			if static.NativeCapabilities.WebSearch != nil {
				if model.NativeCapabilities.WebSearch == nil {
					t.Fatalf("configured model %s lost WebSearch capability", model.ID)
				}
				if *model.NativeCapabilities.WebSearch != *static.NativeCapabilities.WebSearch {
					t.Fatalf("configured model %s WebSearch = %v, want %v", model.ID, *model.NativeCapabilities.WebSearch, *static.NativeCapabilities.WebSearch)
				}
			}
		})
	}
}

// TestConfiguredModelsDoNotInheritCrossChannelCapabilities verifies the channel
// lookup is provenance-preserving: a claude-aliased model must not pick up a
// codex-only capability and vice versa.
func TestConfiguredModelsDoNotInheritCrossChannelCapabilities(t *testing.T) {
	// claude-opus-5 has native web_search in the claude channel only.
	models := buildCodexConfigModels(&config.CodexKey{Models: []config.CodexModel{{
		Name: "claude-opus-5", Alias: "claude-opus-5",
	}}})
	model := findModelInfoByID(models, "claude-opus-5")
	if model == nil {
		t.Fatal("configured codex model not found")
	}
	if model.NativeCapabilities != nil {
		t.Fatalf("codex-declared claude-opus-5 inherited cross-channel capabilities: %+v", model.NativeCapabilities)
	}
}

// TestApplyModelPrefixesDeepCopiesNativeCapabilities verifies prefixed catalog
// entries do not alias the source model's NativeCapabilities (ported from
// upstream 4311ae874774).
func TestApplyModelPrefixesDeepCopiesNativeCapabilities(t *testing.T) {
	webSearch := true
	models := []*ModelInfo{{
		ID: "claude-opus-5", NativeCapabilities: &registry.NativeCapabilities{WebSearch: &webSearch},
	}}
	prefixed := applyModelPrefixes(models, "team", false)
	clone := findModelInfoByID(prefixed, "team/claude-opus-5")
	if clone == nil {
		t.Fatal("missing team/claude-opus-5")
	}
	if clone.NativeCapabilities == nil || clone.NativeCapabilities.WebSearch == nil || !*clone.NativeCapabilities.WebSearch {
		t.Fatalf("prefixed model did not inherit native capabilities: %+v", clone)
	}
	*clone.NativeCapabilities.WebSearch = false
	original := findModelInfoByID(prefixed, "claude-opus-5")
	if original == nil || original.NativeCapabilities == nil || original.NativeCapabilities.WebSearch == nil || !*original.NativeCapabilities.WebSearch {
		t.Fatal("prefixed capability metadata aliases the source model")
	}
}

// TestApplyOAuthModelAliasDeepCopiesNativeCapabilities verifies aliased catalog
// entries do not alias the source model's NativeCapabilities.
func TestApplyOAuthModelAliasDeepCopiesNativeCapabilities(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5.6-terra", Alias: "gpt-alias", Fork: true}},
		},
	}
	webSearch := true
	models := []*ModelInfo{{
		ID: "gpt-5.6-terra", OwnedBy: "openai", NativeCapabilities: &registry.NativeCapabilities{WebSearch: &webSearch},
	}}
	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	alias := findModelInfoByID(out, "gpt-alias")
	if alias == nil {
		t.Fatal("missing aliased model")
	}
	if alias.NativeCapabilities == nil || alias.NativeCapabilities.WebSearch == nil || !*alias.NativeCapabilities.WebSearch {
		t.Fatalf("aliased model did not inherit native capabilities: %+v", alias)
	}
	*alias.NativeCapabilities.WebSearch = false
	original := findModelInfoByID(out, "gpt-5.6-terra")
	if original == nil || original.NativeCapabilities == nil || original.NativeCapabilities.WebSearch == nil || !*original.NativeCapabilities.WebSearch {
		t.Fatal("aliased capability metadata aliases the source model")
	}
}

func TestBuildCodexConfigModelsIncludesImage26Defaults(t *testing.T) {
	models := buildCodexConfigModels(&config.CodexKey{})
	present := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			present[model.ID] = true
		}
	}
	for _, modelID := range []string{"gpt-image-1.5", "gpt-image-2", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst", "gpt-image-2.6"} {
		if !present[modelID] {
			t.Errorf("missing default codex model %q", modelID)
		}
	}
}
