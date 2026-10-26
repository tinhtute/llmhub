package responses

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func parseOpenAIResponsesSSEEvent(t *testing.T, chunk []byte) (string, gjson.Result) {
	t.Helper()

	lines := strings.Split(string(chunk), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected SSE chunk: %q", chunk)
	}

	event := strings.TrimSpace(strings.TrimPrefix(lines[0], "event:"))
	dataLine := strings.TrimSpace(strings.TrimPrefix(lines[1], "data:"))
	if !gjson.Valid(dataLine) {
		t.Fatalf("invalid SSE data JSON: %q", dataLine)
	}
	return event, gjson.Parse(dataLine)
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_ResponseCompletedWaitsForDone(t *testing.T) {
	t.Parallel()

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	tests := []struct {
		name           string
		in             []string
		doneInputIndex int // Index in tt.in where the terminal [DONE] chunk arrives and response.completed must be emitted.
		hasUsage       bool
		inputTokens    int64
		outputTokens   int64
		totalTokens    int64
	}{
		{
			// A provider may send finish_reason first and only attach usage in a later chunk (e.g. Vertex AI),
			// so response.completed must wait for [DONE] to include that usage.
			name: "late usage after finish reason",
			in: []string{
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_late_usage","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
				`data: [DONE]`,
			},
			doneInputIndex: 3,
			hasUsage:       true,
			inputTokens:    11,
			outputTokens:   7,
			totalTokens:    18,
		},
		{
			// When usage arrives on the same chunk as finish_reason, we still expect a
			// single response.completed event and it should remain deferred until [DONE].
			name: "usage on finish reason chunk",
			in: []string{
				`data: {"id":"resp_usage_same_chunk","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_usage_same_chunk","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_usage_same_chunk","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":13,"completion_tokens":5,"total_tokens":18}}`,
				`data: [DONE]`,
			},
			doneInputIndex: 2,
			hasUsage:       true,
			inputTokens:    13,
			outputTokens:   5,
			totalTokens:    18,
		},
		{
			// An OpenAI-compatible streams from a buggy server might never send usage, so response.completed should
			// still wait for [DONE] but omit the usage object entirely.
			name: "no usage chunk",
			in: []string{
				`data: {"id":"resp_no_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_no_usage","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_no_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			},
			doneInputIndex: 2,
			hasUsage:       false,
		},
		{
			// A stream that ends without any finish_reason must still be finalized as completed on [DONE].
			name: "no finish reason",
			in: []string{
				`data: {"id":"resp_no_finish_reason","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
				`data: [DONE]`,
			},
			doneInputIndex: 1,
			hasUsage:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completedCount := 0
			completedInputIndex := -1
			var completedData gjson.Result

			// Reuse converter state across input lines to simulate one streaming response.
			var param any

			for i, line := range tt.in {
				// One upstream chunk can emit multiple downstream SSE events.
				for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param) {
					event, data := parseOpenAIResponsesSSEEvent(t, chunk)
					if event != "response.completed" {
						continue
					}

					completedCount++
					completedInputIndex = i
					completedData = data
					if i < tt.doneInputIndex {
						t.Fatalf("unexpected early response.completed on input index %d", i)
					}
				}
			}

			if completedCount != 1 {
				t.Fatalf("expected exactly 1 response.completed event, got %d", completedCount)
			}
			if completedInputIndex != tt.doneInputIndex {
				t.Fatalf("expected response.completed on terminal [DONE] chunk at input index %d, got %d", tt.doneInputIndex, completedInputIndex)
			}

			// Missing upstream usage should stay omitted in the final completed event.
			if !tt.hasUsage {
				if completedData.Get("response.usage").Exists() {
					t.Fatalf("expected response.completed to omit usage when none was provided, got %s", completedData.Get("response.usage").Raw)
				}
				return
			}

			// When usage is present, the final response.completed event must preserve the usage values.
			if got := completedData.Get("response.usage.input_tokens").Int(); got != tt.inputTokens {
				t.Fatalf("unexpected response.usage.input_tokens: got %d want %d", got, tt.inputTokens)
			}
			if got := completedData.Get("response.usage.output_tokens").Int(); got != tt.outputTokens {
				t.Fatalf("unexpected response.usage.output_tokens: got %d want %d", got, tt.outputTokens)
			}
			if got := completedData.Get("response.usage.total_tokens").Int(); got != tt.totalTokens {
				t.Fatalf("unexpected response.usage.total_tokens: got %d want %d", got, tt.totalTokens)
			}
		})
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_MultipleToolCallsRemainSeparate(t *testing.T) {
	in := []string{
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\",\"limit\":400,\"offset\":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":"call_glob","type":"function","function":{"name":"glob","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"function":{"arguments":"{\"path\":\"C:\\\\repo\",\"pattern\":\"*.{yml,yaml}\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_test","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":10,"total_tokens":20,"prompt_tokens":10}}`,
		`data: [DONE]`,
	}

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	addedNames := map[string]string{}
	doneArgs := map[string]string{}
	doneNames := map[string]string{}
	outputItems := map[string]gjson.Result{}

	for _, chunk := range out {
		ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
		switch ev {
		case "response.output_item.added":
			if data.Get("item.type").String() != "function_call" {
				continue
			}
			addedNames[data.Get("item.call_id").String()] = data.Get("item.name").String()
		case "response.output_item.done":
			if data.Get("item.type").String() != "function_call" {
				continue
			}
			callID := data.Get("item.call_id").String()
			doneArgs[callID] = data.Get("item.arguments").String()
			doneNames[callID] = data.Get("item.name").String()
		case "response.completed":
			output := data.Get("response.output")
			for _, item := range output.Array() {
				if item.Get("type").String() == "function_call" {
					outputItems[item.Get("call_id").String()] = item
				}
			}
		}
	}

	if len(addedNames) != 2 {
		t.Fatalf("expected 2 function_call added events, got %d", len(addedNames))
	}
	if len(doneArgs) != 2 {
		t.Fatalf("expected 2 function_call done events, got %d", len(doneArgs))
	}

	if addedNames["call_read"] != "read" {
		t.Fatalf("unexpected added name for call_read: %q", addedNames["call_read"])
	}
	if addedNames["call_glob"] != "glob" {
		t.Fatalf("unexpected added name for call_glob: %q", addedNames["call_glob"])
	}

	if !gjson.Valid(doneArgs["call_read"]) {
		t.Fatalf("invalid JSON args for call_read: %q", doneArgs["call_read"])
	}
	if !gjson.Valid(doneArgs["call_glob"]) {
		t.Fatalf("invalid JSON args for call_glob: %q", doneArgs["call_glob"])
	}
	if strings.Contains(doneArgs["call_read"], "}{") {
		t.Fatalf("call_read args were concatenated: %q", doneArgs["call_read"])
	}
	if strings.Contains(doneArgs["call_glob"], "}{") {
		t.Fatalf("call_glob args were concatenated: %q", doneArgs["call_glob"])
	}

	if doneNames["call_read"] != "read" {
		t.Fatalf("unexpected done name for call_read: %q", doneNames["call_read"])
	}
	if doneNames["call_glob"] != "glob" {
		t.Fatalf("unexpected done name for call_glob: %q", doneNames["call_glob"])
	}

	if got := gjson.Get(doneArgs["call_read"], "filePath").String(); got != `C:\repo` {
		t.Fatalf("unexpected filePath for call_read: %q", got)
	}
	if got := gjson.Get(doneArgs["call_glob"], "path").String(); got != `C:\repo` {
		t.Fatalf("unexpected path for call_glob: %q", got)
	}
	if got := gjson.Get(doneArgs["call_glob"], "pattern").String(); got != "*.{yml,yaml}" {
		t.Fatalf("unexpected pattern for call_glob: %q", got)
	}

	if len(outputItems) != 2 {
		t.Fatalf("expected 2 function_call items in response.output, got %d", len(outputItems))
	}
	if outputItems["call_read"].Get("name").String() != "read" {
		t.Fatalf("unexpected response.output name for call_read: %q", outputItems["call_read"].Get("name").String())
	}
	if outputItems["call_glob"].Get("name").String() != "glob" {
		t.Fatalf("unexpected response.output name for call_glob: %q", outputItems["call_glob"].Get("name").String())
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_MultiChoiceToolCallsUseDistinctOutputIndexes(t *testing.T) {
	in := []string{
		`data: {"id":"resp_multi_choice","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_choice0","type":"function","function":{"name":"glob","arguments":""}}]},"finish_reason":null},{"index":1,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_choice1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_multi_choice","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"C:\\\\repo\",\"pattern\":\"*.go\"}"}}]},"finish_reason":null},{"index":1,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\",\"limit\":20,\"offset\":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_multi_choice","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"tool_calls"},{"index":1,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":10,"total_tokens":20,"prompt_tokens":10}}`,
		`data: [DONE]`,
	}

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	type fcEvent struct {
		outputIndex int64
		name        string
		arguments   string
	}

	added := map[string]fcEvent{}
	done := map[string]fcEvent{}

	for _, chunk := range out {
		ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
		switch ev {
		case "response.output_item.added":
			if data.Get("item.type").String() != "function_call" {
				continue
			}
			callID := data.Get("item.call_id").String()
			added[callID] = fcEvent{
				outputIndex: data.Get("output_index").Int(),
				name:        data.Get("item.name").String(),
			}
		case "response.output_item.done":
			if data.Get("item.type").String() != "function_call" {
				continue
			}
			callID := data.Get("item.call_id").String()
			done[callID] = fcEvent{
				outputIndex: data.Get("output_index").Int(),
				name:        data.Get("item.name").String(),
				arguments:   data.Get("item.arguments").String(),
			}
		}
	}

	if len(added) != 2 {
		t.Fatalf("expected 2 function_call added events, got %d", len(added))
	}
	if len(done) != 2 {
		t.Fatalf("expected 2 function_call done events, got %d", len(done))
	}

	if added["call_choice0"].name != "glob" {
		t.Fatalf("unexpected added name for call_choice0: %q", added["call_choice0"].name)
	}
	if added["call_choice1"].name != "read" {
		t.Fatalf("unexpected added name for call_choice1: %q", added["call_choice1"].name)
	}
	if added["call_choice0"].outputIndex == added["call_choice1"].outputIndex {
		t.Fatalf("expected distinct output indexes for different choices, both got %d", added["call_choice0"].outputIndex)
	}

	if !gjson.Valid(done["call_choice0"].arguments) {
		t.Fatalf("invalid JSON args for call_choice0: %q", done["call_choice0"].arguments)
	}
	if !gjson.Valid(done["call_choice1"].arguments) {
		t.Fatalf("invalid JSON args for call_choice1: %q", done["call_choice1"].arguments)
	}
	if done["call_choice0"].outputIndex == done["call_choice1"].outputIndex {
		t.Fatalf("expected distinct done output indexes for different choices, both got %d", done["call_choice0"].outputIndex)
	}
	if done["call_choice0"].name != "glob" {
		t.Fatalf("unexpected done name for call_choice0: %q", done["call_choice0"].name)
	}
	if done["call_choice1"].name != "read" {
		t.Fatalf("unexpected done name for call_choice1: %q", done["call_choice1"].name)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_MixedMessageAndToolUseDistinctOutputIndexes(t *testing.T) {
	in := []string{
		`data: {"id":"resp_mixed","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"hello","reasoning_content":null,"tool_calls":null},"finish_reason":null},{"index":1,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_choice1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_mixed","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"stop"},{"index":1,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\",\"limit\":20,\"offset\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":10,"total_tokens":20,"prompt_tokens":10}}`,
		`data: [DONE]`,
	}

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	var messageOutputIndex int64 = -1
	var toolOutputIndex int64 = -1

	for _, chunk := range out {
		ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
		if ev != "response.output_item.added" {
			continue
		}
		switch data.Get("item.type").String() {
		case "message":
			if data.Get("item.id").String() == "msg_resp_mixed_0" {
				messageOutputIndex = data.Get("output_index").Int()
			}
		case "function_call":
			if data.Get("item.call_id").String() == "call_choice1" {
				toolOutputIndex = data.Get("output_index").Int()
			}
		}
	}

	if messageOutputIndex < 0 {
		t.Fatal("did not find message output index")
	}
	if toolOutputIndex < 0 {
		t.Fatal("did not find tool output index")
	}
	if messageOutputIndex == toolOutputIndex {
		t.Fatalf("expected distinct output indexes for message and tool call, both got %d", messageOutputIndex)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_FunctionCallDoneAndCompletedOutputStayAscending(t *testing.T) {
	in := []string{
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_glob","type":"function","function":{"name":"glob","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"C:\\\\repo\",\"pattern\":\"*.go\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":"call_read","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\",\"limit\":20,\"offset\":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_order","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":10,"total_tokens":20,"prompt_tokens":10}}`,
		`data: [DONE]`,
	}

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	var doneIndexes []int64
	var completedOrder []string

	for _, chunk := range out {
		ev, data := parseOpenAIResponsesSSEEvent(t, chunk)
		switch ev {
		case "response.output_item.done":
			if data.Get("item.type").String() == "function_call" {
				doneIndexes = append(doneIndexes, data.Get("output_index").Int())
			}
		case "response.completed":
			for _, item := range data.Get("response.output").Array() {
				if item.Get("type").String() == "function_call" {
					completedOrder = append(completedOrder, item.Get("call_id").String())
				}
			}
		}
	}

	if len(doneIndexes) != 2 {
		t.Fatalf("expected 2 function_call done indexes, got %d", len(doneIndexes))
	}
	if doneIndexes[0] >= doneIndexes[1] {
		t.Fatalf("expected ascending done output indexes, got %v", doneIndexes)
	}
	if len(completedOrder) != 2 {
		t.Fatalf("expected 2 function_call items in completed output, got %d", len(completedOrder))
	}
	if completedOrder[0] != "call_glob" || completedOrder[1] != "call_read" {
		t.Fatalf("unexpected completed function_call order: %v", completedOrder)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_RestoresNamespacedAndCustomDeclarations(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"repo","tools":[
			{"type":"function","name":"read"},
			{"type":"custom","name":"patch"}
		]}]
	}`)
	in := []string{
		`data: {"id":"resp_tools","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_read","function":{"name":"repo__read","arguments":"{}"}},{"index":1,"id":"call_patch","function":{"name":"repo__patch","arguments":"{\"input\":\"diff\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_tools","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}

	var param any
	var out [][]byte
	for _, line := range in {
		out = append(out, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param)...)
	}

	seenFunction := false
	seenCustom := false
	seenCompletedFunction := false
	seenCompletedCustom := false
	for _, chunk := range out {
		event, data := parseOpenAIResponsesSSEEvent(t, chunk)
		switch event {
		case "response.output_item.done":
			switch data.Get("item.call_id").String() {
			case "call_read":
				seenFunction = data.Get("item.type").String() == "function_call" && data.Get("item.name").String() == "read" && data.Get("item.namespace").String() == "repo"
			case "call_patch":
				seenCustom = data.Get("item.type").String() == "custom_tool_call" && data.Get("item.name").String() == "patch" && data.Get("item.namespace").String() == "repo" && data.Get("item.input").String() == "diff"
			}
		case "response.completed":
			for _, item := range data.Get("response.output").Array() {
				switch item.Get("call_id").String() {
				case "call_read":
					seenCompletedFunction = item.Get("name").String() == "read" && item.Get("namespace").String() == "repo"
				case "call_patch":
					seenCompletedCustom = item.Get("type").String() == "custom_tool_call" && item.Get("input").String() == "diff"
				}
			}
		}
	}
	if !seenFunction || !seenCustom || !seenCompletedFunction || !seenCompletedCustom {
		t.Fatalf("missing restored tool events: function=%v custom=%v completed_function=%v completed_custom=%v", seenFunction, seenCustom, seenCompletedFunction, seenCompletedCustom)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_FragmentedCustomInputDeltasExcludeWrapper(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"patch"}]}`)
	in := []string{
		`data: {"id":"resp_custom_fragments","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"patch","arguments":"{\"in"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_custom_fragments","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"put\":\"d"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_custom_fragments","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"if"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_custom_fragments","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"f\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_custom_fragments","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}

	var param any
	var delta strings.Builder
	deltaCount := 0
	seenInputDone := false
	seenItemDone := false
	for _, line := range in {
		for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param) {
			event, data := parseOpenAIResponsesSSEEvent(t, chunk)
			switch event {
			case "response.custom_tool_call_input.delta":
				deltaCount++
				delta.WriteString(data.Get("delta").String())
				if got := data.Get("item_id").String(); got != "ctc_call_patch" {
					t.Fatalf("custom delta item_id = %q, want ctc_call_patch", got)
				}
			case "response.custom_tool_call_input.done":
				seenInputDone = data.Get("item_id").String() == "ctc_call_patch" && data.Get("input").String() == "diff"
			case "response.output_item.done":
				if data.Get("item.call_id").String() == "call_patch" {
					seenItemDone = data.Get("item.id").String() == "ctc_call_patch" && data.Get("item.type").String() == "custom_tool_call" && data.Get("item.input").String() == "diff"
				}
			case "response.function_call_arguments.delta", "response.function_call_arguments.done":
				t.Fatalf("custom call emitted function argument event %q: %s", event, data.Raw)
			}
		}
	}
	if deltaCount == 0 {
		t.Fatal("custom call emitted no input delta events")
	}
	if got := delta.String(); got != "diff" {
		t.Fatalf("custom input deltas = %q, want diff", got)
	}
	if !seenInputDone || !seenItemDone {
		t.Fatalf("custom completion inconsistent: input_done=%v item_done=%v", seenInputDone, seenItemDone)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream_RestoresNamespacedAndCustomDeclarations(t *testing.T) {
	request := []byte(`{
		"tools":[
			{"type":"namespace","name":"repo","tools":[{"type":"function","name":"read"}]},
			{"type":"custom","name":"patch"}
		]
	}`)
	response := []byte(`{"id":"resp_tools","created":1,"choices":[{"index":0,"message":{"tool_calls":[
		{"id":"call_read","function":{"name":"repo__read","arguments":"{}"}},
		{"id":"call_patch","function":{"name":"patch","arguments":"{\"input\":\"diff\"}"}}
	]}}]}`)

	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "model", request, request, response, nil)
	if got := gjson.GetBytes(out, "output.0.name").String(); got != "read" {
		t.Fatalf("function name = %q, want read: %s", got, out)
	}
	if got := gjson.GetBytes(out, "output.0.namespace").String(); got != "repo" {
		t.Fatalf("function namespace = %q, want repo: %s", got, out)
	}
	if got := gjson.GetBytes(out, "output.1.type").String(); got != "custom_tool_call" {
		t.Fatalf("custom type = %q, want custom_tool_call: %s", got, out)
	}
	if got := gjson.GetBytes(out, "output.1.input").String(); got != "diff" {
		t.Fatalf("custom input = %q, want diff: %s", got, out)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_FragmentedToolNames(t *testing.T) {
	request := []byte(`{"tools":[{"type":"namespace","name":"repo","tools":[{"type":"custom","name":"patch"},{"type":"function","name":"patch_extra"},{"type":"function","name":"read"}]}]}`)
	inputs := [][]byte{
		[]byte(`{"id":"resp_fragmented_names","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"repo__pa","arguments":"{\"in"}},{"index":1,"id":"call_read","type":"function","function":{"name":"repo__r","arguments":"{\"pa"}},{"index":2,"id":"call_other","type":"function","function":{"name":"external_","arguments":"{\"q\":"}}]},"finish_reason":null}]}`),
		[]byte(`{"id":"resp_fragmented_names","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"name":"tool","arguments":"\"x\"}"}},{"index":0,"function":{"name":"tch","arguments":"put\":\"x\"}"}},{"index":1,"function":{"name":"ead","arguments":"th\":\"y\"}"}}]},"finish_reason":null}]}`),
		[]byte(`{"id":"resp_fragmented_names","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		[]byte(`data: [DONE]`),
	}

	var param any
	var chunksByInput [][][]byte
	for _, input := range inputs {
		chunksByInput = append(chunksByInput, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, input, &param))
	}
	for _, chunk := range chunksByInput[0] {
		event, data := parseOpenAIResponsesSSEEvent(t, chunk)
		if event == "response.output_item.added" && strings.Contains(data.Get("item.type").String(), "tool_call") {
			t.Fatalf("partial tool name emitted output item: %s", data.Raw)
		}
	}
	for _, chunk := range chunksByInput[1] {
		event, data := parseOpenAIResponsesSSEEvent(t, chunk)
		if event != "response.output_item.added" {
			continue
		}
		switch data.Get("item.call_id").String() {
		case "call_patch":
			t.Fatalf("exact name that prefixes a longer declaration emitted before terminal finish: %s", data.Raw)
		case "call_other":
			t.Fatalf("unresolved ordinary name emitted before terminal finish: %s", data.Raw)
		}
	}

	added := make(map[string]gjson.Result)
	var deltas = map[string]*strings.Builder{
		"ctc_call_patch": {},
		"fc_call_read":   {},
		"fc_call_other":  {},
	}
	inputDone := make(map[string]gjson.Result)
	itemDone := make(map[string]gjson.Result)
	var completed gjson.Result
	for _, chunks := range chunksByInput {
		for _, chunk := range chunks {
			event, data := parseOpenAIResponsesSSEEvent(t, chunk)
			switch event {
			case "response.output_item.added":
				if callID := data.Get("item.call_id").String(); callID != "" {
					added[data.Get("item.id").String()] = data
				}
			case "response.custom_tool_call_input.delta":
				itemID := data.Get("item_id").String()
				if _, ok := added[itemID]; !ok {
					t.Fatalf("custom delta preceded output_item.added: %s", data.Raw)
				}
				builder := deltas[itemID]
				if builder == nil {
					t.Fatalf("unexpected fragmented-call item ID %q: %s", itemID, data.Raw)
				}
				builder.WriteString(data.Get("delta").String())
			case "response.function_call_arguments.delta":
				itemID := data.Get("item_id").String()
				if _, ok := added[itemID]; !ok {
					t.Fatalf("function delta preceded output_item.added: %s", data.Raw)
				}
				builder := deltas[itemID]
				if builder == nil {
					t.Fatalf("unexpected fragmented-call item ID %q: %s", itemID, data.Raw)
				}
				builder.WriteString(data.Get("delta").String())
			case "response.custom_tool_call_input.done", "response.function_call_arguments.done":
				inputDone[data.Get("item_id").String()] = data
			case "response.output_item.done":
				if itemID := data.Get("item.id").String(); itemID != "" {
					itemDone[itemID] = data
				}
			case "response.completed":
				completed = data
			}
		}
	}

	patch := added["ctc_call_patch"]
	if patch.Get("item.type").String() != "custom_tool_call" || patch.Get("item.name").String() != "patch" || patch.Get("item.namespace").String() != "repo" || patch.Get("output_index").Int() != 0 {
		t.Fatalf("fragmented custom declaration restored incorrectly: %s", patch.Raw)
	}
	read := added["fc_call_read"]
	if read.Get("item.type").String() != "function_call" || read.Get("item.name").String() != "read" || read.Get("item.namespace").String() != "repo" || read.Get("output_index").Int() != 1 {
		t.Fatalf("fragmented function declaration restored incorrectly: %s", read.Raw)
	}
	other := added["fc_call_other"]
	if other.Get("item.type").String() != "function_call" || other.Get("item.name").String() != "external_tool" || other.Get("item.namespace").Exists() || other.Get("output_index").Int() != 2 {
		t.Fatalf("unresolved ordinary function restored incorrectly: %s", other.Raw)
	}
	if got := deltas["ctc_call_patch"].String(); got != "x" {
		t.Fatalf("custom input deltas = %q, want x", got)
	}
	if got := deltas["fc_call_read"].String(); got != `{"path":"y"}` {
		t.Fatalf("declared function argument deltas = %q", got)
	}
	if got := deltas["fc_call_other"].String(); got != `{"q":"x"}` {
		t.Fatalf("ordinary function argument deltas = %q", got)
	}
	if done := inputDone["ctc_call_patch"]; done.Get("type").String() != "response.custom_tool_call_input.done" || done.Get("input").String() != "x" {
		t.Fatalf("fragmented custom input.done inconsistent: %s", done.Raw)
	}
	if done := itemDone["ctc_call_patch"]; done.Get("item.type").String() != "custom_tool_call" || done.Get("item.name").String() != "patch" || done.Get("item.namespace").String() != "repo" || done.Get("item.input").String() != "x" {
		t.Fatalf("fragmented custom output_item.done inconsistent: %s", done.Raw)
	}
	for itemID, wantArgs := range map[string]string{"fc_call_read": `{"path":"y"}`, "fc_call_other": `{"q":"x"}`} {
		if done := inputDone[itemID]; done.Get("type").String() != "response.function_call_arguments.done" || done.Get("arguments").String() != wantArgs {
			t.Fatalf("fragmented function arguments.done inconsistent for %s: %s", itemID, done.Raw)
		}
		if done := itemDone[itemID]; done.Get("item.type").String() != "function_call" || done.Get("item.arguments").String() != wantArgs {
			t.Fatalf("fragmented function output_item.done inconsistent for %s: %s", itemID, done.Raw)
		}
	}
	wantCompleted := map[string]struct {
		typeName  string
		name      string
		namespace string
		valuePath string
		value     string
	}{
		"ctc_call_patch": {typeName: "custom_tool_call", name: "patch", namespace: "repo", valuePath: "input", value: "x"},
		"fc_call_read":   {typeName: "function_call", name: "read", namespace: "repo", valuePath: "arguments", value: `{"path":"y"}`},
		"fc_call_other":  {typeName: "function_call", name: "external_tool", valuePath: "arguments", value: `{"q":"x"}`},
	}
	for _, item := range completed.Get("response.output").Array() {
		want, ok := wantCompleted[item.Get("id").String()]
		if !ok {
			continue
		}
		if item.Get("type").String() != want.typeName || item.Get("name").String() != want.name || item.Get("namespace").String() != want.namespace || item.Get(want.valuePath).String() != want.value {
			t.Fatalf("completed fragmented call inconsistent: %s", item.Raw)
		}
		delete(wantCompleted, item.Get("id").String())
	}
	if len(wantCompleted) != 0 {
		t.Fatalf("completed response missing fragmented calls: %v", wantCompleted)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_CustomInputFidelity(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		wantInput string
	}{
		{name: "raw brace text", arguments: `{diff --git a/file b/file`, wantInput: `{diff --git a/file b/file`},
		{name: "incomplete wrapper", arguments: `{"input":"diff`, wantInput: `{"input":"diff`},
		{name: "raw JSON with extra member", arguments: `{"input":"literal","other":1}`, wantInput: `{"input":"literal","other":1` + `}`},
		{name: "exact compatibility wrapper", arguments: `{"input":"literal"}`, wantInput: "literal"},
		{name: "leading whitespace exact wrapper", arguments: " \n { \t\"input\" : \"snow \\u96ea\" } \r", wantInput: "snow 雪"},
		{name: "null input remains raw", arguments: `{"input":null}`, wantInput: `{"input":null}`},
	}

	request := []byte(`{"tools":[{"type":"custom","name":"patch"}]}`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var param any
			var chunks [][]byte
			for i := 0; i < len(tt.arguments); i++ {
				upstream := []byte(`{"id":"resp_custom_fidelity","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]},"finish_reason":null}]}`)
				if i == 0 {
					upstream, _ = sjson.SetBytes(upstream, "choices.0.delta.tool_calls.0.id", "call_patch")
					upstream, _ = sjson.SetBytes(upstream, "choices.0.delta.tool_calls.0.type", "function")
					upstream, _ = sjson.SetBytes(upstream, "choices.0.delta.tool_calls.0.function.name", "patch")
				}
				upstream, _ = sjson.SetBytes(upstream, "choices.0.delta.tool_calls.0.function.arguments", tt.arguments[i:i+1])
				chunks = append(chunks, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, upstream, &param)...)
			}
			if tt.arguments == "" {
				upstream := []byte(`{"id":"resp_custom_fidelity","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"patch","arguments":""}}]},"finish_reason":null}]}`)
				chunks = append(chunks, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, upstream, &param)...)
			}
			chunks = append(chunks, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(`{"id":"resp_custom_fidelity","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`), &param)...)
			chunks = append(chunks, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(`data: [DONE]`), &param)...)

			var delta strings.Builder
			var doneInput, itemInput, completedInput string
			for _, chunk := range chunks {
				event, data := parseOpenAIResponsesSSEEvent(t, chunk)
				switch event {
				case "response.custom_tool_call_input.delta":
					delta.WriteString(data.Get("delta").String())
				case "response.custom_tool_call_input.done":
					doneInput = data.Get("input").String()
				case "response.output_item.done":
					if data.Get("item.type").String() == "custom_tool_call" {
						itemInput = data.Get("item.input").String()
					}
				case "response.completed":
					for _, item := range data.Get("response.output").Array() {
						if item.Get("type").String() == "custom_tool_call" {
							completedInput = item.Get("input").String()
						}
					}
				}
			}
			if got := delta.String(); got != tt.wantInput {
				t.Fatalf("custom input deltas = %q, want %q", got, tt.wantInput)
			}
			if doneInput != tt.wantInput || itemInput != tt.wantInput || completedInput != tt.wantInput {
				t.Fatalf("custom lifecycle inputs = done %q item %q completed %q, want %q", doneInput, itemInput, completedInput, tt.wantInput)
			}
		})
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_IncompleteToolStreamDoesNotFinalizeAsCompleted(t *testing.T) {
	request := []byte(`{"model":"gpt-5.6-terra"}`)

	tests := []struct {
		name   string
		chunks []string
	}{
		{
			name: "zero argument bytes without finish reason",
			chunks: []string{
				`data: {"id":"resp_interrupted_tool","object":"chat.completion.chunk","created":1773896263,"model":"gpt-5.6-terra","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":""}}]},"finish_reason":null}]}`,
				`data: [DONE]`,
			},
		},
		{
			name: "partial json arguments without finish reason",
			chunks: []string{
				`data: {"id":"resp_interrupted_partial","object":"chat.completion.chunk","created":1773896263,"model":"gpt-5.6-terra","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"{\"filePath\":\"foo"}}]},"finish_reason":null}]}`,
				`data: [DONE]`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var param any
			for _, line := range tt.chunks {
				for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-5.6-terra", request, request, []byte(line), &param) {
					event, data := parseOpenAIResponsesSSEEvent(t, chunk)
					if event == "response.completed" {
						t.Fatalf("incomplete tool stream was finalized as response.completed: %s", chunk)
					}
					if event == "response.output_item.done" {
						t.Fatalf("incomplete tool stream emitted output_item.done: %s", chunk)
					}
					if event == "response.function_call_arguments.done" {
						t.Fatalf("incomplete tool stream emitted function_call_arguments.done: %s", chunk)
					}
					_ = data
				}
			}
		})
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_FinishReasonLengthEmitsIncomplete(t *testing.T) {
	request := []byte(`{"model":"gpt-5.6-luna"}`)
	chunks := []string{
		`data: {"id":"resp_length_tool","object":"chat.completion.chunk","created":1773896263,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_length_tool","object":"chat.completion.chunk","created":1773896263,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	}

	var param any
	var incompleteSeen bool
	var itemDoneSeen bool
	for _, line := range chunks {
		for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-5.6-luna", request, request, []byte(line), &param) {
			event, data := parseOpenAIResponsesSSEEvent(t, chunk)
			if event == "response.completed" {
				t.Fatalf("stream with finish_reason=length was finalized as response.completed: %s", chunk)
			}
			if event == "response.output_item.done" {
				itemDoneSeen = true
				if got := data.Get("item.status").String(); got != "incomplete" {
					t.Fatalf("item.status = %q, want incomplete", got)
				}
				if got := data.Get("item.arguments").String(); got == "{}" {
					t.Fatalf("item.arguments synthesized empty object {}, want raw args or empty string")
				}
			}
			if event == "response.incomplete" {
				incompleteSeen = true
				if got := data.Get("response.status").String(); got != "incomplete" {
					t.Fatalf("response.status = %q, want incomplete", got)
				}
				if got := data.Get("response.incomplete_details.reason").String(); got != "max_output_tokens" {
					t.Fatalf("response.incomplete_details.reason = %q, want max_output_tokens", got)
				}
				if got := data.Get("response.output.0.status").String(); got != "incomplete" {
					t.Fatalf("response.output.0.status = %q, want incomplete", got)
				}
			}
		}
	}
	if !itemDoneSeen {
		t.Fatal("expected response.output_item.done event for finish_reason=length")
	}
	if !incompleteSeen {
		t.Fatal("expected response.incomplete event for finish_reason=length")
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_FinishReasonContentFilterEmitsIncomplete(t *testing.T) {
	request := []byte(`{"model":"gpt-5.6-luna"}`)
	chunks := []string{
		`data: {"id":"resp_filter_tool","object":"chat.completion.chunk","created":1773896263,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_filter_tool","object":"chat.completion.chunk","created":1773896263,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	}

	var param any
	var incompleteSeen bool
	var itemDoneSeen bool
	for _, line := range chunks {
		for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-5.6-luna", request, request, []byte(line), &param) {
			event, data := parseOpenAIResponsesSSEEvent(t, chunk)
			if event == "response.completed" {
				t.Fatalf("stream with finish_reason=content_filter was finalized as response.completed: %s", chunk)
			}
			if event == "response.output_item.done" {
				itemDoneSeen = true
				if got := data.Get("item.status").String(); got != "incomplete" {
					t.Fatalf("item.status = %q, want incomplete", got)
				}
			}
			if event == "response.incomplete" {
				incompleteSeen = true
				if got := data.Get("response.status").String(); got != "incomplete" {
					t.Fatalf("response.status = %q, want incomplete", got)
				}
				if got := data.Get("response.incomplete_details.reason").String(); got != "content_filter" {
					t.Fatalf("response.incomplete_details.reason = %q, want content_filter", got)
				}
			}
		}
	}
	if !itemDoneSeen {
		t.Fatal("expected response.output_item.done event for finish_reason=content_filter")
	}
	if !incompleteSeen {
		t.Fatal("expected response.incomplete event for finish_reason=content_filter")
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream_FinishReasonLength(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl_len","object":"chat.completion","created":1773896263,"model":"gpt-5.6","choices":[{"index":0,"message":{"role":"assistant","content":"truncated text"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.6", nil, nil, raw, nil)
	data := gjson.ParseBytes(out)
	if got := data.Get("status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete; out=%s", got, out)
	}
	if got := data.Get("incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete_details.reason = %q, want max_output_tokens; out=%s", got, out)
	}
	if got := data.Get("output.0.status").String(); got != "incomplete" {
		t.Fatalf("output.0.status = %q, want incomplete; out=%s", got, out)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream_FinishReasonContentFilter(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl_filter","object":"chat.completion","created":1773896263,"model":"gpt-5.6","choices":[{"index":0,"message":{"role":"assistant","content":"blocked text"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.6", nil, nil, raw, nil)
	data := gjson.ParseBytes(out)
	if got := data.Get("status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete; out=%s", got, out)
	}
	if got := data.Get("incomplete_details.reason").String(); got != "content_filter" {
		t.Fatalf("incomplete_details.reason = %q, want content_filter; out=%s", got, out)
	}
	if got := data.Get("output.0.status").String(); got != "incomplete" {
		t.Fatalf("output.0.status = %q, want incomplete; out=%s", got, out)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream_ReasoningFallback(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "missing reasoning_content",
			raw:  `{"id":"chatcmpl_reasoning_fallback","object":"chat.completion","created":1773896263,"model":"grok-3","choices":[{"index":0,"message":{"role":"assistant","content":"42","reasoning":"step by step thought"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		},
		{
			name: "empty reasoning_content",
			raw:  `{"id":"chatcmpl_reasoning_fallback","object":"chat.completion","created":1773896263,"model":"grok-3","choices":[{"index":0,"message":{"role":"assistant","content":"42","reasoning_content":"","reasoning":"step by step thought"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "grok-3", nil, nil, []byte(tt.raw), nil)
			data := gjson.ParseBytes(out)

			reasoningType := data.Get("output.0.type").String()
			if reasoningType != "reasoning" {
				t.Fatalf("output.0.type = %q, want reasoning; out=%s", reasoningType, out)
			}
			reasoningText := data.Get("output.0.summary.0.text").String()
			if reasoningText != "step by step thought" {
				t.Fatalf("output.0.summary.0.text = %q, want %q; out=%s", reasoningText, "step by step thought", out)
			}

			msgType := data.Get("output.1.type").String()
			if msgType != "message" {
				t.Fatalf("output.1.type = %q, want message; out=%s", msgType, out)
			}
			msgText := data.Get("output.1.content.0.text").String()
			if msgText != "42" {
				t.Fatalf("output.1.content.0.text = %q, want %q; out=%s", msgText, "42", out)
			}
		})
	}
}
func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_RestoresCappedNamespaceFunctionCall(t *testing.T) {
	originalRequest := []byte(`{
		"model":"deepseek-v4-flash",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__codex_apps__codex_document_control",
				"tools":[{"type":"function","name":"_execute_document_command","parameters":{"type":"object"}}]
			}
		]
	}`)
	// The 66-char flattened name is capped to 64 chars.
	chatName := capResponsesChatToolName("mcp__codex_apps__codex_document_control___execute_document_command")
	chunks := []string{
		`data: {"id":"chatcmpl_capped_stream","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_capped","type":"function","function":{"name":"` + chatName + `","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_capped_stream","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"run\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}

	var param any
	var added gjson.Result
	var done gjson.Result
	var completed gjson.Result
	for _, line := range chunks {
		for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", originalRequest, nil, []byte(line), &param) {
			event, data := parseOpenAIResponsesSSEEvent(t, chunk)
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
		if got := tc.got.Get("item.name").String(); got != "_execute_document_command" {
			t.Fatalf("%s item.name = %q, want _execute_document_command", tc.label, got)
		}
		if got := tc.got.Get("item.namespace").String(); got != "mcp__codex_apps__codex_document_control" {
			t.Fatalf("%s item.namespace = %q, want mcp__codex_apps__codex_document_control", tc.label, got)
		}
	}
	if !completed.Exists() {
		t.Fatal("expected response.completed event")
	}
	if got := completed.Get("response.output.0.name").String(); got != "_execute_document_command" {
		t.Fatalf("completed output name = %q, want _execute_document_command", got)
	}
	if got := completed.Get("response.output.0.namespace").String(); got != "mcp__codex_apps__codex_document_control" {
		t.Fatalf("completed output namespace = %q, want mcp__codex_apps__codex_document_control", got)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream_RestoresCappedNamespaceFunctionCall(t *testing.T) {
	originalRequest := []byte(`{
		"model":"deepseek-v4-flash",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__codex_apps__codex_document_control",
				"tools":[{"type":"function","name":"_execute_document_command","parameters":{"type":"object"}}]
			}
		]
	}`)
	chatName := capResponsesChatToolName("mcp__codex_apps__codex_document_control___execute_document_command")
	raw := []byte(`{"id":"chatcmpl_capped_nonstream","object":"chat.completion","created":1773896263,"model":"model","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_capped","type":"function","function":{"name":"` + chatName + `","arguments":"{\"cmd\":\"run\"}"}}]},"finish_reason":"tool_calls"}]}`)

	resp := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "model", originalRequest, nil, raw, nil)
	data := gjson.ParseBytes(resp)

	if got := data.Get("output.0.name").String(); got != "_execute_document_command" {
		t.Fatalf("non-stream output name = %q, want _execute_document_command; response=%s", got, resp)
	}
	if got := data.Get("output.0.namespace").String(); got != "mcp__codex_apps__codex_document_control" {
		t.Fatalf("non-stream output namespace = %q, want mcp__codex_apps__codex_document_control; response=%s", got, resp)
	}
}

// --- R6 byte-compat pin ------------------------------------------------------
// Hashes captured from the pre-port implementation and held identical across the
// per-stream request-echo/tool-index cache port in this package (plan v8040-p3,
// upstream 3b15314).

var byteCompatHashes = map[string]string{
	"stream":    "a4a0356a37c91b93856e52730accec5249af04d0d6202b89043beec59151f01b",
	"nonstream": "5c077f8e68861ba569f8db879ff936da6fdc6ed11513c37b44160053b43cbf3f",
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

func byteCompatOpenAIResponsesRequest() []byte {
	return []byte(`{"model":"gpt-5.4","instructions":"pinned","max_output_tokens":50,"max_tool_calls":2,"parallel_tool_calls":false,"temperature":0.2,"top_p":0.9,"store":false,"metadata":{"k":"v"},"user":"u1","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]},{"type":"custom","name":"apply_patch"}]}`)
}

func byteCompatOpenAIResponsesFrames() []string {
	return []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"p\":1}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
}

func TestByteCompatOpenAIResponses(t *testing.T) {
	atomic.StoreUint64(&responseIDCounter, 0)
	request := byteCompatOpenAIResponsesRequest()

	var param any
	var stream [][]byte
	for _, frame := range byteCompatOpenAIResponsesFrames() {
		stream = append(stream, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "m", request, nil, []byte(frame), &param)...)
	}
	checkByteCompat(t, "stream", stream)

	nonStream := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(
		context.Background(),
		"m",
		request,
		nil,
		[]byte(`{"id":"c2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done","tool_calls":[{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{\"p\":2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
		nil,
	)
	checkByteCompat(t, "nonstream", [][]byte{nonStream})
}

func BenchmarkOpenAIResponsesStreamTools(b *testing.B) {
	request := byteCompatOpenAIResponsesRequest()
	frames := byteCompatOpenAIResponsesFrames()
	b.ReportAllocs()
	for b.Loop() {
		var param any
		for _, frame := range frames {
			_ = ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "m", request, nil, []byte(frame), &param)
		}
	}
}
