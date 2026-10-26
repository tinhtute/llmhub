package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/cache"
	internalconfig "github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor"
	_ "github.com/therealtinhtute/llmhub/internal/translator"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	coreexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	coresession "github.com/therealtinhtute/llmhub/sdk/cliproxy/session"
	sdkconfig "github.com/therealtinhtute/llmhub/sdk/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// requestExecutionMetadata must record which downstream caller produced the
// request, as an irreversible scope derived from the authenticated principal.
func TestRequestExecutionMetadataIncludesHashedCallerScope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, principal := range []string{"downstream-key-a", "downstream-key-b"} {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ginCtx.Set("userApiKey", principal)
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)
		got, _ := meta[coreexecutor.CallerScopeMetadataKey].(string)
		want := coresession.CallerScope(principal)
		if got != want {
			t.Fatalf("caller scope for %q = %q, want %q", principal, got, want)
		}
		if strings.Contains(got, principal) {
			t.Fatalf("caller scope %q contains the raw downstream credential", got)
		}
	}

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	if _, exists := requestExecutionMetadata(ctx)[coreexecutor.CallerScopeMetadataKey]; exists {
		t.Fatal("unauthenticated request must not carry a caller scope")
	}
}

const callerScopeFirstRequest = `{"model":"gemini-3-flash","tool_choice":"auto","messages":[{"role":"user","content":"What time is it?"}],"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}}]}`

type callerScopeUpstream struct {
	t        *testing.T
	server   *httptest.Server
	requests chan []byte

	mu          sync.Mutex
	responseIdx int
	responses   []string
}

func newCallerScopeUpstream(t *testing.T, responses ...string) *callerScopeUpstream {
	t.Helper()
	cache.ClearAntigravityReasoningReplayCache()
	t.Cleanup(cache.ClearAntigravityReasoningReplayCache)
	upstream := &callerScopeUpstream{t: t, requests: make(chan []byte, 8), responses: responses}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read upstream request: %v", errRead)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		upstream.requests <- body
		upstream.mu.Lock()
		response := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Done."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`
		if upstream.responseIdx < len(upstream.responses) {
			response = upstream.responses[upstream.responseIdx]
		}
		upstream.responseIdx++
		upstream.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", response)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *callerScopeUpstream) next() []byte {
	u.t.Helper()
	select {
	case body := <-u.requests:
		return body
	case <-time.After(5 * time.Second):
		u.t.Fatal("upstream request did not arrive")
		return nil
	}
}

// handlerCallerContext builds the request context the way the server does: the
// authenticated principal sits on the gin context, headers arrive through it.
func handlerCallerContext(t *testing.T, principal, sessionID string) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		ginCtx.Request.Header.Set("X-Session-ID", sessionID)
	}
	if principal != "" {
		ginCtx.Set("userApiKey", principal)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

// streamAssistantMessage drives the real OpenAI chat handler and folds the
// streamed deltas back into one assistant message.
func streamAssistantMessage(t *testing.T, handler *BaseAPIHandler, ctx context.Context) []byte {
	t.Helper()
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "gemini-3-flash", []byte(callerScopeFirstRequest), "")
	if dataChan == nil || errChan == nil {
		t.Fatal("handler returned nil channels")
	}
	var content, reasoning string
	var calls []map[string]any
	for chunk := range dataChan {
		for _, line := range bytes.Split(chunk, []byte("\n")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(line) == 0 || !gjson.ValidBytes(line) {
				continue
			}
			delta := gjson.GetBytes(line, "choices.0.delta")
			content += delta.Get("content").String()
			reasoning += delta.Get("reasoning_content").String()
			for _, call := range delta.Get("tool_calls").Array() {
				index := int(call.Get("index").Int())
				if index != len(calls) {
					t.Fatalf("unexpected new tool index %d, want %d", index, len(calls))
				}
				calls = append(calls, map[string]any{
					"id":   call.Get("id").String(),
					"type": "function",
					"function": map[string]string{
						"name":      call.Get("function.name").String(),
						"arguments": call.Get("function.arguments").String(),
					},
				})
			}
		}
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("handler error: %+v", msg)
		}
	}
	message := []byte(`{"role":"assistant"}`)
	if len(calls) > 0 {
		message, _ = sjson.SetBytes(message, "tool_calls", calls)
	}
	if content != "" {
		message, _ = sjson.SetBytes(message, "content", content)
	}
	if reasoning != "" {
		message, _ = sjson.SetBytes(message, "reasoning_content", reasoning)
	}
	return message
}

func callerScopeFollowup(t *testing.T, assistant []byte) []byte {
	t.Helper()
	calls := gjson.GetBytes(assistant, "tool_calls").Array()
	if len(calls) == 0 {
		t.Fatalf("turn one returned no tool calls: %s", assistant)
	}
	out, errSet := sjson.SetRawBytes([]byte(callerScopeFirstRequest), "messages.-1", assistant)
	if errSet != nil {
		t.Fatal(errSet)
	}
	for _, call := range calls {
		out, errSet = sjson.SetBytes(out, "messages.-1", map[string]string{"role": "tool", "tool_call_id": call.Get("id").String(), "content": "12:00 UTC"})
		if errSet != nil {
			t.Fatal(errSet)
		}
	}
	return out
}

func callerScopeToolSignature(t *testing.T, request []byte) string {
	t.Helper()
	for _, content := range gjson.GetBytes(request, "request.contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("functionCall.name").String() == "get_time" {
				return part.Get("thoughtSignature").String()
			}
		}
	}
	t.Fatalf("missing historical functionCall in upstream request: %s", request)
	return ""
}

func callerScopeToolResponse(signature string) string {
	return `{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_time","args":{"tz":"UTC"}},"thoughtSignature":"` + signature + `"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":17}}}`
}

func callerScopeSignature(t *testing.T, fill byte) string {
	t.Helper()
	payload := append([]byte{0x0A}, bytes.Repeat([]byte{fill}, 48)...)
	return base64Std(payload)
}

// Two API keys that send the same downstream session alias must keep separate
// replay ledgers: caller B's turn must not replace caller A's signature.
func TestAntigravityReplayCallerIsolationThroughHandler(t *testing.T) {
	sigA, sigB := callerScopeSignature(t, 0x56), callerScopeSignature(t, 0x57)
	upstream := newCallerScopeUpstream(t, callerScopeToolResponse(sigA), callerScopeToolResponse(sigB))

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewAntigravityExecutor(&internalconfig.Config{}))
	const authID = "antigravity-handler-caller-scope"
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url": upstream.server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: "gemini-3-flash"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	ctxA := handlerCallerContext(t, "downstream-key-a", "shared-session")
	ctxB := handlerCallerContext(t, "downstream-key-b", "shared-session")

	// Turn one for each caller, both on the same downstream session alias.
	assistantA := streamAssistantMessage(t, handler, ctxA)
	firstA := upstream.next()
	if got := gjson.GetBytes(firstA, "request.contents.0.parts.0.thoughtSignature").String(); got != "" {
		t.Fatalf("first turn must not replay a signature, got %q", got)
	}
	assistantB := streamAssistantMessage(t, handler, ctxB)
	upstream.next()

	// Caller B completes a turn, which replaces whatever the shared ledger held.
	ginB := handlerCallerContext(t, "downstream-key-b", "shared-session")
	followupB := callerScopeFollowup(t, assistantB)
	streamFollowup(t, handler, ginB, followupB)
	upstream.next()

	// Caller A's next turn must still replay A's own signature.
	ginA := handlerCallerContext(t, "downstream-key-a", "shared-session")
	streamFollowup(t, handler, ginA, callerScopeFollowup(t, assistantA))
	secondA := upstream.next()
	if got := callerScopeToolSignature(t, secondA); got != sigA {
		t.Fatalf("caller A replay signature = %q, want its own %q (caller B = %q)", got, sigA, sigB)
	}
}

// streamFollowup drives one follow-up turn through the same handler entry point.
func streamFollowup(t *testing.T, handler *BaseAPIHandler, ctx context.Context, payload []byte) {
	t.Helper()
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "gemini-3-flash", payload, "")
	if dataChan == nil || errChan == nil {
		t.Fatal("handler returned nil channels")
	}
	for range dataChan {
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("handler error: %+v", msg)
		}
	}
}

func base64Std(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}
