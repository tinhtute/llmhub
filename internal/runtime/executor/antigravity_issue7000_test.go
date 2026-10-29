package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	_ "github.com/therealtinhtute/llmhub/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claude55LiteralSchema = `{"type":"object","properties":{"anthropic":{"type":"string","enum":["anthropic","json_schema"],"const":"anthropic"},"response_schema":{"$ref":"#/$defs/anthropic"}},"$defs":{"anthropic":{"type":"string"}},"required":["anthropic"],"additionalProperties":false}`

// assertClaude55PromptSchema decodes the schema out of the system instruction
// without depending on instruction wording or key order, so a rewrite of the
// prompt text cannot mask a lost constraint.
func assertClaude55PromptSchema(t *testing.T, instruction gjson.Result, schemaJSON string) {
	t.Helper()
	var prompt strings.Builder
	for _, part := range instruction.Get("parts").Array() {
		prompt.WriteString(part.Get("text").String())
		prompt.WriteByte('\n')
	}
	var want any
	if errUnmarshal := json.Unmarshal([]byte(schemaJSON), &want); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for offset, char := range prompt.String() {
		if char != '{' {
			continue
		}
		var got any
		if errDecode := json.NewDecoder(strings.NewReader(prompt.String()[offset:])).Decode(&got); errDecode == nil && reflect.DeepEqual(got, want) {
			return
		}
	}
	t.Fatalf("system instruction must contain the complete unmodified schema %s; got %q", schemaJSON, prompt.String())
}

// TestAntigravityStructuredOutputClaude55Fallback drives translation plus the
// real request build: Claude 5.5 routes get the schema as a prompt, no native
// output trigger, and no forced tool mode, while the caller's tools survive.
//
// Only the Responses source is covered: there is no openai-chat → antigravity
// translator chain locally (internal/translator/antigravity holds claude/,
// gemini/ and openai/responses/ only), so a chat response_format never reaches
// generationConfig.responseSchema and this fallback has nothing to convert.
func TestAntigravityStructuredOutputClaude55Fallback(t *testing.T) {
	wantSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":       map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
		},
		"required":             []any{"title", "description"},
		"additionalProperties": false,
	}
	for _, model := range []string{"claude-opus-5-5-high", "claude-sonnet-5-5-high"} {
		for _, tc := range []struct {
			name    string
			format  sdktranslator.Format
			payload string
			tools   string
		}{
			{
				name:   "responses",
				format: sdktranslator.FormatOpenAIResponse,
				payload: `{
					"input":[{"role":"user","content":[{"type":"input_text","text":"Provide title and description for: fix bug"}]}],
					"text":{"format":{"type":"json_schema","strict":true,"name":"codex_output_schema","schema":{
						"type":"object",
						"properties":{"title":{"type":"string"},"description":{"type":"string"}},
						"required":["title","description"],
						"additionalProperties":false
					}}}
				}`,
				tools: `[{"type":"function","name":"get_time","description":"Get the current time.","parameters":{"type":"object","properties":{"timezone":{"type":"string"}},"required":["timezone"]}}]`,
			},
		} {
			for _, withTools := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/tools=%t", model, tc.name, withTools), func(t *testing.T) {
					payload, errSet := sjson.SetBytes([]byte(tc.payload), "model", model)
					if errSet != nil {
						t.Fatalf("set model: %v", errSet)
					}
					if withTools {
						payload, _ = sjson.SetRawBytes(payload, "tools", []byte(tc.tools))
						payload, _ = sjson.SetBytes(payload, "tool_choice", "auto")
					}

					translated := sdktranslator.TranslateRequest(tc.format, sdktranslator.FormatAntigravity, model, payload, false)
					body := buildRequestBodyFromRawPayload(t, model, translated)
					encoded, errMarshal := json.Marshal(body)
					if errMarshal != nil {
						t.Fatalf("marshal outbound request: %v", errMarshal)
					}
					request := gjson.GetBytes(encoded, "request")

					if mode := request.Get("toolConfig.functionCallingConfig.mode").String(); mode != "" && mode != "AUTO" {
						t.Errorf("outbound function calling mode = %q, want AUTO or omitted; Claude 5.5 rejects forced tool use", mode)
					}
					for _, field := range []string{"responseSchema", "responseJsonSchema", "responseMimeType"} {
						if got := request.Get("generationConfig." + field); got.Exists() {
							t.Errorf("outbound generationConfig.%s = %s, want omitted for prompt-based structured output", field, got.Raw)
						}
					}

					var systemText strings.Builder
					for _, part := range request.Get("systemInstruction.parts").Array() {
						systemText.WriteString(part.Get("text").String())
						systemText.WriteByte('\n')
					}
					prompt := systemText.String()
					foundSchema := false
					for offset, char := range prompt {
						if char != '{' {
							continue
						}
						var schema map[string]any
						if errDecode := json.NewDecoder(strings.NewReader(prompt[offset:])).Decode(&schema); errDecode == nil && reflect.DeepEqual(schema, wantSchema) {
							foundSchema = true
							break
						}
					}
					if !foundSchema {
						t.Errorf("system instruction must preserve the title/description schema, required fields, and additionalProperties:false; got %q", prompt)
					}

					if withTools {
						declarations := request.Get("tools.0.functionDeclarations").Array()
						if len(declarations) != 1 {
							t.Fatalf("outbound function declaration count = %d, want 1 real tool; tools = %s", len(declarations), request.Get("tools").Raw)
						}
						for path, want := range map[string]string{
							"name":                                "get_time",
							"description":                         "Get the current time.",
							"parameters.type":                     "object",
							"parameters.properties.timezone.type": "string",
							"parameters.required.0":               "timezone",
						} {
							if got := declarations[0].Get(path).String(); got != want {
								t.Errorf("real tool %s = %q, want %q", path, got, want)
							}
						}
					}
				})
			}
		}
	}
}

// TestAntigravityStructuredOutputModelBoundaries keeps every non-5.5 route on
// its previous payload, and leaves a plain request's legacy tool mode alone.
func TestAntigravityStructuredOutputModelBoundaries(t *testing.T) {
	for _, tc := range []struct {
		model   string
		convert bool
	}{
		{"claude-opus-5-5", true}, {"claude-sonnet-5-5-high", true},
		{"claude-opus-5-5-reasoning", true}, {"claude-sonnet-5-5(high)", true},
		{"claude-opus-5-5-20261001", true}, {"claude-sonnet-5-5[1m]", true},
		{"claude-opus-5-5-20261001[1m](high)", true},
		{"claude-opus-5-50-high", false}, {"claude-sonnet-5-50", false},
		{"claude-opus-4-6", false}, {"claude-haiku-5-5", false},
		{"gemini-3.8-flash-high", false}, {"other/claude-opus-5-5-high", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			payload := []byte(`{"request":{"generationConfig":{"responseSchema":{"type":"object","properties":{"title":{"type":"string"}}},"responseMimeType":"application/json"}}}`)
			body := buildRequestBodyFromRawPayload(t, tc.model, payload)
			encoded, errMarshal := json.Marshal(body)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if got := gjson.GetBytes(encoded, "request.generationConfig.responseSchema").Exists(); got == tc.convert {
				t.Fatalf("native schema presence = %t, conversion = %t", got, tc.convert)
			}
			if tc.convert {
				assertClaude55PromptSchema(t, gjson.GetBytes(encoded, "request.systemInstruction"), `{"type":"object","properties":{"title":{"type":"string"}}}`)
			}
		})
	}
	for _, configJSON := range []string{`{"temperature":0.4}`, `{"responseMimeType":"text/plain"}`} {
		t.Run("plain/"+configJSON, func(t *testing.T) {
			payload := []byte(`{"request":{"generationConfig":` + configJSON + `,"systemInstruction":{"parts":[{"text":"unchanged"}]},"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}}}`)
			body := buildRequestBodyFromRawPayload(t, "claude-opus-5-5-high", payload)
			encoded, errMarshal := json.Marshal(body)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if got := gjson.GetBytes(encoded, "request.systemInstruction.parts.#").Int(); got != 1 {
				t.Fatalf("plain request gained instructions: %s", encoded)
			}
			if got := gjson.GetBytes(encoded, "request.toolConfig.functionCallingConfig.mode").String(); got != "VALIDATED" {
				t.Fatalf("unrelated legacy tool behavior changed: %q", got)
			}
		})
	}
}

// TestAntigravityStructuredOutputAliases covers both container spellings and
// both schema-key families, keeping unrelated settings in place.
func TestAntigravityStructuredOutputAliases(t *testing.T) {
	for _, container := range []string{"generationConfig", "generation_config"} {
		for index, schemaKey := range []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"} {
			for _, mode := range []string{"AUTO", "NONE", "ANY", "VALIDATED"} {
				t.Run(container+"/"+schemaKey+"/"+mode, func(t *testing.T) {
					payload := []byte(`{"request":{"generationConfig":{"temperature":0.25},"generation_config":{"stopSequences":["STOP"]},"systemInstruction":{"role":"system","parts":[{"text":"keep system"}]},"contents":[{"role":"user","parts":[{"text":"keep history"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]}]}}`)
					setRaw := func(path, value string) { payload, _ = sjson.SetRawBytes(payload, path, []byte(value)) }
					setRaw("request."+container+"."+schemaKey, claude55LiteralSchema)
					setRaw("request."+container+".responseMimeType", `"application/json"`)
					setRaw("request."+container+".response_mime_type", `"application/json"`)
					toolContainer := []string{"toolConfig", "tool_config"}[index%2]
					callingKey := []string{"functionCallingConfig", "function_calling_config"}[index/2]
					toolPath := "request." + toolContainer + "." + callingKey
					setRaw(toolPath, fmt.Sprintf(`{"mode":%q,"allowedFunctionNames":["lookup"],"allowed_function_names":["lookup"],"streamFunctionCallArguments":true}`, mode))
					systemKey := "systemInstruction"
					if container == "generation_config" {
						systemKey = "system_instruction"
						setRaw("request."+systemKey, gjson.GetBytes(payload, "request.systemInstruction").Raw)
						payload, _ = sjson.DeleteBytes(payload, "request.systemInstruction")
					}
					body := buildRequestBodyFromRawPayload(t, "claude-opus-5-5-high", payload)
					encoded, errMarshal := json.Marshal(body)
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					request := gjson.GetBytes(encoded, "request")
					assertClaude55PromptSchema(t, request.Get(systemKey), claude55LiteralSchema)
					for path, want := range map[string]string{
						"generationConfig.temperature":        "0.25",
						"generation_config.stopSequences.0":   "STOP",
						systemKey + ".parts.0.text":           "keep system",
						"contents.0.parts.0.text":             "keep history",
						"tools.0.functionDeclarations.0.name": "lookup",
					} {
						if got := request.Get(path).String(); got != want {
							t.Errorf("%s = %q, want %q", path, got, want)
						}
					}
					for _, configKey := range []string{"generationConfig", "generation_config"} {
						for _, key := range []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema", "responseMimeType", "response_mime_type"} {
							if request.Get(configKey + "." + key).Exists() {
								t.Errorf("native output trigger survived: %s.%s", configKey, key)
							}
						}
					}
					wantMode := mode
					if mode == "ANY" || mode == "VALIDATED" {
						wantMode = "AUTO"
						for _, key := range []string{"allowedFunctionNames", "allowed_function_names"} {
							if gjson.GetBytes(encoded, toolPath+"."+key).Exists() {
								t.Errorf("forced-tool allowlist survived AUTO downgrade: %s", key)
							}
						}
					}
					if got := gjson.GetBytes(encoded, toolPath+".mode").String(); got != wantMode {
						t.Errorf("tool choice = %q, want %q", got, wantMode)
					}
					if !gjson.GetBytes(encoded, toolPath+".streamFunctionCallArguments").Bool() {
						t.Error("unrelated tool configuration was removed")
					}
					if toolContainer == "tool_config" && request.Get("toolConfig").Exists() {
						t.Error("compatibility conversion introduced a conflicting camelCase tool config")
					}
				})
			}
		}
	}
}

// TestAntigravityStructuredOutputExecution drives the real executor paths and
// captures what Antigravity would receive. The "override" row is the ordering
// proof: the payload rule runs before the schema conversion, so the rule's own
// system instruction stays at parts.0 and the converted schema is appended
// after it. Converting before the rules would leave a single part and drop the
// schema.
func TestAntigravityStructuredOutputExecution(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse} {
		for _, operation := range []string{"execute", "stream", "count"} {
			// The local Antigravity CountTokens path applies no payload rules at
			// all (no ApplyPayloadConfig call before it dispatches), so the rule
			// rows only describe the request paths.
			ruleRows := []string{"none", "override", "filter"}
			if operation == "count" {
				ruleRows = []string{"none"}
			}
			for _, rules := range ruleRows {
				t.Run(format.String()+"/"+operation+"/"+rules, func(t *testing.T) {
					captured := make(chan []byte, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, errRead := io.ReadAll(r.Body)
						if errRead != nil {
							t.Error(errRead)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						captured <- body
						if operation == "count" {
							if r.URL.Path != antigravityCountTokensPath {
								t.Errorf("count path = %q", r.URL.Path)
							}
							_, _ = w.Write([]byte(`{"totalTokens":17}`))
							return
						}
						if r.URL.Path != antigravityStreamPath {
							t.Errorf("Claude must use stream upstream, got %q", r.URL.Path)
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"{}\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":17,\"candidatesTokenCount\":1,\"totalTokenCount\":18}}}\n\n")
					}))
					defer server.Close()
					cfg := &config.Config{DisableCooling: true}
					models := []config.PayloadModelRule{{Name: "claude-*", Protocol: "antigravity"}}
					switch rules {
					case "override":
						cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{
							"systemInstruction":                     map[string]any{"parts": []any{map[string]any{"text": "configured system"}}},
							"toolConfig.functionCallingConfig.mode": "NONE",
							"generationConfig.responseMimeType":     "configured/mime",
						}}}
					case "filter":
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"systemInstruction", "toolConfig", "contents.0"}}}
					}
					model := "claude-sonnet-5-5-high"
					payload := []byte(`{"model":"` + model + `","temperature":0.25,"tool_choice":"auto"}`)
					payload, _ = sjson.SetBytes(payload, "model", model)
					payload, _ = sjson.SetBytes(payload, "instructions", "keep system")
					payload, _ = sjson.SetRawBytes(payload, "input", []byte(`[{"role":"user","content":"first"},{"role":"assistant","content":"second"},{"role":"user","content":"third"}]`))
					payload, _ = sjson.SetRawBytes(payload, "text.format", []byte(`{"type":"json_schema","schema":`+claude55LiteralSchema+`}`))
					payload, _ = sjson.SetRawBytes(payload, "tools", []byte(`[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]`))
					exec := NewAntigravityExecutor(cfg)
					auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "token", "project_id": "project-test", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}}
					req := cliproxyexecutor.Request{Model: model, Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: format}
					switch operation {
					case "execute":
						if _, errExecute := exec.Execute(context.Background(), auth, req, opts); errExecute != nil {
							t.Fatal(errExecute)
						}
					case "stream":
						result, errStream := exec.ExecuteStream(context.Background(), auth, req, opts)
						if errStream != nil {
							t.Fatal(errStream)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					case "count":
						if _, errCount := exec.CountTokens(context.Background(), auth, req, opts); errCount != nil {
							t.Fatal(errCount)
						}
					}
					body := <-captured
					request := gjson.GetBytes(body, "request")
					if got := request.Get("tools.0.functionDeclarations.0.name").String(); got != "lookup" {
						t.Errorf("real tool lost: %s", body)
					}
					switch rules {
					case "none":
						assertClaude55PromptSchema(t, request.Get("systemInstruction"), claude55LiteralSchema)
						if !strings.Contains(request.Get("systemInstruction.parts.0.text").String(), "keep system") {
							t.Errorf("original system instruction was lost: %s", body)
						}
						for _, key := range []string{"responseSchema", "responseMimeType"} {
							if request.Get("generationConfig." + key).Exists() {
								t.Errorf("native output trigger survived: %s", body)
							}
						}
						if operation != "count" {
							if mode := request.Get("toolConfig.functionCallingConfig.mode").String(); mode != "" && mode != "AUTO" {
								t.Errorf("forced tool mode survived: %s", body)
							}
						}
					case "override":
						parts := request.Get("systemInstruction.parts").Array()
						if len(parts) < 2 {
							t.Fatalf("configured system instruction was replaced instead of extended: %s", body)
						}
						if got := parts[0].Get("text").String(); got != "configured system" {
							t.Errorf("configured system instruction moved or changed = %q; body=%s", got, body)
						}
						assertClaude55PromptSchema(t, request.Get("systemInstruction"), claude55LiteralSchema)
						if got := request.Get("toolConfig.functionCallingConfig.mode").String(); got != "NONE" {
							t.Errorf("payload rule did not remain final for tool mode: %q; body=%s", got, body)
						}
					case "filter":
						// The filter runs first, so the caller's system instruction and
						// tool config are gone; the Claude 5.5 conversion then supplies
						// the only system instruction left. A filter undone by the build
						// would show up as the restored "keep system" text or a toolConfig.
						if strings.Contains(request.Get("systemInstruction").Raw, "keep system") {
							t.Errorf("filtered system instruction was restored: %s", body)
						}
						if request.Get("toolConfig").Exists() || request.Get("contents.#").Int() != 2 {
							t.Errorf("filtered tool config or content survived: %s", body)
						}
						if request.Get("systemInstruction.parts.#").Int() != 1 {
							t.Errorf("expected only the converted schema instruction: %s", body)
						}
						assertClaude55PromptSchema(t, request.Get("systemInstruction"), claude55LiteralSchema)
					}
				})
			}
		}
	}
}
