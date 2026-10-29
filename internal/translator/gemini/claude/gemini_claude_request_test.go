package claude

import (
	"fmt"
	"strings"
	"testing"

	signature "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToGemini_ToolChoice_SpecificTool(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "hi"}
				]
			}
		],
		"tools": [
			{
				"name": "json",
				"description": "A JSON tool",
				"input_schema": {
					"type": "object",
					"properties": {}
				}
			}
		],
		"tool_choice": {"type": "tool", "name": "json"}
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	if got := gjson.GetBytes(output, "toolConfig.functionCallingConfig.mode").String(); got != "ANY" {
		t.Fatalf("Expected toolConfig.functionCallingConfig.mode 'ANY', got '%s'", got)
	}
	allowed := gjson.GetBytes(output, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
	if len(allowed) != 1 || allowed[0].String() != "json" {
		t.Fatalf("Expected allowedFunctionNames ['json'], got %s", gjson.GetBytes(output, "toolConfig.functionCallingConfig.allowedFunctionNames").Raw)
	}
}

func TestConvertClaudeRequestToGemini_StringSystemInstruction(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"system": "Be concise",
		"messages": [{"role": "user", "content": "Hello"}]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	if got := gjson.GetBytes(output, "systemInstruction.parts.0.text").String(); got != "Be concise" {
		t.Fatalf("Expected systemInstruction text %q, got %q", "Be concise", got)
	}
	if gjson.GetBytes(output, "systemInstruction.role").Exists() {
		t.Fatalf("Expected systemInstruction.role to not exist, got %q", gjson.GetBytes(output, "systemInstruction.role").String())
	}
	if gjson.GetBytes(output, "system_instruction").Exists() {
		t.Fatalf("Legacy system_instruction field should not be emitted: %s", output)
	}
}

func TestConvertClaudeRequestToGemini_ImageContent(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "describe this image"},
					{
						"type": "image",
						"source": {
							"type": "base64",
							"media_type": "image/png",
							"data": "aGVsbG8="
						}
					}
				]
			}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("Expected 2 parts, got %d", len(parts))
	}
	if got := parts[0].Get("text").String(); got != "describe this image" {
		t.Fatalf("Expected first part text 'describe this image', got '%s'", got)
	}
	if got := parts[1].Get("inline_data.mime_type").String(); got != "image/png" {
		t.Fatalf("Expected image mime type 'image/png', got '%s'", got)
	}
	if got := parts[1].Get("inline_data.data").String(); got != "aGVsbG8=" {
		t.Fatalf("Expected image data 'aGVsbG8=', got '%s'", got)
	}
}

func TestConvertClaudeRequestToGemini_StripsClaudeCodeAttribution(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"},
			{"type": "text", "text": "You are a Claude agent, built on Anthropic's Claude Agent SDK."},
			{"type": "text", "text": "User system prompt"}
		],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	parts := gjson.GetBytes(output, "systemInstruction.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("Expected 2 system parts after attribution strip, got %d: %s", len(parts), gjson.GetBytes(output, "systemInstruction.parts").Raw)
	}
	if got := parts[0].Get("text").String(); got != "You are a Claude agent, built on Anthropic's Claude Agent SDK." {
		t.Fatalf("Unexpected first system part: %q", got)
	}
	if got := parts[1].Get("text").String(); got != "User system prompt" {
		t.Fatalf("Unexpected second system part: %q", got)
	}
	if gjson.GetBytes(output, `systemInstruction.parts.#(text%"x-anthropic-billing-header:*")`).Exists() {
		t.Fatalf("Claude Code attribution block was forwarded: %s", gjson.GetBytes(output, "systemInstruction.parts").Raw)
	}
}

func TestConvertClaudeRequestToGemini_ConvertsMessageSystemRoleToUserContent(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"system": [{"type": "text", "text": "Top-level rules"}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}]},
			{"role": "system", "content": "String mid-conversation rule"},
			{"role": "system", "content": [{"type": "text", "text": "Array mid-conversation rule"}]}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	if systemContent := gjson.GetBytes(output, `contents.#(role=="system")`); systemContent.Exists() {
		t.Fatalf("system role should not be emitted in contents: %s", systemContent.Raw)
	}

	// Ported from upstream CLIProxyAPI commit 0fe19ede90a4: consecutive user and
	// message-level system turns merge into a single user turn.
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 {
		t.Fatalf("Expected consecutive user and message-level system turns to be merged into a single user turn, got %d: %s", len(contents), gjson.GetBytes(output, "contents").Raw)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected first content role user, got %q", got)
	}
	mergedParts := contents[0].Get("parts").Array()
	if len(mergedParts) != 3 {
		t.Fatalf("Expected 3 parts in merged user content, got %d: %s", len(mergedParts), contents[0].Get("parts").Raw)
	}
	if got := mergedParts[0].Get("text").String(); got != "Hello" {
		t.Fatalf("Unexpected initial user prompt text: %q", got)
	}
	if got := mergedParts[1].Get("text").String(); got != "<system-reminder>\nString mid-conversation rule\n</system-reminder>" {
		t.Fatalf("Unexpected string message-level system content text: %q", got)
	}
	if got := mergedParts[2].Get("text").String(); got != "<system-reminder>\nArray mid-conversation rule\n</system-reminder>" {
		t.Fatalf("Unexpected array message-level system content text: %q", got)
	}

	parts := gjson.GetBytes(output, "systemInstruction.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected only top-level system parts, got %d: %s", len(parts), gjson.GetBytes(output, "systemInstruction.parts").Raw)
	}
	if got := parts[0].Get("text").String(); got != "Top-level rules" {
		t.Fatalf("Unexpected first system part: %q", got)
	}
}

func TestConvertClaudeRequestToGemini_SkipsEmptyTextParts(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": ""},
					{"type": "text", "text": "hello"},
					{"type": "text", "text": ""}
				]
			}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part after skipping empty text, got %d: %s", len(parts), output)
	}
	if got := parts[0].Get("text").String(); got != "hello" {
		t.Fatalf("Expected part text 'hello', got '%s'", got)
	}
}

func TestConvertClaudeRequestToGemini_StructuredToolResult(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "json-call-1", "name": "json", "input": {"ok": true}}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "json-call-1",
						"content": [
							{"type": "text", "text": "alpha"},
							{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}
						]
					}
				]
			}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	fr := gjson.GetBytes(output, "contents.1.parts.0.functionResponse")
	if !fr.Exists() {
		t.Fatalf("expected functionResponse part, contents=%s", gjson.GetBytes(output, "contents").Raw)
	}
	if got := fr.Get("response.result.text").String(); got != "alpha" {
		t.Fatalf("expected structured result text 'alpha', got result=%s", fr.Get("response.result").Raw)
	}
	img := gjson.GetBytes(output, "contents.1.parts.1.inline_data")
	if got := img.Get("mime_type").String(); got != "image/png" {
		t.Fatalf("expected image mime type 'image/png', got '%s'", got)
	}
	if got := img.Get("data").String(); got != "aGVsbG8=" {
		t.Fatalf("expected image data 'aGVsbG8=', got '%s'", got)
	}
}

func TestConvertClaudeRequestToGemini_AlignsPermutedParallelToolResultsWithMixedText(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gemini-3.7-flash-high",
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"/tmp/1"}},
				{"type":"tool_use","id":"call_2","name":"Read","input":{"file_path":"/tmp/2"}},
				{"type":"tool_use","id":"call_3","name":"Read","input":{"file_path":"/tmp/3"}}
			]},
			{"role":"user","content":[
				{"type":"text","text":"Results arrived."},
				{"type":"tool_result","tool_use_id":"call_3","content":"three"},
				{"type":"tool_result","tool_use_id":"call_1","content":"one"},
				{"type":"tool_result","tool_use_id":"call_2","content":"two"},
				{"type":"text","text":"Continue."}
			]}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3.7-flash-high", inputJSON, false)
	callParts := gjson.GetBytes(output, "contents.0.parts").Array()
	responseParts := gjson.GetBytes(output, "contents.1.parts").Array()
	if len(callParts) != 3 || len(responseParts) != 5 {
		t.Fatalf("translated parts = %d calls and %d response-turn parts; output=%s", len(callParts), len(responseParts), output)
	}
	for index, wantID := range []string{"call_1", "call_2", "call_3"} {
		if gotID := callParts[index].Get("functionCall.id").String(); gotID != wantID {
			t.Fatalf("functionCall[%d].id = %q, want %q; output=%s", index, gotID, wantID, output)
		}
	}
	// Ported from upstream CLIProxyAPI commit 4fde97f4144a: text parts reorder
	// before functionResponse parts inside the user turn.
	if got := responseParts[0].Get("text").String(); got != "Results arrived." {
		t.Fatalf("leading text = %q; output=%s", got, output)
	}
	if got := responseParts[1].Get("text").String(); got != "Continue." {
		t.Fatalf("trailing text reordered before functionResponse = %q; output=%s", got, output)
	}
	for index, wantID := range []string{"call_1", "call_2", "call_3"} {
		responsePart := responseParts[index+2]
		if gotID := responsePart.Get("functionResponse.id").String(); gotID != wantID {
			t.Fatalf("functionResponse[%d].id = %q, want %q; output=%s", index, gotID, wantID, output)
		}
		if gotName := responsePart.Get("functionResponse.name").String(); gotName != "Read" {
			t.Fatalf("functionResponse[%d].name = %q, want Read; output=%s", index, gotName, output)
		}
	}
	if errPairing := signature.ValidateGeminiFunctionCallPairing(output); errPairing != nil {
		t.Fatalf("translated parallel tool history is invalid: %v; output=%s", errPairing, output)
	}
}

func TestConvertClaudeRequestToGemini_StringToolResult(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "json-call-1", "name": "json", "input": {"ok": true}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "json-call-1", "content": "alpha"}
				]
			}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	fr := gjson.GetBytes(output, "contents.1.parts.0.functionResponse")
	if !fr.Exists() {
		t.Fatalf("expected functionResponse part, contents=%s", gjson.GetBytes(output, "contents").Raw)
	}
	if got := fr.Get("response.result").String(); got != "alpha" {
		t.Fatalf("expected result 'alpha', got '%s' (raw=%s)", got, fr.Get("response.result").Raw)
	}
}

const capturedGeminiThinkingSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"

func TestConvertClaudeRequestToGemini_SignatureCompatibility(t *testing.T) {
	tests := []struct {
		name          string
		signature     string
		wantSignature string
	}{
		{
			name:          "preserves valid gemini signature",
			signature:     "gemini#" + capturedGeminiThinkingSignature,
			wantSignature: capturedGeminiThinkingSignature,
		},
		{
			name:          "foreign claude signature maps to bypass sentinel",
			signature:     "claude#opaque-signature-12345",
			wantSignature: signature.GeminiSkipThoughtSignatureValidator,
		},
		{
			name:          "empty signature maps to bypass sentinel",
			signature:     "",
			wantSignature: signature.GeminiSkipThoughtSignatureValidator,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"` + tt.signature + `"}]}]}`)
			out := ConvertClaudeRequestToGeminiWithCompat("deepseek-v4", payload, false)
			part := gjson.GetBytes(out, "contents.0.parts.0")
			if !part.Get("thought").Bool() || part.Get("text").String() != "reason" {
				t.Fatalf("translation missing thought part: %s", out)
			}
			if got := part.Get("thoughtSignature").String(); got != tt.wantSignature {
				t.Fatalf("thoughtSignature = %q, want %q; output: %s", got, tt.wantSignature, out)
			}
		})
	}
}

// Ported from upstream CLIProxyAPI commit 0fe19ede90a4 ("preserve Gemini prompt
// cache by demoting mid-session developer messages"): message-level developer
// instructions demote to <system-reminder> user turns merged with neighbors.
func TestConvertClaudeRequestToGemini_MessageLevelDeveloperInstructionsBecomeMergedUserReminder(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"system": "Top-level rules",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}]},
			{"role": "developer", "content": "String mid-conversation developer rule"},
			{"role": "developer", "content": [{"type": "text", "text": "Array mid-conversation developer rule"}]}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	if devContent := gjson.GetBytes(output, `contents.#(role=="developer")`); devContent.Exists() {
		t.Fatalf("developer role should not be emitted in contents: %s", devContent.Raw)
	}

	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 {
		t.Fatalf("Expected consecutive user and developer turns to be merged into a single user turn, got %d: %s", len(contents), gjson.GetBytes(output, "contents").Raw)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected first content role user, got %q", got)
	}
	parts := contents[0].Get("parts").Array()
	if len(parts) != 3 {
		t.Fatalf("Expected 3 parts in merged user content, got %d: %s", len(parts), contents[0].Get("parts").Raw)
	}
	if got := parts[0].Get("text").String(); got != "Hello" {
		t.Fatalf("Unexpected initial user prompt text: %q", got)
	}
	if got := parts[1].Get("text").String(); got != "<system-reminder>\nString mid-conversation developer rule\n</system-reminder>" {
		t.Fatalf("Unexpected string developer content text: %q", got)
	}
	if got := parts[2].Get("text").String(); got != "<system-reminder>\nArray mid-conversation developer rule\n</system-reminder>" {
		t.Fatalf("Unexpected array developer content text: %q", got)
	}

	systemInstructionParts := gjson.GetBytes(output, "systemInstruction.parts").Array()
	if len(systemInstructionParts) != 1 {
		t.Fatalf("Expected only top-level system parts, got %d: %s", len(systemInstructionParts), gjson.GetBytes(output, "systemInstruction.parts").Raw)
	}
	if got := systemInstructionParts[0].Get("text").String(); got != "Top-level rules" {
		t.Fatalf("Unexpected first system part: %q", got)
	}
}

// Ported from upstream CLIProxyAPI commit b532db9c (issue #5959).
func TestConvertClaudeRequestToGemini_ParametersJsonSchema_PreservesAdditionalPropertiesAndPattern_Issue5959(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "Use the submit tool."}],
		"tools": [
			{
				"name": "submit",
				"description": "Submit a bounded schema test value.",
				"input_schema": {
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
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-2.5-flash", inputJSON, false)
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
func TestConvertClaudeRequestToGemini_ToolStrictMapsToValidatedMode(t *testing.T) {
	tests := []struct {
		name         string
		toolChoice   string
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
			toolChoice:   `"tool_choice": {"type": "auto"},`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "null tool_choice maps to VALIDATED",
			toolChoice:   `"tool_choice": null,`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "none tool_choice maps to NONE",
			toolChoice:   `"tool_choice": {"type": "none"},`,
			expectedMode: "NONE",
		},
		{
			name:         "any tool_choice maps to ANY",
			toolChoice:   `"tool_choice": {"type": "any"},`,
			expectedMode: "ANY",
		},
		{
			name:         "specific tool maps to ANY with allowedFunctionNames",
			toolChoice:   `"tool_choice": {"type": "tool", "name": "tool_a"},`,
			expectedMode: "ANY",
			allowedNames: []string{"tool_a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := fmt.Sprintf(`{
				"model": "gemini-3.8-flash",
				"messages": [{"role": "user", "content": "hi"}],
				%s
				"tools": [
					{
						"name": "tool_a",
						"description": "Controlled tool.",
						"strict": true,
						"input_schema": {"type": "object", "properties": {}}
					}
				]
			}`, tt.toolChoice)

			result := ConvertClaudeRequestToGemini("gemini-3.8-flash", []byte(inputJSON), false)
			if gjson.GetBytes(result, "tools.0.functionDeclarations.0.strict").Exists() {
				t.Fatalf("strict must be removed from functionDeclarations: %s", result)
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
		inputJSON := []byte(`{
			"model": "gemini-3.8-flash",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"name": "tool_a",
					"description": "Loose tool.",
					"strict": false,
					"input_schema": {"type": "object", "properties": {}}
				},
				{
					"name": "tool_b",
					"description": "Strict tool.",
					"strict": true,
					"input_schema": {"type": "object", "properties": {}}
				}
			]
		}`)
		result := ConvertClaudeRequestToGemini("gemini-3.8-flash", inputJSON, false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "VALIDATED" {
			t.Fatalf("expected mode = 'VALIDATED' for mixed tools, got %q", mode)
		}
	})

	t.Run("non-strict tools omit toolConfig mode", func(t *testing.T) {
		inputJSON := []byte(`{
			"model": "gemini-3.8-flash",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"name": "tool_a",
					"description": "Loose tool.",
					"strict": false,
					"input_schema": {"type": "object", "properties": {}}
				},
				{
					"name": "tool_b",
					"description": "Unspecified tool.",
					"input_schema": {"type": "object", "properties": {}}
				}
			]
		}`)
		result := ConvertClaudeRequestToGemini("gemini-3.8-flash", inputJSON, false)
		if gjson.GetBytes(result, "toolConfig").Exists() {
			t.Fatalf("expected toolConfig not to be set when no strict tools and no tool_choice, got: %s", result)
		}
	})
}
func TestConvertClaudeRequestToGemini_FunctionResponseJSONRef(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3.8-flash",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "toolu_schema_1", "name": "get_schema", "input": {}}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "toolu_schema_1",
						"content": {
							"schema": {
								"$ref": "#/components/schemas/ErrorModel"
							}
						}
					}
				]
			}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3.8-flash", inputJSON, false)
	result := gjson.GetBytes(output, "contents.1.parts.0.functionResponse.response.result")
	if result.Type != gjson.String {
		t.Fatalf("expected functionResponse.response.result to be string, got %s (raw: %s)", result.Type, result.Raw)
	}
	if !strings.Contains(result.String(), "#/components/schemas/ErrorModel") {
		t.Fatalf("expected result to contain ref target, got %q", result.String())
	}
}

// The internal Codex commentary marker is a forgeable literal a client can send
// in its own thinking block. It must never become a thought signature on the way
// to Gemini: the block is restored as plain assistant text.
func TestConvertClaudeRequestToGemini_ClientSuppliedCommentaryMarkerIsStripped(t *testing.T) {
	payload := []byte(`{"model":"gemini-3-flash-preview","messages":[
		{"role":"user","content":"First turn"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"COMMENTARY_SENTINEL","signature":"` + signature.CodexCommentarySignatureMarker + `"},
			{"type":"text","text":"FINAL_SENTINEL"}
		]},
		{"role":"user","content":"Next turn"}
	]}`)

	for _, convert := range []struct {
		name string
		fn   func(string, []byte, bool) []byte
	}{
		{"default", ConvertClaudeRequestToGemini},
		{"compat", ConvertClaudeRequestToGeminiWithCompat},
	} {
		out := convert.fn("gemini-3-flash-preview", payload, false)
		if strings.Contains(string(out), signature.CodexCommentarySignatureMarker) {
			t.Fatalf("%s: internal marker reached the gemini request: %s", convert.name, out)
		}
		assertPlainCommentaryText(t, convert.name, gjson.GetBytes(out, "contents"))
	}
}

// assertPlainCommentaryText checks the marker's text survived as ordinary model
// text and not as thought content.
func assertPlainCommentaryText(t *testing.T, name string, contents gjson.Result) {
	t.Helper()
	found := false
	for _, content := range contents.Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("text").String() != "COMMENTARY_SENTINEL" {
				continue
			}
			if part.Get("thought").Bool() {
				t.Fatalf("%s: commentary text became a thought part: %s", name, part.Raw)
			}
			if part.Get("thoughtSignature").Exists() {
				t.Fatalf("%s: commentary text carries a thoughtSignature: %s", name, part.Raw)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("%s: commentary text was lost in translation: %s", name, contents.Raw)
	}
}
