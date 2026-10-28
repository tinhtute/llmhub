package chat_completions

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
)

// --- R6 byte-compat pin ------------------------------------------------------
// Hashes captured from the pre-port implementation and held identical across the
// per-stream sanitized-name-map cache port in this package (plan v8040-p3,
// upstream 5de9de9). This package had no response test at all, so the pin lives
// in its own file.

var byteCompatHashes = map[string]string{
	"stream":    "4fea3955863ba6992e6401c20fd67bc46e72ed77a0399ed3f9abc5975b0c13b5",
	"nonstream": "35a55fa3c6f52d9f81769b5b925a706ea8b80019611912f8e210135875db60e0",
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

func TestByteCompatGeminiOpenAIChat(t *testing.T) {
	atomic.StoreUint64(&functionCallIDCounter, 0)
	request := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read"},{"name":"read"},{"name":"Write"}]}`)

	var param any
	var stream [][]byte
	for _, frame := range []string{
		`data: {"responseId":"r1","modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
		`data: {"responseId":"r1","modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{"p":1}}}]},"finishReason":"STOP"}]}`,
		`data: [DONE]`,
	} {
		stream = append(stream, ConvertGeminiResponseToOpenAI(context.Background(), "gemini-2.5-pro", request, request, []byte(frame), &param)...)
	}
	checkByteCompat(t, "stream", stream)

	nonStream := ConvertGeminiResponseToOpenAINonStream(
		context.Background(),
		"gemini-2.5-pro",
		request,
		request,
		[]byte(`{"responseId":"r2","modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"functionCall":{"name":"Write","args":{}}},{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`),
		nil,
	)
	checkByteCompat(t, "nonstream", [][]byte{nonStream})
}

// --- R5 non-content terminal states -----------------------------------------
// Upstream v8.0.41 3f5d3a16: a Gemini answer stopped by a safety/filter or
// malformed reason must reach an OpenAI-chat client as a non-normal terminal
// state carrying the upstream reason, and an unknown reason must survive on
// native_finish_reason instead of being lost.

func TestConvertGeminiResponseToOpenAI_ContentFilterFinishReasons(t *testing.T) {
	reasons := []string{
		"SAFETY",
		"RECITATION",
		"BLOCKLIST",
		"PROHIBITED_CONTENT",
		"SPII",
		"IMAGE_SAFETY",
		"MALFORMED_RESPONSE",
		"MALFORMED_FUNCTION_CALL",
	}
	for _, reason := range reasons {
		t.Run("stream/"+reason, func(t *testing.T) {
			var param any
			chunk := []byte(`{"candidates":[{"index":0,"content":{"role":"model"},"finishReason":"` + reason + `"}],"modelVersion":"gemini-4-argon","responseId":"resp-filter"}`)
			out := ConvertGeminiResponseToOpenAI(context.Background(), "gemini-4-argon", nil, nil, chunk, &param)
			if len(out) != 1 {
				t.Fatalf("chunks = %d, want 1; out=%q", len(out), out)
			}
			if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "content_filter" {
				t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, out[0])
			}
			if got, want := gjson.GetBytes(out[0], "choices.0.native_finish_reason").String(), strings.ToLower(reason); got != want {
				t.Fatalf("native_finish_reason = %q, want %q; payload=%s", got, want, out[0])
			}
		})

		t.Run("nonstream/"+reason, func(t *testing.T) {
			raw := []byte(`{"candidates":[{"index":0,"content":{"role":"model"},"finishReason":"` + reason + `"}],"modelVersion":"gemini-4-argon","responseId":"resp-filter"}`)
			out := ConvertGeminiResponseToOpenAINonStream(context.Background(), "gemini-4-argon", nil, nil, raw, nil)
			if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "content_filter" {
				t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, out)
			}
			if got, want := gjson.GetBytes(out, "choices.0.native_finish_reason").String(), strings.ToLower(reason); got != want {
				t.Fatalf("native_finish_reason = %q, want %q; payload=%s", got, want, out)
			}
		})
	}
}

func TestConvertGeminiResponseToOpenAI_UnknownFinishReasonKeepsNativeSpelling(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		var param any
		chunk := []byte(`{"candidates":[{"index":0,"content":{"role":"model"},"finishReason":"OTHER"}],"modelVersion":"gemini-4-argon"}`)
		out := ConvertGeminiResponseToOpenAI(context.Background(), "gemini-4-argon", nil, nil, chunk, &param)
		if len(out) != 1 {
			t.Fatalf("chunks = %d, want 1; out=%q", len(out), out)
		}
		// An unknown reason is never classified as a content filter, and it is not
		// dropped: the upstream spelling survives on native_finish_reason.
		if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "stop" {
			t.Fatalf("finish_reason = %q, want stop; payload=%s", got, out[0])
		}
		if got := gjson.GetBytes(out[0], "choices.0.native_finish_reason").String(); got != "other" {
			t.Fatalf("native_finish_reason = %q, want other; payload=%s", got, out[0])
		}
	})

	t.Run("nonstream", func(t *testing.T) {
		raw := []byte(`{"candidates":[{"index":0,"content":{"role":"model"},"finishReason":"OTHER"}],"modelVersion":"gemini-4-argon"}`)
		out := ConvertGeminiResponseToOpenAINonStream(context.Background(), "gemini-4-argon", nil, nil, raw, nil)
		if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "stop" {
			t.Fatalf("finish_reason = %q, want stop; payload=%s", got, out)
		}
		if got := gjson.GetBytes(out, "choices.0.native_finish_reason").String(); got != "other" {
			t.Fatalf("native_finish_reason = %q, want other; payload=%s", got, out)
		}
	})
}

func TestConvertGeminiResponseToOpenAI_SingleTerminalFinishChunk(t *testing.T) {
	var param any
	finishChunk := []byte(`{"candidates":[{"index":0,"content":{"role":"model"},"finishReason":"SAFETY"}],"modelVersion":"gemini-4-argon","responseId":"resp-split"}`)
	usageChunk := []byte(`{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5,"totalTokenCount":105},"modelVersion":"gemini-4-argon","responseId":"resp-split"}`)

	var all [][]byte
	all = append(all, ConvertGeminiResponseToOpenAI(context.Background(), "gemini-4-argon", nil, nil, finishChunk, &param)...)
	all = append(all, ConvertGeminiResponseToOpenAI(context.Background(), "gemini-4-argon", nil, nil, usageChunk, &param)...)
	all = append(all, ConvertGeminiResponseToOpenAI(context.Background(), "gemini-4-argon", nil, nil, []byte("[DONE]"), &param)...)

	var terminals [][]byte
	for _, chunk := range all {
		if gjson.GetBytes(chunk, "choices.0.finish_reason").Value() != nil {
			terminals = append(terminals, chunk)
		}
	}
	if len(terminals) != 1 {
		t.Fatalf("terminal chunks = %d, want exactly 1; chunks=%q", len(terminals), all)
	}
	if got := gjson.GetBytes(terminals[0], "choices.0.finish_reason").String(); got != "content_filter" {
		t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, terminals[0])
	}
	if got := gjson.GetBytes(terminals[0], "choices.0.native_finish_reason").String(); got != "safety" {
		t.Fatalf("native_finish_reason = %q, want safety; payload=%s", got, terminals[0])
	}
}

func TestConvertGeminiResponseToOpenAI_PreservesUpstreamToolCallID(t *testing.T) {
	request := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"time?"}],"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}}]}`)

	var param any
	chunk := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_upstream_6825","name":"get_time","args":{"tz":"UTC"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-4-argon"}`)
	out := ConvertGeminiResponseToOpenAI(context.Background(), "gemini-4-argon", request, request, chunk, &param)
	var ids []string
	for _, frame := range out {
		for _, toolCall := range gjson.GetBytes(frame, "choices.0.delta.tool_calls").Array() {
			ids = append(ids, toolCall.Get("id").String())
		}
	}
	if len(ids) != 1 || ids[0] != "call_upstream_6825" {
		t.Fatalf("stream tool call ids = %v, want [call_upstream_6825]", ids)
	}

	raw := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_scl","name":"get_time","args":{"tz":"America/Santiago"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-4-argon"}`)
	nonStream := ConvertGeminiResponseToOpenAINonStream(context.Background(), "gemini-4-argon", request, request, raw, nil)
	if got := gjson.GetBytes(nonStream, "choices.0.message.tool_calls.0.id").String(); got != "call_scl" {
		t.Fatalf("non-stream tool call id = %q, want call_scl; payload=%s", got, nonStream)
	}
}

func TestConvertGeminiResponseToOpenAI_SynthesizesToolCallIDWhenUpstreamOmitsIt(t *testing.T) {
	request := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"time?"}],"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}}]}`)
	raw := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_time","args":{"tz":"UTC"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-4-argon"}`)
	nonStream := ConvertGeminiResponseToOpenAINonStream(context.Background(), "gemini-4-argon", request, request, raw, nil)
	if got := gjson.GetBytes(nonStream, "choices.0.message.tool_calls.0.id").String(); got == "" {
		t.Fatalf("tool call id empty for a call without an upstream id; payload=%s", nonStream)
	}
}
