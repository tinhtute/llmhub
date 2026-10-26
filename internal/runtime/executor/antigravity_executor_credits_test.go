package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

func resetAntigravityCreditsRetryState() {
	// Finish optional background credits work before clearing the maps so a task
	// cannot publish into the next test's transport.
	antigravityCreditsHintRefreshByID.Range(func(_, value any) bool {
		if state, ok := value.(*antigravityCreditsHintRefreshState); ok && state != nil {
			state.mu.Lock()
			task := state.task
			state.mu.Unlock()
			if task != nil {
				select {
				case <-task.done:
				case <-time.After(5 * time.Second):
				}
			}
		}
		return true
	})
	// Clear the maps in place instead of reassigning the package-level variables:
	// a background credits-refresh goroutine may still hold a reference to the old
	// map when this runs as t.Cleanup, and reassigning races with its access.
	for _, m := range []*sync.Map{
		&antigravityCreditsFailureByAuth,
		&antigravityShortCooldownByAuth,
		&antigravityCreditsBalanceByAuth,
		&antigravityCreditsHintRefreshByID,
	} {
		m.Range(func(key, _ any) bool {
			m.Delete(key)
			return true
		})
	}
}

func TestClassifyAntigravity429(t *testing.T) {
	t.Run("quota exhausted", func(t *testing.T) {
		body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`)
		if got := classifyAntigravity429(body); got != antigravity429QuotaExhausted {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429QuotaExhausted)
		}
	})

	t.Run("standard antigravity rate limit with ui message stays rate limited", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"code": 429,
				"message": "You have exhausted your capacity on this model. Your quota will reset after 0s.",
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{
						"@type": "type.googleapis.com/google.rpc.ErrorInfo",
						"reason": "RATE_LIMIT_EXCEEDED",
						"domain": "cloudcode-pa.googleapis.com",
						"metadata": {
							"model": "claude-opus-4-6-thinking",
							"quotaResetDelay": "479.417207ms",
							"quotaResetTimeStamp": "2026-04-20T09:19:49Z",
							"uiMessage": "true"
						}
					},
					{
						"@type": "type.googleapis.com/google.rpc.RetryInfo",
						"retryDelay": "0.479417207s"
					}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429RateLimited {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429RateLimited)
		}
		decision := decideAntigravity429(body)
		if decision.kind != antigravity429DecisionInstantRetrySameAuth {
			t.Fatalf("decideAntigravity429().kind = %q, want %q", decision.kind, antigravity429DecisionInstantRetrySameAuth)
		}
		if decision.retryAfter == nil {
			t.Fatal("decideAntigravity429().retryAfter = nil")
		}
	})

	t.Run("structured rate limit", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED"},
					{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "0.5s"}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429RateLimited {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429RateLimited)
		}
	})

	t.Run("structured quota exhausted", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED"}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429QuotaExhausted {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429QuotaExhausted)
		}
	})

	t.Run("unstructured 429 defaults to soft rate limit", func(t *testing.T) {
		body := []byte(`{"error":{"message":"too many requests"}}`)
		if got := classifyAntigravity429(body); got != antigravity429SoftRateLimit {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429SoftRateLimit)
		}
	})

	// An exhausted AI-credits balance is a full quota decision, not a soft rate
	// limit: retrying the same request cannot succeed until the balance is topped up.
	t.Run("insufficient G1 credits balance is full quota exhaustion", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"code": 429,
				"message": "Insufficient Google One AI credits balance.",
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "INSUFFICIENT_G1_CREDITS_BALANCE"}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429QuotaExhausted {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429QuotaExhausted)
		}
		if got := decideAntigravity429(body).kind; got != antigravity429DecisionFullQuotaExhausted {
			t.Fatalf("decideAntigravity429().kind = %q, want %q", got, antigravity429DecisionFullQuotaExhausted)
		}
	})
}

func TestAntigravityShouldRetryNoCapacity_Standard503(t *testing.T) {
	body := []byte(`{
		"error": {
			"code": 503,
			"message": "No capacity available for model gemini-3.1-flash-image on the server",
			"status": "UNAVAILABLE",
			"details": [
				{
					"@type": "type.googleapis.com/google.rpc.ErrorInfo",
					"reason": "MODEL_CAPACITY_EXHAUSTED",
					"domain": "cloudcode-pa.googleapis.com",
					"metadata": {
						"model": "gemini-3.1-flash-image"
					}
				}
			]
		}
	}`)
	if !antigravityShouldRetryNoCapacity(http.StatusServiceUnavailable, body) {
		t.Fatal("antigravityShouldRetryNoCapacity() = false, want true")
	}
}

func TestInjectEnabledCreditTypes(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","request":{}}`)
	got := injectEnabledCreditTypes(body)
	if got == nil {
		t.Fatal("injectEnabledCreditTypes() returned nil")
	}
	if !strings.Contains(string(got), `"enabledCreditTypes":["GOOGLE_ONE_AI"]`) {
		t.Fatalf("injectEnabledCreditTypes() = %s, want enabledCreditTypes", string(got))
	}

	if got := injectEnabledCreditTypes([]byte(`not json`)); got != nil {
		t.Fatalf("injectEnabledCreditTypes() for invalid json = %s, want nil", string(got))
	}
}

func TestParseRetryDelay_HumanReadableDuration(t *testing.T) {
	body := []byte(`{"error":{"message":"You have exhausted your capacity on this model. Your quota will reset after 1h43m56s."}}`)
	retryAfter, err := parseRetryDelay(body)
	if err != nil {
		t.Fatalf("parseRetryDelay() error = %v", err)
	}
	if retryAfter == nil {
		t.Fatal("parseRetryDelay() returned nil")
	}
	want := time.Hour + 43*time.Minute + 56*time.Second
	if *retryAfter != want {
		t.Fatalf("parseRetryDelay() = %v, want %v", *retryAfter, want)
	}
}

func TestAntigravityExecute_RetriesTransient429ResourceExhausted(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`))
		case 2:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`))
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := &cliproxyauth.Auth{
		ID: "auth-transient-429",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatAntigravity,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("Execute() returned empty payload")
	}
	if requestCount != 2 {
		t.Fatalf("request count = %d, want 2", requestCount)
	}
}

func TestAntigravityExecute_CreditsInjectedWhenConductorRequests(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
			return
		}
		requestBodies = append(requestBodies, string(body))

		if !strings.Contains(string(body), `"enabledCreditTypes":["GOOGLE_ONE_AI"]`) {
			t.Fatalf("request body missing enabledCreditTypes: %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{
		ID: "auth-credits-conductor",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	// Simulate conductor setting credits requested flag in context
	ctx := cliproxyauth.WithAntigravityCredits(context.Background())

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatAntigravity,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("Execute() returned empty payload")
	}
	if len(requestBodies) != 1 {
		t.Fatalf("request count = %d, want 1", len(requestBodies))
	}
}

func TestAntigravityExecute_NoCreditsWithoutConductorFlag(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
			return
		}
		requestBodies = append(requestBodies, string(body))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{
		ID: "auth-no-conductor-flag",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	// No conductor credits flag set in context
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatAntigravity,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want 429")
	}
	if len(requestBodies) != 1 {
		t.Fatalf("request count = %d, want 1", len(requestBodies))
	}
	// Should NOT contain credits since conductor didn't request them
	if strings.Contains(requestBodies[0], `"enabledCreditTypes"`) {
		t.Fatalf("request should not contain enabledCreditTypes without conductor flag: %s", requestBodies[0])
	}
}

func TestAntigravityAuthHasCredits(t *testing.T) {
	t.Run("sufficient balance", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		auth := &cliproxyauth.Auth{ID: "test-sufficient"}
		antigravityCreditsBalanceByAuth.Store("test-sufficient", antigravityCreditsBalance{
			CreditAmount:    25000,
			MinCreditAmount: 50,
			Known:           true,
		})
		if !antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits() = false, want true")
		}
	})

	t.Run("insufficient balance", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		auth := &cliproxyauth.Auth{ID: "test-insufficient"}
		antigravityCreditsBalanceByAuth.Store("test-insufficient", antigravityCreditsBalance{
			CreditAmount:    30,
			MinCreditAmount: 50,
			Known:           true,
		})
		if antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits() = true, want false")
		}
	})

	t.Run("no balance stored returns true (optimistic)", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		auth := &cliproxyauth.Auth{ID: "test-no-balance"}
		if !antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits() = false with no balance stored, want true (optimistic default)")
		}
	})

	t.Run("nil auth returns false", func(t *testing.T) {
		if antigravityAuthHasCredits(nil) {
			t.Fatal("antigravityAuthHasCredits(nil) = true, want false")
		}
	})

	t.Run("empty ID returns false", func(t *testing.T) {
		auth := &cliproxyauth.Auth{}
		if antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits(empty ID) = true, want false")
		}
	})

	t.Run("unknown balance returns false", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		auth := &cliproxyauth.Auth{ID: "test-unknown"}
		antigravityCreditsBalanceByAuth.Store("test-unknown", antigravityCreditsBalance{
			Known: false,
		})
		if antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits() = true for unknown balance, want false")
		}
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestEnsureAccessToken_WarmTokenLoadsCreditsHint(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	exec := NewAntigravityExecutor(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{
		ID: "auth-warm-token-credits",
		Metadata: map[string]any{
			"access_token": "token",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected request url %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`)),
		}, nil
	}))

	token, updatedAuth, err := exec.ensureAccessToken(ctx, auth)
	if err != nil {
		t.Fatalf("ensureAccessToken() error = %v", err)
	}
	if token != "token" {
		t.Fatalf("ensureAccessToken() token = %q, want %q", token, "token")
	}
	if updatedAuth != nil {
		t.Fatalf("ensureAccessToken() updatedAuth = %v, want nil", updatedAuth)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !cliproxyauth.HasKnownAntigravityCreditsHint(auth.ID) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cliproxyauth.HasKnownAntigravityCreditsHint(auth.ID) {
		t.Fatal("expected credits hint to be populated for warm token auth")
	}
	hint, ok := cliproxyauth.GetAntigravityCreditsHint(auth.ID)
	if !ok {
		t.Fatal("expected credits hint lookup to succeed")
	}
	if !hint.Available {
		t.Fatalf("hint.Available = %v, want true", hint.Available)
	}
	if hint.CreditAmount != 25000 || hint.MinCreditAmount != 50 {
		t.Fatalf("hint amounts = (%v, %v), want (25000, 50)", hint.CreditAmount, hint.MinCreditAmount)
	}
}

func TestUpdateAntigravityCreditsBalance_LoadCodeAssistUserAgent(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	exec := NewAntigravityExecutor(&config.Config{})
	const configuredUserAgent = "antigravity/1.23.2 windows/amd64 google-api-nodejs-client/10.3.0"
	const loadCodeAssistUserAgent = "antigravity/1.23.2 windows/amd64"
	auth := &cliproxyauth.Auth{
		ID:         "auth-load-code-assist-ua",
		Attributes: map[string]string{"user_agent": configuredUserAgent},
	}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected request url %s", req.URL.String())
		}
		if got := req.Header.Get("User-Agent"); got != loadCodeAssistUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, loadCodeAssistUserAgent)
		}
		if got := req.Header.Get("X-Goog-Api-Client"); got != "" {
			t.Fatalf("X-Goog-Api-Client = %q, want empty", got)
		}
		body, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if string(body) != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
			t.Fatalf("loadCodeAssist body = %s", string(body))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`)),
		}, nil
	}))

	exec.updateAntigravityCreditsBalance(ctx, auth, "token")
}

func TestParseMetaFloat(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		wantVal float64
		wantOK  bool
	}{
		{"string", "25000", 25000, true},
		{"float64", float64(100), 100, true},
		{"int", int(50), 50, true},
		{"int64", int64(75), 75, true},
		{"empty string", "", 0, false},
		{"invalid string", "abc", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := map[string]any{"key": tt.value}
			got, ok := parseMetaFloat(meta, "key")
			if ok != tt.wantOK {
				t.Fatalf("parseMetaFloat() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.wantVal {
				t.Fatalf("parseMetaFloat() = %f, want %f", got, tt.wantVal)
			}
		})
	}
}

// waitForAntigravityCreditsHint polls the hint an account exposes until cond
// holds. The refresh itself is asynchronous, so the test waits on the value a
// caller reads, not on the task bookkeeping behind it.
func waitForAntigravityCreditsHint(t *testing.T, authID string, cond func(cliproxyauth.AntigravityCreditsHint) bool) cliproxyauth.AntigravityCreditsHint {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hint, _ := cliproxyauth.GetAntigravityCreditsHint(authID)
		if cond(hint) {
			return hint
		}
		if time.Now().After(deadline) {
			t.Fatalf("credits hint for %q did not converge in time: %#v", authID, hint)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An expired negative hint must be re-validated against loadCodeAssist and, when
// the account has credits again, clear the permanent-failure state. A fresh hint
// must not spend a request.
func TestMaybeRefreshAntigravityCreditsHint_RefreshesExpiredHintAndRecoversCredits(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	exec := NewAntigravityExecutor(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{
		ID: fmt.Sprintf("auth-expired-credits-hint-%d", time.Now().UnixNano()),
		Metadata: map[string]any{
			"access_token": "token",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}
	t.Cleanup(func() {
		cliproxyauth.SetAntigravityCreditsHint(auth.ID, cliproxyauth.AntigravityCreditsHint{})
	})

	markAntigravityCreditsPermanentlyDisabled(auth)
	if _, failed := antigravityCreditsFailureByAuth.Load(auth.ID); !failed {
		t.Fatal("expected auth to be marked permanently disabled for credits")
	}

	var loadCodeAssistCalls int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		loadCodeAssistCalls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"2516","minimumCreditAmountForUsage":"50"}]}}`)),
		}, nil
	}))

	// A fresh negative hint is authoritative for this window.
	exec.maybeRefreshAntigravityCreditsHint(ctx, auth, "token")
	if loadCodeAssistCalls != 0 {
		t.Fatalf("loadCodeAssist calls for fresh hint = %d, want 0", loadCodeAssistCalls)
	}

	cliproxyauth.SetAntigravityCreditsHint(auth.ID, cliproxyauth.AntigravityCreditsHint{
		Known:           true,
		Available:       false,
		CreditAmount:    0,
		MinCreditAmount: 1,
		UpdatedAt:       time.Now().Add(-antigravityCreditsHintRefreshInterval - time.Second),
	})
	exec.maybeRefreshAntigravityCreditsHint(ctx, auth, "token")

	hint := waitForAntigravityCreditsHint(t, auth.ID, func(h cliproxyauth.AntigravityCreditsHint) bool {
		return h.Known && h.Available
	})
	if loadCodeAssistCalls != 1 {
		t.Fatalf("loadCodeAssist calls for expired hint = %d, want 1", loadCodeAssistCalls)
	}
	if hint.CreditAmount != 2516 || hint.MinCreditAmount != 50 {
		t.Fatalf("hint amounts = (%v, %v), want (2516, 50)", hint.CreditAmount, hint.MinCreditAmount)
	}
	if _, failed := antigravityCreditsFailureByAuth.Load(auth.ID); failed {
		t.Fatal("expected permanently disabled credits state to be cleared after recovery")
	}
}

// Production path: a topped-up account must recover through the conductor's
// credits fallback while the model is still in quota cooldown, and the cached
// negative hint must be refreshed from loadCodeAssist rather than staying stale.
func TestAntigravityManagerExecuteAndStream_ToppedUpCreditsRecoverDuringQuotaCooldown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		mode := "execute"
		if stream {
			mode = "execute_stream"
		}
		t.Run(mode, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)

			const model = "claude-sonnet-4-6"
			var toppedUp atomic.Bool
			var loadCodeAssistCalls atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()

				if r.URL.Path == "/v1internal:loadCodeAssist" {
					loadCodeAssistCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if toppedUp.Load() {
						_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"2516","minimumCreditAmountForUsage":"50"}]}}`))
					} else {
						_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"0","minimumCreditAmountForUsage":"50"}]}}`))
					}
					return
				}

				if !strings.Contains(string(body), `"enabledCreditTypes":["GOOGLE_ONE_AI"]`) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{
						"error": {
							"code": 429,
							"message": "You have exhausted your capacity on this model. Your quota will reset after 1h0m0s.",
							"status": "RESOURCE_EXHAUSTED",
							"details": [
								{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED"},
								{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "3600s"}
							]
						}
					}`))
					return
				}

				if !toppedUp.Load() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{
						"error": {
							"code": 429,
							"message": "Insufficient Google One AI credits balance.",
							"status": "RESOURCE_EXHAUSTED",
							"details": [
								{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "INSUFFICIENT_G1_CREDITS_BALANCE"}
							]
						}
					}`))
					return
				}

				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"recovered\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}}\n\n"))
			}))
			defer server.Close()

			cfg := &config.Config{
				QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
			}
			exec := NewAntigravityExecutor(cfg)
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(exec)

			authID := fmt.Sprintf("ag-topup-recovery-%s-%d", mode, time.Now().UnixNano())
			registry.GetGlobalRegistry().RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() {
				registry.GetGlobalRegistry().UnregisterClient(authID)
				cliproxyauth.SetAntigravityCreditsHint(authID, cliproxyauth.AntigravityCreditsHint{})
			})

			if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
				ID:       authID,
				Provider: "antigravity",
				Attributes: map[string]string{
					"base_url": server.URL,
				},
				Metadata: map[string]any{
					"access_token": "token",
					"project_id":   "project-1",
					"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				},
			}); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			req := cliproxyexecutor.Request{
				Model:   model,
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
			}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatAntigravity,
				ResponseFormat: sdktranslator.FormatAntigravity,
			}

			// Step 1: weekly quota and the credits balance are both exhausted.
			cliproxyauth.SetAntigravityCreditsHint(authID, cliproxyauth.AntigravityCreditsHint{
				Known:           true,
				Available:       true,
				CreditAmount:    100,
				MinCreditAmount: 50,
				UpdatedAt:       time.Now(),
			})
			if stream {
				if _, errStream := manager.ExecuteStream(context.Background(), []string{"antigravity"}, req, opts); errStream == nil {
					t.Fatal("initial ExecuteStream() succeeded, want 429 after quota and credits exhaustion")
				}
			} else {
				if _, errExec := manager.Execute(context.Background(), []string{"antigravity"}, req, opts); errExec == nil {
					t.Fatal("initial Execute() succeeded, want 429 after quota and credits exhaustion")
				}
			}
			if _, failed := antigravityCreditsFailureByAuth.Load(authID); !failed {
				t.Fatal("expected auth to be marked permanently disabled after INSUFFICIENT_G1_CREDITS_BALANCE")
			}
			exhausted, ok := cliproxyauth.GetAntigravityCreditsHint(authID)
			if !ok || !exhausted.Known || exhausted.Available {
				t.Fatalf("hint after exhaustion = %#v (ok=%v), want Known=true Available=false", exhausted, ok)
			}

			// Step 2: top up and let the cached negative hint expire.
			toppedUp.Store(true)
			callsBeforeTopUp := loadCodeAssistCalls.Load()
			cliproxyauth.SetAntigravityCreditsHint(authID, cliproxyauth.AntigravityCreditsHint{
				Known:           true,
				Available:       false,
				CreditAmount:    0,
				MinCreditAmount: 1,
				UpdatedAt:       time.Now().Add(-antigravityCreditsHintRefreshInterval - time.Second),
			})

			// Step 3: the request recovers through credits while the cooldown holds.
			if stream {
				streamResult, errStream := manager.ExecuteStream(context.Background(), []string{"antigravity"}, req, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream() after top-up error = %v", errStream)
				}
				var streamed strings.Builder
				for chunk := range streamResult.Chunks {
					if chunk.Err != nil {
						t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
					}
					streamed.Write(chunk.Payload)
				}
				if !strings.Contains(streamed.String(), "recovered") {
					t.Fatalf("ExecuteStream() payload = %s, want recovered", streamed.String())
				}
			} else {
				resp, errExec := manager.Execute(context.Background(), []string{"antigravity"}, req, opts)
				if errExec != nil {
					t.Fatalf("Execute() after top-up error = %v", errExec)
				}
				if !strings.Contains(string(resp.Payload), "recovered") {
					t.Fatalf("Execute() payload = %s, want recovered", string(resp.Payload))
				}
			}

			hint := waitForAntigravityCreditsHint(t, authID, func(h cliproxyauth.AntigravityCreditsHint) bool {
				return h.Known && h.Available && h.CreditAmount == 2516
			})
			if hint.CreditAmount != 2516 {
				t.Fatalf("hint credit amount after recovery = %v, want 2516", hint.CreditAmount)
			}
			if loadCodeAssistCalls.Load() <= callsBeforeTopUp {
				t.Fatalf("loadCodeAssist calls after top-up = %d, want > %d", loadCodeAssistCalls.Load(), callsBeforeTopUp)
			}
			if _, failed := antigravityCreditsFailureByAuth.Load(authID); failed {
				t.Fatal("expected permanently disabled credits state to be cleared after recovery")
			}
		})
	}
}
