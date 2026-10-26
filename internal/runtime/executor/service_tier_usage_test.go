package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

type captureServiceTierPlugin struct {
	alias   string
	records chan coreusage.Record
}

func (p *captureServiceTierPlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	if p == nil || record.Alias != p.alias {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

func (p *captureServiceTierPlugin) await(t *testing.T) coreusage.Record {
	t.Helper()
	select {
	case record := <-p.records:
		return record
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the usage record")
		return coreusage.Record{}
	}
}

type noopServiceTierPlugin struct{}

func (noopServiceTierPlugin) HandleUsage(context.Context, coreusage.Record) {}

// TestPayloadBarrierEffectiveServiceTierUsageAfterOverride pins that the usage
// record names the tier the final outbound request carries: a payload override
// that sets service_tier must reach both the upstream body and the record, even
// though the client asked for no tier at all. The negative case pins that a
// request with no override records no tier.
func TestPayloadBarrierEffectiveServiceTierUsageAfterOverride(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override map[string]any
		want     string
	}{
		{name: "override sets priority", override: map[string]any{"service_tier": "priority"}, want: "priority"},
		{name: "no override records nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alias := t.Name()
			capture := &captureServiceTierPlugin{alias: alias, records: make(chan coreusage.Record, 4)}
			coreusage.RegisterNamedPlugin(alias, capture)
			t.Cleanup(func() {
				coreusage.RegisterNamedPlugin(alias, noopServiceTierPlugin{})
			})

			var upstreamBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamBody = bytes.Clone(body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-6-astra\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":99,\"total_tokens\":108}}\n\n")
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			}))
			defer server.Close()

			cfg := &config.Config{}
			if tc.override != nil {
				cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "*"}},
					Params: tc.override,
				}}}
			}
			ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
			executor := NewOpenAICompatExecutor("openai-compatibility", cfg)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL + "/v1",
				"api_key":  "test",
			}}
			result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
				Model:   "gpt-6-astra",
				Payload: []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatOpenAI,
				Stream:       true,
			})
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error: %v", chunk.Err)
				}
			}

			if got := gjson.GetBytes(upstreamBody, "service_tier").String(); got != tc.want {
				t.Fatalf("upstream body service_tier = %q, want %q; body=%s", got, tc.want, upstreamBody)
			}
			record := capture.await(t)
			if record.EffectiveServiceTier != tc.want {
				t.Fatalf("record EffectiveServiceTier = %q, want %q", record.EffectiveServiceTier, tc.want)
			}
		})
	}
}

// TestClaudeExecutorRecordsResponseServiceTier pins that Anthropic's usage.speed
// reaches the usage record as the response tier, on both the non-streaming and
// the streaming path, and that a response reporting no speed records none.
func TestClaudeExecutorRecordsResponseServiceTier(t *testing.T) {
	for _, tc := range []struct {
		name       string
		speed      string
		stream     bool
		wantTier   string
		wantInput  int64
		wantOutput int64
	}{
		{name: "nonstream fast", speed: "fast", wantTier: "fast", wantInput: 10, wantOutput: 5},
		{name: "nonstream standard", speed: "standard", wantTier: "standard", wantInput: 10, wantOutput: 5},
		{name: "nonstream absent", wantInput: 10, wantOutput: 5},
		// The local stream loop reports the message_delta usage frame, which
		// carries the final output count and no input count.
		{name: "stream fast", speed: "fast", stream: true, wantTier: "fast", wantOutput: 5},
		{name: "stream absent", stream: true, wantOutput: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			speedJSON := ""
			if tc.speed != "" {
				speedJSON = fmt.Sprintf(`,"speed":%q`, tc.speed)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1%s}}}\n\n", speedJSON)
					_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
					_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5%s}}\n\n", speedJSON)
					_, _ = fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":5%s}}`, speedJSON)
			}))
			defer server.Close()

			alias := t.Name()
			capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
			coreusage.RegisterNamedPlugin(t.Name(), capture)
			t.Cleanup(func() {
				coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
			})

			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: "auth-" + t.Name(), Provider: "claude", Attributes: map[string]string{
				"api_key":  "test",
				"base_url": server.URL,
			}}
			req := cliproxyexecutor.Request{
				Model:   "claude-opus-5",
				Payload: []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: tc.stream}
			ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
			if tc.stream {
				result, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream error: %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error: %v", chunk.Err)
					}
				}
			} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
				t.Fatalf("Execute error: %v", err)
			}

			record := capture.await(t)
			if record.ResponseServiceTier != tc.wantTier {
				t.Fatalf("record ResponseServiceTier = %q, want %q", record.ResponseServiceTier, tc.wantTier)
			}
			if record.Detail.ResponseServiceTier != tc.wantTier {
				t.Fatalf("detail ResponseServiceTier = %q, want %q", record.Detail.ResponseServiceTier, tc.wantTier)
			}
			// Byte-compat: the token numbers an unchanged response shape reports are unchanged.
			if record.Detail.InputTokens != tc.wantInput || record.Detail.OutputTokens != tc.wantOutput {
				t.Fatalf("tokens = %d/%d, want %d/%d (detail %+v)", record.Detail.InputTokens, record.Detail.OutputTokens, tc.wantInput, tc.wantOutput, record.Detail)
			}
			if record.Failed {
				t.Fatalf("record marked failed: %+v", record.Fail)
			}
		})
	}
}
