package helps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/therealtinhtute/llmhub/internal/logging"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
)

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail := ParseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail := ParseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestParseOpenAIUsageIgnoresNullUsage(t *testing.T) {
	data := []byte(`{"usage":null}`)
	detail := ParseOpenAIUsage(data)
	if detail != (usage.Detail{}) {
		t.Fatalf("detail = %+v, want zero detail", detail)
	}
}

func TestParseOpenAIStreamUsageIgnoresNullUsage(t *testing.T) {
	line := []byte(`data: {"id":"chunk_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"usage":null}`)
	if detail, ok := ParseOpenAIStreamUsage(line); ok {
		t.Fatalf("ParseOpenAIStreamUsage() = (%+v, true), want false for null usage", detail)
	}
}

func TestParseOpenAIStreamUsageResponsesFields(t *testing.T) {
	line := []byte(`data: {"id":"chunk_1","object":"chat.completion.chunk","choices":[],"usage":{"input_tokens":8,"output_tokens":5,"total_tokens":13,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`)
	detail, ok := ParseOpenAIStreamUsage(line)
	if !ok {
		t.Fatal("ParseOpenAIStreamUsage() ok = false, want true")
	}
	if detail.InputTokens != 8 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 8)
	}
	if detail.OutputTokens != 5 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 5)
	}
	if detail.TotalTokens != 13 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 13)
	}
	if detail.CachedTokens != 3 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 3)
	}
	if detail.ReasoningTokens != 2 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 2)
	}
}

func TestParseGeminiCLIUsage_TopLevelUsageMetadata(t *testing.T) {
	data := []byte(`{"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7,"thoughtsTokenCount":3,"totalTokenCount":21,"cachedContentTokenCount":5}}`)
	detail := ParseGeminiCLIUsage(data)
	if detail.InputTokens != 11 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 11)
	}
	if detail.OutputTokens != 7 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 7)
	}
	if detail.ReasoningTokens != 3 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 3)
	}
	if detail.TotalTokens != 21 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 21)
	}
	if detail.CachedTokens != 5 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 5)
	}
}

func TestParseGeminiCLIStreamUsage_ResponseSnakeCaseUsageMetadata(t *testing.T) {
	line := []byte(`data: {"response":{"usage_metadata":{"promptTokenCount":13,"candidatesTokenCount":2,"totalTokenCount":15}}}`)
	detail, ok := ParseGeminiCLIStreamUsage(line)
	if !ok {
		t.Fatal("ParseGeminiCLIStreamUsage() ok = false, want true")
	}
	if detail.InputTokens != 13 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 13)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 15 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 15)
	}
}

func TestParseGeminiCLIStreamUsage_IgnoresTrafficTypeOnlyUsageMetadata(t *testing.T) {
	line := []byte(`data: {"response":{"usageMetadata":{"trafficType":"ON_DEMAND"}}}`)
	if detail, ok := ParseGeminiCLIStreamUsage(line); ok {
		t.Fatalf("ParseGeminiCLIStreamUsage() = (%+v, true), want false for traffic-only usage metadata", detail)
	}
}

func TestUsageReporterBuildRecordIncludesLatency(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "openai",
		model:       "gpt-5.4",
		requestedAt: time.Now().Add(-1500 * time.Millisecond),
	}

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Latency < time.Second {
		t.Fatalf("latency = %v, want >= 1s", record.Latency)
	}
	if record.Latency > 3*time.Second {
		t.Fatalf("latency = %v, want <= 3s", record.Latency)
	}
}

func TestUsageReporterBuildRecordIncludesRequestedModelAlias(t *testing.T) {
	ctx := usage.WithRequestedModelAlias(context.Background(), "client-gpt")
	reporter := NewUsageReporter(ctx, "openai", "gpt-5.4", nil)

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Model != "gpt-5.4" {
		t.Fatalf("model = %q, want %q", record.Model, "gpt-5.4")
	}
	if record.Alias != "client-gpt" {
		t.Fatalf("alias = %q, want %q", record.Alias, "client-gpt")
	}
}

func TestNewExecutorUsageReporterIncludesExecutorType(t *testing.T) {
	reporter := NewExecutorUsageReporter(context.Background(), &TestUsageExecutor{}, "gpt-5.4", nil)

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Provider != "test-provider" {
		t.Fatalf("provider = %q, want %q", record.Provider, "test-provider")
	}
	if record.ExecutorType != "TestUsageExecutor" {
		t.Fatalf("executor type = %q, want %q", record.ExecutorType, "TestUsageExecutor")
	}
}

func TestUsageReporterBuildRecordIncludesReasoningEffort(t *testing.T) {
	ctx := usage.WithReasoningEffort(context.Background(), "medium")
	reporter := NewUsageReporter(ctx, "openai", "gpt-5.4", nil)

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.ReasoningEffort != "medium" {
		t.Fatalf("reasoning effort = %q, want %q", record.ReasoningEffort, "medium")
	}
}

func TestUsageReporterBuildAdditionalModelRecordSkipsZeroTokens(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "codex",
		model:       "gpt-5.4",
		requestedAt: time.Now(),
	}

	if _, ok := reporter.buildAdditionalModelRecord("gpt-image-2", usage.Detail{}); ok {
		t.Fatalf("expected all-zero token usage to be skipped")
	}
	if _, ok := reporter.buildAdditionalModelRecord("gpt-image-2", usage.Detail{InputTokens: 2}); !ok {
		t.Fatalf("expected non-zero input token usage to be recorded")
	}
	if _, ok := reporter.buildAdditionalModelRecord("gpt-image-2", usage.Detail{CachedTokens: 2}); !ok {
		t.Fatalf("expected non-zero cached token usage to be recorded")
	}
}

func TestUsageReporterTrackHTTPClientStartsTTFTBeforeRoundTrip(t *testing.T) {
	delay := 40 * time.Millisecond
	reporter := NewUsageReporter(context.Background(), "openai", "gpt-5.4", nil)
	client := reporter.TrackHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			time.Sleep(delay)
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
	})

	req, errNewRequest := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.invalid/v1/chat/completions", strings.NewReader("{}"))
	if errNewRequest != nil {
		t.Fatalf("NewRequestWithContext() error = %v", errNewRequest)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatalf("Do() error = %v", errDo)
	}
	if _, errRead := io.ReadAll(resp.Body); errRead != nil {
		t.Fatalf("ReadAll() error = %v", errRead)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close error = %v", errClose)
	}
	if got := reporter.ttftDuration(); got < delay {
		t.Fatalf("ttft = %v, want >= %v", got, delay)
	}
}

func TestUsageReporterTrackHTTPClientRoundTripOnly_DoesNotTriggerOnBodyRead(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "codex", "gpt-5.6-luna", nil)
	client := reporter.TrackHTTPClientRoundTripOnly(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n\n")),
				Request:    req,
			}, nil
		}),
	})

	req, errNewRequest := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.invalid/v1/responses", strings.NewReader("{}"))
	if errNewRequest != nil {
		t.Fatalf("NewRequestWithContext() error = %v", errNewRequest)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatalf("Do() error = %v", errDo)
	}
	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatalf("ReadAll() error = %v", errRead)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close error = %v", errClose)
	}

	// 1. Plain body reading must NOT set TTFT
	if reporter.IsTTFTSet() {
		t.Fatalf("TrackHTTPClientRoundTripOnly must not set TTFT on plain body read")
	}

	// 2. Observing metadata event records fallback, but does NOT set effective TTFT
	ObserveResponsesTokenEvent(reporter, bodyBytes)
	if reporter.IsTTFTSet() {
		t.Fatalf("Observing metadata event must not set effective TTFT")
	}
	if reporter.ttftDuration() <= 0 {
		t.Fatalf("Fallback TTFT should be recorded and > 0, got %v", reporter.ttftDuration())
	}

	// 3. Observing substantive token event sets effective TTFT
	ObserveResponsesTokenEvent(reporter, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	if !reporter.IsTTFTSet() {
		t.Fatalf("Observing token event must set effective TTFT")
	}
}

func TestUsageReporterTrackHTTPClientRoundTripOnly_ErrorResponseRecordsFirstPacketFallback(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "codex", "gpt-5.6-luna", nil)
	client := reporter.TrackHTTPClientRoundTripOnly(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Status:     "429 Too Many Requests",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limit"}}`)),
				Request:    req,
			}, nil
		}),
	})

	req, errNewRequest := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.invalid/v1/responses", strings.NewReader("{}"))
	if errNewRequest != nil {
		t.Fatalf("NewRequestWithContext() error = %v", errNewRequest)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatalf("Do() error = %v", errDo)
	}
	_, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatalf("ReadAll() error = %v", errRead)
	}
	_ = resp.Body.Close()

	if reporter.IsTTFTSet() {
		t.Fatalf("error response read must not set substantive token TTFT")
	}
	if !reporter.IsFirstPacketSet() {
		t.Fatalf("error response read must record first packet set fallback")
	}
}

func TestUsageReporterObserveTokenEvent_FastPathNonTokenAndToken(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "codex", "gpt-5.6-luna", nil)
	reporter.StartResponseTTFT()

	// 1. Initial state
	if reporter.IsTTFTSet() {
		t.Fatalf("expected IsTTFTSet() == false initially")
	}

	// 2. First non-token event records firstPacketDuration, but does not mark TTFT set
	reporter.ObserveTokenEvent(false)
	if reporter.IsTTFTSet() {
		t.Fatalf("ObserveTokenEvent(false) must not set TTFT")
	}
	if !reporter.IsFirstPacketSet() {
		t.Fatalf("expected IsFirstPacketSet() == true")
	}
	firstPacketDuration := reporter.firstPacketDuration

	// 3. Subsequent non-token event is a fast-path return and does not alter firstPacketDuration
	reporter.ObserveTokenEvent(false)
	if reporter.firstPacketDuration != firstPacketDuration {
		t.Fatalf("subsequent ObserveTokenEvent(false) must preserve original firstPacketDuration")
	}

	// 4. Substantive token event sets effective TTFT
	reporter.ObserveTokenEvent(true)
	if !reporter.IsTTFTSet() {
		t.Fatalf("ObserveTokenEvent(true) must set IsTTFTSet() == true")
	}
	tokenTTFT := reporter.ttft

	// 5. Subsequent token event is a fast-path return and does not alter TTFT
	reporter.ObserveTokenEvent(true)
	if reporter.ttft != tokenTTFT {
		t.Fatalf("subsequent ObserveTokenEvent(true) must not alter already recorded TTFT")
	}
}

func TestUsageReporterBuildRecordDefaultsStreamFalse(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "openai", "gpt-5.4", nil)

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Stream {
		t.Fatalf("stream = %v, want false", record.Stream)
	}
}

func TestUsageReporterBuildRecordIncludesStreamTrue(t *testing.T) {
	ctx := usage.WithStream(context.Background(), true)
	reporter := NewUsageReporter(ctx, "openai", "gpt-5.4", nil)

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if !record.Stream {
		t.Fatalf("stream = %v, want true", record.Stream)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Ported from upstream CLIProxyAPI commit 580df36423e4: the usage reporter
// captures canonical session lineage from request metadata, validates parents,
// and supports explicit hierarchy overrides.
func TestUsageReporterPropagatesSessionHierarchy(t *testing.T) {
	ctx := logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{
		SessionID:       "claude:sess-1:agent:sub-1",
		ParentSessionID: "claude:sess-1",
	})

	reporter := NewUsageReporter(ctx, "claude", "claude-3-7-sonnet", nil)
	record := reporter.buildRecord(usage.Detail{TotalTokens: 100}, false, usage.Failure{})
	if record.SessionID != "claude:sess-1:agent:sub-1" || record.ParentSessionID != "claude:sess-1" {
		t.Fatalf("record session hierarchy = (%q, %q), want (claude:sess-1:agent:sub-1, claude:sess-1)", record.SessionID, record.ParentSessionID)
	}

	// Test explicit override via SetSessionHierarchy
	reporter.SetSessionHierarchy("override:child", "override:parent")
	record2 := reporter.buildRecord(usage.Detail{TotalTokens: 100}, false, usage.Failure{})
	if record2.SessionID != "override:child" || record2.ParentSessionID != "override:parent" {
		t.Fatalf("overridden record session hierarchy = (%q, %q), want (override:child, override:parent)", record2.SessionID, record2.ParentSessionID)
	}

	// Test self-loop elimination in SetSessionHierarchy
	reporter.SetSessionHierarchy("loop:node", "loop:node")
	record3 := reporter.buildRecord(usage.Detail{TotalTokens: 100}, false, usage.Failure{})
	if record3.SessionID != "loop:node" || record3.ParentSessionID != "" {
		t.Fatalf("self loop record session hierarchy = (%q, %q), want (loop:node, empty)", record3.SessionID, record3.ParentSessionID)
	}

	// Test orphan parent elimination when sessionID is empty
	reporter.SetSessionHierarchy("", "orphan:parent")
	record4 := reporter.buildRecord(usage.Detail{TotalTokens: 100}, false, usage.Failure{})
	if record4.SessionID != "" || record4.ParentSessionID != "" {
		t.Fatalf("orphan parent record session hierarchy = (%q, %q), want empty both", record4.SessionID, record4.ParentSessionID)
	}

	// Test cross-prefix alias rejection (e.g. pck:* and conv:* are aliases, not parent-child)
	ctxAlias := logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{
		SessionID:       "pck:prompt-key-123",
		ParentSessionID: "conv:conv-456",
	})
	reporterAlias := NewUsageReporter(ctxAlias, "openai", "gpt-5.4", nil)
	recordAlias := reporterAlias.buildRecord(usage.Detail{TotalTokens: 100}, false, usage.Failure{})
	if recordAlias.SessionID != "pck:prompt-key-123" || recordAlias.ParentSessionID != "" {
		t.Fatalf("cross prefix alias emitted as parent: (%q, %q), want (pck:prompt-key-123, empty)", recordAlias.SessionID, recordAlias.ParentSessionID)
	}

	reporterAlias.SetSessionHierarchy("pck:key-999", "conv:alias-888")
	recordAlias2 := reporterAlias.buildRecord(usage.Detail{TotalTokens: 100}, false, usage.Failure{})
	if recordAlias2.SessionID != "pck:key-999" || recordAlias2.ParentSessionID != "" {
		t.Fatalf("SetSessionHierarchy cross prefix alias emitted as parent: (%q, %q), want (pck:key-999, empty)", recordAlias2.SessionID, recordAlias2.ParentSessionID)
	}
}

// TestParseInteractionsUsage_CacheSemantics ports upstream e54a8e97's
// setClaudeUsageFromInteractions coverage to the local equivalent
// (parseInteractionsUsageDetail via ParseInteractionsUsage): cache-inclusive
// totals are split into uncached input plus cache_read/cache_creation.
func TestParseInteractionsUsage_CacheSemantics(t *testing.T) {
	tests := []struct {
		name                string
		payload             string
		wantInput           int64
		wantCacheRead       int64
		wantCacheCreation   int64
		wantCachedAggregate int64
	}{
		{
			name:                "cache_hit",
			payload:             `{"usage":{"total_input_tokens":10411,"total_output_tokens":76,"total_cached_tokens":10340,"total_tokens":10487}}`,
			wantInput:           71,
			wantCacheRead:       10340,
			wantCachedAggregate: 10340,
		},
		{
			name:                "non_streaming_cache_hit",
			payload:             `{"usage":{"total_input_tokens":96724,"total_output_tokens":269,"total_cached_tokens":30784,"total_tokens":96993}}`,
			wantInput:           65940,
			wantCacheRead:       30784,
			wantCachedAggregate: 30784,
		},
		{
			name:                "zero_cache_tokens",
			payload:             `{"usage":{"total_input_tokens":100,"total_output_tokens":50,"total_cached_tokens":0,"total_tokens":150}}`,
			wantInput:           100,
			wantCacheRead:       0,
			wantCachedAggregate: 0,
		},
		{
			name:                "explicit_uncached_and_cache_creation",
			payload:             `{"usage":{"input_tokens":71,"total_input_tokens":10411,"total_output_tokens":76,"cache_read_input_tokens":10340,"cache_creation_input_tokens":25,"total_tokens":10487}}`,
			wantInput:           71,
			wantCacheRead:       10340,
			wantCacheCreation:   25,
			wantCachedAggregate: 0,
		},
		{
			name:                "explicit_uncached_with_cache_write_in_total",
			payload:             `{"usage":{"input_tokens":71,"total_input_tokens":10436,"total_output_tokens":76,"cache_read_input_tokens":10340,"cache_creation_input_tokens":25,"total_tokens":10512}}`,
			wantInput:           71,
			wantCacheRead:       10340,
			wantCacheCreation:   25,
			wantCachedAggregate: 0,
		},
		{
			name:                "total_only_cache_read_and_write",
			payload:             `{"usage":{"total_input_tokens":10436,"total_output_tokens":76,"cache_read_input_tokens":10340,"cache_creation_input_tokens":25,"total_tokens":10512}}`,
			wantInput:           71,
			wantCacheRead:       10340,
			wantCacheCreation:   25,
			wantCachedAggregate: 0,
		},
		{
			name:                "full_cache_hit",
			payload:             `{"usage":{"total_input_tokens":500,"total_output_tokens":50,"total_cached_tokens":500,"total_tokens":550}}`,
			wantInput:           0,
			wantCacheRead:       500,
			wantCachedAggregate: 500,
		},
		{
			name:                "cached_tokens_exceeds_input",
			payload:             `{"usage":{"total_input_tokens":50,"total_output_tokens":50,"total_cached_tokens":100,"total_tokens":150}}`,
			wantInput:           0,
			wantCacheRead:       100,
			wantCachedAggregate: 100,
		},
		{
			name:                "prompt_tokens_treated_as_cache_inclusive_total",
			payload:             `{"usage":{"prompt_tokens":1000,"cached_tokens":250,"completion_tokens":40}}`,
			wantInput:           750,
			wantCacheRead:       250,
			wantCachedAggregate: 250,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := ParseInteractionsUsage([]byte(tt.payload))
			if detail.InputTokens != tt.wantInput {
				t.Errorf("InputTokens = %d, want %d", detail.InputTokens, tt.wantInput)
			}
			if detail.CacheReadTokens != tt.wantCacheRead {
				t.Errorf("CacheReadTokens = %d, want %d", detail.CacheReadTokens, tt.wantCacheRead)
			}
			if detail.CacheCreationTokens != tt.wantCacheCreation {
				t.Errorf("CacheCreationTokens = %d, want %d", detail.CacheCreationTokens, tt.wantCacheCreation)
			}
			if detail.CachedTokens != tt.wantCachedAggregate {
				t.Errorf("CachedTokens = %d, want %d", detail.CachedTokens, tt.wantCachedAggregate)
			}
		})
	}
}

type TestUsageExecutor struct{}

func (TestUsageExecutor) Identifier() string {
	return "test-provider"
}

// Ported from upstream CLIProxyAPI commit 9a2201c36a0a.
type usageResponseBodyError struct {
	status  int
	message string
	body    []byte
}

func (e usageResponseBodyError) Error() string {
	return e.message
}

func (e usageResponseBodyError) StatusCode() int {
	return e.status
}

func (e usageResponseBodyError) ResponseBody() []byte {
	return e.body
}

func TestFailFromErrorsPrefersResponseBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "original response body", body: []byte(" \n{\"error\":\"upstream rejected request\"}\r\n")},
		{name: "empty response body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errExecute := fmt.Errorf("execute failed: %w", usageResponseBodyError{
				status:  http.StatusUnauthorized,
				message: "generic upstream error",
				body:    tc.body,
			})
			failure := failFromErrors(errExecute)
			wantBody := errExecute.Error()
			if len(tc.body) > 0 {
				wantBody = string(tc.body)
			}
			if failure.StatusCode != http.StatusUnauthorized || failure.Body != wantBody {
				t.Fatalf("failure = %#v, want status %d body %q", failure, http.StatusUnauthorized, wantBody)
			}
		})
	}
}

func TestUsageReporter_AllocatesUniqueUUIDPerAttemptWithSharedTraceID(t *testing.T) {
	ctx := logging.WithRequestID(context.Background(), "00000042")

	reporter1 := NewUsageReporter(ctx, "openai", "gpt-5.4", nil)
	record1 := reporter1.buildRecord(usage.Detail{TotalTokens: 10}, false, usage.Failure{})

	reporter2 := NewUsageReporter(ctx, "claude", "claude-3-7-sonnet", nil)
	record2 := reporter2.buildRecord(usage.Detail{TotalTokens: 20}, false, usage.Failure{})

	if record1.TraceID != "00000042" {
		t.Fatalf("record1.TraceID = %q, want 00000042", record1.TraceID)
	}
	if record2.TraceID != "00000042" {
		t.Fatalf("record2.TraceID = %q, want 00000042", record2.TraceID)
	}
	if record1.RequestID == "" {
		t.Fatal("record1.RequestID should not be empty")
	}
	if record2.RequestID == "" {
		t.Fatal("record2.RequestID should not be empty")
	}
	if record1.RequestID == record2.RequestID {
		t.Fatalf("consecutive attempts must have distinct UUID RequestIDs: got %q and %q", record1.RequestID, record2.RequestID)
	}
	u1, err1 := uuid.Parse(record1.RequestID)
	if err1 != nil || u1.Version() != 4 {
		t.Fatalf("record1.RequestID %q is not valid UUID v4: %v", record1.RequestID, err1)
	}
	u2, err2 := uuid.Parse(record2.RequestID)
	if err2 != nil || u2.Version() != 4 {
		t.Fatalf("record2.RequestID %q is not valid UUID v4: %v", record2.RequestID, err2)
	}
}

func TestUsageReporter_ExplicitTraceIDPrecedenceOverLogRequestID(t *testing.T) {
	ctx := logging.WithRequestID(context.Background(), "log-id-1")
	ctx = usage.WithTraceID(ctx, "explicit-trace-1")

	reporter := NewUsageReporter(ctx, "openai", "gpt-5.4", nil)
	record := reporter.buildRecord(usage.Detail{TotalTokens: 10}, false, usage.Failure{})

	if record.TraceID != "explicit-trace-1" {
		t.Fatalf("record.TraceID = %q, want explicit-trace-1", record.TraceID)
	}
}

// streamTruncatedFailure is the executor-side classification contract: a
// locally synthesized failure for an accepted stream whose terminal frame never
// arrived.
type streamTruncatedFailure struct {
	msg string
}

func (e streamTruncatedFailure) Error() string           { return e.msg }
func (e streamTruncatedFailure) StatusCode() int         { return http.StatusBadGateway }
func (e streamTruncatedFailure) IsStreamTruncated() bool { return true }

// plainGatewayFailure carries the same status and message without the
// truncation classification, so the test shows the code is driven by the
// classification and not by the status or the text.
type plainGatewayFailure struct {
	msg string
}

func (e plainGatewayFailure) Error() string   { return e.msg }
func (e plainGatewayFailure) StatusCode() int { return http.StatusBadGateway }

func TestFailFromErrorsCarriesStreamTruncatedCode(t *testing.T) {
	const msg = "upstream stream closed before finishReason"

	fail := failFromErrors(streamTruncatedFailure{msg: msg})
	if fail.Code != usage.FailureStreamTruncatedCode {
		t.Fatalf("fail.Code = %q, want %q", fail.Code, usage.FailureStreamTruncatedCode)
	}
	if fail.StatusCode != http.StatusBadGateway {
		t.Fatalf("fail.StatusCode = %d, want %d", fail.StatusCode, http.StatusBadGateway)
	}
	if fail.Body != msg {
		t.Fatalf("fail.Body = %q, want %q", fail.Body, msg)
	}

	plain := failFromErrors(plainGatewayFailure{msg: msg})
	if plain.Code != "" {
		t.Fatalf("an unclassified 502 must not gain a code, got %q", plain.Code)
	}

	textOnly := failFromErrors(errors.New(msg))
	if textOnly.Code != "" {
		t.Fatalf("a plain error must not gain a code, got %q", textOnly.Code)
	}
}
