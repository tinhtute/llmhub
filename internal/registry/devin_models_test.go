package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestValidateDevinModelsJSON covers the catalog validation ported from
// upstream CLIProxyAPI commits 982cd124f9fe (envelope/array forms),
// 61741744889d (auto-namespacing of bare IDs), and db0b957c4831
// (case-insensitive duplicate rejection). Local symbols: ValidateDevinModelsJSON.
func TestValidateDevinModelsJSON(t *testing.T) {
	t.Run("valid envelope devin", func(t *testing.T) {
		data := []byte(`{
			"devin": [
				{
					"id": "devin/swe-2",
					"display_name": "SWE-2",
					"owned_by": "cognition",
					"context_length": 262000
				}
			]
		}`)
		models, err := ValidateDevinModelsJSON(data)
		if err != nil {
			t.Fatalf("expected valid, got error: %v", err)
		}
		if len(models) != 1 || models[0].ID != "devin/swe-2" {
			t.Fatalf("unexpected models: %+v", models)
		}
		if models[0].Type != "devin" {
			t.Errorf("expected type 'devin', got %q", models[0].Type)
		}
	})

	t.Run("valid envelope models", func(t *testing.T) {
		data := []byte(`{
			"models": [
				{
					"id": "devin/glm-5-2",
					"display_name": "GLM-5.2"
				}
			]
		}`)
		models, err := ValidateDevinModelsJSON(data)
		if err != nil {
			t.Fatalf("expected valid, got error: %v", err)
		}
		if len(models) != 1 || models[0].ID != "devin/glm-5-2" {
			t.Fatalf("unexpected models: %+v", models)
		}
	})

	t.Run("valid direct array", func(t *testing.T) {
		data := []byte(`[
			{
				"id": "devin/deepseek-v4-flash",
				"display_name": "DeepSeek V4 Flash"
			}
		]`)
		models, err := ValidateDevinModelsJSON(data)
		if err != nil {
			t.Fatalf("expected valid, got error: %v", err)
		}
		if len(models) != 1 || models[0].ID != "devin/deepseek-v4-flash" {
			t.Fatalf("unexpected models: %+v", models)
		}
	})

	// Upstream 61741744889d: bare IDs are namespaced under devin/.
	t.Run("clean id without devin prefix automatically namespaced", func(t *testing.T) {
		data := []byte(`{
			"devin": [
				{
					"id": "swe-2",
					"display_name": "SWE-2"
				}
			]
		}`)
		models, err := ValidateDevinModelsJSON(data)
		if err != nil {
			t.Fatalf("expected valid, got error: %v", err)
		}
		if len(models) != 1 || models[0].ID != "devin/swe-2" {
			t.Fatalf("expected auto-namespaced to devin/swe-2, got: %q", models[0].ID)
		}
	})

	t.Run("empty payload", func(t *testing.T) {
		_, err := ValidateDevinModelsJSON([]byte(`   `))
		if err == nil {
			t.Fatal("expected error on empty payload, got nil")
		}
	})

	t.Run("empty model id", func(t *testing.T) {
		data := []byte(`{"devin": [{"id": "", "display_name": "No ID"}]}`)
		_, err := ValidateDevinModelsJSON(data)
		if err == nil {
			t.Fatal("expected error on empty model id, got nil")
		}
	})

	// Upstream db0b957c4831: duplicate detection is case-insensitive.
	for name, data := range map[string][]byte{
		"exact duplicate": []byte(`{"devin": [{"id": "devin/swe-2"}, {"id": "devin/swe-2"}]}`),
		"case duplicate":  []byte(`{"devin": [{"id": "devin/SWE-2"}, {"id": "devin/swe-2"}]}`),
	} {
		t.Run("duplicate model id/"+name, func(t *testing.T) {
			_, err := ValidateDevinModelsJSON(data)
			if err == nil {
				t.Fatal("expected error on duplicate model id, got nil")
			}
		})
	}
}

// TestValidateDevinModelsJSONGeminiDefaults covers the auto-populated Gemini
// token limits and generation methods from upstream 0aedd05d31c8.
// Local symbol: sanitizeAndValidateDevinModels via ValidateDevinModelsJSON.
func TestValidateDevinModelsJSONGeminiDefaults(t *testing.T) {
	data := []byte(`{"devin": [{"id": "swe-2", "context_length": 262000, "max_completion_tokens": 128000}]}`)
	models, err := ValidateDevinModelsJSON(data)
	if err != nil {
		t.Fatalf("expected valid, got error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	m := models[0]
	if m.InputTokenLimit != 262000 {
		t.Errorf("InputTokenLimit = %d, want 262000 (from context_length)", m.InputTokenLimit)
	}
	if m.OutputTokenLimit != 128000 {
		t.Errorf("OutputTokenLimit = %d, want 128000 (from max_completion_tokens)", m.OutputTokenLimit)
	}
	if len(m.SupportedGenerationMethods) != 2 ||
		m.SupportedGenerationMethods[0] != "generateContent" ||
		m.SupportedGenerationMethods[1] != "countTokens" {
		t.Errorf("SupportedGenerationMethods = %v, want [generateContent countTokens]", m.SupportedGenerationMethods)
	}
	if m.Object != "model" {
		t.Errorf("Object = %q, want model", m.Object)
	}
	if len(m.SupportedInputModalities) != 1 || m.SupportedInputModalities[0] != "text" {
		t.Errorf("SupportedInputModalities = %v, want [text]", m.SupportedInputModalities)
	}
}

// TestEmbeddedDevinModelsLoadedOnStartup verifies the embedded
// devin_models.json catalog loads at init and that every ID is namespaced
// under devin/ (upstream eed249072d57, 982cd124f9fe).
func TestEmbeddedDevinModelsLoadedOnStartup(t *testing.T) {
	models := GetDevinModels()
	if len(models) < 30 {
		t.Fatalf("expected at least 30 embedded Devin models, got %d", len(models))
	}

	foundMap := make(map[string]bool)
	for _, m := range models {
		foundMap[m.ID] = true
	}

	expectedIDs := []string{
		"devin/swe-2",
		"devin/glm-5-2",
		"devin/glm-5-3",
		"devin/deepseek-v4-flash",
		"devin/deepseek-v4-1-flash",
		"devin/gemini-3-8-flash",
		"devin/grok-4-6",
		"devin/claude-fable-5-1",
		"devin/gpt-6-astra",
		"devin/swe-2-flash",
		"devin/gpt-6-1-terra",
		"devin/nemotron-3-5-super",
	}

	for _, id := range expectedIDs {
		if !foundMap[id] {
			t.Errorf("expected embedded catalog to contain %q", id)
		}
	}
}

// TestGetDevinModelsChannelAndLookup verifies channel dispatch and static
// lookup for namespaced Devin IDs (upstream eed249072d57).
// Local symbols: GetStaticModelDefinitionsByChannel, LookupStaticModelInfo.
func TestGetDevinModelsChannelAndLookup(t *testing.T) {
	byChannel := GetStaticModelDefinitionsByChannel("devin")
	if len(byChannel) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(\"devin\") returned empty list")
	}

	info := LookupStaticModelInfo("devin/swe-2")
	if info == nil {
		t.Fatal("LookupStaticModelInfo(\"devin/swe-2\") = nil, want valid model")
	}
	if info.DisplayName != "SWE-2" {
		t.Errorf("info.DisplayName = %q, want SWE-2", info.DisplayName)
	}
	if info.Type != "devin" {
		t.Errorf("info.Type = %q, want devin", info.Type)
	}
}

// TestLookupDevinModel verifies bare and namespaced lookups against the active
// Devin catalog (upstream 926450e87b5c).
// Local symbol: LookupDevinModel.
func TestLookupDevinModel(t *testing.T) {
	for _, id := range []string{"devin/swe-2", "swe-2", "Devin/SWE-2"} {
		info := LookupDevinModel(id)
		if info == nil {
			t.Fatalf("LookupDevinModel(%q) = nil, want valid model", id)
		}
		if info.ID != "devin/swe-2" {
			t.Errorf("LookupDevinModel(%q).ID = %q, want devin/swe-2", id, info.ID)
		}
	}
	if got := LookupDevinModel("devin/nonexistent-model"); got != nil {
		t.Errorf("LookupDevinModel(nonexistent) = %+v, want nil", got)
	}
	if got := LookupDevinModel(""); got != nil {
		t.Errorf("LookupDevinModel(empty) = %+v, want nil", got)
	}
}

// TestDevinModelsRemoteFetchFallback verifies the remote updater keeps the
// embedded catalog on failure and swaps in validated remote data on success
// (upstream 982cd124f9fe). Local symbols: tryRefreshDevinModels,
// loadDevinModelsFromBytes, devinModelsURLs.
func TestDevinModelsRemoteFetchFallback(t *testing.T) {
	// Test remote failure maintains existing embedded data
	origURLs := devinModelsURLs
	defer func() { devinModelsURLs = origURLs }()

	// Point to failing server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer ts.Close()

	devinModelsURLs = []string{ts.URL + "/devin_models.json"}

	initialCount := len(GetDevinModels())
	if initialCount == 0 {
		t.Fatal("expected non-empty initial Devin models")
	}

	// Attempt refresh from failing remote
	tryRefreshDevinModels(context.Background(), "test failing refresh")

	afterCount := len(GetDevinModels())
	if afterCount != initialCount {
		t.Fatalf("expected catalog to remain intact with %d models, got %d", initialCount, afterCount)
	}

	// Point to succeeding server with valid update
	validUpdate := []byte(`{
		"devin": [
			{
				"id": "devin/custom-test-model",
				"display_name": "Custom Test Model",
				"owned_by": "custom"
			}
		]
	}`)

	tsValid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validUpdate)
	}))
	defer tsValid.Close()

	devinModelsURLs = []string{tsValid.URL + "/devin_models.json"}
	tryRefreshDevinModels(context.Background(), "test succeeding refresh")

	updatedModels := GetDevinModels()
	foundCustom := false
	foundBuiltinSlow := false
	for _, m := range updatedModels {
		if m == nil {
			continue
		}
		if m.ID == "devin/custom-test-model" {
			foundCustom = true
		}
		if m.ID == "devin/swe-1-6-slow" {
			foundBuiltinSlow = true
		}
	}
	if !foundCustom {
		t.Fatalf("expected catalog to be updated to include custom-test-model, got: %+v", updatedModels)
	}
	if !foundBuiltinSlow {
		t.Fatalf("expected catalog to retain builtin swe-1-6-slow, got: %+v", updatedModels)
	}

	// Restore original embedded data for following tests
	_, _ = loadDevinModelsFromBytes(embeddedDevinModelsJSON, "restore-embed")
}
func TestFetchDevinModelsFromRemote_ContextNotCanceledBeforeRead_Issue6095(t *testing.T) {
	origURLs := devinModelsURLs
	defer func() { devinModelsURLs = origURLs }()

	headerFlushed := make(chan struct{})
	sendBody := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(headerFlushed)
		<-sendBody
		_, _ = w.Write([]byte(`{"devin": [{"id": "devin/swe-2"}]}`))
	}))
	defer ts.Close()

	go func() {
		<-headerFlushed
		// Yield briefly to let client.Do return and advance to body reading before streaming body chunks.
		time.Sleep(20 * time.Millisecond)
		close(sendBody)
	}()

	devinModelsURLs = []string{ts.URL + "/devin_models.json"}

	body, source := fetchDevinModelsFromRemote(context.Background())
	if body == nil || source == "" {
		t.Fatalf("expected successful fetch of devin models, got nil body (source=%q)", source)
	}
	expectedBody := `{"devin": [{"id": "devin/swe-2"}]}`
	if string(body) != expectedBody {
		t.Fatalf("expected body %q, got %q", expectedBody, string(body))
	}
	expectedSource := ts.URL + "/devin_models.json"
	if source != expectedSource {
		t.Fatalf("expected source %q, got %q", expectedSource, source)
	}
}

func TestDevinSWE16Slow_Issue6111(t *testing.T) {
	// Verify that devin/swe-1-6-slow is present in GetDevinModels
	models := GetDevinModels()
	var found *ModelInfo
	for _, m := range models {
		if m != nil && m.ID == "devin/swe-1-6-slow" {
			found = m
			break
		}
	}
	if found == nil {
		t.Fatalf("devin/swe-1-6-slow missing from GetDevinModels()")
	}

	// Verify LookupDevinModel with and without prefix
	lookupWithPrefix := LookupDevinModel("devin/swe-1-6-slow")
	if lookupWithPrefix == nil {
		t.Errorf("LookupDevinModel(\"devin/swe-1-6-slow\") returned nil")
	}
	lookupWithoutPrefix := LookupDevinModel("swe-1-6-slow")
	if lookupWithoutPrefix == nil {
		t.Errorf("LookupDevinModel(\"swe-1-6-slow\") returned nil")
	}

	// Verify model metadata
	if lookupWithPrefix != nil {
		if lookupWithPrefix.ID != "devin/swe-1-6-slow" {
			t.Errorf("expected ID 'devin/swe-1-6-slow', got %q", lookupWithPrefix.ID)
		}
		if lookupWithPrefix.DisplayName != "SWE-1.6 Slow" {
			t.Errorf("expected DisplayName 'SWE-1.6 Slow', got %q", lookupWithPrefix.DisplayName)
		}
		if lookupWithPrefix.Type != "devin" {
			t.Errorf("expected Type 'devin', got %q", lookupWithPrefix.Type)
		}
		if lookupWithPrefix.OwnedBy != "cognition" {
			t.Errorf("expected OwnedBy 'cognition', got %q", lookupWithPrefix.OwnedBy)
		}
	}

	// Verify persistence even if dynamic update loads payload without swe-1-6-slow
	payloadWithoutSlow := []byte(`{"devin": [{"id": "devin/swe-2", "display_name": "SWE-2"}]}`)
	if _, errUpdate := loadDevinModelsFromBytes(payloadWithoutSlow, "test-dynamic-update"); errUpdate != nil {
		t.Fatalf("loadDevinModelsFromBytes failed: %v", errUpdate)
	}
	defer func() {
		// Restore embedded models after test
		if _, errRestore := loadDevinModelsFromBytes(embeddedDevinModelsJSON, "embed"); errRestore != nil {
			t.Errorf("loadDevinModelsFromBytes restore failed: %v", errRestore)
		}
	}()

	modelsAfterUpdate := GetDevinModels()
	foundAfter := false
	for _, m := range modelsAfterUpdate {
		if m != nil && m.ID == "devin/swe-1-6-slow" {
			foundAfter = true
			break
		}
	}
	if !foundAfter {
		t.Errorf("devin/swe-1-6-slow missing from GetDevinModels() after dynamic update")
	}
	if LookupDevinModel("devin/swe-1-6-slow") == nil {
		t.Errorf("LookupDevinModel(\"devin/swe-1-6-slow\") returned nil after dynamic update")
	}
}

func TestWithDevinBuiltins(t *testing.T) {
	// Test WithDevinBuiltins with nil slice
	builtins := WithDevinBuiltins(nil)
	if len(builtins) != 1 {
		t.Fatalf("expected 1 builtin model, got %d", len(builtins))
	}
	if builtins[0].ID != "devin/swe-1-6-slow" {
		t.Errorf("expected ID 'devin/swe-1-6-slow', got %q", builtins[0].ID)
	}

	// Test WithDevinBuiltins preserves existing models and injects swe-1-6-slow
	existing := []*ModelInfo{
		{ID: "devin/swe-2", DisplayName: "SWE-2"},
	}
	merged := WithDevinBuiltins(existing)
	if len(merged) != 2 {
		t.Fatalf("expected 2 models, got %d", len(merged))
	}

	// Test WithDevinBuiltins replaces duplicate
	duplicate := []*ModelInfo{
		{ID: "devin/swe-1-6-slow", DisplayName: "Old Display Name"},
	}
	replaced := WithDevinBuiltins(duplicate)
	if len(replaced) != 1 {
		t.Fatalf("expected 1 model after deduplication, got %d", len(replaced))
	}
	if replaced[0].DisplayName != "SWE-1.6 Slow" {
		t.Errorf("expected DisplayName 'SWE-1.6 Slow', got %q", replaced[0].DisplayName)
	}
}

func TestEmbeddedDevinModels_NoThinkingSuffixes_Issue6141(t *testing.T) {
	models := GetDevinModels()
	disallowedSuffixes := []string{
		"-low-fast", "-medium-fast", "-high-fast", "-xhigh-fast", "-max-fast",
		"-none-fast", "-low", "-medium", "-high", "-xhigh", "-max", "-none",
		"-thinking-1m", "-thinking", "_none", "_minimal", "_low", "_medium",
		"_high", "_xhigh", "_max", "_thinking",
	}
	var exposedThinkingVariants []string
	for _, m := range models {
		cleanID := strings.TrimPrefix(m.ID, "devin/")
		for _, s := range disallowedSuffixes {
			if strings.HasSuffix(cleanID, s) {
				exposedThinkingVariants = append(exposedThinkingVariants, m.ID)
				break
			}
		}
	}
	if len(exposedThinkingVariants) > 0 {
		t.Fatalf("Devin catalog exposes thinking-parameter model variants (count=%d): %v", len(exposedThinkingVariants), exposedThinkingVariants)
	}
}

func TestValidateDevinModelsJSON_AggregatesThinkingVariants_Issue6141(t *testing.T) {
	payload := []byte(`{
		"devin": [
			{
				"id": "claude-opus-5",
				"display_name": "Claude Opus 5",
				"owned_by": "anthropic",
				"thinking": {"levels": ["medium"]}
			},
			{
				"id": "claude-opus-5-low-fast",
				"display_name": "Claude Opus 5 Low Fast",
				"owned_by": "anthropic"
			},
			{
				"id": "claude-opus-5-high-fast",
				"display_name": "Claude Opus 5 High Fast",
				"owned_by": "anthropic"
			}
		]
	}`)
	models, err := ValidateDevinModelsJSON(payload)
	if err != nil {
		t.Fatalf("ValidateDevinModelsJSON failed: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 aggregated model, got %d: %+v", len(models), models)
	}
	if models[0].ID != "devin/claude-opus-5" {
		t.Errorf("expected ID devin/claude-opus-5, got %q", models[0].ID)
	}
	if models[0].Thinking == nil {
		t.Fatalf("expected Thinking to be populated")
	}
	expectedLevels := []string{"low", "medium", "high"}
	if !reflect.DeepEqual(models[0].Thinking.Levels, expectedLevels) {
		t.Fatalf("expected thinking levels %v, got %v", expectedLevels, models[0].Thinking.Levels)
	}
}
