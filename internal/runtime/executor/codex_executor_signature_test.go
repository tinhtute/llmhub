package executor

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

// Ported from upstream CLIProxyAPI commit 81d6ba774621
// (internal/runtime/executor/codex_executor_signature_test.go).
// Fork shim: upstream's cliproxyauth.AttributeAuthKind / AuthKindAPIKey /
// AttributeConfigIndex / AuthSourceConfig constants are not ported; the fork
// uses the equivalent literal attribute/metadata keys.

func validCodexReasoningEncryptedContentForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func newCodexSignatureTestAuth(serverURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": serverURL,
		"api_key":  "test",
	}}
}

func TestCodexExecutorDropsInvalidReasoningEncryptedContentFromFinalRequest(t *testing.T) {
	validEncryptedContent := validCodexReasoningEncryptedContentForTest()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[` +
			`{"id":"rs_bad","type":"reasoning","encrypted_content":"gAAAAABqFTIa…abc","summary":[]},` +
			`{"id":"rs_non_string","type":"reasoning","encrypted_content":123,"summary":[]},` +
			`{"id":"rs_good","type":"reasoning","encrypted_content":"` + validEncryptedContent + `","summary":[]},` +
			`{"role":"user","content":"hello","encrypted_content":"leave-message-alone"}` +
			`]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid reasoning encrypted_content exists, want removed; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.0.id").Exists() {
		t.Fatalf("invalid reasoning id should be stripped under store=false default; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.1.encrypted_content").Exists() {
		t.Fatalf("non-string reasoning encrypted_content exists, want removed; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.1.id").Exists() {
		t.Fatalf("non-string reasoning id should be stripped under store=false default; body=%s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.2.encrypted_content").String(); got != validEncryptedContent {
		t.Fatalf("valid reasoning encrypted_content = %q, want preserved", got)
	}
	if got := gjson.GetBytes(gotBody, "input.3.encrypted_content").String(); got != "leave-message-alone" {
		t.Fatalf("non-reasoning encrypted_content = %q, want untouched", got)
	}
}

func TestCodexExecutorExecuteStreamDropsInvalidReasoningEncryptedContentFromFinalRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","stream":true,"input":[{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}
	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid stream reasoning encrypted_content exists, want removed; body=%s", string(gotBody))
	}
}

func TestCodexExecutorCompactDropsInvalidReasoningEncryptedContentFromFinalRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute compact error: %v", err)
	}
	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid compact reasoning encrypted_content exists, want removed; body=%s", string(gotBody))
	}
}

func TestCodexExecutorPreservesReasoningTextForCompatModelInResponsesRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "test-api-key",
				BaseURL: server.URL,
				Models: []config.CodexModel{
					{
						Name:     "DeepSeek-V4.1-Flash",
						Alias:    "deepseek-v4.1-flash",
						IsCompat: true,
					},
				},
			},
		},
	}
	executor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-api-key",
		},
	}

	payload := []byte(`{"model":"deepseek-v4.1-flash","input":[` +
		`{"id":"rs_prev_turn","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"DeepSeek reasoning CoT step 1"}]},` +
		`{"id":"call_1","type":"function_call","name":"tool_1","arguments":"{}"},` +
		`{"id":"output_1","type":"function_call_output","call_id":"call_1","output":"tool result"}` +
		`]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4.1-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	reasoningItem := gjson.GetBytes(gotBody, "input.0")
	if !reasoningItem.Exists() || reasoningItem.Get("type").String() != "reasoning" {
		t.Fatalf("expected reasoning item at input.0, got: %s", string(gotBody))
	}
	content := reasoningItem.Get("content")
	if !content.Exists() || !content.IsArray() || len(content.Array()) == 0 {
		t.Fatalf("expected reasoning.content to be preserved for is-compat model, but content was wiped: %s", string(gotBody))
	}
	if gotText := reasoningItem.Get("content.0.text").String(); gotText != "DeepSeek reasoning CoT step 1" {
		t.Fatalf("expected reasoning_text to be %q, got %q; body=%s", "DeepSeek reasoning CoT step 1", gotText, string(gotBody))
	}
}

func TestCodexExecutorExecuteStreamPreservesReasoningTextForCompatModelInResponsesRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "test-api-key",
				BaseURL: server.URL,
				Models: []config.CodexModel{
					{
						Name:     "DeepSeek-V4.1-Flash",
						Alias:    "deepseek-v4.1-flash",
						IsCompat: true,
					},
				},
			},
		},
	}
	executor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-api-key",
		},
	}

	payload := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"input":[` +
		`{"id":"rs_prev_turn","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"DeepSeek streaming CoT step 1"}]},` +
		`{"id":"call_1","type":"function_call","name":"tool_1","arguments":"{}"}` +
		`]}`)

	streamResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4.1-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range streamResult.Chunks {
	}

	reasoningItem := gjson.GetBytes(gotBody, "input.0")
	if !reasoningItem.Exists() || reasoningItem.Get("type").String() != "reasoning" {
		t.Fatalf("expected reasoning item at input.0, got: %s", string(gotBody))
	}
	content := reasoningItem.Get("content")
	if !content.Exists() || !content.IsArray() || len(content.Array()) == 0 {
		t.Fatalf("expected reasoning.content to be preserved for is-compat streaming model, but content was wiped: %s", string(gotBody))
	}
	if gotText := reasoningItem.Get("content.0.text").String(); gotText != "DeepSeek streaming CoT step 1" {
		t.Fatalf("expected reasoning_text to be %q, got %q; body=%s", "DeepSeek streaming CoT step 1", gotText, string(gotBody))
	}
}

func TestCodexExecutorCompactPreservesReasoningTextForCompatModelInResponsesRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "test-api-key",
				BaseURL: server.URL,
				Models: []config.CodexModel{
					{
						Name:     "DeepSeek-V4.1-Flash",
						Alias:    "deepseek-v4.1-flash",
						IsCompat: true,
					},
				},
			},
		},
	}
	executor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-api-key",
		},
	}

	payload := []byte(`{"model":"deepseek-v4.1-flash","input":[` +
		`{"id":"rs_prev_turn","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"DeepSeek compact CoT step 1"}]},` +
		`{"id":"call_1","type":"function_call","name":"tool_1","arguments":"{}"}` +
		`]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4.1-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute compact error: %v", err)
	}

	reasoningItem := gjson.GetBytes(gotBody, "input.0")
	if !reasoningItem.Exists() || reasoningItem.Get("type").String() != "reasoning" {
		t.Fatalf("expected reasoning item at input.0, got: %s", string(gotBody))
	}
	content := reasoningItem.Get("content")
	if !content.Exists() || !content.IsArray() || len(content.Array()) == 0 {
		t.Fatalf("expected reasoning.content to be preserved for is-compat compact model, but content was wiped: %s", string(gotBody))
	}
	if gotText := reasoningItem.Get("content.0.text").String(); gotText != "DeepSeek compact CoT step 1" {
		t.Fatalf("expected reasoning_text to be %q, got %q; body=%s", "DeepSeek compact CoT step 1", gotText, string(gotBody))
	}
}

func TestCodexExecutorHonorsAuthoritativeResolvedModelInfoIsCompatFalse(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "test-api-key",
				BaseURL: server.URL,
				Models: []config.CodexModel{
					{
						Name:     "gpt-5.4",
						Alias:    "gpt-5.4-alias",
						IsCompat: true, // Config says compat, but runtime metadata binds explicit false
					},
				},
			},
		},
	}
	executor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-api-key",
		},
	}

	payload := []byte(`{"model":"gpt-5.4","input":[` +
		`{"id":"rs_native","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"should be promoted and cleared"}]}` +
		`]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: payload,
		Metadata: map[string]any{
			"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: false},
		},
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	content := gjson.GetBytes(gotBody, "input.0.content")
	if !content.Exists() || !content.IsArray() || len(content.Array()) != 0 {
		t.Fatalf("content should be cleared to [] when authoritative IsCompat is false; body=%s", string(gotBody))
	}
	if gotSummary := gjson.GetBytes(gotBody, "input.0.summary.0.text").String(); gotSummary != "should be promoted and cleared" {
		t.Fatalf("summary should have promoted reasoning_text; body=%s", string(gotBody))
	}
}

func TestCodexExecutorDisambiguatesConfigIndex(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "shared-key",
				BaseURL: server.URL,
				Models: []config.CodexModel{
					{Name: "model-a", IsCompat: false},
				},
			},
			{
				APIKey:  "shared-key",
				BaseURL: server.URL,
				Models: []config.CodexModel{
					{Name: "model-a", IsCompat: true},
				},
			},
		},
	}
	executor := NewCodexExecutor(cfg)

	// Auth pointing to index 1 (IsCompat: true)
	authCompat := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind":    "apikey",
			"config_index": "1",
			"base_url":     server.URL,
			"api_key":      "shared-key",
		},
		Metadata: map[string]any{
			"auth_source": "config",
		},
	}

	payload := []byte(`{"model":"model-a","input":[` +
		`{"id":"rs_1","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"keep me"}]}` +
		`]}`)

	_, err := executor.Execute(context.Background(), authCompat, cliproxyexecutor.Request{
		Model:   "model-a",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	content := gjson.GetBytes(gotBody, "input.0.content")
	if !content.Exists() || !content.IsArray() || len(content.Array()) == 0 {
		t.Fatalf("expected index 1 (IsCompat:true) to preserve content; body=%s", string(gotBody))
	}
	if gotText := content.Get("0.text").String(); gotText != "keep me" {
		t.Fatalf("content text = %q, want keep me", gotText)
	}
}

// TestCodexExecutorExecute_ReplaysRoutedMetaSignatureForMuseTarget is the
// phase's production-path test: it drives the same entry point a real
// /v1/responses request takes (CodexExecutor.Execute → sanitizer → upstream HTTP
// request) and asserts what the upstream actually receives.
func TestCodexExecutorExecute_ReplaysRoutedMetaSignatureForMuseTarget(t *testing.T) {
	metaNative := validMetaReasoningEncryptedContentForTest()
	metaRouted := routedMetaReasoningEncryptedContentForTest()
	gptNative := validOpenAIResponsesReasoningEncryptedContentForTest()
	gptSuffix := strings.Repeat("Ab3", 27)[:79]
	gptRouted := gptNative + "." + gptSuffix

	t.Run("muse target keeps the signature and strips the OpenRouter suffix", func(t *testing.T) {
		var gotBody []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Fatalf("read body: %v", errRead)
			}
			gotBody = body
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
		}))
		defer server.Close()

		executor := NewCodexExecutor(&config.Config{})
		_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
			Model: "muse-spark-1.3",
			Payload: []byte(`{"model":"muse-spark-1.3","input":[` +
				`{"id":"rs_native","type":"reasoning","summary":[],"encrypted_content":"` + metaNative + `"},` +
				`{"id":"rs_routed","type":"reasoning","summary":[],"encrypted_content":"` + metaRouted + `"},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}` +
				`]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}

		if got := gjson.GetBytes(gotBody, "input.0.encrypted_content").String(); got != metaNative {
			t.Fatalf("native Muse signature = %q, want it replayed verbatim to the upstream; body=%s", got, gotBody)
		}
		// The upstream is not OpenRouter, so the private suffix is stripped down
		// to the native envelope instead of the signature being dropped whole.
		if got := gjson.GetBytes(gotBody, "input.1.encrypted_content").String(); got != metaNative {
			t.Fatalf("routed Muse signature = %q, want the native envelope after the router suffix strip; body=%s", got, gotBody)
		}
	})

	t.Run("official gpt target strips the router suffix and the cache prefix", func(t *testing.T) {
		var gotBody []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Fatalf("read body: %v", errRead)
			}
			gotBody = body
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
		}))
		defer server.Close()

		executor := NewCodexExecutor(&config.Config{})
		_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
			Model: "gpt-5.4",
			Payload: []byte(`{"model":"gpt-5.4","input":[` +
				`{"id":"rs_routed","type":"reasoning","summary":[],"encrypted_content":"gpt#` + gptRouted + `"},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}` +
				`]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}

		if got := gjson.GetBytes(gotBody, "input.0.encrypted_content").String(); got != gptNative {
			t.Fatalf("official Codex signature = %q, want the native envelope %q; body=%s", got, gptNative, gotBody)
		}
	})
}

// TestCodexExecutorExecute_StripsForeignSignatureForMetaTarget proves the live
// path drops a signature the Muse upstream cannot verify, instead of replaying
// it. It is the negative twin of the test above.
func TestCodexExecutorExecute_StripsForeignSignatureForMetaTarget(t *testing.T) {
	metaNative := validMetaReasoningEncryptedContentForTest()
	gptNative := validOpenAIResponsesReasoningEncryptedContentForTest()

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "muse-spark-1.3",
		Payload: []byte(`{"model":"muse-spark-1.3","input":[` +
			`{"type":"reasoning","summary":[],"encrypted_content":"` + gptNative + `"},` +
			`{"type":"reasoning","summary":[],"encrypted_content":"` + metaNative + `"}` +
			`]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("GPT signature should be stripped for a Meta target; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.1.encrypted_content").String(); got != metaNative {
		t.Fatalf("native Muse signature = %q, want it preserved; body=%s", got, gotBody)
	}
}
