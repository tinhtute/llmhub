package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/wsrelay"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

var multiFrameCalls atomic.Int64

const (
	multiFrameA = `{"candidates":[{"content":{"role":"model","parts":[{"text":"AAA"}]}}],"modelVersion":"gemini-3.1-pro-preview"}`
	multiFrameB = `{"candidates":[{"content":{"role":"model","parts":[{"text":"BBB"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":4,"totalTokenCount":15},"modelVersion":"gemini-3.1-pro-preview"}`
)

// streamThroughRelay runs ExecuteStream for the source format with the given relay chunks (the "data" strings of
// MessageTypeStreamChunk messages, in order) and returns the concatenated client-bound payload, the usage the
// executor published for this call, and whether it published one at all.
func streamThroughRelay(t *testing.T, source sdktranslator.Format, chunks []string) (string, usage.Record, bool) {
	t.Helper()
	// Unique per call: the usage record below is matched on it, so a late record of an earlier call cannot satisfy the check.
	authID := "aistudio-multiframe-" + strconv.FormatInt(multiFrameCalls.Add(1), 10)
	connected := make(chan struct{})
	relay := wsrelay.NewManager(wsrelay.Options{
		ProviderFactory: func(*http.Request) (string, error) { return authID, nil },
		OnConnected:     func(string) { close(connected) },
	})
	server := httptest.NewServer(relay.Handler())
	defer server.Close()
	defer func() {
		if errStop := relay.Stop(context.Background()); errStop != nil {
			t.Errorf("relay stop error = %v", errStop)
		}
	}()
	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+relay.Path(), nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Errorf("websocket close error = %v", errClose)
		}
	}()
	<-connected

	clientDone := make(chan error, 1)
	go func() {
		var msg wsrelay.Message
		if errReadJSON := conn.ReadJSON(&msg); errReadJSON != nil {
			clientDone <- errReadJSON
			return
		}
		responses := []wsrelay.Message{{ID: msg.ID, Type: wsrelay.MessageTypeStreamStart, Payload: map[string]any{"status": 200}}}
		for _, chunk := range chunks {
			responses = append(responses, wsrelay.Message{ID: msg.ID, Type: wsrelay.MessageTypeStreamChunk, Payload: map[string]any{"data": chunk}})
		}
		responses = append(responses, wsrelay.Message{ID: msg.ID, Type: wsrelay.MessageTypeStreamEnd})
		for _, response := range responses {
			if errWriteJSON := conn.WriteJSON(response); errWriteJSON != nil {
				clientDone <- errWriteJSON
				return
			}
		}
		clientDone <- nil
	}()

	capture := &malformedFunctionCallUsageCapture{authID: authID, records: make(chan usage.Record, 16)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), malformedFunctionCallUsageNoop{})
	})

	exec := NewAIStudioExecutor(&config.Config{}, "aistudio", relay)
	req := cliproxyexecutor.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(`{"model":"gemini-3.1-pro-preview","messages":[{"role":"user","content":"hi"}],"stream":true}`)}
	if source == sdktranslator.FormatOpenAIResponse {
		req.Payload = []byte(`{"model":"gemini-3.1-pro-preview","input":"hi","stream":true}`)
	}
	if source == sdktranslator.FormatClaude {
		req.Payload = []byte(`{"model":"gemini-3.1-pro-preview","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	}
	opts := cliproxyexecutor.Options{SourceFormat: source, ResponseFormat: source, OriginalRequest: req.Payload}
	stream, errExecuteStream := exec.ExecuteStream(context.Background(), &cliproxyauth.Auth{ID: authID, Provider: "aistudio"}, req, opts)
	if errExecuteStream != nil {
		t.Fatal(errExecuteStream)
	}
	var out bytes.Buffer
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		out.Write(chunk.Payload)
		out.WriteByte('\n')
	}
	if errClient := <-clientDone; errClient != nil {
		t.Fatal(errClient)
	}
	select {
	case record := <-capture.records:
		return out.String(), record, true
	case <-time.After(3 * time.Second):
		return out.String(), usage.Record{}, false
	}
}

// spaceFree makes the output comparable regardless of the translator's colon
// and comma spacing.
func spaceFree(s string) string {
	return strings.NewReplacer(" ", "", "\t", "").Replace(s)
}

// A relay chunk may hold several SSE frames. Translating only the first frame
// drops the frame carrying the finish reason and the token usage, so the client
// never sees the stream finish and the published usage is empty. Every response
// format must get both frames whether they arrive in one chunk, in two, or with
// CRLF line endings.
func TestAIStudioStreamTranslatesEveryFrameOfAChunk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		format  sdktranslator.Format
		markers []string
	}{
		{
			name:    "openai",
			format:  sdktranslator.FormatOpenAI,
			markers: []string{"AAA", "BBB", `"finish_reason":"stop"`},
		},
		{
			name:    "openai-response",
			format:  sdktranslator.FormatOpenAIResponse,
			markers: []string{"AAA", "BBB", "event:response.completed"},
		},
		{
			name:    "claude",
			format:  sdktranslator.FormatClaude,
			markers: []string{"AAA", "BBB", `"stop_reason":"end_turn"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline, baselineUsage, baselinePublished := streamThroughRelay(t, tc.format, []string{"data: " + multiFrameA + "\n\n", "data: " + multiFrameB + "\n\n"})
			assertFramesReachedClient(t, "baseline (one frame per chunk)", baseline, spaceFree(baseline), tc.markers)
			assertPublishedUsage(t, "baseline (one frame per chunk)", baseline, baselineUsage, baselinePublished)

			for _, variant := range []struct {
				label  string
				chunks []string
			}{
				{label: "one chunk", chunks: []string{"data: " + multiFrameA + "\n\ndata: " + multiFrameB + "\n\n"}},
				{label: "crlf", chunks: []string{"data: " + multiFrameA + "\r\n\r\ndata: " + multiFrameB + "\r\n\r\n"}},
				{label: "event lines", chunks: []string{"event: message\ndata: " + multiFrameA + "\n\nevent: message\ndata: " + multiFrameB + "\n\n"}},
				{label: "trailing keep-alive", chunks: []string{"data: " + multiFrameA + "\n\ndata: " + multiFrameB + "\n\n", ": keep-alive\n\n"}},
			} {
				got, gotUsage, gotPublished := streamThroughRelay(t, tc.format, variant.chunks)
				assertFramesReachedClient(t, variant.label, got, spaceFree(got), tc.markers)
				assertPublishedUsage(t, variant.label, got, gotUsage, gotPublished)
			}
		})
	}
}

func assertFramesReachedClient(t *testing.T, label, raw, compact string, markers []string) {
	t.Helper()
	for _, marker := range markers {
		if !strings.Contains(compact, marker) {
			t.Fatalf("%s: missing %q in output:\n%s", label, marker, raw)
		}
	}
}

func assertPublishedUsage(t *testing.T, label, output string, record usage.Record, published bool) {
	t.Helper()
	if !published {
		t.Fatalf("%s: no usage was published, want the terminal frame's input 11 output 4; output:\n%s", label, output)
	}
	if record.Failed {
		t.Fatalf("%s: the stream published failed usage: %+v", label, record.Fail)
	}
	if record.Detail.InputTokens != 11 || record.Detail.OutputTokens != 4 {
		t.Fatalf("%s: published usage %+v, want input 11 output 4 from the terminal frame", label, record.Detail)
	}
}

// A pretty-printed JSON chunk has blank lines inside it. It is not SSE, so it
// must be translated whole, as before.
func TestAIStudioStreamKeepsPrettyJSONChunkWhole(t *testing.T) {
	pretty := "{\n  \"candidates\": [\n    {\"content\": {\"role\": \"model\", \"parts\": [{\"text\": \"PPP\"}]},\n\n     \"finishReason\": \"STOP\"}\n  ],\n  \"usageMetadata\": {\"promptTokenCount\": 11, \"candidatesTokenCount\": 4, \"totalTokenCount\": 15}\n}\n"
	got, gotUsage, gotPublished := streamThroughRelay(t, sdktranslator.FormatOpenAI, []string{pretty})
	if !strings.Contains(got, "PPP") {
		t.Fatalf("a pretty-printed chunk was cut or lost:\n%s", got)
	}
	assertPublishedUsage(t, "pretty json", got, gotUsage, gotPublished)
}

// The splitter is the whole contract of "which bytes reach the translators":
// SSE frames are cut at blank lines and reduced to their data lines, anything
// else is one frame and never cut.
func TestSplitRelayFrames(t *testing.T) {
	pretty := "{\n  \"candidates\": [\n    {}\n  ]\n\n}\n"
	for _, c := range []struct {
		name, in string
		want     []string
	}{
		{"one data frame", "data: A\n\n", []string{"data: A\n\n"}},
		{"two data frames", "data: A\n\ndata: B\n\n", []string{"data: A\n\n", "data: B\n\n"}},
		{"crlf frames", "data: A\r\n\r\ndata: B\r\n\r\n", []string{"data: A\n\n", "data: B\n\n"}},
		{"event line dropped, data kept", "event: message\ndata: A\n\nevent: message\ndata: B\n\n", []string{"data: A\n\n", "data: B\n\n"}},
		{"id and retry dropped", "id: 7\nretry: 100\ndata: A\n\n", []string{"data: A\n\n"}},
		{"no trailing blank line", "data: A\n\ndata: B", []string{"data: A\n\n", "data: B\n\n"}},
		{"extra blank lines", "data: A\n\n\n\ndata: B\n\n", []string{"data: A\n\n", "data: B\n\n"}},
		{"keep-alive comment dropped", ": keep-alive\n\ndata: A\n\n", []string{"data: A\n\n"}},
		{"only a comment", ": keep-alive\n\n", nil},
		{"leading whitespace", "\n data: A\n\ndata: B\n\n", []string{"data: A\n\n", "data: B\n\n"}},
		{"several data lines of one frame stay together", "data: A1\ndata: A2\n\ndata: B\n\n", []string{"data: A1\ndata: A2\n\n", "data: B\n\n"}},
		{"bare json is one frame, never cut", pretty, []string{pretty}},
		{"array is one frame", "[\n1\n\n,2]", []string{"[\n1\n\n,2]"}},
		{"done is one frame", "[DONE]", []string{"[DONE]"}},
	} {
		got := splitRelayFrames([]byte(c.in))
		if len(got) != len(c.want) {
			t.Errorf("%s: %d frames %q, want %d", c.name, len(got), got, len(c.want))
			continue
		}
		for i := range got {
			if string(got[i]) != c.want[i] {
				t.Errorf("%s: frame %d = %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}
