package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	coreexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdkconfig "github.com/therealtinhtute/llmhub/sdk/config"
)

type lifecycleTestExecutor struct {
	mu            sync.Mutex
	payloads      [][]byte
	headers       []http.Header
	streamPayload []byte
	streamErr     error
}

func (e *lifecycleTestExecutor) Identifier() string { return "plugin-test" }

func (e *lifecycleTestExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, cloneBytes(req.Payload))
	e.headers = append(e.headers, cloneHeader(opts.Headers))
	e.mu.Unlock()
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`), Headers: http.Header{"X-Upstream": {"yes"}}}, nil
}

func (e *lifecycleTestExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, cloneBytes(req.Payload))
	e.headers = append(e.headers, cloneHeader(opts.Headers))
	payload := cloneBytes(e.streamPayload)
	e.mu.Unlock()
	streamErr := e.streamErr
	if len(payload) == 0 {
		payload = []byte("stream-ok")
	}
	ch := make(chan coreexecutor.StreamChunk, 2)
	if streamErr != nil {
		ch <- coreexecutor.StreamChunk{Err: streamErr}
	} else {
		ch <- coreexecutor.StreamChunk{Payload: payload}
	}
	close(ch)
	return &coreexecutor.StreamResult{Headers: http.Header{"X-Stream": {"yes"}}, Chunks: ch}, nil
}

func (e *lifecycleTestExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *lifecycleTestExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *lifecycleTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *lifecycleTestExecutor) firstPayload() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.payloads) == 0 {
		return ""
	}
	return string(e.payloads[0])
}

func (e *lifecycleTestExecutor) firstHeader(key string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.headers) == 0 || e.headers[0] == nil {
		return ""
	}
	return e.headers[0].Get(key)
}

type lifecycleTestPlugin struct {
	before func(context.Context, *RequestLifecycleRequest) (*RequestLifecycleDecision, error)
	after  func(context.Context, *RequestLifecycleRequest, *RequestLifecycleResponse) (*RequestLifecycleDecision, error)
}

func (p lifecycleTestPlugin) InterceptBefore(ctx context.Context, req *RequestLifecycleRequest) (*RequestLifecycleDecision, error) {
	if p.before == nil {
		return nil, nil
	}
	return p.before(ctx, req)
}

func (p lifecycleTestPlugin) InterceptAfter(ctx context.Context, req *RequestLifecycleRequest, resp *RequestLifecycleResponse) (*RequestLifecycleDecision, error) {
	if p.after == nil {
		return nil, nil
	}
	return p.after(ctx, req, resp)
}

func newLifecycleTestHandler(t *testing.T, executor *lifecycleTestExecutor) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "plugin-auth", Provider: "plugin-test", Status: coreauth.StatusActive, Metadata: map[string]any{"email": "test@example.com"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "plugin-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
}

func TestRequestLifecyclePluginInterceptsBeforeAndAfterExecute(t *testing.T) {
	executor := &lifecycleTestExecutor{}
	handler := newLifecycleTestHandler(t, executor)
	ctx := WithRequestLifecyclePlugin(context.Background(), lifecycleTestPlugin{
		before: func(_ context.Context, req *RequestLifecycleRequest) (*RequestLifecycleDecision, error) {
			if req.Stream {
				t.Fatalf("expected non-stream lifecycle request")
			}
			return &RequestLifecycleDecision{ReplacePayload: true, Payload: []byte(`{"model":"plugin-model","patched":true}`), Headers: http.Header{"X-Plugin": {"before"}}}, nil
		},
		after: func(_ context.Context, _ *RequestLifecycleRequest, resp *RequestLifecycleResponse) (*RequestLifecycleDecision, error) {
			if resp.Error != nil {
				t.Fatalf("unexpected lifecycle response error: %+v", resp.Error)
			}
			return &RequestLifecycleDecision{ReplacePayload: true, Payload: []byte(`{"plugin":"after"}`), Headers: http.Header{"X-Plugin-After": {"yes"}}}, nil
		},
	})

	body, headers, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "plugin-model", []byte(`{"model":"plugin-model"}`), "")
	if errMsg != nil {
		t.Fatalf("unexpected error: %+v", errMsg)
	}
	if got := string(body); got != `{"plugin":"after"}` {
		t.Fatalf("body = %q", got)
	}
	if executor.firstPayload() != `{"model":"plugin-model","patched":true}` {
		t.Fatalf("executor payload = %q", executor.firstPayload())
	}
	if got := executor.firstHeader("X-Plugin"); got != "before" {
		t.Fatalf("executor plugin header = %q", got)
	}
	if got := headers.Get("X-Plugin-After"); got != "yes" {
		t.Fatalf("after header = %q", got)
	}
}

func TestRequestLifecyclePluginTerminatesBeforeExecute(t *testing.T) {
	executor := &lifecycleTestExecutor{}
	handler := newLifecycleTestHandler(t, executor)
	ctx := WithRequestLifecyclePlugin(context.Background(), lifecycleTestPlugin{
		before: func(context.Context, *RequestLifecycleRequest) (*RequestLifecycleDecision, error) {
			return &RequestLifecycleDecision{Termination: &RequestLifecycleTermination{StatusCode: http.StatusAccepted, Body: []byte(`{"accepted":true}`), Headers: http.Header{"X-Terminated": {"yes"}}}}, nil
		},
	})

	body, headers, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "plugin-model", []byte(`{"model":"plugin-model"}`), "")
	if errMsg != nil {
		t.Fatalf("unexpected error: %+v", errMsg)
	}
	if string(body) != `{"accepted":true}` {
		t.Fatalf("body = %q", string(body))
	}
	if headers.Get("X-Terminated") != "yes" {
		t.Fatalf("missing termination header: %#v", headers)
	}
	if executor.firstPayload() != "" {
		t.Fatalf("executor should not have been called, got payload %q", executor.firstPayload())
	}
}

func TestRequestLifecyclePluginObservesStreamCompletionAndCanEmitResult(t *testing.T) {
	executor := &lifecycleTestExecutor{streamPayload: []byte("first")}
	handler := newLifecycleTestHandler(t, executor)
	ctx := WithRequestLifecyclePlugin(context.Background(), lifecycleTestPlugin{
		after: func(_ context.Context, _ *RequestLifecycleRequest, resp *RequestLifecycleResponse) (*RequestLifecycleDecision, error) {
			if !resp.Stream || resp.Error != nil {
				t.Fatalf("unexpected stream lifecycle response: %+v", resp)
			}
			return &RequestLifecycleDecision{Termination: &RequestLifecycleTermination{StatusCode: http.StatusAccepted, Body: []byte("accepted-result")}}, nil
		},
	})

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "plugin-model", []byte(`{"model":"plugin-model"}`), "")
	var chunks []string
	for chunk := range dataChan {
		chunks = append(chunks, string(chunk))
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("unexpected stream error: %+v", msg)
		}
	}
	if strings.Join(chunks, "|") != "first|accepted-result" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestRequestLifecyclePluginCanReplaceStreamErrorWithAcceptedResult(t *testing.T) {
	executor := &lifecycleTestExecutor{streamErr: &coreauth.Error{Code: "upstream_failed", Message: "upstream failed", HTTPStatus: http.StatusBadGateway}}
	handler := newLifecycleTestHandler(t, executor)
	ctx := WithRequestLifecyclePlugin(context.Background(), lifecycleTestPlugin{
		after: func(_ context.Context, _ *RequestLifecycleRequest, resp *RequestLifecycleResponse) (*RequestLifecycleDecision, error) {
			if resp.Error == nil {
				t.Fatalf("expected stream error")
			}
			return &RequestLifecycleDecision{Termination: &RequestLifecycleTermination{StatusCode: http.StatusAccepted, Body: []byte("accepted-after-error")}}, nil
		},
	})

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "plugin-model", []byte(`{"model":"plugin-model"}`), "")
	var chunks []string
	for chunk := range dataChan {
		chunks = append(chunks, string(chunk))
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("plugin replacement should suppress original stream error, got: %+v", msg)
		}
	}
	if strings.Join(chunks, "|") != "accepted-after-error" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

// A plugin-supplied header whose key is not canonical must still be visible to
// every Header.Get consumer downstream. This drives the real handler and xAI
// executor against an upstream server: the plugin supplies a lowercase
// "user-agent" for a client that sent none, and the xAI request must carry it
// instead of the executor's default Grok workspace User-Agent.
func TestRequestLifecyclePluginLowercaseHeaderReachesXAIUpstream(t *testing.T) {
	userAgent := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent <- r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewXAIExecutor(&internalconfig.Config{}))
	const authID = "xai-lifecycle-plugin"
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "xai", []*registry.ModelInfo{{ID: "grok-4.3"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	// The client sends no User-Agent: only the plugin supplies one.
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	ctx = WithRequestLifecyclePlugin(ctx, lifecycleTestPlugin{
		before: func(context.Context, *RequestLifecycleRequest) (*RequestLifecycleDecision, error) {
			return &RequestLifecycleDecision{Headers: http.Header{"user-agent": {"MyApp/1"}}}, nil
		},
	})

	if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai-response", "grok-4.3", []byte(`{"model":"grok-4.3","input":[{"role":"user","content":"hello"}]}`), ""); errMsg != nil {
		t.Fatalf("unexpected error: %+v", errMsg)
	}
	select {
	case got := <-userAgent:
		if got != "MyApp/1" {
			t.Fatalf("xAI upstream User-Agent = %q, want the plugin-supplied MyApp/1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request did not arrive")
	}
}

func TestSafeStreamEmitterRejectsClosedAndRecoversPanic(t *testing.T) {
	emitter := NewSafeStreamEmitter(func([]byte) bool { panic("boom") })
	if result := emitter.Emit([]byte("x")); result.Accepted || result.Err == nil || !strings.Contains(result.Err.Error(), "panic") {
		t.Fatalf("panic result = %+v", result)
	}
	if err := emitter.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if result := emitter.Emit([]byte("x")); result.Accepted || result.Err == nil || !strings.Contains(result.Err.Error(), "closed") {
		t.Fatalf("closed result = %+v", result)
	}
}
