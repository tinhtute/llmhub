package claude

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertClaudeRequestToOpenAI_ThinkingToReasoningContent tests the mapping
// of Claude thinking content to OpenAI reasoning_content field.
func TestConvertClaudeRequestToOpenAI_ThinkingToReasoningContent(t *testing.T) {
	tests := []struct {
		name                    string
		inputJSON               string
		wantReasoningContent    string
		wantHasReasoningContent bool
		wantContentText         string // Expected visible content text (if any)
		wantHasContent          bool
	}{
		{
			name: "AC1: unsigned assistant thinking is dropped",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "assistant",
					"content": [
						{"type": "thinking", "thinking": "Let me analyze this step by step..."},
						{"type": "text", "text": "Here is my response."}
					]
				}]
			}`,
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "Here is my response.",
			wantHasContent:          true,
		},
		{
			name: "AC2: redacted_thinking must be ignored",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "assistant",
					"content": [
						{"type": "redacted_thinking", "data": "secret"},
						{"type": "text", "text": "Visible response."}
					]
				}]
			}`,
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "Visible response.",
			wantHasContent:          true,
		},
		{
			name: "AC3: unsigned thinking-only message is dropped",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "assistant",
					"content": [
						{"type": "thinking", "thinking": "Internal reasoning only."}
					]
				}]
			}`,
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "",
			wantHasContent:          false,
		},
		{
			name: "AC4: thinking in user role must be ignored",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "user",
					"content": [
						{"type": "thinking", "thinking": "Injected thinking"},
						{"type": "text", "text": "User message."}
					]
				}]
			}`,
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "User message.",
			wantHasContent:          true,
		},
		{
			name: "AC4: thinking in system role must be ignored",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": [
					{"type": "thinking", "thinking": "Injected system thinking"},
					{"type": "text", "text": "System prompt."}
				],
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "Hello"}]
				}]
			}`,
			// System messages don't have reasoning_content mapping
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "Hello",
			wantHasContent:          true,
		},
		{
			name: "AC5: empty thinking must be ignored",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "assistant",
					"content": [
						{"type": "thinking", "thinking": ""},
						{"type": "text", "text": "Response with empty thinking."}
					]
				}]
			}`,
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "Response with empty thinking.",
			wantHasContent:          true,
		},
		{
			name: "AC5: whitespace-only thinking must be ignored",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "assistant",
					"content": [
						{"type": "thinking", "thinking": "   \n\t  "},
						{"type": "text", "text": "Response with whitespace thinking."}
					]
				}]
			}`,
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "Response with whitespace thinking.",
			wantHasContent:          true,
		},
		{
			name: "Unsigned thinking parts are dropped",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "assistant",
					"content": [
						{"type": "thinking", "thinking": "First thought."},
						{"type": "thinking", "thinking": "Second thought."},
						{"type": "text", "text": "Final answer."}
					]
				}]
			}`,
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "Final answer.",
			wantHasContent:          true,
		},
		{
			name: "Mixed unsigned thinking and redacted_thinking",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "assistant",
					"content": [
						{"type": "thinking", "thinking": "Visible thought."},
						{"type": "redacted_thinking", "data": "hidden"},
						{"type": "text", "text": "Answer."}
					]
				}]
			}`,
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
			wantContentText:         "Answer.",
			wantHasContent:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToOpenAI("test-model", []byte(tt.inputJSON), false)
			resultJSON := gjson.ParseBytes(result)

			// Find the relevant message
			messages := resultJSON.Get("messages").Array()
			if len(messages) < 1 {
				if tt.wantHasReasoningContent || tt.wantHasContent {
					t.Fatalf("Expected at least 1 message, got %d", len(messages))
				}
				return
			}

			// Check the last non-system message
			var targetMsg gjson.Result
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Get("role").String() != "system" {
					targetMsg = messages[i]
					break
				}
			}

			// Check reasoning_content
			gotReasoningContent := targetMsg.Get("reasoning_content").String()
			gotHasReasoningContent := targetMsg.Get("reasoning_content").Exists()

			if gotHasReasoningContent != tt.wantHasReasoningContent {
				t.Errorf("reasoning_content existence = %v, want %v", gotHasReasoningContent, tt.wantHasReasoningContent)
			}

			if gotReasoningContent != tt.wantReasoningContent {
				t.Errorf("reasoning_content = %q, want %q", gotReasoningContent, tt.wantReasoningContent)
			}

			// Check content
			content := targetMsg.Get("content")
			// content has meaningful content if it's a non-empty array, or a non-empty string
			var gotHasContent bool
			switch {
			case content.IsArray():
				gotHasContent = len(content.Array()) > 0
			case content.Type == gjson.String:
				gotHasContent = content.String() != ""
			default:
				gotHasContent = false
			}

			if gotHasContent != tt.wantHasContent {
				t.Errorf("content existence = %v, want %v", gotHasContent, tt.wantHasContent)
			}

			if tt.wantHasContent && tt.wantContentText != "" {
				// Find text content
				var foundText string
				content.ForEach(func(_, v gjson.Result) bool {
					if v.Get("type").String() == "text" {
						foundText = v.Get("text").String()
						return false
					}
					return true
				})
				if foundText != tt.wantContentText {
					t.Errorf("content text = %q, want %q", foundText, tt.wantContentText)
				}
			}
		})
	}
}

// TestConvertClaudeRequestToOpenAI_UnsignedThinkingOnlyMessageDropped verifies
// that unsigned Claude thinking is not migrated into GPT reasoning state.
func TestConvertClaudeRequestToOpenAI_UnsignedThinkingOnlyMessageDropped(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "user",
				"content": [{"type": "text", "text": "What is 2+2?"}]
			},
			{
				"role": "assistant",
				"content": [{"type": "thinking", "thinking": "Let me calculate: 2+2=4"}]
			},
			{
				"role": "user",
				"content": [{"type": "text", "text": "Thanks"}]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected unsigned thinking-only assistant message to be dropped, got %d. Messages: %v", len(messages), resultJSON.Get("messages").Raw)
	}
	for _, message := range messages {
		if message.Get("reasoning_content").Exists() {
			t.Fatalf("unsigned thinking should not produce reasoning_content. Messages: %v", resultJSON.Get("messages").Raw)
		}
	}
}

func TestConvertClaudeRequestToOpenAI_SignedThinkingCompatibility(t *testing.T) {
	tests := []struct {
		name                    string
		signature               string
		wantReasoningContent    string
		wantHasReasoningContent bool
	}{
		{
			name:                    "GPT-compatible signature keeps reasoning_content",
			signature:               validGPTChatReasoningSignature(),
			wantReasoningContent:    "provider state",
			wantHasReasoningContent: true,
		},
		{
			name:                    "Claude signature drops reasoning_content",
			signature:               "claude#EjQ=",
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
		},
		{
			name:                    "Gemini signature drops reasoning_content",
			signature:               "gemini#EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA",
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
		},
		{
			name:                    "Unknown signature drops reasoning_content",
			signature:               "not-a-provider-signature",
			wantReasoningContent:    "",
			wantHasReasoningContent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := `{
				"model": "claude-3-opus",
				"messages": [{
					"role": "assistant",
					"content": [
						{"type": "thinking", "thinking": "provider state", "signature": "` + tt.signature + `"},
						{"type": "text", "text": "visible answer"}
					]
				}]
			}`

			result := ConvertClaudeRequestToOpenAI("gpt-5", []byte(inputJSON), false)
			assistantMsg := gjson.GetBytes(result, "messages.0")
			gotReasoningContent := assistantMsg.Get("reasoning_content").String()
			gotHasReasoningContent := assistantMsg.Get("reasoning_content").Exists()

			if gotHasReasoningContent != tt.wantHasReasoningContent {
				t.Fatalf("reasoning_content exists = %v, want %v. Output: %s", gotHasReasoningContent, tt.wantHasReasoningContent, string(result))
			}
			if gotReasoningContent != tt.wantReasoningContent {
				t.Fatalf("reasoning_content = %q, want %q. Output: %s", gotReasoningContent, tt.wantReasoningContent, string(result))
			}
			if got := assistantMsg.Get("content.0.text").String(); got != "visible answer" {
				t.Fatalf("visible content = %q, want visible answer. Output: %s", got, string(result))
			}
		})
	}
}

func validGPTChatReasoningSignature() string {
	raw := make([]byte, 1+8+16+16+32)
	raw[0] = 0x80
	raw[8] = 1
	for i := 9; i < len(raw); i++ {
		raw[i] = byte(i)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func TestConvertClaudeRequestToOpenAI_SystemMessageScenarios(t *testing.T) {
	tests := []struct {
		name        string
		inputJSON   string
		wantHasSys  bool
		wantSysText string
	}{
		{
			name: "No system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasSys: false,
		},
		{
			name: "Empty string system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasSys: false,
		},
		{
			name: "String system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "Be helpful",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasSys:  true,
			wantSysText: "Be helpful",
		},
		{
			name: "Array system field with text",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": [{"type": "text", "text": "Array system"}],
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasSys:  true,
			wantSysText: "Array system",
		},
		{
			name: "Array system field with multiple text blocks",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": [
					{"type": "text", "text": "Block 1"},
					{"type": "text", "text": "Block 2"}
				],
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasSys:  true,
			wantSysText: "Block 2", // We will update the test logic to check all blocks or specifically the second one
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToOpenAI("test-model", []byte(tt.inputJSON), false)
			resultJSON := gjson.ParseBytes(result)
			messages := resultJSON.Get("messages").Array()

			hasSys := false
			var sysMsg gjson.Result
			if len(messages) > 0 && messages[0].Get("role").String() == "system" {
				hasSys = true
				sysMsg = messages[0]
			}

			if hasSys != tt.wantHasSys {
				t.Errorf("got hasSystem = %v, want %v", hasSys, tt.wantHasSys)
			}

			if tt.wantHasSys {
				// Check content - it could be string or array in OpenAI
				content := sysMsg.Get("content")
				var gotText string
				if content.IsArray() {
					arr := content.Array()
					if len(arr) > 0 {
						// Get the last element's text for validation
						gotText = arr[len(arr)-1].Get("text").String()
					}
				} else {
					gotText = content.String()
				}

				if tt.wantSysText != "" && gotText != tt.wantSysText {
					t.Errorf("got system text = %q, want %q", gotText, tt.wantSysText)
				}
			}
		})
	}
}

func TestConvertClaudeRequestToOpenAI_ToolResultOrderAndContent(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "call_1", "name": "do_work", "input": {"a": 1}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "before"},
					{"type": "tool_result", "tool_use_id": "call_1", "content": [{"type":"text","text":"tool ok"}]},
					{"type": "text", "text": "after"}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	// OpenAI requires: tool messages MUST immediately follow assistant(tool_calls).
	// Correct order: assistant(tool_calls) + tool(result) + user(before+after)
	if len(messages) != 3 {
		t.Fatalf("Expected 3 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	if messages[0].Get("role").String() != "assistant" || !messages[0].Get("tool_calls").Exists() {
		t.Fatalf("Expected messages[0] to be assistant tool_calls, got %s: %s", messages[0].Get("role").String(), messages[0].Raw)
	}

	// tool message MUST immediately follow assistant(tool_calls) per OpenAI spec
	if messages[1].Get("role").String() != "tool" {
		t.Fatalf("Expected messages[1] to be tool (must follow tool_calls), got %s", messages[1].Get("role").String())
	}
	if got := messages[1].Get("tool_call_id").String(); got != "call_1" {
		t.Fatalf("Expected tool_call_id %q, got %q", "call_1", got)
	}
	if got := messages[1].Get("content").String(); got != "tool ok" {
		t.Fatalf("Expected tool content %q, got %q", "tool ok", got)
	}

	// User message comes after tool message
	if messages[2].Get("role").String() != "user" {
		t.Fatalf("Expected messages[2] to be user, got %s", messages[2].Get("role").String())
	}
	// User message should contain both "before" and "after" text
	if got := messages[2].Get("content.0.text").String(); got != "before" {
		t.Fatalf("Expected user text[0] %q, got %q", "before", got)
	}
	if got := messages[2].Get("content.1.text").String(); got != "after" {
		t.Fatalf("Expected user text[1] %q, got %q", "after", got)
	}
}

func TestConvertClaudeRequestToOpenAI_ToolResultObjectContent(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "call_1", "name": "do_work", "input": {"a": 1}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "call_1", "content": {"foo": "bar"}}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	// assistant(tool_calls) + tool(result)
	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	if messages[1].Get("role").String() != "tool" {
		t.Fatalf("Expected messages[1] to be tool, got %s", messages[1].Get("role").String())
	}

	toolContent := messages[1].Get("content").String()
	parsed := gjson.Parse(toolContent)
	if parsed.Get("foo").String() != "bar" {
		t.Fatalf("Expected tool content JSON foo=bar, got %q", toolContent)
	}
}

func TestConvertClaudeRequestToOpenAI_ToolResultTextAndImageContent(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "call_1", "name": "do_work", "input": {"a": 1}}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "call_1",
						"content": [
							{"type": "text", "text": "tool ok"},
							{
								"type": "image",
								"source": {
									"type": "base64",
									"media_type": "image/png",
									"data": "iVBORw0KGgoAAAANSUhEUg=="
								}
							}
						]
					}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	toolContent := messages[1].Get("content")
	if !toolContent.IsArray() {
		t.Fatalf("Expected tool content array, got %s", toolContent.Raw)
	}
	if got := toolContent.Get("0.type").String(); got != "text" {
		t.Fatalf("Expected first tool content type %q, got %q", "text", got)
	}
	if got := toolContent.Get("0.text").String(); got != "tool ok" {
		t.Fatalf("Expected first tool content text %q, got %q", "tool ok", got)
	}
	if got := toolContent.Get("1.type").String(); got != "image_url" {
		t.Fatalf("Expected second tool content type %q, got %q", "image_url", got)
	}
	if got := toolContent.Get("1.image_url.url").String(); got != "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==" {
		t.Fatalf("Unexpected image_url: %q", got)
	}
}

func TestConvertClaudeRequestToOpenAI_ToolResultURLImageOnly(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "call_1", "name": "do_work", "input": {"a": 1}}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "call_1",
						"content": {
							"type": "image",
							"source": {
								"type": "url",
								"url": "https://example.com/tool.png"
							}
						}
					}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	toolContent := messages[1].Get("content")
	if !toolContent.IsArray() {
		t.Fatalf("Expected tool content array, got %s", toolContent.Raw)
	}
	if got := toolContent.Get("0.type").String(); got != "image_url" {
		t.Fatalf("Expected tool content type %q, got %q", "image_url", got)
	}
	if got := toolContent.Get("0.image_url.url").String(); got != "https://example.com/tool.png" {
		t.Fatalf("Unexpected image_url: %q", got)
	}
}

func TestConvertClaudeRequestToOpenAI_AssistantTextToolUseTextOrder(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": "pre"},
					{"type": "tool_use", "id": "call_1", "name": "do_work", "input": {"a": 1}},
					{"type": "text", "text": "post"}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	// New behavior: content + tool_calls unified in single assistant message
	// Expect: assistant(content[pre,post] + tool_calls)
	if len(messages) != 1 {
		t.Fatalf("Expected 1 message, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	assistantMsg := messages[0]
	if assistantMsg.Get("role").String() != "assistant" {
		t.Fatalf("Expected messages[0] to be assistant, got %s", assistantMsg.Get("role").String())
	}

	// Should have both content and tool_calls in same message
	if !assistantMsg.Get("tool_calls").Exists() {
		t.Fatalf("Expected assistant message to have tool_calls")
	}
	if got := assistantMsg.Get("tool_calls.0.id").String(); got != "call_1" {
		t.Fatalf("Expected tool_call id %q, got %q", "call_1", got)
	}
	if got := assistantMsg.Get("tool_calls.0.function.name").String(); got != "do_work" {
		t.Fatalf("Expected tool_call name %q, got %q", "do_work", got)
	}

	// Content should have both pre and post text
	if got := assistantMsg.Get("content.0.text").String(); got != "pre" {
		t.Fatalf("Expected content[0] text %q, got %q", "pre", got)
	}
	if got := assistantMsg.Get("content.1.text").String(); got != "post" {
		t.Fatalf("Expected content[1] text %q, got %q", "post", got)
	}
}

func TestConvertClaudeRequestToOpenAI_AssistantThinkingToolUseThinkingSplit(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "t1"},
					{"type": "text", "text": "pre"},
					{"type": "tool_use", "id": "call_1", "name": "do_work", "input": {"a": 1}},
					{"type": "thinking", "thinking": "t2"},
					{"type": "text", "text": "post"}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	// New behavior: all content, thinking, and tool_calls unified in single assistant message
	// Expect: assistant(content[pre,post] + tool_calls + reasoning_content[t1+t2])
	if len(messages) != 1 {
		t.Fatalf("Expected 1 message, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	assistantMsg := messages[0]
	if assistantMsg.Get("role").String() != "assistant" {
		t.Fatalf("Expected messages[0] to be assistant, got %s", assistantMsg.Get("role").String())
	}

	// Should have content with both pre and post
	if got := assistantMsg.Get("content.0.text").String(); got != "pre" {
		t.Fatalf("Expected content[0] text %q, got %q", "pre", got)
	}
	if got := assistantMsg.Get("content.1.text").String(); got != "post" {
		t.Fatalf("Expected content[1] text %q, got %q", "post", got)
	}

	// Should have tool_calls
	if !assistantMsg.Get("tool_calls").Exists() {
		t.Fatalf("Expected assistant message to have tool_calls")
	}

	if assistantMsg.Get("reasoning_content").Exists() {
		t.Fatalf("unsigned thinking should not produce reasoning_content: %s", assistantMsg.Raw)
	}
}

func TestConvertClaudeRequestToOpenAI_StopSequences(t *testing.T) {
	tests := []struct {
		name      string
		inputJSON string
		wantStop  []string
	}{
		{
			name: "single stop sequence is emitted as array",
			inputJSON: `{
				"model": "claude-3-opus",
				"stop_sequences": ["</block>"],
				"messages": [{"role": "user", "content": "hi"}]
			}`,
			wantStop: []string{"</block>"},
		},
		{
			name: "multiple stop sequences are emitted as array",
			inputJSON: `{
				"model": "claude-3-opus",
				"stop_sequences": ["stop1", "stop2"],
				"messages": [{"role": "user", "content": "hi"}]
			}`,
			wantStop: []string{"stop1", "stop2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := ConvertClaudeRequestToOpenAI("gpt-4o", []byte(tt.inputJSON), false)
			stopRes := gjson.GetBytes(output, "stop")
			if !stopRes.Exists() {
				t.Fatalf("expected 'stop' field in output, got: %s", string(output))
			}
			if !stopRes.IsArray() {
				t.Fatalf("expected 'stop' field to be JSON array, got: %s", stopRes.Raw)
			}
			items := stopRes.Array()
			if len(items) != len(tt.wantStop) {
				t.Fatalf("expected %d stop items, got %d (%v)", len(tt.wantStop), len(items), stopRes.Raw)
			}
			for i, want := range tt.wantStop {
				if items[i].String() != want {
					t.Errorf("stop[%d] = %q, want %q", i, items[i].String(), want)
				}
			}
		})
	}
}

func TestConvertClaudeRequestToOpenAI_StripsClaudeCodeAttribution(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"},
			{"type": "text", "text": "User system prompt"}
		],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`)

	output := ConvertClaudeRequestToOpenAI("gpt-5", inputJSON, false)
	messages := gjson.GetBytes(output, "messages").Array()
	if len(messages) == 0 || messages[0].Get("role").String() != "system" {
		t.Fatalf("Expected first message to be system, got: %s", gjson.GetBytes(output, "messages").Raw)
	}

	content := messages[0].Get("content").Array()
	if len(content) != 1 {
		t.Fatalf("Expected 1 system content item after attribution strip, got %d: %s", len(content), messages[0].Get("content").Raw)
	}
	if got := content[0].Get("text").String(); got != "User system prompt" {
		t.Fatalf("Unexpected system content: %q", got)
	}
}

// Ported from upstream 7def842554bc (preserve tool names in tool result
// messages): assistant tool_use parts feed a toolNameByID map and the
// converted role:"tool" messages carry the matching function name.
func TestConvertClaudeRequestToOpenAI_ToolResultPreservesFunctionName(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.8-flash",
		"max_tokens": 64,
		"tools": [
			{
				"name": "get_weather",
				"description": "Get weather",
				"input_schema": {
					"type": "object",
					"properties": {"city": {"type": "string"}},
					"required": ["city"]
				}
			},
			{
				"name": "get_time",
				"description": "Get time",
				"input_schema": {
					"type": "object",
					"properties": {"city": {"type": "string"}},
					"required": ["city"]
				}
			}
		],
		"messages": [
			{"role": "user", "content": "What's the weather and time in Jakarta?"},
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "toolu_01ABC", "name": "get_weather", "input": {"city": "Jakarta"}},
					{"type": "tool_use", "id": "toolu_02DEF", "name": "get_time", "input": {"city": "Jakarta"}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "toolu_01ABC", "content": "32C, humid"},
					{"type": "tool_result", "tool_use_id": "toolu_02DEF", "content": "12:00 PM"}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("gemini-3.8-flash", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	var toolMessages []gjson.Result
	for _, msg := range messages {
		if msg.Get("role").String() == "tool" {
			toolMessages = append(toolMessages, msg)
		}
	}

	if len(toolMessages) != 2 {
		t.Fatalf("expected 2 tool messages, got %d. Output: %s", len(toolMessages), result)
	}

	if got := toolMessages[0].Get("tool_call_id").String(); got != "toolu_01ABC" {
		t.Errorf("toolMessages[0].tool_call_id = %q, want %q", got, "toolu_01ABC")
	}
	if got := toolMessages[0].Get("name").String(); got != "get_weather" {
		t.Errorf("toolMessages[0].name = %q, want %q", got, "get_weather")
	}
	if got := toolMessages[0].Get("content").String(); got != "32C, humid" {
		t.Errorf("toolMessages[0].content = %q, want %q", got, "32C, humid")
	}

	if got := toolMessages[1].Get("tool_call_id").String(); got != "toolu_02DEF" {
		t.Errorf("toolMessages[1].tool_call_id = %q, want %q", got, "toolu_02DEF")
	}
	if got := toolMessages[1].Get("name").String(); got != "get_time" {
		t.Errorf("toolMessages[1].name = %q, want %q", got, "get_time")
	}
	if got := toolMessages[1].Get("content").String(); got != "12:00 PM" {
		t.Errorf("toolMessages[1].content = %q, want %q", got, "12:00 PM")
	}
}

func TestConvertClaudeRequestToOpenAI_ToolResultUnknownIDNoName(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.8-flash",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "orphan_call_1", "content": "result"}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("gemini-3.8-flash", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	toolMsg := resultJSON.Get("messages.0")

	if got := toolMsg.Get("role").String(); got != "tool" {
		t.Fatalf("expected tool role, got %q", got)
	}
	if got := toolMsg.Get("tool_call_id").String(); got != "orphan_call_1" {
		t.Errorf("tool_call_id = %q, want %q", got, "orphan_call_1")
	}
	if toolMsg.Get("name").Exists() {
		t.Errorf("expected no name for unknown tool_use_id, got %q", toolMsg.Get("name").String())
	}
}

// The following tests are ported from upstream CLIProxyAPI commit cc545cbf
// (align tool call messages and preserve ordering on ambiguous outputs,
// issue #5925).

func TestConvertClaudeRequestToOpenAI_ToolCallPairingByID(t *testing.T) {
	inputJSON := `{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Run analysis"}]},
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "call_1", "name": "fetch_data", "input": {"id": 123}},
					{"type": "tool_use", "id": "call_2", "name": "calc_metric", "input": {"scale": 1.5}}
				]
			},
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Waiting for results to continue"},
					{"type": "thinking", "thinking": "Thinking about next steps", "signature": "sig_abc"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Reminder: keep timeout short"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "call_2", "content": "metric_ok"},
					{"type": "tool_result", "tool_use_id": "call_1", "content": "data_ok"}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("deepseek-v4.1-flash", []byte(inputJSON), false)
	messages := gjson.GetBytes(result, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Tool results must be paired immediately following the assistant tool_calls message.
	// Expected:
	// messages[0]: user ("Run analysis")
	// messages[1]: assistant (tool_calls [call_1, call_2])
	// messages[2]: tool (tool_call_id: call_2 or call_1)
	// messages[3]: tool (tool_call_id: call_1 or call_2)
	// messages[4]: assistant (content: "Waiting for results to continue", reasoning_content: ...)
	// messages[5]: user ("Reminder: keep timeout short")
	if len(messages) != 6 {
		t.Fatalf("expected 6 messages, got %d: %s", len(messages), result)
	}
	if roles[1] != "assistant" || !messages[1].Get("tool_calls").Exists() {
		t.Fatalf("messages[1] must be assistant with tool_calls, got %s", roles[1])
	}
	if roles[2] != "tool" || roles[3] != "tool" {
		t.Fatalf("messages[2] and messages[3] must be tool messages immediately following assistant tool_calls, got roles: %v", roles)
	}
	toolIDs := []string{messages[2].Get("tool_call_id").String(), messages[3].Get("tool_call_id").String()}
	if toolIDs[0] != "call_2" || toolIDs[1] != "call_1" {
		t.Fatalf("expected tool messages to strictly preserve input relative order [call_2, call_1], got: %v", toolIDs)
	}
}

func TestConvertClaudeRequestToOpenAI_ToolCallPairing_OrphanAndIncompletePreserved(t *testing.T) {
	// Orphan tool results and incomplete histories must not be rearranged or guessed
	inputJSON := `{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Start"}]},
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "call_alpha", "name": "do_a", "input": {}},
					{"type": "tool_use", "id": "call_beta", "name": "do_b", "input": {}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Waiting on results"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "call_unmatched", "content": "orphan_result"}
				]
			}
		]
	}`

	result := ConvertClaudeRequestToOpenAI("deepseek-v4.1-flash", []byte(inputJSON), false)
	messages := gjson.GetBytes(result, "messages").Array()

	// Incomplete history (call_alpha and call_beta are unanswered) and orphan result
	// (call_unmatched has no matching assistant tool_calls) must remain untouched.
	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}
	// messages[0]: user
	// messages[1]: assistant (tool_calls [call_alpha, call_beta])
	// messages[2]: user ("Waiting on results")
	// messages[3]: tool (tool_call_id: call_unmatched)
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d: %s", len(messages), result)
	}
	if roles[1] != "assistant" || roles[2] != "user" || roles[3] != "tool" {
		t.Fatalf("expected untouched order [user, assistant, user, tool], got: %v", roles)
	}
}

// Ported from upstream CLIProxyAPI commit 49eec664 (tool choice fail-closed, issue #5957).
func TestConvertClaudeRequestToOpenAI_ToolChoice(t *testing.T) {
	t.Run("none does not become auto", func(t *testing.T) {
		inputJSON := `{
			"model": "gpt-5.4",
			"max_tokens": 64,
			"messages": [{"role": "user", "content": "Answer without calling tools."}],
			"tool_choice": {"type": "none"},
			"tools": [
				{"name": "tool_a", "description": "test", "input_schema": {"type": "object", "properties": {}}},
				{"name": "tool_b", "description": "test", "input_schema": {"type": "object", "properties": {}}}
			]
		}`
		result := ConvertClaudeRequestToOpenAI("gpt-5.4", []byte(inputJSON), false)
		gotToolChoice := gjson.GetBytes(result, "tool_choice").String()
		if gotToolChoice != "none" {
			t.Fatalf("expected tool_choice to be 'none', got %q. Output: %s", gotToolChoice, result)
		}
	})

	t.Run("disable_parallel_tool_use maps to parallel_tool_calls false", func(t *testing.T) {
		inputJSON := `{
			"model": "gpt-5.4",
			"max_tokens": 64,
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {"type": "auto", "disable_parallel_tool_use": true},
			"tools": [
				{"name": "tool_a", "description": "test", "input_schema": {"type": "object", "properties": {}}}
			]
		}`
		result := ConvertClaudeRequestToOpenAI("gpt-5.4", []byte(inputJSON), false)
		parallel := gjson.GetBytes(result, "parallel_tool_calls")
		if !parallel.Exists() || parallel.Bool() {
			t.Fatalf("expected parallel_tool_calls to be false, got %v. Output: %s", parallel.Value(), result)
		}
	})

	t.Run("unknown tool_choice type fails closed to none", func(t *testing.T) {
		inputJSON := `{
			"model": "gpt-5.4",
			"max_tokens": 64,
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {"type": "unknown_future_restriction"},
			"tools": [
				{"name": "tool_a", "description": "test", "input_schema": {"type": "object", "properties": {}}}
			]
		}`
		result := ConvertClaudeRequestToOpenAI("gpt-5.4", []byte(inputJSON), false)
		gotToolChoice := gjson.GetBytes(result, "tool_choice").String()
		if gotToolChoice != "none" {
			t.Fatalf("expected tool_choice to fail closed to 'none', got %q. Output: %s", gotToolChoice, result)
		}
	})

	t.Run("tool choice with empty name fails closed to none", func(t *testing.T) {
		inputJSON := `{
			"model": "gpt-5.4",
			"max_tokens": 64,
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": {"type": "tool", "name": ""},
			"tools": [
				{"name": "tool_a", "description": "test", "input_schema": {"type": "object", "properties": {}}}
			]
		}`
		result := ConvertClaudeRequestToOpenAI("gpt-5.4", []byte(inputJSON), false)
		gotToolChoice := gjson.GetBytes(result, "tool_choice").String()
		if gotToolChoice != "none" {
			t.Fatalf("expected tool_choice to fail closed to 'none', got %q. Output: %s", gotToolChoice, result)
		}
	})

	t.Run("tool_choice null does not set tool_choice", func(t *testing.T) {
		inputJSON := `{
			"model": "gpt-5.4",
			"max_tokens": 64,
			"messages": [{"role": "user", "content": "test"}],
			"tool_choice": null,
			"tools": [
				{"name": "tool_a", "description": "test", "input_schema": {"type": "object", "properties": {}}}
			]
		}`
		result := ConvertClaudeRequestToOpenAI("gpt-5.4", []byte(inputJSON), false)
		if gjson.GetBytes(result, "tool_choice").Exists() {
			t.Fatalf("expected tool_choice not to be set when tool_choice is null, got: %s", result)
		}
	})
}
func TestConvertClaudeRequestToOpenAI_EnabledThinkingEffort(t *testing.T) {
	tests := []struct {
		name       string
		inputJSON  string
		wantEffort string
	}{
		{
			name:       "explicit output_config effort is preserved without budget",
			inputJSON:  `{"thinking":{"type":"enabled"},"output_config":{"effort":"high"}}`,
			wantEffort: "high",
		},
		{
			name:       "legacy budget remains authoritative when both are present",
			inputJSON:  `{"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"high"}}`,
			wantEffort: "medium",
		},
		{
			name:       "enabled without budget or effort keeps auto default",
			inputJSON:  `{"thinking":{"type":"enabled"}}`,
			wantEffort: "auto",
		},
		{
			name:       "enabled with empty effort string falls back to auto",
			inputJSON:  `{"thinking":{"type":"enabled"},"output_config":{"effort":""}}`,
			wantEffort: "auto",
		},
		{
			name:       "enabled with whitespace-only effort falls back to auto",
			inputJSON:  `{"thinking":{"type":"enabled"},"output_config":{"effort":"   "}}`,
			wantEffort: "auto",
		},
		{
			name:       "enabled with non-string effort falls back to auto",
			inputJSON:  `{"thinking":{"type":"enabled"},"output_config":{"effort":123}}`,
			wantEffort: "auto",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := ConvertClaudeRequestToOpenAI("test-model", []byte(tt.inputJSON), false)
			if got := gjson.GetBytes(out, "reasoning_effort").String(); got != tt.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q; body=%s", got, tt.wantEffort, out)
			}
		})
	}
}

func TestConvertClaudeRequestToOpenAI_ToolWithoutInputSchemaDefaultsParameters(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-opus-5",
		"tools": [
			{"name": "web_search"},
			{"name": "null_schema_custom", "input_schema": null},
			{"name": "with_schema", "input_schema": {"type": "object", "properties": {"q": {"type": "string"}}}}
		],
		"messages": [{"role": "user", "content": "hello"}]
	}`)

	output := ConvertClaudeRequestToOpenAI("test-model", inputJSON, false)
	parsed := gjson.ParseBytes(output)

	for i, toolName := range []string{"web_search", "null_schema_custom"} {
		params := parsed.Get(fmt.Sprintf("tools.%d.function.parameters", i))
		if !params.Exists() {
			t.Fatalf("tool %d (%s) function.parameters missing: %s", i, toolName, parsed.Get(fmt.Sprintf("tools.%d.function", i)).Raw)
		}
		if got := params.Get("type").String(); got != "object" {
			t.Fatalf("tool %d parameters.type = %q, want object: %s", i, got, params.Raw)
		}
		if props := params.Get("properties"); !props.Exists() || !props.IsObject() {
			t.Fatalf("tool %d parameters.properties missing: %s", i, params.Raw)
		}
	}
	if got := parsed.Get("tools.2.function.parameters.properties.q.type").String(); got != "string" {
		t.Fatalf("explicit schema was not preserved: %s", parsed.Get("tools.2.function.parameters").Raw)
	}
}
func TestConvertClaudeRequestToOpenAI_StripsUnsupportedUnicodePropertyEscapePatterns(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-opus-5",
		"messages": [{"role": "user", "content": "hello"}],
		"tools": [{
			"name": "pattern_tool",
			"input_schema": {
				"type": "object",
				"properties": {
					"bad": {"type": "string", "pattern": "^\\p{L}+$"},
					"good": {"type": "string", "pattern": "^[0-9a-f]{32}$"},
					"nested": {"type": "object", "properties": {"deep": {"type": "string", "pattern": "\\P{N}+"}}},
					"userdata": {"type": "string", "default": "keep \\p{L} text"}
				},
				"patternProperties": {
					"^\\p{L}+$": {"type": "string"},
					"^[a-z]+$": {"type": "string"}
				}
			}
		}]
	}`)

	output := ConvertClaudeRequestToOpenAI("test-model", inputJSON, false)
	params := gjson.GetBytes(output, "tools.0.function.parameters")
	if !params.Exists() {
		t.Fatalf("function.parameters missing: %s", output)
	}
	if params.Get("properties.bad.pattern").Exists() {
		t.Errorf("expected properties.bad.pattern to be removed, got: %s", params.Get("properties.bad.pattern").Raw)
	}
	if got := params.Get("properties.good.pattern").String(); got != "^[0-9a-f]{32}$" {
		t.Errorf("expected properties.good.pattern preserved, got %q: %s", got, params.Raw)
	}
	if params.Get("properties.nested.properties.deep.pattern").Exists() {
		t.Errorf("expected nested deep.pattern to be removed, got: %s", params.Raw)
	}
	if got := params.Get("properties.userdata.default").String(); got != `keep \p{L} text` {
		t.Errorf("expected user-data default preserved, got %q: %s", got, params.Raw)
	}
	patternProps := params.Get("patternProperties").Map()
	if _, exists := patternProps[`^\p{L}+$`]; exists {
		t.Errorf("expected patternProperties key with unicode escape to be removed, got: %s", params.Get("patternProperties").Raw)
	}
	if _, exists := patternProps[`^[a-z]+$`]; !exists {
		t.Errorf("expected safe patternProperties key preserved, got: %s", params.Get("patternProperties").Raw)
	}
}

// The internal Codex commentary marker is a forgeable literal a client can send
// in its own thinking block. It must never reach the OpenAI request: the block is
// restored as ordinary assistant content, not reasoning_content.
func TestConvertClaudeRequestToOpenAI_ClientSuppliedCommentaryMarkerIsStripped(t *testing.T) {
	payload := []byte(`{"model":"gpt-5","messages":[
		{"role":"user","content":"First turn"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"COMMENTARY_SENTINEL","signature":"cpa:commentary:v1"},
			{"type":"text","text":"FINAL_SENTINEL"}
		]},
		{"role":"user","content":"Next turn"}
	]}`)

	for _, convert := range []struct {
		name string
		fn   func(string, []byte, bool) []byte
	}{
		{"default", ConvertClaudeRequestToOpenAI},
		{"compat", ConvertClaudeRequestToOpenAIWithCompat},
	} {
		out := convert.fn("gpt-5", payload, false)
		if strings.Contains(string(out), "cpa:commentary:v1") {
			t.Fatalf("%s: internal marker reached the openai request: %s", convert.name, out)
		}
		if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got == "COMMENTARY_SENTINEL" {
			t.Fatalf("%s: commentary text became reasoning_content: %s", convert.name, out)
		}
		if !strings.Contains(string(out), "COMMENTARY_SENTINEL") {
			t.Fatalf("%s: commentary text was lost in translation: %s", convert.name, out)
		}
		if !strings.Contains(string(out), "FINAL_SENTINEL") {
			t.Fatalf("%s: final text was lost in translation: %s", convert.name, out)
		}
	}
}
