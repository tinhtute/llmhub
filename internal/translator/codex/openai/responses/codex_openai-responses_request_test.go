package responses

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertSystemRoleToDeveloper_BasicConversion tests the basic system -> developer role conversion
func TestConvertSystemRoleToDeveloper_BasicConversion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are a pirate."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Say hello."}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that system role was converted to developer
	firstItemRole := gjson.Get(outputStr, "input.0.role")
	if firstItemRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstItemRole.String())
	}

	// Check that user role remains unchanged
	secondItemRole := gjson.Get(outputStr, "input.1.role")
	if secondItemRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", secondItemRole.String())
	}

	// Check content is preserved
	firstItemContent := gjson.Get(outputStr, "input.0.content.0.text")
	if firstItemContent.String() != "You are a pirate." {
		t.Errorf("Expected content 'You are a pirate.', got '%s'", firstItemContent.String())
	}
}

// TestConvertSystemRoleToDeveloper_MultipleSystemMessages tests conversion with multiple system messages
func TestConvertSystemRoleToDeveloper_MultipleSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "Be concise."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that both system roles were converted
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "developer" {
		t.Errorf("Expected second role 'developer', got '%s'", secondRole.String())
	}

	// Check that user role is unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "user" {
		t.Errorf("Expected third role 'user', got '%s'", thirdRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_NoSystemMessages tests that requests without system messages are unchanged
func TestConvertSystemRoleToDeveloper_NoSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi there!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that user and assistant roles are unchanged
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "assistant" {
		t.Errorf("Expected role 'assistant', got '%s'", secondRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_EmptyInput tests that empty input arrays are handled correctly
func TestConvertSystemRoleToDeveloper_EmptyInput(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": []
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that input is still an empty array
	inputArray := gjson.Get(outputStr, "input")
	if !inputArray.IsArray() {
		t.Error("Input should still be an array")
	}
	if len(inputArray.Array()) != 0 {
		t.Errorf("Expected empty array, got %d items", len(inputArray.Array()))
	}
}

// TestConvertSystemRoleToDeveloper_NoInputField tests that requests without input field are unchanged
func TestConvertSystemRoleToDeveloper_NoInputField(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that other fields are still set correctly
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true by conversion")
	}

	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be set to false by conversion")
	}
}

// TestConvertOpenAIResponsesRequestToCodex_OriginalIssue tests the exact issue reported by the user
func TestConvertOpenAIResponsesRequestToCodex_OriginalIssue(t *testing.T) {
	// This is the exact input that was failing with "System messages are not allowed"
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": "You are a pirate. Always respond in pirate speak."
			},
			{
				"type": "message",
				"role": "user",
				"content": "Say hello."
			}
		],
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify system role was converted to developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstRole.String())
	}

	// Verify stream was set to true (as required by Codex)
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true")
	}

	// Verify other required fields for Codex
	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be false")
	}

	parallelCalls := gjson.Get(outputStr, "parallel_tool_calls")
	if !parallelCalls.Bool() {
		t.Error("parallel_tool_calls should be true")
	}

	include := gjson.Get(outputStr, "include")
	if !include.IsArray() || len(include.Array()) != 1 {
		t.Error("include should be an array with one element")
	} else if include.Array()[0].String() != "reasoning.encrypted_content" {
		t.Errorf("Expected include[0] to be 'reasoning.encrypted_content', got '%s'", include.Array()[0].String())
	}
}

// TestConvertSystemRoleToDeveloper_AssistantRole tests that assistant role is preserved
func TestConvertSystemRoleToDeveloper_AssistantRole(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check system -> developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	// Check user unchanged
	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "user" {
		t.Errorf("Expected second role 'user', got '%s'", secondRole.String())
	}

	// Check assistant unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "assistant" {
		t.Errorf("Expected third role 'assistant', got '%s'", thirdRole.String())
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesWebSearchPreview(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4-mini",
		"input": "find latest OpenAI model news",
		"tools": [
			{"type": "web_search_preview_2025_03_11"}
		],
		"tool_choice": {
			"type": "allowed_tools",
			"tools": [
				{"type": "web_search_preview"},
				{"type": "web_search_preview_2025_03_11"}
			]
		}
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4-mini", inputJSON, false)

	if got := gjson.GetBytes(output, "tools.0.type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want %q: %s", got, "web_search", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q, want %q: %s", got, "allowed_tools", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.tools.0.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.tools.0.type = %q, want %q: %s", got, "web_search", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.tools.1.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.tools.1.type = %q, want %q: %s", got, "web_search", string(output))
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesTopLevelToolChoicePreviewAlias(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4-mini",
		"input": "find latest OpenAI model news",
		"tool_choice": {"type": "web_search_preview_2025_03_11"}
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4-mini", inputJSON, false)

	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.type = %q, want %q: %s", got, "web_search", string(output))
	}
}

func TestUserFieldDeletion(t *testing.T) {
	inputJSON := []byte(`{  
		"model": "gpt-5.2",  
		"user": "test-user",  
		"input": [{"role": "user", "content": "Hello"}]  
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify user field is deleted
	userField := gjson.Get(outputStr, "user")
	if userField.Exists() {
		t.Errorf("user field should be deleted, but it was found with value: %s", userField.Raw)
	}
}

func TestContextManagementCompactionCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"context_management": [
			{
				"type": "compaction",
				"compact_threshold": 12000
			}
		],
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "context_management").Exists() {
		t.Fatalf("context_management should be removed for Codex compatibility")
	}
	if gjson.Get(outputStr, "truncation").Exists() {
		t.Fatalf("truncation should be removed for Codex compatibility")
	}
}

func TestTruncationRemovedForCodexCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"truncation": "disabled",
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "truncation").Exists() {
		t.Fatalf("truncation should be removed for Codex compatibility")
	}
}

func TestPromptCacheRetentionRemovedForCodexCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"prompt_cache_retention": "24h",
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)

	if gjson.GetBytes(output, "prompt_cache_retention").Exists() {
		t.Fatalf("prompt_cache_retention should be removed for Codex compatibility: %s", output)
	}
}

func TestConvertOpenAIResponsesRequestToCodex_DeclarationTablePromotesAdditionalAndFlattensNamespace(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"tools":[
			{"type":"namespace","name":"repo","tools":[
				{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}},
				{"type":"custom","name":"patch"},
				{"type":"function","name":"mcp__stable"}
			]}
		],
		"input":[
			{"type":"additional_tools","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":"hello"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	wantNames := []string{"repo__read", "repo__patch", "mcp__stable", "shell"}
	for i, want := range wantNames {
		if got := gjson.GetBytes(output, "tools."+strconv.Itoa(i)+".name").String(); got != want {
			t.Fatalf("tools[%d].name = %q, want %q: %s", i, got, want, output)
		}
	}
	if got := gjson.GetBytes(output, "input.#").Int(); got != 1 {
		t.Fatalf("input count = %d, want 1 after additional_tools promotion: %s", got, output)
	}
	if got := gjson.GetBytes(output, "tools.0.parameters.properties.path.type").String(); got != "string" {
		t.Fatalf("function schema was not preserved: %s", output)
	}
}

// Ported from upstream CLIProxyAPI commit 859c4865 (codex service_tier
// normalization incl. ultrafast, issue #5924).

func TestConvertOpenAIResponsesRequestToCodex_ServiceTier(t *testing.T) {
	tests := []struct {
		name       string
		tierJSON   string
		wantExists bool
		wantTier   string
	}{
		{
			name:       "priority preserved",
			tierJSON:   `"priority"`,
			wantExists: true,
			wantTier:   "priority",
		},
		{
			name:       "priority case insensitive and trimmed",
			tierJSON:   `" Priority "`,
			wantExists: true,
			wantTier:   "priority",
		},
		{
			name:       "fast normalized to priority",
			tierJSON:   `"fast"`,
			wantExists: true,
			wantTier:   "priority",
		},
		{
			name:       "ultrafast preserved",
			tierJSON:   `"ultrafast"`,
			wantExists: true,
			wantTier:   "ultrafast",
		},
		{
			name:       "ultrafast case insensitive and trimmed",
			tierJSON:   `" UltraFast "`,
			wantExists: true,
			wantTier:   "ultrafast",
		},
		{
			name:       "standard stripped",
			tierJSON:   `"standard"`,
			wantExists: false,
		},
		{
			name:       "default stripped",
			tierJSON:   `"default"`,
			wantExists: false,
		},
		{
			name:       "flex stripped",
			tierJSON:   `"flex"`,
			wantExists: false,
		},
		{
			name:       "non-string stripped",
			tierJSON:   `123`,
			wantExists: false,
		},
		{
			name:       "null stripped",
			tierJSON:   `null`,
			wantExists: false,
		},
		{
			name:       "bool stripped",
			tierJSON:   `true`,
			wantExists: false,
		},
		{
			name:       "empty string stripped",
			tierJSON:   `""`,
			wantExists: false,
		},
		{
			name:       "whitespace string stripped",
			tierJSON:   `"   "`,
			wantExists: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := []byte(fmt.Sprintf(`{"model":"gpt-5.6","service_tier":%s,"input":[{"type":"message","role":"user","content":"hello"}]}`, tt.tierJSON))
			output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", inputJSON, true)
			res := gjson.GetBytes(output, "service_tier")
			if res.Exists() != tt.wantExists {
				t.Fatalf("service_tier exists = %v, want %v; output: %s", res.Exists(), tt.wantExists, string(output))
			}
			if tt.wantExists && res.String() != tt.wantTier {
				t.Fatalf("service_tier = %q, want %q; output: %s", res.String(), tt.wantTier, string(output))
			}
		})
	}
}

// Ported from upstream CLIProxyAPI commit 3662d153 (strip prompt_cache_breakpoint
// from input items and function call outputs, issue #5922).
func TestStripCodexResponsesCacheBreakpoints_FunctionCallOutputParts(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "function_call",
				"name": "shell",
				"call_id": "call_abc",
				"arguments": "{}"
			},
			{
				"type": "function_call_output",
				"call_id": "call_abc",
				"output": [
					{
						"type": "input_text",
						"text": "tool output",
						"prompt_cache_breakpoint": {"mode": "explicit"}
					}
				]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if strings.Contains(outputStr, "prompt_cache_breakpoint") {
		t.Fatalf("prompt_cache_breakpoint should not exist in the output JSON")
	}
	if gjson.Get(outputStr, "input.1.output.0.text").String() != "tool output" {
		t.Fatalf("function_call_output text should be preserved")
	}
}

func TestStripCodexResponsesCacheBreakpoints_ItemLevel(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "hi"}],
				"prompt_cache_breakpoint": {"mode": "explicit"}
			},
			{
				"type": "function_call_output",
				"call_id": "call_abc",
				"output": "plain string output",
				"prompt_cache_breakpoint": {"mode": "explicit"}
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if strings.Contains(outputStr, "prompt_cache_breakpoint") {
		t.Fatalf("prompt_cache_breakpoint should not exist in the output JSON")
	}
	if gjson.Get(outputStr, "input.0.content.0.text").String() != "hi" {
		t.Fatalf("message content should be preserved")
	}
	if gjson.Get(outputStr, "input.1.output").String() != "plain string output" {
		t.Fatalf("string output should be preserved")
	}
}

func TestStripCodexResponsesCacheBreakpoints_CombinedItemAndPartLevel(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "function_call_output",
				"call_id": "call_123",
				"prompt_cache_breakpoint": {"mode": "explicit"},
				"output": [
					{
						"type": "input_text",
						"text": "result part 1",
						"prompt_cache_breakpoint": {"mode": "explicit"}
					},
					{
						"type": "input_text",
						"text": "result part 2"
					}
				]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if strings.Contains(outputStr, "prompt_cache_breakpoint") {
		t.Fatalf("prompt_cache_breakpoint should not exist in the output JSON")
	}
	if gjson.Get(outputStr, "input.#").Int() != 1 {
		t.Fatalf("expected 1 input item, got %d", gjson.Get(outputStr, "input.#").Int())
	}
	if gjson.Get(outputStr, "input.0.call_id").String() != "call_123" {
		t.Fatalf("call_id should be preserved")
	}
	if gjson.Get(outputStr, "input.0.output.0.text").String() != "result part 1" {
		t.Fatalf("output part 0 text should be preserved")
	}
	if gjson.Get(outputStr, "input.0.output.1.text").String() != "result part 2" {
		t.Fatalf("output part 1 text should be preserved")
	}
}

// TestConvertOpenAIResponsesRequestToCodex_NormalizesEmptyFunctionCallArguments
// covers the parameter-less tool call shape that strict Codex Responses
// upstreams reject with "`arguments` must be valid JSON": only blank string
// arguments on function_call history items become "{}", everything else is
// preserved byte-for-byte in intent.
func TestConvertOpenAIResponsesRequestToCodex_NormalizesEmptyFunctionCallArguments(t *testing.T) {
	inputJSON := []byte(`{"model":"gpt-5.6","input":[
		{"type":"function_call","id":"fc_empty","call_id":"call_empty","name":"create_worktree","arguments":""},
		{"type":"function_call","id":"fc_blank","call_id":"call_blank","name":"list_artifacts","arguments":"   \t\n"},
		{"type":"function_call","id":"fc_kept","call_id":"call_kept","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
		{"type":"function_call","id":"fc_broken","call_id":"call_broken","name":"exec_command","arguments":"not-json"},
		{"type":"function_call","id":"fc_missing","call_id":"call_missing","name":"no_args"},
		{"type":"function_call","id":"fc_null","call_id":"call_null","name":"null_args","arguments":null},
		{"type":"function_call","id":"fc_num","call_id":"call_num","name":"num_args","arguments":123},
		{"type":"function_call_output","call_id":"call_empty","output":"worktree created"},
		{"type":"custom_tool_call","id":"ctc_1","call_id":"call_custom","name":"apply_patch","input":"exact patch"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]}`)
	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", inputJSON, true)
	for index, want := range []string{"{}", "{}", `{"cmd":"pwd"}`, "not-json"} {
		if got := gjson.GetBytes(output, "input."+strconv.Itoa(index)+".arguments").String(); got != want {
			t.Fatalf("input.%d.arguments = %q, want %q; output: %s", index, got, want, string(output))
		}
	}
	if gjson.GetBytes(output, "input.4.arguments").Exists() {
		t.Fatalf("input.4 (missing arguments) should not gain arguments field; output: %s", string(output))
	}
	if got := gjson.GetBytes(output, "input.5.arguments").Type; got != gjson.Null {
		t.Fatalf("input.5.arguments type = %v, want null; output: %s", got, string(output))
	}
	if got := gjson.GetBytes(output, "input.6.arguments").Int(); got != 123 {
		t.Fatalf("input.6.arguments = %d, want 123; output: %s", got, string(output))
	}
	if got := gjson.GetBytes(output, "input.7.type").String(); got != "function_call_output" || gjson.GetBytes(output, "input.7.output").String() != "worktree created" {
		t.Fatalf("input.7 should be function_call_output with unchanged output; output: %s", string(output))
	}
	if got := gjson.GetBytes(output, "input.8.type").String(); got != "custom_tool_call" {
		t.Fatalf("input.8.type = %q, want custom_tool_call; output: %s", got, string(output))
	}
	if got := gjson.GetBytes(output, "input.8.input").String(); got != "exact patch" {
		t.Fatalf("input.8.input = %q, want exact patch; output: %s", got, string(output))
	}
	if gjson.GetBytes(output, "input.8.arguments").Exists() {
		t.Fatalf("custom_tool_call must not gain arguments; output: %s", string(output))
	}
	if got := gjson.GetBytes(output, "input.9.type").String(); got != "message" {
		t.Fatalf("input.9.type = %q, want message; output: %s", got, string(output))
	}
}

// An explicit parallel_tool_calls=false must survive translation: Codex accepts
// the field at either value, and the client asked for serial tool calls.
func TestConvertOpenAIResponsesRequestToCodexParallelToolCalls(t *testing.T) {
	tests := []struct {
		name     string
		field    string
		wantType gjson.Type
	}{
		{name: "explicit false preserved", field: `,"parallel_tool_calls":false`, wantType: gjson.False},
		{name: "explicit true preserved", field: `,"parallel_tool_calls":true`, wantType: gjson.True},
		{name: "explicit null defaults to true", field: `,"parallel_tool_calls":null`, wantType: gjson.True},
		{name: "omitted defaults to true", field: ``, wantType: gjson.True},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.2","input":[{"type":"message","role":"user","content":"hi"}]` + tt.field + `}`)
			out := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", body, false)
			got := gjson.GetBytes(out, "parallel_tool_calls")
			if got.Type != tt.wantType {
				t.Fatalf("parallel_tool_calls = %v (%s), want %s; payload=%s", got.Value(), got.Type, tt.wantType, out)
			}
		})
	}
}
