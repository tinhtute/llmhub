package redisqueue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/therealtinhtute/llmhub/internal/logging"
	coresession "github.com/therealtinhtute/llmhub/sdk/cliproxy/session"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
)

func TestUsageQueuePluginPayloadIncludesStableFieldsAndSuccess(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)
		responseHeaders := http.Header{}
		responseHeaders.Add("X-Upstream-Request-Id", "upstream-req-1")
		responseHeaders.Add("Retry-After", "30")

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:        "openai",
			ExecutorType:    "KimiExecutor",
			Model:           "gpt-5.4",
			Alias:           "client-gpt",
			APIKey:          "test-key",
			AuthIndex:       "0",
			AuthType:        "apikey",
			Source:          "user@example.com",
			ReasoningEffort: "medium",
			ResponseModel:   "gpt-5.6-luna",
			RequestedAt:     time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:         1500 * time.Millisecond,
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
			ResponseHeaders: responseHeaders.Clone(),
		})
		responseHeaders.Set("Retry-After", "999")

		payload := popSinglePayload(t)
		requireStringField(t, payload, "provider", "openai")
		requireStringField(t, payload, "executor_type", "KimiExecutor")
		requireStringField(t, payload, "model", "gpt-5.4")
		requireStringField(t, payload, "alias", "client-gpt")
		requireStringField(t, payload, "endpoint", "POST /v1/chat/completions")
		requireStringField(t, payload, "auth_type", "apikey")
		requireMissingField(t, payload, "user_api_key")
		requireStringField(t, payload, "request_id", "ctx-request-id")
		requireStringField(t, payload, "reasoning_effort", "medium")
		requireStringField(t, payload, "response_model", "gpt-5.6-luna")
		requireHeaderField(t, payload, "response_headers", "X-Upstream-Request-Id", []string{"upstream-req-1"})
		requireHeaderField(t, payload, "response_headers", "Retry-After", []string{"30"})
		requireBoolField(t, payload, "failed", false)
		requireFailField(t, payload, http.StatusOK, "")
	})
}

func TestUsageQueuePluginAsyncUsesRecordResponseHeaders(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		ctx = internallogging.WithResponseHeadersHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)
		initialHeaders := http.Header{}
		initialHeaders.Set("X-Upstream-Request-Id", "upstream-req-1")
		internallogging.SetResponseHeaders(ctx, initialHeaders)

		mgr := coreusage.NewManager(16)
		defer mgr.Stop()

		mgr.Register(pluginFunc(func(ctx context.Context, _ coreusage.Record) {
			nextHeaders := http.Header{}
			nextHeaders.Set("X-Upstream-Request-Id", "upstream-req-2")
			internallogging.SetResponseHeaders(ctx, nextHeaders)
		}))
		mgr.Register(&usageQueuePlugin{})

		mgr.Publish(ctx, coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.4",
			Alias:       "client-gpt",
			APIKey:      "test-key",
			AuthIndex:   "0",
			AuthType:    "apikey",
			Source:      "user@example.com",
			RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:     1500 * time.Millisecond,
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
			ResponseHeaders: internallogging.GetResponseHeaders(ctx),
		})

		payload := waitForSinglePayload(t, 2*time.Second)
		requireHeaderField(t, payload, "response_headers", "X-Upstream-Request-Id", []string{"upstream-req-1"})
	})
}

func TestUsageQueuePluginPayloadIncludesStableFieldsAndFailureAndGinRequestID(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "gin-request-id")
		ctx = internallogging.WithEndpoint(ctx, "GET /v1/responses")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusInternalServerError)

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.4-mini",
			Alias:       "client-mini",
			APIKey:      "test-key",
			AuthIndex:   "0",
			AuthType:    "apikey",
			Source:      "user@example.com",
			RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:     2500 * time.Millisecond,
			Fail: coreusage.Failure{
				StatusCode: http.StatusInternalServerError,
				Body:       "upstream failed",
			},
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "provider", "openai")
		requireStringField(t, payload, "model", "gpt-5.4-mini")
		requireStringField(t, payload, "alias", "client-mini")
		requireStringField(t, payload, "endpoint", "GET /v1/responses")
		requireStringField(t, payload, "auth_type", "apikey")
		requireMissingField(t, payload, "user_api_key")
		requireStringField(t, payload, "request_id", "gin-request-id")
		requireBoolField(t, payload, "failed", true)
		requireFailField(t, payload, http.StatusInternalServerError, "upstream failed")
	})
}

func TestUsageQueuePluginAsyncIgnoresRecycledGinContext(t *testing.T) {
	withEnabledQueue(t, func() {
		ginCtx := newTestGinContext(t, http.MethodPost, "/v1/chat/completions", http.StatusOK)
		ctx := context.WithValue(context.Background(), "gin", ginCtx)
		ctx = internallogging.WithRequestID(ctx, "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusInternalServerError)

		mgr := coreusage.NewManager(16)
		defer mgr.Stop()

		mgr.Register(pluginFunc(func(_ context.Context, _ coreusage.Record) {
			ginCtx.Request = httptest.NewRequest(http.MethodGet, "http://example.com/v1/responses", nil)
			ginCtx.Status(http.StatusOK)
		}))
		mgr.Register(&usageQueuePlugin{})

		mgr.Publish(ctx, coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.4",
			Alias:       "client-gpt",
			APIKey:      "test-key",
			AuthIndex:   "0",
			AuthType:    "apikey",
			Source:      "user@example.com",
			RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:     1500 * time.Millisecond,
			Fail: coreusage.Failure{
				StatusCode: http.StatusBadGateway,
				Body:       "bad gateway",
			},
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
		})

		payload := waitForSinglePayload(t, 2*time.Second)
		requireStringField(t, payload, "endpoint", "POST /v1/chat/completions")
		requireStringField(t, payload, "alias", "client-gpt")
		requireMissingField(t, payload, "user_api_key")
		requireStringField(t, payload, "request_id", "ctx-request-id")
		requireBoolField(t, payload, "failed", true)
		requireFailField(t, payload, http.StatusBadGateway, "bad gateway")
	})
}

func withEnabledQueue(t *testing.T, fn func()) {
	t.Helper()

	prevQueueEnabled := Enabled()
	prevUsageEnabled := UsageStatisticsEnabled()

	SetEnabled(false)
	SetEnabled(true)
	SetUsageStatisticsEnabled(true)

	defer func() {
		SetEnabled(false)
		SetEnabled(prevQueueEnabled)
		SetUsageStatisticsEnabled(prevUsageEnabled)
	}()

	fn()
}

func newTestGinContext(t *testing.T, method, path string, status int) *gin.Context {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(method, "http://example.com"+path, nil)
	if status != 0 {
		ginCtx.Status(status)
	}
	return ginCtx
}

func popSinglePayload(t *testing.T) map[string]json.RawMessage {
	t.Helper()

	items := PopOldest(10)
	if len(items) != 1 {
		t.Fatalf("PopOldest() items = %d, want 1", len(items))
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(items[0], &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

func waitForSinglePayload(t *testing.T, timeout time.Duration) map[string]json.RawMessage {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		items := PopOldest(10)
		if len(items) == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if len(items) != 1 {
			t.Fatalf("PopOldest() items = %d, want 1", len(items))
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(items[0], &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		return payload
	}
	t.Fatalf("timeout waiting for queued payload")
	return nil
}

func requireStringField(t *testing.T, payload map[string]json.RawMessage, key, want string) {
	t.Helper()

	raw, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q", key)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

func requireMissingField(t *testing.T, payload map[string]json.RawMessage, key string) {
	t.Helper()

	if _, ok := payload[key]; ok {
		t.Fatalf("payload unexpectedly contains %q", key)
	}
}

type pluginFunc func(context.Context, coreusage.Record)

func (fn pluginFunc) HandleUsage(ctx context.Context, record coreusage.Record) {
	fn(ctx, record)
}

func requireBoolField(t *testing.T, payload map[string]json.RawMessage, key string, want bool) {
	t.Helper()

	raw, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q", key)
	}
	var got bool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s = %t, want %t", key, got, want)
	}
}

func requireFailField(t *testing.T, payload map[string]json.RawMessage, wantStatus int, wantBody string) {
	t.Helper()

	raw, ok := payload["fail"]
	if !ok {
		t.Fatalf("payload missing %q", "fail")
	}
	var got struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal fail: %v", err)
	}
	if got.StatusCode != wantStatus || got.Body != wantBody {
		t.Fatalf("fail = {status_code:%d body:%q}, want {status_code:%d body:%q}", got.StatusCode, got.Body, wantStatus, wantBody)
	}
}

func requireHeaderField(t *testing.T, payload map[string]json.RawMessage, field, key string, want []string) {
	t.Helper()

	raw, ok := payload[field]
	if !ok {
		t.Fatalf("payload missing %q", field)
	}
	var headers map[string][]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		t.Fatalf("unmarshal %q: %v", field, err)
	}
	got, ok := headers[key]
	if !ok {
		t.Fatalf("%s missing header %q", field, key)
	}
	if len(got) != len(want) {
		t.Fatalf("%s[%q] = %v, want %v", field, key, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%q] = %v, want %v", field, key, got, want)
		}
	}
}

// Ported from upstream CLIProxyAPI commits 580df36423e4 and 6b187e778ceb:
// queue payloads carry session/parent lineage normalized to canonical UUIDv8.
func TestUsageQueuePluginPayloadIncludesExplicitSessionHierarchy(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-session-req-1")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithClientRequestMetadata(ctx, internallogging.ClientRequestMetadata{
			ClientIP:         "192.0.2.10",
			ResolvedClientIP: "203.0.113.5",
			SessionID:        "slot:pi-worker-1",
			ParentSessionID:  "slot:pi-main-root",
		})
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:        "openai",
			Model:           "gpt-5.6-sol",
			APIKey:          "test-key",
			SessionID:       "slot:pi-worker-1",
			ParentSessionID: "slot:pi-main-root",
			RequestedAt:     time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			Latency:         5000 * time.Millisecond,
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "request_id", "ctx-session-req-1")
		requireStringField(t, payload, "resolved_client_ip", "203.0.113.5")
		wantSession := coresession.NormalizeToCanonicalUUID("slot:pi-worker-1")
		wantParent := coresession.NormalizeToCanonicalUUID("slot:pi-main-root")
		requireStringField(t, payload, "session_id", wantSession)
		requireStringField(t, payload, "parent_session_id", wantParent)
		if len(wantSession) != 36 || wantSession[14] != '8' {
			t.Fatalf("expected 36-char UUIDv8 for session_id, got %q", wantSession)
		}
		if len(wantParent) != 36 || wantParent[14] != '8' {
			t.Fatalf("expected 36-char UUIDv8 for parent_session_id, got %q", wantParent)
		}
	})
}

// Ported from upstream CLIProxyAPI commit 6b187e778ceb: protocol-prefixed
// native UUIDs are unwrapped to the canonical lowercase UUID in queue payloads.
func TestUsageQueuePluginPayloadNormalizesPrefixedNativeUUID(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-native-uuid-1")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:        "codex",
			Model:           "gpt-5.6-sol",
			APIKey:          "test-key",
			SessionID:       "codex:01a07e72-c84d-7fd3-8207-d217b41cc649",
			ParentSessionID: "codex:01a07e71-a1b2-7c3d-98e1-f23456789abc",
			RequestedAt:     time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			Latency:         1000 * time.Millisecond,
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "session_id", "01a07e72-c84d-7fd3-8207-d217b41cc649")
		requireStringField(t, payload, "parent_session_id", "01a07e71-a1b2-7c3d-98e1-f23456789abc")
	})
}

// Ported from upstream CLIProxyAPI commit 580df36423e4: self-referential
// parents are dropped before queueing.
func TestUsageQueuePluginPayloadPreventsSelfReferentialLoop(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-loop-req-1")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:        "openai",
			Model:           "gpt-5.6-sol",
			APIKey:          "test-key",
			SessionID:       "loop-sess-1",
			ParentSessionID: "loop-sess-1",
			RequestedAt:     time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			Latency:         1000 * time.Millisecond,
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "request_id", "ctx-loop-req-1")
		wantSession := coresession.NormalizeToCanonicalUUID("loop-sess-1")
		requireStringField(t, payload, "session_id", wantSession)
		if _, exists := payload["parent_session_id"]; exists {
			t.Fatalf("expected parent_session_id to be omitted on self-referential loop, got %s", payload["parent_session_id"])
		}
	})
}

// Ported from upstream CLIProxyAPI commit 580df36423e4: a record-defined root
// session does not inherit a stale context parent (ghost-parent guard).
func TestUsageQueuePluginPayloadSameOriginFallback(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-same-origin-1")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithClientRequestMetadata(ctx, internallogging.ClientRequestMetadata{
			SessionID:       "old-root",
			ParentSessionID: "old-parent",
		})
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		plugin := &usageQueuePlugin{}
		// Record explicitly defines an independent root session without parent.
		// It should NOT pick up old-parent from context.
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:        "openai",
			Model:           "gpt-5.6-sol",
			APIKey:          "test-key",
			SessionID:       "new-custom-root",
			ParentSessionID: "",
			RequestedAt:     time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			Latency:         1000 * time.Millisecond,
		})

		payload := popSinglePayload(t)
		wantSession := coresession.NormalizeToCanonicalUUID("new-custom-root")
		requireStringField(t, payload, "session_id", wantSession)
		if _, exists := payload["parent_session_id"]; exists {
			t.Fatalf("expected parent_session_id to be omitted when record is independent root, got %s", payload["parent_session_id"])
		}
	})
}

// Context metadata fallback covers records that lack explicit session fields,
// and a context parent that does not match the record session is not adopted.
func TestUsageQueuePluginPayloadContextFallbackAndMismatch(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-fallback-1")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithClientRequestMetadata(ctx, internallogging.ClientRequestMetadata{
			SessionID:       "ctx-session",
			ParentSessionID: "ctx-parent",
		})
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		plugin := &usageQueuePlugin{}
		// Record carries no session fields: context metadata supplies both.
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.6-sol",
			RequestedAt: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			Latency:     1000 * time.Millisecond,
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "session_id", coresession.NormalizeToCanonicalUUID("ctx-session"))
		requireStringField(t, payload, "parent_session_id", coresession.NormalizeToCanonicalUUID("ctx-parent"))
	})

	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-mismatch-1")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithClientRequestMetadata(ctx, internallogging.ClientRequestMetadata{
			SessionID:       "different-session",
			ParentSessionID: "ctx-parent",
		})
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		plugin := &usageQueuePlugin{}
		// Record session differs from context session: context parent must not leak.
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.6-sol",
			SessionID:   "record-session",
			RequestedAt: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			Latency:     1000 * time.Millisecond,
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "session_id", coresession.NormalizeToCanonicalUUID("record-session"))
		if _, exists := payload["parent_session_id"]; exists {
			t.Fatalf("expected parent_session_id to be omitted when context session mismatches record, got %s", payload["parent_session_id"])
		}
	})
}

func TestUsageQueuePlugin_ExecutionIDAndTraceID(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "000000ab")
		execUUID := "12345678-1234-4234-8234-123456789abc"

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			RequestID: execUUID,
			TraceID:   "000000ab",
			Provider:  "openai",
			Model:     "gpt-5.4",
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 5,
				TotalTokens:  15,
			},
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "request_id", "000000ab")
		requireStringField(t, payload, "execution_id", execUUID)
		requireStringField(t, payload, "trace_id", "000000ab")
	})
}

func TestUsageQueuePlugin_StrictLegacyRequestIDPreservation(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "legacy-log-id")
		execUUID := "12345678-1234-4234-8234-123456789abc"

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			RequestID: execUUID,
			TraceID:   "custom-trace-id",
			Provider:  "openai",
			Model:     "gpt-5.4",
			Detail: coreusage.Detail{
				InputTokens: 5,
			},
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "request_id", "legacy-log-id")
		requireStringField(t, payload, "execution_id", execUUID)
		requireStringField(t, payload, "trace_id", "custom-trace-id")
	})
}

// prePortPayload is the run-log payload a record with no service tier produced
// before the tier fields existed. Both keys are omitempty, so such a record must
// keep emitting exactly these bytes.
const prePortPayload = `{"timestamp":"2026-04-25T00:00:00Z","latency_ms":1500,"source":"user@example.com","auth_index":"0","tokens":{"input_tokens":10,"output_tokens":20,"reasoning_tokens":0,"cached_tokens":0,"cache_read_tokens":0,"cache_creation_tokens":0,"total_tokens":30},"failed":false,"fail":{"status_code":200,"body":""},"provider":"openai","executor_type":"KimiExecutor","model":"gpt-5.4","alias":"client-gpt","endpoint":"POST /v1/chat/completions","auth_type":"apikey","api_key":"","request_id":"ctx-request-id","execution_id":"EXECUTION_ID","trace_id":"ctx-request-id","reasoning_effort":""}`

func TestUsageQueuePluginPayloadKeepsByteIdentityWithoutServiceTiers(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{
			Provider:     "openai",
			ExecutorType: "KimiExecutor",
			Model:        "gpt-5.4",
			Alias:        "client-gpt",
			AuthIndex:    "0",
			AuthType:     "apikey",
			Source:       "user@example.com",
			RequestedAt:  time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:      1500 * time.Millisecond,
			Detail:       coreusage.Detail{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		})

		items := PopOldest(10)
		if len(items) != 1 {
			t.Fatalf("PopOldest() items = %d, want 1", len(items))
		}
		got := string(items[0])
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(items[0], &envelope); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if raw, ok := envelope["execution_id"]; ok {
			var id string
			if err := json.Unmarshal(raw, &id); err != nil {
				t.Fatalf("unmarshal execution_id: %v", err)
			}
			got = strings.Replace(got, id, "EXECUTION_ID", 1)
		}
		if got != prePortPayload {
			t.Fatalf("payload for a tier-less record changed:\n got %s\nwant %s", got, prePortPayload)
		}
	})
}

func TestUsageQueuePluginPayloadCarriesServiceTiers(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{
			Provider:             "claude",
			Model:                "claude-opus-5",
			RequestedAt:          time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			EffectiveServiceTier: "priority",
			ResponseServiceTier:  "fast",
			Detail:               coreusage.Detail{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "effective_service_tier", "priority")
		requireStringField(t, payload, "response_service_tier", "fast")
	})
}
