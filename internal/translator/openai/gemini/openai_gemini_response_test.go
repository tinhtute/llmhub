package gemini

import (
	"context"
	"testing"

	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

// Ported from upstream CLIProxyAPI commit 4dce5f3a2b9a ("ignore null and empty
// finish reasons in openai to gemini response"): null/empty finish_reason must
// not emit a Gemini finishReason.
func TestConvertOpenAIResponseToGeminiStream_NullFinishReasonIgnored(t *testing.T) {
	var param any

	// Chunk 1: Contentless delta with explicit finish_reason: null
	chunk1 := []byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
	out1 := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, chunk1, &param)
	for i, chunk := range out1 {
		if fr := gjson.GetBytes(chunk, "candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
			t.Fatalf("chunk1[%d] unexpectedly set finishReason = %q on non-final chunk; payload=%s", i, fr.String(), chunk)
		}
	}

	// Chunk 2: Contentless delta with finish_reason: "" (empty string should not be treated as stop)
	chunk2 := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":""}]}`)
	out2 := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, chunk2, &param)
	for i, chunk := range out2 {
		if fr := gjson.GetBytes(chunk, "candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
			t.Fatalf("chunk2[%d] unexpectedly set finishReason = %q on contentless chunk with empty finish_reason; payload=%s", i, fr.String(), chunk)
		}
	}

	// Chunk 3: Final chunk with finish_reason: "stop"
	chunk3 := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	out3 := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, chunk3, &param)
	if len(out3) == 0 {
		t.Fatalf("expected output for final chunk, got 0 chunks")
	}
	if got := gjson.GetBytes(out3[len(out3)-1], "candidates.0.finishReason").String(); got != "STOP" {
		t.Fatalf("expected finishReason STOP on final chunk, got %q", got)
	}
}

// Ported from upstream CLIProxyAPI commit 4dce5f3a2b9a: the non-streaming
// converter must also ignore null/empty finish_reason.
func TestConvertOpenAIResponseToGeminiNonStream_NullFinishReasonIgnored(t *testing.T) {
	testCases := []struct {
		name    string
		payload []byte
	}{
		{
			name:    "null finish_reason",
			payload: []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":null}]}`),
		},
		{
			name:    "empty finish_reason",
			payload: []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":""}]}`),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			out := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "gpt-test", nil, nil, tc.payload, nil)
			if fr := gjson.GetBytes(out, "candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
				t.Fatalf("unexpectedly set finishReason = %q for %s; payload=%s", fr.String(), tc.name, out)
			}
		})
	}

	out := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "gpt-test", nil, nil,
		[]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`), nil)
	if got := gjson.GetBytes(out, "candidates.0.finishReason").String(); got != "STOP" {
		t.Fatalf("expected finishReason STOP, got %q; payload=%s", got, out)
	}
}

// TestOpenAIChatGeminiThoughtSignatureRoundTrip is the R3 production-path test:
// the whole Gemini client → openai-chat upstream → Gemini client round trip is
// driven through the sdktranslator registry, i.e. the entry point a live request
// takes, and the thoughtSignature must survive it (upstream CLIProxyAPI
// 387063909692).
func TestOpenAIChatGeminiThoughtSignatureRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Turn 1: the openai-chat upstream answers with a tool call whose signature
	// rides extra_content, and the Gemini client must receive it as a part field.
	upstreamTurn1 := []byte(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"},"extra_content":{"google":{"thought_signature":"SIG_ROUNDTRIP"}}},
		{"id":"c2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}}
	]}}]}`)
	turn1 := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, sdktranslator.FormatGemini, "google/gemini-3-pro", nil, nil, upstreamTurn1, nil)

	if got := gjson.GetBytes(turn1, "candidates.0.content.parts.0.thoughtSignature").String(); got != "SIG_ROUNDTRIP" {
		t.Fatalf("candidates.0.content.parts.0.thoughtSignature = %q, want SIG_ROUNDTRIP; out=%s", got, turn1)
	}
	if gjson.GetBytes(turn1, "candidates.0.content.parts.1.thoughtSignature").Exists() {
		t.Fatalf("expected the unsigned sibling tool call not to have thoughtSignature; out=%s", turn1)
	}

	// Turn 2: the Gemini client replays that part, and the next openai-chat
	// request must carry the signature back to the upstream.
	replay := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"weather in Paris?"}]},
		{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"` + gjson.GetBytes(turn1, "candidates.0.content.parts.0.thoughtSignature").String() + `"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"t":20}}}]}
	]}`)
	turn2 := sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatOpenAI, "google/gemini-3-pro", replay, false)

	if got := gjson.GetBytes(turn2, "messages.1.tool_calls.0.extra_content.google.thought_signature").String(); got != "SIG_ROUNDTRIP" {
		t.Fatalf("round-trip thought signature = %q, want SIG_ROUNDTRIP; out=%s", got, turn2)
	}
}

func TestConvertOpenAIResponseToGeminiStream_PreservesToolCallThoughtSignature(t *testing.T) {
	ctx := context.Background()
	var param any

	// Non-function deltas may arrive in any order, and the signature must land on
	// its own tool call once the message closes.
	out := ConvertOpenAIResponseToGemini(ctx, "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[
		{"index":1,"id":"c2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}},
		{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"},"extra_content":{"google":{"thought_signature":"SIG"}}}
	]}}]}`), &param)
	ConvertOpenAIResponseToGemini(ctx, "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[
		{"index":1,"function":{"arguments":"\"London\"}"}},
		{"index":0,"function":{"arguments":"\"Paris\"}"}}
	]}}]}`), &param)
	out = append(out, ConvertOpenAIResponseToGemini(ctx, "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`), &param)...)

	if len(out) == 0 {
		t.Fatal("stream output is empty")
	}
	last := out[len(out)-1]
	// Tool index 0 (Paris) must be emitted first even though the upstream sent
	// index 1 in the same delta first.
	if got := gjson.GetBytes(last, "candidates.0.content.parts.0.functionCall.args.city").String(); got != "Paris" {
		t.Fatalf("candidates.0.content.parts.0 functionCall.args.city = %q, want Paris (ascending tool index); payload=%s", got, last)
	}
	if got := gjson.GetBytes(last, "candidates.0.content.parts.0.thoughtSignature").String(); got != "SIG" {
		t.Fatalf("candidates.0.content.parts.0.thoughtSignature = %q, want SIG; payload=%s", got, last)
	}
	if got := gjson.GetBytes(last, "candidates.0.content.parts.1.functionCall.args.city").String(); got != "London" {
		t.Fatalf("candidates.0.content.parts.1 functionCall.args.city = %q, want London; payload=%s", got, last)
	}
	if gjson.GetBytes(last, "candidates.0.content.parts.1.thoughtSignature").Exists() {
		t.Fatalf("expected the unsigned sibling tool call not to have thoughtSignature; payload=%s", last)
	}
}

func TestConvertOpenAIResponseToGeminiStream_SingleChunkToolCallAndLateSignature(t *testing.T) {
	ctx := context.Background()

	// A single SSE delta may carry the tool call and the finish reason at once.
	var singleParam any
	singleOut := ConvertOpenAIResponseToGemini(ctx, "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c_single","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"},"extra_content":{"google":{"thought_signature":"SIG_SINGLE"}}}]},"finish_reason":"tool_calls"}]}`), &singleParam)
	if len(singleOut) == 0 {
		t.Fatal("expected output for a single-chunk tool call carrying finish_reason")
	}
	if got := gjson.GetBytes(singleOut[len(singleOut)-1], "candidates.0.content.parts.0.thoughtSignature").String(); got != "SIG_SINGLE" {
		t.Fatalf("single-chunk thoughtSignature = %q, want SIG_SINGLE; payload=%s", got, singleOut[len(singleOut)-1])
	}

	// The signature may also arrive in a later delta that carries no function data.
	var lateParam any
	ConvertOpenAIResponseToGemini(ctx, "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c_late","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Berlin\"}"}}]}}]}`), &lateParam)
	ConvertOpenAIResponseToGemini(ctx, "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"extra_content":{"google":{"thought_signature":"SIG_LATE"}}}]}}]}`), &lateParam)
	lateOut := ConvertOpenAIResponseToGemini(ctx, "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`), &lateParam)
	if len(lateOut) == 0 {
		t.Fatal("expected output when flushing a late-signature tool call")
	}
	if got := gjson.GetBytes(lateOut[len(lateOut)-1], "candidates.0.content.parts.0.thoughtSignature").String(); got != "SIG_LATE" {
		t.Fatalf("late-signature thoughtSignature = %q, want SIG_LATE; payload=%s", got, lateOut[len(lateOut)-1])
	}
}
