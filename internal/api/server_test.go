package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	codexmodels "github.com/therealtinhtute/llmhub/internal/client/codex/models"
	proxyconfig "github.com/therealtinhtute/llmhub/internal/config"
	internallogging "github.com/therealtinhtute/llmhub/internal/logging"
	"github.com/therealtinhtute/llmhub/internal/quotaalert"
	"github.com/therealtinhtute/llmhub/internal/redisqueue"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtimecontrol"
	sdkaccess "github.com/therealtinhtute/llmhub/sdk/access"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	coreexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdkconfig "github.com/therealtinhtute/llmhub/sdk/config"
	"github.com/tidwall/gjson"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithAuthManager(t, auth.NewManager(nil, nil, nil))
}

func newTestServerWithOptions(t *testing.T, options ...ServerOption) *Server {
	t.Helper()
	return newTestServerWithAuthManagerAndOptions(t, auth.NewManager(nil, nil, nil), options...)
}

func newTestServerWithAuthManager(t *testing.T, authManager *auth.Manager) *Server {
	t.Helper()
	return newTestServerWithAuthManagerAndOptions(t, authManager)
}

type runtimeControlContextStore struct {
	settings runtimecontrol.Settings
}

func (s *runtimeControlContextStore) LoadRuntimeSettings(context.Context) (runtimecontrol.Settings, error) {
	return s.settings, nil
}

func (s *runtimeControlContextStore) SaveRuntimeSettings(context.Context, int64, runtimecontrol.Settings) (runtimecontrol.Settings, error) {
	return runtimecontrol.Settings{}, nil
}

func newTestServerWithAuthManagerAndOptions(t *testing.T, authManager *auth.Manager, options ...ServerOption) *Server {
	t.Helper()
	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
	}
	return newTestServerWithConfig(t, authManager, cfg, options...)
}

// newTestServerWithConfig mirrors upstream CLIProxyAPI commit a962b77d4383's
// harness: the caller supplies the base config and this helper fills in the
// operational test fields.
func newTestServerWithConfig(t *testing.T, authManager *auth.Manager, cfg *proxyconfig.Config, options ...ServerOption) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg.Port = 0
	cfg.AuthDir = authDir
	cfg.Debug = true
	cfg.LoggingToFile = false
	cfg.UsageStatisticsEnabled = false

	accessManager := sdkaccess.NewManager()

	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath, options...)
}

// Ported from upstream CLIProxyAPI commit a962b77d4383.
func TestNewServerAppliesTrustedProxyConfiguration(t *testing.T) {
	server := newTestServerWithConfig(t, auth.NewManager(nil, nil, nil), &proxyconfig.Config{
		SDKConfig:      sdkconfig.SDKConfig{APIKeys: []string{"test-key"}},
		TrustedProxies: []string{"192.0.2.0/24"},
	})
	server.engine.GET("/test-client-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})

	tests := []struct {
		name       string
		remoteAddr string
		wantIP     string
	}{
		{name: "trusted proxy", remoteAddr: "192.0.2.10:43123", wantIP: "203.0.113.5"},
		{name: "untrusted peer", remoteAddr: "198.51.100.20:43123", wantIP: "198.51.100.20"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test-client-ip", nil)
			req.RemoteAddr = test.remoteAddr
			req.Header.Set("X-Forwarded-For", "203.0.113.5")
			recorder := httptest.NewRecorder()
			server.engine.ServeHTTP(recorder, req)
			if got := recorder.Body.String(); got != test.wantIP {
				t.Fatalf("client IP = %q, want %q", got, test.wantIP)
			}
		})
	}

	defaultServer := newTestServer(t)
	defaultServer.engine.GET("/test-client-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})
	request := httptest.NewRequest(http.MethodGet, "/test-client-ip", nil)
	request.RemoteAddr = "198.51.100.20:43123"
	request.Header.Set("X-Forwarded-For", "203.0.113.5")
	recorder := httptest.NewRecorder()
	defaultServer.engine.ServeHTTP(recorder, request)
	if got := recorder.Body.String(); got != "198.51.100.20" {
		t.Fatalf("default client IP = %q, want direct peer IP", got)
	}
}

func TestHealthz(t *testing.T) {
	server := newTestServer(t)

	t.Run("GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}

		var resp struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
		}
		if resp.Status != "ok" {
			t.Fatalf("unexpected response status: got %q want %q", resp.Status, "ok")
		}
	})

	t.Run("HEAD", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("expected empty body for HEAD request, got %q", rr.Body.String())
		}
	})
}

func TestRuntimeControlRequestContextDisablesCodexCloaking(t *testing.T) {
	store := &runtimeControlContextStore{settings: runtimecontrol.Settings{Cloaking: runtimecontrol.CloakingSettings{DisableCodex: true}}}
	server := newTestServerWithOptions(t, WithRuntimeSettingsStore(store))
	router := gin.New()
	router.Use(server.runtimeControlRequestContext())
	router.GET("/probe", func(c *gin.Context) {
		if !coreexecutor.CodexCloakingDisabled(c.Request.Context()) {
			t.Fatal("request context did not disable Codex cloaking")
		}
		ctx, cancel := server.handlers.GetContextWithCancel(nil, c, context.Background())
		defer cancel()
		if !coreexecutor.CodexCloakingDisabled(ctx) {
			t.Fatal("handler execution context did not preserve Codex cloaking disable")
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
}

func TestManagementUsageRequiresManagementAuthAndPopsArray(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	prevQueueEnabled := redisqueue.Enabled()
	redisqueue.SetEnabled(false)
	t.Cleanup(func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevQueueEnabled)
	})

	server := newTestServer(t)

	redisqueue.Enqueue([]byte(`{"id":1}`))
	redisqueue.Enqueue([]byte(`{"id":2}`))

	missingKeyReq := httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)
	missingKeyRR := httptest.NewRecorder()
	server.engine.ServeHTTP(missingKeyRR, missingKeyReq)
	if missingKeyRR.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want %d body=%s", missingKeyRR.Code, http.StatusUnauthorized, missingKeyRR.Body.String())
	}

	legacyReq := httptest.NewRequest(http.MethodGet, "/v0/management/usage?count=2", nil)
	legacyReq.Header.Set("Authorization", "Bearer test-management-key")
	legacyRR := httptest.NewRecorder()
	server.engine.ServeHTTP(legacyRR, legacyReq)
	if legacyRR.Code != http.StatusNotFound {
		t.Fatalf("legacy usage status = %d, want %d body=%s", legacyRR.Code, http.StatusNotFound, legacyRR.Body.String())
	}

	authReq := httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)
	authReq.Header.Set("Authorization", "Bearer test-management-key")
	authRR := httptest.NewRecorder()
	server.engine.ServeHTTP(authRR, authReq)
	if authRR.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d body=%s", authRR.Code, http.StatusOK, authRR.Body.String())
	}

	var payload []json.RawMessage
	if errUnmarshal := json.Unmarshal(authRR.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v body=%s", errUnmarshal, authRR.Body.String())
	}
	if len(payload) != 2 {
		t.Fatalf("response records = %d, want 2", len(payload))
	}
	for i, raw := range payload {
		var record struct {
			ID int `json:"id"`
		}
		if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
			t.Fatalf("unmarshal record %d: %v", i, errUnmarshal)
		}
		if record.ID != i+1 {
			t.Fatalf("record %d id = %d, want %d", i, record.ID, i+1)
		}
	}

	if remaining := redisqueue.PopOldest(1); len(remaining) != 0 {
		t.Fatalf("remaining queue = %q, want empty", remaining)
	}
}

func TestManagementQuotaAlertSettingsRouteRequiresManagementAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	store := &apiQuotaAlertRouteStore{settings: quotaalert.DefaultSettings()}
	server := newTestServerWithOptions(t, WithQuotaAlertStore(store))

	missingKeyReq := httptest.NewRequest(http.MethodGet, "/v0/management/quota-alerts/settings", nil)
	missingKeyRR := httptest.NewRecorder()
	server.engine.ServeHTTP(missingKeyRR, missingKeyReq)
	if missingKeyRR.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want %d body=%s", missingKeyRR.Code, http.StatusUnauthorized, missingKeyRR.Body.String())
	}
	if store.loadSettingsCount != 0 {
		t.Fatalf("store loads before auth = %d, want 0", store.loadSettingsCount)
	}

	authReq := httptest.NewRequest(http.MethodGet, "/v0/management/quota-alerts/settings", nil)
	authReq.Header.Set("Authorization", "Bearer test-management-key")
	authRR := httptest.NewRecorder()
	server.engine.ServeHTTP(authRR, authReq)
	if authRR.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d body=%s", authRR.Code, http.StatusOK, authRR.Body.String())
	}
	if store.loadSettingsCount != 1 {
		t.Fatalf("store loads after auth = %d, want 1", store.loadSettingsCount)
	}
}

func TestHomeEnabledHidesManagementEndpointsAndControlPanel(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	server.cfg.Home.Enabled = true

	t.Run("management endpoints return 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.Header.Set("Authorization", "Bearer test-management-key")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
	})

	t.Run("management control panel returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
	})
}

func TestModelsWithClientVersionReturnsCodexCatalog(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-client-version-catalog"
	modelRegistry.RegisterClient(clientID, "openai", []*registry.ModelInfo{
		{
			ID:            "gpt-5.5",
			Object:        "model",
			Created:       1776902400,
			OwnedBy:       "openai",
			Type:          "openai",
			DisplayName:   "GPT 5.5",
			Description:   "Frontier model for complex coding, research, and real-world work.",
			ContextLength: 272000,
			Thinking:      &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
		},
		{
			ID:            "custom-codex-model-test",
			Object:        "model",
			OwnedBy:       "test",
			Type:          "openai",
			DisplayName:   "Custom Codex Model",
			Description:   "Custom model from registry",
			ContextLength: 123456,
			Thinking:      &registry.ThinkingSupport{Levels: []string{"none", "minimal", "low", "medium", "unsupported", "high", "xhigh"}},
		},
		{ID: "grok-imagine-image-quality", Object: "model", OwnedBy: "xai", Type: "openai"},
		{ID: "gpt-image-2", Object: "model", OwnedBy: "openai", Type: "openai"},
		{ID: "grok-imagine-image", Object: "model", OwnedBy: "xai", Type: "openai"},
		{ID: "grok-imagine-video", Object: "model", OwnedBy: "xai", Type: "openai"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("User-Agent", "claude-cli/1.0")

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp struct {
		Models []map[string]any `json:"models"`
		Object string           `json:"object"`
		Data   []any            `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
	}
	if resp.Object != "" || resp.Data != nil {
		t.Fatalf("expected codex catalog format without object/data, got object=%q data=%v", resp.Object, resp.Data)
	}
	if len(resp.Models) == 0 {
		t.Fatal("expected codex catalog models")
	}

	var gpt55 map[string]any
	var custom map[string]any
	for _, model := range resp.Models {
		switch slug, _ := model["slug"].(string); slug {
		case "gpt-5.5":
			gpt55 = model
		case "custom-codex-model-test":
			custom = model
		}
	}
	if gpt55 == nil {
		t.Fatal("expected gpt-5.5 codex catalog entry")
	}
	if _, ok := gpt55["minimal_client_version"]; !ok {
		t.Fatal("expected minimal_client_version in codex catalog")
	}
	serviceTiers, ok := gpt55["service_tiers"].([]any)
	if !ok || len(serviceTiers) != 1 {
		t.Fatalf("expected gpt-5.5 priority service tier, got %#v", gpt55["service_tiers"])
	}
	if custom == nil {
		t.Fatal("expected custom model codex catalog entry")
	}
	if got, _ := custom["display_name"].(string); got != "Custom Codex Model" {
		t.Fatalf("custom display_name = %q, want Custom Codex Model", got)
	}
	if got, _ := custom["description"].(string); got != "Custom model from registry" {
		t.Fatalf("custom description = %q, want Custom model from registry", got)
	}
	if got, _ := custom["context_window"].(float64); got != 123456 {
		t.Fatalf("custom context_window = %v, want 123456", custom["context_window"])
	}
	assertCodexSupportedReasoningLevels(t, custom, []string{"none", "minimal", "low", "medium", "high", "xhigh"})
	// Upstream 673131f57484: non-template models carry the compact literal
	// fallback instead of cloning the template's full base_instructions.
	if got, _ := custom["base_instructions"].(string); got != "You are Codex, a coding agent. You and the user share one workspace." {
		t.Fatalf("custom base_instructions = %q, want compact fallback", got)
	}
	if _, ok := custom["available_in_plans"].([]any); !ok {
		t.Fatalf("expected custom model to use gpt-5.5 available_in_plans fallback, got %#v", custom["available_in_plans"])
	}
	if got, _ := custom["prefer_websockets"].(bool); got {
		t.Fatalf("custom prefer_websockets = %v, want false", custom["prefer_websockets"])
	}
	// Upstream 673131f57484: these keys must be present and null for Codex
	// catalog decoding; omitting them rejects the entire catalog.
	for _, key := range []string{"apply_patch_tool_type", "upgrade", "availability_nux"} {
		value, exists := custom[key]
		if !exists {
			t.Fatalf("expected custom model to carry null %s", key)
		}
		if value != nil {
			t.Fatalf("custom %s = %#v, want null", key, value)
		}
	}

	hiddenModels := map[string]bool{
		"grok-imagine-image-quality": false,
		"gpt-image-2":                false,
		"grok-imagine-image":         false,
		"grok-imagine-video":         false,
	}
	for _, model := range resp.Models {
		slug, _ := model["slug"].(string)
		if _, ok := hiddenModels[slug]; !ok {
			continue
		}
		if visibility, _ := model["visibility"].(string); visibility != "hide" {
			t.Fatalf("%s visibility = %q, want hide", slug, visibility)
		}
		hiddenModels[slug] = true
	}
	for slug, found := range hiddenModels {
		if !found {
			t.Fatalf("expected hidden model %s in codex catalog", slug)
		}
	}
}

func assertCodexSupportedReasoningLevels(t *testing.T, model map[string]any, want []string) {
	t.Helper()

	rawLevels, ok := model["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("expected supported_reasoning_levels, got %#v", model["supported_reasoning_levels"])
	}
	if len(rawLevels) != len(want) {
		t.Fatalf("supported_reasoning_levels length = %d, want %d: %#v", len(rawLevels), len(want), rawLevels)
	}
	for index, rawLevel := range rawLevels {
		levelEntry, ok := rawLevel.(map[string]any)
		if !ok {
			t.Fatalf("supported_reasoning_levels[%d] = %#v, want object", index, rawLevel)
		}
		if got, _ := levelEntry["effort"].(string); got != want[index] {
			t.Fatalf("supported_reasoning_levels[%d].effort = %q, want %q", index, got, want[index])
		}
	}
}

// TestHomeModelsRequirePerEntryWebSearchCapabilityAndConservativeRoutes is ported
// from upstream CLIProxyAPI 4311ae874774 (replacing the provider-heuristic
// approach introduced by 294b7f5b191b). Local symbols under test:
// decodeHomeModels, homeModelEntry.nativeCapabilityRoutes, formatHomeCodexModel,
// homeWebSearchCapabilityForModel, codexmodels.BuildResponseForClientWithCPACapabilities.
func TestHomeModelsRequirePerEntryWebSearchCapabilityAndConservativeRoutes(t *testing.T) {
	entries, errDecode := decodeHomeModels([]byte(`{
		"codex":[
			{"id":"home-codex","native_capabilities":{"web_search":true}},
			{"id":"home-unknown"},
			{"id":"home-duplicate","native_capabilities":{"web_search":true}},
			{"id":"home-duplicate","native_capabilities":{"web_search":false}}
		],
		"xai":[{"id":"home-xai","native_capabilities":{"web_search":true}}],
		"claude":[{"id":"gpt-5.5","native_capabilities":{"web_search":true}}],
		"gemini":[{"id":"home-gemini","native_capabilities":{"web_search":true}}],
		"custom":[{"id":"home-custom","native_capabilities":{"web_search":true}}]
	}`))
	if errDecode != nil {
		t.Fatalf("decode Home models: %v", errDecode)
	}

	models := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		models = append(models, formatHomeCodexModel(entry))
	}
	response := codexmodels.BuildResponseForClientWithCPACapabilities(models, nil, homeWebSearchCapabilityForModel(entries), false, "cpa")
	catalog, ok := response["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models = %#v, want []map[string]any", response["models"])
	}
	bySlug := make(map[string]map[string]any, len(catalog))
	for _, model := range catalog {
		slug, _ := model["slug"].(string)
		bySlug[slug] = model
	}
	for _, modelID := range []string{"home-codex", "home-xai", "gpt-5.5"} {
		assertSerializedCPAWebSearch(t, bySlug[modelID], true)
	}
	if supportsSearchTool, _ := bySlug["gpt-5.5"]["supports_search_tool"].(bool); !supportsSearchTool {
		t.Fatal("CPA capability metadata changed legacy Home supports_search_tool")
	}
	for _, modelID := range []string{"home-gemini", "home-duplicate"} {
		assertSerializedCPAWebSearch(t, bySlug[modelID], false)
	}
	for _, modelID := range []string{"home-unknown", "home-custom"} {
		if _, exists := bySlug[modelID]["cpa_capabilities"]; exists {
			t.Fatalf("%s cpa_capabilities = %#v, want omitted", modelID, bySlug[modelID]["cpa_capabilities"])
		}
	}
}

func assertSerializedCPAWebSearch(t *testing.T, entry map[string]any, want bool) {
	t.Helper()
	if entry == nil {
		t.Fatal("missing model entry")
	}
	capabilities, ok := entry["cpa_capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("cpa_capabilities = %#v, want object", entry["cpa_capabilities"])
	}
	if got, ok := capabilities["web_search"].(bool); !ok || got != want {
		t.Fatalf("web_search = %#v, want %v", capabilities["web_search"], want)
	}
}

func TestDefaultRequestLoggerFactory_UsesResolvedLogDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")

	originalWD, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("failed to get current working directory: %v", errGetwd)
	}

	tmpDir := t.TempDir()
	if errChdir := os.Chdir(tmpDir); errChdir != nil {
		t.Fatalf("failed to switch working directory: %v", errChdir)
	}
	defer func() {
		if errChdirBack := os.Chdir(originalWD); errChdirBack != nil {
			t.Fatalf("failed to restore working directory: %v", errChdirBack)
		}
	}()

	// Force ResolveLogDirectory to fallback to auth-dir/logs by making ./logs not a writable directory.
	if errWriteFile := os.WriteFile(filepath.Join(tmpDir, "logs"), []byte("not-a-directory"), 0o644); errWriteFile != nil {
		t.Fatalf("failed to create blocking logs file: %v", errWriteFile)
	}

	configDir := filepath.Join(tmpDir, "config")
	if errMkdirConfig := os.MkdirAll(configDir, 0o755); errMkdirConfig != nil {
		t.Fatalf("failed to create config dir: %v", errMkdirConfig)
	}
	configPath := filepath.Join(configDir, "config.yaml")

	authDir := filepath.Join(tmpDir, "auth")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: proxyconfig.SDKConfig{
			RequestLog: false,
		},
		AuthDir:           authDir,
		ErrorLogsMaxFiles: 10,
	}

	logger := defaultRequestLoggerFactory(cfg, configPath)
	fileLogger, ok := logger.(*internallogging.FileRequestLogger)
	if !ok {
		t.Fatalf("expected *FileRequestLogger, got %T", logger)
	}

	errLog := fileLogger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		true,
		"issue-1711",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("failed to write forced error request log: %v", errLog)
	}

	authLogsDir := filepath.Join(authDir, "logs")
	authEntries, errReadAuthDir := os.ReadDir(authLogsDir)
	if errReadAuthDir != nil {
		t.Fatalf("failed to read auth logs dir %s: %v", authLogsDir, errReadAuthDir)
	}
	foundErrorLogInAuthDir := false
	for _, entry := range authEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			foundErrorLogInAuthDir = true
			break
		}
	}
	if !foundErrorLogInAuthDir {
		t.Fatalf("expected forced error log in auth fallback dir %s, got entries: %+v", authLogsDir, authEntries)
	}

	configLogsDir := filepath.Join(configDir, "logs")
	configEntries, errReadConfigDir := os.ReadDir(configLogsDir)
	if errReadConfigDir != nil && !os.IsNotExist(errReadConfigDir) {
		t.Fatalf("failed to inspect config logs dir %s: %v", configLogsDir, errReadConfigDir)
	}
	for _, entry := range configEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			t.Fatalf("unexpected forced error log in config dir %s", configLogsDir)
		}
	}
}

// Ported from upstream CLIProxyAPI commit 28743473c11a ("feat(codex): append
// (Devin) suffix to Devin model display names"). Local symbols under test:
// codexmodels.BuildResponseForClient (via GET /v1/models?client_version=...),
// formatHomeCodexModel.
func TestModelsWithClientVersion_DevinDisplayName(t *testing.T) {
	devinClientID := "test-devin-client-version-models"
	openaiClientID := "test-openai-client-version-models"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(devinClientID, "devin", []*registry.ModelInfo{
		{
			ID:          "devin/swe-2",
			Object:      "model",
			OwnedBy:     "cognition",
			Type:        "devin",
			DisplayName: "SWE-2",
		},
		{
			ID:          "devin/gpt-6-astra",
			Object:      "model",
			OwnedBy:     "openai",
			Type:        "devin",
			DisplayName: "GPT-6 Astra",
		},
	})
	modelRegistry.RegisterClient(openaiClientID, "openai", []*registry.ModelInfo{
		{
			ID:          "standard-openai-model",
			Object:      "model",
			OwnedBy:     "openai",
			Type:        "openai",
			DisplayName: "Standard Model",
		},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(devinClientID)
		modelRegistry.UnregisterClient(openaiClientID)
	})

	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.153.4", nil)
	req.Header.Set("Authorization", "Bearer test-key")

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(rr.Body.Bytes(), &resp); errUnmarshal != nil {
		t.Fatalf("failed to parse response JSON: %v", errUnmarshal)
	}

	bySlug := make(map[string]map[string]any, len(resp.Models))
	for _, m := range resp.Models {
		if slug, ok := m["slug"].(string); ok {
			bySlug[slug] = m
		}
	}

	swe2, ok := bySlug["devin/swe-2"]
	if !ok {
		t.Fatal("expected devin/swe-2 in models list")
	}
	if got, _ := swe2["display_name"].(string); got != "SWE-2 (Devin)" {
		t.Fatalf("devin/swe-2 display_name = %q, want SWE-2 (Devin)", got)
	}

	astra, ok := bySlug["devin/gpt-6-astra"]
	if !ok {
		t.Fatal("expected devin/gpt-6-astra in models list")
	}
	if got, _ := astra["display_name"].(string); got != "GPT-6 Astra (Devin)" {
		t.Fatalf("devin/gpt-6-astra display_name = %q, want GPT-6 Astra (Devin)", got)
	}

	std, ok := bySlug["standard-openai-model"]
	if !ok {
		t.Fatal("expected standard-openai-model in models list")
	}
	if got, _ := std["display_name"].(string); got != "Standard Model" {
		t.Fatalf("standard-openai-model display_name = %q, want Standard Model", got)
	}
}

// Ported from upstream CLIProxyAPI commit 28743473c11a.
func TestHomeCodexClientModels_DevinDisplayName(t *testing.T) {
	entries := []homeModelEntry{
		{
			id:          "devin/swe-2",
			displayName: "SWE-2",
			providers:   []string{"devin"},
		},
		{
			id:          "home-regular",
			displayName: "Home Regular",
			providers:   []string{"openai"},
		},
	}
	models := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		models = append(models, formatHomeCodexModel(entry))
	}
	resp := codexmodels.BuildResponseForClient(models, nil, false, "0.153.4")
	catalog, ok := resp["models"].([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any, got %T", resp["models"])
	}
	bySlug := make(map[string]map[string]any, len(catalog))
	for _, m := range catalog {
		slug, _ := m["slug"].(string)
		bySlug[slug] = m
	}
	if got, _ := bySlug["devin/swe-2"]["display_name"].(string); got != "SWE-2 (Devin)" {
		t.Fatalf("devin/swe-2 display_name = %q, want SWE-2 (Devin)", got)
	}
	if got, _ := bySlug["home-regular"]["display_name"].(string); got != "Home Regular" {
		t.Fatalf("home-regular display_name = %q, want Home Regular", got)
	}
}

// revocableServerAccessProvider models a frontend key that a fresh handshake
// rejects once revoked, while one already-open socket must be re-checked per turn.
type revocableServerAccessProvider struct {
	revoked atomic.Bool
}

func (p *revocableServerAccessProvider) Identifier() string { return "revocable-server-provider" }

func (p *revocableServerAccessProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if r == nil || r.Header.Get("Authorization") != "Bearer key-1" {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	if p.revoked.Load() {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	return &sdkaccess.Result{Provider: p.Identifier(), Principal: "key-1"}, nil
}

type serverResponsesWebsocketCaptureExecutor struct {
	calls atomic.Int32
}

func (e *serverResponsesWebsocketCaptureExecutor) Identifier() string { return "codex" }

func (e *serverResponsesWebsocketCaptureExecutor) Execute(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *serverResponsesWebsocketCaptureExecutor) ExecuteStream(_ context.Context, _ *auth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	n := e.calls.Add(1)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{
		Payload: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d","output":[]}}`, n)),
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *serverResponsesWebsocketCaptureExecutor) Refresh(_ context.Context, entry *auth.Auth) (*auth.Auth, error) {
	return entry, nil
}

func (e *serverResponsesWebsocketCaptureExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *serverResponsesWebsocketCaptureExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// TestServerResponsesWebsocketReauthorizesEachTurn covers the production route
// wiring: the responses handler must receive the server's access manager, so a
// key revoked after the handshake is re-checked per turn instead of the socket
// running on the upgrade-time decision (upstream CLIProxyAPI commit b9bb031dcbb0).
func TestServerResponsesWebsocketReauthorizesEachTurn(t *testing.T) {
	server := newTestServer(t)
	provider := &revocableServerAccessProvider{}
	server.accessManager.SetProviders([]sdkaccess.Provider{provider})

	executor := &serverResponsesWebsocketCaptureExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	authEntry := &auth.Auth{
		ID:         "ws-reauth-auth",
		Provider:   executor.Identifier(),
		Status:     auth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, errReg := server.handlers.AuthManager.Register(context.Background(), authEntry); errReg != nil {
		t.Fatalf("Register auth: %v", errReg)
	}
	const modelName = "ws-reauth-model"
	registry.GetGlobalRegistry().RegisterClient(authEntry.ID, authEntry.Provider, []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authEntry.ID)
	})

	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/responses"
	headers := http.Header{"Authorization": []string{"Bearer key-1"}}
	conn, _, errDial := websocket.DefaultDialer.Dial(wsURL, headers)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	turnPayload := []byte(`{"type":"response.create","model":"` + modelName + `","input":[{"type":"message","role":"user","content":"turn"}]}`)
	if errWrite := conn.WriteMessage(websocket.TextMessage, turnPayload); errWrite != nil {
		t.Fatalf("write turn 1: %v", errWrite)
	}
	_, msg1, errRead1 := conn.ReadMessage()
	if errRead1 != nil {
		t.Fatalf("read turn 1: %v", errRead1)
	}
	if got := gjson.GetBytes(msg1, "type").String(); got != "response.completed" {
		t.Fatalf("turn 1 event type = %q, want response.completed (%s)", got, msg1)
	}

	provider.revoked.Store(true)

	freshConn, resp, errFreshDial := websocket.DefaultDialer.Dial(wsURL, headers)
	if errFreshDial == nil {
		_ = freshConn.Close()
		t.Fatal("expected fresh dial with a revoked key to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("fresh dial status = %d, want %d", status, http.StatusUnauthorized)
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, turnPayload); errWrite != nil {
		t.Fatalf("write turn 2: %v", errWrite)
	}
	_, msg2, errRead2 := conn.ReadMessage()
	if errRead2 != nil {
		t.Fatalf("read turn 2: %v", errRead2)
	}
	if got := gjson.GetBytes(msg2, "type").String(); got != "error" {
		t.Fatalf("turn 2 event type = %q, want error (%s)", got, msg2)
	}
	if got := int(gjson.GetBytes(msg2, "status").Int()); got != http.StatusUnauthorized {
		t.Fatalf("turn 2 error status = %d, want %d (%s)", got, http.StatusUnauthorized, msg2)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("executor calls after revoked turn = %d, want 1", got)
	}
}
