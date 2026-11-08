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

// TestStreamUsageBufferKeepsLastUsageEvent proves the buffer keeps the final
// authoritative snapshot rather than the first usage object a stream reports.
// Before the merge layer existed the reporter's once.Do recorded the first
// event, so a stream's terminal totals were silently discarded.
func TestStreamUsageBufferKeepsLastUsageEvent(t *testing.T) {
	var buffer StreamUsageBuffer

	ObserveMergedStreamUsage(&buffer, usage.Detail{
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  15,
	})
	ObserveMergedStreamUsage(&buffer, usage.Detail{
		InputTokens:  20,
		OutputTokens: 8,
		TotalTokens:  28,
	})

	detail, ok := buffer.Detail()
	if !ok {
		t.Fatal("StreamUsageBuffer.Detail() ok = false, want true")
	}
	if detail.InputTokens != 20 || detail.OutputTokens != 8 || detail.TotalTokens != 28 {
		t.Fatalf("detail = {input:%d output:%d total:%d}, want {input:20 output:8 total:28}",
			detail.InputTokens, detail.OutputTokens, detail.TotalTokens)
	}
	if !detail.TokenBreakdown.Valid() || detail.TokenBreakdown.TotalTokens != 28 {
		t.Fatalf("breakdown = %+v, want a valid breakdown totalling 28", detail.TokenBreakdown)
	}

	reporter := NewUsageReporter(context.Background(), "openai", "gpt-5.4", nil)
	if !buffer.Publish(context.Background(), reporter) {
		t.Fatal("StreamUsageBuffer.Publish() = false, want true")
	}
}

// claudeMessageStartLine is a Messages API message_start frame carrying the
// prompt-cache TTL split, per-iteration usage and the served speed.
const claudeMessageStartLine = `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":6,"output_tokens":1,"cache_read_input_tokens":3036,"cache_creation_input_tokens":8846,"cache_creation":{"ephemeral_5m_input_tokens":5810,"ephemeral_1h_input_tokens":3036},"speed":"standard","iterations":[{"type":"message","input_tokens":4,"output_tokens":53,"cache_read_input_tokens":0,"cache_creation_input_tokens":3036,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":3036}},{"type":"message","input_tokens":2,"output_tokens":430,"cache_read_input_tokens":3036,"cache_creation_input_tokens":5810,"cache_creation":{"ephemeral_5m_input_tokens":5810,"ephemeral_1h_input_tokens":0}}]}}}`

// TestParseClaudeStreamUsageKeepsCacheCreationTTLBuckets pins the TTL split,
// the iteration list and the served speed from a message_start frame.
// Ported from upstream 89e472f3251e.
func TestParseClaudeStreamUsageKeepsCacheCreationTTLBuckets(t *testing.T) {
	detail, ok := ParseClaudeStreamUsage([]byte(claudeMessageStartLine))
	if !ok {
		t.Fatal("ParseClaudeStreamUsage() ok = false, want true")
	}
	if detail.CacheCreation5mTokens == nil || *detail.CacheCreation5mTokens != 5810 {
		t.Fatalf("CacheCreation5mTokens = %v, want 5810", detail.CacheCreation5mTokens)
	}
	if detail.CacheCreation1hTokens == nil || *detail.CacheCreation1hTokens != 3036 {
		t.Fatalf("CacheCreation1hTokens = %v, want 3036", detail.CacheCreation1hTokens)
	}
	if detail.CacheCreation30mTokens != nil {
		t.Fatalf("CacheCreation30mTokens = %v, want nil", *detail.CacheCreation30mTokens)
	}
	if detail.CacheDetails == nil || len(detail.CacheDetails.ByTTL) != 2 {
		t.Fatalf("CacheDetails = %+v, want two TTL buckets", detail.CacheDetails)
	}
	if iterations := detail.IterationSlice(); len(iterations) != 2 {
		t.Fatalf("len(Iterations) = %d, want 2", len(iterations))
	}
	if detail.Speed != "standard" {
		t.Fatalf("Speed = %q, want %q", detail.Speed, "standard")
	}
	if detail.ResponseServiceTier != "standard" {
		t.Fatalf("ResponseServiceTier = %q, want %q", detail.ResponseServiceTier, "standard")
	}
	if !detail.TokenBreakdown.Valid() {
		t.Fatalf("TokenBreakdown = %+v, want a valid breakdown", detail.TokenBreakdown)
	}
}

// TestStreamUsageBufferObservesClaudeMessageStart drives the same frame through
// the stream buffer, which is the path executors use.
func TestStreamUsageBufferObservesClaudeMessageStart(t *testing.T) {
	var buffer StreamUsageBuffer
	buffer.ObserveClaudeStream([]byte(claudeMessageStartLine))

	detail, ok := buffer.Detail()
	if !ok {
		t.Fatal("StreamUsageBuffer.Detail() ok = false, want true")
	}
	if detail.CacheCreation5mTokens == nil || *detail.CacheCreation5mTokens != 5810 {
		t.Fatalf("CacheCreation5mTokens = %v, want 5810", detail.CacheCreation5mTokens)
	}
	if detail.CacheCreation1hTokens == nil || *detail.CacheCreation1hTokens != 3036 {
		t.Fatalf("CacheCreation1hTokens = %v, want 3036", detail.CacheCreation1hTokens)
	}
	if got := buffer.ResponseModel(); got != "claude-opus-5-5" {
		t.Fatalf("ResponseModel() = %q, want %q", got, "claude-opus-5-5")
	}
}

// TestParseCodexUsageExtractsXAIServerSideToolUsageAndModalities pins the xAI
// Responses usage shape: modality sub-buckets, server-side tool counters and
// cost ticks. Ported from upstream 24840fddf843.
func TestParseCodexUsageExtractsXAIServerSideToolUsageAndModalities(t *testing.T) {
	data := []byte(`{"type":"response.completed","response":{"id":"resp_xai_1","usage":{"input_tokens":3804,"input_tokens_details":{"cached_tokens":3072,"text_tokens":3500,"image_tokens":304,"cached_prompt_text_tokens":3072},"output_tokens":889,"output_tokens_details":{"reasoning_tokens":828,"text_tokens":61,"accepted_prediction_tokens":5,"rejected_prediction_tokens":2},"total_tokens":4693,"num_sources_used":0,"num_server_side_tools_used":1,"cost_in_usd_ticks":76246640,"server_side_tool_usage_details":{"web_search_calls":0,"x_search_calls":1,"x_posts_fetched":1,"x_users_fetched":0,"code_interpreter_calls":0,"file_search_calls":0,"mcp_calls":0,"document_search_calls":0,"image_generation_calls":0}}}}`)
	detail, ok := ParseCodexUsage(data)
	if !ok {
		t.Fatal("expected ParseCodexUsage ok = true")
	}
	if detail.TextInputTokens == nil || *detail.TextInputTokens != 3500 || detail.ImageInputTokens == nil || *detail.ImageInputTokens != 304 || detail.CachedTextTokens == nil || *detail.CachedTextTokens != 3072 {
		t.Fatalf("unexpected input modality tokens: %+v", detail)
	}
	if detail.TextOutputTokens == nil || *detail.TextOutputTokens != 61 || detail.AcceptedPredictionTokens == nil || *detail.AcceptedPredictionTokens != 5 || detail.RejectedPredictionTokens == nil || *detail.RejectedPredictionTokens != 2 {
		t.Fatalf("unexpected output modality tokens: %+v", detail)
	}
	if detail.ToolUsageDetails == nil || detail.ToolUsageDetails.XSearchCalls == nil || *detail.ToolUsageDetails.XSearchCalls != 1 || detail.ToolUsageDetails.XPostsFetched == nil || *detail.ToolUsageDetails.XPostsFetched != 1 || detail.ToolUsageDetails.NumServerSideToolsUsed == nil || *detail.ToolUsageDetails.NumServerSideToolsUsed != 1 {
		t.Fatalf("unexpected ToolUsageDetails: %+v", detail.ToolUsageDetails)
	}
	if detail.CostDetails == nil || detail.CostDetails.CostInUSDTicks == nil || *detail.CostDetails.CostInUSDTicks != 76246640 {
		t.Fatalf("unexpected CostDetails: %+v", detail.CostDetails)
	}
}

// TestParseXAIImageAndVideoUsage pins the xAI media parameter extraction.
// Ported from upstream 24840fddf843.
func TestParseXAIImageAndVideoUsage(t *testing.T) {
	imgDetail := ParseXAIImageUsage(
		[]byte(`{"model":"grok-imagine-image","prompt":"cat","aspect_ratio":"16:9","resolution":"2k","quality":"medium"}`),
		[]byte(`{"data":[{"b64_json":"AA=="},{"b64_json":"BB=="}],"usage":{"prompt_tokens":12,"completion_tokens":100,"total_tokens":112}}`),
	)
	if imgDetail.TotalTokens != 112 || imgDetail.ImageCount == nil || *imgDetail.ImageCount != 2 {
		t.Fatalf("unexpected image usage: %+v", imgDetail)
	}
	if imgDetail.ImageDetails == nil || imgDetail.ImageDetails.AspectRatio != "16:9" || imgDetail.ImageDetails.Resolution != "2k" || imgDetail.ImageDetails.Quality != "medium" {
		t.Fatalf("unexpected image details: %+v", imgDetail.ImageDetails)
	}

	vidDetail := ParseXAIVideoUsage(
		[]byte(`{"model":"grok-imagine-video","prompt":"wave","aspect_ratio":"16:9","resolution":"720p","duration":5}`),
		[]byte(`{"status":"completed","url":"https://example.com/v.mp4","usage":{"prompt_tokens":20,"completion_tokens":400,"total_tokens":420}}`),
	)
	if vidDetail.TotalTokens != 420 || vidDetail.ImageDetails == nil || vidDetail.ImageDetails.Resolution != "720p" || vidDetail.ImageDetails.AspectRatio != "16:9" {
		t.Fatalf("unexpected video usage: %+v", vidDetail)
	}
	if vidDetail.MediaCounts == nil || vidDetail.MediaCounts.CompletionMedia == nil || *vidDetail.MediaCounts.CompletionMedia != 1 || vidDetail.MediaCounts.AudioSeconds == nil || *vidDetail.MediaCounts.AudioSeconds != 5 {
		t.Fatalf("unexpected video media counts: %+v", vidDetail.MediaCounts)
	}
}

// TestParseCodexUsageWithCollectedOutputItemsAndImageToolUsage pins the
// response.output_item.done collection path, the cache retention policy and the
// image-tool split. Ported from upstream 01551f5f6b26.
func TestParseCodexUsageWithCollectedOutputItemsAndImageToolUsage(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_codex_1","service_tier":"priority","prompt_cache_retention":"24h","output":[],"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1024,"text_tokens":1100,"image_tokens":100,"cached_tokens_details":{"text_tokens":1024}},"output_tokens":350,"output_tokens_details":{"reasoning_tokens":200,"text_tokens":150},"total_tokens":1550,"attribution":{"tier":"priority"}},"tool_usage":{"image_gen":{"input_tokens":80,"input_tokens_details":{"text_tokens":30,"image_tokens":50},"output_tokens":1056,"output_tokens_details":{"image_tokens":1056,"text_tokens":0},"total_tokens":1136}}}}`)
	itemsByIndex := map[int64][]byte{
		0: []byte(`{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"openai responses usage","sources":[{"url":"https://a.example"},{"url":"https://b.example"}]}}`),
		1: []byte(`{"id":"ci_1","type":"code_interpreter_call","status":"completed"}`),
		2: []byte(`{"id":"ig_1","type":"image_generation_call","status":"completed","size":"1024x1024","quality":"high","background":"transparent","output_format":"png","result":"AA=="}`),
		3: []byte(`{"id":"fc_1","type":"function_call","status":"completed","name":"run_check","call_id":"call_1","arguments":"{}"}`),
	}
	detail, ok := ParseCodexUsageWithOutputItems(completed, itemsByIndex, nil)
	if !ok {
		t.Fatal("expected ParseCodexUsageWithOutputItems ok = true")
	}
	if detail.ResponseServiceTier != "priority" || detail.UpstreamServiceTier != "priority" {
		t.Fatalf("unexpected service tier: response=%q upstream=%q", detail.ResponseServiceTier, detail.UpstreamServiceTier)
	}
	if detail.CacheDetails == nil || detail.CacheDetails.Retention != "24h" {
		t.Fatalf("unexpected CacheDetails: %+v", detail.CacheDetails)
	}
	if detail.CachedTextTokens == nil || *detail.CachedTextTokens != 1024 {
		t.Fatalf("unexpected CachedTextTokens: %+v", detail.CachedTextTokens)
	}
	if detail.WebSearchRequests == nil || *detail.WebSearchRequests != 1 {
		t.Fatalf("unexpected WebSearchRequests: %+v", detail.WebSearchRequests)
	}
	if detail.ToolUsageDetails == nil ||
		detail.ToolUsageDetails.WebSearchCalls == nil || *detail.ToolUsageDetails.WebSearchCalls != 1 ||
		detail.ToolUsageDetails.WebSearchQueries == nil || *detail.ToolUsageDetails.WebSearchQueries != 1 ||
		detail.ToolUsageDetails.NumSourcesUsed == nil || *detail.ToolUsageDetails.NumSourcesUsed != 2 ||
		detail.ToolUsageDetails.CodeExecutionCalls == nil || *detail.ToolUsageDetails.CodeExecutionCalls != 1 ||
		detail.ToolUsageDetails.ImageGenerationCalls == nil || *detail.ToolUsageDetails.ImageGenerationCalls != 1 ||
		detail.ToolUsageDetails.ToolCallsRequested == nil || *detail.ToolUsageDetails.ToolCallsRequested != 1 {
		t.Fatalf("unexpected ToolUsageDetails: %+v", detail.ToolUsageDetails)
	}
	if detail.ImageCount != nil || detail.ImageDetails != nil {
		t.Fatalf("expected orchestrator record ImageCount/ImageDetails to be nil when tool_usage.image_gen is present, got count=%+v details=%+v", detail.ImageCount, detail.ImageDetails)
	}

	imgToolDetail, okImg := ParseCodexImageToolUsageWithOutputItems([]byte(`{"tools":[{"type":"image_generation","input_fidelity":"high"}]}`), completed, itemsByIndex, nil)
	if !okImg {
		t.Fatal("expected ParseCodexImageToolUsageWithOutputItems ok = true")
	}
	if imgToolDetail.TotalTokens != 1136 || imgToolDetail.TextInputTokens == nil || *imgToolDetail.TextInputTokens != 30 || imgToolDetail.ImageInputTokens == nil || *imgToolDetail.ImageInputTokens != 50 || imgToolDetail.ImageOutputTokens == nil || *imgToolDetail.ImageOutputTokens != 1056 {
		t.Fatalf("unexpected image tool token breakdown: %+v", imgToolDetail)
	}
	if imgToolDetail.ImageCount == nil || *imgToolDetail.ImageCount != 1 || imgToolDetail.ImageDetails == nil || imgToolDetail.ImageDetails.Size != "1024x1024" || imgToolDetail.ImageDetails.InputFidelity != "high" {
		t.Fatalf("unexpected image tool metadata: count=%+v details=%+v", imgToolDetail.ImageCount, imgToolDetail.ImageDetails)
	}
}

// TestStreamUsageBufferObserveOpenAIStreamToolCallsAndImageCompleted pins the
// tool-call and image-completion counting the OpenAI stream observer performs.
// Ported from upstream 01551f5f6b26.
func TestStreamUsageBufferObserveOpenAIStreamToolCallsAndImageCompleted(t *testing.T) {
	var buffer StreamUsageBuffer
	buffer.ObserveOpenAIStream([]byte(`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f1","arguments":""}}]}}]}`))
	buffer.ObserveOpenAIStream([]byte(`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}},{"index":1,"id":"call_2","type":"function","function":{"name":"f2","arguments":"{}"}}]}}]}`))
	buffer.ObserveOpenAIStream([]byte(`data: {"id":"chatcmpl-1","service_tier":"default","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":20,"total_tokens":70}}`))
	detail, ok := buffer.Detail()
	if !ok {
		t.Fatal("expected buffer.Detail() ok = true")
	}
	if detail.TotalTokens != 70 || detail.ToolUsageDetails == nil || detail.ToolUsageDetails.ToolCallsRequested == nil || *detail.ToolUsageDetails.ToolCallsRequested != 2 {
		t.Fatalf("unexpected OpenAI stream tool call usage: %+v, tools=%+v", detail, detail.ToolUsageDetails)
	}

	var imgBuffer StreamUsageBuffer
	imgBuffer.ObserveOpenAIStream([]byte(`data: {"type":"image_generation.completed","b64_json":"AA==","size":"1024x1536","quality":"medium","background":"opaque","output_format":"webp","usage":{"input_tokens":25,"input_tokens_details":{"text_tokens":25,"image_tokens":0},"output_tokens":1500,"output_tokens_details":{"image_tokens":1500,"text_tokens":0},"total_tokens":1525}}`))
	imgDetail, okImg := imgBuffer.Detail()
	if !okImg {
		t.Fatal("expected imgBuffer.Detail() ok = true")
	}
	if imgDetail.TotalTokens != 1525 || imgDetail.ImageCount == nil || *imgDetail.ImageCount != 1 || imgDetail.ImageDetails == nil || imgDetail.ImageDetails.Size != "1024x1536" || imgDetail.ImageDetails.Quality != "medium" || imgDetail.ImageDetails.OutputFormat != "webp" {
		t.Fatalf("unexpected OpenAI image stream usage: %+v, details=%+v", imgDetail, imgDetail.ImageDetails)
	}
}

// TestResponsesCacheWriteLifetimesAreReportedNotInferred proves the 30m/12h
// cache-write buckets come from the payload and are never inferred from the
// retention policy. Ported from upstream 01551f5f6b26.
func TestResponsesCacheWriteLifetimesAreReportedNotInferred(t *testing.T) {
	tests := []struct {
		name, body  string
		short, long *int64
	}{
		{name: "mixed", body: `{"response":{"prompt_cache_options":{"mode":"explicit","ttl":"12h"},"usage":{"input_tokens":100,"output_tokens":1,"total_tokens":101,"input_tokens_details":{"cache_write_tokens":70,"cache_write_tokens_details":{"ttl_30m_tokens":30,"ttl_12h_tokens":40}}}}}`, short: usage.Int64Ptr(30), long: usage.Int64Ptr(40)},
		{name: "explicit zero", body: `{"response":{"usage":{"input_tokens":100,"output_tokens":1,"input_tokens_details":{"cache_write_tokens":70,"cache_write_tokens_details":{"ttl_30m_tokens":0,"ttl_12h_tokens":70}}}}}`, short: usage.Int64Ptr(0), long: usage.Int64Ptr(70)},
		{name: "ttl does not imply bucket", body: `{"response":{"prompt_cache_options":{"mode":"explicit","ttl":"12h"},"usage":{"input_tokens":100,"output_tokens":1,"input_tokens_details":{"cache_write_tokens":70}}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, ok := ParseCodexUsage([]byte(tt.body))
			if !ok {
				t.Fatal("missing usage")
			}
			check := func(name string, got, want *int64) {
				t.Helper()
				if (got == nil) != (want == nil) || (got != nil && *got != *want) {
					t.Errorf("%s got %v want %v", name, got, want)
				}
			}
			check("30m", d.CacheCreation30mTokens, tt.short)
			check("12h", d.CacheCreation12hTokens, tt.long)
			if d.CacheCreationTokens != 70 {
				t.Fatalf("cache total=%d", d.CacheCreationTokens)
			}
		})
	}
}

// TestOpenAIStreamToolCallsAcrossChoicesAndIncrementalChunks proves tool calls
// are counted once per (choice, index) across incremental delta frames.
// Ported from upstream 01551f5f6b26.
func TestOpenAIStreamToolCallsAcrossChoicesAndIncrementalChunks(t *testing.T) {
	var b StreamUsageBuffer
	b.ObserveOpenAIStream([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a"}]}}]}`))
	b.ObserveOpenAIStream([]byte(`data: {"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"b"}]}}]}`))
	d, ok := b.Detail()
	if !ok || d.ToolUsageDetails == nil || d.ToolUsageDetails.ToolCallsRequested == nil || *d.ToolUsageDetails.ToolCallsRequested != 2 {
		t.Fatalf("partial tool calls = %+v, want 2", d.ToolUsageDetails)
	}
	b.ObserveOpenAIStream([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	d, _ = b.Detail()
	if d.ToolUsageDetails == nil || d.ToolUsageDetails.ToolCallsRequested == nil || *d.ToolUsageDetails.ToolCallsRequested != 2 {
		t.Fatalf("terminal tool calls = %+v, want 2", d.ToolUsageDetails)
	}

	var inc StreamUsageBuffer
	inc.ObserveOpenAIStream([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0}]}}]}`))
	inc.ObserveOpenAIStream([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1}]}}]}`))
	dInc, _ := inc.Detail()
	if dInc.ToolUsageDetails == nil || dInc.ToolUsageDetails.ToolCallsRequested == nil || *dInc.ToolUsageDetails.ToolCallsRequested != 2 {
		t.Fatalf("incremental tool calls = %+v, want 2", dInc.ToolUsageDetails)
	}
}

// TestOpenAIStreamMultipleImageCompletionsWithoutUsage proves image completions
// are counted even when no usage object is present, and the latest metadata wins.
// Ported from upstream 01551f5f6b26.
func TestOpenAIStreamMultipleImageCompletionsWithoutUsage(t *testing.T) {
	var b StreamUsageBuffer
	b.ObserveOpenAIStream([]byte(`data: {"type":"image_generation.completed","b64_json":"AA==","size":"1024x1024"}`))
	b.ObserveOpenAIStream([]byte(`data: {"type":"image_generation.completed","b64_json":"BB==","size":"1024x1536"}`))
	d, ok := b.Detail()
	if !ok || d.ImageCount == nil || *d.ImageCount != 2 || d.ImageDetails == nil || d.ImageDetails.Size != "1024x1536" {
		t.Fatalf("image stream detail = %+v details=%+v, want ImageCount=2 and latest size", d, d.ImageDetails)
	}
}

// TestCodexCustomToolCallWithoutXSearchPrefixCountsAsRequestedToolCall proves a
// client custom tool call is not mistaken for an xAI X-search call.
// Ported from upstream 01551f5f6b26.
func TestCodexCustomToolCallWithoutXSearchPrefixCountsAsRequestedToolCall(t *testing.T) {
	d, ok := ParseCodexUsage([]byte(`{"response":{"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"custom_tool_call","status":"completed","name":"x_keyword_search","call_id":"call_local_1"}]}}`))
	if !ok || d.ToolUsageDetails == nil || d.ToolUsageDetails.ToolCallsRequested == nil || *d.ToolUsageDetails.ToolCallsRequested != 1 {
		t.Fatalf("misclassified client custom_tool_call: %+v", d.ToolUsageDetails)
	}
}
