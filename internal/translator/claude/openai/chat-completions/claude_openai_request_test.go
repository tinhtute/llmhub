package chat_completions

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToClaude_MaxTokensAndMaxCompletionTokens(t *testing.T) {
	tests := []struct {
		name      string
		rawJSON   string
		wantLimit int64
	}{
		{
			name:      "only max_completion_tokens",
			rawJSON:   `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":128000}`,
			wantLimit: 128000,
		},
		{
			name:      "only max_tokens",
			rawJSON:   `{"messages":[{"role":"user","content":"hi"}],"max_tokens":4096}`,
			wantLimit: 4096,
		},
		{
			name:      "both present prefers max_tokens",
			rawJSON:   `{"messages":[{"role":"user","content":"hi"}],"max_tokens":4096,"max_completion_tokens":128000}`,
			wantLimit: 4096,
		},
		{
			name:      "neither present uses default template limit",
			rawJSON:   `{"messages":[{"role":"user","content":"hi"}]}`,
			wantLimit: 32000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := ConvertOpenAIRequestToClaude("claude-opus-4-1-20250805", []byte(tc.rawJSON), false)
			got := gjson.GetBytes(out, "max_tokens").Int()
			if got != tc.wantLimit {
				t.Fatalf("max_tokens = %d, want %d. Output: %s", got, tc.wantLimit, string(out))
			}
		})
	}
}

func TestConvertOpenAIRequestToClaudeNormalizesToolInputSchema(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [{"role":"user","content":"Use lookup"}],
		"tools": [
			{"type":"function","function":{
				"name":"lookup",
				"description":"Lookup",
				"parameters":{"anyOf":[{"type":"null"},{"type":"object","properties":{"query":{"type":"string"}}}]}
			}}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	tool := gjson.GetBytes(result, "tools.0")
	if got := tool.Get("input_schema.type").String(); got != "object" {
		t.Fatalf("input_schema.type = %q, want object; result=%s", got, result)
	}
	if tool.Get("input_schema.anyOf").Exists() {
		t.Fatalf("input_schema.anyOf remained: %s", tool.Get("input_schema").Raw)
	}
	if got := tool.Get("input_schema.properties.query.type").String(); got != "string" {
		t.Fatalf("query type = %q, want string; schema=%s", got, tool.Get("input_schema").Raw)
	}
}

func TestConvertOpenAIRequestToClaude_ToolResultTextAndBase64Image(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "do_work",
							"arguments": "{\"a\":1}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call_1",
				"content": [
					{"type": "text", "text": "tool ok"},
					{
						"type": "image_url",
						"image_url": {
							"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
						}
					}
				]
			}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	toolResult := messages[1].Get("content.0")
	if got := toolResult.Get("type").String(); got != "tool_result" {
		t.Fatalf("Expected content[0].type %q, got %q", "tool_result", got)
	}
	if got := toolResult.Get("tool_use_id").String(); got != "call_1" {
		t.Fatalf("Expected tool_use_id %q, got %q", "call_1", got)
	}

	toolContent := toolResult.Get("content")
	if !toolContent.IsArray() {
		t.Fatalf("Expected tool_result content array, got %s", toolContent.Raw)
	}
	if got := toolContent.Get("0.type").String(); got != "text" {
		t.Fatalf("Expected first tool_result part type %q, got %q", "text", got)
	}
	if got := toolContent.Get("0.text").String(); got != "tool ok" {
		t.Fatalf("Expected first tool_result part text %q, got %q", "tool ok", got)
	}
	if got := toolContent.Get("1.type").String(); got != "image" {
		t.Fatalf("Expected second tool_result part type %q, got %q", "image", got)
	}
	if got := toolContent.Get("1.source.type").String(); got != "base64" {
		t.Fatalf("Expected image source type %q, got %q", "base64", got)
	}
	if got := toolContent.Get("1.source.media_type").String(); got != "image/png" {
		t.Fatalf("Expected image media type %q, got %q", "image/png", got)
	}
	if got := toolContent.Get("1.source.data").String(); got != "iVBORw0KGgoAAAANSUhEUg==" {
		t.Fatalf("Unexpected base64 image data: %q", got)
	}
}

func TestConvertOpenAIRequestToClaude_ToolResultURLImageOnly(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "do_work",
							"arguments": "{\"a\":1}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call_1",
				"content": [
					{
						"type": "image_url",
						"image_url": {
							"url": "https://example.com/tool.png"
						}
					}
				]
			}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	toolContent := messages[1].Get("content.0.content")
	if !toolContent.IsArray() {
		t.Fatalf("Expected tool_result content array, got %s", toolContent.Raw)
	}
	if got := toolContent.Get("0.type").String(); got != "image" {
		t.Fatalf("Expected tool_result part type %q, got %q", "image", got)
	}
	if got := toolContent.Get("0.source.type").String(); got != "url" {
		t.Fatalf("Expected image source type %q, got %q", "url", got)
	}
	if got := toolContent.Get("0.source.url").String(); got != "https://example.com/tool.png" {
		t.Fatalf("Unexpected image URL: %q", got)
	}
}

func TestConvertOpenAIRequestToClaude_GroupsConsecutiveToolResults(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{"id":"call_1","type":"function","function":{"name":"first","arguments":"{}"}},
					{"id":"call_2","type":"function","function":{"name":"second","arguments":"{}"}}
				]
			},
			{"role":"tool","tool_call_id":"call_1","content":"first ok"},
			{"role":"tool","tool_call_id":"call_2","content":"second ok"},
			{"role":"user","content":"next"}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 3 {
		t.Fatalf("Expected 3 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}
	if got := messages[1].Get("role").String(); got != "user" {
		t.Fatalf("Expected grouped tool results role %q, got %q", "user", got)
	}
	contents := messages[1].Get("content").Array()
	if len(contents) != 2 {
		t.Fatalf("Expected 2 grouped tool results, got %d. Message: %s", len(contents), messages[1].Raw)
	}
	if got := contents[0].Get("tool_use_id").String(); got != "call_1" {
		t.Fatalf("Expected first tool_use_id %q, got %q", "call_1", got)
	}
	if got := contents[1].Get("tool_use_id").String(); got != "call_2" {
		t.Fatalf("Expected second tool_use_id %q, got %q", "call_2", got)
	}
	if got := messages[2].Get("content.0.text").String(); got != "next" {
		t.Fatalf("Expected following user message to remain separate, got %q", got)
	}
}

func TestConvertOpenAIRequestToClaude_SystemRoleBecomesTopLevelSystem(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Hello"}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	system := resultJSON.Get("system")
	if !system.IsArray() {
		t.Fatalf("Expected top-level system array, got %s", system.Raw)
	}
	if len(system.Array()) != 1 {
		t.Fatalf("Expected 1 system block, got %d. System: %s", len(system.Array()), system.Raw)
	}
	if got := system.Get("0.type").String(); got != "text" {
		t.Fatalf("Expected system block type %q, got %q", "text", got)
	}
	if got := system.Get("0.text").String(); got != "You are a helpful assistant." {
		t.Fatalf("Expected system text %q, got %q", "You are a helpful assistant.", got)
	}

	messages := resultJSON.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("Expected 1 non-system message, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected remaining message role %q, got %q", "user", got)
	}
	if got := messages[0].Get("content.0.text").String(); got != "Hello" {
		t.Fatalf("Expected user text %q, got %q", "Hello", got)
	}
}

func TestConvertOpenAIRequestToClaude_MultipleSystemMessagesMergedIntoTopLevelSystem(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{"role": "system", "content": "Rule 1"},
			{"role": "system", "content": [{"type": "text", "text": "Rule 2"}]},
			{"role": "user", "content": "Hello"}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	system := resultJSON.Get("system").Array()
	if len(system) != 2 {
		t.Fatalf("Expected 2 system blocks, got %d. System: %s", len(system), resultJSON.Get("system").Raw)
	}
	if got := system[0].Get("text").String(); got != "Rule 1" {
		t.Fatalf("Expected first system text %q, got %q", "Rule 1", got)
	}
	if got := system[1].Get("text").String(); got != "Rule 2" {
		t.Fatalf("Expected second system text %q, got %q", "Rule 2", got)
	}

	messages := resultJSON.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("Expected 1 non-system message, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected remaining message role %q, got %q", "user", got)
	}
	if got := messages[0].Get("content.0.text").String(); got != "Hello" {
		t.Fatalf("Expected user text %q, got %q", "Hello", got)
	}
}

func TestConvertOpenAIRequestToClaude_SystemOnlyInputKeepsFallbackUserMessage(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	system := resultJSON.Get("system").Array()
	if len(system) != 1 {
		t.Fatalf("Expected 1 system block, got %d. System: %s", len(system), resultJSON.Get("system").Raw)
	}
	if got := system[0].Get("text").String(); got != "You are a helpful assistant." {
		t.Fatalf("Expected system text %q, got %q", "You are a helpful assistant.", got)
	}

	messages := resultJSON.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("Expected 1 fallback message, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected fallback message role %q, got %q", "user", got)
	}
	if got := messages[0].Get("content.0.type").String(); got != "text" {
		t.Fatalf("Expected fallback content type %q, got %q", "text", got)
	}
	if got := messages[0].Get("content.0.text").String(); got != "" {
		t.Fatalf("Expected fallback text %q, got %q", "", got)
	}
}

func TestConvertOpenAIRequestToClaude_PreservesCallerSuppliedMetadataUserID(t *testing.T) {
	testCases := []struct {
		name     string
		rawJSON  string
		expected string
	}{
		{
			name:     "plain string",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"custom-user-123"},"messages":[{"role":"user","content":"hello"}]}`,
			expected: "custom-user-123",
		},
		{
			name:     "special characters and json string",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"foo\"bar\nbaz\\qux"},"messages":[{"role":"user","content":"hello"}]}`,
			expected: "foo\"bar\nbaz\\qux",
		},
		{
			name:     "claude code json format",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"{\"device_id\":\"0000000000000000000000000000000000000000000000000000000000000000\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"},"messages":[{"role":"user","content":"hello"}]}`,
			expected: `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","session_id":"11111111-2222-4333-8444-555555555555"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			out := ConvertOpenAIRequestToClaude("claude-test", []byte(tc.rawJSON), false)
			if !gjson.ValidBytes(out) {
				t.Fatalf("output is invalid json: %s", string(out))
			}
			got := gjson.GetBytes(out, "metadata.user_id").String()
			if got != tc.expected {
				t.Fatalf("metadata.user_id = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestConvertOpenAIRequestToClaude_PreservesOpenAIUserField(t *testing.T) {
	raw := []byte(`{"model":"claude-test","user":"openai-user-456","messages":[{"role":"user","content":"hello"}]}`)
	out := ConvertOpenAIRequestToClaude("claude-test", raw, false)
	if !gjson.ValidBytes(out) {
		t.Fatalf("output is invalid json: %s", string(out))
	}
	got := gjson.GetBytes(out, "metadata.user_id").String()
	if got != "openai-user-456" {
		t.Fatalf("metadata.user_id = %q, want %q", got, "openai-user-456")
	}
}

func TestConvertOpenAIRequestToClaude_DifferentSessionsProduceDifferentUserIDs(t *testing.T) {
	a := []byte(`{"model":"claude-test","prompt_cache_key":"session-a","messages":[{"role":"user","content":"hello"}]}`)
	b := []byte(`{"model":"claude-test","prompt_cache_key":"session-b","messages":[{"role":"user","content":"hello"}]}`)
	outA := ConvertOpenAIRequestToClaude("claude-test", a, false)
	outB := ConvertOpenAIRequestToClaude("claude-test", b, false)
	idA := gjson.GetBytes(outA, "metadata.user_id").String()
	idB := gjson.GetBytes(outB, "metadata.user_id").String()
	if idA == idB {
		t.Fatalf("different prompt_cache_key produced identical metadata.user_id: %q", idA)
	}
}

func TestConvertOpenAIRequestToClaude_DeterministicWithoutSessionKey(t *testing.T) {
	first := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"stable first message"}]}`)
	second := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"stable first message"},{"role":"assistant","content":"hi"},{"role":"user","content":"second message"}]}`)
	outFirst := ConvertOpenAIRequestToClaude("claude-test", first, false)
	outSecond := ConvertOpenAIRequestToClaude("claude-test", second, false)
	idFirst := gjson.GetBytes(outFirst, "metadata.user_id").String()
	idSecond := gjson.GetBytes(outSecond, "metadata.user_id").String()
	if idFirst == "" || idFirst == "unknown" {
		t.Fatalf("expected non-empty derived user_id, got %q", idFirst)
	}
	if idFirst != idSecond {
		t.Fatalf("turn growth changed derived user_id: %q vs %q", idFirst, idSecond)
	}
}

// Ported from upstream CLIProxyAPI commit f247e2b0 (strict tool mode, issue #5958).
func TestConvertOpenAIRequestToClaude_ToolStrict(t *testing.T) {
	t.Run("preserves strict true on function tool", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "hi"}],
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
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		toolStrict := gjson.GetBytes(result, "tools.0.strict")
		if !toolStrict.Exists() {
			t.Fatalf("expected tools.0.strict to exist in Claude output: %s", result)
		}
		if !toolStrict.Bool() {
			t.Fatalf("expected tools.0.strict to be true, got %v", toolStrict.Value())
		}
	})

	t.Run("preserves strict true when on top level tool", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"strict": true,
					"function": {
						"name": "tool_b",
						"description": "Controlled tool.",
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		toolStrict := gjson.GetBytes(result, "tools.0.strict")
		if !toolStrict.Exists() || !toolStrict.Bool() {
			t.Fatalf("expected tools.0.strict to be true, got %s", result)
		}
	})

	t.Run("preserves strict false on function tool", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_c",
						"description": "Controlled tool.",
						"strict": false,
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		toolStrict := gjson.GetBytes(result, "tools.0.strict")
		if !toolStrict.Exists() {
			t.Fatalf("expected tools.0.strict to exist in Claude output: %s", result)
		}
		if toolStrict.Bool() {
			t.Fatalf("expected tools.0.strict to be false, got %v", toolStrict.Value())
		}
	})

	t.Run("omits strict when not provided", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_d",
						"description": "Controlled tool.",
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		if gjson.GetBytes(result, "tools.0.strict").Exists() {
			t.Fatalf("expected tools.0.strict to be omitted when not provided, got %s", result)
		}
	})
}

// Ported from upstream CLIProxyAPI commit 49eec664 (tool choice fail-closed, issue #5957).
func TestConvertOpenAIRequestToClaude_ToolChoice(t *testing.T) {
	t.Run("none produces type none", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "Answer without calling tools."}],
			"tool_choice": "none",
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}},
				{"type": "function", "function": {"name": "tool_b", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotType := gjson.GetBytes(result, "tool_choice.type").String()
		if gotType != "none" {
			t.Fatalf("expected tool_choice.type to be 'none', got %q. Output: %s", gotType, result)
		}
	})

	t.Run("object none produces type none", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "Answer without calling tools."}],
			"tool_choice": {"type": "none"},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotType := gjson.GetBytes(result, "tool_choice.type").String()
		if gotType != "none" {
			t.Fatalf("expected tool_choice.type to be 'none', got %q. Output: %s", gotType, result)
		}
	})

	t.Run("allowed_tools filters tools and sets auto mode", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "Use tool_b"}],
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
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotType := gjson.GetBytes(result, "tool_choice.type").String()
		tools := gjson.GetBytes(result, "tools").Array()
		if gotType != "auto" {
			t.Fatalf("expected tool_choice type='auto', got %q. Output: %s", gotType, result)
		}
		if len(tools) != 1 || tools[0].Get("name").String() != "tool_b" {
			t.Fatalf("expected tools to contain only tool_b, got %v. Output: %s", tools, result)
		}
	})

	t.Run("allowed_tools multi function filters tools and supports required mode", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "Use tools"}],
			"tool_choice": {
				"type": "allowed_tools",
				"allowed_tools": {
					"mode": "required",
					"tools": [
						{"type": "function", "function": {"name": "tool_b"}},
						{"type": "function", "function": {"name": "tool_c"}}
					]
				}
			},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}},
				{"type": "function", "function": {"name": "tool_b", "parameters": {"type": "object", "properties": {}}}},
				{"type": "function", "function": {"name": "tool_c", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotType := gjson.GetBytes(result, "tool_choice.type").String()
		tools := gjson.GetBytes(result, "tools").Array()
		if gotType != "any" {
			t.Fatalf("expected tool_choice type='any', got %q. Output: %s", gotType, result)
		}
		if len(tools) != 2 {
			t.Fatalf("expected 2 tools, got %d. Output: %s", len(tools), result)
		}
	})

	t.Run("parallel_tool_calls false adds disable_parallel_tool_use", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "required",
			"parallel_tool_calls": false,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotType := gjson.GetBytes(result, "tool_choice.type").String()
		gotDisable := gjson.GetBytes(result, "tool_choice.disable_parallel_tool_use").Bool()
		if gotType != "any" || !gotDisable {
			t.Fatalf("expected type='any' with disable_parallel_tool_use=true, got type=%q disable=%v. Output: %s", gotType, gotDisable, result)
		}
	})

	t.Run("parallel_tool_calls null does not add disable_parallel_tool_use", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "required",
			"parallel_tool_calls": null,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotDisable := gjson.GetBytes(result, "tool_choice.disable_parallel_tool_use")
		if gotDisable.Exists() && gotDisable.Bool() {
			t.Fatalf("expected disable_parallel_tool_use to not be true for parallel_tool_calls=null. Output: %s", result)
		}
	})

	t.Run("parallel_tool_calls true does not add disable_parallel_tool_use", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": "required",
			"parallel_tool_calls": true,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotDisable := gjson.GetBytes(result, "tool_choice.disable_parallel_tool_use")
		if gotDisable.Exists() && gotDisable.Bool() {
			t.Fatalf("expected disable_parallel_tool_use to not be true for parallel_tool_calls=true. Output: %s", result)
		}
	})

	t.Run("omitted tool_choice with parallel_tool_calls false sets auto with disable_parallel_tool_use", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "test"}],
			"parallel_tool_calls": false,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotType := gjson.GetBytes(result, "tool_choice.type").String()
		gotDisable := gjson.GetBytes(result, "tool_choice.disable_parallel_tool_use").Bool()
		if gotType != "auto" || !gotDisable {
			t.Fatalf("expected type='auto' with disable_parallel_tool_use=true, got type=%q disable=%v. Output: %s", gotType, gotDisable, result)
		}
	})

	t.Run("empty allowed_tools fails closed to type none", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
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
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotType := gjson.GetBytes(result, "tool_choice.type").String()
		if gotType != "none" {
			t.Fatalf("expected tool_choice type='none', got %q. Output: %s", gotType, result)
		}
	})

	t.Run("function choice with missing name fails closed to type none", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {"type": "function", "function": {}},
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		gotType := gjson.GetBytes(result, "tool_choice.type").String()
		if gotType != "none" {
			t.Fatalf("expected tool_choice type='none', got %q. Output: %s", gotType, result)
		}
	})

	t.Run("tool_choice null does not set tool_choice", func(t *testing.T) {
		inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": null,
			"tools": [
				{"type": "function", "function": {"name": "tool_a", "parameters": {"type": "object", "properties": {}}}}
			]
		}`
		result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)
		if gjson.GetBytes(result, "tool_choice").Exists() {
			t.Fatalf("expected tool_choice not to be set when tool_choice is null, got: %s", result)
		}
	})
}
func TestConvertOpenAIRequestToClaude_SanitizesToolNamesAndProvidesFallbackSchema(t *testing.T) {
	inputJSON := `{
		"model": "claude-sonnet-4-6",
		"messages": [
			{
				"role": "assistant",
				"content": "calling tool",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "mcp.server.special:get_time",
							"arguments": "{}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call_1",
				"content": "12:00 PM"
			},
			{
				"role": "user",
				"content": "continue"
			}
		],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "mcp.server.special:get_time",
					"description": "Get current time"
				}
			},
			{
				"type": "function",
				"function": {
					"name": "clean_tool",
					"description": "Parameterless clean tool"
				}
			}
		],
		"tool_choice": {
			"type": "function",
			"function": {
				"name": "mcp.server.special:get_time"
			}
		}
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-6", []byte(inputJSON), false)

	// 1. Tool name in declarations must be sanitized
	tool0Name := gjson.GetBytes(result, "tools.0.name").String()
	if tool0Name != "mcp_server_special_get_time" {
		t.Fatalf("tools.0.name = %q, want mcp_server_special_get_time. Output: %s", tool0Name, result)
	}

	// 2. Parameterless tool must have a fallback input_schema object
	tool0Schema := gjson.GetBytes(result, "tools.0.input_schema")
	if !tool0Schema.Exists() || tool0Schema.Get("type").String() != "object" {
		t.Fatalf("tools.0.input_schema = %s, want object schema. Output: %s", tool0Schema, result)
	}
	tool1Schema := gjson.GetBytes(result, "tools.1.input_schema")
	if !tool1Schema.Exists() || tool1Schema.Get("type").String() != "object" {
		t.Fatalf("tools.1.input_schema = %s, want object schema. Output: %s", tool1Schema, result)
	}

	// 3. Historical tool_use name in assistant turn must be sanitized
	toolUseName := gjson.GetBytes(result, "messages.0.content.1.name").String()
	if toolUseName != "mcp_server_special_get_time" {
		t.Fatalf("messages.0.content.1.name = %q, want mcp_server_special_get_time. Output: %s", toolUseName, result)
	}

	// 4. Tool choice function name must be sanitized
	toolChoiceName := gjson.GetBytes(result, "tool_choice.name").String()
	if toolChoiceName != "mcp_server_special_get_time" {
		t.Fatalf("tool_choice.name = %q, want mcp_server_special_get_time. Output: %s", toolChoiceName, result)
	}
}

// TestConvertOpenAIRequestToClaudeDropsEmptyTextBlocks pins that empty and
// whitespace-only text never becomes a Claude block: Anthropic rejects a
// cache_control breakpoint on an empty text block, and the block carries nothing.
func TestConvertOpenAIRequestToClaudeDropsEmptyTextBlocks(t *testing.T) {
	t.Run("blank system and message text are dropped", func(t *testing.T) {
		raw := []byte(`{"model":"claude-test","messages":[
			{"role":"system","content":[{"type":"text","text":"kept"},{"type":"text","text":"   "}]},
			{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":""}]},
			{"role":"user","content":"   "}
		]}`)
		out := ConvertOpenAIRequestToClaude("claude-test", raw, false)

		system := gjson.GetBytes(out, "system")
		if got := system.Get("#").Int(); got != 1 {
			t.Fatalf("system blocks = %d, want 1: %s", got, out)
		}
		if got := system.Get("0.text").String(); got != "kept" {
			t.Fatalf("system.0.text = %q, want kept", got)
		}
		// The turn itself is kept with no blocks (upstream parity: the guard only
		// stops the block from being built), so the observable result is that no
		// empty text block reaches Claude.
		if got := gjson.GetBytes(out, "messages.1.content.#").Int(); got != 0 {
			t.Fatalf("whitespace-only turn kept %d blocks, want 0: %s", got, out)
		}
		content := gjson.GetBytes(out, "messages.0.content")
		if got := content.Get("#").Int(); got != 1 {
			t.Fatalf("message blocks = %d, want 1: %s", got, out)
		}
		if got := content.Get("0.text").String(); got != "hello" {
			t.Fatalf("message text = %q, want hello", got)
		}
	})

	t.Run("blank tool result text is dropped", func(t *testing.T) {
		raw := []byte(`{"model":"claude-test","messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":"ok","tool_calls":[{"id":"call_1","type":"function","function":{"name":"tool","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"   "},{"type":"text","text":"result"}]}
		]}`)
		out := ConvertOpenAIRequestToClaude("claude-test", raw, false)
		found := false
		gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
			msg.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() != "tool_result" {
					return true
				}
				found = true
				parts := part.Get("content")
				if parts.IsArray() && parts.Get("#").Int() != 1 {
					t.Fatalf("tool_result blocks = %s, want one non-empty block: %s", parts.Raw, out)
				}
				if parts.IsArray() && strings.TrimSpace(parts.Get("0.text").String()) != "result" {
					t.Fatalf("tool_result text = %q, want result: %s", parts.Get("0.text").String(), out)
				}
				return false
			})
			return true
		})
		if !found {
			t.Fatalf("expected a tool_result block: %s", out)
		}
	})
}
