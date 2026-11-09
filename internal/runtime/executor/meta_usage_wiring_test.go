package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// TestMetaExecutor_ExecuteStreamPreservesOutputItemDoneToolCounts proves the
// meta stream parses terminal usage with the collected output items, so tool
// calls survive an empty response.output.
func TestMetaExecutor_ExecuteStreamPreservesOutputItemDoneToolCounts(t *testing.T) {
	capture := &multiProviderUsageCapture{alias: t.Name(), records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_item.done\n" +
			"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{}\",\"status\":\"completed\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"muse-spark-1.3\",\"output\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":2,\"total_tokens\":6}}}\n\n"))
	}))
	defer server.Close()

	result, err := NewMetaExecutor(&config.Config{}).ExecuteStream(
		coreusage.WithRequestedModelAlias(context.Background(), t.Name()),
		&cliproxyauth.Auth{
			Provider: "meta",
			Attributes: map[string]string{
				"api_key":  "meta-token",
				"base_url": server.URL,
			},
		},
		cliproxyexecutor.Request{
			Model:   "muse-spark-1.3",
			Payload: []byte(`{"model":"muse-spark-1.3","messages":[{"role":"user","content":"hello"}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	record := capture.await(t)
	if record.Failed {
		t.Fatalf("failed = true, want false; failure=%+v", record.Fail)
	}
	if record.Detail.InputTokens != 4 || record.Detail.OutputTokens != 2 || record.Detail.TotalTokens != 6 {
		t.Fatalf("detail = %+v, want input/output/total = 4/2/6", record.Detail)
	}
	if record.Detail.ToolUsageDetails == nil || record.Detail.ToolUsageDetails.ToolCallsRequested == nil {
		t.Fatalf("detail = %+v, want ToolCallsRequested from response.output_item.done", record.Detail)
	}
	if got := *record.Detail.ToolUsageDetails.ToolCallsRequested; got != 1 {
		t.Fatalf("ToolCallsRequested = %d, want 1", got)
	}
}
