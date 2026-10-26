package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/registry"
)

// Ported from CLIProxyAPI 50cfd238 (model definitions fall back to the global
// registry for channels without static definitions; the plugin-host branch is
// not ported). Local surface: Handler.GetStaticModelDefinitions.

type modelDefinitionsBody struct {
	Channel string               `json:"channel"`
	Error   string               `json:"error"`
	Models  []registry.ModelInfo `json:"models"`
}

// invokeGetStaticModelDefinitions calls the handler the way the management
// router does: channel from the path param.
func invokeGetStaticModelDefinitions(t *testing.T, channel string) (*httptest.ResponseRecorder, modelDefinitionsBody) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// The endpoint derives everything from the channel and the global registry,
	// so the zero handler is the real input surface.
	handler := &Handler{}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "channel", Value: channel}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/model-definitions/"+channel, nil)
	handler.GetStaticModelDefinitions(ctx)

	var body modelDefinitionsBody
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &body); errUnmarshal != nil {
		t.Fatalf("body %q is not a model-definitions response: %v", recorder.Body.String(), errUnmarshal)
	}
	return recorder, body
}

func modelIDs(models []registry.ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestGetStaticModelDefinitionsKnownChannelReturnsStaticList keeps the static
// branch authoritative for a channel that has static definitions.
func TestGetStaticModelDefinitionsKnownChannelReturnsStaticList(t *testing.T) {
	recorder, body := invokeGetStaticModelDefinitions(t, "claude")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if body.Channel != "claude" {
		t.Errorf("channel = %q, want %q", body.Channel, "claude")
	}

	staticModels := registry.GetStaticModelDefinitionsByChannel("claude")
	wantIDs := make([]string, 0, len(staticModels))
	for _, model := range staticModels {
		if model != nil {
			wantIDs = append(wantIDs, model.ID)
		}
	}
	if len(wantIDs) == 0 {
		t.Fatal("static definitions for claude are empty; the fixture is wrong")
	}
	if gotIDs := modelIDs(body.Models); !slices.Equal(gotIDs, wantIDs) {
		t.Errorf("model ids = %v, want the static definition order %v", gotIDs, wantIDs)
	}
}

// TestGetStaticModelDefinitionsFallsBackToRegistrySorted pins the R21 behavior
// change: a channel without static definitions answers with the registry's
// available models for that provider, deduplicated and sorted by model id.
func TestGetStaticModelDefinitionsFallsBackToRegistrySorted(t *testing.T) {
	const provider = "workbuddy"
	registries := registry.GetGlobalRegistry()
	t.Cleanup(func() {
		registries.UnregisterClient("p11-workbuddy-a")
		registries.UnregisterClient("p11-workbuddy-b")
	})
	// Registration order is deliberately not sorted, and one id is shared so the
	// response proves dedupe as well as order.
	registries.RegisterClient("p11-workbuddy-a", provider, []*registry.ModelInfo{
		{ID: "workbuddy-zeta"},
		{ID: "workbuddy-alpha"},
		{ID: "workbuddy-shared"},
	})
	registries.RegisterClient("p11-workbuddy-b", provider, []*registry.ModelInfo{
		{ID: "workbuddy-mike"},
		{ID: "workbuddy-shared"},
	})

	recorder, body := invokeGetStaticModelDefinitions(t, provider)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if body.Channel != provider {
		t.Errorf("channel = %q, want %q", body.Channel, provider)
	}
	wantIDs := []string{"workbuddy-alpha", "workbuddy-mike", "workbuddy-shared", "workbuddy-zeta"}
	if gotIDs := modelIDs(body.Models); !slices.Equal(gotIDs, wantIDs) {
		t.Errorf("model ids = %v, want %v", gotIDs, wantIDs)
	}
}

// TestGetStaticModelDefinitionsUnknownChannelWithoutModelsIsBadRequest keeps
// the 400 for a channel that neither the static tables nor the registry know.
func TestGetStaticModelDefinitionsUnknownChannelWithoutModelsIsBadRequest(t *testing.T) {
	const channel = "no-such-channel-p11"
	registries := registry.GetGlobalRegistry()
	t.Cleanup(func() { registries.UnregisterClient("p11-no-such-channel") })

	recorder, body := invokeGetStaticModelDefinitions(t, channel)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if body.Error != "unknown channel" {
		t.Errorf("error = %q, want %q", body.Error, "unknown channel")
	}
	if body.Channel != channel {
		t.Errorf("channel = %q, want %q", body.Channel, channel)
	}
	if len(body.Models) != 0 {
		t.Errorf("models = %v, want none", body.Models)
	}
}
