package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
)

// Ported from CLIProxyAPI 07feedf7 (allow empty home model catalog) and
// 4d34502f (reject home models error envelopes). Local surface: decodeHomeModels
// and the four home catalog handlers behind it.

// TestDecodeHomeModelsEmptyCatalog pins the empty-catalog contract: a home
// catalog whose sections hold no model records is a normal empty answer.
func TestDecodeHomeModelsEmptyCatalog(t *testing.T) {
	for _, raw := range []string{"{}", " { } ", `{"claude":[],"codex-pro":[]}`} {
		entries, errDecode := decodeHomeModels([]byte(raw))
		if errDecode != nil {
			t.Errorf("decodeHomeModels(%q) error = %v, want an empty catalog", raw, errDecode)
			continue
		}
		if entries == nil {
			t.Errorf("decodeHomeModels(%q) entries = nil, want a non-nil empty slice", raw)
		}
		if len(entries) != 0 {
			t.Errorf("decodeHomeModels(%q) entries = %v, want empty", raw, entries)
		}
	}
}

// TestDecodeHomeModelsRejectsMalformedAndErrorPayloads keeps null payloads,
// null sections, sections without usable ids and every error envelope
// (including array-shaped ones that used to look like provider sections)
// malformed.
func TestDecodeHomeModelsRejectsMalformedAndErrorPayloads(t *testing.T) {
	for _, raw := range []string{
		"", "null", "[]", "{bad}",
		`{"claude":[{}]}`, `{"claude":null}`, `{"error":null}`,
		`{"error":{"type":"internal_error","message":"unavailable"}}`,
		`{"error":[]}`, `{"error":[],"claude":[]}`,
		`{"error":[{"id":"not-a-model"}]}`,
		`{"error":[],"claude":[{"id":"model-a"}]}`,
	} {
		if _, errDecode := decodeHomeModels([]byte(raw)); errDecode == nil {
			t.Errorf("decodeHomeModels(%q) accepted a malformed or error payload", raw)
		}
	}
}

// newHomeCatalogEngine wires the production route handlers against a canned
// home payload through the test-only WithHomeModelsReader seam.
func newHomeCatalogEngine(t *testing.T, payload string) *gin.Engine {
	t.Helper()
	server := newTestServerWithOptions(t, WithHomeModelsReader(func(context.Context) ([]byte, error) {
		return []byte(payload), nil
	}))
	server.cfg.Home.Enabled = true

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/models", server.unifiedModelsHandler(nil, nil))
	engine.GET("/v1beta/models", server.geminiModelsHandler(nil))
	engine.GET("/v1beta/models/*action", server.geminiGetHandler(nil))
	return engine
}

func serveHomeCatalogRequest(t *testing.T, engine *gin.Engine, path string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	body := map[string]any{}
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &body); errUnmarshal != nil {
		t.Fatalf("%s body %q is not a JSON object: %v", path, recorder.Body.String(), errUnmarshal)
	}
	return recorder, body
}

// TestHomeModelCatalogHandlersServeEmptyCatalog drives the live /v1/models and
// /v1beta/models handlers: an empty home catalog answers 200 in the local
// success shape, with an empty array rather than null.
func TestHomeModelCatalogHandlersServeEmptyCatalog(t *testing.T) {
	engine := newHomeCatalogEngine(t, "{}")

	t.Run("openai model list", func(t *testing.T) {
		recorder, body := serveHomeCatalogRequest(t, engine, "/v1/models", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		if object, _ := body["object"].(string); object != "list" {
			t.Errorf("object = %#v, want %q", body["object"], "list")
		}
		data, okArray := body["data"].([]any)
		if !okArray {
			t.Fatalf("data = %#v, want an empty array", body["data"])
		}
		if len(data) != 0 {
			t.Errorf("data = %v, want empty", data)
		}
	})

	t.Run("claude model list", func(t *testing.T) {
		recorder, body := serveHomeCatalogRequest(t, engine, "/v1/models", map[string]string{"User-Agent": "claude-cli/1.0.32"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		data, okArray := body["data"].([]any)
		if !okArray {
			t.Fatalf("data = %#v, want an empty array", body["data"])
		}
		if len(data) != 0 {
			t.Errorf("data = %v, want empty", data)
		}
		if hasMore, _ := body["has_more"].(bool); hasMore {
			t.Errorf("has_more = %#v, want false", body["has_more"])
		}
		if firstID, _ := body["first_id"].(string); firstID != "" {
			t.Errorf("first_id = %q, want empty", firstID)
		}
		if lastID, _ := body["last_id"].(string); lastID != "" {
			t.Errorf("last_id = %q, want empty", lastID)
		}
	})

	t.Run("gemini model list", func(t *testing.T) {
		recorder, body := serveHomeCatalogRequest(t, engine, "/v1beta/models", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		models, okArray := body["models"].([]any)
		if !okArray {
			t.Fatalf("models = %#v, want an empty array", body["models"])
		}
		if len(models) != 0 {
			t.Errorf("models = %v, want empty", models)
		}
	})

	t.Run("gemini single model", func(t *testing.T) {
		recorder, body := serveHomeCatalogRequest(t, engine, "/v1beta/models/gemini-x", nil)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
		}
		errorBody, okObject := body["error"].(map[string]any)
		if !okObject {
			t.Fatalf("error = %#v, want the local error envelope", body["error"])
		}
		if errorType, _ := errorBody["type"].(string); errorType != "not_found" {
			t.Errorf("error.type = %#v, want %q", errorBody["type"], "not_found")
		}
	})
}

// TestHomeModelCatalogHandlersRefuseErrorEnvelopes drives the live handlers
// with a home response that carries an error envelope: it must be refused with
// the local error envelope, never relayed as a catalog.
func TestHomeModelCatalogHandlersRefuseErrorEnvelopes(t *testing.T) {
	for _, payload := range []string{
		`{"error":[]}`,
		`{"error":[{"id":"not-a-model"}]}`,
		`{"error":[],"claude":[{"id":"model-a"}]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			engine := newHomeCatalogEngine(t, payload)
			recorder, body := serveHomeCatalogRequest(t, engine, "/v1/models", nil)
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
			}
			if _, hasCatalog := body["data"]; hasCatalog {
				t.Errorf("error response carried a model catalog: %s", recorder.Body.String())
			}
			var envelope handlers.ErrorResponse
			if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &envelope); errUnmarshal != nil {
				t.Fatalf("body %q does not match handlers.ErrorResponse: %v", recorder.Body.String(), errUnmarshal)
			}
			if envelope.Error.Type != "server_error" {
				t.Errorf("error.type = %q, want %q", envelope.Error.Type, "server_error")
			}
			if !strings.Contains(envelope.Error.Message, "error envelope") {
				t.Errorf("error.message = %q, want it to name the refused error envelope", envelope.Error.Message)
			}
		})
	}
}
