package responses

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func parseClaudeResponsesSSEEvent(t *testing.T, chunk []byte) (string, gjson.Result) {
	t.Helper()

	var event string
	var data string
	for _, line := range strings.Split(string(chunk), "\n") {
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if data == "" {
		t.Fatalf("SSE chunk has no data line: %s", string(chunk))
	}

	return event, gjson.Parse(data)
}

func TestConvertClaudeResponseToOpenAIResponses_ThinkingIncludesSignature(t *testing.T) {
	signature := "claude_sig_123"
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"internal "}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param)...)
	}

	var reasoningDone gjson.Result
	var completed gjson.Result
	for _, output := range outputs {
		event, data := parseClaudeResponsesSSEEvent(t, output)
		switch event {
		case "response.output_item.done":
			if data.Get("item.type").String() == "reasoning" {
				reasoningDone = data
			}
		case "response.completed":
			completed = data
		}
	}

	if !reasoningDone.Exists() {
		t.Fatal("expected reasoning output_item.done event")
	}
	if got := reasoningDone.Get("item.encrypted_content").String(); got != signature {
		t.Fatalf("reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := reasoningDone.Get("item.summary.0.text").String(); got != "internal reasoning" {
		t.Fatalf("reasoning summary text = %q", got)
	}
	if got := completed.Get("response.output.0.encrypted_content").String(); got != signature {
		t.Fatalf("completed reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := completed.Get("response.output.0.summary.0.text").String(); got != "internal reasoning" {
		t.Fatalf("completed reasoning summary text = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_ThinkingIncludesSignature(t *testing.T) {
	signature := "claude_sig_nonstream"
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_nonstream","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"nonstream reasoning"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.0.encrypted_content").String(); got != signature {
		t.Fatalf("non-stream reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := root.Get("output.0.summary.0.text").String(); got != "nonstream reasoning" {
		t.Fatalf("non-stream reasoning summary text = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_PreservesContentBlockOrder(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_nonstream_order","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_order","name":"exec_command","input":{}}}`,
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"pwd\"}"}}`,
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"done"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"content_block_stop","index":3}`,
		`data: {"type":"content_block_start","index":4,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":4,"delta":{"type":"thinking_delta","thinking":"more"}}`,
		`data: {"type":"content_block_stop","index":4}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	root := gjson.ParseBytes(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil))
	wantTypes := []string{"message", "reasoning", "function_call", "message", "reasoning"}
	if got := root.Get("output.#").Int(); got != int64(len(wantTypes)) {
		t.Fatalf("non-stream output count = %d, want %d", got, len(wantTypes))
	}
	for index, wantType := range wantTypes {
		if got := root.Get(fmt.Sprintf("output.%d.type", index)).String(); got != wantType {
			t.Fatalf("non-stream output.%d.type = %q, want %q", index, got, wantType)
		}
	}
	if got := root.Get("output.0.content.0.text").String(); got != "" {
		t.Fatalf("empty text block content = %q, want empty string", got)
	}
	if got := root.Get("output.1.summary.0.text").String(); got != "plan" {
		t.Fatalf("first reasoning text = %q, want %q", got, "plan")
	}
	if got := root.Get("output.2.call_id").String(); got != "call_order" {
		t.Fatalf("function call id = %q, want %q", got, "call_order")
	}
	if got := root.Get("output.2.arguments").String(); got != `{"cmd":"pwd"}` {
		t.Fatalf("function call arguments = %q, want %q", got, `{"cmd":"pwd"}`)
	}
	if got := root.Get("output.3.content.0.text").String(); got != "done" {
		t.Fatalf("second message text = %q, want %q", got, "done")
	}
	if got := root.Get("output.4.summary.0.text").String(); got != "more" {
		t.Fatalf("second reasoning text = %q, want %q", got, "more")
	}
	if got := root.Get("usage.output_tokens_details.reasoning_tokens").Int(); got != 2 {
		t.Fatalf("reasoning tokens = %d, want 2", got)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbols
// claudeResponsesTerminalState and claudeResponsesOutputStatus via
// ConvertClaudeResponseToOpenAIResponses.
func TestConvertClaudeResponseToOpenAIResponses_MaxTokensEmitsIncomplete(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_max_tokens","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"unfinished reasoning"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_max_tokens"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var incomplete gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.completed" {
				t.Fatalf("max_tokens response emitted response.completed: %s", output)
			}
			if event == "response.incomplete" {
				incomplete = data
			}
		}
	}

	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event")
	}
	if got := incomplete.Get("response.status").String(); got != "incomplete" {
		t.Fatalf("response.status = %q, want incomplete", got)
	}
	if got := incomplete.Get("response.incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete reason = %q, want max_output_tokens", got)
	}
	if got := incomplete.Get("response.output.0.type").String(); got != "reasoning" {
		t.Fatalf("response.output.0.type = %q, want reasoning; response=%s", got, incomplete.Raw)
	}
	if got := incomplete.Get("response.output.0.status").String(); got != "incomplete" {
		t.Fatalf("response.output.0.status = %q, want incomplete", got)
	}
	if got := incomplete.Get("response.output.0.summary.0.text").String(); got != "unfinished reasoning" {
		t.Fatalf("reasoning summary = %q, want unfinished reasoning", got)
	}
	if got := incomplete.Get("response.usage.output_tokens").Int(); got != 64000 {
		t.Fatalf("output_tokens = %d, want 64000", got)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbol
// ConvertClaudeResponseToOpenAIResponsesNonStream.
func TestConvertClaudeResponseToOpenAIResponsesNonStream_MaxTokensPreservesPartialText(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_partial","usage":{"input_tokens":10,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial answer"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-fable-5-1", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)
	if got := root.Get("status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete; response=%s", got, out)
	}
	if got := root.Get("incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete reason = %q, want max_output_tokens", got)
	}
	if got := root.Get("output.0.status").String(); got != "incomplete" {
		t.Fatalf("output.0.status = %q, want incomplete", got)
	}
	if got := root.Get("output.0.content.0.text").String(); got != "partial answer" {
		t.Fatalf("partial text = %q, want partial answer", got)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbol finalizeFuncItem via
// ConvertClaudeResponseToOpenAIResponses.
func TestConvertClaudeResponseToOpenAIResponses_MaxTokensWithToolCallAndTextEmitsIncomplete(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_tool_incomplete","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"calling tool"}}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_inc_1","name":"get_weather","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"San"}}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var incomplete gjson.Result
	var funcDone gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.output_item.done" && data.Get("item.type").String() == "function_call" {
				funcDone = data
			}
			if event == "response.incomplete" {
				incomplete = data
			}
		}
	}

	if !funcDone.Exists() {
		t.Fatal("expected function_call response.output_item.done event")
	}
	if got := funcDone.Get("item.status").String(); got != "incomplete" {
		t.Fatalf("funcDone item.status = %q, want incomplete", got)
	}
	if got := funcDone.Get("item.arguments").String(); got != "{\"city\":\"San" {
		t.Fatalf("funcDone item.arguments = %q, want '{\"city\":\"San'", got)
	}

	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event")
	}
	if got := incomplete.Get("response.status").String(); got != "incomplete" {
		t.Fatalf("response.status = %q, want incomplete", got)
	}
	if got := incomplete.Get("response.incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete reason = %q, want max_output_tokens", got)
	}
	if got := incomplete.Get("response.output.0.status").String(); got != "completed" {
		t.Fatalf("output.0.status = %q, want completed", got)
	}
	if got := incomplete.Get("response.output.0.content.0.text").String(); got != "calling tool" {
		t.Fatalf("output.0 text = %q, want 'calling tool'", got)
	}
	if got := incomplete.Get("response.output.1.status").String(); got != "incomplete" {
		t.Fatalf("output.1.status = %q, want incomplete", got)
	}
	if got := incomplete.Get("response.output.1.type").String(); got != "function_call" {
		t.Fatalf("output.1.type = %q, want function_call", got)
	}
	if got := incomplete.Get("response.output.1.arguments").String(); got != "{\"city\":\"San" {
		t.Fatalf("output.1.arguments = %q, want '{\"city\":\"San'", got)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbol
// finalizeWebSearchWithStatus via ConvertClaudeResponseToOpenAIResponses.
func TestConvertClaudeResponseToOpenAIResponses_MaxTokensWithWebSearchEmitsIncomplete(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_ws_incomplete","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"golang\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var incomplete gjson.Result
	var wsDone gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.output_item.done" && data.Get("item.type").String() == "web_search_call" {
				wsDone = data
			}
			if event == "response.incomplete" {
				incomplete = data
			}
		}
	}

	if !wsDone.Exists() {
		t.Fatal("expected web_search_call response.output_item.done event")
	}
	if got := wsDone.Get("item.status").String(); got != "incomplete" {
		t.Fatalf("wsDone item.status = %q, want incomplete", got)
	}
	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event")
	}
	if got := incomplete.Get("response.output.0.status").String(); got != "incomplete" {
		t.Fatalf("response.output.0.status = %q, want incomplete", got)
	}

	// Non-stream
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_ws_incomplete","usage":{"input_tokens":10,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"golang\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))
	nonStreamOut := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-fable-5-1", nil, nil, raw, nil)
	nsRoot := gjson.ParseBytes(nonStreamOut)
	if got := nsRoot.Get("status").String(); got != "incomplete" {
		t.Fatalf("ns status = %q, want incomplete", got)
	}
	if got := nsRoot.Get("output.0.status").String(); got != "incomplete" {
		t.Fatalf("ns output.0.status = %q, want incomplete", got)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbol finalizeReasoningItem
// via ConvertClaudeResponseToOpenAIResponses.
func TestConvertClaudeResponseToOpenAIResponses_MaxTokensReasoningWithoutBlockStop(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_reasoning_nostop","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"partial thought before cut"}}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var incomplete gjson.Result
	var rsDone gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.output_item.done" && data.Get("item.type").String() == "reasoning" {
				rsDone = data
			}
			if event == "response.incomplete" {
				incomplete = data
			}
		}
	}

	if !rsDone.Exists() {
		t.Fatal("expected reasoning response.output_item.done event")
	}
	if got := rsDone.Get("item.status").String(); got != "incomplete" {
		t.Fatalf("rsDone item.status = %q, want incomplete", got)
	}
	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event")
	}
	if got := incomplete.Get("response.output.0.status").String(); got != "incomplete" {
		t.Fatalf("response.output.0.status = %q, want incomplete", got)
	}
	if got := incomplete.Get("response.output.0.type").String(); got != "reasoning" {
		t.Fatalf("response.output.0.type = %q, want reasoning", got)
	}
	if got := incomplete.Get("response.output.0.summary.0.text").String(); got != "partial thought before cut" {
		t.Fatalf("summary = %q, want 'partial thought before cut'", got)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbol finalizeFuncItem via
// ConvertClaudeResponseToOpenAIResponses.
func TestConvertClaudeResponseToOpenAIResponses_MaxTokensWithToolBlockStopEmitsIncomplete(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_tool_blockstop","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_stop_1","name":"do_work","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"step\":1}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var incomplete gjson.Result
	var funcDone gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.output_item.done" && data.Get("item.type").String() == "function_call" {
				funcDone = data
			}
			if event == "response.incomplete" {
				incomplete = data
			}
		}
	}

	if !funcDone.Exists() {
		t.Fatal("expected function_call response.output_item.done event")
	}
	if got := funcDone.Get("item.status").String(); got != "incomplete" {
		t.Fatalf("funcDone item.status = %q, want incomplete", got)
	}
	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event")
	}
	if got := incomplete.Get("response.output.0.status").String(); got != "incomplete" {
		t.Fatalf("output.0.status = %q, want incomplete", got)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbol
// finalizeWebSearchWithStatus via ConvertClaudeResponseToOpenAIResponses.
func TestConvertClaudeResponseToOpenAIResponses_MaxTokensWithWebSearchResultsEmitsIncomplete(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_ws_res_incomplete","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_2","name":"web_search"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"golang\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srv_2","content":[{"type":"web_search_result","title":"Go","url":"https://golang.org"}]}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var incomplete gjson.Result
	var wsDone gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.output_item.done" && data.Get("item.type").String() == "web_search_call" {
				wsDone = data
			}
			if event == "response.incomplete" {
				incomplete = data
			}
		}
	}

	if !wsDone.Exists() {
		t.Fatal("expected web_search_call response.output_item.done event")
	}
	if got := wsDone.Get("item.status").String(); got != "incomplete" {
		t.Fatalf("wsDone item.status = %q, want incomplete", got)
	}
	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event")
	}
	if got := incomplete.Get("response.output.0.status").String(); got != "incomplete" {
		t.Fatalf("response.output.0.status = %q, want incomplete", got)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbols
// finalizeReasoningDeltas and finalizeReasoningItem via
// ConvertClaudeResponseToOpenAIResponses.
func TestConvertClaudeResponseToOpenAIResponses_ReasoningEventsNotDuplicated(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_rs_once","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"full thought"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":10}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	counts := make(map[string]int)
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", nil, nil, chunk, &param) {
			event, _ := parseClaudeResponsesSSEEvent(t, output)
			counts[event]++
		}
	}

	if counts["response.reasoning_summary_text.done"] != 1 {
		t.Fatalf("reasoning_summary_text.done count = %d, want 1", counts["response.reasoning_summary_text.done"])
	}
	if counts["response.reasoning_summary_part.done"] != 1 {
		t.Fatalf("reasoning_summary_part.done count = %d, want 1", counts["response.reasoning_summary_part.done"])
	}
	if counts["response.output_item.done"] != 1 {
		t.Fatalf("output_item.done count = %d, want 1", counts["response.output_item.done"])
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbol finalizeFuncItem via
// ConvertClaudeResponseToOpenAIResponses.
func TestConvertClaudeResponseToOpenAIResponses_EmptyFunctionArgsConsistentOnTruncation(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_empty_args_trunc","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_empty","name":"get_info","input":{}}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var funcDone gjson.Result
	var argsDone gjson.Result
	var incomplete gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.function_call_arguments.done" {
				argsDone = data
			}
			if event == "response.output_item.done" && data.Get("item.type").String() == "function_call" {
				funcDone = data
			}
			if event == "response.incomplete" {
				incomplete = data
			}
		}
	}

	if !argsDone.Exists() {
		t.Fatal("expected response.function_call_arguments.done event")
	}
	if !funcDone.Exists() {
		t.Fatal("expected function_call response.output_item.done event")
	}
	if got := argsDone.Get("arguments").String(); got != "" {
		t.Fatalf("argsDone arguments = %q, want empty string", got)
	}
	if got := funcDone.Get("item.arguments").String(); got != "" {
		t.Fatalf("funcDone arguments = %q, want empty string", got)
	}
	if got := funcDone.Get("item.status").String(); got != "incomplete" {
		t.Fatalf("funcDone item.status = %q, want incomplete", got)
	}
	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event")
	}
	if got := incomplete.Get("response.output.0.arguments").String(); got != "" {
		t.Fatalf("response.output.0.arguments = %q, want empty string", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_RestoresAdditionalNamespaceCustomToolCall(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"input":[{"type":"additional_tools","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}
		]}]
	}`)
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_custom","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_custom","name":"functions__exec","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"pwd\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var added, inputDone, done, completed gjson.Result
	functionEvents := 0
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", originalRequest, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			switch event {
			case "response.output_item.added":
				if data.Get("item.type").String() == "custom_tool_call" {
					added = data
				}
			case "response.custom_tool_call_input.done":
				inputDone = data
			case "response.output_item.done":
				if data.Get("item.type").String() == "custom_tool_call" {
					done = data
				}
			case "response.function_call_arguments.delta", "response.function_call_arguments.done":
				functionEvents++
			case "response.completed":
				completed = data
			}
		}
	}

	if !added.Exists() || !inputDone.Exists() || !done.Exists() || !completed.Exists() {
		t.Fatalf("missing custom tool lifecycle events: added=%v input_done=%v done=%v completed=%v", added.Exists(), inputDone.Exists(), done.Exists(), completed.Exists())
	}
	if functionEvents != 0 {
		t.Fatalf("function call events = %d, want 0", functionEvents)
	}
	for _, test := range []struct {
		label string
		item  gjson.Result
	}{
		{label: "added", item: added.Get("item")},
		{label: "done", item: done.Get("item")},
		{label: "completed", item: completed.Get("response.output.0")},
	} {
		if got := test.item.Get("name").String(); got != "exec" {
			t.Fatalf("%s name = %q, want exec", test.label, got)
		}
		if got := test.item.Get("namespace").String(); got != "functions" {
			t.Fatalf("%s namespace = %q, want functions", test.label, got)
		}
	}
	if got := inputDone.Get("input").String(); got != "pwd" {
		t.Fatalf("custom input.done input = %q, want pwd", got)
	}
	if got := done.Get("item.input").String(); got != "pwd" {
		t.Fatalf("done input = %q, want pwd", got)
	}
	if got := completed.Get("response.output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("completed output type = %q, want custom_tool_call", got)
	}
	if got := completed.Get("response.output.0.input").String(); got != "pwd" {
		t.Fatalf("completed input = %q, want pwd", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_DirectCustomWinsNamespaceCollision(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"tools":[
			{"type":"namespace","name":"n","tools":[{"type":"function","name":"x"}]},
			{"type":"custom","name":"n__x"}
		]
	}`)
	streamChunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_collision","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_collision","name":"n__x","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"pwd\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var streamCompleted gjson.Result
	for _, chunk := range streamChunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", originalRequest, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.completed" {
				streamCompleted = data
			}
		}
	}
	if got := streamCompleted.Get("response.output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("stream output type = %q, want custom_tool_call", got)
	}
	if got := streamCompleted.Get("response.output.0.input").String(); got != "pwd" {
		t.Fatalf("stream output input = %q, want pwd", got)
	}
	item := streamCompleted.Get("response.output.0")
	if got := item.Get("name").String(); got != "n__x" {
		t.Fatalf("name = %q, want n__x", got)
	}
	if item.Get("namespace").Exists() {
		t.Fatalf("unexpected namespace: %s", item.Get("namespace").Raw)
	}

	nonStreamRaw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_collision_nonstream","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_collision_nonstream","name":"n__x","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"pwd\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))
	nonStream := gjson.ParseBytes(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", originalRequest, nil, nonStreamRaw, nil))
	if got := nonStream.Get("output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("non-stream output type = %q, want custom_tool_call", got)
	}
	if got := nonStream.Get("output.0.input").String(); got != "pwd" {
		t.Fatalf("non-stream output input = %q, want pwd", got)
	}
	item = nonStream.Get("output.0")
	if got := item.Get("name").String(); got != "n__x" {
		t.Fatalf("name = %q, want n__x", got)
	}
	if item.Get("namespace").Exists() {
		t.Fatalf("unexpected namespace: %s", item.Get("namespace").Raw)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_RestoresAdditionalNamespaceCustomToolCall(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"input":[{"type":"additional_tools","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}
		]}]
	}`)
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_custom_nonstream","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_custom_nonstream","name":"functions__exec","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"pwd\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	root := gjson.ParseBytes(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", originalRequest, nil, raw, nil))
	if got := root.Get("output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("non-stream output type = %q, want custom_tool_call; output=%s", got, root.Raw)
	}
	if got := root.Get("output.0.input").String(); got != "pwd" {
		t.Fatalf("non-stream input = %q, want pwd", got)
	}
	if got := root.Get("output.0.call_id").String(); got != "call_custom_nonstream" {
		t.Fatalf("non-stream call_id = %q, want call_custom_nonstream", got)
	}
	if got := root.Get("output.0.name").String(); got != "exec" {
		t.Fatalf("non-stream name = %q, want exec; output=%s", got, root.Raw)
	}
	if got := root.Get("output.0.namespace").String(); got != "functions" {
		t.Fatalf("non-stream namespace = %q, want functions; output=%s", got, root.Raw)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_CustomToolEmptyInputMatchesNonStream(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"tools":[{"type":"custom","name":"exec"}]
	}`)
	streamChunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_custom_empty","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_custom_empty","name":"exec","input":{}}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var streamCompleted gjson.Result
	for _, chunk := range streamChunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", originalRequest, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.completed" {
				streamCompleted = data
			}
		}
	}
	if got := streamCompleted.Get("response.output.0.input").String(); got != "" {
		t.Fatalf("stream empty custom input = %q, want empty string", got)
	}

	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_custom_empty_nonstream","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_custom_empty","name":"exec","input":{}}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))
	nonStream := gjson.ParseBytes(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", originalRequest, nil, raw, nil))
	if got := nonStream.Get("output.0.input").String(); got != "" {
		t.Fatalf("non-stream empty custom input = %q, want empty string", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_RestoresNamespaceFunctionCall(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__node_repl",
				"tools":[{"type":"function","name":"js","parameters":{"type":"object","properties":{}}}]
			}
		]
	}`)
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_abc","name":"mcp__node_repl__js","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"code\":\"nodeRepl.write('hello')\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var added gjson.Result
	var done gjson.Result
	var completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", originalRequest, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			switch event {
			case "response.output_item.added":
				if data.Get("item.type").String() == "function_call" {
					added = data
				}
			case "response.output_item.done":
				if data.Get("item.type").String() == "function_call" {
					done = data
				}
			case "response.completed":
				completed = data
			}
		}
	}

	for _, tc := range []struct {
		label string
		got   gjson.Result
	}{
		{"added", added},
		{"done", done},
	} {
		if !tc.got.Exists() {
			t.Fatalf("expected function_call %s event", tc.label)
		}
		if got := tc.got.Get("item.name").String(); got != "js" {
			t.Fatalf("%s item.name = %q, want js", tc.label, got)
		}
		if got := tc.got.Get("item.namespace").String(); got != "mcp__node_repl" {
			t.Fatalf("%s item.namespace = %q, want mcp__node_repl", tc.label, got)
		}
	}

	if !completed.Exists() {
		t.Fatal("expected response.completed event")
	}
	if got := completed.Get("response.output.0.name").String(); got != "js" {
		t.Fatalf("completed output name = %q, want js", got)
	}
	if got := completed.Get("response.output.0.namespace").String(); got != "mcp__node_repl" {
		t.Fatalf("completed output namespace = %q, want mcp__node_repl", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_RestoresNamespaceFunctionCall(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__node_repl",
				"tools":[{"type":"function","name":"js","parameters":{"type":"object","properties":{}}}]
			}
		]
	}`)
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_nonstream","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_abc","name":"mcp__node_repl__js","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"code\":\"nodeRepl.write('hello')\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", originalRequest, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.0.name").String(); got != "js" {
		t.Fatalf("non-stream output name = %q, want js", got)
	}
	if got := root.Get("output.0.namespace").String(); got != "mcp__node_repl" {
		t.Fatalf("non-stream output namespace = %q, want mcp__node_repl", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_CustomToolTruncatedInputUnwrapped(t *testing.T) {
	originalRequest := []byte(`{"tools":[{"type":"custom","name":"bash"}]}`)
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_custom_trunc","usage":{"input_tokens":10,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_c1","name":"bash","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"echo \\u4F60"}}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var customDone gjson.Result
	var incomplete gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-fable-5-1", originalRequest, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.output_item.done" && data.Get("item.type").String() == "custom_tool_call" {
				customDone = data
			}
			if event == "response.incomplete" {
				incomplete = data
			}
		}
	}

	if !customDone.Exists() {
		t.Fatal("expected custom_tool_call response.output_item.done event")
	}
	if got := customDone.Get("item.input").String(); got != "echo 你" {
		t.Fatalf("customDone item.input = %q, want 'echo 你'", got)
	}
	if got := customDone.Get("item.status").String(); got != "incomplete" {
		t.Fatalf("customDone item.status = %q, want incomplete", got)
	}
	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event")
	}
	if got := incomplete.Get("response.output.0.input").String(); got != "echo 你" {
		t.Fatalf("response.output.0.input = %q, want 'echo 你'", got)
	}
	if got := incomplete.Get("response.output.0.status").String(); got != "incomplete" {
		t.Fatalf("response.output.0.status = %q, want incomplete", got)
	}
}

// The request body is picked and validated once when the per-stream state is
// created; later chunks reuse that pinned copy for tool metadata and the
// completed event instead of re-picking (and re-validating) the full body on
// every chunk. Regression test for issue #6665.
func TestConvertClaudeResponseToOpenAIResponses_PinsRequestPerStream(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"instructions":"pinned-instructions",
		"input":[{"type":"additional_tools","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}
		]}]
	}`)
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_pinned","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_pinned","name":"functions__exec","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"pwd\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var added, completed gjson.Result
	for i, chunk := range chunks {
		// Only the first chunk carries the request copy; the pinned state
		// must keep tool metadata and completed-event fields working even
		// when later chunks do not carry the request again.
		var requestForChunk []byte
		if i == 0 {
			requestForChunk = originalRequest
		}
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", requestForChunk, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			switch event {
			case "response.output_item.added":
				if data.Get("item.type").String() == "custom_tool_call" {
					added = data
				}
			case "response.completed":
				completed = data
			}
		}
	}

	if !added.Exists() || !completed.Exists() {
		t.Fatalf("missing lifecycle events: added=%v completed=%v", added.Exists(), completed.Exists())
	}
	if got := added.Get("item.name").String(); got != "exec" {
		t.Fatalf("added name = %q, want exec", got)
	}
	if got := added.Get("item.namespace").String(); got != "functions" {
		t.Fatalf("added namespace = %q, want functions", got)
	}
	if got := completed.Get("response.instructions").String(); got != "pinned-instructions" {
		t.Fatalf("completed instructions = %q, want pinned-instructions", got)
	}
}

// --- R6 byte-compat pin ------------------------------------------------------
// Hashes captured from the pre-port implementation and held identical across the
// per-stream namespace/winners + request-echo cache port in this package (plan
// v8040-p3, upstream f4a2d04).

var byteCompatHashes = map[string]string{
	"stream":    "0859f5a8a44db4dffed3b32a6f466fa7416a4c1bb8dfa5cdde0afaacc52de7a2",
	"nonstream": "a61699174cef1f30677afa968550551b10a994bf56e3d93d0190f8a7859b9374",
}

// byteCompatVolatile masks timestamp and synthesized-id byte runs (Unix seconds,
// UnixNano decimal or hex) so the pin holds everything else exactly; no field
// order, name, namespace or echo field is masked.
var byteCompatVolatile = regexp.MustCompile(`[0-9a-f]{10,}`)

func byteCompatHash(frames [][]byte) string {
	h := sha256.New()
	for _, frame := range frames {
		masked := byteCompatVolatile.ReplaceAll(frame, []byte("<volatile>"))
		fmt.Fprintf(h, "%d:", len(masked))
		_, _ = h.Write(masked)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func checkByteCompat(t *testing.T, name string, frames [][]byte) {
	t.Helper()
	got := byteCompatHash(frames)
	want := byteCompatHashes[name]
	if want == "" {
		t.Fatalf("bytecompat capture %s: %s", name, got)
	}
	if got != want {
		t.Fatalf("%s byte-compat hash = %s, want %s", name, got, want)
	}
}

func byteCompatClaudeResponsesRequest() []byte {
	return []byte(`{"model":"gpt-test","instructions":"pinned-instructions","max_output_tokens":42,"temperature":0.3,"metadata":{"k":"v"},"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`)
}

func byteCompatClaudeResponsesFrames() []string {
	return []string{
		`data: {"type":"message_start","message":{"id":"msg_pinned","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_pinned","name":"functions__exec","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"pwd\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}
}

func TestByteCompatClaudeResponses(t *testing.T) {
	request := byteCompatClaudeResponsesRequest()

	var param any
	var stream [][]byte
	for _, frame := range byteCompatClaudeResponsesFrames() {
		stream = append(stream, ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", request, nil, []byte(frame), &param)...)
	}
	checkByteCompat(t, "stream", stream)

	var raw []byte
	for _, frame := range byteCompatClaudeResponsesFrames()[:4] {
		raw = append(raw, []byte(frame+"\n")...)
	}
	nonStream := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", request, nil, raw, nil)
	checkByteCompat(t, "nonstream", [][]byte{nonStream})
}

func BenchmarkClaudeResponsesStreamTools(b *testing.B) {
	request := byteCompatClaudeResponsesRequest()
	frames := byteCompatClaudeResponsesFrames()
	b.ReportAllocs()
	for b.Loop() {
		var param any
		for _, frame := range frames {
			_ = ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", request, nil, []byte(frame), &param)
		}
	}
}

// claudeStopReasonChunks renders one finished Claude response with the given
// stop reason, for the terminal-state table below.
func claudeStopReasonChunks(reason string) [][]byte {
	return [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_table","usage":{"input_tokens":7,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial answer"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"` + reason + `"},"usage":{"output_tokens":3}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
}

type claudeStopReasonCase struct {
	name    string
	reason  string
	event   string
	status  string
	detail  string
	endTurn string
}

// TestClaudeResponsesStopReasonTerminalTable pins the Claude→Responses table
// through the streaming converter a handler calls. One terminal event per
// stream; a reason with no row leaves the response completed with no
// incomplete_details and no end_turn marker.
func TestClaudeResponsesStopReasonTerminalTable(t *testing.T) {
	for _, tc := range []claudeStopReasonCase{
		{name: "max_tokens", reason: "max_tokens", event: "response.incomplete", status: "incomplete", detail: "max_output_tokens"},
		{name: "model_context_window_exceeded", reason: "model_context_window_exceeded", event: "response.incomplete", status: "incomplete", detail: "max_output_tokens"},
		{name: "refusal", reason: "refusal", event: "response.incomplete", status: "incomplete", detail: "content_filter"},
		{name: "refusal wrapped", reason: " REFUSAL ", event: "response.incomplete", status: "incomplete", detail: "content_filter"},
		{name: "pause_turn", reason: "pause_turn", event: "response.completed", status: "completed", endTurn: "false"},
		{name: "pause_turn wrapped", reason: " PAUSE_TURN ", event: "response.completed", status: "completed", endTurn: "false"},
		{name: "end_turn", reason: "end_turn", event: "response.completed", status: "completed"},
		{name: "tool_use", reason: "tool_use", event: "response.completed", status: "completed"},
		{name: "stop_sequence", reason: "stop_sequence", event: "response.completed", status: "completed"},
		{name: "sensitive unmapped", reason: "sensitive", event: "response.completed", status: "completed"},
		{name: "unknown reason", reason: "some_future_reason", event: "response.completed", status: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var param any
			terminals := 0
			var event string
			var response gjson.Result
			for _, chunk := range claudeStopReasonChunks(tc.reason) {
				for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param) {
					name, data := parseClaudeResponsesSSEEvent(t, output)
					switch name {
					case "response.completed", "response.incomplete", "response.failed":
						terminals++
						event, response = name, data.Get("response")
					}
				}
			}
			if terminals != 1 {
				t.Fatalf("terminal events = %d, want 1", terminals)
			}
			if event != tc.event {
				t.Fatalf("terminal = %q, want %q", event, tc.event)
			}
			if got := response.Get("status").String(); got != tc.status {
				t.Fatalf("status = %q, want %q", got, tc.status)
			}
			if got := response.Get("incomplete_details.reason").String(); got != tc.detail {
				t.Fatalf("incomplete_details.reason = %q, want %q", got, tc.detail)
			}
			if got := response.Get("end_turn").Raw; got != tc.endTurn {
				t.Fatalf("end_turn = %q, want %q", got, tc.endTurn)
			}
		})
	}
}

// TestClaudeResponsesStopReasonNonStreamTable asserts the same table through the
// non-streaming converter, on the top-level response object.
func TestClaudeResponsesStopReasonNonStreamTable(t *testing.T) {
	for _, tc := range []claudeStopReasonCase{
		{name: "max_tokens", reason: "max_tokens", status: "incomplete", detail: "max_output_tokens"},
		{name: "model_context_window_exceeded", reason: "model_context_window_exceeded", status: "incomplete", detail: "max_output_tokens"},
		{name: "refusal", reason: "refusal", status: "incomplete", detail: "content_filter"},
		{name: "pause_turn", reason: "pause_turn", status: "completed", endTurn: "false"},
		{name: "end_turn", reason: "end_turn", status: "completed"},
		{name: "unknown reason", reason: "some_future_reason", status: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			for _, chunk := range claudeStopReasonChunks(tc.reason) {
				raw = append(append(raw, chunk...), '\n')
			}
			out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
			root := gjson.ParseBytes(out)
			if got := root.Get("status").String(); got != tc.status {
				t.Fatalf("status = %q, want %q; out=%s", got, tc.status, out)
			}
			if got := root.Get("incomplete_details.reason").String(); got != tc.detail {
				t.Fatalf("incomplete_details.reason = %q, want %q; out=%s", got, tc.detail, out)
			}
			if got := root.Get("end_turn").Raw; got != tc.endTurn {
				t.Fatalf("end_turn = %q, want %q; out=%s", got, tc.endTurn, out)
			}
			if got := root.Get("output.0.content.0.text").String(); got != "partial answer" {
				t.Fatalf("text = %q, want %q; out=%s", got, "partial answer", out)
			}
			if got := root.Get("usage.output_tokens").Int(); got != 3 {
				t.Fatalf("usage.output_tokens = %d, want 3; out=%s", got, out)
			}
		})
	}
}
