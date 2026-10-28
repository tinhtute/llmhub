package claude

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	signature "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodex_SystemMessageScenarios(t *testing.T) {
	tests := []struct {
		name             string
		inputJSON        string
		wantHasDeveloper bool
		wantTexts        []string
	}{
		{
			name: "No system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: false,
		},
		{
			name: "Empty string system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: false,
		},
		{
			name: "String system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "Be helpful",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: true,
			wantTexts:        []string{"Be helpful"},
		},
		{
			name: "System role in messages",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [
					{"role": "system", "content": "Follow the project instructions"},
					{"role": "user", "content": "hello"}
				]
			}`,
			wantHasDeveloper: true,
			wantTexts:        []string{"Follow the project instructions"},
		},
		{
			name: "Array system field with filtered billing header",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": [
					{"type": "text", "text": "x-anthropic-billing-header: tenant-123"},
					{"type": "text", "text": "Block 1"},
					{"type": "text", "text": "Block 2"}
				],
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: true,
			wantTexts:        []string{"Block 1", "Block 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
			resultJSON := gjson.ParseBytes(result)
			inputs := resultJSON.Get("input").Array()

			hasDeveloper := len(inputs) > 0 && inputs[0].Get("role").String() == "developer"
			if hasDeveloper != tt.wantHasDeveloper {
				t.Fatalf("got hasDeveloper = %v, want %v. Output: %s", hasDeveloper, tt.wantHasDeveloper, resultJSON.Get("input").Raw)
			}

			if !tt.wantHasDeveloper {
				return
			}

			content := inputs[0].Get("content").Array()
			if len(content) != len(tt.wantTexts) {
				t.Fatalf("got %d system content items, want %d. Content: %s", len(content), len(tt.wantTexts), inputs[0].Get("content").Raw)
			}

			for i, wantText := range tt.wantTexts {
				if gotType := content[i].Get("type").String(); gotType != "input_text" {
					t.Fatalf("content[%d] type = %q, want %q", i, gotType, "input_text")
				}
				if gotText := content[i].Get("text").String(); gotText != wantText {
					t.Fatalf("content[%d] text = %q, want %q", i, gotText, wantText)
				}
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_ParallelToolCalls(t *testing.T) {
	tests := []struct {
		name                  string
		inputJSON             string
		wantParallelToolCalls bool
	}{
		{
			name: "Default to true when tool_choice.disable_parallel_tool_use is absent",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: true,
		},
		{
			name: "Disable parallel tool calls when client opts out",
			inputJSON: `{
				"model": "claude-3-opus",
				"tool_choice": {"disable_parallel_tool_use": true},
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: false,
		},
		{
			name: "Keep parallel tool calls enabled when client explicitly allows them",
			inputJSON: `{
				"model": "claude-3-opus",
				"tool_choice": {"disable_parallel_tool_use": false},
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
			resultJSON := gjson.ParseBytes(result)

			if got := resultJSON.Get("parallel_tool_calls").Bool(); got != tt.wantParallelToolCalls {
				t.Fatalf("parallel_tool_calls = %v, want %v. Output: %s", got, tt.wantParallelToolCalls, string(result))
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_ShortenLongToolUseIDs(t *testing.T) {
	longID := "toolu_" + strings.Repeat("a", 62)
	if len(longID) <= 64 {
		t.Fatalf("test setup error: longID length = %d, want > 64", len(longID))
	}

	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{"role": "user", "content": [{"type":"text","text":"run pwd"}]},
			{"role": "assistant", "content": [
				{"type":"tool_use","id":"` + longID + `","name":"Bash","input":{"cmd":"pwd"}}
			]},
			{"role": "user", "content": [
				{"type":"tool_result","tool_use_id":"` + longID + `","content":"ok"}
			]}
		]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	inputs := gjson.GetBytes(result, "input").Array()

	var callID string
	var outputCallID string
	for _, item := range inputs {
		switch item.Get("type").String() {
		case "function_call":
			callID = item.Get("call_id").String()
		case "function_call_output":
			outputCallID = item.Get("call_id").String()
		}
	}

	if callID == "" {
		t.Fatalf("missing function_call item. Output: %s", string(result))
	}
	if outputCallID == "" {
		t.Fatalf("missing function_call_output item. Output: %s", string(result))
	}
	if callID != outputCallID {
		t.Fatalf("call_id mismatch: function_call=%q function_call_output=%q. Output: %s", callID, outputCallID, string(result))
	}
	if len(callID) > 64 {
		t.Fatalf("call_id length = %d, want <= 64: %q", len(callID), callID)
	}
	if callID == longID {
		t.Fatalf("long call_id was not shortened: %q", callID)
	}
}

func TestConvertClaudeRequestToCodex_ToolChoiceModeMapping(t *testing.T) {
	tests := []struct {
		name                string
		claudeToolChoice    string
		wantCodexToolChoice string
	}{
		{
			name:                "Any requires at least one tool",
			claudeToolChoice:    `{"type":"any"}`,
			wantCodexToolChoice: "required",
		},
		{
			name:                "None disables tools",
			claudeToolChoice:    `{"type":"none"}`,
			wantCodexToolChoice: "none",
		},
		{
			name:                "Auto stays auto",
			claudeToolChoice:    `{"type":"auto"}`,
			wantCodexToolChoice: "auto",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := `{
				"model": "claude-3-opus",
				"tools": [
					{"name": "lookup", "description": "Lookup", "input_schema": {"type":"object","properties":{}}}
				],
				"tool_choice": ` + tt.claudeToolChoice + `,
				"messages": [{"role": "user", "content": "hello"}]
			}`

			result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
			resultJSON := gjson.ParseBytes(result)

			if got := resultJSON.Get("tool_choice").String(); got != tt.wantCodexToolChoice {
				t.Fatalf("tool_choice = %q, want %q. Output: %s", got, tt.wantCodexToolChoice, string(result))
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_ToolChoicePreservesExistingMCPName(t *testing.T) {
	longName := "mcp__server_with_a_very_long_name_that_exceeds_sixty_four_characters__search"
	inputJSON := `{
		"model": "claude-3-opus",
		"tools": [
			{"name": "` + longName + `", "description": "Search", "input_schema": {"type":"object","properties":{}}}
		],
		"tool_choice": {"type":"tool","name":"` + longName + `"},
		"messages": [{"role": "user", "content": "hello"}]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if got := resultJSON.Get("tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function. Output: %s", got, string(result))
	}
	toolName := resultJSON.Get("tools.0.name").String()
	choiceName := resultJSON.Get("tool_choice.name").String()
	if choiceName != toolName {
		t.Fatalf("tool_choice.name = %q, want converted tool name %q. Output: %s", choiceName, toolName, string(result))
	}
	if choiceName != longName {
		t.Fatalf("tool_choice.name = %q, want byte-stable MCP name %q. Output: %s", choiceName, longName, string(result))
	}
}

func TestConvertClaudeRequestToCodex_WebSearchToolMapping(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"tools": [
			{
				"type": "web_search_20260209",
				"name": "web_search",
				"allowed_domains": ["example.com"],
				"blocked_domains": ["blocked.example"],
				"user_location": {
					"type": "approximate",
					"city": "Beijing",
					"country": "CN",
					"timezone": "Asia/Shanghai"
				}
			}
		],
		"tool_choice": {"type":"tool","name":"web_search"},
		"messages": [{"role": "user", "content": "hello"}]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if got := resultJSON.Get("tools.0.type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search. Output: %s", got, string(result))
	}
	if got := resultJSON.Get("tools.0.filters.allowed_domains.0").String(); got != "example.com" {
		t.Fatalf("tools.0.filters.allowed_domains.0 = %q, want example.com. Output: %s", got, string(result))
	}
	if resultJSON.Get("tools.0.blocked_domains").Exists() {
		t.Fatalf("tools.0.blocked_domains should not be forwarded to Codex. Output: %s", string(result))
	}
	if got := resultJSON.Get("tools.0.user_location.city").String(); got != "Beijing" {
		t.Fatalf("tools.0.user_location.city = %q, want Beijing. Output: %s", got, string(result))
	}
	if got := resultJSON.Get("tool_choice.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.type = %q, want web_search. Output: %s", got, string(result))
	}
}

func TestConvertClaudeRequestToCodex_WebSearchToolChoiceUsesDeclaredTypedToolName(t *testing.T) {
	inputJSON := `{
		"model": "claude-opus-4-7",
		"tools": [
			{"type": "web_search_20250305", "name": "browser_search"},
			{"name": "web_search", "description": "Local search", "input_schema": {"type":"object","properties":{}}}
		],
		"tool_choice": {"type":"tool","name":"web_search"},
		"messages": [{"role": "user", "content": "hello"}]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if got := resultJSON.Get("tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function. Output: %s", got, string(result))
	}
	if got := resultJSON.Get("tool_choice.name").String(); got != "web_search" {
		t.Fatalf("tool_choice.name = %q, want web_search. Output: %s", got, string(result))
	}
}

func TestConvertClaudeRequestToCodex_AssistantThinkingSignatureToReasoningItem(t *testing.T) {
	signature := validCodexReasoningSignature()
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": "visible summary must not be replayed",
						"signature": "` + signature + `"
					},
					{
						"type": "text",
						"text": "visible answer"
					}
				]
			},
			{
				"role": "user",
				"content": "continue"
			}
		]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	inputs := resultJSON.Get("input").Array()
	if len(inputs) != 3 {
		t.Fatalf("got %d input items, want 3. Output: %s", len(inputs), string(result))
	}

	reasoning := inputs[0]
	if got := reasoning.Get("type").String(); got != "reasoning" {
		t.Fatalf("first input type = %q, want reasoning. Output: %s", got, string(result))
	}
	if got := reasoning.Get("encrypted_content").String(); got != signature {
		t.Fatalf("encrypted_content = %q, want %q", got, signature)
	}
	if got := reasoning.Get("summary").Raw; got != "[]" {
		t.Fatalf("summary = %s, want []", got)
	}
	if got := reasoning.Get("content").Raw; got != "null" {
		t.Fatalf("content = %s, want null", got)
	}

	assistantMessage := inputs[1]
	if got := assistantMessage.Get("role").String(); got != "assistant" {
		t.Fatalf("second input role = %q, want assistant. Output: %s", got, string(result))
	}
	if got := assistantMessage.Get("content.0.type").String(); got != "output_text" {
		t.Fatalf("assistant content type = %q, want output_text", got)
	}
	if got := assistantMessage.Get("content.0.text").String(); got != "visible answer" {
		t.Fatalf("assistant text = %q, want visible answer", got)
	}
	if strings.Contains(string(result), "visible summary must not be replayed") {
		t.Fatalf("thinking text should not be replayed into Codex input. Output: %s", string(result))
	}
}

func TestConvertClaudeRequestToCodex_IgnoresNonCodexThinkingSignatures(t *testing.T) {
	tests := []struct {
		name      string
		inputJSON string
	}{
		{
			name: "Ignore user thinking even with Codex-shaped signature",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [
					{
						"role": "user",
						"content": [
							{
								"type": "thinking",
								"thinking": "user supplied thinking",
								"signature": "` + validCodexReasoningSignature() + `"
							},
							{
								"type": "text",
								"text": "hello"
							}
						]
					}
				]
			}`,
		},
		{
			name: "Ignore Anthropic native signature",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [
					{
						"role": "assistant",
						"content": [
							{
								"type": "thinking",
								"thinking": "anthropic thinking",
								"signature": "Eo8Canthropic-state"
							},
							{
								"type": "text",
								"text": "visible answer"
							}
						]
					}
				]
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
			if got := countRequestInputItemsByType(result, "reasoning"); got != 0 {
				t.Fatalf("got %d reasoning items, want 0. Output: %s", got, string(result))
			}
		})
	}
}

func countRequestInputItemsByType(result []byte, itemType string) int {
	count := 0
	gjson.GetBytes(result, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == itemType {
			count++
		}
		return true
	})
	return count
}

func validCodexReasoningSignature() string {
	raw := make([]byte, 1+8+16+16+32)
	raw[0] = 0x80
	raw[8] = 1
	return base64.URLEncoding.EncodeToString(raw)
}

func TestConvertClaudeRequestToCodex_PreservesBase64PDFDocumentContent(t *testing.T) {
	inputJSON := `{
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "before"},
				{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "JVBERi0xLjQK"}},
				{"type": "text", "text": "after"}
			]
		}]
	}`

	result := ConvertClaudeRequestToCodex("gpt-5.6-sol", []byte(inputJSON), false)
	content := gjson.GetBytes(result, "input.0.content").Array()
	if len(content) != 3 {
		t.Fatalf("got %d content items, want 3. Output: %s", len(content), result)
	}

	wantTypes := []string{"input_text", "input_file", "input_text"}
	for i, wantType := range wantTypes {
		if got := content[i].Get("type").String(); got != wantType {
			t.Fatalf("content[%d].type = %q, want %q. Output: %s", i, got, wantType, result)
		}
	}
	if got := content[1].Get("file_data").String(); got != "data:application/pdf;base64,JVBERi0xLjQK" {
		t.Fatalf("content[1].file_data = %q, want PDF data URL", got)
	}
	if got := content[1].Get("filename").String(); got != "document.pdf" {
		t.Fatalf("content[1].filename = %q, want %q", got, "document.pdf")
	}
}

func TestConvertClaudeRequestToCodex_OutputConfigFormatStrictDowngrade(t *testing.T) {
	t.Run("json_schema with optional property downgrades strict", func(t *testing.T) {
		payload := []byte(`{
			"model": "gpt-5.4",
			"messages": [
				{"role": "user", "content": "hello"}
			],
			"output_config": {
				"format": {
					"type": "json_schema",
					"name": "cli_proxy_structured_output",
					"strict": true,
					"schema": {
						"type": "object",
						"properties": {
							"answer": {"type": "string"},
							"impossible": {"type": "string"}
						},
						"required": ["answer"],
						"additionalProperties": false
					}
				}
			}
		}`)

		translated := ConvertClaudeRequestToCodex("gpt-5.4", payload, false)
		root := gjson.ParseBytes(translated)
		if got := root.Get("text.format.strict").Bool(); got != false {
			t.Errorf("expected text.format.strict to be false for non-strict-compatible schema, got %v (%s)", got, translated)
		}
		if got := root.Get("text.format.name").String(); got != "cli_proxy_structured_output" {
			t.Errorf("expected text.format.name to be preserved, got %q", got)
		}
	})

	t.Run("json_schema fully required keeps strict", func(t *testing.T) {
		payload := []byte(`{
			"model": "gpt-5.4",
			"messages": [
				{"role": "user", "content": "hello"}
			],
			"output_config": {
				"format": {
					"type": "json_schema",
					"schema": {
						"type": "object",
						"properties": {
							"answer": {"type": "string"}
						},
						"required": ["answer"],
						"additionalProperties": false
					}
				}
			}
		}`)

		translated := ConvertClaudeRequestToCodex("gpt-5.4", payload, false)
		root := gjson.ParseBytes(translated)
		if got := root.Get("text.format.strict").Bool(); !got {
			t.Errorf("expected text.format.strict to stay true for strict-compatible schema, got %v (%s)", got, translated)
		}
	})

	t.Run("json_schema nested optional property downgrades strict", func(t *testing.T) {
		payload := []byte(`{
			"model": "gpt-5.4",
			"messages": [
				{"role": "user", "content": "hello"}
			],
			"output_config": {
				"format": {
					"type": "json_schema",
					"schema": {
						"type": "object",
						"properties": {
							"answer": {
								"type": "object",
								"properties": {
									"text": {"type": "string"},
									"note": {"type": "string"}
								},
								"required": ["text"]
							}
						},
						"required": ["answer"]
					}
				}
			}
		}`)

		translated := ConvertClaudeRequestToCodex("gpt-5.4", payload, false)
		root := gjson.ParseBytes(translated)
		if got := root.Get("text.format.strict").Bool(); got != false {
			t.Errorf("expected text.format.strict to be false for nested optional property, got %v (%s)", got, translated)
		}
	})

	t.Run("json_schema missing required entirely downgrades strict", func(t *testing.T) {
		payload := []byte(`{
			"model": "gpt-5.4",
			"messages": [
				{"role": "user", "content": "hello"}
			],
			"output_config": {
				"format": {
					"type": "json_schema",
					"schema": {
						"type": "object",
						"properties": {
							"answer": {"type": "string"}
						}
					}
				}
			}
		}`)

		translated := ConvertClaudeRequestToCodex("gpt-5.4", payload, false)
		root := gjson.ParseBytes(translated)
		if got := root.Get("text.format.strict").Bool(); got != false {
			t.Errorf("expected text.format.strict to be false when required is missing, got %v (%s)", got, translated)
		}
	})

	t.Run("explicit strict false stays false", func(t *testing.T) {
		payload := []byte(`{
			"model": "gpt-5.4",
			"messages": [
				{"role": "user", "content": "hello"}
			],
			"output_config": {
				"format": {
					"type": "json_schema",
					"strict": false,
					"schema": {
						"type": "object",
						"properties": {
							"answer": {"type": "string"}
						},
						"required": ["answer"]
					}
				}
			}
		}`)

		translated := ConvertClaudeRequestToCodex("gpt-5.4", payload, false)
		root := gjson.ParseBytes(translated)
		if got := root.Get("text.format.strict").Bool(); got != false {
			t.Errorf("expected text.format.strict to stay false, got %v (%s)", got, translated)
		}
	})
}

func TestConvertClaudeRequestToCodex_StripsUnsupportedUnicodePropertyEscapePatterns(t *testing.T) {
	inputJSON := `{
		"model": "gpt-5.6",
		"messages": [{"role": "user", "content": "hello"}],
		"tools": [{
			"name": "pattern_tool",
			"input_schema": {
				"type": "object",
				"properties": {
					"bad": {"type": "string", "pattern": "^\\p{L}+$"},
					"escaped": {"type": "string", "pattern": "\u005c\u0070{N}+"},
					"good": {"type": "string", "pattern": "^[a-z]+$"},
					"nested": {"type": "object", "properties": {"deep": {"type": "string", "pattern": "\\P{N}+"}}},
					"userdata": {"type": "string", "default": "keep \\p{L} text", "description": "a pattern mention"}
				},
				"patternProperties": {
					"^\\p{L}+$": {"type": "string"},
					"^[a-z]+$": {"type": "string"}
				}
			}
		}]
	}`

	translated := ConvertClaudeRequestToCodex("gpt-5.6", []byte(inputJSON), false)
	tools := gjson.GetBytes(translated, "tools").Array()
	if len(tools) == 0 {
		t.Fatalf("expected tools in translated payload, got: %s", translated)
	}
	params := tools[0].Get("parameters")

	if params.Get("properties.bad.pattern").Exists() {
		t.Errorf("expected properties.bad.pattern (\\p{L}) to be removed, got: %s", params.Get("properties.bad.pattern").Raw)
	}
	if params.Get("properties.escaped.pattern").Exists() {
		t.Errorf("expected JSON-escaped properties.escaped.pattern to be removed, got: %s", params.Get("properties.escaped.pattern").Raw)
	}
	if got := params.Get("properties.good.pattern").String(); got != "^[a-z]+$" {
		t.Errorf("expected properties.good.pattern preserved, got %q: %s", got, params.Raw)
	}
	if params.Get("properties.nested.properties.deep.pattern").Exists() {
		t.Errorf("expected nested deep.pattern to be removed, got: %s", params.Raw)
	}
	if got := params.Get("properties.userdata.default").String(); got != `keep \p{L} text` {
		t.Errorf("expected user-data default preserved, got %q: %s", got, params.Raw)
	}
	if got := params.Get("properties.userdata.description").String(); got != "a pattern mention" {
		t.Errorf("expected user-data description preserved, got %q: %s", got, params.Raw)
	}
	patternProps := params.Get("patternProperties").Map()
	if _, exists := patternProps[`^\p{L}+$`]; exists {
		t.Errorf("expected patternProperties key with \\p{L} to be removed, got: %s", params.Get("patternProperties").Raw)
	}
	if _, exists := patternProps[`^[a-z]+$`]; !exists {
		t.Errorf("expected safe patternProperties key preserved, got: %s", params.Get("patternProperties").Raw)
	}
}

func testCodexClaudeMetaSignature() string {
	decoded := make([]byte, 45+1024)
	copy(decoded, []byte{0x43, 0xe3, 0xda, 0x0e, 0x01})
	return base64.RawURLEncoding.EncodeToString(decoded)
}

func TestConvertClaudeRequestToCodex_PreservesMetaSignatureForMuseTarget(t *testing.T) {
	metaSignature := testCodexClaudeMetaSignature()
	payload := []byte(`{"model":"muse-spark-1.3","messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"internal reasoning","signature":"` + metaSignature + `"}]},` +
		`{"role":"user","content":"continue"}` +
		`]}`)

	out := ConvertClaudeRequestToCodex("muse-spark-1.3", payload, false)

	var reasoningSignatures []string
	for _, item := range gjson.GetBytes(out, "input").Array() {
		if item.Get("type").String() == "reasoning" {
			reasoningSignatures = append(reasoningSignatures, item.Get("encrypted_content").String())
		}
	}
	if len(reasoningSignatures) != 1 {
		t.Fatalf("reasoning items = %d, want 1; output=%s", len(reasoningSignatures), out)
	}
	if reasoningSignatures[0] != metaSignature {
		t.Fatalf("reasoning encrypted_content = %q, want the Muse envelope replayed verbatim", reasoningSignatures[0])
	}
}

func TestConvertClaudeRequestToCodex_DropsMetaSignatureForNonMetaTarget(t *testing.T) {
	metaSignature := testCodexClaudeMetaSignature()
	payload := []byte(`{"model":"gpt-5.4","messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"internal reasoning","signature":"` + metaSignature + `"}]},` +
		`{"role":"user","content":"continue"}` +
		`]}`)

	out := ConvertClaudeRequestToCodex("gpt-5.4", payload, false)

	for _, item := range gjson.GetBytes(out, "input").Array() {
		if item.Get("type").String() != "reasoning" {
			continue
		}
		if got := item.Get("encrypted_content").String(); got == metaSignature {
			t.Fatalf("Muse envelope leaked to a GPT target; output=%s", out)
		}
	}
}

// TestConvertClaudeRequestToCodex_TargetFamilyGating pins the Codex replay gate
// review finding 2 identified: the target family now decides which signature
// may leave (upstream 772df04b30d5 / 674dbe8fd1be).
//   - a grok-* target keeps a Grok blob and drops a GPT signature (base kept the
//     GPT one through CompatibleSignatureForProvider(GPT, ...));
//   - a claude-* or gemini-* target now drops a GPT signature (base kept it);
//   - a gemini-* target keeps its own envelope;
//   - a namespaced tail resolves to its canonical family (openai/o3-mini -> GPT).
func TestConvertClaudeRequestToCodex_TargetFamilyGating(t *testing.T) {
	gptSig := testCodexClaudeGPTSignature()
	metaSig := testCodexClaudeMetaSignature()
	grokSig := testCodexClaudeGrokSignature()
	geminiSig := testCodexClaudeGeminiSignature()

	build := func(signature string) []byte {
		return []byte(`{"messages":[` +
			`{"role":"assistant","content":[{"type":"thinking","thinking":"internal reasoning","signature":"` + signature + `"}]},` +
			`{"role":"user","content":"continue"}` +
			`]}`)
	}
	reasoningSignature := func(out []byte) string {
		for _, item := range gjson.GetBytes(out, "input").Array() {
			if item.Get("type").String() == "reasoning" {
				return item.Get("encrypted_content").String()
			}
		}
		return ""
	}

	tests := []struct {
		name      string
		model     string
		signature string
		want      string
	}{
		{"grok target drops GPT signature", "grok-4", gptSig, ""},
		{"grok target keeps grok blob", "grok-4", grokSig, grokSig},
		{"claude target drops GPT signature", "claude-sonnet-4-6", gptSig, ""},
		{"gemini target drops GPT signature", "gemini-3-pro", gptSig, ""},
		{"gemini target keeps gemini envelope", "gemini-3-pro", geminiSig, geminiSig},
		{"muse target keeps muse envelope", "muse-spark-1.3", metaSig, metaSig},
		{"namespaced tail resolves to GPT", "openai/o3-mini", gptSig, gptSig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := ConvertClaudeRequestToCodex(tc.model, build(tc.signature), false)
			if got := reasoningSignature(out); got != tc.want {
				t.Fatalf("reasoning encrypted_content = %q, want %q (model=%s); output=%s", got, tc.want, tc.model, out)
			}
		})
	}
}

func testCodexClaudeGPTSignature() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func testCodexClaudeGrokSignature() string {
	payload := make([]byte, 135)
	for i := range payload {
		payload[i] = byte((i*73 + 19) & 0xff)
	}
	return base64.RawStdEncoding.EncodeToString(payload)
}

func testCodexClaudeGeminiSignature() string {
	payload := make([]byte, 64)
	payload[0] = 0x0a
	for i := 1; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(payload)
}

// reasoning.effort precedence on the Claude->Codex conversion: thinking
// semantics win over a standalone output_config.effort, and a budget token
// count wins over the effort field itself.
func TestConvertClaudeRequestToCodex_ReasoningEffortPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		fields     string
		wantEffort string
	}{
		{name: "standalone effort", fields: `,"output_config":{"effort":"high"}`, wantEffort: "high"},
		{name: "standalone effort is normalized", fields: `,"output_config":{"effort":" LOW "}`, wantEffort: "low"},
		{name: "adaptive effort", fields: `,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}`, wantEffort: "low"},
		{name: "adaptive without effort", fields: `,"thinking":{"type":"adaptive"}`, wantEffort: "xhigh"},
		{name: "budget beats effort", fields: `,"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"high"}`, wantEffort: "medium"},
		{name: "disabled beats effort", fields: `,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}`, wantEffort: "none"},
		{name: "missing effort keeps default", fields: ``, wantEffort: "medium"},
		{name: "blank effort keeps default", fields: `,"output_config":{"effort":"   "}`, wantEffort: "medium"},
		{name: "non-string effort keeps default", fields: `,"output_config":{"effort":123}`, wantEffort: "medium"},
		{name: "empty thinking object keeps default", fields: `,"thinking":{},"output_config":{"effort":"high"}`, wantEffort: "medium"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"model":"gpt-6.1-sol","max_tokens":256,"messages":[{"role":"user","content":"hi"}]` + tt.fields + `}`)
			out := ConvertClaudeRequestToCodex("gpt-6.1-sol", payload, false)
			if got := gjson.GetBytes(out, "reasoning.effort").String(); got != tt.wantEffort {
				t.Fatalf("reasoning.effort = %q, want %q; body=%s", got, tt.wantEffort, out)
			}
		})
	}
}

// The round trip this phase exists for: a Codex commentary item becomes a marked
// Claude thinking block, and replaying that history to Codex restores the item
// instead of turning the preamble into reasoning history.
func TestConvertClaudeRequestToCodex_CommentaryMarkerRoundTrip(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5","output":[
		{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"COMMENTARY_SENTINEL"}]},
		{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"FINAL_SENTINEL"}]}
	],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	var param any
	claudeResponse := ConvertCodexResponseToClaudeNonStream(context.Background(), "", []byte(`{"messages":[]}`), nil, raw, &param)

	if got := gjson.GetBytes(claudeResponse, "content.0.signature").String(); got != signature.CodexCommentarySignatureMarker {
		t.Fatalf("precondition: commentary block signature = %q, want the internal marker", got)
	}

	// Replaying the assistant turn is what a Claude client sends back next turn.
	history := []byte(`{"model":"gpt-5","max_tokens":128,"messages":[{"role":"user","content":"First turn"},{"role":"assistant","content":` +
		gjson.GetBytes(claudeResponse, "content").Raw + `},{"role":"user","content":"Next turn"}]}`)

	for _, stream := range []bool{false, true} {
		out := ConvertClaudeRequestToCodex("gpt-5", history, stream)
		if strings.Contains(string(out), signature.CodexCommentarySignatureMarker) {
			t.Fatalf("stream=%v: internal marker leaked into the upstream request: %s", stream, out)
		}
		if gjson.GetBytes(out, "input.#").Int() != 4 {
			t.Fatalf("stream=%v: input item count = %d, want 4: %s", stream, gjson.GetBytes(out, "input.#").Int(), out)
		}
		commentary := gjson.GetBytes(out, "input.1")
		if commentary.Get("type").String() != "message" || commentary.Get("role").String() != "assistant" || commentary.Get("phase").String() != "commentary" {
			t.Fatalf("stream=%v: second input item should be an assistant commentary message: %s", stream, commentary.Raw)
		}
		if got := commentary.Get("content.0.type").String(); got != "output_text" {
			t.Fatalf("stream=%v: commentary content type = %q, want output_text", stream, got)
		}
		if got := commentary.Get("content.0.text").String(); got != "COMMENTARY_SENTINEL" {
			t.Fatalf("stream=%v: commentary text = %q, want COMMENTARY_SENTINEL", stream, got)
		}
		final := gjson.GetBytes(out, "input.2")
		if final.Get("type").String() != "message" || final.Get("phase").Exists() {
			t.Fatalf("stream=%v: third input item should be a plain assistant message: %s", stream, final.Raw)
		}
		if got := final.Get("content.0.text").String(); got != "FINAL_SENTINEL" {
			t.Fatalf("stream=%v: final text = %q, want FINAL_SENTINEL", stream, got)
		}
	}
}

// The marker is a forgeable literal: a client can type it into its own thinking
// block. It must be consumed here and never forwarded, and a genuine signature in
// the same payload must keep becoming reasoning history.
func TestConvertClaudeRequestToCodex_ClientSuppliedCommentaryMarkerIsStripped(t *testing.T) {
	payload := []byte(`{"model":"gpt-5","max_tokens":128,"messages":[
		{"role":"user","content":"First turn"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"COMMENTARY_SENTINEL","signature":"cpa:commentary:v1"},
			{"type":"thinking","thinking":"REASON_SENTINEL","signature":"` + testCodexClaudeGPTSignature() + `"},
			{"type":"text","text":"FINAL_SENTINEL"}
		]}
	]}`)

	for _, stream := range []bool{false, true} {
		out := ConvertClaudeRequestToCodex("gpt-5", payload, stream)
		if strings.Contains(string(out), signature.CodexCommentarySignatureMarker) {
			t.Fatalf("stream=%v: client-supplied marker reached the upstream request: %s", stream, out)
		}
		commentary := gjson.GetBytes(out, "input.1")
		if commentary.Get("phase").String() != "commentary" {
			t.Fatalf("stream=%v: forged marker should still restore the commentary item: %s", stream, commentary.Raw)
		}
		if got := commentary.Get("content.0.text").String(); got != "COMMENTARY_SENTINEL" {
			t.Fatalf("stream=%v: commentary text = %q, want COMMENTARY_SENTINEL", stream, got)
		}
		reasoning := gjson.GetBytes(out, "input.2")
		if reasoning.Get("type").String() != "reasoning" {
			t.Fatalf("stream=%v: genuine signature should stay reasoning history: %s", stream, reasoning.Raw)
		}
		if got := reasoning.Get("encrypted_content").String(); got != testCodexClaudeGPTSignature() {
			t.Fatalf("stream=%v: reasoning signature = %q, want the original value", stream, got)
		}
	}
}
