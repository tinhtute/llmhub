package registry

import (
	"reflect"
	"testing"
)

func TestGetAvailableModelsReturnsClonedSnapshots(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One"}})

	first := r.GetAvailableModels("openai")
	if len(first) != 1 {
		t.Fatalf("expected 1 model, got %d", len(first))
	}
	first[0]["id"] = "mutated"
	first[0]["display_name"] = "Mutated"

	second := r.GetAvailableModels("openai")
	if got := second[0]["id"]; got != "m1" {
		t.Fatalf("expected cached snapshot to stay isolated, got id %v", got)
	}
	if got := second[0]["display_name"]; got != "Model One" {
		t.Fatalf("expected cached snapshot to stay isolated, got display_name %v", got)
	}
}

func TestGetAvailableModelsInvalidatesCacheOnRegistryChanges(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One"}})

	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if got := models[0]["display_name"]; got != "Model One" {
		t.Fatalf("expected initial display_name Model One, got %v", got)
	}

	r.RegisterClient("client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One Updated"}})
	models = r.GetAvailableModels("openai")
	if got := models[0]["display_name"]; got != "Model One Updated" {
		t.Fatalf("expected updated display_name after cache invalidation, got %v", got)
	}

	r.SuspendClientModel("client-1", "m1", "manual")
	models = r.GetAvailableModels("openai")
	if len(models) != 0 {
		t.Fatalf("expected no available models after suspension, got %d", len(models))
	}

	r.ResumeClientModel("client-1", "m1")
	models = r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected model to reappear after resume, got %d", len(models))
	}
}

// --- R6 Gemini catalog token-limit fallback (upstream v8.0.41 83cb1b95) -----
// A Gemini catalog entry that only carries the generic limit fields must expose
// Gemini's inputTokenLimit / outputTokenLimit. An entry that already carries the
// dedicated limits must come back exactly as before, and an entry with no limit
// at all must not gain the fields.

func TestGetAvailableModelsGeminiIncludesTokenLimits(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{
		{
			ID:                  "gemini-3.8-flash",
			Name:                "models/gemini-3.8-flash",
			DisplayName:         "Gemini 3.8 Flash",
			ContextLength:       1048576,
			MaxCompletionTokens: 65536,
		},
		{
			ID:               "gemini-2.5-pro",
			Name:             "models/gemini-2.5-pro",
			DisplayName:      "Gemini 2.5 Pro",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
		},
		{
			ID:   "gemini-no-limits",
			Name: "models/gemini-no-limits",
		},
		{
			// Both families present: the dedicated Gemini limits must win.
			ID:                  "gemini-precedence",
			Name:                "models/gemini-precedence",
			InputTokenLimit:     1000,
			OutputTokenLimit:    100,
			ContextLength:       999999,
			MaxCompletionTokens: 888888,
		},
	})

	models := r.GetAvailableModels("gemini")
	byName := make(map[string]map[string]any, len(models))
	for _, model := range models {
		name, _ := model["name"].(string)
		byName[name] = model
	}

	fromGeneric, ok := byName["models/gemini-3.8-flash"]
	if !ok {
		t.Fatalf("expected models/gemini-3.8-flash in available models, got %v", byName)
	}
	if got := fromGeneric["inputTokenLimit"]; got != 1048576 {
		t.Fatalf("gemini-3.8-flash inputTokenLimit = %v, want 1048576", got)
	}
	if got := fromGeneric["outputTokenLimit"]; got != 65536 {
		t.Fatalf("gemini-3.8-flash outputTokenLimit = %v, want 65536", got)
	}

	// Byte-compat: a fully specified entry must be unchanged field for field.
	gotExplicit, ok := byName["models/gemini-2.5-pro"]
	if !ok {
		t.Fatalf("expected models/gemini-2.5-pro in available models, got %v", byName)
	}
	wantExplicit := map[string]any{
		"name":             "models/gemini-2.5-pro",
		"displayName":      "Gemini 2.5 Pro",
		"inputTokenLimit":  1048576,
		"outputTokenLimit": 65536,
	}
	if !reflect.DeepEqual(gotExplicit, wantExplicit) {
		t.Fatalf("explicit-limit entry changed:\n got: %#v\nwant: %#v", gotExplicit, wantExplicit)
	}

	noLimits, ok := byName["models/gemini-no-limits"]
	if !ok {
		t.Fatalf("expected models/gemini-no-limits in available models, got %v", byName)
	}
	if _, exists := noLimits["inputTokenLimit"]; exists {
		t.Fatalf("expected inputTokenLimit to be omitted when unknown, got %v", noLimits)
	}
	if _, exists := noLimits["outputTokenLimit"]; exists {
		t.Fatalf("expected outputTokenLimit to be omitted when unknown, got %v", noLimits)
	}

	precedence, ok := byName["models/gemini-precedence"]
	if !ok {
		t.Fatalf("expected models/gemini-precedence in available models, got %v", byName)
	}
	if got := precedence["inputTokenLimit"]; got != 1000 {
		t.Fatalf("gemini-precedence inputTokenLimit = %v, want the dedicated limit 1000", got)
	}
	if got := precedence["outputTokenLimit"]; got != 100 {
		t.Fatalf("gemini-precedence outputTokenLimit = %v, want the dedicated limit 100", got)
	}
}
