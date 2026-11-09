package executor

import (
	"context"
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

// Terminal-stream regression tests ported from upstream 7c32971b91c8
// (claude_executor_stream_terminal_test.go): a client disconnect after the
// upstream stream reached its terminal event must not surface as a stream
// failure, and the executor must stop reading upstream once completion was
// forwarded.
//
// Local API adaptations: the usage manager only supports RegisterPlugin (no
// named/unregister API), so the capture plugin filters records by a unique
// auth ID per test. Upstream's token assertions (InputTokens=100,
// OutputTokens=15) are not portable: the local UsageReporter.buildRecordForModel
// drops the Detail field entirely, so the Failed=false assertion is the parity
// signal here. ParseClaudeStreamUsage does carry the message.usage fallback and
// the executor does merge frames through StreamUsageBuffer as of 2ecb4e979.

type captureClaudeUsagePlugin struct {
	authID  string
	records chan usage.Record
}

func (p *captureClaudeUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if p == nil || record.Provider != "claude" || record.AuthID != p.authID {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

const claudeTerminalStreamData = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[],"model":"claude-opus-5","stop_reason":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// newClaudeTerminalStreamServer returns a test server that writes the full SSE
// stream immediately, then holds the response body open until the client
// disconnects or the test ends — reproducing upstream lag where the body close
// happens after the terminal event.
func newClaudeTerminalStreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	upstreamClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(claudeTerminalStreamData))
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-upstreamClosed:
		}
	}))
	t.Cleanup(func() {
		close(upstreamClosed)
		server.Close()
	})
	return server
}

// drainClaudeStreamAfterTerminal asserts that no error chunk follows the
// terminal event and that the channel closes promptly even though the server
// still holds the response body open. The result is funnelled back to the test
// goroutine so a lingering producer cannot panic on t.Errorf after the test
// has already failed.
func drainClaudeStreamAfterTerminal(t *testing.T, chunks <-chan cliproxyexecutor.StreamChunk) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		for chunk := range chunks {
			if chunk.Err != nil {
				errCh <- chunk.Err
				return
			}
		}
		errCh <- nil
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected chunk error after terminal event: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close promptly after terminal event")
	}
}

func TestClaudeExecutor_ExecuteStream_Translated_ClientDisconnectAfterTerminalEventIsNotFailed(t *testing.T) {
	server := newClaudeTerminalStreamServer(t)

	const authID = "test-claude-translated-disconnect"
	plugin := &captureClaudeUsagePlugin{
		authID:  authID,
		records: make(chan usage.Record, 4),
	}
	usage.RegisterPlugin(plugin)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: authID,
		Attributes: map[string]string{
			"api_key":  "key-123",
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
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	sawTerminalEvent := false
	// Read chunks until the terminal event is seen.
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error during stream read: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "response.completed") {
			sawTerminalEvent = true
			break
		}
	}
	if !sawTerminalEvent {
		t.Fatal("expected to observe terminal response.completed event before stream finished")
	}
	// The executor must stop reading upstream once the terminal event was
	// forwarded: the channel must close promptly even though the test server
	// still holds the response body open (upstream 7c32971b91c8). This runs
	// before the disconnect so a missing break is caught deterministically.
	drainClaudeStreamAfterTerminal(t, result.Chunks)
	// Client disconnects immediately upon receiving the terminal event
	// (e.g. Codex CLI >=0.153.4).
	cancel()

	select {
	case record := <-plugin.records:
		if record.Failed {
			t.Fatalf("expected usage record to not be marked failed, but got failed=true, fail status: %d body: %s", record.Fail.StatusCode, record.Fail.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestClaudeExecutor_ExecuteStream_Passthrough_ClientDisconnectAfterTerminalEventIsNotFailed(t *testing.T) {
	server := newClaudeTerminalStreamServer(t)

	const authID = "test-claude-passthrough-disconnect"
	plugin := &captureClaudeUsagePlugin{
		authID:  authID,
		records: make(chan usage.Record, 4),
	}
	usage.RegisterPlugin(plugin)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: authID,
		Attributes: map[string]string{
			"api_key":  "key-123",
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
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	sawTerminalEvent := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "message_stop") {
			sawTerminalEvent = true
			break
		}
	}
	if !sawTerminalEvent {
		t.Fatal("expected to observe terminal message_stop event before stream finished")
	}
	// Same as above: prompt close while the server still holds the body open
	// proves the scan loop broke at upstream completion (upstream 7c32971b91c8).
	drainClaudeStreamAfterTerminal(t, result.Chunks)
	cancel()

	select {
	case record := <-plugin.records:
		if record.Failed {
			t.Fatalf("expected usage record to not be marked failed in passthrough, but got failed=true, fail status: %d body: %s", record.Fail.StatusCode, record.Fail.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}
