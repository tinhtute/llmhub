package gemini

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToGemini_StreamEmptyOutputUsesOutputItemDoneMessageFallback(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)...)
	}

	found := false
	for _, out := range outputs {
		if gjson.GetBytes(out, "candidates.0.content.parts.0.text").String() == "ok" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected fallback content from response.output_item.done message; outputs=%q", outputs)
	}
}

func TestConvertCodexResponseToGemini_StreamPartialImageEmitsInlineData(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunk := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)
	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.inlineData.data").String()
	if got != "aGVsbG8=" {
		t.Fatalf("expected inlineData.data %q, got %q; chunk=%s", "aGVsbG8=", got, string(out[0]))
	}

	gotMime := gjson.GetBytes(out[0], "candidates.0.content.parts.0.inlineData.mimeType").String()
	if gotMime != "image/png" {
		t.Fatalf("expected inlineData.mimeType %q, got %q; chunk=%s", "image/png", gotMime, string(out[0]))
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)
	if len(out) != 0 {
		t.Fatalf("expected duplicate image chunk to be suppressed, got %d", len(out))
	}
}

func TestConvertCodexResponseToGemini_StreamImageGenerationCallDoneEmitsInlineData(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"png","result":"aGVsbG8="}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected output_item.done to be suppressed when identical to last partial image, got %d", len(out))
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"jpeg","result":"Ymll"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.inlineData.data").String()
	if got != "Ymll" {
		t.Fatalf("expected inlineData.data %q, got %q; chunk=%s", "Ymll", got, string(out[0]))
	}

	gotMime := gjson.GetBytes(out[0], "candidates.0.content.parts.0.inlineData.mimeType").String()
	if gotMime != "image/jpeg" {
		t.Fatalf("expected inlineData.mimeType %q, got %q; chunk=%s", "image/jpeg", gotMime, string(out[0]))
	}
}

func TestConvertCodexResponseToGemini_NonStreamImageGenerationCallAddsInlineDataPart(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","output_format":"png","result":"aGVsbG8="}]}}`)
	out := ConvertCodexResponseToGeminiNonStream(ctx, "gemini-2.5-pro", originalRequest, nil, raw, nil)

	got := gjson.GetBytes(out, "candidates.0.content.parts.1.inlineData.data").String()
	if got != "aGVsbG8=" {
		t.Fatalf("expected inlineData.data %q, got %q; chunk=%s", "aGVsbG8=", got, string(out))
	}

	gotMime := gjson.GetBytes(out, "candidates.0.content.parts.1.inlineData.mimeType").String()
	if gotMime != "image/png" {
		t.Fatalf("expected inlineData.mimeType %q, got %q; chunk=%s", "image/png", gotMime, string(out))
	}
}

func TestConvertCodexResponseToGemini_ReasoningThoughtSignature(t *testing.T) {
	ctx := context.Background()
	signature := testCodexGeminiMetaSignature()
	carrier := encodeGeminiCodexThoughtSignature(signature)

	t.Run("non-stream summary carries signature", func(t *testing.T) {
		raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"muse-spark-1.3","created_at":1,"usage":{"input_tokens":1,"output_tokens":2},"output":[{"type":"reasoning","encrypted_content":"` + signature + `","summary":[{"type":"summary_text","text":"thinking summary"}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}}`)
		out := ConvertCodexResponseToGeminiNonStream(ctx, "muse-spark-1.3", nil, nil, raw, nil)
		part := gjson.GetBytes(out, "candidates.0.content.parts.0")
		if !part.Get("thought").Bool() || part.Get("text").String() != "thinking summary" {
			t.Fatalf("part.0 = %s, want thought summary part; payload=%s", part.Raw, out)
		}
		if got := part.Get("thoughtSignature").String(); got != carrier {
			t.Fatalf("thoughtSignature is not the carrier-encoded Muse envelope; payload=%s", out)
		}
		// The Gemini wire form must survive an official-SDK-style []byte JSON
		// decode (standard base64), and unwrap back to the raw signature.
		var sdkPart struct {
			ThoughtSignature []byte `json:"thoughtSignature"`
		}
		if err := json.Unmarshal([]byte(part.Raw), &sdkPart); err != nil {
			t.Fatalf("SDK []byte decode of thoughtSignature failed: %v", err)
		}
		if got := decodeGeminiCodexThoughtSignature(part.Get("thoughtSignature").String()); got != signature {
			t.Fatalf("carrier round-trip = %q, want the raw Muse envelope", got)
		}
		if got := gjson.GetBytes(out, "candidates.0.content.parts.1.text").String(); got != "answer" {
			t.Fatalf("part.1 text = %q, want answer; payload=%s", got, out)
		}
	})

	t.Run("non-stream empty summary still carries signature", func(t *testing.T) {
		raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"muse-spark-1.3","created_at":1,"usage":{"input_tokens":1,"output_tokens":2},"output":[{"type":"reasoning","encrypted_content":"` + signature + `","summary":[]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}}`)
		out := ConvertCodexResponseToGeminiNonStream(ctx, "muse-spark-1.3", nil, nil, raw, nil)
		part := gjson.GetBytes(out, "candidates.0.content.parts.0")
		if !part.Get("thought").Bool() || part.Get("thoughtSignature").String() != carrier {
			t.Fatalf("part.0 = %s, want an empty thought carrier with the signature; payload=%s", part.Raw, out)
		}
	})

	t.Run("stream signature attaches to the next visible part", func(t *testing.T) {
		var param any
		done := []byte(`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"` + signature + `","summary":[]}}`)
		if out := ConvertCodexResponseToGemini(ctx, "muse-spark-1.3", nil, nil, done, &param); len(out) != 0 {
			t.Fatalf("reasoning done emitted %d chunks before a visible carrier, want 0", len(out))
		}
		out := ConvertCodexResponseToGemini(ctx, "muse-spark-1.3", nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"answer"}`), &param)
		if len(out) != 1 {
			t.Fatalf("text delta emitted %d chunks, want 1", len(out))
		}
		part := gjson.GetBytes(out[0], "candidates.0.content.parts.0")
		if part.Get("text").String() != "answer" || part.Get("thoughtSignature").String() != carrier {
			t.Fatalf("text part = %s, want answer with the carrier signature", part.Raw)
		}
	})
}

func TestConvertCodexResponseToGemini_StreamMultipleFunctionCallsKeepSignatureAndOrder(t *testing.T) {
	ctx := context.Background()
	signature := testCodexGeminiMetaSignature()
	var param any
	var outputs [][]byte
	events := [][]byte{
		[]byte(`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"` + signature + `","summary":[]}}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_a","name":"alpha","arguments":"{}"}}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_b","name":"beta","arguments":"{}"}}`),
		[]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`),
	}
	for _, event := range events {
		outputs = append(outputs, ConvertCodexResponseToGemini(ctx, "muse-spark-1.3", nil, nil, event, &param)...)
	}

	var names []string
	signatureOnAlpha := false
	for _, chunk := range outputs {
		for _, part := range gjson.GetBytes(chunk, "candidates.0.content.parts").Array() {
			name := part.Get("functionCall.name").String()
			if name == "" {
				continue
			}
			names = append(names, name)
			if name == "alpha" && decodeGeminiCodexThoughtSignature(part.Get("thoughtSignature").String()) == signature {
				signatureOnAlpha = true
			}
		}
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("function calls = %v, want [alpha beta] in stream order; outputs=%q", names, outputs)
	}
	if !signatureOnAlpha {
		t.Fatalf("first function call lost the pending Muse thoughtSignature; outputs=%q", outputs)
	}
}

// --- R5 finish reason carried (upstream v8.0.43 bdde391d) -------------------
// A normal Codex completion must close the Gemini stream with finishReason STOP;
// without it the client sees a stream that never terminates.

func TestConvertCodexResponseToGemini_CompletedFinishReasonSetsStop(t *testing.T) {
	var param any
	terminal := []byte(`{"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`)
	out := ConvertCodexResponseToGemini(context.Background(), "gpt-test", nil, nil, append([]byte("data: "), terminal...), &param)
	if len(out) == 0 {
		t.Fatal("response.completed emitted no chunk")
	}
	var finishReasons []string
	for _, chunk := range out {
		if reason := gjson.GetBytes(chunk, "candidates.0.finishReason").String(); reason != "" {
			finishReasons = append(finishReasons, reason)
		}
	}
	if len(finishReasons) != 1 || finishReasons[0] != "STOP" {
		t.Fatalf("finishReason = %v, want exactly [STOP]; outputs=%q", finishReasons, out)
	}
}
