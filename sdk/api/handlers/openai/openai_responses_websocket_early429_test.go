package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	runtimeexecutor "github.com/therealtinhtute/llmhub/internal/runtime/executor"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// TestResponsesWebsocketEarlyUsageLimit429FailsOverWithoutClosingDownstream covers R4 end to end:
// a Codex websocket upstream answers the first credential with an early 429 usage_limit_reached
// before anything reached the client. The conductor must retry the second credential on the same
// downstream websocket, so the caller sees one completed response served by credential B and the
// socket stays open for the next turn (upstream 86a1aabf0178, issue 7051). When every eligible
// credential returns the early 429, the retry is exhausted and the downstream socket closes
// instead of hanging.
func TestResponsesWebsocketEarlyUsageLimit429FailsOverWithoutClosingDownstream(t *testing.T) {
	early429Frames := map[string]string{
		"error_frame":     `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":600}}`,
		"response_failed": `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","status_code":429,"message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":600}}}`,
	}

	for frameName, early429Frame := range early429Frames {
		for _, buffering := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/buffering=%t", frameName, buffering), func(t *testing.T) {
				gin.SetMode(gin.TestMode)

				// authBRejected makes credential B reject too, for the exhausted-retry phase.
				var authARejects, authBRejects, authBCreates atomic.Int32
				var authBRejected atomic.Bool

				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					key := strings.TrimSpace(r.Header.Get("Authorization"))
					if key != "Bearer key-a" && key != "Bearer key-b" {
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if errUpgrade != nil {
						t.Error(errUpgrade)
						return
					}
					defer func() { _ = conn.Close() }()
					_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
					for {
						_, payload, errRead := conn.ReadMessage()
						if errRead != nil {
							return
						}
						if gjson.GetBytes(payload, "type").String() != "response.create" {
							continue
						}
						reject := func() {
							if key == "Bearer key-a" {
								authARejects.Add(1)
							} else {
								authBRejects.Add(1)
								authBRejected.Store(true)
							}
							_ = conn.WriteMessage(websocket.TextMessage, []byte(early429Frame))
						}
						if key == "Bearer key-a" || authBRejected.Load() {
							reject()
							continue
						}
						served := authBCreates.Add(1)
						respID := fmt.Sprintf("resp-b-%d", served)
						_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress","output":[]}}`, respID)))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"served"}]}]}}`, respID)))
					}
				}))
				t.Cleanup(upstream.Close)

				cfg := &config.Config{
					DisableCooling:                true,
					RequestRetry:                  0,
					MaxRetryCredentials:           2,
					CodexStreamBootstrapBuffering: buffering,
				}
				manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
				manager.SetConfig(cfg)
				manager.SetRetryConfig(cfg.RequestRetry, 0, cfg.MaxRetryCredentials)
				manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))

				modelName := "codex-early-429-" + strings.ReplaceAll(t.Name(), "/", "-")
				for _, spec := range []struct{ key, priority string }{{key: "key-a", priority: "4"}, {key: "key-b", priority: "3"}} {
					authID := modelName + "-" + spec.key
					if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
						ID:       authID,
						Provider: "codex",
						Status:   coreauth.StatusActive,
						Attributes: map[string]string{
							"api_key":    spec.key,
							"base_url":   upstream.URL,
							"websockets": "true",
							"priority":   spec.priority,
						},
						Metadata: map[string]any{"disable_cooling": true},
					}); errRegister != nil {
						t.Fatal(errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelName}})
					id := authID
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				}

				h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
				router := gin.New()
				router.GET("/v1/responses", h.ResponsesWebsocket)
				downstream := httptest.NewServer(router)
				t.Cleanup(downstream.Close)
				wsURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/v1/responses"

				dial := func() *websocket.Conn {
					conn, resp, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
					if resp != nil && resp.Body != nil {
						t.Cleanup(func() { _ = resp.Body.Close() })
					}
					if errDial != nil {
						t.Fatalf("dial downstream websocket: %v", errDial)
					}
					t.Cleanup(func() { _ = conn.Close() })
					return conn
				}
				readCompleted := func(conn *websocket.Conn, turn string) []byte {
					t.Helper()
					for {
						_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
						_, frame, errRead := conn.ReadMessage()
						if errRead != nil {
							t.Fatalf("%s: downstream websocket closed before the retry completed: %v (aRejects=%d bRejects=%d bCreates=%d)",
								turn, errRead, authARejects.Load(), authBRejects.Load(), authBCreates.Load())
						}
						eventType := gjson.GetBytes(frame, "type").String()
						if eventType == "error" || eventType == "response.failed" {
							t.Fatalf("%s: early 429 leaked downstream instead of failing over: %s", turn, frame)
						}
						if eventType == "response.created" && !strings.HasPrefix(gjson.GetBytes(frame, "response.id").String(), "resp-b-") {
							t.Fatalf("%s: rejected credential's handshake leaked downstream: %s", turn, frame)
						}
						if eventType == "response.completed" {
							return frame
						}
					}
				}

				// Phase 1: credential A is rejected with the early 429 and credential B completes
				// the same downstream request.
				conn := dial()
				request := func(conn *websocket.Conn, msgID, content string) {
					t.Helper()
					if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"type":"message","id":%q,"role":"user","content":%q}]}`, modelName, msgID, content))); errWrite != nil {
						t.Fatalf("write %s: %v", msgID, errWrite)
					}
				}
				request(conn, "msg-1", "hello")
				completed := readCompleted(conn, "first turn")
				if got := gjson.GetBytes(completed, "response.id").String(); got != "resp-b-1" {
					t.Fatalf("first turn response.id = %q, want resp-b-1 (credential B); frame=%s", got, completed)
				}
				if authARejects.Load() != 1 || authBCreates.Load() != 1 {
					t.Fatalf("after first turn: aRejects=%d bCreates=%d, want 1 and 1", authARejects.Load(), authBCreates.Load())
				}

				// Phase 2: the same downstream socket serves the next turn, proving the failed
				// attempt never closed it.
				request(conn, "msg-2", "again")
				followup := readCompleted(conn, "follow-up turn")
				if got := gjson.GetBytes(followup, "response.id").String(); got != "resp-b-2" {
					t.Fatalf("follow-up turn response.id = %q, want resp-b-2 (credential B); frame=%s", got, followup)
				}
				// The follow-up stays on the credential that served the previous turn, so it adds
				// no rejection on credential A.
				if authARejects.Load() != 1 || authBCreates.Load() != 2 {
					t.Fatalf("after follow-up turn: aRejects=%d bCreates=%d, want 1 and 2", authARejects.Load(), authBCreates.Load())
				}

				// Phase 3: with no eligible credential left the retry is exhausted after the
				// configured credential cap and the client is told with one error event; the
				// socket stays open so the caller can retry the turn itself.
				authBRejected.Store(true)
				exhausted := dial()
				request(exhausted, "msg-3", "exhausted")
				_ = exhausted.SetReadDeadline(time.Now().Add(10 * time.Second))
				_, frame, errRead := exhausted.ReadMessage()
				if errRead != nil {
					t.Fatalf("exhausted credentials: expected a 429 error event, got %v", errRead)
				}
				if got := gjson.GetBytes(frame, "type").String(); got != "error" {
					t.Fatalf("exhausted credentials: first frame type = %q, want error; frame=%s", got, frame)
				}
				if got := gjson.GetBytes(frame, "status").Int(); got != http.StatusTooManyRequests {
					t.Fatalf("exhausted credentials: status = %d, want %d; frame=%s", got, http.StatusTooManyRequests, frame)
				}
				if got := gjson.GetBytes(frame, "error.type").String(); got != "usage_limit_reached" {
					t.Fatalf("exhausted credentials: error.type = %q, want usage_limit_reached; frame=%s", got, frame)
				}
				if authARejects.Load() != 2 || authBRejects.Load() != 1 {
					t.Fatalf("after exhaustion: aRejects=%d bRejects=%d, want 2 and 1 (credential cap = 2)", authARejects.Load(), authBRejects.Load())
				}
			})
		}
	}
}
