package registry

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGrok45ModelDefinition(t *testing.T) {
	got := requireModelDefinition(t, GetXAIModels(), "grok-4.5")
	want := &ModelInfo{
		ID:                        "grok-4.5",
		Object:                    "model",
		Created:                   1783526400,
		OwnedBy:                   "xai",
		Type:                      "xai",
		DisplayName:               "Grok 4.5",
		Name:                      "grok-4.5",
		Description:               "SpaceXAI's intelligent coding model for agentic software, engineering, and workflow tasks.",
		ContextLength:             500000,
		MaxCompletionTokens:       65536,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
		Thinking: &ThinkingSupport{
			Levels: []string{"low", "medium", "high"},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grok-4.5 definition = %#v, want %#v", got, want)
	}
}

func TestGeminiProductionModelDefinitions(t *testing.T) {
	generationMethods := []string{
		"generateContent",
		"countTokens",
		"createCachedContent",
		"batchGenerateContent",
	}
	tests := []struct {
		id               string
		created          int64
		displayName      string
		version          string
		description      string
		levels           []string
		inputModalities  []string
		outputModalities []string
	}{
		{
			id:               "gemini-3-pro",
			created:          1737158400,
			displayName:      "Gemini 3 Pro",
			version:          "3.0",
			description:      "Gemini 3 Pro",
			levels:           []string{"low", "high"},
			inputModalities:  []string{"text", "image", "audio", "video"},
			outputModalities: []string{"text"},
		},
		{
			id:               "gemini-3-flash",
			created:          1765929600,
			displayName:      "Gemini 3 Flash",
			version:          "3.0",
			description:      "Our most intelligent model built for speed, combining frontier intelligence with superior search and grounding.",
			levels:           []string{"minimal", "low", "medium", "high"},
			inputModalities:  []string{"text", "image", "audio", "video"},
			outputModalities: []string{"text"},
		},
		{
			id:               "gemini-3.1-pro",
			created:          1771459200,
			displayName:      "Gemini 3.1 Pro",
			version:          "3.1",
			description:      "Gemini 3.1 Pro",
			levels:           []string{"low", "medium", "high"},
			inputModalities:  []string{"text", "image", "audio", "video"},
			outputModalities: []string{"text"},
		},
		{
			id:               "gemini-3.1-flash-image",
			created:          1771459200,
			displayName:      "Gemini 3.1 Flash Image",
			version:          "3.1",
			description:      "Gemini 3.1 Flash Image",
			levels:           []string{"minimal", "high"},
			inputModalities:  []string{"text", "image"},
			outputModalities: []string{"text", "image"},
		},
		{
			id:               "gemini-3.1-flash-lite",
			created:          1776288000,
			displayName:      "Gemini 3.1 Flash Lite",
			version:          "3.1",
			description:      "Our smallest and most cost effective model, built for at scale usage.",
			levels:           []string{"minimal", "low", "medium", "high"},
			inputModalities:  []string{"text", "image", "audio", "video"},
			outputModalities: []string{"text"},
		},
		{
			id:               "gemini-3-pro-image",
			created:          1737158400,
			displayName:      "Gemini 3 Pro Image",
			version:          "3.0",
			description:      "Gemini 3 Pro Image",
			levels:           []string{"low", "high"},
			inputModalities:  []string{"text", "image"},
			outputModalities: []string{"text", "image"},
		},
	}

	models := GetGeminiVertexModels()
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := requireModelDefinition(t, models, tt.id)
			want := &ModelInfo{
				ID:                         tt.id,
				Object:                     "model",
				Created:                    tt.created,
				OwnedBy:                    "google",
				Type:                       "gemini",
				DisplayName:                tt.displayName,
				Name:                       "models/" + tt.id,
				Version:                    tt.version,
				Description:                tt.description,
				InputTokenLimit:            1048576,
				OutputTokenLimit:           65536,
				SupportedGenerationMethods: generationMethods,
				SupportedInputModalities:   tt.inputModalities,
				SupportedOutputModalities:  tt.outputModalities,
				Thinking: &ThinkingSupport{
					Min:            128,
					Max:            32768,
					DynamicAllowed: true,
					Levels:         tt.levels,
				},
			}

			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s definition = %#v, want %#v", tt.id, got, want)
			}
		})
	}
}

func TestGeminiProductionAndPreviewModelsCoexist(t *testing.T) {
	pairs := []struct {
		production string
		preview    string
	}{
		{production: "gemini-3-pro", preview: "gemini-3-pro-preview"},
		{production: "gemini-3-flash", preview: "gemini-3-flash-preview"},
		{production: "gemini-3.1-pro", preview: "gemini-3.1-pro-preview"},
		{production: "gemini-3.1-flash-image", preview: "gemini-3.1-flash-image-preview"},
		{production: "gemini-3.1-flash-lite", preview: "gemini-3.1-flash-lite-preview"},
		{production: "gemini-3-pro-image", preview: "gemini-3-pro-image-preview"},
	}

	models := GetGeminiVertexModels()
	for _, pair := range pairs {
		t.Run(pair.production, func(t *testing.T) {
			requireModelDefinition(t, models, pair.production)
			requireModelDefinition(t, models, pair.preview)
		})
	}
}

func TestUpstreamCheckpointModelCatalogAdditions(t *testing.T) {
	tests := []struct {
		channel string
		models  []*ModelInfo
		ids     []string
	}{
		{channel: "claude", models: GetClaudeModels(), ids: []string{"claude-opus-4-8", "claude-opus-5", "claude-sonnet-5", "claude-fable-5"}},
		{channel: "gemini", models: GetGeminiModels(), ids: []string{"gemini-3.5-flash-lite", "gemini-3.6-flash"}},
		{channel: "vertex", models: GetGeminiVertexModels(), ids: []string{"gemini-3.5-flash-lite", "gemini-3.6-flash"}},
		{channel: "aistudio", models: GetAIStudioModels(), ids: []string{"gemini-3.5-flash-lite", "gemini-3.6-flash"}},
		{channel: "codex-free", models: GetCodexFreeModels(), ids: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
		{channel: "codex-team", models: GetCodexTeamModels(), ids: []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}},
		{channel: "codex-plus", models: GetCodexPlusModels(), ids: []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}},
		{channel: "codex-pro", models: GetCodexProModels(), ids: []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}},
		{channel: "kimi", models: GetKimiModels(), ids: []string{"kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k3", "kimi-k3-256k"}},
		{channel: "antigravity", models: GetAntigravityModels(), ids: []string{"gemini-3.6-flash-high", "gemini-3.5-flash-extra-low"}},
		{channel: "xai", models: GetXAIModels(), ids: []string{"grok-composer-2.5-fast"}},
	}

	for _, tt := range tests {
		t.Run(tt.channel, func(t *testing.T) {
			for _, id := range tt.ids {
				requireModelDefinition(t, tt.models, id)
			}
		})
	}
}

func TestLocalKiroModelDefinitionsRemainAvailable(t *testing.T) {
	models := GetKiroModels()
	for _, id := range []string{"auto", "claude-sonnet-4.5", "claude-sonnet-4.5-thinking", "claude-sonnet-4.5-agentic", "claude-sonnet-4.5-thinking-agentic"} {
		requireModelDefinition(t, models, id)
	}
}

func requireModelDefinition(t *testing.T, models []*ModelInfo, id string) *ModelInfo {
	t.Helper()
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	t.Fatalf("model %q not found", id)
	return nil
}

func TestGemini37FlashRegisteredAcrossGeminiSurfaces(t *testing.T) {
	for name, models := range map[string][]*ModelInfo{
		"gemini":   GetGeminiModels(),
		"vertex":   GetGeminiVertexModels(),
		"aistudio": GetAIStudioModels(),
	} {
		got := requireModelDefinition(t, models, "gemini-3.7-flash")
		if got.Type != "gemini" {
			t.Fatalf("%s: gemini-3.7-flash type = %q, want gemini", name, got.Type)
		}
		if got.ContextLength != 1048576 {
			t.Fatalf("%s: gemini-3.7-flash context_length = %d, want 1048576", name, got.ContextLength)
		}
		if got.Thinking == nil || got.Thinking.Max != 65535 {
			t.Fatalf("%s: gemini-3.7-flash thinking max missing, want 65535", name)
		}
	}
}

// TestValidateModelsCatalog_Meta is ported from upstream CLIProxyAPI commit 06660dd6.
func TestValidateModelsCatalog_Meta(t *testing.T) {
	valid := &staticModelsJSON{
		Meta: []*ModelInfo{
			{ID: "muse-spark-1.3"},
		},
	}
	if err := validateModelsCatalog(valid); err != nil {
		t.Fatalf("expected valid Meta catalog to pass, got: %v", err)
	}

	withNull := &staticModelsJSON{
		Meta: []*ModelInfo{nil},
	}
	if err := validateModelsCatalog(withNull); err == nil {
		t.Fatal("expected error for Meta section with null model, got nil")
	}

	withEmptyID := &staticModelsJSON{
		Meta: []*ModelInfo{{ID: " "}},
	}
	if err := validateModelsCatalog(withEmptyID); err == nil {
		t.Fatal("expected error for Meta section with empty model id, got nil")
	}

	withDuplicate := &staticModelsJSON{
		Meta: []*ModelInfo{
			{ID: "muse-spark-1.3"},
			{ID: "muse-spark-1.3"},
		},
	}
	if err := validateModelsCatalog(withDuplicate); err == nil {
		t.Fatal("expected error for Meta section with duplicate model id, got nil")
	}
}

func TestGetStaticModelDefinitionsByChannelSupportsKimiAndKimiAI(t *testing.T) {
	for _, channel := range []string{"kimi", "kimi-ai", "kimi.ai", "kimi.com"} {
		models := GetStaticModelDefinitionsByChannel(channel)
		if len(models) == 0 {
			t.Fatalf("GetStaticModelDefinitionsByChannel(%s) returned no models", channel)
		}
	}
}

func TestCodexConfigurationUpdateCapability(t *testing.T) {
	for _, tier := range []struct {
		name    string
		models  func() []*ModelInfo
		capable []string
	}{
		{name: "free", models: GetCodexFreeModels, capable: []string{"gpt-6-luna"}},
		{name: "team", models: GetCodexTeamModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
		{name: "plus", models: GetCodexPlusModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
		{name: "pro", models: GetCodexProModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
	} {
		t.Run(tier.name, func(t *testing.T) {
			byID := make(map[string]*ModelInfo)
			for _, info := range tier.models() {
				if info != nil {
					byID[info.ID] = info
				}
			}
			for _, id := range append([]string{"gpt-5.5"}, tier.capable...) {
				info := byID[id]
				if info == nil {
					t.Errorf("%s is missing", id)
					continue
				}
				want := id != "gpt-5.5"
				if info.SupportConfigurationUpdate != want {
					t.Errorf("%s configuration_update = %v, want %v", id, info.SupportConfigurationUpdate, want)
				}
				if cloned := cloneModelInfo(info); cloned.SupportConfigurationUpdate != want {
					t.Errorf("%s clone configuration_update = %v, want %v", id, cloned.SupportConfigurationUpdate, want)
				}
				raw, errMarshal := json.Marshal(info)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if strings.Contains(string(raw), "support_configuration_update") || strings.Contains(string(raw), "SupportConfigurationUpdate") {
					t.Errorf("%s exposed configuration_update capability: %s", id, raw)
				}
			}
		})
	}

	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "true", raw: `{"id":"test","support_configuration_update":true}`, want: true},
		{name: "false", raw: `{"id":"test","support_configuration_update":false}`},
		{name: "absent", raw: `{"id":"test"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := ModelInfo{SupportConfigurationUpdate: true}
			if errUnmarshal := json.Unmarshal([]byte(tt.raw), &info); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if info.SupportConfigurationUpdate != tt.want {
				t.Errorf("capability = %v, want %v", info.SupportConfigurationUpdate, tt.want)
			}
		})
	}
}

func TestR16RegistryCatalogAdditions(t *testing.T) {
	// Gemini 4 Argon in standard gemini section
	geminiArgon := requireModelDefinition(t, GetGeminiModels(), "gemini-4-argon")
	if geminiArgon.Name != "models/gemini-4-argon" {
		t.Fatalf("gemini-4-argon name = %q, want models/gemini-4-argon", geminiArgon.Name)
	}
	if geminiArgon.ContextLength != 1048576 || geminiArgon.MaxCompletionTokens != 1048576 {
		t.Fatalf("gemini-4-argon limits = %d/%d, want 1048576/1048576", geminiArgon.ContextLength, geminiArgon.MaxCompletionTokens)
	}
	if !geminiArgon.HasNativeWebSearch() {
		t.Fatal("gemini-4-argon should have native web search")
	}

	// Meta Muse Wonder Full 1.0 variants
	for _, id := range []string{"muse-wonder-full-1.0", "muse-wonder-full-1.0-contributor"} {
		got := requireModelDefinition(t, GetMetaModels(), id)
		if got.ContextLength != 1007997 || got.MaxCompletionTokens != 128000 {
			t.Fatalf("%s limits = %d/%d, want 1007997/128000", id, got.ContextLength, got.MaxCompletionTokens)
		}
		if got.Thinking == nil || len(got.Thinking.Levels) != 6 {
			t.Fatalf("%s thinking levels = %#v, want 6 levels", id, got.Thinking)
		}
	}

	// Claude capability facts in static catalog
	sonnet55 := requireModelDefinition(t, GetClaudeModels(), "claude-sonnet-5-5")
	if sonnet55.Thinking == nil || sonnet55.Thinking.DisableType != "between_tools" {
		t.Fatalf("claude-sonnet-5-5 disable_type = %q, want between_tools", sonnet55.Thinking.DisableType)
	}
	if !sonnet55.Thinking.ZeroAllowed || !sonnet55.Thinking.DynamicAllowed {
		t.Fatalf("claude-sonnet-5-5 zero/dynamic = %v/%v, want true/true", sonnet55.Thinking.ZeroAllowed, sonnet55.Thinking.DynamicAllowed)
	}

	opus55 := requireModelDefinition(t, GetClaudeModels(), "claude-opus-5-5")
	if opus55.Thinking == nil || opus55.Thinking.ZeroAllowed || !opus55.Thinking.DynamicAllowed {
		t.Fatalf("claude-opus-5-5 zero/dynamic = %v/%v, want false/true", opus55.Thinking.ZeroAllowed, opus55.Thinking.DynamicAllowed)
	}

	opus47 := requireModelDefinition(t, GetClaudeModels(), "claude-opus-4-7")
	if opus47.Thinking == nil || !opus47.Thinking.ZeroAllowed || !opus47.Thinking.DynamicAllowed {
		t.Fatalf("claude-opus-4-7 zero/dynamic = %v/%v, want true/true", opus47.Thinking.ZeroAllowed, opus47.Thinking.DynamicAllowed)
	}
}

func TestGetXAIModelsPreservesVideo15LiteAfterCatalogRefresh(t *testing.T) {
	const liteID = "grok-imagine-video-1.5-lite"
	original := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	refreshed := *original
	remoteModel := &ModelInfo{ID: "refreshed-xai-model", OwnedBy: "xai", Type: "xai", DisplayName: "Refreshed model"}
	refreshed.XAI = []*ModelInfo{remoteModel}
	payload, err := json.Marshal(&refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if err := loadModelsFromBytes(payload, "test"); err != nil {
		t.Fatalf("publish catalog without Lite: %v", err)
	}

	models := GetXAIModels()
	foundLite, foundRemote := false, false
	for _, model := range models {
		if model.ID == liteID {
			foundLite = true
			if model.OwnedBy != "xai" || model.Type != "xai" ||
				!reflect.DeepEqual(model.SupportedInputModalities, []string{"text", "image"}) ||
				!reflect.DeepEqual(model.SupportedOutputModalities, []string{"video"}) {
				t.Fatalf("Lite lost xAI video capabilities after catalog refresh: %#v", model)
			}
		}
		if model.ID == remoteModel.ID {
			foundRemote = true
			if !reflect.DeepEqual(model, remoteModel) {
				t.Fatalf("refreshed model = %#v, want %#v", model, remoteModel)
			}
		}
	}
	if !foundLite || !foundRemote {
		t.Fatalf("catalog lost Lite or refreshed model: Lite=%v, refreshed=%v", foundLite, foundRemote)
	}

	const clientID = "test-xai-lite-catalog-refresh"
	registryRef := GetGlobalRegistry()
	registryRef.RegisterClient(clientID, "xai", models)
	t.Cleanup(func() { registryRef.UnregisterClient(clientID) })
	if !registryRef.ClientSupportsModel(clientID, liteID) {
		t.Fatal("xAI credential registration lost Lite support after catalog refresh")
	}
	if providers := registryRef.GetModelProviders(liteID); !reflect.DeepEqual(providers, []string{"xai"}) {
		t.Fatalf("Lite providers = %v, want [xai]", providers)
	}
}

func TestWithXAIBuiltinsReplacesVideo15LiteWithoutMutatingCatalog(t *testing.T) {
	const liteID = "grok-imagine-video-1.5-lite"
	stale := &ModelInfo{ID: liteID, DisplayName: "Stale Lite", SupportedInputModalities: []string{"text"}}
	duplicate := &ModelInfo{ID: " GROK-IMAGINE-VIDEO-1.5-LITE ", DisplayName: "Duplicate Lite"}
	custom := &ModelInfo{ID: "custom-xai-model", DisplayName: "Custom model"}
	input := []*ModelInfo{stale, custom, duplicate}
	for range 2 {
		models := WithXAIBuiltins(input)
		liteCount := 0
		foundCustom := false
		for _, model := range models {
			if strings.EqualFold(strings.TrimSpace(model.ID), liteID) {
				liteCount++
				if model.ID != liteID || model.DisplayName != "Grok Imagine Video 1.5 Lite" ||
					!reflect.DeepEqual(model.SupportedInputModalities, []string{"text", "image"}) {
					t.Fatalf("Lite builtin did not replace stale metadata: %#v", model)
				}
				model.SupportedInputModalities[0] = "mutated"
			}
			if model.ID == custom.ID {
				foundCustom = reflect.DeepEqual(model, custom)
			}
		}
		if liteCount != 1 || !foundCustom {
			t.Fatalf("Lite count = %d, custom preserved = %v, want 1 and true", liteCount, foundCustom)
		}
	}
	if input[0] != stale || input[1] != custom || input[2] != duplicate ||
		stale.DisplayName != "Stale Lite" || !reflect.DeepEqual(stale.SupportedInputModalities, []string{"text"}) ||
		duplicate.ID != " GROK-IMAGINE-VIDEO-1.5-LITE " || duplicate.DisplayName != "Duplicate Lite" {
		t.Fatal("builtin merge mutated the input catalog")
	}
}
