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

// R5 (upstream v8.0.41 3f5d3a16) on the Antigravity executor: a response with no
// candidate plus a promptFeedback.blockReason is a blocked prompt, not an empty
// completion. Every Antigravity path must answer 502 with the upstream reason
// and record the failure instead of publishing the tokens as a success.

type antigravityBlockedPromptUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *antigravityBlockedPromptUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil || record.Provider != "antigravity" || record.AuthID != c.authID {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type antigravityBlockedPromptUsageNoop struct{}

func (antigravityBlockedPromptUsageNoop) HandleUsage(context.Context, usage.Record) {}

const antigravityBlockedPromptBody = `{"response":{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"blocked by policy"},"usageMetadata":{"promptTokenCount":12,"totalTokenCount":12}}}`

func newAntigravityBlockedPromptAuth(t *testing.T, baseURL string) *cliproxyauth.Auth {
	t.Helper()
	return &cliproxyauth.Auth{
		ID:       t.Name(),
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "test-token",
			"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"project_id":   "test-proj",
		},
		Attributes: map[string]string{"base_url": baseURL},
	}
}

func captureAntigravityBlockedPromptUsage(t *testing.T) *antigravityBlockedPromptUsageCapture {
	t.Helper()
	capture := &antigravityBlockedPromptUsageCapture{authID: t.Name(), records: make(chan usage.Record, 4)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), antigravityBlockedPromptUsageNoop{})
	})
	return capture
}

func assertBlockedPromptUsageFailed(t *testing.T, capture *antigravityBlockedPromptUsageCapture) {
	t.Helper()
	select {
	case record := <-capture.records:
		if !record.Failed {
			t.Fatalf("blocked prompt recorded as a success: %+v", record.Detail)
		}
		if record.Detail.InputTokens != 12 {
			t.Fatalf("blocked prompt usage = %+v, want input 12", record.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no usage record for a blocked prompt")
	}
}

func TestAntigravityExecutorExecuteBlockedPromptReturns502(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, antigravityBlockedPromptBody)
	}))
	defer server.Close()

	capture := captureAntigravityBlockedPromptUsage(t)

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := newAntigravityBlockedPromptAuth(t, server.URL)
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"hi"}]}`),
	}

	_, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	assertStatusError(t, err, http.StatusBadGateway, "upstream blocked the prompt: SAFETY - blocked by policy")
	assertBlockedPromptUsageFailed(t, capture)
}

// The Claude-shaped models take the streaming-then-aggregate non-stream path;
// the block must survive that route too.
func TestAntigravityExecutorExecuteBlockedPromptStreamedPathReturns502(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", antigravityBlockedPromptBody)
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := newAntigravityBlockedPromptAuth(t, server.URL)
	req := cliproxyexecutor.Request{
		Model:   "gemini-3-pro-preview",
		Payload: []byte(`{"model":"gemini-3-pro-preview","messages":[{"role":"user","content":"hi"}]}`),
	}

	_, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	assertStatusError(t, err, http.StatusBadGateway, "upstream blocked the prompt: SAFETY - blocked by policy")
}

func TestAntigravityExecutorStreamBlockedPromptReturns502Chunk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", antigravityBlockedPromptBody)
	}))
	defer server.Close()

	capture := captureAntigravityBlockedPromptUsage(t)

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := newAntigravityBlockedPromptAuth(t, server.URL)
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
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
		if strings.Contains(payload, `"finish_reason":"stop"`) {
			t.Fatalf("blocked prompt must not terminate as a normal stop: %q", payload)
		}
	}
	assertBlockedPromptUsageFailed(t, capture)
}

// A response that carries a candidate is never a prompt block, even when it also
// carries promptFeedback: the answer must still reach the client.
func TestAntigravityExecutorStreamCandidateWithPromptFeedbackPasses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"promptFeedback\":{\"blockReason\":\"SAFETY\"},\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1,\"totalTokenCount\":4}}}\n\n")
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := newAntigravityBlockedPromptAuth(t, server.URL)
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
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
	joined := strings.Join(payloads, "\n")
	if !strings.Contains(joined, `"content":"ok"`) {
		t.Fatalf("answer content missing from the stream: %q", joined)
	}
}
