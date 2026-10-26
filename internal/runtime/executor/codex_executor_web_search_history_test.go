package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	_ "github.com/therealtinhtute/llmhub/internal/translator"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

// A compaction payload that replays hosted web searches: the assistant used
// web_search, the client dropped the tool declarations afterwards. Upstream
// rejects that combination ("response protection is unavailable") unless the
// replayed history is reconciled with the declared tools.
const codexReplayedWebSearchPayload = `{"model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"search docs"}]},{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"openai responses web search"}},{"id":"ws_2","type":"web_search_call","status":"completed","action":{"type":"open_page","url":"https://platform.openai.com/docs"}},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Found docs."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"Summarize conversation"}]}],"tools":[],"tool_choice":"auto"}`

func countCodexWebSearchTools(body []byte) int {
	count := 0
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("type").String() == "web_search" {
			count++
		}
	}
	return count
}

func hasCodexWebSearchCallInInput(body []byte) bool {
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "web_search_call" {
			return true
		}
	}
	return false
}

// newCodexWebSearchServer records the upstream body and answers with the
// upstream failure mode whenever a replayed search item arrives without a
// web_search tool declaration, so a passing run cannot come from a payload the
// fixture forgot to omit. /responses always answers as an event stream (the
// non-stream executor aggregates it), /responses/compact as JSON.
func newCodexWebSearchServer(got *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = body
		compact := r.URL.Path == "/responses/compact"
		protectionFailure := hasCodexWebSearchCallInInput(body) && countCodexWebSearchTools(body) == 0
		if protectionFailure {
			if compact {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":{"message":"response protection is unavailable","type":"internal_error","param":null,"code":null}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"error\",\"error\":{\"message\":\"response protection is unavailable\",\"type\":\"internal_error\",\"param\":null,\"code\":null}}\n\n"))
			return
		}
		if compact {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_compact_1","object":"response.compaction","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-6.1-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n"))
	}))
}

func drainCodexStream(t *testing.T, result *cliproxyexecutor.StreamResult) {
	t.Helper()
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
}

func TestCodexExecutorReplayedWebSearchCallHistoryEnsuresWebSearchTool(t *testing.T) {
	auth := func(baseURL string) *cliproxyauth.Auth {
		return &cliproxyauth.Auth{
			Provider: "codex",
			Attributes: map[string]string{
				"base_url":  baseURL,
				"api_key":   "test",
				"plan_type": "plus",
			},
		}
	}

	t.Run("ExecuteStream injects web_search so upstream response protection stays available", func(t *testing.T) {
		var gotBody []byte
		server := newCodexWebSearchServer(&gotBody)
		defer server.Close()

		executor := NewCodexExecutor(&config.Config{})
		result, err := executor.ExecuteStream(context.Background(), auth(server.URL), cliproxyexecutor.Request{
			Model:   "gpt-6.1-sol",
			Payload: []byte(codexReplayedWebSearchPayload),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
			Stream:       true,
		})
		if err != nil {
			t.Fatalf("ExecuteStream() error = %v", err)
		}
		drainCodexStream(t, result)
		if got := countCodexWebSearchTools(gotBody); got != 1 {
			t.Fatalf("web_search tool count = %d, want 1; body=%s", got, gotBody)
		}
		if !hasCodexWebSearchCallInInput(gotBody) {
			t.Fatalf("replayed web_search_call history must be kept for non-lite requests; body=%s", gotBody)
		}
	})

	t.Run("Execute keeps web_search when image generation is disabled", func(t *testing.T) {
		var gotBody []byte
		server := newCodexWebSearchServer(&gotBody)
		defer server.Close()

		executor := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
		if _, err := executor.Execute(context.Background(), auth(server.URL), cliproxyexecutor.Request{
			Model:   "gpt-6.1-sol",
			Payload: []byte(codexReplayedWebSearchPayload),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
		}); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if got := countCodexWebSearchTools(gotBody); got != 1 {
			t.Fatalf("web_search tool count = %d, want 1; body=%s", got, gotBody)
		}
		if got := len(gjson.GetBytes(gotBody, "tools").Array()); got != 1 {
			t.Fatalf("tools len = %d, want 1 (image generation disabled); body=%s", got, gotBody)
		}
	})

	t.Run("Execute on the compact endpoint injects web_search", func(t *testing.T) {
		var gotBody []byte
		server := newCodexWebSearchServer(&gotBody)
		defer server.Close()

		executor := NewCodexExecutor(&config.Config{})
		if _, err := executor.Execute(context.Background(), auth(server.URL), cliproxyexecutor.Request{
			Model:   "gpt-6.1-sol",
			Payload: []byte(`{"model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":"history"},{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"q"}},{"type":"compaction_trigger"}]}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
			Alt:          "responses/compact",
		}); err != nil {
			t.Fatalf("Execute compact error = %v", err)
		}
		if got := countCodexWebSearchTools(gotBody); got != 1 {
			t.Fatalf("compact web_search tool count = %d, want 1; body=%s", got, gotBody)
		}
	})

	t.Run("prepareCodexWebsocketStream injects web_search into the upstream body", func(t *testing.T) {
		wsExecutor := NewCodexWebsocketsExecutor(&config.Config{})
		prepared, err := wsExecutor.prepareCodexWebsocketStream(context.Background(), auth("http://127.0.0.1:1"), cliproxyexecutor.Request{
			Model:   "gpt-6.1-sol",
			Payload: []byte(codexReplayedWebSearchPayload),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
			Stream:       true,
		})
		if err != nil {
			t.Fatalf("prepareCodexWebsocketStream() error = %v", err)
		}
		if got := countCodexWebSearchTools(prepared.upstreamBody); got != 1 {
			t.Fatalf("websocket upstreamBody web_search tool count = %d, want 1; body=%s", got, prepared.upstreamBody)
		}
	})

	t.Run("responses-lite strips the replayed history instead of declaring the tool", func(t *testing.T) {
		var gotBody []byte
		server := newCodexWebSearchServer(&gotBody)
		defer server.Close()

		executor := NewCodexExecutor(&config.Config{})
		liteHeaders := http.Header{}
		liteHeaders.Set(codexResponsesLiteHeader, "true")
		if _, err := executor.Execute(context.Background(), auth(server.URL), cliproxyexecutor.Request{
			Model:   "gpt-6.1-sol",
			Payload: []byte(codexReplayedWebSearchPayload),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
			Headers:      liteHeaders,
		}); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if hasCodexWebSearchCallInInput(gotBody) {
			t.Fatalf("responses-lite request must not replay web_search_call items; body=%s", gotBody)
		}
		if got := countCodexWebSearchTools(gotBody); got != 0 {
			t.Fatalf("responses-lite web_search tool count = %d, want 0; body=%s", got, gotBody)
		}
		if got := gjson.GetBytes(gotBody, "parallel_tool_calls"); got.Type != gjson.False {
			t.Fatalf("responses-lite parallel_tool_calls = %v (%s), want false; body=%s", got.Value(), got.Type, gotBody)
		}
	})

	t.Run("an existing web_search declaration is preserved without duplication", func(t *testing.T) {
		var gotBody []byte
		server := newCodexWebSearchServer(&gotBody)
		defer server.Close()

		executor := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
		payload := `{"model":"gpt-6.1-sol","input":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"q"}}],"tools":[{"type":"web_search","external_web_access":true}]}`
		if _, err := executor.Execute(context.Background(), auth(server.URL), cliproxyexecutor.Request{
			Model:   "gpt-6.1-sol",
			Payload: []byte(payload),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
		}); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if got := countCodexWebSearchTools(gotBody); got != 1 {
			t.Fatalf("web_search tool count = %d, want 1; body=%s", got, gotBody)
		}
		if got := gjson.GetBytes(gotBody, "tools.0.external_web_access"); !got.Bool() {
			t.Fatalf("existing web_search declaration lost its settings; body=%s", gotBody)
		}
	})
}

// ensureWebSearchToolForHistory is the reconciliation step itself: it must be a
// no-op on payloads that need nothing, restore exactly one declaration when one
// is missing, leave an existing declaration alone (including legacy aliases),
// and converge when it runs twice — retries and restarts replay it.
func TestEnsureWebSearchToolForHistory(t *testing.T) {
	liteHeaders := func() http.Header {
		headers := http.Header{}
		headers.Set(codexResponsesLiteHeader, "true")
		return headers
	}

	t.Run("payload without replayed history is returned untouched", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"message","role":"user","content":"hi"}],"tools":[]}`)
		if got := ensureWebSearchToolForHistory(body, nil); string(got) != string(body) {
			t.Fatalf("body changed: %s", got)
		}
	})

	t.Run("non-array input is returned untouched", func(t *testing.T) {
		body := []byte(`{"input":"web_search_call","tools":[]}`)
		if got := ensureWebSearchToolForHistory(body, nil); string(got) != string(body) {
			t.Fatalf("body changed: %s", got)
		}
	})

	t.Run("missing tools array gains the declaration", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"web_search_call","id":"ws_1"}]}`)
		out := ensureWebSearchToolForHistory(body, nil)
		if got := countCodexWebSearchTools(out); got != 1 {
			t.Fatalf("web_search tool count = %d, want 1; body=%s", got, out)
		}
	})

	t.Run("empty tools array gains exactly one declaration", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"web_search_call","id":"ws_1"}],"tools":[]}`)
		out := ensureWebSearchToolForHistory(body, nil)
		if got := len(gjson.GetBytes(out, "tools").Array()); got != 1 {
			t.Fatalf("tools len = %d, want 1; body=%s", got, out)
		}
	})

	t.Run("existing declarations are preserved and the new one appended", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"web_search_call","id":"ws_1"}],"tools":[{"type":"function","name":"f"}]}`)
		out := ensureWebSearchToolForHistory(body, nil)
		if got := len(gjson.GetBytes(out, "tools").Array()); got != 2 {
			t.Fatalf("tools len = %d, want 2; body=%s", got, out)
		}
		if got := gjson.GetBytes(out, "tools.0.name").String(); got != "f" {
			t.Fatalf("existing tool reordered: %s", out)
		}
		if got := countCodexWebSearchTools(out); got != 1 {
			t.Fatalf("web_search tool count = %d, want 1; body=%s", got, out)
		}
	})

	t.Run("a legacy web_search_preview declaration counts as declared", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"web_search_call","id":"ws_1"}],"tools":[{"type":"web_search_preview"}]}`)
		out := ensureWebSearchToolForHistory(body, nil)
		if got := len(gjson.GetBytes(out, "tools").Array()); got != 1 {
			t.Fatalf("tools len = %d, want 1 (alias satisfies the requirement); body=%s", got, out)
		}
	})

	t.Run("responses-lite drops only the replayed items", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"message","role":"user","content":"a"},{"type":"web_search_call","id":"ws_1"},{"type":"message","role":"assistant","content":"b"}],"tools":[]}`)
		out := ensureWebSearchToolForHistory(body, liteHeaders())
		items := gjson.GetBytes(out, "input").Array()
		if len(items) != 2 {
			t.Fatalf("input len = %d, want 2; body=%s", len(items), out)
		}
		if items[0].Get("role").String() != "user" || items[1].Get("role").String() != "assistant" {
			t.Fatalf("non-search items must keep their order; body=%s", out)
		}
		if got := countCodexWebSearchTools(out); got != 0 {
			t.Fatalf("responses-lite must not declare hosted tools; body=%s", out)
		}
	})

	t.Run("running twice converges", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			body    string
			headers http.Header
		}{
			{name: "standard request", body: `{"input":[{"type":"web_search_call","id":"ws_1"}],"tools":[]}`},
			{name: "responses-lite request", body: `{"input":[{"type":"web_search_call","id":"ws_1"},{"type":"message","role":"user","content":"hi"}],"tools":[]}`, headers: liteHeaders()},
			{name: "already declared", body: `{"input":[{"type":"web_search_call","id":"ws_1"}],"tools":[{"type":"web_search"}]}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				once := ensureWebSearchToolForHistory([]byte(tc.body), tc.headers)
				twice := ensureWebSearchToolForHistory(once, tc.headers)
				if string(once) != string(twice) {
					t.Fatalf("second pass changed the payload: once=%s twice=%s", once, twice)
				}
				if got := countCodexWebSearchTools(twice); got > 1 {
					t.Fatalf("web_search tool count = %d, want at most 1; body=%s", got, twice)
				}
			})
		}
	})
}
