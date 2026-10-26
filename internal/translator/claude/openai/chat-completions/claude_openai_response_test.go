package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeResponseToOpenAI_StreamUsageIncludesCachedTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertClaudeResponseToOpenAI(
		ctx,
		"claude-opus-4-6",
		nil,
		nil,
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":13,"output_tokens":4,"cache_read_input_tokens":22000,"cache_creation_input_tokens":31}}`),
		&param,
	)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gotPromptTokens := gjson.GetBytes(out[0], "usage.prompt_tokens").Int(); gotPromptTokens != 22044 {
		t.Fatalf("expected prompt_tokens %d, got %d", 22044, gotPromptTokens)
	}
	if gotCompletionTokens := gjson.GetBytes(out[0], "usage.completion_tokens").Int(); gotCompletionTokens != 4 {
		t.Fatalf("expected completion_tokens %d, got %d", 4, gotCompletionTokens)
	}
	if gotTotalTokens := gjson.GetBytes(out[0], "usage.total_tokens").Int(); gotTotalTokens != 22048 {
		t.Fatalf("expected total_tokens %d, got %d", 22048, gotTotalTokens)
	}
	if gotCachedTokens := gjson.GetBytes(out[0], "usage.prompt_tokens_details.cached_tokens").Int(); gotCachedTokens != 22000 {
		t.Fatalf("expected cached_tokens %d, got %d", 22000, gotCachedTokens)
	}
}

func TestConvertClaudeResponseToOpenAI_StreamUsageMergesMessageStartUsage(t *testing.T) {
	ctx := context.Background()
	var param any

	ConvertClaudeResponseToOpenAI(
		ctx,
		"claude-opus-4-6",
		nil,
		nil,
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-opus-4-6","usage":{"input_tokens":13,"output_tokens":1,"cache_read_input_tokens":22000,"cache_creation_input_tokens":31}}}`),
		&param,
	)
	out := ConvertClaudeResponseToOpenAI(
		ctx,
		"claude-opus-4-6",
		nil,
		nil,
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`),
		&param,
	)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gotPromptTokens := gjson.GetBytes(out[0], "usage.prompt_tokens").Int(); gotPromptTokens != 22044 {
		t.Fatalf("expected prompt_tokens %d, got %d", 22044, gotPromptTokens)
	}
	if gotCompletionTokens := gjson.GetBytes(out[0], "usage.completion_tokens").Int(); gotCompletionTokens != 4 {
		t.Fatalf("expected completion_tokens %d, got %d", 4, gotCompletionTokens)
	}
	if gotTotalTokens := gjson.GetBytes(out[0], "usage.total_tokens").Int(); gotTotalTokens != 22048 {
		t.Fatalf("expected total_tokens %d, got %d", 22048, gotTotalTokens)
	}
	if gotCachedTokens := gjson.GetBytes(out[0], "usage.prompt_tokens_details.cached_tokens").Int(); gotCachedTokens != 22000 {
		t.Fatalf("expected cached_tokens %d, got %d", 22000, gotCachedTokens)
	}
}

func TestConvertClaudeResponseToOpenAINonStream_UsageIncludesCachedTokens(t *testing.T) {
	rawJSON := []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"model\":\"claude-opus-4-6\"}}\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":13,\"output_tokens\":4,\"cache_read_input_tokens\":22000,\"cache_creation_input_tokens\":31}}\n")

	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "", nil, nil, rawJSON, nil)

	if gotPromptTokens := gjson.GetBytes(out, "usage.prompt_tokens").Int(); gotPromptTokens != 22044 {
		t.Fatalf("expected prompt_tokens %d, got %d", 22044, gotPromptTokens)
	}
	if gotCompletionTokens := gjson.GetBytes(out, "usage.completion_tokens").Int(); gotCompletionTokens != 4 {
		t.Fatalf("expected completion_tokens %d, got %d", 4, gotCompletionTokens)
	}
	if gotTotalTokens := gjson.GetBytes(out, "usage.total_tokens").Int(); gotTotalTokens != 22048 {
		t.Fatalf("expected total_tokens %d, got %d", 22048, gotTotalTokens)
	}
	if gotCachedTokens := gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int(); gotCachedTokens != 22000 {
		t.Fatalf("expected cached_tokens %d, got %d", 22000, gotCachedTokens)
	}
}

func TestConvertClaudeResponseToOpenAINonStream_UsageMergesMessageStartUsage(t *testing.T) {
	rawJSON := []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":13,\"output_tokens\":1,\"cache_read_input_tokens\":22000,\"cache_creation_input_tokens\":31}}}\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n")

	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "", nil, nil, rawJSON, nil)

	if gotPromptTokens := gjson.GetBytes(out, "usage.prompt_tokens").Int(); gotPromptTokens != 22044 {
		t.Fatalf("expected prompt_tokens %d, got %d", 22044, gotPromptTokens)
	}
	if gotCompletionTokens := gjson.GetBytes(out, "usage.completion_tokens").Int(); gotCompletionTokens != 4 {
		t.Fatalf("expected completion_tokens %d, got %d", 4, gotCompletionTokens)
	}
	if gotTotalTokens := gjson.GetBytes(out, "usage.total_tokens").Int(); gotTotalTokens != 22048 {
		t.Fatalf("expected total_tokens %d, got %d", 22048, gotTotalTokens)
	}
	if gotCachedTokens := gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int(); gotCachedTokens != 22000 {
		t.Fatalf("expected cached_tokens %d, got %d", 22000, gotCachedTokens)
	}
}

func TestConvertClaudeResponseToOpenAINonStream_RefusalSensitiveMapToContentFilter(t *testing.T) {
	tests := []struct {
		name             string
		anthropicReason  string
		wantFinishReason string
	}{
		{name: "refusal maps to content_filter", anthropicReason: "refusal", wantFinishReason: "content_filter"},
		{name: "sensitive maps to content_filter", anthropicReason: "sensitive", wantFinishReason: "content_filter"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rawJSON := []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + tc.anthropicReason + "\"},\"usage\":{\"output_tokens\":1}}\n")

			out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "", nil, nil, rawJSON, nil)
			if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != tc.wantFinishReason {
				t.Fatalf("finish_reason = %q, want %q", got, tc.wantFinishReason)
			}
		})
	}
}

func TestConvertClaudeResponseToOpenAINonStream_ReasoningContent(t *testing.T) {
	rawJSON := []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"model\":\"claude-opus-4-6\"}}\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Let me think... \"}}\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Done thinking.\"}}\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello world\"}}\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":1}\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":20}}\n")

	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "", nil, nil, rawJSON, nil)

	gotReasoningContent := gjson.GetBytes(out, "choices.0.message.reasoning_content").String()
	wantReasoningContent := "Let me think... Done thinking."
	if gotReasoningContent != wantReasoningContent {
		t.Fatalf("expected reasoning_content %q, got %q, payload=%s", wantReasoningContent, gotReasoningContent, string(out))
	}
	if gotLegacyReasoning := gjson.GetBytes(out, "choices.0.message.reasoning"); gotLegacyReasoning.Exists() {
		t.Fatalf("expected reasoning to not exist, got %q, payload=%s", gotLegacyReasoning.String(), string(out))
	}
}

// TestConvertClaudeResponseToOpenAINonStreamFinishReasonTable pins the whole
// Claude→Chat Completions stop-reason table through the converter a handler
// calls. A reason with no row keeps the documented default of "stop":
// Chat Completions has no status for a server-tool pause, so pause_turn and any
// unrecognised reason both end the turn normally.
func TestConvertClaudeResponseToOpenAINonStreamFinishReasonTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopReason string
		want       string
	}{
		{name: "missing", want: "stop"},
		{name: "end_turn", stopReason: "end_turn", want: "stop"},
		{name: "stop_sequence", stopReason: "stop_sequence", want: "stop"},
		{name: "max_tokens", stopReason: "max_tokens", want: "length"},
		{name: "model_context_window_exceeded", stopReason: "model_context_window_exceeded", want: "length"},
		{name: "refusal", stopReason: "refusal", want: "content_filter"},
		{name: "sensitive", stopReason: "sensitive", want: "content_filter"},
		{name: "pause_turn", stopReason: "pause_turn", want: "stop"},
		{name: "unknown reason", stopReason: "some_future_reason", want: "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`data: {"type":"message_delta","delta":{"stop_reason":"` + tc.stopReason + `"}}`)
			out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "", nil, nil, raw, nil)
			if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != tc.want {
				t.Fatalf("finish_reason = %q, want %q; output=%s", got, tc.want, out)
			}
			if gjson.GetBytes(out, "error").Exists() {
				t.Fatalf("stop reason %q must not produce an error body: %s", tc.stopReason, out)
			}
		})
	}
}

// TestChatTruncatedToolCallKeepsLengthFinishReason covers a tool call cut off by
// max_tokens or by the context window: the caller must see "length" on both the
// stream and the non-stream path, because the call it received is incomplete.
func TestChatTruncatedToolCallKeepsLengthFinishReason(t *testing.T) {
	for _, reason := range []string{"max_tokens", "model_context_window_exceeded"} {
		t.Run(reason, func(t *testing.T) {
			chunks := [][]byte{
				[]byte(`data: {"type":"message_start","message":{"id":"msg_tc","model":"claude-test","usage":{"input_tokens":5,"output_tokens":0}}}`),
				[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`),
				[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls -l"}}`),
				[]byte(`data: {"type":"content_block_stop","index":0}`),
				[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"` + reason + `"},"usage":{"output_tokens":4}}`),
				[]byte(`data: {"type":"message_stop"}`),
			}

			var param any
			var streamFinish string
			var streamArgs string
			for _, chunk := range chunks {
				for _, output := range ConvertClaudeResponseToOpenAI(context.Background(), "claude-test", nil, nil, chunk, &param) {
					parsed := gjson.ParseBytes(output)
					if args := parsed.Get("choices.0.delta.tool_calls.0.function.arguments"); args.Exists() {
						streamArgs = args.String()
					}
					if fr := parsed.Get("choices.0.finish_reason").String(); fr != "" {
						streamFinish = fr
					}
				}
			}
			if streamFinish != "length" {
				t.Fatalf("stream finish_reason = %q, want length", streamFinish)
			}
			if streamArgs != `{"command":"ls -l` {
				t.Fatalf("stream tool arguments = %q", streamArgs)
			}

			raw := make([]byte, 0, 1024)
			for _, chunk := range chunks {
				raw = append(append(raw, chunk...), '\n')
			}
			out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "claude-test", nil, nil, raw, nil)
			if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "length" {
				t.Fatalf("non-stream finish_reason = %q, want length: %s", got, out)
			}
			if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"command":"ls -l` {
				t.Fatalf("non-stream tool arguments = %q: %s", got, out)
			}
			if got := gjson.GetBytes(out, "usage.completion_tokens").Int(); got != 4 {
				t.Fatalf("non-stream completion_tokens = %d, want 4: %s", got, out)
			}
		})
	}
}
