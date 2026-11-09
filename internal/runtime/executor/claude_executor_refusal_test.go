package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// claudeRefusalStreamData is a completed stream whose message_delta carries a
// policy refusal in stop_details — the shape Anthropic returns for a cyber
// policy block (upstream c3938b72bdaa, be69162478da).
const claudeRefusalStreamData = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_refusal","type":"message","role":"assistant","content":[],"model":"claude-opus-5","usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"blocked by policy"}},"usage":{"output_tokens":2}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// TestClaudeExecutorStreamPolicyRefusalIsRequestFault pins that a policy
// refusal ends the stream as a 400 request fault rather than a 502 upstream
// failure, so a single refused prompt cannot put the credential in cooldown.
// Before the fix the refusal was either ignored (stream completed normally) or
// classified as 502.
func TestClaudeExecutorStreamPolicyRefusalIsRequestFault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(claudeRefusalStreamData))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "test-claude-policy-refusal",
		Attributes: map[string]string{
			"api_key":  "key-refusal",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("stream ended without surfacing the policy refusal")
	}
	var status statusErr
	if !errors.As(streamErr, &status) {
		t.Fatalf("stream error type = %T, want statusErr", streamErr)
	}
	if status.code != http.StatusBadRequest {
		t.Fatalf("stream error status = %d, want %d (%v)", status.code, http.StatusBadRequest, streamErr)
	}
}
