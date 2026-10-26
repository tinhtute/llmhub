package chat_completions

import (
	"regexp"
	"testing"

	internalsignature "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToAntigravityTranslatesVideoURL(t *testing.T) {
	input := []byte(`{
		"model": "gemini-3.7-flash-high",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "Name the colours in order"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}},
				{"type": "video_url", "video_url": {"url": "data:video/mp4;base64,AAAAIGZ0eXBtcDQy"}}
			]
		}]
	}`)

	output := ConvertOpenAIRequestToAntigravity("gemini-3.7-flash-high", input, false)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 3 {
		t.Fatalf("parts length = %d, want 3. Output: %s", len(parts), output)
	}

	if got := parts[0].Get("text").String(); got != "Name the colours in order" {
		t.Fatalf("parts[0].text = %q, want text content", got)
	}
	if got := parts[1].Get("inlineData.mimeType").String(); got != "image/png" {
		t.Fatalf("parts[1].inlineData.mimeType = %q, want image/png", got)
	}
	if got := parts[2].Get("inlineData.mimeType").String(); got != "video/mp4" {
		t.Fatalf("parts[2].inlineData.mimeType = %q, want video/mp4", got)
	}
	if got := parts[2].Get("inlineData.data").String(); got != "AAAAIGZ0eXBtcDQy" {
		t.Fatalf("parts[2].inlineData.data = %q, want video payload", got)
	}
	if parts[2].Get("thoughtSignature").Exists() {
		t.Fatal("video part should not receive an image thought signature")
	}
}

func TestConvertOpenAIRequestToAntigravity_MaxCompletionTokens(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected float64
	}{
		{
			name:     "only max_tokens",
			body:     `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`,
			expected: 100,
		},
		{
			name:     "only max_completion_tokens",
			body:     `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":200}`,
			expected: 200,
		},
		{
			name:     "max_tokens preferred over max_completion_tokens",
			body:     `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"max_completion_tokens":200}`,
			expected: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := ConvertOpenAIRequestToAntigravity("gemini-2.5-flash", []byte(tt.body), false)
			got := gjson.GetBytes(out, "request.generationConfig.maxOutputTokens")
			if !got.Exists() {
				t.Fatalf("request.generationConfig.maxOutputTokens missing. Output: %s", out)
			}
			if got.Float() != tt.expected {
				t.Fatalf("maxOutputTokens = %v, want %v. Output: %s", got.Float(), tt.expected, out)
			}
		})
	}
}

func TestConvertOpenAIRequestToAntigravityPreservesToolResponseAsString(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{
				"role": "user",
				"content": "read file"
			},
			{
				"role": "assistant",
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "read_file", "arguments": "{\"path\":\"config.json\"}"}
				}]
			},
			{
				"role": "tool",
				"tool_call_id": "call_1",
				"content": "{\"key\":\"value\",\"items\":[1,2,3]}"
			}
		]
	}`

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	contents := gjson.GetBytes(result, "request.contents").Array()
	if len(contents) < 3 {
		t.Fatalf("expected at least 3 contents, got %d. Output: %s", len(contents), result)
	}
	frResult := contents[2].Get("parts.0.functionResponse.response.result")
	if frResult.Type != gjson.String {
		t.Fatalf("expected functionResponse.response.result to be string, got type %s (raw: %s)", frResult.Type, frResult.Raw)
	}
	expected := `{"key":"value","items":[1,2,3]}`
	if got := frResult.String(); got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

// Ported from upstream CLIProxyAPI commit 80aa2b1d17a6 ("preserve tool call
// replay continuity"): tool_choice none pins mode NONE but keeps the tool
// declarations, so a replayed history of earlier tool calls stays declarable.
// This reverses a76da7115486, which dropped request.tools.
func TestConvertOpenAIRequestToAntigravityToolChoiceNoneKeepsTools(t *testing.T) {
	for _, tc := range []struct {
		name       string
		toolChoice string
	}{
		{name: "string none", toolChoice: `"none"`},
		{name: "object none", toolChoice: `{"type":"none"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputJSON := []byte(`{
				"model":"gemini-3-flash",
				"messages":[{"role":"user","content":"hi"}],
				"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
				"tool_choice":` + tc.toolChoice + `
			}`)
			out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", inputJSON, false)
			if got := gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.mode").String(); got != "NONE" {
				t.Fatalf("expected mode NONE, got %q", got)
			}
			if got := gjson.GetBytes(out, "request.tools.0.functionDeclarations.0.name").String(); got != "get_weather" {
				t.Fatalf("expected request.tools to keep get_weather, got %q (%s)", got, out)
			}
		})
	}
}

// Ported from upstream CLIProxyAPI commit 0fe19ede90a4 ("preserve Gemini prompt
// cache by demoting mid-session developer messages"): a mid-session developer
// message demotes to a user turn instead of mutating request.systemInstruction.
func TestConvertOpenAIRequestToAntigravity_MidSessionDeveloperMessageDoesNotMutateSystemInstruction(t *testing.T) {
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

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	output := gjson.ParseBytes(result)

	// request.systemInstruction must contain only original system prompt
	sysParts := output.Get("request.systemInstruction.parts").Array()
	if len(sysParts) != 1 {
		t.Fatalf("request.systemInstruction parts = %d, want 1. Output: %s", len(sysParts), result)
	}
	if got := sysParts[0].Get("text").String(); got != "You are a helpful assistant" {
		t.Fatalf("systemInstruction text = %q, want %q", got, "You are a helpful assistant")
	}

	// contents must contain user, model, user (demoted dev message), user
	contents := output.Get("request.contents").Array()
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
func TestConvertOpenAIRequestToAntigravity_MidSessionSystemReminderEnvelope(t *testing.T) {
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

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	output := gjson.ParseBytes(result)

	contents := output.Get("request.contents").Array()
	if len(contents) != 4 {
		t.Fatalf("contents length = %d, want 4. Output: %s", len(contents), result)
	}
	expectedReminder := "<system-reminder>\nPlease decide which tool to call next.\n</system-reminder>"
	if got := contents[2].Get("parts.0.text").String(); got != expectedReminder {
		t.Fatalf("mid-session system reminder mismatch:\ngot:  %q\nwant: %q", got, expectedReminder)
	}
}

func TestConvertOpenAIRequestToAntigravity_MidSessionTransientSystemInstructionPreservesTurnBoundaries(t *testing.T) {
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

	outWith := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(turnWithTransient), false)
	outWithout := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(turnWithoutTransient), false)

	contentsWith := gjson.GetBytes(outWith, "request.contents").Array()
	contentsWithout := gjson.GetBytes(outWithout, "request.contents").Array()

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

func TestConvertOpenAIRequestToAntigravity_MidSessionSystemReminderObjectAndArrayContent(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi"},
			{"role": "system", "content": {"type": "text", "text": "Object instruction"}},
			{"role": "developer", "content": [{"type": "text", "text": "Array instruction"}]}
		]
	}`

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	output := gjson.ParseBytes(result)

	contents := output.Get("request.contents").Array()
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
func TestConvertOpenAIRequestToAntigravity_MultiTurnRepeatedToolCallID_Issue5933(t *testing.T) {
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

	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)

	// In Turn 1 (contents[1] = model functionCall, contents[2] = user functionResponse):
	// functionCall.name must be "glob", and functionResponse.name must be "glob".
	call1Name := gjson.GetBytes(out, "request.contents.1.parts.0.functionCall.name").String()
	resp1Name := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.name").String()
	resp1Result := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.response.result").String()

	if call1Name != "glob" {
		t.Fatalf("turn 1 functionCall.name = %q, want glob", call1Name)
	}
	if resp1Name != "glob" {
		t.Fatalf("turn 1 functionResponse.name = %q, want glob (got overwritten by subsequent turn)", resp1Name)
	}
	if resp1Result != "[\"main.go\"]" {
		t.Fatalf("turn 1 functionResponse result = %q, want [\"main.go\"]", resp1Result)
	}

	// In Turn 2 (contents[4] = model functionCall, contents[5] = user functionResponse):
	// functionCall.name must be "read", and functionResponse.name must be "read".
	call2Name := gjson.GetBytes(out, "request.contents.4.parts.0.functionCall.name").String()
	resp2Name := gjson.GetBytes(out, "request.contents.5.parts.0.functionResponse.name").String()
	resp2Result := gjson.GetBytes(out, "request.contents.5.parts.0.functionResponse.response.result").String()

	if call2Name != "read" {
		t.Fatalf("turn 2 functionCall.name = %q, want read", call2Name)
	}
	if resp2Name != "read" {
		t.Fatalf("turn 2 functionResponse.name = %q, want read", resp2Name)
	}
	if resp2Result != "package main" {
		t.Fatalf("turn 2 functionResponse result = %q, want package main", resp2Result)
	}

	// Verify pairing validator passes without error
	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(out); errPairing != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed on Antigravity output: %v; output=%s", errPairing, out)
	}
}

// Ported from upstream CLIProxyAPI commit c2ea2684.
func TestConvertOpenAIRequestToAntigravity_ParallelAndOutOfOrderToolResponses(t *testing.T) {
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

	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)

	resp0Name := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.name").String()
	resp0Result := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.response.result").String()
	resp1Name := gjson.GetBytes(out, "request.contents.2.parts.1.functionResponse.name").String()
	resp1Result := gjson.GetBytes(out, "request.contents.2.parts.1.functionResponse.response.result").String()

	if resp0Name != "tool_a" || resp0Result != "res_a" {
		t.Fatalf("part 0 want tool_a / res_a, got %s / %s", resp0Name, resp0Result)
	}
	if resp1Name != "tool_b" || resp1Result != "res_b" {
		t.Fatalf("part 1 want tool_b / res_b, got %s / %s", resp1Name, resp1Result)
	}

	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(out); errPairing != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed: %v; output=%s", errPairing, out)
	}
}
func TestConvertOpenAIRequestToAntigravity_SanitizesClaudeToolIDs(t *testing.T) {
	inputJSON := `{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "user", "content": "hello"},
			{
				"role": "assistant",
				"tool_calls": [
					{
						"id": "call_test|fc_test",
						"type": "function",
						"function": {"name": "example_tool", "arguments": "{\"arg\":\"val\"}"}
					},
					{
						"id": "call_valid-123_456",
						"type": "function",
						"function": {"name": "valid_tool", "arguments": "{}"}
					}
				]
			},
			{"role": "tool", "tool_call_id": "call_test|fc_test", "content": "{\"result\":\"ok\"}"},
			{"role": "tool", "tool_call_id": "call_valid-123_456", "content": "{\"result\":\"valid_ok\"}"}
		]
	}`

	out := ConvertOpenAIRequestToAntigravity("claude-sonnet-4-6", []byte(inputJSON), false)

	claudePattern := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

	fc0ID := gjson.GetBytes(out, "request.contents.1.parts.0.functionCall.id").String()
	fc1ID := gjson.GetBytes(out, "request.contents.1.parts.1.functionCall.id").String()
	fr0ID := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.id").String()
	fr1ID := gjson.GetBytes(out, "request.contents.2.parts.1.functionResponse.id").String()

	if !claudePattern.MatchString(fc0ID) {
		t.Fatalf("functionCall 0 id %q does not match Claude pattern ^[a-zA-Z0-9_-]+$", fc0ID)
	}
	if want := "call_test_fc_test"; fc0ID != want {
		t.Fatalf("functionCall 0 id = %q, want %q", fc0ID, want)
	}
	if fr0ID != fc0ID {
		t.Fatalf("functionResponse 0 id = %q, want matching functionCall id %q", fr0ID, fc0ID)
	}

	fr0Result := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.response.result").String()
	if want := `{"result":"ok"}`; fr0Result != want {
		t.Fatalf("functionResponse 0 result = %q, want %q", fr0Result, want)
	}

	if !claudePattern.MatchString(fc1ID) {
		t.Fatalf("functionCall 1 id %q does not match Claude pattern ^[a-zA-Z0-9_-]+$", fc1ID)
	}
	if want := "call_valid-123_456"; fc1ID != want {
		t.Fatalf("functionCall 1 id = %q, want %q", fc1ID, want)
	}
	if fr1ID != fc1ID {
		t.Fatalf("functionResponse 1 id = %q, want matching functionCall id %q", fr1ID, fc1ID)
	}

	fr1Result := gjson.GetBytes(out, "request.contents.2.parts.1.functionResponse.response.result").String()
	if want := `{"result":"valid_ok"}`; fr1Result != want {
		t.Fatalf("functionResponse 1 result = %q, want %q", fr1Result, want)
	}
}

func TestConvertOpenAIRequestToAntigravity_SanitizesClaudeToolIDs_DisambiguatesCollisionsAndReversedResponses(t *testing.T) {
	inputJSON := `{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "user", "content": "run multiple tools"},
			{
				"role": "assistant",
				"tool_calls": [
					{
						"id": "call_a|b",
						"type": "function",
						"function": {"name": "tool_first", "arguments": "{\"n\":1}"}
					},
					{
						"id": "call_a_b",
						"type": "function",
						"function": {"name": "tool_second", "arguments": "{\"n\":2}"}
					}
				]
			},
			{"role": "tool", "tool_call_id": "call_a_b", "content": "result_for_a_b"},
			{"role": "tool", "tool_call_id": "call_a|b", "content": "result_for_a_pipe_b"}
		]
	}`

	out := ConvertOpenAIRequestToAntigravity("claude-sonnet-4-6", []byte(inputJSON), false)

	claudePattern := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

	fc0ID := gjson.GetBytes(out, "request.contents.1.parts.0.functionCall.id").String()
	fc1ID := gjson.GetBytes(out, "request.contents.1.parts.1.functionCall.id").String()
	fr0ID := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.id").String()
	fr1ID := gjson.GetBytes(out, "request.contents.2.parts.1.functionResponse.id").String()

	if !claudePattern.MatchString(fc0ID) {
		t.Fatalf("functionCall 0 id %q does not match Claude pattern", fc0ID)
	}
	if !claudePattern.MatchString(fc1ID) {
		t.Fatalf("functionCall 1 id %q does not match Claude pattern", fc1ID)
	}
	if fc0ID == fc1ID {
		t.Fatalf("functionCall IDs collided: fc0=%q, fc1=%q", fc0ID, fc1ID)
	}

	if fr0ID != fc0ID {
		t.Fatalf("functionResponse 0 id = %q, want matching functionCall id %q", fr0ID, fc0ID)
	}
	if fr1ID != fc1ID {
		t.Fatalf("functionResponse 1 id = %q, want matching functionCall id %q", fr1ID, fc1ID)
	}

	// Verify that results did not get swapped or overwritten despite reverse response order and raw ID collision
	fr0Result := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.response.result").String()
	if want := "result_for_a_pipe_b"; fr0Result != want {
		t.Fatalf("functionResponse 0 result = %q, want %q", fr0Result, want)
	}

	fr1Result := gjson.GetBytes(out, "request.contents.2.parts.1.functionResponse.response.result").String()
	if want := "result_for_a_b"; fr1Result != want {
		t.Fatalf("functionResponse 1 result = %q, want %q", fr1Result, want)
	}

	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(out); errPairing != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed: %v; output=%s", errPairing, out)
	}
}

func TestConvertOpenAIRequestToAntigravity_SanitizesClaudeToolIDs_MissingResponseDoesNotStealCollidingID(t *testing.T) {
	inputJSON := `{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "user", "content": "run multiple tools"},
			{
				"role": "assistant",
				"tool_calls": [
					{
						"id": "call_a|b",
						"type": "function",
						"function": {"name": "tool_first", "arguments": "{\"n\":1}"}
					},
					{
						"id": "call_a_b",
						"type": "function",
						"function": {"name": "tool_second", "arguments": "{\"n\":2}"}
					}
				]
			},
			{"role": "tool", "tool_call_id": "call_a_b", "content": "result_for_a_b"}
		]
	}`

	out := ConvertOpenAIRequestToAntigravity("claude-sonnet-4-6", []byte(inputJSON), false)

	claudePattern := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

	fc0ID := gjson.GetBytes(out, "request.contents.1.parts.0.functionCall.id").String()
	fc1ID := gjson.GetBytes(out, "request.contents.1.parts.1.functionCall.id").String()
	fr0ID := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.id").String()
	fr1ID := gjson.GetBytes(out, "request.contents.2.parts.1.functionResponse.id").String()

	if !claudePattern.MatchString(fc0ID) || !claudePattern.MatchString(fc1ID) {
		t.Fatalf("functionCall ids do not match Claude pattern: fc0=%q, fc1=%q", fc0ID, fc1ID)
	}
	if fc0ID == fc1ID {
		t.Fatalf("functionCall IDs collided: fc0=%q, fc1=%q", fc0ID, fc1ID)
	}
	if fr0ID != fc0ID || fr1ID != fc1ID {
		t.Fatalf("functionResponse ids must match functionCall ids: fr0=%q, fc0=%q, fr1=%q, fc1=%q", fr0ID, fc0ID, fr1ID, fc1ID)
	}

	// First call has missing response, should fallback to "{}" and NOT steal second call's result
	fr0Result := gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.response.result").String()
	if want := "{}"; fr0Result != want {
		t.Fatalf("functionResponse 0 result = %q, want %q (stole colliding call's result)", fr0Result, want)
	}

	fr1Result := gjson.GetBytes(out, "request.contents.2.parts.1.functionResponse.response.result").String()
	if want := "result_for_a_b"; fr1Result != want {
		t.Fatalf("functionResponse 1 result = %q, want %q", fr1Result, want)
	}

	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(out); errPairing != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed: %v; output=%s", errPairing, out)
	}
}
