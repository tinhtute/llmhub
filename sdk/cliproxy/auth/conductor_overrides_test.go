package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
)

const requestScopedNotFoundMessage = "Item with id 'rs_0b5f3eb6f51f175c0169ca74e4a85881998539920821603a74' not found. Items are not persisted when `store` is set to false. Try again with `store` set to true, or remove this item from your input."

func TestManager_ShouldRetryAfterError_RespectsAuthRequestRetryOverride(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"request_retry": float64(0),
		},
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, _, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for request_retry=0, got true (wait=%v)", wait)
	}

	auth.Metadata["request_retry"] = float64(1)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	wait, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true for request_retry=1, got false")
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}

	_, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 1, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false on attempt=1 for request_retry=1, got true")
	}
}

func TestManager_ShouldRetryAfterError_UsesOAuthModelAliasForCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"kimi": {
			{Name: "deepseek-v3.1", Alias: "pool-model"},
		},
	})

	routeModel := "pool-model"
	upstreamModel := "deepseek-v3.1"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "kimi",
		ModelStates: map[string]*ModelState{
			upstreamModel: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, _, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 429, Message: "quota"}, 0, []string{"kimi"}, routeModel, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true, got false (wait=%v)", wait)
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}
}

type credentialRetryLimitExecutor struct {
	id string

	mu    sync.Mutex
	calls int
}

func (e *credentialRetryLimitExecutor) Identifier() string {
	return e.id
}

func (e *credentialRetryLimitExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall()
	return nil, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *credentialRetryLimitExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *credentialRetryLimitExecutor) recordCall() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
}

func (e *credentialRetryLimitExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type authFallbackExecutor struct {
	id string

	mu                       sync.Mutex
	executeCalls             []string
	streamCalls              []string
	refreshCalls             []string
	executeErrors            map[string]error
	streamFirstErrors        map[string]error
	streamAfterPayloadErrors map[string]error
}

func (e *authFallbackExecutor) Identifier() string {
	return e.id
}

func (e *authFallbackExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, auth.ID)
	err := e.executeErrors[auth.ID]
	e.mu.Unlock()
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *authFallbackExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, auth.ID)
	firstErr := e.streamFirstErrors[auth.ID]
	afterPayloadErr := e.streamAfterPayloadErrors[auth.ID]
	e.mu.Unlock()

	ch := make(chan cliproxyexecutor.StreamChunk, 2)
	if firstErr != nil {
		ch <- cliproxyexecutor.StreamChunk{Err: firstErr}
		close(ch)
		return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: ch}, nil
	}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	if afterPayloadErr != nil {
		ch <- cliproxyexecutor.StreamChunk{Err: afterPayloadErr}
	}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: ch}, nil
}

func (e *authFallbackExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls = append(e.refreshCalls, auth.ID)
	e.mu.Unlock()
	return auth, nil
}

func (e *authFallbackExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "not implemented"}
}

func (e *authFallbackExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *authFallbackExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeCalls))
	copy(out, e.executeCalls)
	return out
}

func (e *authFallbackExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.streamCalls))
	copy(out, e.streamCalls)
	return out
}

func (e *authFallbackExecutor) RefreshCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.refreshCalls))
	copy(out, e.refreshCalls)
	return out
}

type requestScopedStatusError struct {
	status  int
	message string
}

func (e *requestScopedStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *requestScopedStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *requestScopedStatusError) IsRequestScoped() bool {
	return e != nil
}

type retryAfterStatusError struct {
	status     int
	message    string
	retryAfter time.Duration
}

func (e *retryAfterStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *retryAfterStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *retryAfterStatusError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	d := e.retryAfter
	return &d
}

func newCredentialRetryLimitTestManager(t *testing.T, maxRetryCredentials int) (*Manager, *credentialRetryLimitExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, maxRetryCredentials)

	executor := &credentialRetryLimitExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "claude"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "claude"}

	// Auth selection requires that the global model registry knows each credential supports the model.
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	return m, executor
}

func TestManager_MaxRetryCredentials_LimitsCrossCredentialRetries(t *testing.T) {
	request := cliproxyexecutor.Request{Model: "test-model"}
	testCases := []struct {
		name   string
		invoke func(*Manager) error
	}{
		{
			name: "execute",
			invoke: func(m *Manager) error {
				_, errExecute := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "execute_count",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteCount(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "execute_stream",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			limitedManager, limitedExecutor := newCredentialRetryLimitTestManager(t, 1)
			if errInvoke := tc.invoke(limitedManager); errInvoke == nil {
				t.Fatalf("expected error for limited retry execution")
			}
			if calls := limitedExecutor.Calls(); calls != 1 {
				t.Fatalf("expected 1 call with max-retry-credentials=1, got %d", calls)
			}

			unlimitedManager, unlimitedExecutor := newCredentialRetryLimitTestManager(t, 0)
			if errInvoke := tc.invoke(unlimitedManager); errInvoke == nil {
				t.Fatalf("expected error for unlimited retry execution")
			}
			if calls := unlimitedExecutor.Calls(); calls != 2 {
				t.Fatalf("expected 2 calls with max-retry-credentials=0, got %d", calls)
			}
		})
	}
}

func TestManager_ModelSupportBadRequest_FallsBackAndSuspendsAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    "invalid_request_error: The requested model is not supported.",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "claude-opus-4-6"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "claude"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "claude"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: model}
	for i := 0; i < 2; i++ {
		resp, errExecute := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute %d error = %v, want success", i, errExecute)
		}
		if string(resp.Payload) != goodAuth.ID {
			t.Fatalf("execute %d payload = %q, want %q", i, string(resp.Payload), goodAuth.ID)
		}
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	state := updatedBad.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state for %q", model)
	}
	if !state.Unavailable {
		t.Fatalf("expected bad auth model state to be unavailable")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected bad auth model state cooldown to be set")
	}
}

func TestManagerExecuteStream_ModelSupportBadRequestFallsBackAndSuspendsAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		streamFirstErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    "invalid_request_error: The requested model is not supported.",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "claude-opus-4-6"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "claude"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "claude"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: model}
	for i := 0; i < 2; i++ {
		streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute stream %d error = %v, want success", i, errExecute)
		}
		var payload []byte
		for chunk := range streamResult.Chunks {
			if chunk.Err != nil {
				t.Fatalf("execute stream %d chunk error = %v, want success", i, chunk.Err)
			}
			payload = append(payload, chunk.Payload...)
		}
		if string(payload) != goodAuth.ID {
			t.Fatalf("execute stream %d payload = %q, want %q", i, string(payload), goodAuth.ID)
		}
	}

	got := executor.StreamCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	state := updatedBad.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state for %q", model)
	}
	if !state.Unavailable {
		t.Fatalf("expected bad auth model state to be unavailable")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected bad auth model state cooldown to be set")
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model"
	m.MarkResult(context.Background(), Result{
		AuthID:   "auth-1",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 500, Message: "boom"},
	})

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride_On403(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-403",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-403"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}

	if count := reg.GetModelCount(model); count <= 0 {
		t.Fatalf("expected model count > 0 when disable_cooling=true, got %d", count)
	}
}

func TestManager_Execute_DisableCooling_DoesNotBlackoutAfter403(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-403-exec": &Error{
				HTTPStatus: http.StatusForbidden,
				Message:    "forbidden",
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-403-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-403-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute1 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute1 == nil {
		t.Fatal("expected first execute error")
	}
	if statusCodeFromError(errExecute1) != http.StatusForbidden {
		t.Fatalf("first execute status = %d, want %d", statusCodeFromError(errExecute1), http.StatusForbidden)
	}

	_, errExecute2 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute2 == nil {
		t.Fatal("expected second execute error")
	}
	if statusCodeFromError(errExecute2) != http.StatusForbidden {
		t.Fatalf("second execute status = %d, want %d", statusCodeFromError(errExecute2), http.StatusForbidden)
	}
}

func TestManager_Execute_DisableCooling_DoesNotBlackoutAfter429RetryAfter(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-429-exec": &retryAfterStatusError{
				status:     http.StatusTooManyRequests,
				message:    "quota exhausted",
				retryAfter: 2 * time.Minute,
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-429-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-429-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute1 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute1 == nil {
		t.Fatal("expected first execute error")
	}
	if statusCodeFromError(errExecute1) != http.StatusTooManyRequests {
		t.Fatalf("first execute status = %d, want %d", statusCodeFromError(errExecute1), http.StatusTooManyRequests)
	}

	_, errExecute2 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute2 == nil {
		t.Fatal("expected second execute error")
	}
	if statusCodeFromError(errExecute2) != http.StatusTooManyRequests {
		t.Fatalf("second execute status = %d, want %d", statusCodeFromError(errExecute2), http.StatusTooManyRequests)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 2 {
		t.Fatalf("execute calls = %d, want 2", len(calls))
	}

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestManager_Execute_DisableCooling_RetriesAfter429RetryAfter(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 100*time.Millisecond, 0)

	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-429-retryafter-exec": &retryAfterStatusError{
				status:     http.StatusTooManyRequests,
				message:    "quota exhausted",
				retryAfter: 5 * time.Millisecond,
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-429-retryafter-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-429-retryafter-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected execute error")
	}
	if statusCodeFromError(errExecute) != http.StatusTooManyRequests {
		t.Fatalf("execute status = %d, want %d", statusCodeFromError(errExecute), http.StatusTooManyRequests)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 4 {
		t.Fatalf("execute calls = %d, want 4 (initial + 3 retries)", len(calls))
	}
}

func TestManager_MarkResult_RequestScopedNotFoundDoesNotCooldownAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "openai",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "gpt-4.1"
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusNotFound,
			Message:    requestScopedNotFoundMessage,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if updated.Failed != 0 {
		t.Fatalf("expected request-scoped 404 to keep failed count at 0, got %d", updated.Failed)
	}
	for _, bucket := range updated.RecentRequestsSnapshot(time.Now()) {
		if bucket.Failed != 0 {
			t.Fatalf("expected request-scoped 404 to avoid recent failure accounting, got %#v", bucket)
		}
	}
	if updated.Unavailable {
		t.Fatalf("expected request-scoped 404 to keep auth available")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("expected request-scoped 404 to keep auth cooldown unset, got %v", updated.NextRetryAfter)
	}
	if state := updated.ModelStates[model]; state != nil {
		t.Fatalf("expected request-scoped 404 to avoid model cooldown state, got %#v", state)
	}
}

func TestManager_RequestScopedMessageTooBigStopsCredentialFallback(t *testing.T) {
	testCases := []struct {
		name   string
		invoke func(*Manager, cliproxyexecutor.Request) error
		calls  func(*authFallbackExecutor) []string
	}{
		{
			name: "execute",
			invoke: func(m *Manager, req cliproxyexecutor.Request) error {
				_, err := m.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
				return err
			},
			calls: func(executor *authFallbackExecutor) []string { return executor.ExecuteCalls() },
		},
		{
			name: "execute_stream",
			invoke: func(m *Manager, req cliproxyexecutor.Request) error {
				_, err := m.ExecuteStream(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
				return err
			},
			calls: func(executor *authFallbackExecutor) []string { return executor.StreamCalls() },
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			badAuthID := "aa-" + uuid.NewString()
			goodAuthID := "bb-" + uuid.NewString()
			messageTooBigErr := &requestScopedStatusError{
				status:  http.StatusRequestEntityTooLarge,
				message: `{"error":{"message":"upstream websocket message too big","type":"invalid_request_error","code":"message_too_big"}}`,
			}
			executor := &authFallbackExecutor{
				id:                "codex",
				executeErrors:     map[string]error{badAuthID: messageTooBigErr},
				streamFirstErrors: map[string]error{badAuthID: messageTooBigErr},
			}
			m := NewManager(nil, nil, nil)
			m.RegisterExecutor(executor)

			model := "gpt-5-codex-" + uuid.NewString()
			badAuth := &Auth{ID: badAuthID, Provider: "codex"}
			goodAuth := &Auth{ID: goodAuthID, Provider: "codex"}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(badAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
			reg.RegisterClient(goodAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() {
				reg.UnregisterClient(badAuth.ID)
				reg.UnregisterClient(goodAuth.ID)
			})
			if _, err := m.Register(context.Background(), badAuth); err != nil {
				t.Fatalf("register bad auth: %v", err)
			}
			if _, err := m.Register(context.Background(), goodAuth); err != nil {
				t.Fatalf("register good auth: %v", err)
			}

			err := tc.invoke(m, cliproxyexecutor.Request{Model: model})
			if err == nil {
				t.Fatal("expected request-scoped message-too-big error")
			}
			if got := statusCodeFromError(err); got != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d", got, http.StatusRequestEntityTooLarge)
			}
			if !strings.Contains(err.Error(), `"code":"message_too_big"`) {
				t.Fatalf("error = %v, want message_too_big code", err)
			}
			if got := tc.calls(executor); len(got) != 1 || got[0] != badAuth.ID {
				t.Fatalf("credential calls = %v, want [%s]", got, badAuth.ID)
			}

			if got := executor.RefreshCalls(); len(got) != 0 {
				t.Fatalf("refresh calls = %v, want none", got)
			}

			updatedBad, ok := m.GetByID(badAuth.ID)
			if !ok || updatedBad == nil {
				t.Fatal("expected bad auth to remain registered")
			}
			if updatedBad.Failed != 0 {
				t.Fatalf("request-scoped error failed count = %d, want 0", updatedBad.Failed)
			}
			for _, bucket := range updatedBad.RecentRequestsSnapshot(time.Now()) {
				if bucket.Failed != 0 {
					t.Fatalf("request-scoped error changed recent failure accounting: %#v", bucket)
				}
			}
			if updatedBad.Unavailable || !updatedBad.NextRetryAfter.IsZero() {
				t.Fatalf("request-scoped error changed auth availability: %#v", updatedBad)
			}
			if state := updatedBad.ModelStates[model]; state != nil {
				t.Fatalf("request-scoped error created model state: %#v", state)
			}
		})
	}
}

func TestManager_TypedRequestScopedErrorDoesNotDependOnMessage(t *testing.T) {
	badAuthID := "aa-" + uuid.NewString()
	goodAuthID := "bb-" + uuid.NewString()
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			badAuthID: &requestScopedStatusError{status: http.StatusRequestEntityTooLarge, message: "request payload rejected"},
		},
	}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	model := "gpt-5-codex-" + uuid.NewString()
	badAuth := &Auth{ID: badAuthID, Provider: "codex"}
	goodAuth := &Auth{ID: goodAuthID, Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, badAuth.Provider, []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, goodAuth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})
	if _, err := m.Register(context.Background(), badAuth); err != nil {
		t.Fatalf("register bad auth: %v", err)
	}
	if _, err := m.Register(context.Background(), goodAuth); err != nil {
		t.Fatalf("register good auth: %v", err)
	}

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil || !strings.Contains(err.Error(), "request payload rejected") {
		t.Fatalf("Execute() error = %v, want typed request-scoped error", err)
	}
	if got := executor.ExecuteCalls(); len(got) != 1 || got[0] != badAuth.ID {
		t.Fatalf("credential calls = %v, want [%s]", got, badAuth.ID)
	}
	if got := executor.RefreshCalls(); len(got) != 0 {
		t.Fatalf("refresh calls = %v, want none", got)
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatal("expected bad auth to remain registered")
	}
	if updatedBad.Failed != 0 {
		t.Fatalf("typed request-scoped error failed count = %d, want 0", updatedBad.Failed)
	}
	for _, bucket := range updatedBad.RecentRequestsSnapshot(time.Now()) {
		if bucket.Failed != 0 {
			t.Fatalf("typed request-scoped error changed recent failures: %#v", bucket)
		}
	}
	if updatedBad.Unavailable || !updatedBad.NextRetryAfter.IsZero() {
		t.Fatalf("typed request-scoped error changed auth availability: %#v", updatedBad)
	}
	if state := updatedBad.ModelStates[model]; state != nil {
		t.Fatalf("typed request-scoped error created model state: %#v", state)
	}
}

func TestManager_UntypedMessageTooBigTextRetainsCredentialFallbackAndAccounting(t *testing.T) {
	badAuthID := "aa-" + uuid.NewString()
	goodAuthID := "bb-" + uuid.NewString()
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			badAuthID: &Error{
				HTTPStatus: http.StatusRequestEntityTooLarge,
				Message:    `{"error":{"code":"message_too_big"}}`,
			},
		},
	}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	model := "gpt-5-codex-" + uuid.NewString()
	badAuth := &Auth{ID: badAuthID, Provider: "codex"}
	goodAuth := &Auth{ID: goodAuthID, Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, badAuth.Provider, []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, goodAuth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})
	if _, err := m.Register(context.Background(), badAuth); err != nil {
		t.Fatalf("register bad auth: %v", err)
	}
	if _, err := m.Register(context.Background(), goodAuth); err != nil {
		t.Fatalf("register good auth: %v", err)
	}

	resp, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v, want fallback success", err)
	}
	if string(resp.Payload) != goodAuth.ID {
		t.Fatalf("payload = %q, want %q", resp.Payload, goodAuth.ID)
	}
	if got := executor.ExecuteCalls(); len(got) != 2 || got[0] != badAuth.ID || got[1] != goodAuth.ID {
		t.Fatalf("credential calls = %v, want [%s %s]", got, badAuth.ID, goodAuth.ID)
	}
	if got := executor.RefreshCalls(); len(got) != 0 {
		t.Fatalf("refresh calls = %v, want none", got)
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatal("expected bad auth to remain registered")
	}
	if updatedBad.Failed != 1 {
		t.Fatalf("untyped 413 failed count = %d, want 1", updatedBad.Failed)
	}
	recentFailures := int64(0)
	for _, bucket := range updatedBad.RecentRequestsSnapshot(time.Now()) {
		recentFailures += bucket.Failed
	}
	if recentFailures != 1 {
		t.Fatalf("untyped 413 recent failures = %d, want 1", recentFailures)
	}
}

func TestManager_RequestScopedMessageTooBigAfterStreamDataDoesNotMutateCredential(t *testing.T) {
	authID := "auth-" + uuid.NewString()
	messageTooBigErr := &requestScopedStatusError{
		status:  http.StatusRequestEntityTooLarge,
		message: `{"error":{"message":"upstream websocket message too big","type":"invalid_request_error","code":"message_too_big"}}`,
	}
	executor := &authFallbackExecutor{
		id: "codex",
		streamAfterPayloadErrors: map[string]error{
			authID: messageTooBigErr,
		},
	}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	model := "gpt-5-codex-" + uuid.NewString()
	auth := &Auth{ID: authID, Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	result, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil || statusCodeFromError(streamErr) != http.StatusRequestEntityTooLarge {
		t.Fatalf("stream error = %v, want status %d", streamErr, http.StatusRequestEntityTooLarge)
	}

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	if updated.Failed != 0 {
		t.Fatalf("request-scoped stream error failed count = %d, want 0", updated.Failed)
	}
	for _, bucket := range updated.RecentRequestsSnapshot(time.Now()) {
		if bucket.Failed != 0 {
			t.Fatalf("request-scoped stream error changed recent failures: %#v", bucket)
		}
	}
	if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("request-scoped stream error changed auth availability: %#v", updated)
	}
	if state := updated.ModelStates[model]; state != nil {
		t.Fatalf("request-scoped stream error created model state: %#v", state)
	}
}

func TestManager_NonRequestScopedTransportErrorRetainsCredentialFallback(t *testing.T) {
	badAuthID := "aa-" + uuid.NewString()
	goodAuthID := "bb-" + uuid.NewString()
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			badAuthID: &retryAfterStatusError{status: http.StatusBadGateway, message: "upstream transport failed"},
		},
	}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	model := "gpt-5-codex-" + uuid.NewString()
	badAuth := &Auth{ID: badAuthID, Provider: "codex"}
	goodAuth := &Auth{ID: goodAuthID, Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})
	if _, err := m.Register(context.Background(), badAuth); err != nil {
		t.Fatalf("register bad auth: %v", err)
	}
	if _, err := m.Register(context.Background(), goodAuth); err != nil {
		t.Fatalf("register good auth: %v", err)
	}

	resp, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v, want fallback success", err)
	}
	if string(resp.Payload) != goodAuth.ID {
		t.Fatalf("payload = %q, want %q", resp.Payload, goodAuth.ID)
	}
	if got := executor.ExecuteCalls(); len(got) != 2 || got[0] != badAuth.ID || got[1] != goodAuth.ID {
		t.Fatalf("credential calls = %v, want [%s %s]", got, badAuth.ID, goodAuth.ID)
	}
	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatal("expected failed auth to remain registered")
	}
	if updatedBad.Failed != 1 {
		t.Fatalf("normal transport error failed count = %d, want 1", updatedBad.Failed)
	}
	recentFailures := int64(0)
	for _, bucket := range updatedBad.RecentRequestsSnapshot(time.Now()) {
		recentFailures += bucket.Failed
	}
	if recentFailures != 1 {
		t.Fatalf("normal transport error recent failures = %d, want 1", recentFailures)
	}
}

func TestManager_RequestScopedNotFoundStopsRetryWithoutSuspendingAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "openai",
		executeErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusNotFound,
				Message:    requestScopedNotFoundMessage,
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "gpt-4.1"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "openai"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "openai"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "openai", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "openai", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	_, errExecute := m.Execute(context.Background(), []string{"openai"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected request-scoped not-found error")
	}
	errResult, ok := errExecute.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", errExecute)
	}
	if errResult.HTTPStatus != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", errResult.HTTPStatus, http.StatusNotFound)
	}
	if errResult.Message != requestScopedNotFoundMessage {
		t.Fatalf("message = %q, want %q", errResult.Message, requestScopedNotFoundMessage)
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	if updatedBad.Unavailable {
		t.Fatalf("expected request-scoped 404 to keep bad auth available")
	}
	if !updatedBad.NextRetryAfter.IsZero() {
		t.Fatalf("expected request-scoped 404 to keep bad auth cooldown unset, got %v", updatedBad.NextRetryAfter)
	}
	if state := updatedBad.ModelStates[model]; state != nil {
		t.Fatalf("expected request-scoped 404 to avoid bad auth model cooldown state, got %#v", state)
	}
}

func TestManager_InvalidGrantUsesThirtyMinuteSuspension(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	testCases := []struct {
		name    string
		status  int
		message string
	}{
		{
			name:    "structured 400",
			status:  http.StatusBadRequest,
			message: `{"error":"invalid_grant","error_description":"Bad Request"}`,
		},
		{
			name:    "textual 400",
			status:  http.StatusBadRequest,
			message: "oauth token exchange failed: invalid_grant",
		},
		{
			name:    "structured 401",
			status:  http.StatusUnauthorized,
			message: `{"error":{"code":"invalid_grant"}}`,
		},
		{
			name:    "textual 401",
			status:  http.StatusUnauthorized,
			message: "oauth refresh rejected with invalid_grant",
		},
		{
			name:    "structured narrative message 400",
			status:  http.StatusBadRequest,
			message: `{"type":"invalid_request_error","message":"oauth exchange failed: invalid_grant."}`,
		},
		{
			name:    "structured narrative description 401",
			status:  http.StatusUnauthorized,
			message: `{"error":{"description":"refresh rejected (invalid_grant)"}}`,
		},
		{
			name:    "structured narrative error description 400",
			status:  http.StatusBadRequest,
			message: `{"type":"invalid_request_error","error_description":"prefix-invalid_grant-suffix"}`,
		},
		{
			name:    "textual punctuation boundaries 401",
			status:  http.StatusUnauthorized,
			message: "oauth refresh rejected: prefix-invalid_grant-suffix",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			suffix := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
			model := "invalid-grant-model-" + suffix
			authID := "invalid-grant-auth-" + suffix
			resultErr := &Error{HTTPStatus: testCase.status, Message: testCase.message}
			if !isInvalidGrantResultError(resultErr) {
				t.Fatalf("expected invalid_grant classification for %#v", resultErr)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })

			manager := NewManager(nil, nil, nil)
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "antigravity"}); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}
			manager.MarkResult(context.Background(), Result{
				AuthID:   authID,
				Provider: "antigravity",
				Model:    model,
				Success:  false,
				Error:    resultErr,
			})

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatal("expected auth to remain registered")
			}
			state := updated.ModelStates[model]
			if state == nil || !state.Unavailable {
				t.Fatalf("model state = %#v, want unavailable", state)
			}
			remaining := time.Until(state.NextRetryAfter)
			if remaining < 29*time.Minute || remaining > 30*time.Minute {
				t.Fatalf("invalid_grant cooldown = %v, want about 30m", remaining)
			}
			if count := reg.GetModelCount(model); count != 0 {
				t.Fatalf("available model count = %d, want 0", count)
			}
		})
	}
}

func TestManager_InvalidGrantUnrelatedBadRequestRetainsOrdinaryBehavior(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	testCases := []struct {
		name    string
		message string
	}{
		{
			name:    "unrelated invalid request",
			message: `{"type":"invalid_request_error","error":{"code":"invalid_request"}}`,
		},
		{
			name:    "structured invalid_grant_type",
			message: `{"type":"invalid_request_error","error":{"code":"invalid_grant_type"}}`,
		},
		{
			name:    "structured invalid_granted",
			message: `{"type":"invalid_request_error","error":"invalid_granted"}`,
		},
		{
			name:    "invalid request message with longer identifiers",
			message: `{"type":"invalid_request_error","message":"received invalid_grant_type after invalid_granted response"}`,
		},
		{
			name:    "machine error prefixed invalid_grant",
			message: `{"type":"invalid_request_error","error":"prefix-invalid_grant"}`,
		},
		{
			name:    "machine code suffixed invalid_grant",
			message: `{"type":"invalid_request_error","error":{"code":"invalid_grant-suffix"}}`,
		},
		{
			name:    "nested punctuation delimited machine identifier",
			message: `{"type":"invalid_request_error","error":{"details":[{"code":"prefix.invalid_grant.suffix"}]}}`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			resultErr := &Error{Code: "invalid_request", HTTPStatus: http.StatusBadRequest, Message: testCase.message}
			if isInvalidGrantResultError(resultErr) || isInvalidGrantError(resultErr) {
				t.Fatalf("longer or unrelated identifier classified as invalid_grant: %s", testCase.message)
			}
			if !isRequestInvalidError(resultErr) {
				t.Fatal("unrelated invalid_request_error 400 should retain request-invalid behavior")
			}

			suffix := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
			model := "unrelated-bad-request-model-" + suffix
			authID := "unrelated-bad-request-auth-" + suffix
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })

			manager := NewManager(nil, nil, nil)
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "antigravity"}); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}
			manager.MarkResult(context.Background(), Result{
				AuthID:   authID,
				Provider: "antigravity",
				Model:    model,
				Success:  false,
				Error:    resultErr,
			})

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatal("expected auth to remain registered")
			}
			state := updated.ModelStates[model]
			if state == nil {
				t.Fatal("ordinary 400 should retain normal error state")
			}
			if !state.NextRetryAfter.IsZero() {
				t.Fatalf("ordinary 400 unexpectedly set model cooldown: %#v", state)
			}
			if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
				t.Fatalf("ordinary 400 unexpectedly changed auth availability: %#v", updated)
			}
			if count := reg.GetModelCount(model); count <= 0 {
				t.Fatalf("available model count = %d, want positive", count)
			}
		})
	}
}

func TestManager_InvalidGrantLongerIdentifiersAtUnauthorizedStatusAreNotInvalidGrant(t *testing.T) {
	testCases := []struct {
		name    string
		message string
	}{
		{name: "machine code invalid_grant_type", message: `{"error":{"code":"invalid_grant_type"}}`},
		{name: "machine error invalid_granted", message: `{"error":"invalid_granted"}`},
		{name: "machine error prefixed invalid_grant", message: `{"error":"prefix-invalid_grant"}`},
		{name: "machine type suffixed invalid_grant", message: `{"error":{"type":"invalid_grant-suffix"}}`},
		{name: "nested punctuation delimited machine identifier", message: `{"error":{"details":[{"code":"prefix.invalid_grant.suffix"}]}}`},
		{name: "text invalid_grant_type", message: "oauth refresh failed: invalid_grant_type"},
		{name: "text invalid_granted", message: "oauth refresh failed: invalid_granted"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := &Error{Code: "invalid_request", HTTPStatus: http.StatusUnauthorized, Message: testCase.message}
			if isInvalidGrantError(err) || isInvalidGrantResultError(err) {
				t.Fatalf("HTTP 401 longer identifier classified as invalid_grant: %s", testCase.message)
			}
		})
	}
}

func TestManager_InvalidGrantLongerIdentifiersStopCredentialFallback(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	messages := []string{
		`{"type":"invalid_request_error","error":{"code":"invalid_grant_type"}}`,
		`{"type":"invalid_request_error","message":"oauth returned invalid_granted"}`,
		`{"type":"invalid_request_error","error":"prefix-invalid_grant"}`,
		`{"type":"invalid_request_error","error":{"code":"invalid_grant-suffix"}}`,
		`{"type":"invalid_request_error","error":{"details":[{"type":"prefix.invalid_grant.suffix"}]}}`,
	}
	for index, message := range messages {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			suffix := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
			model := "invalid-grant-negative-fallback-model-" + suffix
			badAuthID := "aa-invalid-grant-negative-" + suffix
			goodAuthID := "bb-invalid-grant-negative-" + suffix
			requestErr := &Error{Code: "invalid_request", HTTPStatus: http.StatusBadRequest, Message: message}

			executor := &authFallbackExecutor{
				id:            "antigravity",
				executeErrors: map[string]error{badAuthID: requestErr},
			}
			manager := NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(badAuthID, "antigravity", []*registry.ModelInfo{{ID: model}})
			reg.RegisterClient(goodAuthID, "antigravity", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() {
				reg.UnregisterClient(badAuthID)
				reg.UnregisterClient(goodAuthID)
			})
			if _, err := manager.Register(context.Background(), &Auth{ID: badAuthID, Provider: "antigravity"}); err != nil {
				t.Fatalf("register bad auth: %v", err)
			}
			if _, err := manager.Register(context.Background(), &Auth{ID: goodAuthID, Provider: "antigravity"}); err != nil {
				t.Fatalf("register good auth: %v", err)
			}

			_, errExecute := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if errExecute == nil {
				t.Fatal("expected ordinary request-invalid error")
			}
			if calls := executor.ExecuteCalls(); len(calls) != 1 || calls[0] != badAuthID {
				t.Fatalf("credential calls = %v, want [%s]", calls, badAuthID)
			}

			updated, ok := manager.GetByID(badAuthID)
			if !ok || updated == nil {
				t.Fatal("expected bad auth to remain registered")
			}
			state := updated.ModelStates[model]
			if state == nil || !state.NextRetryAfter.IsZero() {
				t.Fatalf("longer identifier unexpectedly entered 30m suspension: %#v", state)
			}
			if count := reg.GetModelCount(model); count != 2 {
				t.Fatalf("available model count = %d, want 2", count)
			}
		})
	}
}

func TestManager_InvalidGrantBadRequestIsNotRequestInvalid(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"type":"invalid_request_error","error":{"error":"invalid_grant"}}`,
	}
	if isRequestInvalidError(err) {
		t.Fatal("invalid_grant 400 should allow credential fallback")
	}
}

func TestManager_Execute_DevinMCPConfigurationIssue403_DoesNotRetryOrCooldown(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)

	mcpErr := &Error{
		HTTPStatus: http.StatusForbidden,
		Message:    "devin upstream error (permission_denied): Unable to process request due to an MCP configuration issue.",
	}
	executor := &authFallbackExecutor{
		id: "devin",
		executeErrors: map[string]error{
			"auth-devin-1": mcpErr,
			"auth-devin-2": mcpErr,
		},
		streamFirstErrors: map[string]error{
			"auth-devin-1": mcpErr,
			"auth-devin-2": mcpErr,
		},
	}
	m.RegisterExecutor(executor)

	model := "devin/claude-opus-5-5"
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{"auth-devin-1", "auth-devin-2"} {
		auth := &Auth{
			ID:       id,
			Provider: "devin",
			Status:   StatusActive,
		}
		if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
		reg.RegisterClient(id, "devin", []*registry.ModelInfo{{ID: model}})
		cleanupID := id
		t.Cleanup(func() { reg.UnregisterClient(cleanupID) })
	}

	_, errExec := m.Execute(context.Background(), []string{"devin"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExec == nil {
		t.Fatal("expected Execute error")
	}
	if statusCodeFromError(errExec) != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", statusCodeFromError(errExec), http.StatusForbidden)
	}
	if gotCalls := len(executor.executeCalls); gotCalls != 1 {
		t.Fatalf("upstream execute attempts = %d (%v), want 1 (must not retry deterministic request fault)", gotCalls, executor.executeCalls)
	}

	_, errStream := m.ExecuteStream(context.Background(), []string{"devin"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errStream == nil {
		t.Fatal("expected ExecuteStream error")
	}
	if statusCodeFromError(errStream) != http.StatusForbidden {
		t.Fatalf("stream status = %d, want %d", statusCodeFromError(errStream), http.StatusForbidden)
	}
	if gotStreamCalls := len(executor.streamCalls); gotStreamCalls != 1 {
		t.Fatalf("upstream stream attempts = %d (%v), want 1 (must not retry deterministic request fault)", gotStreamCalls, executor.streamCalls)
	}
	for _, id := range []string{"auth-devin-1", "auth-devin-2"} {
		updated, ok := m.GetByID(id)
		if !ok || updated == nil {
			t.Fatalf("expected %s to exist", id)
		}
		if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
			t.Fatalf("%s unexpectedly cooled down at auth level: unavailable=%v nextRetryAfter=%v", id, updated.Unavailable, updated.NextRetryAfter)
		}
		if state := updated.ModelStates[model]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
			t.Fatalf("%s unexpectedly cooled down at model level: %#v", id, state)
		}
	}
}
