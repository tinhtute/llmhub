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

// R5 (upstream v8.0.41 3f5d3a16) on the Gemini CLI executor: a response with no
// candidate plus a promptFeedback.blockReason is a blocked prompt, not an empty
// completion. The Gemini and Vertex executors already answer 502 here; the CLI
// executor answered 200 with an empty answer, so the same policy block reached
// the client as a successful turn.

const geminiCLIBlockedPromptBody = `{"response":{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"blocked by policy"},"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":0,"totalTokenCount":7}}}`

// geminiCLIBlockedPromptUsageCapture records the usage records this test's auth
// produced, so a blocked prompt is proven to be reported as a failure.
type geminiCLIBlockedPromptUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *geminiCLIBlockedPromptUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil || record.Provider != "gemini-cli" || record.AuthID != c.authID {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

// geminiCLIPromptBlockServer serves the given body for any Code Assist call and
// points codeAssistEndpoint at itself for the duration of the test.
func geminiCLIPromptBlockServer(t *testing.T, contentType, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	previous := codeAssistEndpoint
	codeAssistEndpoint = server.URL
	t.Cleanup(func() { codeAssistEndpoint = previous })
}

func geminiCLIPromptBlockAuth(t *testing.T) *cliproxyauth.Auth {
	t.Helper()
	return &cliproxyauth.Auth{
		ID:       t.Name(),
		Provider: "gemini-cli",
		Metadata: map[string]any{
			"access_token": "test-access-token",
			"expiry":       time.Now().Add(time.Hour).Format(time.RFC3339),
			"project_id":   "test-project",
		},
	}
}

func TestGeminiCLIExecutorExecuteBlockedPromptReturns502(t *testing.T) {
	geminiCLIPromptBlockServer(t, "application/json", geminiCLIBlockedPromptBody)

	capture := &geminiCLIBlockedPromptUsageCapture{authID: t.Name(), records: make(chan usage.Record, 4)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), geminiBlockedPromptUsageNoop{})
	})

	exec := NewGeminiCLIExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`),
	}

	_, err := exec.Execute(context.Background(), geminiCLIPromptBlockAuth(t), req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	assertStatusError(t, err, http.StatusBadGateway, "upstream blocked the prompt: SAFETY - blocked by policy")

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

func TestGeminiCLIExecutorStreamBlockedPromptReturns502Chunk(t *testing.T) {
	geminiCLIPromptBlockServer(t, "text/event-stream", fmt.Sprintf("data: %s\n\n", geminiCLIBlockedPromptBody))

	exec := NewGeminiCLIExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}

	result, err := exec.ExecuteStream(context.Background(), geminiCLIPromptBlockAuth(t), req, cliproxyexecutor.Options{
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
		if strings.Contains(payload, "chat.completion") {
			t.Fatalf("blocked prompt must not synthesize a completion chunk: %q", payload)
		}
	}
}

// A response with a candidate is never a prompt block, even when it also carries
// promptFeedback: the answer must reach the client.
func TestGeminiCLIExecutorKeepsAnswerThatCarriesPromptFeedback(t *testing.T) {
	geminiCLIPromptBlockServer(t, "application/json", `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"SAFETY"}],"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}}`)

	exec := NewGeminiCLIExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`),
	}

	resp, err := exec.Execute(context.Background(), geminiCLIPromptBlockAuth(t), req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(string(resp.Payload), "ok") {
		t.Fatalf("answer with a candidate was dropped: %s", resp.Payload)
	}
}
