package chat_completions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAI_StreamSetsModelFromResponseCreated(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.3-codex"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected no output for response.created, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_FirstChunkUsesRequestModelName(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallChunkOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls").Exists() {
		t.Fatalf("expected tool_calls to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallArgumentsDeltaOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected tool call announcement chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"query\":\"OpenAI\"}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").Exists() {
		t.Fatalf("expected tool call arguments delta to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_CustomToolCallStreamDeltas(t *testing.T) {
	ctx := context.Background()
	var param any
	send := func(event string) [][]byte {
		return ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte("data: "+event), &param)
	}

	out := send(`{"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"unexpected input"}}`)
	if len(out) != 1 {
		t.Fatalf("expected 1 announcement chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "ApplyPatch" {
		t.Fatalf("expected custom tool name, got %q; chunk=%s", got, out[0])
	}

	out = send(`{"type":"response.custom_tool_call_input.delta","delta":"patch"}`)
	if len(out) != 1 {
		t.Fatalf("expected 1 input delta chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "patch" {
		t.Fatalf("expected patch arguments, got %q; chunk=%s", got, out[0])
	}

	out = send(`{"type":"response.custom_tool_call_input.done","input":"patch"}`)
	if len(out) != 0 {
		t.Fatalf("expected custom input done to be suppressed after delta, got %d chunks", len(out))
	}
	out = send(`{"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"patch"}}`)
	if len(out) != 0 {
		t.Fatalf("expected output item done to be suppressed after delta, got %d chunks", len(out))
	}
}

func TestConvertCodexResponseToOpenAI_InterleavedToolCallsKeepStateByItem(t *testing.T) {
	ctx := context.Background()
	var param any
	send := func(event string) [][]byte {
		return ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte("data: "+event), &param)
	}

	out := send(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":""}}`)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("expected function call index 0, got %d; chunk=%s", got, out[0])
	}
	out = send(`{"type":"response.output_item.added","output_index":1,"item":{"id":"ctc_2","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":""}}`)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
		t.Fatalf("expected custom call index 1, got %d; chunk=%s", got, out[0])
	}

	out = send(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\\"query\\":"}`)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("expected interleaved function delta index 0, got %d; chunk=%s", got, out[0])
	}
	out = send(`{"type":"response.custom_tool_call_input.done","output_index":1,"input":"patch"}`)
	if len(out) != 1 {
		t.Fatalf("expected custom done fallback, got %d chunks", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
		t.Fatalf("expected custom done index 1, got %d; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_MapsCacheWriteUsage(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":4}}}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 stream chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "usage.prompt_tokens_details.cached_creation_tokens").Int(); got != 4 {
		t.Fatalf("stream cached_creation_tokens = %d, want 4; chunk=%s", got, out[0])
	}

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.5","status":"completed","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":4}},"output":[]}}`)
	body := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.5", nil, nil, raw, nil)
	if got := gjson.GetBytes(body, "usage.prompt_tokens_details.cached_creation_tokens").Int(); got != 4 {
		t.Fatalf("nonstream cached_creation_tokens = %d, want 4; body=%s", got, body)
	}
}

func TestConvertCodexResponseToOpenAI_StreamPartialImageEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	chunk := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out[0]))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 0 {
		t.Fatalf("expected duplicate image chunk to be suppressed, got %d", len(out))
	}
}

func TestConvertCodexResponseToOpenAI_StreamImageGenerationCallDoneEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"png","result":"aGVsbG8="}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected output_item.done to be suppressed when identical to last partial image, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"jpeg","result":"Ymll"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/jpeg;base64,Ymll" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/jpeg;base64,Ymll", gotURL, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamImageGenerationCallAddsMessageImages(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","output_format":"png","result":"aGVsbG8="}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	gotURL := gjson.GetBytes(out, "choices.0.message.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out))
	}
}

// --- R6 byte-compat pin ------------------------------------------------------
// Hashes captured from the pre-port implementation and held identical across the
// per-stream tool-name cache port in this package (plan v8040-p3, upstream
// 74e7ff6).

var byteCompatHashes = map[string]string{
	"stream":    "c07fef636a5f32082f722829d190fb71e3ecd948f53678895949113a35bf8b34",
	"nonstream": "945c64f1773f91a98b7e4794fbaa8c4bb7c1ceebb22e4b2a8503c2164386e38b",
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

func byteCompatCodexChatFrames() []string {
	return []string{
		`data: {"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.4"}}`,
		`data: {"type":"response.output_item.added","item_id":"function_1","item":{"id":"function_1","type":"function_call","call_id":"call_1","name":"websearch"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"function_1","delta":"{\"q\":\"x\"}"}`,
		`data: {"type":"response.output_item.done","item_id":"function_1","item":{"id":"function_1","type":"function_call","call_id":"call_1","name":"websearch","arguments":"{\"q\":\"x\"}"}}`,
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"output":[{"type":"function_call","id":"function_1","call_id":"call_1","name":"websearch","arguments":"{\"q\":\"x\"}"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`,
	}
}

func byteCompatCodexChatRequest() []byte {
	return []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"search"},{"role":"assistant","content":"","tool_calls":[{"id":"call_0","type":"function","function":{"name":"websearch","arguments":"{}"}}]}],"tools":[{"type":"function","function":{"name":"websearch","parameters":{"type":"object"}}},{"type":"function","function":{"name":"mcp__averylongtoolnamethatgoesonandonandonandonandonandonandonandonandonandon_extra","parameters":{"type":"object"}}}]}`)
}

func TestByteCompatCodexChat(t *testing.T) {
	request := byteCompatCodexChatRequest()

	var param any
	var stream [][]byte
	for _, frame := range byteCompatCodexChatFrames() {
		stream = append(stream, ConvertCodexResponseToOpenAI(context.Background(), "gpt-5.4", request, nil, []byte(frame), &param)...)
	}
	checkByteCompat(t, "stream", stream)

	nonStream := ConvertCodexResponseToOpenAINonStream(
		context.Background(),
		"gpt-5.4",
		request,
		nil,
		[]byte(`{"type":"response.completed","response":{"id":"resp_2","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"output":[{"type":"function_call","id":"function_2","call_id":"call_2","name":"websearch","arguments":"{\"q\":\"y\"}"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`),
		nil,
	)
	checkByteCompat(t, "nonstream", [][]byte{nonStream})
}

func BenchmarkCodexChatStreamToolNames(b *testing.B) {
	request := byteCompatCodexChatRequest()
	frames := byteCompatCodexChatFrames()
	b.ReportAllocs()
	for b.Loop() {
		var param any
		for _, frame := range frames {
			_ = ConvertCodexResponseToOpenAI(context.Background(), "gpt-5.4", request, nil, []byte(frame), &param)
		}
	}
}
