package cliproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers/gemini"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/therealtinhtute/llmhub/sdk/config"
)

func TestApplyOAuthModelAlias_Rename(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "g5" {
		t.Fatalf("expected model id %q, got %q", "g5", out[0].ID)
	}
	if out[0].Name != "models/g5" {
		t.Fatalf("expected model name %q, got %q", "models/g5", out[0].Name)
	}
}

func TestApplyOAuthModelAlias_ForkAddsAlias(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 2 {
		t.Fatalf("expected 2 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
}

func TestApplyOAuthModelAlias_ForkAddsMultipleAliases(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true},
				{Name: "gpt-5", Alias: "g5-2", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 3 {
		t.Fatalf("expected 3 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
	if out[2].ID != "g5-2" {
		t.Fatalf("expected third model id %q, got %q", "g5-2", out[2].ID)
	}
	if out[2].Name != "models/g5-2" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5-2", out[2].Name)
	}
}

// Ported from upstream CLIProxyAPI commit 4311ae874774
// (sdk/cliproxy/service_oauth_model_alias_test.go): prefixed catalog clones must
// inherit a deep copy of NativeCapabilities so mutating the clone cannot corrupt
// the source model's capability metadata. MetadataModelID assertions were
// deferred until commit 8f23ad029144 added the field; they are covered by
// TestApplyModelPrefixes_PreservesMetadataModelID below.
func TestApplyModelPrefixes_ClonesNativeCapabilities(t *testing.T) {
	webSearch := true
	models := []*ModelInfo{
		{ID: "gpt-6-astra"},
		{ID: "codex-main", NativeCapabilities: &registry.NativeCapabilities{WebSearch: &webSearch}},
	}

	out := applyModelPrefixes(models, "1", false)
	entryMap := make(map[string]*ModelInfo, len(out))
	for _, entry := range out {
		if entry == nil {
			continue
		}
		entryMap[entry.ID] = entry
	}

	m, ok := entryMap["1/codex-main"]
	if !ok {
		t.Fatal("missing 1/codex-main")
	} else if m.NativeCapabilities == nil || m.NativeCapabilities.WebSearch == nil || !*m.NativeCapabilities.WebSearch {
		t.Fatalf("1/codex-main did not inherit native capabilities: %+v", m)
	} else {
		*m.NativeCapabilities.WebSearch = false
		if !*entryMap["codex-main"].NativeCapabilities.WebSearch {
			t.Fatal("prefixed capability metadata aliases the source model")
		}
	}
}

// Ported from upstream CLIProxyAPI commit 8335eac7
// (sdk/cliproxy/service_oauth_model_alias_test.go TestApplyOAuthModelAlias_Meta).
func TestApplyOAuthModelAlias_Meta(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"meta": {
				{Name: "muse-spark-1.3", Alias: "muse-latest", DisplayName: "Muse Latest"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "muse-spark-1.3", Name: "models/muse-spark-1.3", DisplayName: "Muse Spark 1.3"},
	}

	out := applyOAuthModelAlias(cfg, "meta", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "muse-latest" {
		t.Fatalf("expected model id %q, got %q", "muse-latest", out[0].ID)
	}
	if out[0].Name != "models/muse-latest" {
		t.Fatalf("expected model name %q, got %q", "models/muse-latest", out[0].Name)
	}
	if out[0].DisplayName != "Muse Latest" {
		t.Fatalf("expected display name %q, got %q", "Muse Latest", out[0].DisplayName)
	}
}

// Ported from upstream CLIProxyAPI commit 8f23ad029144
// (sdk/cliproxy/service_oauth_model_alias_test.go): OAuth aliases must carry the
// canonical model ID so Codex client template metadata resolves correctly. The
// upstream TestApplyOAuthModelAlias_PerAuthAlias hunk is not ported because the
// fork has no applyOAuthModelAliasForAuth (per-auth alias) helper.
func TestApplyOAuthModelAlias_PreservesMetadataModelID(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-6-astra", Alias: "codex-main", Fork: true},
				{Name: "gpt-5.6-luna", Alias: "codex-luna", Fork: false},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-6-astra", Name: "models/gpt-6-astra"},
		{ID: "gpt-5.6-luna", Name: "models/gpt-5.6-luna"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 3 {
		t.Fatalf("expected 3 models (original astra + forked astra alias + renamed luna alias), got %d", len(out))
	}

	entryMap := make(map[string]*ModelInfo, len(out))
	for _, m := range out {
		entryMap[m.ID] = m
	}

	if astra := entryMap["gpt-6-astra"]; astra == nil {
		t.Fatal("missing original gpt-6-astra")
	}
	if codexMain := entryMap["codex-main"]; codexMain == nil {
		t.Fatal("missing alias codex-main")
	} else if codexMain.MetadataModelID != "gpt-6-astra" {
		t.Fatalf("codex-main MetadataModelID = %q, want gpt-6-astra", codexMain.MetadataModelID)
	}

	if codexLuna := entryMap["codex-luna"]; codexLuna == nil {
		t.Fatal("missing alias codex-luna")
	} else if codexLuna.MetadataModelID != "gpt-5.6-luna" {
		t.Fatalf("codex-luna MetadataModelID = %q, want gpt-5.6-luna", codexLuna.MetadataModelID)
	}
}

// Ported from upstream CLIProxyAPI commit 8f23ad029144
// (sdk/cliproxy/service_oauth_model_alias_test.go): prefixed catalog clones must
// propagate the canonical metadata model ID.
func TestApplyModelPrefixes_PreservesMetadataModelID(t *testing.T) {
	models := []*ModelInfo{
		{ID: "gpt-6-astra"},
		{ID: "codex-main", MetadataModelID: "gpt-6-astra"},
	}

	out := applyModelPrefixes(models, "1", false)
	if len(out) != 4 {
		t.Fatalf("expected 4 models (2 unprefixed + 2 prefixed), got %d", len(out))
	}

	entryMap := make(map[string]*ModelInfo, len(out))
	for _, m := range out {
		entryMap[m.ID] = m
	}

	if m := entryMap["1/gpt-6-astra"]; m == nil {
		t.Fatal("missing 1/gpt-6-astra")
	} else if m.MetadataModelID != "gpt-6-astra" {
		t.Fatalf("1/gpt-6-astra MetadataModelID = %q, want gpt-6-astra", m.MetadataModelID)
	}

	if m := entryMap["1/codex-main"]; m == nil {
		t.Fatal("missing 1/codex-main")
	} else if m.MetadataModelID != "gpt-6-astra" {
		t.Fatalf("1/codex-main MetadataModelID = %q, want gpt-6-astra", m.MetadataModelID)
	}
}

// Ported from upstream CLIProxyAPI commit 908e95d94b61
// (sdk/cliproxy/service_oauth_model_alias_test.go): a prefixed clone must carry
// the channel prefix in its model name, not only in its ID, so the Gemini model
// list and the model-detail lookup resolve the prefixed entry instead of
// shadowing the unprefixed one.
func TestApplyModelPrefixes_RewritesModelNameForGeminiEndpoints(t *testing.T) {
	models := []*ModelInfo{
		{ID: "gemini-3-pro", Name: "models/gemini-3-pro", DisplayName: "Gemini 3 Pro"},
		{ID: "gemini-3-flash", Name: "gemini-3-flash", DisplayName: "Gemini 3 Flash"},
	}

	out := applyModelPrefixes(models, "gcp", false)
	if len(out) != 4 {
		t.Fatalf("expected 4 models (2 unprefixed + 2 prefixed), got %d", len(out))
	}

	entryMap := make(map[string]*ModelInfo, len(out))
	for _, m := range out {
		entryMap[m.ID] = m
	}

	if base := entryMap["gemini-3-pro"]; base == nil {
		t.Fatal("missing unprefixed gemini-3-pro")
	} else if base.Name != "models/gemini-3-pro" {
		t.Fatalf("unprefixed gemini-3-pro Name = %q, want models/gemini-3-pro", base.Name)
	}
	if base := entryMap["gemini-3-flash"]; base == nil {
		t.Fatal("missing unprefixed gemini-3-flash")
	} else if base.Name != "gemini-3-flash" {
		t.Fatalf("unprefixed gemini-3-flash Name = %q, want gemini-3-flash", base.Name)
	}

	if prefixed := entryMap["gcp/gemini-3-pro"]; prefixed == nil {
		t.Fatal("missing gcp/gemini-3-pro")
	} else {
		if prefixed.Name != "models/gcp/gemini-3-pro" {
			t.Fatalf("gcp/gemini-3-pro Name = %q, want models/gcp/gemini-3-pro", prefixed.Name)
		}
		if prefixed.DisplayName != "Gemini 3 Pro" {
			t.Fatalf("gcp/gemini-3-pro DisplayName = %q, want Gemini 3 Pro", prefixed.DisplayName)
		}
		if prefixed.MetadataModelID != "gemini-3-pro" {
			t.Fatalf("gcp/gemini-3-pro MetadataModelID = %q, want gemini-3-pro", prefixed.MetadataModelID)
		}
	}
	if prefixedBare := entryMap["gcp/gemini-3-flash"]; prefixedBare == nil {
		t.Fatal("missing gcp/gemini-3-flash")
	} else if prefixedBare.Name != "gcp/gemini-3-flash" {
		t.Fatalf("gcp/gemini-3-flash Name = %q, want gcp/gemini-3-flash", prefixedBare.Name)
	}

	forced := applyModelPrefixes(models, "gcp", true)
	if len(forced) != 2 {
		t.Fatalf("expected 2 forced-prefix models, got %d", len(forced))
	}
	for _, m := range forced {
		if m.ID == "gcp/gemini-3-pro" && m.Name != "models/gcp/gemini-3-pro" {
			t.Fatalf("forced gcp/gemini-3-pro Name = %q, want models/gcp/gemini-3-pro", m.Name)
		}
	}

	gin.SetMode(gin.TestMode)
	reg := GlobalModelRegistry()
	authID := t.Name() + "-vertex-gcp"
	reg.UnregisterClient(authID)
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	svc := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "vertex",
		Prefix:   "gcp",
		Status:   coreauth.StatusActive,
	}
	if !svc.registerModelsForAuth(auth) {
		t.Fatal("vertex registration with prefix gcp failed")
	}
	if !reg.ClientSupportsModel(authID, "gcp/gemini-3-pro") {
		t.Fatal("registry does not serve gcp/gemini-3-pro for the prefixed vertex auth")
	}

	handler := gemini.NewGeminiAPIHandler(&handlers.BaseAPIHandler{})
	router := gin.New()
	router.GET("/v1beta/models", handler.GeminiModels)
	router.GET("/v1beta/models/*action", handler.GeminiGetHandler)

	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/v1beta/models", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /v1beta/models status = %d, want 200", listRec.Code)
	}
	var listResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(listRec.Body.Bytes(), &listResp); errUnmarshal != nil {
		t.Fatalf("decode model list response: %v", errUnmarshal)
	}
	listed := make(map[string]int, len(listResp.Models))
	for _, m := range listResp.Models {
		listed[m.Name]++
	}
	if listed["models/gcp/gemini-3-pro"] != 1 {
		t.Fatalf("GET /v1beta/models lists models/gcp/gemini-3-pro %d times, want exactly 1 (names: %v)", listed["models/gcp/gemini-3-pro"], listed)
	}

	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v1beta/models/gcp/gemini-3-pro", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /v1beta/models/gcp/gemini-3-pro status = %d, want 200 (body: %s)", getRec.Code, getRec.Body.String())
	}
	var getResp struct {
		Name string `json:"name"`
	}
	if errUnmarshal := json.Unmarshal(getRec.Body.Bytes(), &getResp); errUnmarshal != nil {
		t.Fatalf("decode model detail response: %v", errUnmarshal)
	}
	if getResp.Name != "models/gcp/gemini-3-pro" {
		t.Fatalf("model detail name = %q, want models/gcp/gemini-3-pro", getResp.Name)
	}

	unprefixedRec := httptest.NewRecorder()
	router.ServeHTTP(unprefixedRec, httptest.NewRequest(http.MethodGet, "/v1beta/models/gemini-3-pro", nil))
	if unprefixedRec.Code != http.StatusOK {
		t.Fatalf("GET /v1beta/models/gemini-3-pro status = %d, want 200 when force-model-prefix is false", unprefixedRec.Code)
	}
}
