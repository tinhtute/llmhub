package chat_completions

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	internalsignature "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToGeminiMapsJSONSchemaResponseFormat(t *testing.T) {
	input := []byte(`{
		"generationConfig": {
			"temperature": 0.2,
			"responseSchema": {"type":"string"},
			"responseJsonSchema": {"type":"number"}
		},
		"messages": [{"role":"user","content":"Return JSON"}],
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"name": "answer",
				"schema": {"type":"object","properties":{"ok":{"type":"boolean"}},"additionalProperties":false}
			}
		}
	}`)

	out := ConvertOpenAIRequestToGemini("gemini-test", input, false)
	if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "generationConfig.responseJsonSchema.properties.ok.type").String(); got != "boolean" {
		t.Fatalf("responseJsonSchema ok.type = %q, want boolean; out=%s", got, out)
	}
	if gjson.GetBytes(out, "generationConfig.responseSchema").Exists() {
		t.Fatalf("stale responseSchema survived: %s", out)
	}
	if got := gjson.GetBytes(out, "generationConfig.responseJsonSchema.additionalProperties"); !got.Exists() || got.Bool() {
		t.Fatalf("responseJsonSchema.additionalProperties = %s, want false; out=%s", got.Raw, out)
	}
	if got := gjson.GetBytes(out, "generationConfig.temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2; out=%s", got, out)
	}
}

func TestConvertOpenAIRequestToGeminiMapsJSONObjectResponseFormat(t *testing.T) {
	input := []byte(`{"generationConfig":{"responseJsonSchema":{"type":"string"}},"messages":[{"role":"user","content":"Return JSON"}],"response_format":{"type":"json_object"}}`)

	out := ConvertOpenAIRequestToGemini("gemini-test", input, false)
	if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json; out=%s", got, out)
	}
	if gjson.GetBytes(out, "generationConfig.responseJsonSchema").Exists() {
		t.Fatalf("responseJsonSchema should not be set for json_object; out=%s", out)
	}
}

func TestConvertOpenAIRequestToGeminiJSONSchemaWithoutSchemaDoesNotSetStaleSchema(t *testing.T) {
	input := []byte(`{"generationConfig":{"responseJsonSchema":{"type":"string"}},"messages":[{"role":"user","content":"Return JSON"}],"response_format":{"type":"json_schema","json_schema":{"name":"answer"}}}`)

	out := ConvertOpenAIRequestToGemini("gemini-test", input, false)
	if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json; out=%s", got, out)
	}
	if gjson.GetBytes(out, "generationConfig.responseJsonSchema").Exists() {
		t.Fatalf("responseJsonSchema should not be set without schema; out=%s", out)
	}
}

func TestConvertOpenAIRequestToGeminiNormalizesFileData(t *testing.T) {
	input := []byte(`{
		"messages": [
			{
				"role": "user",
				"content": [
					{"type":"image_url","image_url":{"url":"data:image/png;base64,image-user"}},
					{"type":"image_url","image_url":{"url":"data:image/png,image-invalid"}},
					{"type":"file","file":{"filename":"report.PDF","file_data":"file-raw"}},
					{"type":"file","file":{"filename":"wrong.txt","file_data":"data:image/jpeg;base64,file-url"}},
					{"type":"file","file":{"filename":"guess.pdf","file_data":"data:;base64,file-invalid"}}
				]
			},
			{
				"role": "assistant",
				"content": [
					{"type":"image_url","image_url":{"url":"data:image/webp;base64,image-assistant"}},
					{"type":"image_url","image_url":{"url":"data:image/webp;base64,"}}
				]
			}
		]
	}`)

	out := ConvertOpenAIRequestToGemini("gemini-test", input, false)
	userParts := gjson.GetBytes(out, "contents.0.parts").Array()
	if len(userParts) != 3 {
		t.Fatalf("expected 3 normalized user parts, got %d: %s", len(userParts), gjson.GetBytes(out, "contents.0.parts").Raw)
	}
	assertGeminiInlineData(t, userParts[0], "image/png", "image-user", geminiFunctionThoughtSignature)
	assertGeminiInlineData(t, userParts[1], "application/pdf", "file-raw", "")
	assertGeminiInlineData(t, userParts[2], "image/jpeg", "file-url", "")

	assistantParts := gjson.GetBytes(out, "contents.1.parts").Array()
	if len(assistantParts) != 1 {
		t.Fatalf("expected 1 normalized assistant part, got %d: %s", len(assistantParts), gjson.GetBytes(out, "contents.1.parts").Raw)
	}
	assertGeminiInlineData(t, assistantParts[0], "image/webp", "image-assistant", geminiFunctionThoughtSignature)
}

func assertGeminiInlineData(t *testing.T, part gjson.Result, wantMIMEType, wantData, wantThoughtSignature string) {
	t.Helper()
	if got := part.Get("inlineData.mime_type").String(); got != wantMIMEType {
		t.Errorf("inlineData.mime_type = %q, want %q", got, wantMIMEType)
	}
	if got := part.Get("inlineData.data").String(); got != wantData {
		t.Errorf("inlineData.data = %q, want %q", got, wantData)
	}
	if got := part.Get("thoughtSignature").String(); got != wantThoughtSignature {
		t.Errorf("thoughtSignature = %q, want %q", got, wantThoughtSignature)
	}
}

// Ported from upstream CLIProxyAPI commit 0fe19ede90a4 ("preserve Gemini prompt
// cache by demoting mid-session developer messages"): a mid-session developer
// message demotes to a user turn instead of mutating systemInstruction.
func TestConvertOpenAIRequestToGemini_MidSessionDeveloperMessageDoesNotMutateSystemInstruction(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant"},
			{"role": "user", "content": "Turn 1 user"},
			{"role": "assistant", "content": "Turn 1 assistant"},
			{"role": "developer", "content": "<image_resize_notice>Image 1 was resized to 800x600</image_resize_notice>"},
			{"role": "user", "content": "Turn 2 user"}
		]
	}`

	result := ConvertOpenAIRequestToGemini("gemini-3-flash", []byte(inputJSON), false)
	output := gjson.ParseBytes(result)

	// systemInstruction must contain only original system prompt
	sysParts := output.Get("systemInstruction.parts").Array()
	if len(sysParts) != 1 {
		t.Fatalf("systemInstruction parts = %d, want 1. Output: %s", len(sysParts), result)
	}
	if got := sysParts[0].Get("text").String(); got != "You are a helpful assistant" {
		t.Fatalf("systemInstruction text = %q, want %q", got, "You are a helpful assistant")
	}

	// contents must contain user, model, user (demoted dev message), user
	contents := output.Get("contents").Array()
	if len(contents) != 4 {
		t.Fatalf("contents length = %d, want 4. Output: %s", len(contents), result)
	}
	if contents[0].Get("role").String() != "user" || contents[0].Get("parts.0.text").String() != "Turn 1 user" {
		t.Fatalf("turn 0 mismatch: %s", contents[0].Raw)
	}
	if contents[1].Get("role").String() != "model" || contents[1].Get("parts.0.text").String() != "Turn 1 assistant" {
		t.Fatalf("turn 1 mismatch: %s", contents[1].Raw)
	}
	// Demoted developer text is wrapped in the system-reminder envelope
	// (upstream b681a1e0f7b8).
	expectedDevText := "<system-reminder>\n<image_resize_notice>Image 1 was resized to 800x600</image_resize_notice>\n</system-reminder>"
	if contents[2].Get("role").String() != "user" || contents[2].Get("parts.0.text").String() != expectedDevText {
		t.Fatalf("turn 2 mismatch: %s", contents[2].Raw)
	}
	if contents[3].Get("role").String() != "user" || contents[3].Get("parts.0.text").String() != "Turn 2 user" {
		t.Fatalf("turn 3 mismatch: %s", contents[3].Raw)
	}
}

// Ported from upstream b681a1e0f7b8 (system-reminder envelope for demoted
// mid-session system/developer messages).
func TestConvertOpenAIRequestToGemini_MidSessionSystemReminderEnvelope(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant"},
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi there"},
			{"role": "system", "content": "Please decide which tool to call next."},
			{"role": "user", "content": "Search for news"}
		]
	}`

	result := ConvertOpenAIRequestToGemini("gemini-3-flash", []byte(inputJSON), false)
	output := gjson.ParseBytes(result)

	contents := output.Get("contents").Array()
	if len(contents) != 4 {
		t.Fatalf("contents length = %d, want 4. Output: %s", len(contents), result)
	}
	expectedReminder := "<system-reminder>\nPlease decide which tool to call next.\n</system-reminder>"
	if got := contents[2].Get("parts.0.text").String(); got != expectedReminder {
		t.Fatalf("mid-session system reminder mismatch:\ngot:  %q\nwant: %q", got, expectedReminder)
	}
}

func TestConvertOpenAIRequestToGemini_MidSessionTransientSystemInstructionPreservesTurnBoundaries(t *testing.T) {
	turnWithTransient := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "system", "content": "System prompt"},
			{"role": "user", "content": "Turn 1 user"},
			{"role": "assistant", "content": "Turn 1 assistant"},
			{"role": "system", "content": "Call tool now"},
			{"role": "user", "content": "Turn 2 user"}
		]
	}`

	turnWithoutTransient := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "system", "content": "System prompt"},
			{"role": "user", "content": "Turn 1 user"},
			{"role": "assistant", "content": "Turn 1 assistant"},
			{"role": "user", "content": "Turn 2 user"},
			{"role": "assistant", "content": "Turn 2 assistant"}
		]
	}`

	outWith := ConvertOpenAIRequestToGemini("gemini-3-flash", []byte(turnWithTransient), false)
	outWithout := ConvertOpenAIRequestToGemini("gemini-3-flash", []byte(turnWithoutTransient), false)

	contentsWith := gjson.GetBytes(outWith, "contents").Array()
	contentsWithout := gjson.GetBytes(outWithout, "contents").Array()

	// Ensure demoted system instruction is standalone and not merged into adjacent user turn
	if len(contentsWith) != 4 {
		t.Fatalf("expected 4 standalone content items in request with transient instruction, got %d", len(contentsWith))
	}
	expectedReminder := "<system-reminder>\nCall tool now\n</system-reminder>"
	if contentsWith[2].Get("role").String() != "user" || contentsWith[2].Get("parts.0.text").String() != expectedReminder {
		t.Fatalf("turn 2 mismatch: %s", contentsWith[2].Raw)
	}
	if contentsWith[3].Get("role").String() != "user" || contentsWith[3].Get("parts.0.text").String() != "Turn 2 user" {
		t.Fatalf("turn 3 mismatch: %s", contentsWith[3].Raw)
	}

	// Prior turn history entries (Turn 1 user, Turn 1 assistant) are byte-identical
	if contentsWith[0].Raw != contentsWithout[0].Raw {
		t.Fatalf("turn 0 diverged: %s vs %s", contentsWith[0].Raw, contentsWithout[0].Raw)
	}
	if contentsWith[1].Raw != contentsWithout[1].Raw {
		t.Fatalf("turn 1 diverged: %s vs %s", contentsWith[1].Raw, contentsWithout[1].Raw)
	}
	// Turn 2 user text is also identical between turns because it was not merged
	if contentsWith[3].Get("parts.0.text").String() != contentsWithout[2].Get("parts.0.text").String() {
		t.Fatalf("turn 2 user text diverged due to merging: %s vs %s", contentsWith[3].Raw, contentsWithout[2].Raw)
	}
}

func TestConvertOpenAIRequestToGemini_MidSessionSystemReminderObjectAndArrayContent(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi"},
			{"role": "system", "content": {"type": "text", "text": "Object instruction"}},
			{"role": "developer", "content": [{"type": "text", "text": "Array instruction"}]}
		]
	}`

	result := ConvertOpenAIRequestToGemini("gemini-3-flash", []byte(inputJSON), false)
	output := gjson.ParseBytes(result)

	contents := output.Get("contents").Array()
	if len(contents) != 4 {
		t.Fatalf("contents length = %d, want 4. Output: %s", len(contents), result)
	}
	expectedObject := "<system-reminder>\nObject instruction\n</system-reminder>"
	if got := contents[2].Get("parts.0.text").String(); got != expectedObject {
		t.Fatalf("object instruction mismatch:\ngot:  %q\nwant: %q", got, expectedObject)
	}
	expectedArray := "<system-reminder>\nArray instruction\n</system-reminder>"
	if got := contents[3].Get("parts.0.text").String(); got != expectedArray {
		t.Fatalf("array instruction mismatch:\ngot:  %q\nwant: %q", got, expectedArray)
	}
}

// Ported from upstream CLIProxyAPI commit c2ea2684 (scope tool responses per
// turn to handle repeated tool call IDs, issue #5933).
func TestConvertOpenAIRequestToGemini_MultiTurnRepeatedToolCallID_Issue5933(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "list files"},
			{
				"role": "assistant",
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "glob", "arguments": "{\"pattern\":\"*.go\"}"}
				}]
			},
			{"role": "tool", "tool_call_id": "call_1", "content": "[\"main.go\"]"},
			{"role": "user", "content": "read main.go"},
			{
				"role": "assistant",
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "read", "arguments": "{\"path\":\"main.go\"}"}
				}]
			},
			{"role": "tool", "tool_call_id": "call_1", "content": "package main"}
		]
	}`

	out := ConvertOpenAIRequestToGemini("gemini-3-flash", []byte(inputJSON), false)

	// In Turn 1 (contents[1] = model functionCall, contents[2] = user functionResponse):
	// functionCall.name must be "glob", and functionResponse.name must be "glob".
	call1Name := gjson.GetBytes(out, "contents.1.parts.0.functionCall.name").String()
	resp1Name := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.name").String()
	resp1Result := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.response.result").String()

	if call1Name != "glob" {
		t.Fatalf("turn 1 functionCall.name = %q, want glob", call1Name)
	}
	if resp1Name != "glob" {
		t.Fatalf("turn 1 functionResponse.name = %q, want glob (got overwritten by subsequent turn)", resp1Name)
	}
	if resp1Result != `"[\"main.go\"]"` {
		t.Fatalf("turn 1 functionResponse result = %q, want %q", resp1Result, `"[\"main.go\"]"`)
	}

	// In Turn 2 (contents[4] = model functionCall, contents[5] = user functionResponse):
	// functionCall.name must be "read", and functionResponse.name must be "read".
	call2Name := gjson.GetBytes(out, "contents.4.parts.0.functionCall.name").String()
	resp2Name := gjson.GetBytes(out, "contents.5.parts.0.functionResponse.name").String()
	resp2Result := gjson.GetBytes(out, "contents.5.parts.0.functionResponse.response.result").String()

	if call2Name != "read" {
		t.Fatalf("turn 2 functionCall.name = %q, want read", call2Name)
	}
	if resp2Name != "read" {
		t.Fatalf("turn 2 functionResponse.name = %q, want read", resp2Name)
	}
	if resp2Result != `"package main"` {
		t.Fatalf("turn 2 functionResponse result = %q, want %q", resp2Result, `"package main"`)
	}

	// Verify pairing validator passes without error
	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(out); errPairing != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed on Gemini output: %v; output=%s", errPairing, out)
	}
}

// Ported from upstream CLIProxyAPI commit c2ea2684.
func TestConvertOpenAIRequestToGemini_ParallelAndOutOfOrderToolResponses(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "run parallel tools"},
			{
				"role": "assistant",
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "tool_a", "arguments": "{}"}},
					{"id": "call_2", "type": "function", "function": {"name": "tool_b", "arguments": "{}"}}
				]
			},
			{"role": "tool", "tool_call_id": "call_2", "content": "res_b"},
			{"role": "tool", "tool_call_id": "call_1", "content": "res_a"}
		]
	}`

	out := ConvertOpenAIRequestToGemini("gemini-3-flash", []byte(inputJSON), false)

	resp0Name := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.name").String()
	resp0Result := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.response.result").String()
	resp1Name := gjson.GetBytes(out, "contents.2.parts.1.functionResponse.name").String()
	resp1Result := gjson.GetBytes(out, "contents.2.parts.1.functionResponse.response.result").String()

	if resp0Name != "tool_a" || resp0Result != `"res_a"` {
		t.Fatalf("part 0 want tool_a / %q, got %s / %s", `"res_a"`, resp0Name, resp0Result)
	}
	if resp1Name != "tool_b" || resp1Result != `"res_b"` {
		t.Fatalf("part 1 want tool_b / %q, got %s / %s", `"res_b"`, resp1Name, resp1Result)
	}

	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(out); errPairing != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed: %v; output=%s", errPairing, out)
	}
}

// Ported from upstream CLIProxyAPI commit b532db9c (issue #5959).
func TestConvertOpenAIRequestToGemini_ParametersJsonSchema_PreservesAdditionalPropertiesAndPattern_Issue5959(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "Use the submit tool."}],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "submit",
					"description": "Submit a bounded schema test value.",
					"parameters": {
						"$schema": "https://json-schema.org/draft/2020-12/schema",
						"type": "object",
						"additionalProperties": false,
						"properties": {
							"recipient": {
								"type": "string",
								"pattern": "^(alice|bob)$"
							},
							"amount": {
								"type": "number"
							}
						},
						"required": ["recipient", "amount"]
					}
				}
			}
		]
	}`)

	output := ConvertOpenAIRequestToGemini("gemini-2.5-flash", inputJSON, false)
	schema := gjson.GetBytes(output, "tools.0.functionDeclarations.0.parametersJsonSchema")
	if !schema.Exists() {
		t.Fatalf("parametersJsonSchema missing. Output: %s", output)
	}
	if got := schema.Get("additionalProperties"); !got.Exists() || got.Type != gjson.False {
		t.Fatalf("additionalProperties should be preserved as false, got: %v. Schema: %s", got, schema.Raw)
	}
	if got := schema.Get("properties.recipient.pattern"); !got.Exists() || got.String() != "^(alice|bob)$" {
		t.Fatalf("pattern should be preserved, got: %v. Schema: %s", got, schema.Raw)
	}
	if schema.Get("description").Exists() && schema.Get("description").String() == "No extra properties allowed" {
		t.Fatalf("additionalProperties: false should not be converted to description hint. Schema: %s", schema.Raw)
	}
	if got := schema.Get("properties.recipient.description"); got.Exists() && strings.Contains(got.String(), "pattern:") {
		t.Fatalf("pattern should not be converted to description hint. Schema: %s", schema.Raw)
	}
}

// Ported from upstream CLIProxyAPI commit f247e2b0 (strict tool mode, issue #5958).
func TestConvertOpenAIRequestToGemini_ToolStrictMapsToValidatedMode(t *testing.T) {
	tests := []struct {
		name         string
		toolChoice   string // empty means absent
		expectedMode string
		allowedNames []string
	}{
		{
			name:         "absent tool_choice maps to VALIDATED",
			toolChoice:   "",
			expectedMode: "VALIDATED",
		},
		{
			name:         "explicit auto tool_choice maps to VALIDATED",
			toolChoice:   `"tool_choice": "auto",`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "null tool_choice maps to VALIDATED",
			toolChoice:   `"tool_choice": null,`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "required tool_choice maps to ANY",
			toolChoice:   `"tool_choice": "required",`,
			expectedMode: "ANY",
		},
		{
			name:         "none tool_choice maps to NONE",
			toolChoice:   `"tool_choice": "none",`,
			expectedMode: "NONE",
		},
		{
			name:         "specific function tool_choice maps to ANY with allowedFunctionNames",
			toolChoice:   `"tool_choice": {"type": "function", "function": {"name": "tool_a"}},`,
			expectedMode: "ANY",
			allowedNames: []string{"tool_a"},
		},
		{
			name:         "allowed_tools with auto mode and strict tool maps to VALIDATED",
			toolChoice:   `"tool_choice": {"type": "allowed_tools", "allowed_tools": {"mode": "auto", "tools": [{"type": "function", "function": {"name": "tool_a"}}]}},`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "allowed_tools with required mode and strict tool maps to ANY",
			toolChoice:   `"tool_choice": {"type": "allowed_tools", "allowed_tools": {"mode": "required", "tools": [{"type": "function", "function": {"name": "tool_a"}}]}},`,
			expectedMode: "ANY",
			allowedNames: []string{"tool_a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := fmt.Sprintf(`{
				"model": "gemini-3.1-pro-high",
				"messages": [{"role": "user", "content": "hi"}],
				%s
				"tools": [
					{
						"type": "function",
						"function": {
							"name": "tool_a",
							"description": "Controlled tool.",
							"strict": true,
							"parameters": {"type": "object", "properties": {}}
						}
					}
				]
			}`, tt.toolChoice)
			result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
			if gjson.GetBytes(result, "tools.0.functionDeclarations.0.strict").Exists() {
				t.Fatalf("tools.0.functionDeclarations.0.strict should be removed from function declaration: %s", result)
			}
			mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
			if mode != tt.expectedMode {
				t.Fatalf("expected toolConfig.functionCallingConfig.mode = %q, got %q. Output: %s", tt.expectedMode, mode, result)
			}
			if len(tt.allowedNames) > 0 {
				allowed := gjson.GetBytes(result, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
				if len(allowed) != len(tt.allowedNames) {
					t.Fatalf("expected %d allowedFunctionNames, got %d", len(tt.allowedNames), len(allowed))
				}
				for i, name := range tt.allowedNames {
					if allowed[i].String() != name {
						t.Fatalf("allowedFunctionNames[%d] = %q, want %q", i, allowed[i].String(), name)
					}
				}
			}
		})
	}

	t.Run("mixed tools where one is strict maps to VALIDATED", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_a",
						"description": "Loose tool.",
						"strict": false,
						"parameters": {"type": "object", "properties": {}}
					}
				},
				{
					"type": "function",
					"function": {
						"name": "tool_b",
						"description": "Strict tool.",
						"strict": true,
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "VALIDATED" {
			t.Fatalf("expected mode = 'VALIDATED' for mixed tools, got %q", mode)
		}
	})

	t.Run("non-strict tools omit toolConfig mode", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_a",
						"description": "Loose tool.",
						"strict": false,
						"parameters": {"type": "object", "properties": {}}
					}
				},
				{
					"type": "function",
					"function": {
						"name": "tool_b",
						"description": "Unspecified tool.",
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		if gjson.GetBytes(result, "toolConfig").Exists() {
			t.Fatalf("expected toolConfig not to be set when no strict tools and no tool_choice, got: %s", result)
		}
	})
}

// Ported from upstream CLIProxyAPI commit 49eec664 (tool choice fail-closed, issue #5957).
func TestConvertOpenAIRequestToGemini_ToolChoice(t *testing.T) {
	t.Run("named function maps to toolConfig mode ANY and allowedFunctionNames", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "Call tool_a."}],
			"tool_choice": {"type": "function", "function": {"name": "tool_a"}},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		allowed := gjson.GetBytes(result, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
		if mode != "ANY" {
			t.Fatalf("expected toolConfig.functionCallingConfig.mode = 'ANY', got %q. Output: %s", mode, result)
		}
		if len(allowed) != 1 || allowed[0].String() != "tool_a" {
			t.Fatalf("expected allowedFunctionNames = ['tool_a'], got %v. Output: %s", allowed, result)
		}
	})

	t.Run("none maps to mode NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "none",
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "NONE" {
			t.Fatalf("expected toolConfig.functionCallingConfig.mode = 'NONE', got %q. Output: %s", mode, result)
		}
	})

	t.Run("auto maps to mode AUTO", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "auto",
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "AUTO" {
			t.Fatalf("expected toolConfig.functionCallingConfig.mode = 'AUTO', got %q. Output: %s", mode, result)
		}
	})

	t.Run("required maps to mode ANY", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "required",
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "ANY" {
			t.Fatalf("expected toolConfig.functionCallingConfig.mode = 'ANY', got %q. Output: %s", mode, result)
		}
	})

	t.Run("parallel_tool_calls false fails closed to NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "auto",
			"parallel_tool_calls": false,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "NONE" {
			t.Fatalf("expected toolConfig.functionCallingConfig.mode = 'NONE', got %q. Output: %s", mode, result)
		}
	})

	t.Run("required tool_choice with parallel_tool_calls false fails closed to NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "required",
			"parallel_tool_calls": false,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "NONE" {
			t.Fatalf("expected toolConfig.functionCallingConfig.mode = 'NONE', got %q. Output: %s", mode, result)
		}
	})

	t.Run("parallel_tool_calls null does not fail closed to NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "auto",
			"parallel_tool_calls": null,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "AUTO" {
			t.Fatalf("expected toolConfig.functionCallingConfig.mode = 'AUTO', got %q. Output: %s", mode, result)
		}
	})

	t.Run("parallel_tool_calls true does not fail closed to NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "auto",
			"parallel_tool_calls": true,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "AUTO" {
			t.Fatalf("expected toolConfig.functionCallingConfig.mode = 'AUTO', got %q. Output: %s", mode, result)
		}
	})

	t.Run("allowed_tools filters function declarations and sets AUTO mode without allowedFunctionNames", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {
				"type": "allowed_tools",
				"allowed_tools": {
					"mode": "auto",
					"tools": [{"type": "function", "function": {"name": "tool_b"}}]
				}
			},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}},
				{"type": "function", "function": {"name": "tool_b", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		allowed := gjson.GetBytes(result, "toolConfig.functionCallingConfig.allowedFunctionNames")
		if mode != "AUTO" {
			t.Fatalf("expected mode = 'AUTO', got %q. Output: %s", mode, result)
		}
		if allowed.Exists() {
			t.Fatalf("expected allowedFunctionNames to not be set for AUTO mode, got %v", allowed.Value())
		}
		decls := gjson.GetBytes(result, "tools.0.functionDeclarations").Array()
		if len(decls) != 1 || decls[0].Get("name").String() != "tool_b" {
			t.Fatalf("expected functionDeclarations to contain only tool_b, got %v", decls)
		}
	})

	t.Run("allowed_tools with required mode sets mode ANY and allowedFunctionNames", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {
				"type": "allowed_tools",
				"allowed_tools": {
					"mode": "required",
					"tools": [{"type": "function", "function": {"name": "tool_b"}}]
				}
			},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}},
				{"type": "function", "function": {"name": "tool_b", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		allowed := gjson.GetBytes(result, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
		if mode != "ANY" {
			t.Fatalf("expected mode = 'ANY', got %q. Output: %s", mode, result)
		}
		if len(allowed) != 1 || allowed[0].String() != "tool_b" {
			t.Fatalf("expected allowedFunctionNames = ['tool_b'], got %v", allowed)
		}
	})

	t.Run("empty allowed_tools fails closed to NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {
				"type": "allowed_tools",
				"allowed_tools": {
					"tools": []
				}
			},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "NONE" {
			t.Fatalf("expected mode = 'NONE', got %q. Output: %s", mode, result)
		}
	})

	t.Run("function choice with missing name fails closed to NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {"type": "function", "function": {}},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "NONE" {
			t.Fatalf("expected mode = 'NONE', got %q. Output: %s", mode, result)
		}
	})

	t.Run("allowed_tools matches exact original name and does not conflate sanitization", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {
				"type": "allowed_tools",
				"allowed_tools": {
					"tools": [{"type": "function", "function": {"name": "1tool"}}]
				}
			},
			"tools": [
				{"type": "function", "function": {"name": "_1tool", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "NONE" {
			t.Fatalf("expected mode = 'NONE' when exact original name not found, got %q. Output: %s", mode, result)
		}
	})

	t.Run("sanitized name collision fails closed to NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "auto",
			"tools": [
				{"type": "function", "function": {"name": "1tool", "parameters": {"type": "object", "properties": {}}}},
				{"type": "function", "function": {"name": "_1tool", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "NONE" {
			t.Fatalf("expected mode = 'NONE' on name collision, got %q. Output: %s", mode, result)
		}
	})

	t.Run("undeclared function choice fails closed to NONE", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {"type": "function", "function": {"name": "undeclared_tool"}},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "NONE" {
			t.Fatalf("expected mode = 'NONE' for undeclared function, got %q. Output: %s", mode, result)
		}
	})

	t.Run("allowed_tools filtering avoids false collision when excluded tool collides", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {
				"type": "allowed_tools",
				"allowed_tools": {
					"mode": "auto",
					"tools": [{"type": "function", "function": {"name": "_1tool"}}]
				}
			},
			"tools": [
				{"type": "function", "function": {"name": "1tool", "parameters": {"type": "object", "properties": {}}}},
				{"type": "function", "function": {"name": "_1tool", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "AUTO" {
			t.Fatalf("expected mode = 'AUTO', got %q. Output: %s", mode, result)
		}
		decls := gjson.GetBytes(result, "tools.0.functionDeclarations").Array()
		if len(decls) != 1 || decls[0].Get("name").String() != "_1tool" {
			t.Fatalf("expected only _1tool to remain in functionDeclarations, got: %v", decls)
		}
	})

	t.Run("tool_choice null does not create toolConfig or fail closed", func(t *testing.T) {
		inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": null,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
		if gjson.GetBytes(result, "toolConfig").Exists() {
			t.Fatalf("expected toolConfig not to be set when tool_choice is null, got: %s", result)
		}
	})
}

// A request that mixes a built-in tool with function declarations must declare
// server-side tool invocations, because Gemini rejects the combination otherwise
// (upstream 4550161). A request carrying only one of the two shapes must keep
// producing exactly the bytes this translator produced before that flag existed.
func TestConvertOpenAIRequestToGemini_MixedBuiltinToolsAndFunctions(t *testing.T) {
	const model = "gemini-3.8-flash"
	const fnTool = `{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}`

	mixed := []struct{ name, output string }{
		{"google_search", "googleSearch"},
		{"code_execution", "codeExecution"},
		{"url_context", "urlContext"},
	}
	for _, builtin := range mixed {
		t.Run(builtin.name, func(t *testing.T) {
			req := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"tools":[` + fnTool + `,{"` + builtin.name + `":{}}],"tool_choice":"auto"}`
			out := ConvertOpenAIRequestToGemini(model, []byte(req), false)
			if !gjson.GetBytes(out, "toolConfig.includeServerSideToolInvocations").Bool() {
				t.Fatalf("mixed %s + function tools must declare server-side tool invocations, got: %s", builtin.name, out)
			}
			if mode := gjson.GetBytes(out, "toolConfig.functionCallingConfig.mode").String(); mode != "AUTO" {
				t.Fatalf("toolConfig.functionCallingConfig.mode = %q, want AUTO, got: %s", mode, out)
			}
			if name := gjson.GetBytes(out, "tools.0.functionDeclarations.0.name").String(); name != "get_weather" {
				t.Fatalf("tools.0.functionDeclarations.0.name = %q, want get_weather, got: %s", name, out)
			}
			if !gjson.GetBytes(out, "tools.1."+builtin.output).Exists() {
				t.Fatalf("expected built-in tool %s alongside functionDeclarations, got: %s", builtin.output, out)
			}
		})
	}

	unchanged := []struct{ name, req, wantDigest string }{
		{
			"functions-only with tool_choice",
			`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"tools":[` + fnTool + `],"tool_choice":"auto"}`,
			"4645045386436fcfbb1cbe7123c69e8fcd38c31f8d86c4bb530d232d10729537",
		},
		{
			"builtin-only with tool_choice",
			`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"tools":[{"google_search":{}}],"tool_choice":"auto"}`,
			"e12c048aadd2706bb9c79b326e149d0ae4da4767f5d9b381c2d65f3a4f0becc7",
		},
		{
			"functions-only",
			`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"tools":[` + fnTool + `]}`,
			"5accb8dcecf65a9d760bfe9c52f236048528539f573f6b649d3e4362c7ed7d57",
		},
		{
			"builtin-only",
			`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"tools":[{"google_search":{}}]}`,
			"3fdae5750d86a5981ea241e090b122cd047ac85a920c4dc7a4639b5c8fe59cae",
		},
	}
	for _, c := range unchanged {
		t.Run("unchanged/"+c.name, func(t *testing.T) {
			out := ConvertOpenAIRequestToGemini(model, []byte(c.req), false)
			if got := fmt.Sprintf("%x", sha256.Sum256(out)); got != c.wantDigest {
				t.Fatalf("%s output must stay byte-identical: digest = %s, want %s; output = %s", c.name, got, c.wantDigest, out)
			}
		})
	}
}
