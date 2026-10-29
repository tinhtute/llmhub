package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

const (
	codexEarly429ErrorFrame     = `{"type":"error","status":429,"headers":{"X-Request-Id":"early-429"},"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":600}}`
	codexEarly429FailedFrame    = `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","status_code":429,"message":"The usage limit has been reached","resets_in_seconds":600}}}`
	codexEarly429PreambleCreate = `{"type":"response.created","response":{"id":"resp-early","status":"in_progress","output":[]}}`
	codexEarly429PreambleProg   = `{"type":"response.in_progress","response":{"id":"resp-early","status":"in_progress","output":[]}}`
	codexEarly429OutputDelta    = `{"type":"response.output_text.delta","item_id":"msg_early","output_index":0,"content_index":0,"delta":"partial"}`
)

// codexEarly429Server serves one websocket turn: it reads the submitted request, writes frames,
// then keeps the socket open so the executor must terminate it on its own.
func codexEarly429Server(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Error(errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Error(errRead)
			return
		}
		for _, frame := range frames {
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(frame)); errWrite != nil {
				t.Error(errWrite)
				return
			}
		}
		for {
			// Hold the connection open: a pass here must not depend on the peer closing.
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func codexEarly429Executor(t *testing.T, buffering bool) *CodexWebsocketsExecutor {
	t.Helper()
	exec := NewCodexWebsocketsExecutor(&config.Config{CodexStreamBootstrapBuffering: buffering})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	return exec
}

// codexEarly429ExecuteStream runs one websocket turn and reports the surfaced failure together
// with every payload the caller would have observed.
func codexEarly429ExecuteStream(t *testing.T, exec *CodexWebsocketsExecutor, sessionID, serverURL string) (error, [][]byte) {
	t.Helper()
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
	return codexEarly429ExecuteStreamOnce(t, exec, sessionID, serverURL)
}

func codexEarly429ExecuteStreamOnce(t *testing.T, exec *CodexWebsocketsExecutor, sessionID, serverURL string) (error, [][]byte) {
	t.Helper()
	auth := &cliproxyauth.Auth{ID: "auth-early-429", Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{
		"api_key":    "sk-test",
		"base_url":   serverURL,
		"websockets": "true",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-terra",
		Payload: []byte(`{"model":"gpt-5.6-terra","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	var payloads [][]byte
	if result == nil {
		return err, payloads
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		payloads = append(payloads, chunk.Payload)
	}
	if streamErr != nil {
		return streamErr, payloads
	}
	return err, payloads
}

// An upstream usage-limit 429 arriving before any downstream payload chunk is a failover
// decision, not a downstream disconnect: the executor must not signal UpstreamDisconnectChan,
// which is what makes the Responses websocket handler close the client socket and cancel the
// request context before the conductor retries another credential (upstream 86a1aabf0178).
func TestCodexWebsocketsEarlyUsageLimit429DoesNotNotifyDisconnect(t *testing.T) {
	frames := map[string]string{
		"error_frame":     codexEarly429ErrorFrame,
		"response_failed": codexEarly429FailedFrame,
	}
	cases := []struct {
		label     string
		buffering bool
		preamble  []string
		// failover is true only while the bootstrap window is open: the attempt then fails
		// before any downstream byte is committed, so the conductor can pick another credential.
		failover bool
	}{
		{label: "immediate/buffering=false", buffering: false, failover: false},
		{label: "immediate/buffering=true", buffering: true, failover: true},
		{label: "after_handshake/buffering=true", buffering: true, preamble: []string{codexEarly429PreambleCreate, codexEarly429PreambleProg}, failover: true},
	}
	for name, frame := range frames {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/%s", name, tc.label), func(t *testing.T) {
				frames := append(append([]string(nil), tc.preamble...), frame)
				server := codexEarly429Server(t, frames...)
				sessionID := fmt.Sprintf("early-429-%s-%s", name, tc.label)

				exec := codexEarly429Executor(t, tc.buffering)
				disconnects := exec.UpstreamDisconnectChan(sessionID)
				if disconnects == nil {
					t.Fatal("expected a disconnect channel for the session")
				}

				streamErr, payloads := codexEarly429ExecuteStream(t, exec, sessionID, server.URL)
				if streamErr == nil {
					t.Fatal("expected the upstream usage-limit failure to surface")
				}
				coded, ok := streamErr.(interface{ StatusCode() int })
				if !ok {
					t.Fatalf("failure %T has no status code: %v", streamErr, streamErr)
				}
				if got := coded.StatusCode(); got != http.StatusTooManyRequests {
					t.Fatalf("status code = %d, want %d: %v", got, http.StatusTooManyRequests, streamErr)
				}
				if tc.failover && len(payloads) != 0 {
					t.Fatalf("bootstrap-buffered handshake leaked downstream before the retry: %d chunks", len(payloads))
				}

				select {
				case disconnectErr := <-disconnects:
					t.Fatalf("pre-output usage-limit 429 must not notify the downstream disconnect, got %v", disconnectErr)
				case <-time.After(200 * time.Millisecond):
				}
			})
		}
	}
}

// After the turn has started emitting payload, a usage-limit 429 keeps today's behaviour: the
// error is delivered in-stream and only then is the downstream disconnect published, so the
// client socket still terminates with the failure.
func TestCodexWebsocketsLateUsageLimit429StillNotifiesDisconnect(t *testing.T) {
	frames := map[string]string{
		"error_frame":     codexEarly429ErrorFrame,
		"response_failed": codexEarly429FailedFrame,
	}
	for name, frame := range frames {
		for _, buffering := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/buffering=%t", name, buffering), func(t *testing.T) {
				server := codexEarly429Server(t, codexEarly429PreambleCreate, codexEarly429OutputDelta, frame)
				sessionID := fmt.Sprintf("late-429-%s-%t", name, buffering)

				exec := codexEarly429Executor(t, buffering)
				disconnects := exec.UpstreamDisconnectChan(sessionID)
				if disconnects == nil {
					t.Fatal("expected a disconnect channel for the session")
				}

				streamErr, payloads := codexEarly429ExecuteStream(t, exec, sessionID, server.URL)
				if streamErr == nil {
					t.Fatal("expected the upstream usage-limit failure to surface")
				}
				if len(payloads) == 0 {
					t.Fatal("expected the started turn to reach the downstream before the failure")
				}

				select {
				case disconnectErr, open := <-disconnects:
					if !open {
						t.Fatal("disconnect channel closed without an error after a started turn")
					}
					if disconnectErr == nil {
						t.Fatal("expected the upstream failure on the disconnect channel")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("a started turn must still publish the upstream disconnect")
				}
			})
		}
	}
}
