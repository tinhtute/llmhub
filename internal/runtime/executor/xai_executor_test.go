package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	_ "github.com/therealtinhtute/llmhub/internal/translator"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIExecutionSessionIDUsesDerivedFallback(t *testing.T) {
	id := xaiExecutionSessionID(cliproxyexecutor.Request{}, cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:xai-root",
	}})
	if id == "" {
		t.Fatal("expected derived session UUID")
	}
	if repeated := xaiExecutionSessionID(cliproxyexecutor.Request{}, cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:xai-root",
	}}); repeated != id {
		t.Fatalf("derived session UUID is not stable: first=%q repeated=%q", id, repeated)
	}

	explicit := xaiExecutionSessionID(cliproxyexecutor.Request{}, cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "explicit-session",
		cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:xai-root",
	}})
	if explicit != "explicit-session" {
		t.Fatalf("explicit execution session = %q, want explicit-session", explicit)
	}
}

func TestXAIExecutorExecuteShapesResponsesRequest(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotGrokConvID string
	var gotOriginator string
	var gotAccountID string
	var gotBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotGrokConvID = r.Header.Get("x-grok-conv-id")
		gotOriginator = r.Header.Get("Originator")
		gotAccountID = r.Header.Get("Chatgpt-Account-Id")
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{XAI: config.XAIConfig{InjectXSearch: true}})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"access_token": "xai-token",
			"email":        "user@example.com",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"test"}],"content":null,"encrypted_content":null},{"type":"reasoning","summary":[{"type":"summary_text","text":"second"}]},{"role":"user","content":"hello"}],"include":["reasoning.encrypted_content"],"reasoning":{"effort":"high"},"tools":[{"type":"tool_search"},{"type":"image_generation"},{"type":"custom","name":"apply_patch"},{"type":"custom","name":"custom_lookup"},{"type":"function","name":"lookup"},{"type":"web_search","external_web_access":true,"search_content_types":["text","image"]},{"type":"namespace","name":"codex_app","description":"Tools in the codex_app namespace.","tools":[{"type":"function","name":"automation_update"},{"type":"custom","name":"namespace_custom"},{"type":"tool_search"}]}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"automation_update","namespace":"codex_app"},{"type":"function","name":"lookup"},{"type":"web_search"}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "conv-xai-1",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", gotPath)
	}
	if gotAuth != "Bearer xai-token" {
		t.Fatalf("Authorization = %q, want Bearer xai-token", gotAuth)
	}
	if gotGrokConvID != "conv-xai-1" {
		t.Fatalf("x-grok-conv-id = %q, want conv-xai-1", gotGrokConvID)
	}
	if gotOriginator != "" {
		t.Fatalf("Originator = %q, want empty", gotOriginator)
	}
	if gotAccountID != "" {
		t.Fatalf("Chatgpt-Account-Id = %q, want empty", gotAccountID)
	}
	if gjson.GetBytes(gotBody, "prompt_cache_key").String() != "conv-xai-1" {
		t.Fatalf("prompt_cache_key missing from body: %s", string(gotBody))
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("stream = false, want true; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "reasoning.effort").String() != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", gjson.GetBytes(gotBody, "reasoning.effort").String(), string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.0.content").Exists() {
		t.Fatalf("input.0.content exists, want removed; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("input.0.encrypted_content exists, want removed; body=%s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.0.summary.0.text").String(); got != "test" {
		t.Fatalf("input.0.summary.0.text = %q, want test; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.0.summary.1.text").String(); got != "second" {
		t.Fatalf("input.0.summary.1.text = %q, want second; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.1.role").String(); got != "user" {
		t.Fatalf("input.1.role = %q, want user; body=%s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.2").Exists() {
		t.Fatalf("input.2 exists, want consecutive reasoning item merged; body=%s", string(gotBody))
	}
	tools := gjson.GetBytes(gotBody, "tools").Array()
	if len(tools) != 6 {
		t.Fatalf("tools length = %d, want 6; body=%s", len(tools), string(gotBody))
	}
	foundAutomationUpdate := false
	foundNamespaceCustom := false
	foundXSearch := false
	for i, tool := range tools {
		toolType := tool.Get("type").String()
		if toolType == "image_generation" {
			t.Fatalf("tools.%d.type = image_generation, want removed; body=%s", i, string(gotBody))
		}
		if toolType != "function" && toolType != "web_search" && toolType != "x_search" {
			t.Fatalf("tools.%d.type = %q, want function, web_search, or x_search; body=%s", i, toolType, string(gotBody))
		}
		if toolType == "x_search" {
			foundXSearch = true
		}
		if toolType == "function" && !tool.Get("parameters").Exists() {
			t.Fatalf("tools.%d.parameters missing for xAI function tool; body=%s", i, string(gotBody))
		}
		if got := tool.Get("name").String(); got == "apply_patch" {
			t.Fatalf("tools.%d.name = apply_patch, want removed; body=%s", i, string(gotBody))
		}
		switch tool.Get("name").String() {
		case "codex_app__automation_update":
			foundAutomationUpdate = true
		case "codex_app__namespace_custom":
			foundNamespaceCustom = true
		}
		if toolType == "web_search" {
			if tool.Get("external_web_access").Exists() {
				t.Fatalf("tools.%d.external_web_access exists, want removed; body=%s", i, string(gotBody))
			}
			if got := tool.Get("search_content_types.1").String(); got != "image" {
				t.Fatalf("tools.%d.search_content_types missing image entry; body=%s", i, string(gotBody))
			}
		}
	}
	if !foundAutomationUpdate {
		t.Fatalf("namespace function tool was not moved to top-level tools; body=%s", string(gotBody))
	}
	if !foundNamespaceCustom {
		t.Fatalf("namespace custom tool was not moved to top-level tools; body=%s", string(gotBody))
	}
	if !foundXSearch {
		t.Fatalf("native x_search tool was not injected; body=%s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "tool_choice.tools.0.name").String(); got != "codex_app__automation_update" {
		t.Fatalf("tool_choice.tools.0.name = %q, want codex_app__automation_update; body=%s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "tool_choice.tools.0.namespace").Exists() {
		t.Fatalf("tool_choice.tools.0.namespace should be removed for xAI upstream: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "tool_choice.tools.1.name").String(); got != "lookup" {
		t.Fatalf("tool_choice.tools.1.name = %q, want lookup; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "tool_choice.tools.2.type").String(); got != "x_search" {
		t.Fatalf("tool_choice.tools.2.type = %q, want x_search; body=%s", got, string(gotBody))
	}
	for _, tool := range gjson.GetBytes(gotBody, "tool_choice.tools").Array() {
		if tool.Get("type").String() == "web_search" {
			t.Fatalf("web_search must not remain in allowed_tools: %s", string(gotBody))
		}
	}
	xSearchAllowedCount := 0
	for _, tool := range gjson.GetBytes(gotBody, "tool_choice.tools").Array() {
		if tool.Get("type").String() == "x_search" {
			xSearchAllowedCount++
		}
	}
	if xSearchAllowedCount != 1 {
		t.Fatalf("allowed_tools x_search count = %d, want 1; body=%s", xSearchAllowedCount, string(gotBody))
	}
	for _, include := range gjson.GetBytes(gotBody, "include").Array() {
		if include.String() == "reasoning.encrypted_content" {
			t.Fatalf("xai request must not ask for encrypted reasoning content: %s", string(gotBody))
		}
	}
}

func TestXAIExecutorOmitsUnsupportedReasoningEffort(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("xai-non-thinking-test", "xai", []*registry.ModelInfo{{ID: "grok-4", Type: "xai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient("xai-non-thinking-test") })

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4",
		Payload: []byte(`{"model":"grok-4","input":"hello","reasoning":{"effort":"high"}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gjson.GetBytes(gotBody, "reasoning").Exists() {
		t.Fatalf("unsupported xAI model must omit reasoning key: %s", string(gotBody))
	}
}

func TestXAIExecutorAppliesThinkingSuffix(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3(low)",
		Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(gotBody, "model").String(); got != "grok-4.3" {
		t.Fatalf("model = %q, want grok-4.3; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want low; body=%s", got, string(gotBody))
	}
}

func TestXAIExecutorExecuteStreamFiltersToolSearchTool(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"test"}],"content":null,"encrypted_content":null},{"type":"reasoning","summary":[{"type":"summary_text","text":"second"}]},{"role":"user","content":"hello"},{"type":"reasoning","summary":[{"type":"summary_text","text":"separate"}]}],"tools":[{"type":"tool_search"},{"type":"image_generation"},{"type":"custom","name":"apply_patch"},{"type":"custom","name":"custom_lookup"},{"type":"function","name":"lookup"},{"type":"web_search","external_web_access":true,"search_content_types":["text","image"]},{"type":"namespace","name":"codex_app","description":"Tools in the codex_app namespace.","tools":[{"type":"function","name":"automation_update"},{"type":"custom","name":"namespace_custom"},{"type":"tool_search"}]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	tools := gjson.GetBytes(gotBody, "tools").Array()
	if len(tools) != 5 {
		t.Fatalf("tools length = %d, want 5; body=%s", len(tools), string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.0.content").Exists() {
		t.Fatalf("input.0.content exists, want removed; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("input.0.encrypted_content exists, want removed; body=%s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.0.summary.0.text").String(); got != "test" {
		t.Fatalf("input.0.summary.0.text = %q, want test; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.0.summary.1.text").String(); got != "second" {
		t.Fatalf("input.0.summary.1.text = %q, want second; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.1.role").String(); got != "user" {
		t.Fatalf("input.1.role = %q, want user; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.2.summary.0.text").String(); got != "separate" {
		t.Fatalf("input.2.summary.0.text = %q, want separate; body=%s", got, string(gotBody))
	}
	foundAutomationUpdate := false
	foundNamespaceCustom := false
	for i, tool := range tools {
		toolType := tool.Get("type").String()
		if toolType == "image_generation" {
			t.Fatalf("tools.%d.type = image_generation, want removed; body=%s", i, string(gotBody))
		}
		if toolType != "function" && toolType != "web_search" {
			t.Fatalf("tools.%d.type = %q, want function or web_search; body=%s", i, toolType, string(gotBody))
		}
		if toolType == "function" && !tool.Get("parameters").Exists() {
			t.Fatalf("tools.%d.parameters missing for xAI function tool; body=%s", i, string(gotBody))
		}
		if got := tool.Get("name").String(); got == "apply_patch" {
			t.Fatalf("tools.%d.name = apply_patch, want removed; body=%s", i, string(gotBody))
		}
		switch tool.Get("name").String() {
		case "codex_app__automation_update":
			foundAutomationUpdate = true
		case "codex_app__namespace_custom":
			foundNamespaceCustom = true
		}
		if toolType == "web_search" {
			if tool.Get("external_web_access").Exists() {
				t.Fatalf("tools.%d.external_web_access exists, want removed; body=%s", i, string(gotBody))
			}
			if got := tool.Get("search_content_types.1").String(); got != "image" {
				t.Fatalf("tools.%d.search_content_types missing image entry; body=%s", i, string(gotBody))
			}
		}
	}
	if !foundAutomationUpdate {
		t.Fatalf("namespace function tool was not moved to top-level tools; body=%s", string(gotBody))
	}
	if !foundNamespaceCustom {
		t.Fatalf("namespace custom tool was not moved to top-level tools; body=%s", string(gotBody))
	}
}

func TestXAIExecutorAdditionalToolsNamespaceCustomToolDeclarationRoundTrip(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"grok-4.3","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"mcp__stable","arguments":"patch"}]}}` + "\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":[{"role":"user","content":"hello"},{"type":"additional_tools","tools":[{"type":"namespace","name":"plugin","tools":[{"type":"custom","name":"mcp__stable"}]},{"type":"function","name":"extra"}]}],"tools":[{"type":"namespace","name":"app","tools":[{"type":"function","name":"lookup"},{"type":"function","name":"app__ready"}]}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","namespace":"app","name":"lookup"},{"type":"custom","namespace":"plugin","name":"mcp__stable"}]}}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gjson.GetBytes(gotBody, `input.#(type=="additional_tools")`).Exists() {
		t.Fatalf("additional_tools input was not promoted: %s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, `tools.#(name=="app__lookup").name`).String(); got != "app__lookup" {
		t.Fatalf("namespaced function effective name = %q; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, `tools.#(name=="app__ready").name`).String(); got != "app__ready" {
		t.Fatalf("already-qualified name = %q, want app__ready; body=%s", got, gotBody)
	}
	if strings.Contains(string(gotBody), "app__app__ready") {
		t.Fatalf("already-qualified tool was double-qualified: %s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, `tools.#(name=="mcp__stable").name`).String(); got != "mcp__stable" {
		t.Fatalf("mcp name = %q, want byte-stable; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, `tools.#(name=="mcp__stable").type`).String(); got != "function" {
		t.Fatalf("promoted custom tool type = %q, want function; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, `tools.#(name=="extra").name`).String(); got != "extra" {
		t.Fatalf("promoted function missing; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tool_choice.tools.0.name").String(); got != "app__lookup" {
		t.Fatalf("function choice name = %q, want app__lookup; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tool_choice.tools.1.name").String(); got != "mcp__stable" {
		t.Fatalf("custom choice name = %q, want mcp__stable; body=%s", got, gotBody)
	}
	for index := range gjson.GetBytes(gotBody, "tool_choice.tools").Array() {
		if gjson.GetBytes(gotBody, fmt.Sprintf("tool_choice.tools.%d.namespace", index)).Exists() {
			t.Fatalf("choice %d retained namespace; body=%s", index, gotBody)
		}
		if got := gjson.GetBytes(gotBody, fmt.Sprintf("tool_choice.tools.%d.type", index)).String(); got != "function" {
			t.Fatalf("choice %d type = %q, want function; body=%s", index, got, gotBody)
		}
	}

	if got := gjson.GetBytes(resp.Payload, "output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("response tool type = %q, want custom_tool_call; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.namespace").String(); got != "plugin" {
		t.Fatalf("response namespace = %q, want plugin; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.name").String(); got != "mcp__stable" {
		t.Fatalf("response name = %q, want mcp__stable; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.input").String(); got != "patch" {
		t.Fatalf("response input = %q, want patch; payload=%s", got, resp.Payload)
	}
	if gjson.GetBytes(resp.Payload, "output.0.arguments").Exists() {
		t.Fatalf("custom response retained arguments: %s", resp.Payload)
	}
}

func TestXAIExecutorToolNameCollisionRejectedBeforeNetwork(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":[{"role":"user","content":"hello"},{"type":"additional_tools","tools":[{"type":"namespace","name":"team","tools":[{"type":"custom","name":"lookup"}]}]}],"tools":[{"type":"function","name":"team__lookup"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err == nil {
		t.Fatal("Execute() error = nil, want tool_name_collision")
	}
	if requestCount != 0 {
		t.Fatalf("network requests = %d, want 0", requestCount)
	}
	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok || statusProvider.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %v, want 400; err=%v", statusProvider, err)
	}
	if got := gjson.Get(err.Error(), "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error; err=%v", got, err)
	}
	if got := gjson.Get(err.Error(), "error.code").String(); got != "tool_name_collision" {
		t.Fatalf("error code = %q, want tool_name_collision; err=%v", got, err)
	}
}

func TestXAIExecutorXSearchLifecycleUsesExactDeclarationIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`event: response.output_item.added\ndata: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"internal_1","type":"custom_tool_call","call_id":"xs_call_internal","name":"x_keyword_search","input":"hidden"}}\n\n`,
			`event: response.custom_tool_call_input.delta\ndata: {"type":"response.custom_tool_call_input.delta","sequence_number":2,"output_index":0,"item_id":"internal_1","delta":"hidden"}\n\n`,
			`event: response.output_item.done\ndata: {"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"internal_1","type":"custom_tool_call","call_id":"xs_call_internal","name":"x_keyword_search","input":"hidden"}}\n\n`,
			`event: response.output_item.added\ndata: {"type":"response.output_item.added","sequence_number":4,"output_index":1,"item":{"id":"client_1","type":"function_call","call_id":"call_client","name":"search__x_keyword_search","arguments":""}}\n\n`,
			`event: response.function_call_arguments.delta\ndata: {"type":"response.function_call_arguments.delta","sequence_number":5,"output_index":1,"item_id":"client_1","delta":"{\"q\":\"x\"}"}\n\n`,
			`event: response.function_call_arguments.done\ndata: {"type":"response.function_call_arguments.done","sequence_number":6,"output_index":1,"item_id":"client_1","arguments":"{\"q\":\"x\"}"}\n\n`,
			`event: response.output_item.done\ndata: {"type":"response.output_item.done","sequence_number":7,"output_index":1,"item":{"id":"client_1","type":"function_call","call_id":"call_client","name":"search__x_keyword_search","arguments":"{\"q\":\"x\"}"}}\n\n`,
			`event: response.completed\ndata: {"type":"response.completed","sequence_number":8,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"grok-4.3","output":[{"id":"internal_1","type":"custom_tool_call","call_id":"xs_call_internal","name":"x_keyword_search","input":"hidden"},{"id":"client_1","type":"function_call","call_id":"call_client","name":"search__x_keyword_search","arguments":"{\"q\":\"x\"}"}]}}\n\n`,
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte(strings.ReplaceAll(frame, `\n`, "\n")))
		}
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":"hello","tools":[{"type":"namespace","name":"search","tools":[{"type":"custom","name":"x_keyword_search"}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var streamed bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	output := streamed.String()
	for _, hidden := range []string{"internal_1", "xs_call_internal", `"input":"hidden"`} {
		if strings.Contains(output, hidden) {
			t.Fatalf("stream retained internal x_search trace %q: %s", hidden, output)
		}
	}
	for _, want := range []string{
		`"id":"client_1"`,
		`"call_id":"call_client"`,
		`"namespace":"search"`,
		`"name":"x_keyword_search"`,
		`"type":"custom_tool_call"`,
		`"type":"response.custom_tool_call_input.delta"`,
		`"type":"response.custom_tool_call_input.done"`,
		`"input":"{\"q\":\"x\"}"`,
		`"output_index":0`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stream missing %q: %s", want, output)
		}
	}
	if strings.Contains(output, `"output_index":1`) {
		t.Fatalf("stream retained uncompact output index: %s", output)
	}
	completedMarker := `"type":"response.completed"`
	completedIndex := strings.LastIndex(output, completedMarker)
	if completedIndex < 0 {
		t.Fatalf("stream missing completed event: %s", output)
	}
	completed := output[completedIndex:]
	if strings.Count(completed, `"call_id":"call_client"`) != 1 || strings.Contains(completed, "xs_call_internal") {
		t.Fatalf("completed output lifecycle was not filtered exactly: %s", completed)
	}
}

func TestXAIExecutorSameNameXSearchCallIdentityFiltering(t *testing.T) {
	writeResponse := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`event: response.output_item.added\ndata: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"internal_same","type":"function_call","call_id":"xs_call_internal","name":"x_keyword_search","arguments":""}}\n\n`,
			`event: response.function_call_arguments.delta\ndata: {"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"item_id":"internal_same","delta":"internal"}\n\n`,
			`event: response.function_call_arguments.done\ndata: {"type":"response.function_call_arguments.done","sequence_number":3,"output_index":0,"item_id":"internal_same","arguments":"internal"}\n\n`,
			`event: response.output_item.done\ndata: {"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"id":"internal_same","type":"function_call","call_id":"xs_call_internal","name":"x_keyword_search","arguments":"internal"}}\n\n`,
			`event: response.output_item.added\ndata: {"type":"response.output_item.added","sequence_number":5,"output_index":1,"item":{"id":"client_same","type":"function_call","call_id":"call_client","name":"x_keyword_search","arguments":""}}\n\n`,
			`event: response.function_call_arguments.delta\ndata: {"type":"response.function_call_arguments.delta","sequence_number":6,"output_index":1,"item_id":"client_same","delta":"{\"q\":\"visible\"}"}\n\n`,
			`event: response.function_call_arguments.done\ndata: {"type":"response.function_call_arguments.done","sequence_number":7,"output_index":1,"item_id":"client_same","arguments":"{\"q\":\"visible\"}"}\n\n`,
			`event: response.output_item.done\ndata: {"type":"response.output_item.done","sequence_number":8,"output_index":1,"item":{"id":"client_same","type":"function_call","call_id":"call_client","name":"x_keyword_search","arguments":"{\"q\":\"visible\"}"}}\n\n`,
			`event: response.output_item.added\ndata: {"type":"response.output_item.added","sequence_number":9,"output_index":2,"item":{"id":"client_namespaced","type":"function_call","call_id":"xs_call_client","namespace":"search","name":"x_keyword_search","arguments":""}}\n\n`,
			`event: response.function_call_arguments.delta\ndata: {"type":"response.function_call_arguments.delta","sequence_number":10,"output_index":2,"item_id":"client_namespaced","delta":"{\"q\":\"namespaced\"}"}\n\n`,
			`event: response.function_call_arguments.done\ndata: {"type":"response.function_call_arguments.done","sequence_number":11,"output_index":2,"item_id":"client_namespaced","arguments":"{\"q\":\"namespaced\"}"}\n\n`,
			`event: response.output_item.done\ndata: {"type":"response.output_item.done","sequence_number":12,"output_index":2,"item":{"id":"client_namespaced","type":"function_call","call_id":"xs_call_client","namespace":"search","name":"x_keyword_search","arguments":"{\"q\":\"namespaced\"}"}}\n\n`,
			`event: response.output_item.added\ndata: {"type":"response.output_item.added","sequence_number":13,"output_index":3,"item":{"id":"message_1","type":"message","role":"assistant","content":[]}}\n\n`,
			`event: response.output_item.done\ndata: {"type":"response.output_item.done","sequence_number":14,"output_index":3,"item":{"id":"message_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}\n\n`,
			`event: response.completed\ndata: {"type":"response.completed","sequence_number":15,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"grok-4.3","output":[{"id":"internal_same","type":"function_call","call_id":"xs_call_internal","name":"x_keyword_search","arguments":"internal"},{"id":"client_same","type":"function_call","call_id":"call_client","name":"x_keyword_search","arguments":"{\"q\":\"visible\"}"},{"id":"client_namespaced","type":"function_call","call_id":"xs_call_client","namespace":"search","name":"x_keyword_search","arguments":"{\"q\":\"namespaced\"}"},{"id":"message_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}\n\n`,
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte(strings.ReplaceAll(frame, `\n`, "\n")))
		}
	}
	request := cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":"hello","tools":[{"type":"function","name":"x_keyword_search"},{"type":"namespace","name":"search","tools":[{"type":"function","name":"x_keyword_search"}]}]}`),
	}

	t.Run("streaming", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w)
		}))
		defer server.Close()
		auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "xai-token"}}
		result, err := NewXAIExecutor(&config.Config{}).ExecuteStream(context.Background(), auth, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
		if err != nil {
			t.Fatalf("ExecuteStream() error = %v", err)
		}
		var streamed bytes.Buffer
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
			streamed.Write(chunk.Payload)
		}
		output := streamed.String()
		for _, hidden := range []string{"internal_same", "xs_call_internal", `"delta":"internal"`, `"arguments":"internal"`} {
			if strings.Contains(output, hidden) {
				t.Fatalf("stream retained internal same-name lifecycle %q: %s", hidden, output)
			}
		}
		for _, visible := range []string{
			`"output_index":0,"item":{"id":"client_same"`,
			`"output_index":0,"item_id":"client_same"`,
			`"call_id":"call_client"`,
			`"type":"response.output_item.added","sequence_number":9,"output_index":1,"item":{"id":"client_namespaced"`,
			`"type":"response.function_call_arguments.delta","sequence_number":10,"output_index":1,"item_id":"client_namespaced"`,
			`"type":"response.function_call_arguments.done","sequence_number":11,"output_index":1,"item_id":"client_namespaced"`,
			`"type":"response.output_item.done","sequence_number":12,"output_index":1,"item":{"id":"client_namespaced"`,
			`"call_id":"xs_call_client"`,
			`"namespace":"search"`,
			`"output_index":2,"item":{"id":"message_1"`,
			`"type":"response.completed"`,
		} {
			if !strings.Contains(output, visible) {
				t.Fatalf("stream missing visible same-name lifecycle %q: %s", visible, output)
			}
		}
		if strings.Contains(output, `"output_index":3`) {
			t.Fatalf("stream retained uncompacted later index: %s", output)
		}
	})

	t.Run("nonstream", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w)
		}))
		defer server.Close()
		auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "xai-token"}}
		resp, err := NewXAIExecutor(&config.Config{}).Execute(context.Background(), auth, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if strings.Contains(string(resp.Payload), "internal_same") || strings.Contains(string(resp.Payload), "xs_call_internal") {
			t.Fatalf("nonstream output retained internal same-name item: %s", resp.Payload)
		}
		if got := len(gjson.GetBytes(resp.Payload, "output").Array()); got != 3 {
			t.Fatalf("nonstream output length = %d, want 3; payload=%s", got, resp.Payload)
		}
		if got := gjson.GetBytes(resp.Payload, "output.0.call_id").String(); got != "call_client" {
			t.Fatalf("nonstream legitimate call_id = %q, want call_client; payload=%s", got, resp.Payload)
		}
		if got := gjson.GetBytes(resp.Payload, "output.1.call_id").String(); got != "xs_call_client" {
			t.Fatalf("nonstream namespaced call_id = %q, want xs_call_client; payload=%s", got, resp.Payload)
		}
		if got := gjson.GetBytes(resp.Payload, "output.1.namespace").String(); got != "search" {
			t.Fatalf("nonstream namespaced identity = %q, want search; payload=%s", got, resp.Payload)
		}
		if got := gjson.GetBytes(resp.Payload, "output.2.id").String(); got != "message_1" {
			t.Fatalf("nonstream later visible item = %q, want message_1; payload=%s", got, resp.Payload)
		}
	})
}

func TestXAIExecutorExecuteStreamNormalizesReasoningTextEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[]}}\n\n"))
		_, _ = w.Write([]byte("event: response.content_part.added\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"reasoning_text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.reasoning_text.delta\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_text.delta\",\"sequence_number\":3,\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"thinking\"}\n\n"))
		_, _ = w.Write([]byte("event: response.reasoning_text.done\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_text.done\",\"sequence_number\":4,\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"text\":\"thinking\"}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.done\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"sequence_number\":5,\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"thinking\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"sequence_number\":6,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var streamed bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	output := streamed.String()
	if strings.Contains(output, "reasoning_text") {
		t.Fatalf("stream contains xAI reasoning_text shape: %s", output)
	}
	for _, want := range []string{
		"event: response.reasoning_summary_part.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
		"event: response.reasoning_summary_part.done",
		`"type":"response.reasoning_summary_part.added"`,
		`"type":"response.reasoning_summary_text.delta"`,
		`"type":"response.reasoning_summary_text.done"`,
		`"type":"response.reasoning_summary_part.done"`,
		`"part":{"type":"summary_text","text":"thinking"}`,
		`"summary_index":0`,
		`"summary":[{"type":"summary_text","text":"thinking"}]`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stream missing %q: %s", want, output)
		}
	}
	textDoneIndex := strings.Index(output, `"type":"response.reasoning_summary_text.done"`)
	partDoneIndex := strings.Index(output, `"type":"response.reasoning_summary_part.done"`)
	if textDoneIndex < 0 || partDoneIndex < 0 || textDoneIndex > partDoneIndex {
		t.Fatalf("reasoning done events are out of order: %s", output)
	}
}

func TestXAIExecutorExecuteNormalizesReasoningOutputForNonStreamTranslation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"thinking\"}]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := string(resp.Payload)
	if strings.Contains(output, "reasoning_text") {
		t.Fatalf("nonstream output contains xAI reasoning_text shape: %s", output)
	}
	if !strings.Contains(output, "thinking") {
		t.Fatalf("nonstream output missing reasoning summary text: %s", output)
	}
}

func TestXAIExecutorExecuteImagesUsesImagesEndpoint(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotAccept string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"AA=="}]}`))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-imagine-image",
		Payload: []byte(`{"model":"grok-imagine-image","prompt":"draw"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-image"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotPath != "/images/generations" {
		t.Fatalf("path = %q, want /images/generations", gotPath)
	}
	if gotAuth != "Bearer xai-token" {
		t.Fatalf("Authorization = %q, want Bearer xai-token", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if string(gotBody) != `{"model":"grok-imagine-image","prompt":"draw"}` {
		t.Fatalf("body = %s", string(gotBody))
	}
	if gjson.GetBytes(resp.Payload, "data.0.b64_json").String() != "AA==" {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestXAIExecutorExecuteImagesUsesEditsEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"url":"https://x.ai/image.png"}]}`))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-imagine-image",
		Payload: []byte(`{"model":"grok-imagine-image","prompt":"edit","image":{"type":"image_url","url":"https://example.com/a.png"}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-image"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/edits",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotPath != "/images/edits" {
		t.Fatalf("path = %q, want /images/edits", gotPath)
	}
}

func TestXAIExecutorExecuteVideosCreate(t *testing.T) {
	var gotPath string
	var gotMethod string
	var gotAuth string
	var gotIdempotencyKey string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotIdempotencyKey = r.Header.Get("x-idempotency-key")
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"vid_123"}`))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-imagine-video",
		Payload: []byte(`{"model":"grok-imagine-video","prompt":"animate","duration":4}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-video"),
		Metadata: map[string]any{
			"idempotency_key": "idem-123",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/videos/generations" {
		t.Fatalf("path = %q, want /videos/generations", gotPath)
	}
	if gotAuth != "Bearer xai-token" {
		t.Fatalf("Authorization = %q, want Bearer xai-token", gotAuth)
	}
	if gotIdempotencyKey != "idem-123" {
		t.Fatalf("x-idempotency-key = %q, want idem-123", gotIdempotencyKey)
	}
	if string(gotBody) != `{"model":"grok-imagine-video","prompt":"animate","duration":4}` {
		t.Fatalf("body = %s", string(gotBody))
	}
	if gjson.GetBytes(resp.Payload, "request_id").String() != "vid_123" {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestXAIExecutorExecuteVideosRetrieve(t *testing.T) {
	var gotPath string
	var gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4","duration":6},"model":"grok-imagine-video","progress":100}`))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-imagine-video",
		Payload: []byte(`{"request_id":"vid_123"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-video"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/videos/vid_123" {
		t.Fatalf("path = %q, want /videos/vid_123", gotPath)
	}
	if gjson.GetBytes(resp.Payload, "video.url").String() != "https://vidgen.x.ai/video.mp4" {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestXAIExecutorExecuteVideosUsesNativeEndpointFromRequestPath(t *testing.T) {
	tests := []struct {
		name        string
		requestPath string
		wantPath    string
	}{
		{
			name:        "generations",
			requestPath: "/v1/videos/generations",
			wantPath:    "/videos/generations",
		},
		{
			name:        "edits",
			requestPath: "/v1/videos/edits",
			wantPath:    "/videos/edits",
		},
		{
			name:        "extensions",
			requestPath: "/v1/videos/extensions",
			wantPath:    "/videos/extensions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			var gotMethod string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"request_id":"vid_123"}`))
			}))
			defer server.Close()

			exec := NewXAIExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				Provider:   "xai",
				Attributes: map[string]string{"base_url": server.URL},
				Metadata:   map[string]any{"access_token": "xai-token"},
			}

			_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "grok-imagine-video",
				Payload: []byte(`{"model":"grok-imagine-video","prompt":"animate"}`),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-video"),
				Metadata: map[string]any{
					cliproxyexecutor.RequestPathMetadataKey: tt.requestPath,
				},
			})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			if gotMethod != http.MethodPost {
				t.Fatalf("method = %q, want POST", gotMethod)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("path = %q, want %s", gotPath, tt.wantPath)
			}
		})
	}
}

func TestXAIExecutorExecuteAcceptsResponseIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":1,\"total_tokens\":9}}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"access_token": "xai-token",
		},
	}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: []byte(`{"model":"grok-4.5","input":"hi","max_output_tokens":1}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete reason = %q, want max_output_tokens; payload=%s", got, resp.Payload)
	}
}

func TestXAIExecutorExecuteStreamForwardsResponseIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":1,\"total_tokens\":9}}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"access_token": "xai-token",
		},
	}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: []byte(`{"model":"grok-4.5","input":"hi","stream":true,"max_output_tokens":1}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
}

func TestXAISupportsNativeImageGeneration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  bool
	}{
		{model: "", want: false},
		{model: "grok-4.5", want: false},
		{model: "grok-4.3", want: false},
		{model: "grok-4", want: false},
		{model: "grok-4.20-0309-reasoning", want: false},
		{model: "grok-4.20-multi-agent-0309", want: false},
		{model: "grok-build-0.1", want: false},
		{model: "grok-composer-2.5-fast", want: false},
		{model: "grok-3-mini", want: false},
		{model: "gpt-5.6", want: false},
		{model: "grok-4.6", want: true},
		{model: "grok-4.6(high)", want: true},
		{model: "xai/grok-4.6", want: true},
		{model: "grok-4.7", want: true},
		{model: "grok-5", want: true},
		{model: "grok-5.0", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			if got := xaiSupportsNativeImageGeneration(tt.model); got != tt.want {
				t.Fatalf("xaiSupportsNativeImageGeneration(%q) = %t, want %t", tt.model, got, tt.want)
			}
		})
	}
}

func TestNormalizeXAITools_ImageGenerationByModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       []byte
		wantKeep   bool
		wantAction string
	}{
		{
			name:     "missing model still strips",
			body:     []byte(`{"tools":[{"type":"image_generation"},{"type":"web_search"}]}`),
			wantKeep: false,
		},
		{
			name:     "grok-4.5 strips",
			body:     []byte(`{"model":"grok-4.5","tools":[{"type":"image_generation"},{"type":"web_search"}]}`),
			wantKeep: false,
		},
		{
			name:     "grok-4.20 strips despite larger minor",
			body:     []byte(`{"model":"grok-4.20-0309-reasoning","tools":[{"type":"image_generation"},{"type":"web_search"}]}`),
			wantKeep: false,
		},
		{
			name:       "grok-4.6 keeps action",
			body:       []byte(`{"model":"grok-4.6","tools":[{"type":"image_generation","action":"generate"},{"type":"web_search"}]}`),
			wantKeep:   true,
			wantAction: "generate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := normalizeXAITools(tt.body)
			tools := gjson.GetBytes(out, "tools").Array()
			foundImage := false
			foundWebSearch := false
			var imageTool gjson.Result
			for _, tool := range tools {
				switch tool.Get("type").String() {
				case "image_generation":
					foundImage = true
					imageTool = tool
				case "web_search":
					foundWebSearch = true
				}
			}
			if !foundWebSearch {
				t.Fatalf("web_search missing; body=%s", out)
			}
			if foundImage != tt.wantKeep {
				t.Fatalf("image_generation kept=%t, want %t; body=%s", foundImage, tt.wantKeep, out)
			}
			if tt.wantKeep && tt.wantAction != "" {
				if got := imageTool.Get("action").String(); got != tt.wantAction {
					t.Fatalf("image_generation.action = %q, want %q; body=%s", got, tt.wantAction, out)
				}
			}
		})
	}
}

func TestXAIExecutorPrepareKeepsNativeImageGenerationForGrok46(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"draw a red circle",
			"tools":[{"type":"image_generation","action":"generate"}],
			"tool_choice":{"type":"image_generation"}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation; body=%s", got, prepared.body)
	}
	if got := tools[0].Get("action").String(); got != "generate" {
		t.Fatalf("tools.0.action = %q, want generate; body=%s", got, prepared.body)
	}
	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "required" {
		t.Fatalf("tool_choice = %s, want string required; body=%s", choice.Raw, prepared.body)
	}
}

func TestXAIExecutorPrepareRewritesImageGenerationAllowedToolsToRequired(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"draw a red circle",
			"tools":[{"type":"image_generation","action":"generate"},{"type":"web_search"}],
			"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"image_generation"}]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "required" {
		t.Fatalf("tool_choice = %s, want string required; body=%s", choice.Raw, prepared.body)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation; body=%s", got, prepared.body)
	}
}

func TestXAIExecutorPrepareRewritesImageOnlyAllowedToolsAutoToAuto(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"draw a red circle",
			"tools":[{"type":"image_generation"},{"type":"web_search"}],
			"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"image_generation"}]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "auto" {
		t.Fatalf("tool_choice = %s, want string auto; body=%s", choice.Raw, prepared.body)
	}
}

func TestXAIExecutorPrepareStripsImageGenerationFromMixedAllowedTools(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"draw or search",
			"tools":[{"type":"image_generation"},{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],
			"tool_choice":{"type":"allowed_tools","mode":"required","tools":[
				{"type":"image_generation"},
				{"type":"function","name":"lookup"}
			]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if got := choice.Get("type").String(); got != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q, want allowed_tools; body=%s", got, prepared.body)
	}
	allowed := choice.Get("tools").Array()
	if len(allowed) != 1 {
		t.Fatalf("tool_choice.tools length = %d, want 1; body=%s", len(allowed), prepared.body)
	}
	if got := allowed[0].Get("name").String(); got != "lookup" {
		t.Fatalf("tool_choice.tools.0.name = %q, want lookup; body=%s", got, prepared.body)
	}
	for _, tool := range allowed {
		if tool.Get("type").String() == "image_generation" {
			t.Fatalf("image_generation must not remain in allowed_tools: %s", prepared.body)
		}
	}
}

// Ported from upstream c616193a (xai_executor_test.go): Claude's web_search tool
// choice must normalize to the string "required" form with only the hosted tool.
func TestXAIExecutorPrepareNormalizesClaudeWebSearchToolChoice(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.5",
		Payload: []byte(`{
			"model":"grok-4.5",
			"max_tokens":4096,
			"stream":true,
			"output_config":{"effort":"high"},
			"thinking":{"type":"disabled"},
			"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search"}]}],
			"tool_choice":{"type":"tool","name":"web_search"},
			"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Stream:       true,
	}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "required" {
		t.Fatalf("tool_choice = %s, want string required; body=%s", choice.Raw, prepared.body)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search; body=%s", got, prepared.body)
	}
}

func TestXAIExecutorPrepareNormalizesClaudeWebSearchToolChoice_Grok46(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"max_tokens":64000,
			"stream":true,
			"output_config":{"effort":"high"},
			"thinking":{"type":"disabled"},
			"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search"}]}],
			"tool_choice":{"type":"tool","name":"web_search"},
			"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8,"allowed_domains":["github.com"]}]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Stream:       true,
	}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "required" {
		t.Fatalf("tool_choice = %s, want string required; body=%s", choice.Raw, prepared.body)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search; body=%s", got, prepared.body)
	}
	if got := tools[0].Get("filters.allowed_domains.0").String(); got != "github.com" {
		t.Fatalf("tools.0.filters.allowed_domains.0 = %q, want github.com; body=%s", got, prepared.body)
	}
}

func TestXAIExecutorPrepareStripsUnsupportedWebSearchOptions(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.3",
		Payload: []byte(`{
			"model":"grok-4.3",
			"input":[{"role":"user","content":"hello"}],
			"tools":[
				{"type":"web_search","external_web_access":true,"search_context_size":"high","search_content_types":["text","image"],"user_location":{"type":"approximate","country":"NL"},"filters":{"allowed_domains":["github.com"]}},
				{"type":"namespace","name":"research","description":"Research tools.","tools":[{"type":"web_search","search_context_size":"low"}]}
			]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
	}, false)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}

	webSearchTools := 0
	for i, tool := range gjson.GetBytes(prepared.body, "tools").Array() {
		if tool.Get("type").String() != "web_search" {
			continue
		}
		webSearchTools++
		for _, field := range []string{"external_web_access", "search_context_size"} {
			if tool.Get(field).Exists() {
				t.Fatalf("tools.%d.%s exists, want removed; body=%s", i, field, prepared.body)
			}
		}
		if webSearchTools > 1 {
			continue
		}
		if got := tool.Get("search_content_types.1").String(); got != "image" {
			t.Fatalf("tools.%d.search_content_types.1 = %q, want image; body=%s", i, got, prepared.body)
		}
		if got := tool.Get("user_location.country").String(); got != "NL" {
			t.Fatalf("tools.%d.user_location.country = %q, want NL; body=%s", i, got, prepared.body)
		}
		if got := tool.Get("filters.allowed_domains.0").String(); got != "github.com" {
			t.Fatalf("tools.%d.filters.allowed_domains.0 = %q, want github.com; body=%s", i, got, prepared.body)
		}
	}
	if webSearchTools == 0 {
		t.Fatalf("web_search tool missing from prepared request; body=%s", prepared.body)
	}
}

func TestXAIExecutorPrepareRewritesWebSearchAllowedToolsToRequired(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"search the web",
			"tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],
			"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"}]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "required" {
		t.Fatalf("tool_choice = %s, want string required; body=%s", choice.Raw, prepared.body)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search; body=%s", got, prepared.body)
	}
}

func TestXAIExecutorPrepareForcedWebSearchDropsOtherToolsAndSkipsXSearchInject(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{XAI: config.XAIConfig{InjectXSearch: true}})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"search the web",
			"tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],
			"tool_choice":{"type":"web_search"}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "required" {
		t.Fatalf("tool_choice = %s, want string required; body=%s", choice.Raw, prepared.body)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search; body=%s", got, prepared.body)
	}
}

func TestXAIExecutorPrepareRewritesWebSearchOnlyAllowedToolsAutoToAuto(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"search the web",
			"tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],
			"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "auto" {
		t.Fatalf("tool_choice = %s, want string auto; body=%s", choice.Raw, prepared.body)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search; body=%s", got, prepared.body)
	}
}

func TestXAIExecutorPrepareStripsWebSearchFromMixedAllowedTools(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"draw or search",
			"tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],
			"tool_choice":{"type":"allowed_tools","mode":"required","tools":[
				{"type":"web_search"},
				{"type":"function","name":"lookup"}
			]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if got := choice.Get("type").String(); got != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q, want allowed_tools; body=%s", got, prepared.body)
	}
	allowed := choice.Get("tools").Array()
	if len(allowed) != 1 {
		t.Fatalf("tool_choice.tools length = %d, want 1; body=%s", len(allowed), prepared.body)
	}
	if got := allowed[0].Get("name").String(); got != "lookup" {
		t.Fatalf("tool_choice.tools.0.name = %q, want lookup; body=%s", got, prepared.body)
	}
	for _, tool := range allowed {
		if tool.Get("type").String() == "web_search" {
			t.Fatalf("web_search must not remain in allowed_tools: %s", prepared.body)
		}
	}
}

func TestXAIExecutorPrepareForcedImageGenerationDropsOtherToolsAndSkipsXSearchInject(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{XAI: config.XAIConfig{InjectXSearch: true}})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"draw a red circle",
			"tools":[{"type":"image_generation","action":"generate"},{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],
			"tool_choice":{"type":"image_generation"}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	choice := gjson.GetBytes(prepared.body, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "required" {
		t.Fatalf("tool_choice = %s, want string required; body=%s", choice.Raw, prepared.body)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation; body=%s", got, prepared.body)
	}
}

// Compact-shaped requests funnel through the same preparation path locally, so
// an orphaned tool_choice must be dropped there once normalization strips the
// referenced hosted tool (87fb01b23788).
func TestXAIExecutorPrepareDropsOrphanedImageGenerationToolChoice(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.5",
		Payload: []byte(`{
			"model":"grok-4.5",
			"input":"compact this",
			"tools":[{"type":"image_generation","action":"generate"}],
			"tool_choice":{"type":"image_generation"},
			"parallel_tool_calls":true
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	}, false)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}
	if gjson.GetBytes(prepared.body, "tools").Exists() {
		t.Fatalf("tools exists in prepared body: %s", prepared.body)
	}
	if gjson.GetBytes(prepared.body, "tool_choice").Exists() {
		t.Fatalf("orphaned tool_choice leaked into prepared body: %s", prepared.body)
	}
	if gjson.GetBytes(prepared.body, "parallel_tool_calls").Exists() {
		t.Fatalf("parallel_tool_calls exists in prepared body: %s", prepared.body)
	}
}
func TestXAIExecutorExecuteFoldsNamespacesWhenToolsExceed200(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"name\":\"mcp__app_0\",\"call_id\":\"call_1\",\"arguments\":\"{\\\"name\\\":\\\"tool_2\\\",\\\"arguments\\\":{\\\"q\\\":\\\"test\\\"}}\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"grok-4.6\",\"output\":[{\"type\":\"function_call\",\"name\":\"mcp__app_0\",\"call_id\":\"call_1\",\"arguments\":\"{\\\"name\\\":\\\"tool_2\\\",\\\"arguments\\\":{\\\"q\\\":\\\"test\\\"}}\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	var nsList []string
	for i := 0; i < 47; i++ {
		var childTools []string
		for j := 0; j < 10; j++ {
			childTools = append(childTools, fmt.Sprintf(`{"type":"function","name":"tool_%d","description":"child tool %d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}`, j, j))
		}
		nsList = append(nsList, fmt.Sprintf(`{"type":"namespace","name":"mcp__app_%d","description":"App %d tools","tools":[%s]}`, i, i, strings.Join(childTools, ",")))
	}

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	turn1Payload := fmt.Sprintf(`{
		"model":"grok-4.6",
		"tools":[%s],
		"input":[{"role":"user","content":"call tool_2"}]
	}`, strings.Join(nsList, ","))

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.6",
		Payload: []byte(turn1Payload),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	tools := gjson.GetBytes(gotBody, "tools").Array()
	if len(tools) != 47 {
		t.Fatalf("upstream tools count = %d, want 47 dispatcher tools; body=%s", len(tools), string(gotBody))
	}
	if got := tools[0].Get("name").String(); got != "mcp__app_0" {
		t.Fatalf("upstream tools[0].name = %q, want mcp__app_0", got)
	}
	if got := tools[0].Get("type").String(); got != "function" {
		t.Fatalf("upstream tools[0].type = %q, want function", got)
	}

	output := gjson.GetBytes(resp.Payload, "output.0")
	if got := output.Get("name").String(); got != "tool_2" {
		t.Fatalf("response output name = %q, want tool_2; payload=%s", got, resp.Payload)
	}
	if got := output.Get("namespace").String(); got != "mcp__app_0" {
		t.Fatalf("response output namespace = %q, want mcp__app_0; payload=%s", got, resp.Payload)
	}
	if got := output.Get("arguments").String(); got != `{"q":"test"}` {
		t.Fatalf("response output arguments = %q, want {\"q\":\"test\"}; payload=%s", got, resp.Payload)
	}
}

func TestNormalizeXAITools_InlinesLocalRefs(t *testing.T) {
	body := []byte(`{
		"tools":[
			{
				"type":"function",
				"name":"query_user",
				"strict":true,
				"parameters":{
					"type":"object",
					"properties":{
						"user":{"$ref":"#/$defs/User"}
					},
					"required":["user"],
					"$defs":{
						"User":{
							"type":"object",
							"properties":{
								"name":{"type":"string"},
								"age":{"type":"integer"}
							},
							"required":["name"]
						}
					}
				}
			},
			{
				"type":"function",
				"name":"render_shape",
				"strict":true,
				"parameters":{
					"type":"object",
					"oneOf":[
						{"$ref":"#/$defs/Circle"},
						{"$ref":"#/$defs/Square"}
					],
					"$defs":{
						"Circle":{"type":"object","properties":{"radius":{"type":"number"}},"required":["radius"]},
						"Square":{"type":"object","properties":{"side":{"type":"number"}},"required":["side"]}
					}
				}
			}
		]
	}`)
	out := normalizeXAITools(body)

	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools length = %d, want 2; body=%s", len(tools), string(out))
	}

	userTool := tools[0]
	if got := userTool.Get("parameters.properties.user.properties.name.type").String(); got != "string" {
		t.Fatalf("user.name.type = %q, want string; tool=%s", got, userTool.Raw)
	}
	if got := userTool.Get("parameters.properties.user.properties.age.type").String(); got != "integer" {
		t.Fatalf("user.age.type = %q, want integer; tool=%s", got, userTool.Raw)
	}
	if userTool.Get("parameters.$defs").Exists() {
		t.Fatalf("$defs should be removed after inlining: %s", userTool.Raw)
	}

	shapeTool := tools[1]
	if shapeTool.Get("parameters.oneOf.#").Int() != 2 {
		t.Fatalf("oneOf length = %d, want 2; tool=%s", shapeTool.Get("parameters.oneOf.#").Int(), shapeTool.Raw)
	}
	if got := shapeTool.Get("parameters.oneOf.0.properties.radius.type").String(); got != "number" {
		t.Fatalf("oneOf.0.radius.type = %q, want number; tool=%s", got, shapeTool.Raw)
	}
	if got := shapeTool.Get("parameters.oneOf.1.properties.side.type").String(); got != "number" {
		t.Fatalf("oneOf.1.side.type = %q, want number; tool=%s", got, shapeTool.Raw)
	}
	if shapeTool.Get("parameters.$defs").Exists() {
		t.Fatalf("$defs should be removed after inlining: %s", shapeTool.Raw)
	}
}

func TestXAIExecutorExecuteImagesOAuthBaseURLResolution(t *testing.T) {
	tests := []struct {
		name    string
		auth    *cliproxyauth.Auth
		wantURL string
	}{
		{
			name: "oauth defaults to cli chat proxy",
			auth: &cliproxyauth.Auth{
				Metadata: map[string]any{"auth_kind": "oauth"},
			},
			wantURL: "https://cli-chat-proxy.grok.com/v1/images/generations",
		},
		{
			name: "api key defaults to official api",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"api_key": "xai-test"},
			},
			wantURL: "https://api.x.ai/v1/images/generations",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseURL := xaiChatBaseURL(tt.auth)
			url := strings.TrimSuffix(baseURL, "/") + xaiDefaultImageEndpointPath
			if url != tt.wantURL {
				t.Fatalf("xaiChatBaseURL() = %q, want %q", url, tt.wantURL)
			}
		})
	}
}

// Ported from upstream 660a5800 (xai_executor_test.go), adapted to the local
// monolith: prepared.filterInternalXSearch is expressed as
// xaiRequestHasNativeXSearch(prepared.body).
func TestXAIExecutorPrepareHonorsInjectXSearchConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         *config.Config
		wantXSearch bool
	}{
		{name: "default disabled", cfg: &config.Config{}, wantXSearch: false},
		{name: "explicitly enabled", cfg: &config.Config{XAI: config.XAIConfig{InjectXSearch: true}}, wantXSearch: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			exec := NewXAIExecutor(tt.cfg)
			prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
				Model: "grok-4.5",
				Payload: []byte(`{
					"model":"grok-4.5",
					"input":"search the web",
					"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}],
					"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"web_search"}]}
				}`),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatOpenAIResponse,
				Stream:       false,
			}, false)
			if errPrepare != nil {
				t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
			}

			wantXSearchCount := 0
			if tt.wantXSearch {
				wantXSearchCount = 1
			}
			tools := gjson.GetBytes(prepared.body, "tools").Array()
			if len(tools) != 1+wantXSearchCount {
				t.Fatalf("tools length = %d, want %d; body=%s", len(tools), 1+wantXSearchCount, prepared.body)
			}
			if got := tools[0].Get("name").String(); got != "clientfn_web_search" {
				t.Fatalf("client web_search tool missing or not aliased; body=%s", prepared.body)
			}
			xSearchTools := 0
			for _, tool := range tools {
				if tool.Get("type").String() == "x_search" {
					xSearchTools++
				}
			}
			if xSearchTools != wantXSearchCount {
				t.Fatalf("x_search tools = %d, want %d; body=%s", xSearchTools, wantXSearchCount, prepared.body)
			}

			xSearchAllowed := 0
			for _, tool := range gjson.GetBytes(prepared.body, "tool_choice.tools").Array() {
				if tool.Get("type").String() == "x_search" {
					xSearchAllowed++
				}
			}
			if xSearchAllowed != wantXSearchCount {
				t.Fatalf("allowed x_search tools = %d, want %d; body=%s", xSearchAllowed, wantXSearchCount, prepared.body)
			}
			if got := xaiRequestHasNativeXSearch(prepared.body); got != tt.wantXSearch {
				t.Fatalf("xaiRequestHasNativeXSearch(body) = %t, want %t", got, tt.wantXSearch)
			}
		})
	}
}

func TestXAIExecutorAliasesClientWebSearchFunctionInRequest(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"search ranking"}]},
				{"type":"function_call","name":"web_search","call_id":"call_1","arguments":"{\"query\":\"ranking\"}"}
			],
			"tools":[
				{"type":"function","name":"web_search","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}},
				{"type":"function","name":"read","parameters":{"type":"object"}}
			],
			"tool_choice":{"type":"function","name":"web_search"}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}

	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools length = %d, want 2; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("name").String(); got != "clientfn_web_search" {
		t.Fatalf("tools.0.name = %q, want clientfn_web_search; body=%s", got, prepared.body)
	}
	if got := tools[1].Get("name").String(); got != "read" {
		t.Fatalf("tools.1.name = %q, want read; body=%s", got, prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "tool_choice.name").String(); got != "clientfn_web_search" {
		t.Fatalf("tool_choice.name = %q, want clientfn_web_search; body=%s", got, prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "input.1.name").String(); got != "clientfn_web_search" {
		t.Fatalf("input.1.name = %q, want clientfn_web_search; body=%s", got, prepared.body)
	}

	// Also verify allowed_tools mode
	preparedAllowed, errAllowed := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}],
			"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"web_search"}]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	}, true)
	if errAllowed != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errAllowed)
	}
	if got := gjson.GetBytes(preparedAllowed.body, "tool_choice.tools.0.name").String(); got != "clientfn_web_search" {
		t.Fatalf("tool_choice.tools.0.name = %q, want clientfn_web_search; body=%s", got, preparedAllowed.body)
	}
}

func TestXAIExecutorPreservesHostedWebSearchToolType(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"input":"search ranking",
			"tools":[
				{"type":"web_search"}
			]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}

	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search; body=%s", got, prepared.body)
	}
	if tools[0].Get("name").Exists() {
		t.Fatalf("tools.0.name should not exist for hosted tool; body=%s", prepared.body)
	}
}

func TestXAIExecutorRestoresAliasedWebSearchInStreamAndExecute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"call_1\",\"type\":\"function_call\",\"name\":\"clientfn_web_search\",\"arguments\":\"\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"call_1\",\"type\":\"function_call\",\"name\":\"clientfn_web_search\",\"arguments\":\"{\\\"query\\\":\\\"golang\\\"}\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"id\":\"call_1\",\"type\":\"function_call\",\"name\":\"clientfn_web_search\",\"arguments\":\"{\\\"query\\\":\\\"golang\\\"}\"}]}}\n\n")
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	// 1. Verify streaming restoration
	streamRes, errStream := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}],
			"input":"search query"
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}

	var streamOutput bytes.Buffer
	for chunk := range streamRes.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamOutput.Write(chunk.Payload)
		streamOutput.WriteByte('\n')
	}
	streamText := streamOutput.String()
	if strings.Contains(streamText, "clientfn_web_search") {
		t.Fatalf("clientfn_web_search leaked into client stream: %s", streamText)
	}
	if !strings.Contains(streamText, `"name":"web_search"`) {
		t.Fatalf("restored web_search missing from client stream: %s", streamText)
	}

	// 2. Verify non-streaming restoration
	execRes, errExec := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}],
			"input":"search query"
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	})
	if errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	if strings.Contains(string(execRes.Payload), "clientfn_web_search") {
		t.Fatalf("clientfn_web_search leaked into non-stream response: %s", execRes.Payload)
	}
	if got := gjson.GetBytes(execRes.Payload, "output.0.name").String(); got != "web_search" {
		t.Fatalf("non-stream output.0.name = %q, want web_search; payload=%s", got, execRes.Payload)
	}
}

func TestXAIExecutorAliasesClientWebSearchWithExistingAliasCollision(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"tools":[
				{"type":"function","name":"clientfn_web_search","parameters":{"type":"object"}},
				{"type":"function","name":"web_search","parameters":{"type":"object"}}
			],
			"input":[
				{"type":"function_call","name":"clientfn_web_search","call_id":"call_1","arguments":"{}"},
				{"type":"function_call","name":"web_search","call_id":"call_2","arguments":"{}"}
			]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}

	if prepared.webSearchAlias != "clientfn_web_search_1" {
		t.Fatalf("prepared.webSearchAlias = %q, want clientfn_web_search_1", prepared.webSearchAlias)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools length = %d, want 2; body=%s", len(tools), prepared.body)
	}
	if got := tools[0].Get("name").String(); got != "clientfn_web_search" {
		t.Fatalf("tools.0.name = %q, want original clientfn_web_search; body=%s", got, prepared.body)
	}
	if got := tools[1].Get("name").String(); got != "clientfn_web_search_1" {
		t.Fatalf("tools.1.name = %q, want clientfn_web_search_1; body=%s", got, prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "input.0.name").String(); got != "clientfn_web_search" {
		t.Fatalf("input.0.name = %q, want original clientfn_web_search; body=%s", got, prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "input.1.name").String(); got != "clientfn_web_search_1" {
		t.Fatalf("input.1.name = %q, want clientfn_web_search_1; body=%s", got, prepared.body)
	}

	// Verify restoration only targets clientfn_web_search_1
	event := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"clientfn_web_search","call_id":"call_1"}}`)
	restoredOriginal := restoreXAIClientWebSearchName(event, prepared.webSearchAlias)
	if got := gjson.GetBytes(restoredOriginal, "item.name").String(); got != "clientfn_web_search" {
		t.Fatalf("original clientfn_web_search must not be overwritten: %s", restoredOriginal)
	}

	eventAliased := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"clientfn_web_search_1","call_id":"call_2"}}`)
	restoredAliased := restoreXAIClientWebSearchName(eventAliased, prepared.webSearchAlias)
	if got := gjson.GetBytes(restoredAliased, "item.name").String(); got != "web_search" {
		t.Fatalf("aliased clientfn_web_search_1 must be restored to web_search: %s", restoredAliased)
	}
}

func TestXAIExecutorDoesNotAliasNamespacedWebSearchToolChoice(t *testing.T) {
	t.Parallel()

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"tools":[
				{"type":"function","name":"web_search","parameters":{"type":"object"}},
				{"type":"namespace","name":"acme","tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}]}
			],
			"tool_choice":{"type":"allowed_tools","tools":[
				{"type":"function","name":"web_search","namespace":"acme"},
				{"type":"function","name":"web_search"}
			]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}

	allowedTools := gjson.GetBytes(prepared.body, "tool_choice.tools").Array()
	if len(allowedTools) != 2 {
		t.Fatalf("tool_choice.tools length = %d, want 2; body=%s", len(allowedTools), prepared.body)
	}
	// The namespaced tool should be flattened to acme__web_search, NOT clientfn_web_search
	if got := allowedTools[0].Get("name").String(); got != "acme__web_search" {
		t.Fatalf("tool_choice.tools.0.name = %q, want acme__web_search; body=%s", got, prepared.body)
	}
	// The top-level tool should be aliased to clientfn_web_search
	if got := allowedTools[1].Get("name").String(); got != "clientfn_web_search" {
		t.Fatalf("tool_choice.tools.1.name = %q, want clientfn_web_search; body=%s", got, prepared.body)
	}
}

func TestXAIExecutorAliasesClientWebSearchBeyond100Collisions(t *testing.T) {
	t.Parallel()

	// Construct body with clientfn_web_search and clientfn_web_search_1 .. _100
	tools := []string{
		`{"type":"function","name":"web_search","parameters":{"type":"object"}}`,
		`{"type":"function","name":"clientfn_web_search","parameters":{"type":"object"}}`,
	}
	for i := 1; i <= 100; i++ {
		tools = append(tools, fmt.Sprintf(`{"type":"function","name":"clientfn_web_search_%d","parameters":{"type":"object"}}`, i))
	}
	payload := fmt.Sprintf(`{"model":"grok-4.6","tools":[%s]}`, strings.Join(tools, ","))

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "grok-4.6",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}

	if prepared.webSearchAlias != "clientfn_web_search_101" {
		t.Fatalf("prepared.webSearchAlias = %q, want clientfn_web_search_101", prepared.webSearchAlias)
	}
}

func TestXAIExecutorDoesNotRestoreNamespacedClientfnWebSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Event 1: namespaced tool call with name matching alias: acme__clientfn_web_search
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"call_1\",\"type\":\"function_call\",\"name\":\"acme__clientfn_web_search\",\"arguments\":\"{}\"}}\n\n")
		// Event 2: unnamespaced tool call with name matching alias: clientfn_web_search
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"call_2\",\"type\":\"function_call\",\"name\":\"clientfn_web_search\",\"arguments\":\"{}\"}}\n\n")
		// Completed event containing both
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"id\":\"call_1\",\"type\":\"function_call\",\"name\":\"acme__clientfn_web_search\"},{\"id\":\"call_2\",\"type\":\"function_call\",\"name\":\"clientfn_web_search\"}]}}\n\n")
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	result, errStream := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model: "grok-4.6",
		Payload: []byte(`{
			"model":"grok-4.6",
			"tools":[
				{"type":"function","name":"web_search","parameters":{"type":"object"}},
				{"type":"namespace","name":"acme","tools":[{"type":"function","name":"clientfn_web_search","parameters":{"type":"object"}}]}
			]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}

	var streamOutput bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamOutput.Write(chunk.Payload)
		streamOutput.WriteByte('\n')
	}
	streamText := streamOutput.String()

	// Verify namespaced tool kept name clientfn_web_search and namespace acme
	if !strings.Contains(streamText, `"name":"clientfn_web_search"`) {
		t.Fatalf("namespaced clientfn_web_search should be preserved: %s", streamText)
	}
	if !strings.Contains(streamText, `"namespace":"acme"`) {
		t.Fatalf("namespace acme should be preserved: %s", streamText)
	}
	// Verify unnamespaced tool was restored to web_search
	if !strings.Contains(streamText, `"name":"web_search"`) {
		t.Fatalf("unnamespaced tool should be restored to web_search: %s", streamText)
	}
}

func TestXAIExecutorFoldsNamespaceNamedWebSearchWithoutAliasing(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"name\":\"web_search\",\"call_id\":\"call_1\",\"arguments\":\"{\\\"name\\\":\\\"query_web\\\",\\\"arguments\\\":{\\\"q\\\":\\\"golang\\\"}}\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"grok-4.6\",\"output\":[{\"type\":\"function_call\",\"name\":\"web_search\",\"call_id\":\"call_1\",\"arguments\":\"{\\\"name\\\":\\\"query_web\\\",\\\"arguments\\\":{\\\"q\\\":\\\"golang\\\"}}\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	var nsList []string
	// 46 apps + 1 namespace named web_search = 47 namespaces (>200 tools, triggers folding)
	for i := 0; i < 46; i++ {
		var childTools []string
		for j := 0; j < 10; j++ {
			childTools = append(childTools, fmt.Sprintf(`{"type":"function","name":"tool_%d","description":"child tool %d","parameters":{"type":"object"}}`, j, j))
		}
		nsList = append(nsList, fmt.Sprintf(`{"type":"namespace","name":"mcp__app_%d","tools":[%s]}`, i, strings.Join(childTools, ",")))
	}
	// Namespace named web_search
	nsList = append(nsList, `{"type":"namespace","name":"web_search","tools":[{"type":"function","name":"query_web","parameters":{"type":"object"}}]}`)

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}

	turnPayload := fmt.Sprintf(`{
		"model":"grok-4.6",
		"tools":[%s],
		"input":[{"role":"user","content":"search query"}]
	}`, strings.Join(nsList, ","))

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.6",
		Payload: []byte(turnPayload),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Stream:         false,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify upstream tools contains the dispatcher named "web_search" (not clientfn_web_search)
	foundDispatcher := false
	for _, tool := range gjson.GetBytes(gotBody, "tools").Array() {
		if tool.Get("name").String() == "web_search" {
			foundDispatcher = true
		}
		if tool.Get("name").String() == "clientfn_web_search" {
			t.Fatalf("namespace dispatcher web_search must not be aliased to clientfn_web_search; body=%s", gotBody)
		}
	}
	if !foundDispatcher {
		t.Fatalf("dispatcher web_search tool missing from upstream tools; body=%s", gotBody)
	}

	// Verify response unwrapped into child tool call
	output := gjson.GetBytes(resp.Payload, "output.0")
	if got := output.Get("name").String(); got != "query_web" {
		t.Fatalf("output.0.name = %q, want query_web; payload=%s", got, resp.Payload)
	}
	if got := output.Get("namespace").String(); got != "web_search" {
		t.Fatalf("output.0.namespace = %q, want web_search; payload=%s", got, resp.Payload)
	}
}
func TestXAIExecutorThinkingPayloadOverride(t *testing.T) {
	const remoteModel = "grok-home-only-thinking-test"
	if registry.LookupModelInfo(remoteModel, "xai") != nil {
		t.Fatal("test model must be absent from the local registry")
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("xai-non-thinking-override-test", "xai", []*registry.ModelInfo{{ID: "grok-4", Type: "xai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient("xai-non-thinking-override-test") })
	levels := &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
	tests := []struct {
		name        string
		model       string
		suffix      string
		metadataKey string
		thinking    *registry.ThinkingSupport
		wantEffort  string
	}{
		{
			name: "home-only model", model: remoteModel,
			metadataKey: "cliproxy.resolved_home_model_info", thinking: levels, wantEffort: "high",
		},
		{
			name: "configured API-key model", model: remoteModel,
			metadataKey: "cliproxy.resolved_api_key_model_info", thinking: levels, wantEffort: "high",
		},
		{
			name: "home disables local thinking support", model: "grok-4.5",
			metadataKey: "cliproxy.resolved_home_model_info",
		},
		{
			name: "API-key disables local thinking support", model: "grok-4.5",
			metadataKey: "cliproxy.resolved_api_key_model_info",
		},
		{
			name: "home supplies no thinking levels", model: "grok-4.5",
			metadataKey: "cliproxy.resolved_home_model_info", thinking: &registry.ThinkingSupport{}, wantEffort: "high",
		},
		{
			name: "home restricts thinking levels", model: "grok-4.5",
			metadataKey: "cliproxy.resolved_home_model_info", thinking: &registry.ThinkingSupport{Levels: []string{"low"}}, wantEffort: "low",
		},
		{name: "local supported fallback", model: "grok-4.5", wantEffort: "high"},
		// Adaptation: upstream uses grok-build-0.1, which carries thinking
		// levels in this fork's registry; grok-4 is registered non-thinking
		// below to preserve the unsupported-model strip coverage.
		{name: "local unsupported fallback", model: "grok-4"},
		{name: "local unknown fallback", model: remoteModel, wantEffort: "high"},
		{name: "model suffix", model: "grok-4.5", suffix: "(low)", wantEffort: "low"},
	}
	for _, tt := range tests {
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/override=%t", tt.name, override), func(t *testing.T) {
				cfg := &config.Config{}
				wantEffort := tt.wantEffort
				if override {
					// Explicit overrides may force an effort beyond the model's declared capabilities.
					wantEffort = "xhigh"
					cfg.Payload.Override = []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: tt.model}},
						Params: map[string]any{"reasoning.effort": wantEffort},
					}}
				}
				exec := NewXAIExecutor(cfg)
				req := cliproxyexecutor.Request{
					Model:   tt.model + tt.suffix,
					Payload: []byte(fmt.Sprintf(`{"model":%q,"input":"hello","reasoning":{"effort":"high"}}`, tt.model)),
				}
				if tt.metadataKey != "" {
					req.Metadata = map[string]any{
						tt.metadataKey: &registry.ModelInfo{ID: tt.model, Type: "xai", Thinking: tt.thinking},
					}
				}
				prepared, errPrepare := exec.prepareResponsesRequest(t.Context(), nil, req, cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatOpenAIResponse,
				}, false)
				if errPrepare != nil {
					t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
				}
				if got := gjson.GetBytes(prepared.body, "reasoning.effort").String(); got != wantEffort {
					t.Fatalf("reasoning.effort = %q, want %q; body=%s", got, wantEffort, prepared.body)
				}
			})
		}
	}
}

// testGrokRouterFixture builds the deterministic high-entropy unpadded
// standard-base64 blob shape native xAI encrypted_content has, plus the router
// suffix shape OpenRouter appends.
func testGrokRouterFixture() (native, routed string) {
	payload := make([]byte, 135)
	for i := range payload {
		payload[i] = byte((i*73 + 19) & 0xff)
	}
	native = base64.RawStdEncoding.EncodeToString(payload)
	return native, native + "." + strings.Repeat("Ab3", 21)[:62]
}

// TestXAIExecutorStripsRouterSuffixFromGrokReasoningReplay is the F1 live path:
// a conversation started through OpenRouter x-ai/grok-* carries a Grok
// encrypted_content with the router suffix, and replaying it to the native xAI
// credential must strip the suffix or api.x.ai answers 400 "Could not decrypt"
// (upstream CLIProxyAPI a68c7d8ed7c2).
func TestXAIExecutorStripsRouterSuffixFromGrokReasoningReplay(t *testing.T) {
	native, routed := testGrokRouterFixture()

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"grok-4.5\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-router",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  server.URL,
			"api_key":   "xai-key",
			"auth_kind": "api",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "grok-4.5",
		Payload: []byte(`{"model":"grok-4.5","input":[` +
			`{"type":"reasoning","summary":[],"encrypted_content":"` + routed + `"},` +
			`{"role":"user","content":[{"type":"input_text","text":"continue"}]}` +
			`]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(gotBody, "input.0.encrypted_content").String(); got != native {
		t.Fatalf("upstream encrypted_content = %q, want the native Grok blob %q (router suffix stripped); body=%s", got, native, gotBody)
	}
}

// TestXAIExecutorUserAgentPassthrough drives the live executor path: the
// downstream User-Agent wins when present, and a request that carries none gets
// the Grok workspace identity instead of Go's default HTTP client User-Agent.
func TestXAIExecutorUserAgentPassthrough(t *testing.T) {
	var gotUserAgent string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.3\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-ua-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}

	run := func(headers http.Header) string {
		t.Helper()
		gotUserAgent = ""
		if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Headers:      headers,
		}); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		return gotUserAgent
	}

	if got := run(http.Header{"User-Agent": []string{"GrokDesktop/2.5"}}); got != "GrokDesktop/2.5" {
		t.Fatalf("downstream User-Agent = %q, want GrokDesktop/2.5", got)
	}

	if got := run(nil); !strings.HasPrefix(got, "xai-grok-workspace/") || got == "xai-grok-workspace/" {
		t.Fatalf("default User-Agent = %q, want a versioned xai-grok-workspace identity", got)
	}
}

// TestNormalizeXAIInputReasoningItemsRouterSuffix pins both directions of the
// suffix decision: an OpenRouter upstream receives the value it minted verbatim,
// every other upstream receives the native blob. It complements the live-path
// test above, which can only reach a non-OpenRouter host over httptest.
func TestNormalizeXAIInputReasoningItemsRouterSuffix(t *testing.T) {
	native, routed := testGrokRouterFixture()
	body := []byte(`{"input":[` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + routed + `"},` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + native + `"},` +
		`{"role":"user","content":[{"type":"input_text","text":"continue"}]}` +
		`]}`)

	official := normalizeXAIInputReasoningItems(body, false)
	if got := gjson.GetBytes(official, "input.0.encrypted_content").String(); got != native {
		t.Fatalf("official upstream encrypted_content = %q, want %q", got, native)
	}
	if got := gjson.GetBytes(official, "input.1.encrypted_content").String(); got != native {
		t.Fatalf("native blob must be untouched, got %q", got)
	}

	openRouter := normalizeXAIInputReasoningItems(body, true)
	if got := gjson.GetBytes(openRouter, "input.0.encrypted_content").String(); got != routed {
		t.Fatalf("OpenRouter encrypted_content = %q, want the routed value verbatim %q", got, routed)
	}
	if got := gjson.GetBytes(openRouter, "input.1.encrypted_content").String(); got != native {
		t.Fatalf("native blob must be untouched on OpenRouter, got %q", got)
	}
}

// The same namespace declared at the top level and again in a historical
// additional_tools item used to be emitted twice, and xAI rejects a request that
// declares the same tool name twice. The top-level declaration is authoritative
// and the historical copy is dropped.
func TestPromoteXAIAdditionalTools_DeduplicatesNamespacedHistory(t *testing.T) {
	body := []byte(`{
		"tools":[{"type":"namespace","name":"clock","description":"clock tools","tools":[{"type":"function","name":"sleep","description":"top declaration","parameters":{"type":"object","properties":{"duration_ms":{"type":"string"}},"required":["duration_ms"]}}]}],
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"clock","tools":[{"type":"function","name":"sleep","description":"history declaration","parameters":{"type":"object","properties":{"seconds":{"type":"string"}},"required":["seconds"]}}]}]},
			{"role":"user","content":"hello"}
		]
	}`)
	out := promoteXAIAdditionalTools(normalizeXAITools(body))

	declared := 0
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		if tool.Get("name").String() == "clock__sleep" {
			declared++
		}
	}
	if declared != 1 {
		t.Fatalf("clock__sleep declared %d times, want 1; body=%s", declared, out)
	}
	if got := gjson.GetBytes(out, "tools.0.description").String(); got != "top declaration" {
		t.Fatalf("tools.0.description = %q, want the top-level declaration; body=%s", got, out)
	}
	if !gjson.GetBytes(out, "tools.0.parameters.properties.duration_ms").Exists() {
		t.Fatalf("tools.0 lost the top-level schema; body=%s", out)
	}
	if gjson.GetBytes(out, "tools.0.parameters.properties.seconds").Exists() {
		t.Fatalf("tools.0 took the historical schema; body=%s", out)
	}
	for _, input := range gjson.GetBytes(out, "input").Array() {
		if input.Get("type").String() == "additional_tools" {
			t.Fatalf("unpromoted additional_tools input remained: %s", out)
		}
	}
}

// Overlapping history must not inflate the flattened tool count: 120 unique
// children declared twice are 120 tools, not 240, so the request is not folded.
// When folding does happen, a repeated namespace yields one dispatcher whose
// description carries the top-level child and any history-only child.
func TestXAIShouldFoldNamespaceTools_DeduplicatesAcrossAdditionalTools(t *testing.T) {
	var childTools []string
	for i := 0; i < 120; i++ {
		childTools = append(childTools, fmt.Sprintf(`{"type":"function","name":"tool_%d","parameters":{"type":"object"}}`, i))
	}
	ns := fmt.Sprintf(`{"type":"namespace","name":"mcp__app","tools":[%s]}`, strings.Join(childTools, ","))
	body := []byte(fmt.Sprintf(`{"tools":[%s],"input":[{"type":"additional_tools","role":"developer","tools":[%s]},{"role":"user","content":"hi"}]}`, ns, ns))
	if xaiShouldFoldNamespaceTools(body, false) {
		t.Fatalf("xaiShouldFoldNamespaceTools = true, want false: 120 + 120 overlapping tools deduplicate to 120 (<= %d)", xaiMaxTools)
	}
	if got := len(gjson.GetBytes(promoteXAIAdditionalTools(normalizeXAITools(body)), "tools").Array()); got != 120 {
		t.Fatalf("deduplicated tools length = %d, want 120", got)
	}

	var extraTools []string
	for i := 120; i < 210; i++ {
		extraTools = append(extraTools, fmt.Sprintf(`{"type":"function","name":"tool_%d","parameters":{"type":"object"}}`, i))
	}
	topNS := `{"type":"namespace","name":"clock","tools":[{"type":"function","name":"sleep","description":"top sleep","parameters":{"type":"object"}}]}`
	histNS := fmt.Sprintf(`{"type":"namespace","name":"clock","tools":[{"type":"function","name":"sleep","description":"history sleep","parameters":{"type":"object"}},{"type":"function","name":"alarm","description":"history alarm","parameters":{"type":"object"}}]},{"type":"namespace","name":"mcp__extra","tools":[%s]}`, strings.Join(extraTools, ","))
	foldBody := []byte(fmt.Sprintf(`{"tools":[%s,%s],"input":[{"type":"additional_tools","role":"developer","tools":[%s]},{"role":"user","content":"hi"}]}`, topNS, ns, histNS))
	if !xaiShouldFoldNamespaceTools(foldBody, false) {
		t.Fatalf("xaiShouldFoldNamespaceTools = false, want true: unique flattened tools exceed %d", xaiMaxTools)
	}

	folded := gjson.GetBytes(promoteXAIAdditionalTools(normalizeXAITools(foldBody)), "tools").Array()
	if len(folded) != 3 {
		t.Fatalf("folded tools length = %d, want 3 (clock, mcp__app, mcp__extra); tools=%v", len(folded), folded)
	}
	clockDesc := folded[0].Get("description").String()
	if !strings.Contains(clockDesc, "top sleep") {
		t.Fatalf("clock dispatcher description lost the top-level child: %s", clockDesc)
	}
	if strings.Contains(clockDesc, "history sleep") {
		t.Fatalf("clock dispatcher description kept the duplicate history child: %s", clockDesc)
	}
	if !strings.Contains(clockDesc, "history alarm") {
		t.Fatalf("clock dispatcher description dropped the history-only child: %s", clockDesc)
	}
}

// End to end through the executor: whatever the request declares twice, the body
// that reaches the upstream carries each tool name once and keeps the top-level
// declaration, and the reply still restores the namespaced call.
func TestXAIExecutorDeduplicatesToolsAcrossAdditionalTools(t *testing.T) {
	top := `{"type":"namespace","name":"clock","tools":[{"type":"function","name":"sleep","description":"top declaration","parameters":{"type":"object","properties":{"duration_ms":{"type":"string"}},"required":["duration_ms"]}}]}`
	history := `{"type":"namespace","name":"clock","tools":[{"type":"function","name":"sleep","description":"history declaration","parameters":{"type":"object","properties":{"seconds":{"type":"string"}},"required":["seconds"]}}]}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Errorf("read body: %v", errRead)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"name\":\"clock__sleep\",\"call_id\":\"call_1\",\"arguments\":\"{\\\"duration_ms\\\":\\\"10\\\"}\"}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"grok-4.7\",\"output\":[{\"type\":\"function_call\",\"name\":\"clock__sleep\",\"call_id\":\"call_1\",\"arguments\":\"{\\\"duration_ms\\\":\\\"10\\\"}\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
			}))
			defer server.Close()

			payload := fmt.Sprintf(`{"model":"grok-4.7","tools":[%s],"input":[{"type":"additional_tools","role":"developer","tools":[%s]},{"role":"user","content":"say hello"}]}`, top, history)
			exec := NewXAIExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				Provider:   "xai",
				Attributes: map[string]string{"base_url": server.URL, "api_key": "fake-token"},
			}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(payload)}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
				Stream:         stream,
			}
			if stream {
				result, errExec := exec.ExecuteStream(t.Context(), auth, req, opts)
				if errExec != nil {
					t.Fatalf("ExecuteStream() error = %v", errExec)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error = %v", chunk.Err)
					}
				}
			} else {
				resp, errExec := exec.Execute(t.Context(), auth, req, opts)
				if errExec != nil {
					t.Fatalf("Execute() error = %v", errExec)
				}
				if got := gjson.GetBytes(resp.Payload, "output.0.name").String(); got != "sleep" {
					t.Fatalf("restored output.0.name = %q, want sleep; payload=%s", got, resp.Payload)
				}
				if got := gjson.GetBytes(resp.Payload, "output.0.namespace").String(); got != "clock" {
					t.Fatalf("restored output.0.namespace = %q, want clock; payload=%s", got, resp.Payload)
				}
			}

			declared := 0
			for _, tool := range gjson.GetBytes(gotBody, "tools").Array() {
				if tool.Get("name").String() == "clock__sleep" {
					declared++
				}
			}
			if declared != 1 {
				t.Fatalf("upstream body declares clock__sleep %d times, want 1; body=%s", declared, gotBody)
			}
			if got := gjson.GetBytes(gotBody, "tools.0.description").String(); got != "top declaration" {
				t.Fatalf("tools.0.description = %q, want the top-level declaration; body=%s", got, gotBody)
			}
			if gjson.GetBytes(gotBody, "tools.0.parameters.properties.seconds").Exists() {
				t.Fatalf("tools.0 took the historical schema; body=%s", gotBody)
			}
			for _, input := range gjson.GetBytes(gotBody, "input").Array() {
				if input.Get("type").String() == "additional_tools" {
					t.Fatalf("unpromoted additional_tools input reached the upstream: %s", gotBody)
				}
			}
		})
	}
}
