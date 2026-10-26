package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// TestOpenAICompatStreamUsageDialects drives an OpenAI-compatible stream and
// asserts the reasoning tokens the usage sink observes for the two dialects an
// upstream may speak: nested `*_tokens_details` and the flat `usage.reasoning_tokens`
// SGLang emits. A nested value always wins when both are present, so an unchanged
// OpenAI-shaped stream keeps reporting the nested number.
func TestOpenAICompatStreamUsageDialects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usageJSON string
		want      int64
	}{
		{
			name:      "flat reasoning_tokens",
			usageJSON: `{"prompt_tokens":9,"completion_tokens":99,"total_tokens":108,"reasoning_tokens":60}`,
			want:      60,
		},
		{
			name:      "nested completion_tokens_details",
			usageJSON: `{"prompt_tokens":9,"completion_tokens":99,"total_tokens":108,"completion_tokens_details":{"reasoning_tokens":60}}`,
			want:      60,
		},
		{
			name:      "nested wins over flat",
			usageJSON: `{"prompt_tokens":9,"completion_tokens":99,"total_tokens":108,"reasoning_tokens":60,"completion_tokens_details":{"reasoning_tokens":45}}`,
			want:      45,
		},
		{
			name:      "no reasoning reported",
			usageJSON: `{"prompt_tokens":9,"completion_tokens":99,"total_tokens":108}`,
			want:      0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alias := t.Name()
			capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
			coreusage.RegisterNamedPlugin(alias, capture)
			t.Cleanup(func() {
				coreusage.RegisterNamedPlugin(alias, multiProviderNoopUsagePlugin{})
			})

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"mercury-3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"pong\"},\"finish_reason\":null}]}\n\n")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"mercury-3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":%s}\n\n", tc.usageJSON)
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			}))
			defer server.Close()

			ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL + "/v1",
				"api_key":  "test",
			}}
			result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
				Model:   "mercury-3",
				Payload: []byte(`{"model":"mercury-3","messages":[{"role":"user","content":"ping"}]}`),
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

			record := capture.await(t)
			if got := record.Detail.ReasoningTokens; got != tc.want {
				t.Fatalf("ReasoningTokens = %d, want %d (detail %+v)", got, tc.want, record.Detail)
			}
			// Byte-compat: the token numbers an unchanged stream shape reports are unchanged.
			if record.Detail.InputTokens != 9 || record.Detail.OutputTokens != 99 || record.Detail.TotalTokens != 108 {
				t.Fatalf("usage = %+v, want input/output/total = 9/99/108", record.Detail)
			}
			if record.Failed {
				t.Fatalf("record marked failed: %+v", record.Fail)
			}
		})
	}
}
