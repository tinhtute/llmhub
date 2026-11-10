package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestEnsureImageGenerationTool_NoTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	if !tools.IsArray() {
		t.Fatalf("expected tools array, got %v", tools.Type)
	}
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected type=image_generation, got %s", arr[0].Get("type").String())
	}
	if arr[0].Get("output_format").String() != "png" {
		t.Fatalf("expected output_format=png, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_ExistingToolsWithoutImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"get_weather","parameters":{}}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "function" {
		t.Fatalf("expected first tool type=function, got %s", arr[0].Get("type").String())
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", arr[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_AlreadyPresent(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","output_format":"webp"},{"type":"function","name":"f1"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools (no duplicate), got %d", len(arr))
	}
	if arr[0].Get("output_format").String() != "webp" {
		t.Fatalf("expected original output_format=webp preserved, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_EmptyToolsArray(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected type=image_generation, got %s", arr[0].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_WebSearchAndImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"web_search"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "web_search" {
		t.Fatalf("expected first tool type=web_search, got %s", arr[0].Get("type").String())
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", arr[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_GPT53CodexSparkDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.3-codex-spark","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.3-codex-spark", nil, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for gpt-5.3-codex-spark, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestEnsureImageGenerationTool_FreeCodexAuthDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	freeAuth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"plan_type": "free"},
	}
	result := ensureImageGenerationTool(body, "gpt-5.4", freeAuth, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for free codex auth, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestEnsureImageGenerationTool_ResponsesLiteModelsDoNotInjectToolByDefault(t *testing.T) {
	for _, model := range []string{
		"gpt-6-astra",
		"gpt-6-sol",
		"gpt-6.1-sol",
		"gpt-6-luna",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"codex/gpt-6-astra(medium)",
		"codex/gpt-5.6-sol(high)",
	} {
		t.Run(model, func(t *testing.T) {
			body := []byte(`{"model":"` + model + `","input":"hello"}`)
			result := ensureImageGenerationTool(body, model, nil, nil)
			if gjson.GetBytes(result, "tools").Exists() {
				t.Fatalf("expected no auto-injected tools for %s, got %s", model, result)
			}
		})
	}

	for _, nonLiteModel := range []string{"gpt-5.5", "gpt-5.4"} {
		t.Run("non-lite/"+nonLiteModel, func(t *testing.T) {
			body := []byte(`{"model":"` + nonLiteModel + `","input":"hello"}`)
			result := ensureImageGenerationTool(body, nonLiteModel, nil, nil)
			if got := gjson.GetBytes(result, "tools.0.type").String(); got != "image_generation" {
				t.Fatalf("for %s: tools.0.type = %q, want image_generation; body=%s", nonLiteModel, got, result)
			}
		})
	}

	headers := http.Header{}
	headers.Set(codexResponsesLiteHeader, "false")
	body := []byte(`{"model":"gpt-6-astra","input":"hello"}`)
	if result := ensureImageGenerationTool(body, "gpt-6-astra", nil, headers); gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no auto-injected tools even when responses-lite is explicitly false, got %s", result)
	}
}

func TestNormalizeCodexImageGenerationToolsStripsComplexity(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","tools":[{"type":"image_generation","output_format":"png","complexity":"high"},{"type":"function","name":"lookup","complexity":"keep"}],"input":[{"type":"additional_tools","tools":[{"type":"image_generation","output_format":"png","complexity":"medium"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"draw"}]}]}`)
	result := normalizeCodexImageGenerationTools(body)

	if gjson.GetBytes(result, "tools.0.complexity").Exists() {
		t.Fatalf("tools.0.complexity should be stripped: %s", result)
	}
	if gjson.GetBytes(result, "input.0.tools.0.complexity").Exists() {
		t.Fatalf("input.0.tools.0.complexity should be stripped: %s", result)
	}
	if got := gjson.GetBytes(result, "tools.1.complexity").String(); got != "keep" {
		t.Fatalf("function tool complexity = %q, want keep: %s", got, result)
	}
	if got := gjson.GetBytes(result, "tools.0.complexity").String(); got != "" {
		t.Fatalf("image tool complexity = %q, want empty", got)
	}
}

func TestCodexRequestForcesImageGeneration(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "required with only image tools",
			body: `{"tools":[{"type":"image_generation"}],"tool_choice":"required"}`,
			want: true,
		},
		{
			name: "explicit image_generation choice",
			body: `{"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`,
			want: true,
		},
		{
			name: "allowed_tools required all image",
			body: `{"tools":[{"type":"image_generation"}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"image_generation"}]}}`,
			want: true,
		},
		{
			name: "additional_tools required",
			body: `{"input":[{"type":"additional_tools","tools":[{"type":"image_generation"}]}],"tool_choice":"required"}`,
			want: true,
		},
		{
			name: "choice omitted",
			body: `{"tools":[{"type":"image_generation"}]}`,
			want: false,
		},
		{
			name: "choice auto",
			body: `{"tools":[{"type":"image_generation"}],"tool_choice":"auto"}`,
			want: false,
		},
		{
			name: "choice none",
			body: `{"tools":[{"type":"image_generation"}],"tool_choice":"none"}`,
			want: false,
		},
		{
			name: "required with mixed tools",
			body: `{"tools":[{"type":"image_generation"},{"type":"function","name":"lookup"}],"tool_choice":"required"}`,
			want: false,
		},
		{
			name: "allowed_tools auto",
			body: `{"tools":[{"type":"image_generation"}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"image_generation"}]}}`,
			want: false,
		},
		{
			name: "allowed_tools excludes image generation",
			body: `{"tools":[{"type":"image_generation"},{"type":"function","name":"lookup"}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"lookup"}]}}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexRequestForcesImageGeneration([]byte(tc.body)); got != tc.want {
				t.Fatalf("codexRequestForcesImageGeneration() = %v, want %v for %s", got, tc.want, tc.body)
			}
		})
	}
}

func TestApplyCodexHostedImageGenerationModelRewritesForcedTurns(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"max"},"tools":[{"type":"image_generation","output_format":"png","complexity":"high"}],"tool_choice":{"type":"image_generation"}}`)
	body = normalizeCodexImageGenerationTools(body)
	result, upstreamModel, err := applyCodexHostedImageGenerationModel(&config.Config{}, body, "gpt-6-astra")
	if err != nil {
		t.Fatalf("applyCodexHostedImageGenerationModel() error = %v", err)
	}
	if upstreamModel != codexOpenAIImagesMainModel {
		t.Fatalf("upstreamModel = %q, want %q", upstreamModel, codexOpenAIImagesMainModel)
	}
	if got := gjson.GetBytes(result, "model").String(); got != codexOpenAIImagesMainModel {
		t.Fatalf("body model = %q, want %q", got, codexOpenAIImagesMainModel)
	}
	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh for %s", got, codexOpenAIImagesMainModel)
	}
	if gjson.GetBytes(result, "tools.0.complexity").Exists() {
		t.Fatalf("complexity should be stripped: %s", result)
	}
}

func TestApplyCodexHostedImageGenerationModelLeavesOptionalTurns(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "choice omitted", body: `{"model":"gpt-6-astra","tools":[{"type":"image_generation"}]}`},
		{name: "choice auto", body: `{"model":"gpt-6-astra","tools":[{"type":"image_generation"}],"tool_choice":"auto"}`},
		{name: "choice none", body: `{"model":"gpt-6-astra","tools":[{"type":"image_generation"}],"tool_choice":"none"}`},
		{name: "allowed_tools auto", body: `{"model":"gpt-6-astra","tools":[{"type":"image_generation"}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"image_generation"}]}}`},
		{name: "non-lite model", body: `{"model":"gpt-5.4","tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, upstreamModel, err := applyCodexHostedImageGenerationModel(&config.Config{}, []byte(tc.body), gjson.Get(tc.body, "model").String())
			if err != nil {
				t.Fatalf("applyCodexHostedImageGenerationModel() error = %v", err)
			}
			model := gjson.Get(tc.body, "model").String()
			if upstreamModel != model {
				t.Fatalf("upstreamModel = %q, want %q", upstreamModel, model)
			}
			if got := gjson.GetBytes(result, "model").String(); got != model {
				t.Fatalf("body model = %q, want %q", got, model)
			}
		})
	}
}

func TestApplyCodexHostedImageGenerationModelSkipsDisabledConfig(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	result, upstreamModel, err := applyCodexHostedImageGenerationModel(cfg, body, "gpt-6-astra")
	if err != nil {
		t.Fatalf("applyCodexHostedImageGenerationModel() error = %v", err)
	}
	if upstreamModel != "gpt-6-astra" {
		t.Fatalf("upstreamModel = %q, want gpt-6-astra", upstreamModel)
	}
	if got := gjson.GetBytes(result, "model").String(); got != "gpt-6-astra" {
		t.Fatalf("body model = %q, want gpt-6-astra", got)
	}
}

func TestResolveCodexGPTImage2BaseModelRejectsResponsesLiteOverride(t *testing.T) {
	for _, configured := range []string{"gpt-6-astra", "gpt-5.6-sol", "claude-sonnet-5"} {
		cfg := &config.Config{SDKConfig: config.SDKConfig{GPTImage2BaseModel: configured}}
		if got := resolveCodexGPTImage2BaseModel(cfg); got != codexOpenAIImagesMainModel {
			t.Errorf("configured %q: resolveCodexGPTImage2BaseModel() = %q, want %q", configured, got, codexOpenAIImagesMainModel)
		}
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{GPTImage2BaseModel: "gpt-5.5"}}
	if got := resolveCodexGPTImage2BaseModel(cfg); got != "gpt-5.5" {
		t.Errorf("resolveCodexGPTImage2BaseModel() = %q, want gpt-5.5", got)
	}
}

func TestCodexTerminalFailureStatusMapsImageGenerationUserError(t *testing.T) {
	event := []byte(`{"type":"error","error":{"type":"image_generation_user_error","code":"invalid_value","message":"Complexity 'medium' is not supported for this image model.","param":"complexity"}}`)
	body, ok := codexTerminalFailureBody(event)
	if !ok {
		t.Fatal("expected terminal failure body")
	}
	if status := codexTerminalFailureStatus(body); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	message := gjson.GetBytes(body, "error.message").String()
	if !strings.Contains(message, "/v1/images/generations") || !strings.Contains(message, "gpt-5.4-mini") {
		t.Fatalf("expected remediation guidance, got %q", message)
	}
}

func TestCodexTerminalErrorBodyLeavesUnrelatedErrorsUnchanged(t *testing.T) {
	event := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad input"}}`)
	body, ok := codexTerminalFailureBody(event)
	if !ok {
		t.Fatal("expected terminal failure body")
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "bad input" {
		t.Fatalf("error.message = %q, want unchanged", got)
	}
}

func TestCodexExecutorHostedImageGenerationRoutingHint(t *testing.T) {
	const payload = `{"model":"gpt-6-astra","input":"draw a cat","tools":[{"type":"image_generation","output_format":"png","complexity":"high"}],"tool_choice":{"type":"image_generation"}}`
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			var captured capturedCodexRequest
			server := newCodexRoutingHintServer(t, &captured)
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(payload)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: stream}
			if stream {
				result, err := executor.ExecuteStream(context.Background(), codexOAuthTestAuth(server.URL), req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream error: %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error: %v", chunk.Err)
					}
				}
			} else if _, err := executor.Execute(context.Background(), codexOAuthTestAuth(server.URL), req, opts); err != nil {
				t.Fatalf("Execute error: %v", err)
			}

			if got := captured.model.String(); got != codexOpenAIImagesMainModel {
				t.Fatalf("body model = %q, want %q", got, codexOpenAIImagesMainModel)
			}
			if want := "model=" + codexOpenAIImagesMainModel; captured.routingHint != want {
				t.Fatalf("routing hint = %q, want %q", captured.routingHint, want)
			}
		})
	}
}
