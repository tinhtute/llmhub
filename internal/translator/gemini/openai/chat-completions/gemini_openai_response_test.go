package chat_completions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sync/atomic"
	"testing"
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
