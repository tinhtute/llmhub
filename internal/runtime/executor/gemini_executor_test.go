package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCapGeminiMaxOutputTokensUsesOutputTokenLimit(t *testing.T) {
	body := []byte(`{"generationConfig":{"maxOutputTokens":500000,"temperature":0.2},"contents":[]}`)

	out := capGeminiMaxOutputTokens(body, "gemini-3.1-pro-preview")

	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 65536 {
		t.Fatalf("maxOutputTokens = %d, want 65536", got)
	}
	if got := gjson.GetBytes(out, "generationConfig.temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2", got)
	}
}

func TestCapGeminiMaxOutputTokensLeavesAllowedOrUnknown(t *testing.T) {
	tests := []struct {
		name  string
		model string
		body  []byte
		want  int64
	}{
		{
			name:  "allowed value",
			model: "gemini-3.1-pro-preview",
			body:  []byte(`{"generationConfig":{"maxOutputTokens":64000}}`),
			want:  64000,
		},
		{
			name:  "unknown model",
			model: "custom-gemini-model",
			body:  []byte(`{"generationConfig":{"maxOutputTokens":500000}}`),
			want:  500000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := capGeminiMaxOutputTokens(tt.body, tt.model)
			if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != tt.want {
				t.Fatalf("maxOutputTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGeminiExecutorExecuteCapsMaxOutputTokensBeforeUpstream(t *testing.T) {
	var upstreamMaxOutputTokens int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		upstreamMaxOutputTokens = gjson.GetBytes(body, "generationConfig.maxOutputTokens").Int()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "test-key",
		"base_url": server.URL,
	}}
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-preview",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":500000}}`),
	}

	if _, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if upstreamMaxOutputTokens != 65536 {
		t.Fatalf("upstream maxOutputTokens = %d, want 65536", upstreamMaxOutputTokens)
	}
}

func assertGeminiLeadingUserContents(t *testing.T, body []byte) {
	t.Helper()
	contents := gjson.GetBytes(body, "contents").Array()
	if len(contents) < 2 {
		t.Fatalf("contents length = %d, want at least 2; body=%s", len(contents), body)
	}
	if contents[0].Get("role").String() != "user" || contents[1].Get("role").String() != "model" {
		t.Fatalf("leading roles = %q, %q; body=%s", contents[0].Get("role").String(), contents[1].Get("role").String(), body)
	}
	if text := contents[0].Get("parts.0.text"); !text.Exists() || text.String() != "" {
		t.Fatalf("synthetic leading user missing; body=%s", body)
	}
}

func TestGeminiExecutorExecutePrependsLeadingUser(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		upstreamBody = append([]byte(nil), body...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "test-key",
		"base_url": server.URL,
	}}
	request := cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"prior output"}]},{"role":"user","parts":[{"text":"continue"}]}]}`),
	}

	if _, errExecute := exec.Execute(context.Background(), auth, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	assertGeminiLeadingUserContents(t, upstreamBody)
}

func TestGeminiExecutorStreamPrependsLeadingUser(t *testing.T) {
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
			return
		}
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "test-key",
		"base_url": server.URL,
	}}
	result, errExecute := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"prior output"}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Stream: true})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	assertGeminiLeadingUserContents(t, <-captured)
}

func TestGeminiExecutorCountTokensPrependsLeadingUser(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		upstreamBody = append([]byte(nil), body...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"totalTokens":7}`))
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "test-key",
		"base_url": server.URL,
	}}
	request := cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"prior output"}]}]}`),
	}

	if _, errCount := exec.CountTokens(context.Background(), auth, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	assertGeminiLeadingUserContents(t, upstreamBody)
}

// TestSanitizeGeminiInteractionsUnsupportedInputIDs covers the Interactions
// input-step ID rules ported from upstream f51d3ae93b87: function_call steps
// require `id` (backfilled from `call_id`) and reject `call_id`,
// function_result steps keep `call_id` and reject `id`, and every other step
// or content part drops `id`. Upstream exercises this end-to-end through its
// native Interactions executor path, which llmhub does not have, so the
// semantics are asserted on the sanitizer directly.
func TestSanitizeGeminiInteractionsUnsupportedInputIDs(t *testing.T) {
	t.Run("function_call backfills id from call_id and drops call_id", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"function_call","name":"bash","call_id":"toolu_1","arguments":{"command":"ls"}}]}`)
		out := sanitizeGeminiInteractionsUnsupportedInputIDs(body)
		if got := gjson.GetBytes(out, "input.0.id").String(); got != "toolu_1" {
			t.Fatalf("function_call id = %q, want toolu_1; body=%s", got, out)
		}
		if gjson.GetBytes(out, "input.0.call_id").Exists() {
			t.Fatalf("function_call call_id must be omitted; body=%s", out)
		}
	})

	t.Run("function_call keeps explicit id and still drops call_id", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"function_call","name":"bash","id":"call_9","call_id":"toolu_9"}]}`)
		out := sanitizeGeminiInteractionsUnsupportedInputIDs(body)
		if got := gjson.GetBytes(out, "input.0.id").String(); got != "call_9" {
			t.Fatalf("function_call id = %q, want call_9; body=%s", got, out)
		}
		if gjson.GetBytes(out, "input.0.call_id").Exists() {
			t.Fatalf("function_call call_id must be omitted; body=%s", out)
		}
	})

	t.Run("function_result drops id and keeps call_id", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"function_result","id":"call_1","call_id":"toolu_1","result":"ok"}]}`)
		out := sanitizeGeminiInteractionsUnsupportedInputIDs(body)
		if gjson.GetBytes(out, "input.0.id").Exists() {
			t.Fatalf("function_result id must be omitted; body=%s", out)
		}
		if got := gjson.GetBytes(out, "input.0.call_id").String(); got != "toolu_1" {
			t.Fatalf("function_result call_id = %q, want toolu_1; body=%s", got, out)
		}
	})

	t.Run("other steps and content parts drop id", func(t *testing.T) {
		body := []byte(`{"input":[
			{"type":"text","id":"x1","text":"hi"},
			{"type":"model_output","id":"x2","content":[{"type":"text","id":"p1","text":"planning"}]},
			{"type":"function_call","name":"read_file","call_id":"call_2","content":[{"type":"text","id":"p2","text":"note"}]}
		]}`)
		out := sanitizeGeminiInteractionsUnsupportedInputIDs(body)
		if gjson.GetBytes(out, "input.0.id").Exists() {
			t.Fatalf("text step id must be omitted; body=%s", out)
		}
		if gjson.GetBytes(out, "input.1.id").Exists() {
			t.Fatalf("model_output step id must be omitted; body=%s", out)
		}
		if gjson.GetBytes(out, "input.1.content.0.id").Exists() {
			t.Fatalf("content part id must be omitted; body=%s", out)
		}
		if gjson.GetBytes(out, "input.2.content.0.id").Exists() {
			t.Fatalf("function_call content part id must be omitted; body=%s", out)
		}
		if got := gjson.GetBytes(out, "input.2.id").String(); got != "call_2" {
			t.Fatalf("function_call id = %q, want call_2 backfilled from call_id; body=%s", got, out)
		}
	})

	t.Run("non-array input is unchanged", func(t *testing.T) {
		body := []byte(`{"input":{"type":"function_call","id":"x","call_id":"y"}}`)
		out := sanitizeGeminiInteractionsUnsupportedInputIDs(body)
		if string(out) != string(body) {
			t.Fatalf("non-array input was modified: %s", out)
		}
	})
}

// A mid-stream Gemini content filter frame is a request fault: the stream must
// end with a 400 instead of a 502 so a single-prompt trigger does not cool the
// credential (upstream c3938b72bdaa).
func TestGeminiExecutorStreamFilterBlockReturns400(t *testing.T) {
	thoughtFrame := `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"thinking"}]}}],"usageMetadata":{"promptTokenCount":100,"thoughtsTokenCount":10,"totalTokenCount":110},"modelVersion":"gemini-4-argon-b","responseId":"resp-1"}`
	filterFrame := `{"candidates":[{"content":{"parts":[{"text":"This request was blocked by Gemini's filters. They can occasionally trigger by mistake on safe coding, security, or biology-related queries. Please try rephrasing your prompt. You can [send feedback](https://ai.google.dev/gemini-api/docs/troubleshooting#file-bug) or read more about [our policies here](https://policies.google.com/terms/generative-ai/use-policy)."}]}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+thoughtFrame+"\n\ndata: "+filterFrame+"\n\n")
	}))
	defer server.Close()

	capture := &geminiBlockedPromptUsageCapture{authID: t.Name(), records: make(chan usage.Record, 4)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), geminiBlockedPromptUsageNoop{})
	})

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       t.Name(),
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-4-argon-high",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	}
	streamRes, errStream := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatGemini,
		Stream:         true,
	})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	var streamErr error
	for chunk := range streamRes.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("ExecuteStream() chunk.Err = nil, want 400 filter block error")
	}
	status, okStatus := streamErr.(interface{ StatusCode() int })
	if !okStatus || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("ExecuteStream() chunk.Err = %T %v, want status 400", streamErr, streamErr)
	}
	// The failure record keeps the tokens the upstream charged before the block.
	select {
	case record := <-capture.records:
		if !record.Failed {
			t.Fatalf("filter block recorded as a success: %+v", record.Detail)
		}
		if record.Detail.InputTokens != 100 || record.Detail.TotalTokens != 110 {
			t.Fatalf("filter block usage = %+v, want input 100 / total 110", record.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no usage record for a filter block")
	}
}
