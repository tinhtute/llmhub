package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityBuildRequestBaseURL(t *testing.T) {
	t.Run("default uses daily endpoint", func(t *testing.T) {
		if got := buildBaseURL(&cliproxyauth.Auth{}); got != antigravityBaseURLDaily {
			t.Fatalf("base URL = %q, want %q", got, antigravityBaseURLDaily)
		}
	})

	t.Run("custom attribute endpoint remains supported", func(t *testing.T) {
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://enterprise.example.com/"}}
		if got := buildBaseURL(auth); got != "https://enterprise.example.com" {
			t.Fatalf("base URL = %q, want custom endpoint", got)
		}
	})

	t.Run("custom auth file endpoint remains supported", func(t *testing.T) {
		auth := &cliproxyauth.Auth{Metadata: map[string]any{"base_url": "https://enterprise.example.com/"}}
		if got := buildBaseURL(auth); got != "https://enterprise.example.com" {
			t.Fatalf("base URL = %q, want custom auth file endpoint", got)
		}
	})
}

func TestAntigravityLoadCodeAssistBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{"default daily", &cliproxyauth.Auth{}, antigravityBaseURLDaily},
		{"custom attribute", &cliproxyauth.Auth{Attributes: map[string]string{"base_url": antigravityBaseURLProd + "/"}}, antigravityBaseURLProd},
		{"custom metadata", &cliproxyauth.Auth{Metadata: map[string]any{"base_url": "https://enterprise.example.com/"}}, "https://enterprise.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := antigravityLoadCodeAssistBaseURL(tc.auth); got != tc.want {
				t.Fatalf("loadCodeAssist base URL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAntigravityBuildRequest_SanitizesGeminiToolSchema(t *testing.T) {
	body := buildRequestBodyFromPayload(t, "gemini-2.5-pro")

	decl := extractFirstFunctionDeclaration(t, body)
	if _, ok := decl["parametersJsonSchema"]; ok {
		t.Fatalf("parametersJsonSchema should be renamed to parameters")
	}

	params, ok := decl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing or invalid type")
	}
	assertSchemaSanitizedAndPropertyPreserved(t, params)
}

func TestAntigravityBuildRequest_SanitizesAntigravityToolSchema(t *testing.T) {
	body := buildRequestBodyFromPayload(t, "claude-opus-4-6")

	decl := extractFirstFunctionDeclaration(t, body)
	params, ok := decl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing or invalid type")
	}
	assertSchemaSanitizedAndPropertyPreserved(t, params)
}

func TestAntigravityBuildRequest_SkipsSchemaSanitizationWithoutToolsField(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-image", []byte(`{
		"request": {
			"contents": [
				{
					"role": "user",
					"x-debug": "keep-me",
					"parts": [
						{
							"text": "hello"
						}
					]
				}
			],
			"nonSchema": {
				"nullable": true,
				"x-extra": "keep-me"
			},
			"generationConfig": {
				"maxOutputTokens": 128
			}
		}
	}`))

	assertNonSchemaRequestPreserved(t, body)
}

func TestAntigravityBuildRequest_SkipsSchemaSanitizationWithEmptyToolsArray(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-image", []byte(`{
		"request": {
			"tools": [],
			"contents": [
				{
					"role": "user",
					"x-debug": "keep-me",
					"parts": [
						{
							"text": "hello"
						}
					]
				}
			],
			"nonSchema": {
				"nullable": true,
				"x-extra": "keep-me"
			},
			"generationConfig": {
				"maxOutputTokens": 128
			}
		}
	}`))

	assertNonSchemaRequestPreserved(t, body)
}

func TestAntigravityBuildRequest_PreservesHistoryWhileSanitizingSchemas(t *testing.T) {
	payload := []byte(`{
		"request": {
			"contents": [
				{"role": "model", "parts": [{"functionCall": {"name": "manage_todo_list", "args": {
					"operation": "write",
					"todoList": [
						{"id": 1, "title": "output 1", "description": "d1", "status": "not-started"},
						{"id": 2, "title": "output 2", "description": "d2", "status": "not-started"}
					]
				}}}]},
				{"role": "model", "parts": [{"functionCall": {"name": "write_file", "args": {
					"path": "a.md", "format": "markdown", "default": "x", "pattern": "p",
					"const": "c", "deprecated": false, "nullable": "n", "examples": "e",
					"additionalProperties": "ap", "x-custom": "keepme"
				}}}]}
			],
			"tools": [{"functionDeclarations": [{
				"name": "manage_todo_list",
				"parametersJsonSchema": {
					"type": "object",
					"required": ["todoList"],
					"properties": {"todoList": {"type": "array", "items": {
						"type": "object",
						"required": ["id", "title"],
						"title": "TodoItem",
						"properties": {"id": {"type": "number"}, "title": {"type": "string", "minLength": 3}}
					}}}
				}
			}] }]
		}
	}`)

	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-pro", payload)
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}

	todo := gjson.GetBytes(encoded, `request.contents.0.parts.0.functionCall.args.todoList`)
	for i, item := range todo.Array() {
		if !item.Get("title").Exists() {
			t.Errorf("todoList[%d] lost its title: %s", i, item.Raw)
		}
	}

	args := gjson.GetBytes(encoded, `request.contents.1.parts.0.functionCall.args`)
	for _, key := range []string{"format", "default", "pattern", "const", "deprecated", "examples", "additionalProperties", "x-custom"} {
		if !args.Get(gjson.Escape(key)).Exists() {
			t.Errorf("argument key %q was stripped from history: %s", key, args.Raw)
		}
	}
	if args.Get("enum").Exists() {
		t.Errorf("cleaner fabricated an enum key in history args: %s", args.Raw)
	}

	items := gjson.GetBytes(encoded, "request.tools.0.functionDeclarations.0.parameters.properties.todoList.items")
	if items.Get("title").Exists() {
		t.Errorf("schema keyword title was not removed: %s", items.Raw)
	}
	if items.Get("properties.title.minLength").Exists() {
		t.Errorf("unsupported keyword minLength was not removed: %s", items.Raw)
	}
	if !items.Get("properties.title").Exists() {
		t.Errorf("schema property named title must be preserved: %s", items.Raw)
	}
}

func TestAntigravityBuildRequest_SanitizesAdditionalSchemaLocations(t *testing.T) {
	payload := []byte(`{"request": {
		"tools": [{"function_declarations": [{
			"name": "t",
			"parameters_json_schema": {"type": "object", "$id": "drop-a", "properties": {"a": {"type": "string"}}},
			"response": {"type": "object", "$comment": "drop-b", "properties": {"b": {"type": "string"}}},
			"response_json_schema": {"type": "object", "$id": "drop-c", "properties": {"c": {"type": "string"}}}
		}] }],
		"generation_config": {"response_schema": {"type": "object", "$id": "drop-d", "properties": {"d": {"type": "string"}}}}
	}}`)

	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-pro", payload)
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}

	for _, path := range []string{
		`request.tools.0.function_declarations.0.parameters_json_schema.\$id`,
		`request.tools.0.function_declarations.0.response.\$comment`,
		`request.tools.0.function_declarations.0.response_json_schema.\$id`,
		`request.generation_config.response_schema.\$id`,
	} {
		if gjson.GetBytes(encoded, path).Exists() {
			t.Errorf("unsupported keyword at %s survived cleaning: %s", path, encoded)
		}
	}
}

func TestAntigravityBuildRequest_PreservesResponseUnionAndEnumType(t *testing.T) {
	payload := []byte(`{"request":{
		"tools":[{"functionDeclarations":[{"name":"tool","parameters":{"type":"object","properties":{
			"choice":{"anyOf":[{"type":"string"},{"type":"null"}]},
			"level":{"type":"number","enum":[1,2]}
		}}}]}],
		"generationConfig":{"responseSchema":{"type":"object","properties":{
			"action":{"anyOf":[{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},{"type":"null"}]},
			"conviction":{"type":"number","enum":[0.25,0.5,1]}
		}}}
	}}`)

	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-pro", payload)
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}

	responseSchema := gjson.GetBytes(encoded, "request.generationConfig.responseSchema")
	union := responseSchema.Get("properties.action.anyOf")
	if !union.IsArray() || len(union.Array()) != 2 || union.Get("1.type").String() != "null" {
		t.Errorf("response anyOf union was flattened: %s", responseSchema.Raw)
	}
	conviction := responseSchema.Get("properties.conviction")
	if gotType := conviction.Get("type").String(); gotType != "number" {
		t.Errorf("response enum type = %q, want number: %s", gotType, responseSchema.Raw)
	}
	for _, enumValue := range conviction.Get("enum").Array() {
		if enumValue.Type != gjson.String {
			t.Errorf("response enum value is not a string: %s", conviction.Raw)
		}
	}

	toolSchema := gjson.GetBytes(encoded, "request.tools.0.functionDeclarations.0.parameters")
	if toolSchema.Get("properties.choice.anyOf").Exists() {
		t.Errorf("tool anyOf union was not flattened: %s", toolSchema.Raw)
	}
	if gotType := toolSchema.Get("properties.level.type").String(); gotType != "string" {
		t.Errorf("tool enum type = %q, want string: %s", gotType, toolSchema.Raw)
	}
}

func TestAntigravityBuildRequest_UsesAuthProjectID(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-pro", []byte(`{
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "hello"}]
				}
			]
		}
	}`))

	if got, ok := body["project"].(string); !ok || got != "project-1" {
		t.Fatalf("project should come from auth metadata, got=%v", body["project"])
	}
}

func TestAntigravityBuildRequest_UsesRouteModelWhenPayloadContainsDifferentModel(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3-flash-agent", []byte(`{
		"model": "gemini-3.1-flash-lite",
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "Perform a web search"}]
				}
			],
			"tools": [{"googleSearch": {}}]
		}
	}`))

	if got, ok := body["model"].(string); !ok || got != "gemini-3-flash-agent" {
		t.Fatalf("request model should stay on route model, got=%v", body["model"])
	}
}

func TestAntigravityBuildRequestUsesDerivedSessionIDAndPreservesExplicit(t *testing.T) {
	t.Parallel()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	req, err := executor.buildRequest(context.Background(), auth, "token", "gemini-3.1-pro", payload, false, "", "https://example.com", "-123456789")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	body := requestBody(t, req)
	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid: %v", body["request"])
	}
	if got := request["sessionId"]; got != "-123456789" {
		t.Fatalf("request.sessionId = %v, want -123456789", got)
	}

	explicitPayload := []byte(`{"request":{"sessionId":"-987654321","contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	explicitReq, errExplicit := executor.buildRequest(context.Background(), auth, "token", "gemini-3.1-pro", explicitPayload, false, "", "https://example.com", "-123456789")
	if errExplicit != nil {
		t.Fatalf("buildRequest explicit error: %v", errExplicit)
	}
	explicitBody := requestBody(t, explicitReq)
	explicitRequest, ok := explicitBody["request"].(map[string]any)
	if !ok {
		t.Fatalf("explicit request missing or invalid: %v", explicitBody["request"])
	}
	if got := explicitRequest["sessionId"]; got != "-987654321" {
		t.Fatalf("explicit request.sessionId = %v, want -987654321", got)
	}
}

func TestAntigravityBuildRequest_PreservesIndependentWebSearchRequestType(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-lite", []byte(`{
		"requestType": "web_search",
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "北京天气 2026-06-12"}]
				}
			],
			"tools": [
				{
					"googleSearch": {
						"enhancedContent": {
							"imageSearch": {
								"maxResultCount": 5
							}
						}
					}
				}
			],
			"generationConfig": {
				"candidateCount": 1
			}
		}
	}`))

	if got, ok := body["requestType"].(string); !ok || got != "web_search" {
		t.Fatalf("requestType should stay web_search, got=%v", body["requestType"])
	}
	if _, ok := body["requestId"]; ok {
		t.Fatalf("web_search request should not add requestId: %v", body["requestId"])
	}
	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid: %v", body["request"])
	}
	if _, ok := request["sessionId"]; ok {
		t.Fatalf("web_search request should not add request.sessionId: %v", request["sessionId"])
	}
	if got, ok := body["project"].(string); !ok || got != "project-1" {
		t.Fatalf("project should come from auth metadata, got=%v", body["project"])
	}
}

func TestShouldResolveAntigravityWebSearchGroundingURLsRequiresTypedWebSearchAndSearchRequest(t *testing.T) {
	original := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	originalResponses := []byte(`{"tools":[{"type":"web_search"}]}`)
	originalResponsesPreview := []byte(`{"tools":[{"type":"web_search_preview_2025_03_11"}]}`)
	translatedWithGoogleSearch := []byte(`{"requestType":"web_search","request":{"tools":[{"googleSearch":{}}]}}`)
	translatedWithoutGoogleSearch := []byte(`{"request":{"contents":[]}}`)

	if !shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatClaude, original, translatedWithGoogleSearch) {
		t.Fatal("expected typed Claude web search translated to web_search request to resolve grounding URLs")
	}
	if !shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatOpenAIResponse, originalResponses, translatedWithGoogleSearch) {
		t.Fatal("expected typed OpenAI Responses web search translated to web_search request to resolve grounding URLs")
	}
	if !shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatOpenAIResponse, originalResponsesPreview, translatedWithGoogleSearch) {
		t.Fatal("expected web_search_preview_2025_03_11 translated to web_search request to resolve grounding URLs")
	}
	if shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatClaude, original, translatedWithoutGoogleSearch) {
		t.Fatal("expected request without googleSearch to skip grounding URL resolution")
	}
	if shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatOpenAI, original, translatedWithGoogleSearch) {
		t.Fatal("expected non-Claude/non-Responses source format to skip grounding URL resolution")
	}
}

func TestAntigravityPrepareRequestAuth_FetchesMissingProjectID(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"access_token": "token",
		"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected project discovery request: %s", req.URL.String())
		}
		if got := req.Header.Get("X-Goog-Api-Client"); got != "" {
			t.Fatalf("X-Goog-Api-Client = %q, want empty", got)
		}
		raw, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("read discovery body: %v", errRead)
		}
		if !strings.Contains(string(raw), `"ideType":"ANTIGRAVITY"`) {
			t.Fatalf("unexpected discovery body: %s", string(raw))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"cloudaicompanionProject":"fetched-project"}`)),
		}, nil
	}))

	updated, err := executor.PrepareRequestAuth(ctx, auth)
	if err != nil {
		t.Fatalf("PrepareRequestAuth error: %v", err)
	}
	if updated == nil {
		t.Fatalf("PrepareRequestAuth returned nil auth")
	}
	if _, ok := auth.Metadata["project_id"]; ok {
		t.Fatalf("original auth metadata should not be mutated")
	}
	if got, ok := updated.Metadata["project_id"].(string); !ok || got != "fetched-project" {
		t.Fatalf("updated auth metadata project_id = %v, want fetched-project", updated.Metadata["project_id"])
	}
}

func TestAntigravityPrepareRequestAuth_UpstreamForbiddenPreserves403(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"access_token": "token",
		"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"The caller does not have permission"}}`)),
		}, nil
	}))

	_, err := executor.PrepareRequestAuth(ctx, auth)
	if err == nil {
		t.Fatalf("PrepareRequestAuth should fail on upstream 403")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error should expose StatusCode(), got %T (%v)", err, err)
	}
	if got := status.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status code = %d, want %d", got, http.StatusForbidden)
	}
}

func TestAntigravityBuildRequest_RejectsMissingProjectID(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{}}

	_, err := executor.buildRequest(context.Background(), auth, "token", "gemini-3.1-pro", []byte(`{"request":{}}`), false, "", "https://example.com")
	if err == nil {
		t.Fatalf("buildRequest should fail when auth has no project_id")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error should expose status code, got %T", err)
	}
	if got := status.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", got, http.StatusBadRequest)
	}
}

func assertNonSchemaRequestPreserved(t *testing.T, body map[string]any) {
	t.Helper()

	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid type")
	}

	contents, ok := request["contents"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatalf("contents missing or empty")
	}
	content, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("content missing or invalid type")
	}
	if got, ok := content["x-debug"].(string); !ok || got != "keep-me" {
		t.Fatalf("x-debug should be preserved when no tool schema exists, got=%v", content["x-debug"])
	}

	nonSchema, ok := request["nonSchema"].(map[string]any)
	if !ok {
		t.Fatalf("nonSchema missing or invalid type")
	}
	if _, ok := nonSchema["nullable"]; !ok {
		t.Fatalf("nullable should be preserved outside schema cleanup path")
	}
	if got, ok := nonSchema["x-extra"].(string); !ok || got != "keep-me" {
		t.Fatalf("x-extra should be preserved outside schema cleanup path, got=%v", nonSchema["x-extra"])
	}

	if generationConfig, ok := request["generationConfig"].(map[string]any); ok {
		if _, ok := generationConfig["maxOutputTokens"]; ok {
			t.Fatalf("maxOutputTokens should still be removed for non-Claude requests")
		}
	}
}

func buildRequestBodyFromPayload(t *testing.T, modelName string) map[string]any {
	t.Helper()
	return buildRequestBodyFromRawPayload(t, modelName, []byte(`{
		"request": {
			"tools": [
				{
					"function_declarations": [
						{
							"name": "tool_1",
							"parametersJsonSchema": {
								"$schema": "http://json-schema.org/draft-07/schema#",
								"$id": "root-schema",
								"type": "object",
								"properties": {
									"$id": {"type": "string"},
									"arg": {
										"type": "object",
										"prefill": "hello",
										"properties": {
											"mode": {
												"type": "string",
												"deprecated": true,
												"enum": ["a", "b"],
												"enumTitles": ["A", "B"]
											}
										}
									}
								},
								"patternProperties": {
									"^x-": {"type": "string"}
								}
							}
						}
					]
				}
			]
		}
	}`))
}

func buildRequestBodyFromRawPayload(t *testing.T, modelName string, payload []byte) map[string]any {
	t.Helper()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}

	req, err := executor.buildRequest(context.Background(), auth, "token", modelName, payload, false, "", "https://example.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}

	return requestBody(t, req)
}

func requestBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body error: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal request body error: %v, body=%s", err, string(raw))
	}
	return body
}

func extractFirstFunctionDeclaration(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid type")
	}
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing or empty")
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("first tool invalid type")
	}
	decls, ok := tool["function_declarations"].([]any)
	if !ok || len(decls) == 0 {
		t.Fatalf("function_declarations missing or empty")
	}
	decl, ok := decls[0].(map[string]any)
	if !ok {
		t.Fatalf("first function declaration invalid type")
	}
	return decl
}

func assertSchemaSanitizedAndPropertyPreserved(t *testing.T, params map[string]any) {
	t.Helper()

	if _, ok := params["$id"]; ok {
		t.Fatalf("root $id should be removed from schema")
	}
	if _, ok := params["patternProperties"]; ok {
		t.Fatalf("patternProperties should be removed from schema")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or invalid type")
	}
	if _, ok := props["$id"]; !ok {
		t.Fatalf("property named $id should be preserved")
	}

	arg, ok := props["arg"].(map[string]any)
	if !ok {
		t.Fatalf("arg property missing or invalid type")
	}
	if _, ok := arg["prefill"]; ok {
		t.Fatalf("prefill should be removed from nested schema")
	}

	argProps, ok := arg["properties"].(map[string]any)
	if !ok {
		t.Fatalf("arg.properties missing or invalid type")
	}
	mode, ok := argProps["mode"].(map[string]any)
	if !ok {
		t.Fatalf("mode property missing or invalid type")
	}
	if _, ok := mode["enumTitles"]; ok {
		t.Fatalf("enumTitles should be removed from nested schema")
	}
	if _, ok := mode["deprecated"]; ok {
		t.Fatalf("deprecated should be removed from nested schema")
	}
}

// Ported from upstream CLIProxyAPI commit c93978c4 (normalize true boolean
// subschemas, issue #3551); upstream file antigravity_schema_sanitize_test.go
// adapted into the local buildrequest test layout.

func TestSanitizeAntigravityRequestSchemasNormalizesTrueBooleanSubschemas(t *testing.T) {
	// Issue #3551: RevenueCat MCP and other MCP tools declare boolean `true` subschemas in properties.
	// Upstream Antigravity rejects boolean `true` in Schema objects with HTTP 400.
	payload := `{"request":{
		"tools":[{"functionDeclarations":[{
			"name":"mcp__revenuecat_create_product_store_state_plan",
			"parametersJsonSchema":{
				"type":"object",
				"properties":{
					"screenshot_id":true,
					"filename":true,
					"file_size":true,
					"source_file_checksum":true,
					"disabled":false,
					"metadata":{
						"type":"object",
						"properties":{
							"nested_flag":true
						}
					},
					"tags":{
						"type":"array",
						"items":true
					},
					"variants":{
						"anyOf":[true,{"type":"string"}]
					},
					"large_int":9007199254740993
				},
				"additionalProperties":true
			}
		}]}]
	}}`

	for _, useAntigravitySchema := range []bool{false, true} {
		got := sanitizeAntigravityRequestSchemasForModel(payload, useAntigravitySchema, false)
		decl := gjson.Get(got, "request.tools.0.functionDeclarations.0")
		params := decl.Get("parameters")
		if !params.Exists() {
			t.Fatalf("parameters missing in sanitized declaration: %s", decl.Raw)
		}

		for _, prop := range []string{"screenshot_id", "filename", "file_size", "source_file_checksum"} {
			val := params.Get("properties." + prop)
			if !val.Exists() {
				t.Errorf("useAntigravitySchema=%v: property %s missing: %s", useAntigravitySchema, prop, params.Raw)
			}
			if val.Type != gjson.JSON || val.Raw != "{}" {
				t.Errorf("useAntigravitySchema=%v: property %s should be {}, got %s", useAntigravitySchema, prop, val.Raw)
			}
		}

		// Check false is preserved
		if val := params.Get("properties.disabled"); val.Type != gjson.False {
			t.Errorf("useAntigravitySchema=%v: disabled should be false, got %s", useAntigravitySchema, val.Raw)
		}

		// Check nested property
		if val := params.Get("properties.metadata.properties.nested_flag"); val.Type != gjson.JSON || val.Raw != "{}" {
			t.Errorf("useAntigravitySchema=%v: nested_flag should be {}, got %s", useAntigravitySchema, val.Raw)
		}

		// Check items: true -> items: {}
		if val := params.Get("properties.tags.items"); val.Type != gjson.JSON || val.Raw != "{}" {
			t.Errorf("useAntigravitySchema=%v: tags.items should be {}, got %s", useAntigravitySchema, val.Raw)
		}

		// Check large int preserved without float conversion
		if val := params.Get("properties.large_int"); val.Raw != "9007199254740993" {
			t.Errorf("useAntigravitySchema=%v: large_int corrupted, got %s", useAntigravitySchema, val.Raw)
		}
	}
}

func TestAntigravityBuildRequestScalesLinearlyWithDeclarations(t *testing.T) {
	const declCount = 256

	var input strings.Builder
	input.WriteString(`{"request":{"tools":[{"functionDeclarations":[`)
	for i := range declCount {
		if i > 0 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, `{"name":"tool_%d","description":"%s","parametersJsonSchema":{"type":"object","$id":"drop","properties":{"q":{"type":"string"}}}}`, i, strings.Repeat("x", 4096))
	}
	input.WriteString(`]}]}}`)

	payload := []byte(input.String())
	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "test-auth", Provider: "antigravity", Metadata: map[string]any{"project_id": "test-project"}}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	req, errReq := exec.buildRequest(context.Background(), auth, "token", "gemini-3.8-flash-high", payload, false, "", "")
	runtime.ReadMemStats(&after)

	if errReq != nil {
		t.Fatalf("buildRequest() error = %v", errReq)
	}
	reqBody, _ := io.ReadAll(req.Body)
	firstDecl := gjson.GetBytes(reqBody, "request.tools.0.functionDeclarations.0")
	if !firstDecl.Get("parameters.properties.q").Exists() {
		t.Fatalf("first declaration parameter q missing: %s", firstDecl.Raw)
	}
	if firstDecl.Get("parametersJsonSchema").Exists() {
		t.Fatalf("parametersJsonSchema was not renamed: %s", firstDecl.Raw)
	}

	const allowedCopies = 48
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(allowedCopies*len(payload)) {
		t.Fatalf("live buildRequest with %d declarations allocated %d bytes for a %d byte payload, want at most %d (must scale linearly with tools size)",
			declCount, allocated, len(payload), allowedCopies*len(payload))
	}
}

func TestAntigravityEnsureGeminiBoundaryUserContent_DropsTrailingThoughtTurns(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":"thought","thought":true}]}]}}`)

	out := ensureAntigravityGeminiBoundaryUserContent("gemini-3.8-flash-high", payload)
	contents := gjson.GetBytes(out, "request.contents").Array()
	if len(contents) != 1 {
		t.Fatalf("expected 1 turn (trailing thought dropped), got %d turns: %s", len(contents), out)
	}
	if contents[0].Get("role").String() != "user" || contents[0].Get("parts.0.text").String() != "hello" {
		t.Fatalf("user turn corrupted: %s", out)
	}
}

func TestAntigravityBuildRequest_NonClaudeSourceTargetingClaudeModel_FlattensToolUnions(t *testing.T) {
	payload := []byte(`{"request":{"tools":[{"functionDeclarations":[{
		"name": "edit_item",
		"parametersJsonSchema": {
			"type": "object",
			"properties": {
				"item": {
					"anyOf": [
						{"type": "object", "properties": {"image": {"type": "string"}}},
						{"type": "object", "properties": {"audio": {"type": "string"}}},
						{"type": "string"}
					]
				}
			}
		}
	}]}]}}`)

	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "test-auth", Provider: "antigravity", Metadata: map[string]any{"project_id": "test-project"}}

	// 1. Non-Claude source request targeting Claude model flattens the union
	claudeReq, errClaude := exec.buildRequest(context.Background(), auth, "token", "claude-sonnet-4-6", payload, false, "", "")
	if errClaude != nil {
		t.Fatalf("buildRequest(claude) error = %v", errClaude)
	}
	claudeBody, _ := io.ReadAll(claudeReq.Body)
	claudeItem := gjson.GetBytes(claudeBody, "request.tools.0.functionDeclarations.0.parameters.properties.item")
	if claudeItem.Get("anyOf").Exists() || claudeItem.Get("oneOf").Exists() {
		t.Fatalf("Claude model tool union was not flattened: %s", claudeBody)
	}
	if got := claudeItem.Get("type").String(); got != "object" {
		t.Fatalf("Claude model tool item type = %q, want object: %s", got, claudeBody)
	}
	if !claudeItem.Get("properties.image").Exists() {
		t.Fatalf("Claude model tool item lost first object branch: %s", claudeBody)
	}

	// 2. Non-Claude source request targeting Gemini model preserves the union
	geminiReq, errGemini := exec.buildRequest(context.Background(), auth, "token", "gemini-3.8-flash-high", payload, false, "", "")
	if errGemini != nil {
		t.Fatalf("buildRequest(gemini) error = %v", errGemini)
	}
	geminiBody, _ := io.ReadAll(geminiReq.Body)
	geminiItem := gjson.GetBytes(geminiBody, "request.tools.0.functionDeclarations.0.parameters.properties.item")
	if !geminiItem.Get("anyOf").Exists() {
		t.Fatalf("Gemini model tool union should be preserved: %s", geminiBody)
	}
}

func TestAntigravityBuildRequestGenerationSchemaAllEightSpellings(t *testing.T) {
	cases := []string{
		"request.generationConfig.responseSchema",
		"request.generationConfig.response_schema",
		"request.generationConfig.responseJsonSchema",
		"request.generationConfig.response_json_schema",
		"request.generation_config.responseSchema",
		"request.generation_config.response_schema",
		"request.generation_config.responseJsonSchema",
		"request.generation_config.response_json_schema",
	}
	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "test-auth", Provider: "antigravity", Metadata: map[string]any{"project_id": "test-project"}}

	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			container := ""
			key := ""
			for i := len(path) - 1; i >= 0; i-- {
				if path[i] == '.' {
					container = path[:i]
					key = path[i+1:]
					break
				}
			}
			payload := []byte(`{"request":{"` + container[len("request."):] + `":{"` + key + `":{"type":"object","$id":"drop","properties":{"d":{"type":"string"}}}}}}`)
			req, err := exec.buildRequest(context.Background(), auth, "token", "gemini-3.1-pro", payload, false, "", "")
			if err != nil {
				t.Fatalf("buildRequest() error = %v", err)
			}
			raw, _ := io.ReadAll(req.Body)
			if gjson.GetBytes(raw, path+".\\$id").Exists() {
				t.Fatalf("unsupported keyword $id survived cleaning at %s: %s", path, raw)
			}
			if !gjson.GetBytes(raw, path+".properties.d").Exists() {
				t.Fatalf("property d was lost at %s: %s", path, raw)
			}
		})
	}
}
