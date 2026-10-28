package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// geminiBlockedPromptUsageCapture records the usage records the executor
// publishes for this test's auth.
type geminiBlockedPromptUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *geminiBlockedPromptUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil || record.Provider != "gemini" || record.AuthID != c.authID {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type geminiBlockedPromptUsageNoop struct{}

func (geminiBlockedPromptUsageNoop) HandleUsage(context.Context, usage.Record) {}

// R5 (upstream v8.0.41 3f5d3a16): a candidate-less response carrying a
// promptFeedback.blockReason is a blocked request, not an empty completion. Both
// executors must answer 502 with the upstream reason instead of delivering a
// normal, empty answer to the client.

const geminiBlockedPromptBody = `{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"blocked by policy"},"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":0,"totalTokenCount":7}}`

func assertStatusError(t *testing.T, err error, wantStatus int, wantSubstrings ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %v does not expose StatusCode()", err)
	}
	if got := statusProvider.StatusCode(); got != wantStatus {
		t.Fatalf("StatusCode() = %d, want %d (error: %v)", got, wantStatus, err)
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// drainStreamChunks returns the delivered payloads and the first stream error.
func drainStreamChunks(t *testing.T, result *cliproxyexecutor.StreamResult) ([]string, error) {
	t.Helper()
	var payloads []string
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			if streamErr == nil {
				streamErr = chunk.Err
			}
			continue
		}
		payloads = append(payloads, string(chunk.Payload))
	}
	return payloads, streamErr
}

func TestGeminiExecutorExecuteBlockedPromptReturns502(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, geminiBlockedPromptBody)
	}))
	defer server.Close()

	capture := &geminiBlockedPromptUsageCapture{authID: t.Name(), records: make(chan usage.Record, 4)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), geminiBlockedPromptUsageNoop{})
	})

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         t.Name(),
		Provider:   "gemini",
		Attributes: map[string]string{"api_key": "test-gemini-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`),
	}

	_, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	assertStatusError(t, err, http.StatusBadGateway, "upstream blocked the prompt: SAFETY - blocked by policy")

	// The block is a failure, and the tokens the upstream charged are kept.
	select {
	case record := <-capture.records:
		if !record.Failed {
			t.Fatalf("blocked prompt recorded as a success: %+v", record.Detail)
		}
		if record.Detail.InputTokens != 7 || record.Detail.TotalTokens != 7 {
			t.Fatalf("blocked prompt usage = %+v, want input 7 / total 7", record.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no usage record for a blocked prompt")
	}
}

func TestGeminiExecutorStreamBlockedPromptReturns502Chunk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", geminiBlockedPromptBody)
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "gemini",
		Attributes: map[string]string{"api_key": "test-gemini-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAI,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads, streamErr := drainStreamChunks(t, result)
	assertStatusError(t, streamErr, http.StatusBadGateway, "upstream blocked the prompt: SAFETY")
	for _, payload := range payloads {
		if strings.Contains(payload, "chat.completion.chunk") {
			t.Fatalf("blocked prompt must not synthesize a completion chunk: %q", payload)
		}
	}
}

func TestGeminiExecutorStreamFinishReasonWithoutSignalContentPasses(t *testing.T) {
	// A response with a candidate is never a prompt block, even when it also
	// carries promptFeedback: the answer must reach the client.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"SAFETY\"}],\"promptFeedback\":{\"blockReason\":\"SAFETY\"},\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1}}\n\n")
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "gemini",
		Attributes: map[string]string{"api_key": "test-gemini-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAI,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads, streamErr := drainStreamChunks(t, result)
	if streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	var finishReasons []string
	for _, payload := range payloads {
		if strings.Contains(payload, `"content_filter"`) {
			finishReasons = append(finishReasons, payload)
		}
	}
	if len(finishReasons) != 1 {
		t.Fatalf("content_filter terminal chunks = %d, want 1; payloads=%q", len(finishReasons), payloads)
	}
}

// R5 (upstream v8.0.41 3f5d3a16): an OpenAI-sourced Gemini stream that closes
// without any finish signal is truncated and must report a 502 instead of
// passing an unterminated stream through.
func TestGeminiExecutorStreamMissingFinishReasonOpenAI(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantError bool
	}{
		{
			name:      "missing finish reason",
			body:      "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}\n\n",
			wantError: true,
		},
		{
			name:      "finish reason present",
			body:      "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1}}\n\n",
			wantError: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()

			exec := NewGeminiExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				Provider:   "gemini",
				Attributes: map[string]string{"api_key": "test-gemini-key", "base_url": server.URL},
			}
			req := cliproxyexecutor.Request{
				Model:   "gemini-2.5-flash",
				Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
			}
			result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAI,
				ResponseFormat: sdktranslator.FormatOpenAI,
				Stream:         true,
			})
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			_, streamErr := drainStreamChunks(t, result)
			if tc.wantError {
				assertStatusError(t, streamErr, http.StatusBadGateway, "finishReason")
				return
			}
			if streamErr != nil {
				t.Fatalf("unexpected stream error: %v", streamErr)
			}
		})
	}
}

func TestGeminiVertexExecutorExecuteBlockedPromptReturns502(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, geminiBlockedPromptBody)
	}))
	defer server.Close()

	exec := NewGeminiVertexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "vertex",
		Attributes: map[string]string{"api_key": "test-vertex-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`),
	}

	_, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	assertStatusError(t, err, http.StatusBadGateway, "upstream blocked the prompt: SAFETY - blocked by policy")
}

func TestGeminiVertexExecutorStreamBlockedPromptReturns502Chunk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", geminiBlockedPromptBody)
	}))
	defer server.Close()

	exec := NewGeminiVertexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "vertex",
		Attributes: map[string]string{"api_key": "test-vertex-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAI,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	payloads, streamErr := drainStreamChunks(t, result)
	assertStatusError(t, streamErr, http.StatusBadGateway, "upstream blocked the prompt: SAFETY")
	for _, payload := range payloads {
		if strings.Contains(payload, "chat.completion.chunk") {
			t.Fatalf("blocked prompt must not synthesize a completion chunk: %q", payload)
		}
	}
}

func TestGeminiVertexExecutorStreamMissingFinishReasonOpenAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}\n\n")
	}))
	defer server.Close()

	exec := NewGeminiVertexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "vertex",
		Attributes: map[string]string{"api_key": "test-vertex-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}
	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAI,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	_, streamErr := drainStreamChunks(t, result)
	assertStatusError(t, streamErr, http.StatusBadGateway, "finishReason")
}
