package responses

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func prettyJSONForTest(raw []byte) string {
	if !gjson.ValidBytes(raw) {
		return string(raw)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return out.String()
}

func BenchmarkConvertOpenAIResponsesRequestWithLargeNonConvertibleToolArray(b *testing.B) {
	var request bytes.Buffer
	request.WriteString(`{"input":"hello","parallel_tool_calls":true,"tool_choice":"auto","tools":[`)
	for index := range 1024 {
		if index > 0 {
			request.WriteByte(',')
		}
		request.WriteString(`{"type":"web_search"}`)
	}
	request.WriteString(`]}`)
	raw := request.Bytes()
	if out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", raw, false); gjson.GetBytes(out, "tools").Exists() {
		b.Fatalf("non-convertible tools leaked into output: %s", out)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		_ = ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", raw, false)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MergeConsecutiveFunctionCalls(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"exec_command:0","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call","call_id":"exec_command:1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"exec_command:0","output":"ok0"},
			{"type":"function_call_output","call_id":"exec_command:1","output":"ok1"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, true)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	msgs := gjson.GetBytes(out, "messages")
	if !msgs.Exists() || !msgs.IsArray() {
		t.Fatalf("messages should be an array")
	}
	if got := len(msgs.Array()); got != 3 {
		t.Fatalf("messages count = %d, want %d", got, 3)
	}

	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want %q", got, "assistant")
	}
	if got := len(gjson.GetBytes(out, "messages.0.tool_calls").Array()); got != 2 {
		t.Fatalf("messages.0.tool_calls length = %d, want %d", got, 2)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String(); got != "exec_command:0" {
		t.Fatalf("messages.0.tool_calls.0.id = %q, want %q", got, "exec_command:0")
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.1.id").String(); got != "exec_command:1" {
		t.Fatalf("messages.0.tool_calls.1.id = %q, want %q", got, "exec_command:1")
	}

	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "exec_command:0" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "exec_command:0")
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != "exec_command:1" {
		t.Fatalf("messages.2.tool_call_id = %q, want %q", got, "exec_command:1")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_SplitFunctionCallsWhenInterrupted(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_a","name":"tool_a","arguments":"{}"},
			{"type":"message","role":"user","content":"next"},
			{"type":"function_call","call_id":"call_b","name":"tool_b","arguments":"{}"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, false)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 3 {
		t.Fatalf("messages count = %d, want %d", got, 3)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String(); got != "call_a" {
		t.Fatalf("messages.0.tool_calls.0.id = %q, want %q", got, "call_a")
	}
	if got := gjson.GetBytes(out, "messages.2.tool_calls.0.id").String(); got != "call_b" {
		t.Fatalf("messages.2.tool_calls.0.id = %q, want %q", got, "call_b")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DefersMessageUntilToolOutput(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_x","name":"exec_command","arguments":"{\"cmd\":\"echo hi\"}"},
			{"type":"message","role":"user","content":"Approved command prefix saved"},
			{"type":"function_call_output","call_id":"call_x","output":"ok"},
			{"type":"message","role":"user","content":"next"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, true)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 4 {
		t.Fatalf("messages count = %d, want %d", got, 4)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want %q", got, "assistant")
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "tool" {
		t.Fatalf("messages.1.role = %q, want %q", got, "tool")
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "call_x" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_x")
	}
	if got := gjson.GetBytes(out, "messages.2.role").String(); got != "user" {
		t.Fatalf("messages.2.role = %q, want %q", got, "user")
	}
	if got := gjson.GetBytes(out, "messages.2.content").String(); got != "Approved command prefix saved" {
		t.Fatalf("messages.2.content = %q, want %q", got, "Approved command prefix saved")
	}
	if got := gjson.GetBytes(out, "messages.3.content").String(); got != "next" {
		t.Fatalf("messages.3.content = %q, want %q", got, "next")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_PreservesReasoningOnFollowUpToolTurns(t *testing.T) {
	raw := []byte(`{
		"model": "deepseek-v4.1-flash",
		"reasoning": {"effort": "high"},
		"input": [
			{"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "first plan"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "starting"}]},
			{"type": "function_call", "call_id": "call_1", "name": "exec_command", "arguments": "{\"cmd\":\"ls\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
			{"type": "function_call", "call_id": "call_2", "name": "write_stdin", "arguments": "{\"data\":\"x\"}"},
			{"type": "function_call_output", "call_id": "call_2", "output": "ok"},
			{"type": "reasoning", "id": "rs_2", "summary": [{"type": "summary_text", "text": "second plan"}]},
			{"type": "function_call", "call_id": "call_3", "name": "exec_command", "arguments": "{\"cmd\":\"pwd\"}"},
			{"type": "function_call_output", "call_id": "call_3", "output": "ok"},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "apply_patch is available"}]},
			{"type": "function_call", "call_id": "call_4", "name": "exec_command", "arguments": "{\"cmd\":\"cat\"}"},
			{"type": "function_call_output", "call_id": "call_4", "output": "ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", raw, true)

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 8 {
		t.Fatalf("messages count = %d, want 8; output=%s", len(messages), out)
	}

	for i, msg := range messages {
		if msg.Get("role").String() == "assistant" && msg.Get("tool_calls").Exists() && len(msg.Get("tool_calls").Array()) > 0 {
			rc := msg.Get("reasoning_content").String()
			if rc == "" {
				t.Fatalf("messages[%d] with tool_calls is missing reasoning_content; message=%s", i, msg.Raw)
			}
		}
	}

	if got := messages[2].Get("tool_calls.0.id").String(); got != "call_2" {
		t.Fatalf("messages.2 tool call id = %q, want call_2; output=%s", got, out)
	}
	if got := messages[2].Get("reasoning_content").String(); got != "first plan" {
		t.Fatalf("messages.2 reasoning_content = %q, want %q; output=%s", got, "first plan", out)
	}
	if got := messages[6].Get("tool_calls.0.id").String(); got != "call_4" {
		t.Fatalf("messages.6 tool call id = %q, want call_4; output=%s", got, out)
	}
	if got := messages[6].Get("reasoning_content").String(); got != "second plan" {
		t.Fatalf("messages.6 reasoning_content = %q, want %q; output=%s", got, "second plan", out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_FallsBackToPlaceholderWhenNoPriorReasoning(t *testing.T) {
	raw := []byte(`{
		"model": "deepseek-v4.1-flash",
		"reasoning": {"effort": "high"},
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "exec_command", "arguments": "{\"cmd\":\"ls\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", raw, false)

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("messages count = %d, want 2; output=%s", len(messages), out)
	}
	if got := messages[0].Get("reasoning_content").String(); got != "[reasoning unavailable]" {
		t.Fatalf("messages.0 reasoning_content = %q, want %q; output=%s", got, "[reasoning unavailable]", out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_EffortNoneDoesNotInjectReasoning(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-4o",
		"reasoning": {"effort": "none"},
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "exec_command", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-4o", raw, false)

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("messages count = %d, want 2; output=%s", len(messages), out)
	}
	if messages[0].Get("reasoning_content").Exists() {
		t.Fatalf("messages.0 should not have reasoning_content when effort is none; output=%s", out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_PreservesReasoningOnCustomToolCallTurns(t *testing.T) {
	raw := []byte(`{
		"model": "deepseek-v4.1-flash",
		"reasoning": {"effort": "high"},
		"input": [
			{"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "custom plan"}]},
			{"type": "custom_tool_call", "call_id": "cust_1", "name": "do_work", "input": "step1"},
			{"type": "custom_tool_call_output", "call_id": "cust_1", "output": "done"},
			{"type": "custom_tool_call", "call_id": "cust_2", "name": "do_work", "input": "step2"},
			{"type": "custom_tool_call_output", "call_id": "cust_2", "output": "done"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", raw, false)

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 4 {
		t.Fatalf("messages count = %d, want 4; output=%s", len(messages), out)
	}
	if got := messages[0].Get("reasoning_content").String(); got != "custom plan" {
		t.Fatalf("messages.0 reasoning_content = %q, want %q; output=%s", got, "custom plan", out)
	}
	if got := messages[2].Get("reasoning_content").String(); got != "custom plan" {
		t.Fatalf("messages.2 reasoning_content = %q, want %q; output=%s", got, "custom plan", out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_ResetsReasoningAcrossUserMessageBoundary(t *testing.T) {
	raw := []byte(`{
		"model": "deepseek-v4.1-flash",
		"reasoning": {"effort": "high"},
		"input": [
			{"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "turn 1 plan"}]},
			{"type": "function_call", "call_id": "call_1", "name": "exec_command", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
			{"type": "message", "role": "user", "content": "now do step 2"},
			{"type": "function_call", "call_id": "call_2", "name": "exec_command", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_2", "output": "ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", raw, false)

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 5 {
		t.Fatalf("messages count = %d, want 5; output=%s", len(messages), out)
	}
	if got := messages[0].Get("reasoning_content").String(); got != "turn 1 plan" {
		t.Fatalf("messages.0 reasoning_content = %q, want %q; output=%s", got, "turn 1 plan", out)
	}
	if got := messages[3].Get("reasoning_content").String(); got != "[reasoning unavailable]" {
		t.Fatalf("messages.3 reasoning_content = %q, want %q; output=%s", got, "[reasoning unavailable]", out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_OmitsToolSettingsWithoutTools(t *testing.T) {
	tests := map[string][]byte{
		"empty tools": []byte(`{
			"input": [{"role":"user","content":"say ok"}],
			"tools": [],
			"tool_choice": "auto",
			"parallel_tool_calls": false
		}`),
		"unconvertible tools": []byte(`{
			"tools": [{"type":"unsupported"}],
			"tool_choice": "auto",
			"parallel_tool_calls": false
		}`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("grok-4.5", raw, false)
			for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
				if got := gjson.GetBytes(out, field); got.Exists() {
					t.Fatalf("%s should be omitted without tools; output=%s", field, out)
				}
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_PreservesToolSettingsWithTools(t *testing.T) {
	raw := []byte(`{
		"tools": [{"type":"function","name":"run_command","parameters":{"type":"object"}}],
		"tool_choice": {"type":"function","function":{"name":"run_command"}},
		"parallel_tool_calls": false
	}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("grok-4.5", raw, false)
	if got := gjson.GetBytes(out, "parallel_tool_calls"); !got.Exists() || got.Bool() {
		t.Fatalf("parallel_tool_calls = %v, want false; output=%s", got.Value(), out)
	}
	if got := gjson.GetBytes(out, "tool_choice.function.name").String(); got != "run_command" {
		t.Fatalf("tool_choice.function.name = %q, want run_command; output=%s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_ConvertsAdditionalTools(t *testing.T) {
	raw := []byte(`{
		"input": [{
			"type": "additional_tools",
			"tools": [{"type":"custom","name":"ask_user","description":"Ask user"}]
		}],
		"tool_choice": "auto",
		"parallel_tool_calls": true
	}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", raw, false)
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "ask_user" {
		t.Fatalf("tools.0.function.name = %q, want ask_user; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.input.type").String(); got != "string" {
		t.Fatalf("custom tool input schema type = %q, want string; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "parallel_tool_calls"); !got.Exists() || !got.Bool() {
		t.Fatalf("parallel_tool_calls = %v, want true; output=%s", got.Value(), out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_ConvertsNamespaceTools(t *testing.T) {
	raw := []byte(`{
		"tools": [{
			"type": "namespace",
			"name": "shell",
			"tools": [
				{"type":"function","name":"run","description":"Run command","parameters":{"type":"object"}},
				{"type":"custom","name":"edit","description":"Patch file"}
			]
		}],
		"input": [{"type":"function_call","call_id":"call_1","namespace":"shell","name":"run","arguments":"{}"}]
	}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", raw, false)
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "shell__run" {
		t.Fatalf("tools.0.function.name = %q, want shell__run; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.1.function.name").String(); got != "shell__edit" {
		t.Fatalf("tools.1.function.name = %q, want shell__edit; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.1.function.parameters.properties.input.type").String(); got != "string" {
		t.Fatalf("namespace custom input schema type = %q, want string; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.function.name").String(); got != "shell__run" {
		t.Fatalf("namespaced function call name = %q, want shell__run; output=%s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_OrphanFunctionCallOutputBecomesUserMessage(t *testing.T) {
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"role":"user","content":[{"type":"input_text","text":"Task initialization"}]},
			{"type":"function_call_output","id":"fco_01a09fca-8d33-73a1-97fd-4d83ecc02f9d","name":"send_message_to_thread","output":"<codex_delegation>\n  <source_thread_id>01a022d7-d4d0-72b2-8571-4590484ccaee</source_thread_id>\n  <input>Execute sub-task</input>\n</codex_delegation>"},
			{"type":"function_call","call_id":"call_1789387253098037589_85","name":"Bash","arguments":"{\"command\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call_1789387253098037589_85","id":"fco_01a09fca-a5f0-7b40-9943-21fbc923c537","output":"/Users/developer"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	delegationFound := false
	bashToolFound := false
	for _, message := range messages {
		role := message.Get("role").String()
		if role == "tool" && strings.TrimSpace(message.Get("tool_call_id").String()) == "" {
			t.Fatalf("orphan output emitted as tool message with empty tool_call_id: %s", string(out))
		}
		if role == "user" && strings.Contains(message.Get("content").String(), "<codex_delegation>") {
			delegationFound = true
		}
		if role == "tool" && message.Get("tool_call_id").String() == "call_1789387253098037589_85" {
			bashToolFound = true
			if got := message.Get("content").String(); got != "/Users/developer" {
				t.Fatalf("bash tool content = %q, want /Users/developer; output=%s", got, string(out))
			}
		}
	}
	if !delegationFound {
		t.Fatalf("expected orphan send_message_to_thread output as user content; output=%s", string(out))
	}
	if !bashToolFound {
		t.Fatalf("expected paired Bash tool message; output=%s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_UnpairedExplicitCallIDBecomesUserMessage(t *testing.T) {
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"role":"user","content":[{"type":"input_text","text":"Task initialization"}]},
			{"type":"function_call_output","call_id":"call_missing","name":"send_message_to_thread","output":"<codex_delegation>Execute sub-task</codex_delegation>"},
			{"type":"function_call","call_id":"call_1789387253098037589_85","name":"Bash","arguments":"{\"command\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call_1789387253098037589_85","output":"/Users/developer"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	delegationFound := false
	bashToolFound := false
	for _, message := range messages {
		role := message.Get("role").String()
		if role == "tool" && message.Get("tool_call_id").String() == "call_missing" {
			t.Fatalf("unpaired output emitted as tool message: %s", string(out))
		}
		if role == "user" && strings.Contains(message.Get("content").String(), "<codex_delegation>") {
			delegationFound = true
		}
		if role == "tool" && message.Get("tool_call_id").String() == "call_1789387253098037589_85" {
			bashToolFound = true
		}
	}
	if !delegationFound {
		t.Fatalf("expected unpaired send_message_to_thread output as user content; output=%s", string(out))
	}
	if !bashToolFound {
		t.Fatalf("expected paired Bash tool message; output=%s", string(out))
	}
}
func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_CapsLongNamespaceToolNames(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"role":"user","content":"hi"}
		],
		"tools": [
			{"type":"function","name":"exec_command","parameters":{"type":"object"}},
			{
				"type":"namespace",
				"name":"mcp__codex_apps__codex_document_control",
				"tools":[
					{"type":"function","name":"_execute_document_command","parameters":{"type":"object"}},
					{"type":"function","name":"_get_document_tool_schemas","parameters":{"type":"object"}}
				]
			},
			{
				"type":"namespace",
				"name":"mcp__codex_apps__safety_settings",
				"tools":[
					{"type":"function","name":"_prepare_parental_control_update","parameters":{"type":"object"}}
				]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", raw, false)
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 4 {
		t.Fatalf("tools count = %d, want 4; output=%s", len(tools), out)
	}
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		name := tool.Get("function.name").String()
		if len(name) > 64 {
			t.Errorf("function.name %q (len %d) exceeds the 64-character limit; output=%s", name, len(name), out)
		}
		if seen[name] {
			t.Errorf("duplicate function.name %q after flattening; output=%s", name, out)
		}
		seen[name] = true
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DisambiguatesTruncationCollisions(t *testing.T) {
	// Two distinct namespace tools whose qualified names both truncate to the
	// same 64-char tail must survive as two usable chat tools, not be merged
	// or silently dropped by the first-wins deduplication.
	filler := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	raw := []byte(`{
		"input": [
			{"role":"user","content":"hi"}
		],
		"tools": [
			{
				"type":"namespace",
				"name":"mcp__server_one__` + filler + `",
				"tools":[{"type":"function","name":"_same_tail_tool_name","parameters":{"type":"object"}}]
			},
			{
				"type":"namespace",
				"name":"mcp__server_two__` + filler + `",
				"tools":[{"type":"function","name":"_same_tail_tool_name","parameters":{"type":"object"}}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", raw, false)
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools count = %d, want 2; output=%s", len(tools), out)
	}
	first := tools[0].Get("function.name").String()
	second := tools[1].Get("function.name").String()
	if first == second {
		t.Fatalf("truncation collision was not disambiguated: both tools are %q; output=%s", first, out)
	}
	for _, name := range []string{first, second} {
		if len(name) > 64 {
			t.Errorf("disambiguated name %q (len %d) exceeds 64; output=%s", name, len(name), out)
		}
	}

	// A replayed call to the renamed declaration must resolve to the renamed
	// chat name so the assistant history matches the tools array.
	merged := []byte(`{
		"input": [
			{"type":"custom_tool_call","namespace":"mcp__server_two__` + filler + `","name":"_same_tail_tool_name","call_id":"call_1","input":"x"},
			{"type":"custom_tool_call_output","call_id":"call_1","output":"y"}
		],
		"tools": [
			{
				"type":"namespace",
				"name":"mcp__server_one__` + filler + `",
				"tools":[{"type":"function","name":"_same_tail_tool_name","parameters":{"type":"object"}}]
			},
			{
				"type":"namespace",
				"name":"mcp__server_two__` + filler + `",
				"tools":[{"type":"function","name":"_same_tail_tool_name","parameters":{"type":"object"}}]
			}
		]
	}`)
	replayOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", merged, false)
	replayedName := ""
	for _, m := range gjson.GetBytes(replayOut, "messages").Array() {
		if m.Get("role").String() == "assistant" {
			replayedName = m.Get("tool_calls.0.function.name").String()
		}
	}
	if replayedName != second {
		t.Fatalf("replayed collision-suffixed call name = %q, want %q to match the tools array; output=%s", replayedName, second, replayOut)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_LongDeclarationDoesNotDisplaceShortOriginal(t *testing.T) {
	// A long namespace declaration whose capped tail equals a later flat
	// declaration's original name must take the suffix itself: the flat tool's
	// original name is what replayed calls and tool_choice carry, so
	// displacing it would dispatch those calls to the wrong tool.
	longNamespace := "mcp__a__" + strings.Repeat("b", 60)
	longChild := "child_tool"
	qualified := longNamespace + "__" + longChild
	flatName := capResponsesChatToolName(qualified)
	if len(qualified) <= 64 || len(flatName) != 64 || flatName == qualified {
		t.Fatalf("fixture drift: qualified %q (len %d) must exceed the cap and cap to 64 chars", qualified, len(qualified))
	}
	suffixed := capResponsesChatToolName(flatName + "_1")

	toolsJSON := `[
		{
			"type":"namespace",
			"name":"` + longNamespace + `",
			"tools":[{"type":"function","name":"` + longChild + `","parameters":{"type":"object"}}]
		},
		{"type":"function","name":"` + flatName + `","parameters":{"type":"object"}}
	]`

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`
	}`), false)
	emitted := gjson.GetBytes(out, "tools").Array()
	if len(emitted) != 2 {
		t.Fatalf("tools count = %d, want 2; output=%s", len(emitted), out)
	}
	if got := emitted[1].Get("function.name").String(); got != flatName {
		t.Fatalf("flat declaration was displaced: its name is %q, want its original %q; output=%s", got, flatName, out)
	}
	if got := emitted[0].Get("function.name").String(); got != suffixed {
		t.Fatalf("long declaration name = %q, want suffixed %q; output=%s", got, suffixed, out)
	}
	for _, tool := range emitted {
		if name := tool.Get("function.name").String(); len(name) > 64 {
			t.Errorf("function.name %q (len %d) exceeds 64; output=%s", name, len(name), out)
		}
	}

	// Replayed calls and tool_choice for the flat tool carry its original
	// name; they must resolve to the flat declaration, not to the long
	// declaration that caps onto it.
	replay := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_1","name":"` + flatName + `","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"tools": ` + toolsJSON + `
	}`)
	replayOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", replay, false)
	for _, m := range gjson.GetBytes(replayOut, "messages").Array() {
		if m.Get("role").String() == "assistant" {
			if got := m.Get("tool_calls.0.function.name").String(); got != flatName {
				t.Fatalf("replayed flat call resolved to %q, want %q; output=%s", got, flatName, replayOut)
			}
		}
	}

	forcedOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`,
		"tool_choice": {"type":"function","function":{"name":"`+flatName+`"}}
	}`), false)
	if got := gjson.GetBytes(forcedOut, "tool_choice.function.name").String(); got != flatName {
		t.Fatalf("tool_choice for the flat tool resolved to %q, want %q; output=%s", got, flatName, forcedOut)
	}

	// A replayed call carrying the long declaration's fully-qualified
	// uncapped name (history from an older build or a foreign client that
	// flattened the name itself) must resolve to the suffixed chat name,
	// not to the capped tail that now belongs to the flat tool.
	longReplay := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_2","name":"` + qualified + `","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_2","output":"ok"}
		],
		"tools": ` + toolsJSON + `
	}`)
	longReplayOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", longReplay, false)
	for _, m := range gjson.GetBytes(longReplayOut, "messages").Array() {
		if m.Get("role").String() == "assistant" {
			if got := m.Get("tool_calls.0.function.name").String(); got != suffixed {
				t.Fatalf("replayed long-qualified call resolved to %q, want %q; output=%s", got, suffixed, longReplayOut)
			}
		}
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_AmbiguousLongLocalNameStaysUnresolved(t *testing.T) {
	// Two namespace declarations sharing the same >64-byte local name, with a
	// replayed call that omits the namespace: resolution is ambiguous, and the
	// capped fallback must not land on either declaration's alias, or the call
	// would silently invoke that namespace's tool.
	longLocal := "shared_" + strings.Repeat("x", 60)
	toolsJSON := `[
		{
			"type":"namespace",
			"name":"mcp__alpha",
			"tools":[{"type":"function","name":"` + longLocal + `","parameters":{"type":"object"}}]
		},
		{
			"type":"namespace",
			"name":"mcp__beta",
			"tools":[{"type":"function","name":"` + longLocal + `","parameters":{"type":"object"}}]
		}
	]`

	replay := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_1","name":"` + longLocal + `","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"tools": ` + toolsJSON + `
	}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", replay, false)

	declaredAliases := map[string]bool{}
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		declaredAliases[tool.Get("function.name").String()] = true
	}
	if len(declaredAliases) != 2 {
		t.Fatalf("tools count = %d, want 2; output=%s", len(declaredAliases), out)
	}
	replayedName := ""
	for _, m := range gjson.GetBytes(out, "messages").Array() {
		if m.Get("role").String() == "assistant" {
			replayedName = m.Get("tool_calls.0.function.name").String()
		}
	}
	if len(replayedName) > 64 {
		t.Fatalf("replayed ambiguous name %q (len %d) exceeds 64; output=%s", replayedName, len(replayedName), out)
	}
	if declaredAliases[replayedName] {
		t.Fatalf("ambiguous replayed call resolved to declared alias %q, silently invoking one namespace's tool; output=%s", replayedName, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_LongAliasDoesNotDisplaceNamespacedLocalName(t *testing.T) {
	// A long declaration's capped alias must not occupy the (<=64-char) local
	// name of a namespaced declaration whose qualified identity exceeds the
	// cap: replayed calls and tool_choice that omit the namespace carry the
	// bare local name, and local-name recovery must resolve them to the
	// namespaced tool instead of to the earlier long declaration's alias.
	localName := "l" + strings.Repeat("m", 63) // 64 chars
	longFlatName := strings.Repeat("n", 11) + localName
	if len(longFlatName) <= 64 || capResponsesChatToolName(longFlatName) != localName {
		t.Fatalf("fixture drift: cap(%q) = %q, want %q", longFlatName, capResponsesChatToolName(longFlatName), localName)
	}
	// mcp__beta__localName is 75 chars, so the namespaced declaration is long
	// too, and its 11-char prefix is exactly what the cap drops: its alias is
	// localName itself unless the reservation keeps the flat tool off it.
	toolsJSON := `[
		{"type":"function","name":"` + longFlatName + `","parameters":{"type":"object"}},
		{
			"type":"namespace",
			"name":"mcp__beta",
			"tools":[{"type":"function","name":"` + localName + `","parameters":{"type":"object"}}]
		}
	]`

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`
	}`), false)
	emitted := gjson.GetBytes(out, "tools").Array()
	if len(emitted) != 2 {
		t.Fatalf("tools count = %d, want 2; output=%s", len(emitted), out)
	}
	if got := emitted[0].Get("function.name").String(); got == localName {
		t.Fatalf("long declaration claimed the namespaced local name %q as its capped alias; output=%s", localName, out)
	}
	namespacedAlias := emitted[1].Get("function.name").String()
	for i, tool := range emitted {
		if name := tool.Get("function.name").String(); len(name) > 64 {
			t.Errorf("tools[%d].function.name %q (len %d) exceeds 64; output=%s", i, name, len(name), out)
		}
	}

	// A replayed call omitting the namespace carries the bare local name and
	// must resolve to the namespaced declaration's alias, not the long one.
	replay := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_1","name":"` + localName + `","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"tools": ` + toolsJSON + `
	}`)
	replayOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", replay, false)
	for _, m := range gjson.GetBytes(replayOut, "messages").Array() {
		if m.Get("role").String() == "assistant" {
			if got := m.Get("tool_calls.0.function.name").String(); got != namespacedAlias {
				t.Fatalf("replayed bare local name resolved to %q, want the namespaced alias %q; output=%s", got, namespacedAlias, replayOut)
			}
		}
	}

	// tool_choice carrying the bare local name must resolve the same way.
	forcedOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`,
		"tool_choice": {"type":"function","function":{"name":"`+localName+`"}}
	}`), false)
	if got := gjson.GetBytes(forcedOut, "tool_choice.function.name").String(); got != namespacedAlias {
		t.Fatalf("tool_choice bare local name resolved to %q, want the namespaced alias %q; output=%s", got, namespacedAlias, forcedOut)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_SharedLocalNameIsNeverEmitted(t *testing.T) {
	// Two namespaces declaring the same 64-byte local name: both qualified
	// identities exceed the cap and their tails are exactly that bare local
	// name, so the name is ambiguous for any namespace-less call yet also the
	// natural capped alias of both declarations. Reserving it for the first
	// declaration alone would make it emit the ambiguous name verbatim, where
	// the exact-emitted-alias match attributes every namespace-less call and
	// tool_choice to that namespace. The name must be burned instead, leaving
	// both declarations on distinct aliases.
	sharedLocal := "s" + strings.Repeat("t", 63) // exactly 64 chars
	for _, namespace := range []string{"mcp__alpha", "mcp__beta"} {
		if got := capResponsesChatToolName(rawResponsesNamespaceQualifiedName(namespace, sharedLocal)); got != sharedLocal {
			t.Fatalf("fixture drift: %s alias = %q, want the ambiguous bare name %q", namespace, got, sharedLocal)
		}
	}
	toolsJSON := `[
		{
			"type":"namespace",
			"name":"mcp__alpha",
			"tools":[{"type":"function","name":"` + sharedLocal + `","parameters":{"type":"object"}}]
		},
		{
			"type":"namespace",
			"name":"mcp__beta",
			"tools":[{"type":"function","name":"` + sharedLocal + `","parameters":{"type":"object"}}]
		}
	]`

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`
	}`), false)
	emitted := gjson.GetBytes(out, "tools").Array()
	if len(emitted) != 2 {
		t.Fatalf("tools count = %d, want 2; output=%s", len(emitted), out)
	}
	aliases := make(map[string]bool, len(emitted))
	for i, tool := range emitted {
		name := tool.Get("function.name").String()
		if len(name) > 64 {
			t.Errorf("tools[%d].function.name %q (len %d) exceeds 64; output=%s", i, name, len(name), out)
		}
		if name == sharedLocal {
			t.Errorf("tools[%d] emits the ambiguous local name %q; output=%s", i, name, out)
		}
		if aliases[name] {
			t.Errorf("tools[%d] duplicates alias %q; output=%s", i, name, out)
		}
		aliases[name] = true
	}
	alphaAlias := emitted[0].Get("function.name").String()
	betaAlias := emitted[1].Get("function.name").String()

	// Each namespace still reaches its own declaration.
	for _, tc := range []struct {
		namespace string
		want      string
	}{
		{"mcp__alpha", alphaAlias},
		{"mcp__beta", betaAlias},
	} {
		namespacedOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
			"input": [
				{"type":"function_call","call_id":"call_1","namespace":"`+tc.namespace+`","name":"`+sharedLocal+`","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_1","output":"ok"}
			],
			"tools": `+toolsJSON+`
		}`), false)
		got := ""
		for _, m := range gjson.GetBytes(namespacedOut, "messages").Array() {
			if m.Get("role").String() == "assistant" {
				got = m.Get("tool_calls.0.function.name").String()
			}
		}
		if got != tc.want {
			t.Fatalf("namespaced replay for %s resolved to %q, want %q; output=%s", tc.namespace, got, tc.want, namespacedOut)
		}
	}

	// A namespace-less replayed call or tool_choice carrying the ambiguous
	// bare name must stay unresolved rather than pick a winner.
	bareOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_1","name":"`+sharedLocal+`","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"tools": `+toolsJSON+`
	}`), false)
	for _, m := range gjson.GetBytes(bareOut, "messages").Array() {
		if m.Get("role").String() != "assistant" {
			continue
		}
		got := m.Get("tool_calls.0.function.name").String()
		if len(got) > 64 {
			t.Fatalf("ambiguous replayed name %q (len %d) exceeds 64; output=%s", got, len(got), bareOut)
		}
		if aliases[got] {
			t.Fatalf("ambiguous replayed call resolved to declared alias %q, silently invoking one namespace's tool; output=%s", got, bareOut)
		}
	}
	bareForced := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`,
		"tool_choice": {"type":"function","function":{"name":"`+sharedLocal+`"}}
	}`), false)
	if got := gjson.GetBytes(bareForced, "tool_choice.function.name").String(); aliases[got] {
		t.Fatalf("ambiguous tool_choice resolved to declared alias %q, silently invoking one namespace's tool; output=%s", got, bareForced)
	}
	forcedAlpha := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`,
		"tool_choice": {"type":"function","namespace":"mcp__beta","function":{"name":"`+sharedLocal+`"}}
	}`), false)
	if got := gjson.GetBytes(forcedAlpha, "tool_choice.function.name").String(); got != betaAlias {
		t.Fatalf("namespaced tool_choice resolved to %q, want %q; output=%s", got, betaAlias, forcedAlpha)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_QualifiedIdentityOutranksForeignLocalName(t *testing.T) {
	// A replayed call or tool_choice carrying a fully-qualified uncapped name
	// can match two things: the declaration whose qualified identity it is (A),
	// and a different namespace's child that literally uses that whole
	// qualified string as its own name (B). Local-name recovery runs on a bare
	// name and is a guess about the namespace, so it must not outrank the
	// identity match, or the call gets dispatched to B's tool.
	longChild := "read_" + strings.Repeat("f", 60)
	qualified := rawResponsesNamespaceQualifiedName("alpha_ns", longChild)
	if len(qualified) <= responsesChatToolNameLimit {
		t.Fatalf("fixture drift: qualified identity %q (len %d) must exceed the cap", qualified, len(qualified))
	}
	if capResponsesChatToolName(rawResponsesNamespaceQualifiedName("beta_ns", qualified)) != capResponsesChatToolName(qualified) {
		t.Fatalf("fixture drift: the two declarations must cap onto the same alias; got %q and %q",
			capResponsesChatToolName(rawResponsesNamespaceQualifiedName("beta_ns", qualified)),
			capResponsesChatToolName(qualified))
	}
	toolsJSON := `[
		{
			"type":"namespace",
			"name":"alpha_ns",
			"tools":[{"type":"function","name":"` + longChild + `","parameters":{"type":"object"}}]
		},
		{
			"type":"namespace",
			"name":"beta_ns",
			"tools":[{"type":"function","name":"` + qualified + `","parameters":{"type":"object"}}]
		}
	]`

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`
	}`), false)
	emitted := gjson.GetBytes(out, "tools").Array()
	if len(emitted) != 2 {
		t.Fatalf("tools count = %d, want 2; output=%s", len(emitted), out)
	}
	alphaAlias := emitted[0].Get("function.name").String()
	betaAlias := emitted[1].Get("function.name").String()
	if alphaAlias == betaAlias {
		t.Fatalf("both declarations emitted %q; output=%s", alphaAlias, out)
	}
	for i, tool := range emitted {
		if name := tool.Get("function.name").String(); len(name) > 64 {
			t.Errorf("tools[%d].function.name %q (len %d) exceeds 64; output=%s", i, name, len(name), out)
		}
	}

	// Bare qualified name: provenance points at the alpha declaration.
	bareOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_1","name":"`+qualified+`","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"tools": `+toolsJSON+`
	}`), false)
	for _, m := range gjson.GetBytes(bareOut, "messages").Array() {
		if m.Get("role").String() != "assistant" {
			continue
		}
		if got := m.Get("tool_calls.0.function.name").String(); got != alphaAlias {
			t.Fatalf("bare qualified name resolved to %q, want the identity owner's alias %q (beta's alias is %q); output=%s", got, alphaAlias, betaAlias, bareOut)
		}
	}
	bareForced := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
		"input": [{"role":"user","content":"hi"}],
		"tools": `+toolsJSON+`,
		"tool_choice": {"type":"function","function":{"name":"`+qualified+`"}}
	}`), false)
	if got := gjson.GetBytes(bareForced, "tool_choice.function.name").String(); got != alphaAlias {
		t.Fatalf("tool_choice bare qualified name resolved to %q, want %q; output=%s", got, alphaAlias, bareForced)
	}

	// Both namespaces stay individually reachable when the namespace is present.
	for _, tc := range []struct {
		namespace string
		name      string
		want      string
	}{
		{"alpha_ns", longChild, alphaAlias},
		{"beta_ns", qualified, betaAlias},
	} {
		namespacedOut := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("z-ai/glm-5.3-free", []byte(`{
			"input": [
				{"type":"function_call","call_id":"call_1","namespace":"`+tc.namespace+`","name":"`+tc.name+`","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_1","output":"ok"}
			],
			"tools": `+toolsJSON+`
		}`), false)
		got := ""
		for _, m := range gjson.GetBytes(namespacedOut, "messages").Array() {
			if m.Get("role").String() == "assistant" {
				got = m.Get("tool_calls.0.function.name").String()
			}
		}
		if got != tc.want {
			t.Fatalf("namespaced replay for %s/%s resolved to %q, want %q; output=%s", tc.namespace, tc.name, got, tc.want, namespacedOut)
		}
	}
}

// Ported from upstream CLIProxyAPI internal/translator/openai/openai/responses/openai_openai-responses_request_test.go
// (690f4f3116b6).
func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MapsMaxOutputTokensToMaxTokens(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-5.4",
		"input": "hello",
		"max_output_tokens": 1024
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", raw, false)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 1024 {
		t.Fatalf("max_tokens = %d, want 1024; output=%s", got, string(out))
	}
	if gjson.GetBytes(out, "max_completion_tokens").Exists() {
		t.Fatalf("max_completion_tokens should be absent; output=%s", string(out))
	}

	rawWithoutLimit := []byte(`{
		"model": "gpt-5.4",
		"input": "hello"
	}`)

	outWithoutLimit := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", rawWithoutLimit, false)
	if gjson.GetBytes(outWithoutLimit, "max_completion_tokens").Exists() {
		t.Fatalf("max_completion_tokens should be absent when omitted; output=%s", string(outWithoutLimit))
	}
	if gjson.GetBytes(outWithoutLimit, "max_tokens").Exists() {
		t.Fatalf("max_tokens should be absent when omitted; output=%s", string(outWithoutLimit))
	}

	rawNull := []byte(`{
		"model": "gpt-5.4",
		"input": "hello",
		"max_output_tokens": null
	}`)

	outNull := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", rawNull, false)
	if got := gjson.GetBytes(outNull, "max_tokens"); !got.Exists() || got.Type != gjson.Null {
		t.Fatalf("max_tokens = %v, want null; output=%s", got, string(outNull))
	}
	if gjson.GetBytes(outNull, "max_completion_tokens").Exists() {
		t.Fatalf("max_completion_tokens should be absent; output=%s", string(outNull))
	}
}

// The following tests are ported from upstream CLIProxyAPI commit cc545cbf
// (align tool call messages and preserve ordering on ambiguous outputs,
// issue #5925).

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_IncompleteToolCallsDoNotDeferMessages(t *testing.T) {
	// If assistant issues function_call(a) and function_call(b), but only function_call_output(a)
	// arrives, the history is incomplete. Intervening messages (e.g. user reminder) must NOT
	// be deferred; original order must be preserved.
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"type":"function_call","call_id":"call_a","name":"tool_a","arguments":"{}"},
			{"type":"function_call","call_id":"call_b","name":"tool_b","arguments":"{}"},
			{"role":"user","content":"reminder before results"},
			{"type":"function_call_output","call_id":"call_a","output":"result_a"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Incomplete history must stay untouched:
	// messages[0]: assistant (tool_calls [call_a, call_b])
	// messages[1]: user ("reminder before results")
	// messages[2]: tool (tool_call_id: call_a)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %s", len(messages), out)
	}
	if roles[0] != "assistant" || roles[1] != "user" || roles[2] != "tool" {
		t.Fatalf("expected untouched order [assistant, user, tool], got: %v (output=%s)", roles, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_CompleteToolCallsDoPairMessages(t *testing.T) {
	// If assistant issues function_call(a) and function_call(b), and both function_call_output(a)
	// and function_call_output(b) arrive, with an intervening user reminder, the tool outputs
	// must be paired immediately following the assistant tool_calls message.
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"type":"function_call","call_id":"call_a","name":"tool_a","arguments":"{}"},
			{"type":"function_call","call_id":"call_b","name":"tool_b","arguments":"{}"},
			{"role":"user","content":"reminder during execution"},
			{"type":"function_call_output","call_id":"call_b","output":"result_b"},
			{"type":"function_call_output","call_id":"call_a","output":"result_a"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Expected:
	// messages[0]: assistant (tool_calls [call_a, call_b])
	// messages[1]: tool
	// messages[2]: tool
	// messages[3]: user ("reminder during execution")
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d: %s", len(messages), out)
	}
	if roles[0] != "assistant" || roles[1] != "tool" || roles[2] != "tool" || roles[3] != "user" {
		t.Fatalf("expected order [assistant, tool, tool, user], got: %v (output=%s)", roles, out)
	}
	toolIDs := []string{messages[1].Get("tool_call_id").String(), messages[2].Get("tool_call_id").String()}
	if toolIDs[0] != "call_b" || toolIDs[1] != "call_a" {
		t.Fatalf("expected tool messages in relative input order [call_b, call_a], got: %v", toolIDs)
	}
	if messages[1].Get("content").String() != "result_b" || messages[2].Get("content").String() != "result_a" {
		t.Fatalf("expected contents [result_b, result_a], got [%s, %s]", messages[1].Get("content").String(), messages[2].Get("content").String())
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MixedEmptyIDDoesNotReorder(t *testing.T) {
	// Mixed empty call_id and valid call_a: incomplete/ambiguous history must remain untouched
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"type":"function_call","call_id":"","name":"unknown","arguments":"{}"},
			{"type":"function_call","call_id":"a","name":"known","arguments":"{}"},
			{"role":"user","content":"reminder"},
			{"type":"function_call_output","call_id":"a","output":"ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Must stay in natural input order: assistant -> user -> tool
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %s", len(messages), out)
	}
	if roles[0] != "assistant" || roles[1] != "user" || roles[2] != "tool" {
		t.Fatalf("expected untouched order [assistant, user, tool], got: %v (output=%s)", roles, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DuplicateCallIDDoesNotReorder(t *testing.T) {
	// Duplicate call_id: ambiguous history must remain untouched
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"type":"function_call","call_id":"dup","name":"tool_1","arguments":"{}"},
			{"type":"function_call","call_id":"dup","name":"tool_2","arguments":"{}"},
			{"role":"user","content":"reminder"},
			{"type":"function_call_output","call_id":"dup","output":"ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Must stay in natural input order: assistant -> user -> tool
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %s", len(messages), out)
	}
	if roles[0] != "assistant" || roles[1] != "user" || roles[2] != "tool" {
		t.Fatalf("expected untouched order [assistant, user, tool], got: %v (output=%s)", roles, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DuplicateOutputCallIDDoesNotReorder(t *testing.T) {
	// Duplicate function_call_output for the same call_id: ambiguous results must not be prematurely reordered
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"type":"function_call","call_id":"call_dup_out","name":"tool_a","arguments":"{}"},
			{"role":"user","content":"reminder before results"},
			{"type":"function_call_output","call_id":"call_dup_out","output":"first"},
			{"type":"function_call_output","call_id":"call_dup_out","output":"second"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Must stay in natural input order: assistant -> user ("reminder before results") -> tool/user
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d: %s", len(messages), out)
	}
	if roles[0] != "assistant" || roles[1] != "user" {
		t.Fatalf("expected natural order with user reminder preserved at index 1, got roles: %v (output=%s)", roles, out)
	}
	if messages[1].Get("content").String() != "reminder before results" {
		t.Fatalf("expected message[1] to be reminder, got: %s", messages[1].Raw)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DuplicateCustomOutputCallIDDoesNotReorder(t *testing.T) {
	// Duplicate custom_tool_call_output for the same call_id: ambiguous results must not be prematurely reordered
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"type":"custom_tool_call","call_id":"custom_dup","name":"custom_a","input":"{}"},
			{"role":"user","content":"reminder before custom results"},
			{"type":"custom_tool_call_output","call_id":"custom_dup","output":"output 1"},
			{"type":"custom_tool_call_output","call_id":"custom_dup","output":"output 2"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Must stay in natural input order: assistant -> user ("reminder before custom results") -> tool/user
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d: %s", len(messages), out)
	}
	if roles[0] != "assistant" || roles[1] != "user" {
		t.Fatalf("expected natural order with user reminder preserved at index 1, got roles: %v (output=%s)", roles, out)
	}
	if messages[1].Get("content").String() != "reminder before custom results" {
		t.Fatalf("expected message[1] to be reminder, got: %s", messages[1].Raw)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MultipleOutputsWithoutIDDoNotGuessOrReorder(t *testing.T) {
	// If assistant issues function_call(a) and function_call(b), and multiple outputs arrive
	// without call_ids, the assignment is a non-unique guess. The history must stay untouched!
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"type":"function_call","call_id":"a","name":"unknown_a","arguments":"{}"},
			{"type":"function_call","call_id":"b","name":"unknown_b","arguments":"{}"},
			{"role":"user","content":"reminder before results"},
			{"type":"function_call_output","output":"output X"},
			{"type":"function_call_output","output":"output Y"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Must stay in natural input order: assistant -> user ("reminder before results") -> user ("output X") -> user ("output Y")
	// Crucially, outputs X and Y must NOT be given guessed tool_call_id "a" or "b" and must NOT be fabricated as tool messages.
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d: %s", len(messages), out)
	}
	if roles[0] != "assistant" || roles[1] != "user" || roles[2] != "user" || roles[3] != "user" {
		t.Fatalf("expected natural order with user reminder and un-guessed outputs preserved, got roles: %v (output=%s)", roles, out)
	}
	if messages[1].Get("content").String() != "reminder before results" {
		t.Fatalf("expected message[1] to be reminder, got: %s", messages[1].Raw)
	}
	if messages[2].Get("content").String() != "output X" || messages[3].Get("content").String() != "output Y" {
		t.Fatalf("expected outputs X and Y as standalone user messages, got msg2=%s msg3=%s", messages[2].Raw, messages[3].Raw)
	}
	// Verify no tool messages were created with guessed call IDs
	for _, m := range messages {
		if m.Get("role").String() == "tool" {
			t.Fatalf("unexpected tool message created via guessing: %s", m.Raw)
		}
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MultipleOutputsWithoutIDAndOrphanOutputDoNotGuessOrReorder(t *testing.T) {
	// Call A, Call B.
	// User reminder.
	// Output 1 has NO ID (X).
	// Output 2 has NO ID (Y).
	// Output 3 has orphan explicit ID (Z).
	// The presence of orphan_id must NOT cause X/Y to be guessed and reordered before the reminder!
	inputJSON := []byte(`{
		"model": "deepseek-v4.1-flash",
		"input": [
			{"type":"function_call","call_id":"a","name":"tool_a","arguments":"{}"},
			{"type":"function_call","call_id":"b","name":"tool_b","arguments":"{}"},
			{"role":"user","content":"reminder before results"},
			{"type":"function_call_output","output":"output X"},
			{"type":"function_call_output","output":"output Y"},
			{"type":"function_call_output","call_id":"orphan_id","output":"output Z"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()

	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Get("role").String())
	}

	// Must stay in natural input order: assistant -> user ("reminder before results") -> user (X) -> user (Y) -> user (Z)
	// Outputs X and Y must NOT be guessed as tool calls "a" and "b" and must NOT be moved before reminder!
	if len(messages) != 5 {
		t.Fatalf("expected 5 messages, got %d: %s", len(messages), out)
	}
	if roles[0] != "assistant" || roles[1] != "user" {
		t.Fatalf("expected natural order with user reminder preserved at index 1 without guessing, got roles: %v (output=%s)", roles, out)
	}
	if messages[1].Get("content").String() != "reminder before results" {
		t.Fatalf("expected message[1] to be reminder, got: %s", messages[1].Raw)
	}
	// Verify no tool messages were created with guessed call IDs
	for _, m := range messages {
		if m.Get("role").String() == "tool" {
			t.Fatalf("unexpected tool message created via guessing: %s", m.Raw)
		}
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_PreservesDeveloperRoles(t *testing.T) {
	input := []byte(`{
		"instructions":"base instructions",
		"input":[
			{"type":"message","role":"developer","content":"mode: plan"},
			{"type":"message","role":"user","content":"continue"},
			{"type":"message","role":"developer","content":"mode: default"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("test-model", input, false)
	messages := gjson.GetBytes(out, "messages").Array()
	var roles []string
	for _, message := range messages {
		roles = append(roles, message.Get("role").String())
	}
	want := []string{"system", "developer", "user", "developer"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v; out=%s", roles, want, out)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v; out=%s", roles, want, out)
		}
	}
	if got := gjson.GetBytes(out, "messages.1.content").String(); got != "mode: plan" {
		t.Fatalf("developer content = %q, want %q", got, "mode: plan")
	}
}
