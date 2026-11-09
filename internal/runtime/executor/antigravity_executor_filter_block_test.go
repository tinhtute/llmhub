package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// A real Antigravity response that quotes the filter notice is not a filter
// block: the usageMetadata it carries marks the frame as upstream evidence.
// Classification runs on the unfiltered payload (upstream be69162478da), so the
// client-facing usage filter and its cpaUsageMetadata rename cannot turn the
// frame into a 400.
func TestAntigravityStreamPreservesQuotedFilterNoticeWithUsageMetadata(t *testing.T) {
	quotedNotice := "This request was blocked by Gemini's filters. They can occasionally trigger by mistake on safe coding, security, or biology-related queries. Please try rephrasing your prompt. You can [send feedback](https://ai.google.dev/gemini-api/docs/troubleshooting#file-bug) or read more about [our policies here](https://policies.google.com/terms/generative-ai/use-policy)."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"response":{"candidates":[{"content":{"parts":[{"text":"`+quotedNotice+`"}]}}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":40,"totalTokenCount":52}}}`)
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := newAntigravityBlockedPromptAuth(t, server.URL)
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"explain the filter message"}],"stream":true}`),
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
	if !strings.Contains(joined, "blocked by Gemini's filters") {
		t.Fatalf("expected the quoted filter notice in the stream output, got %q", joined)
	}
}
