package executor

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claude55ForcedTools = `[{"name":"lookup_weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`

func claude55ForcedPayload(model, choice string) []byte {
	payload := `{"model":"` + model + `","max_tokens":4096,"messages":[{"role":"user","content":"Weather in Paris?"}],"tools":` + claude55ForcedTools
	if choice != "" {
		payload += `,"tool_choice":` + choice
	}
	return []byte(payload + `}`)
}

type claude55Executor interface {
	Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
}

// captureClaude55Body drives a real executor and captures only the network
// boundary, so translation and every request preparation stage stay real.
func captureClaude55Body(t *testing.T, executor claude55Executor, auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options, method string) ([]byte, http.Header) {
	t.Helper()
	var captured []byte
	var headers http.Header
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		if errClose := req.Body.Close(); errClose != nil {
			return nil, errClose
		}
		captured, headers = body, req.Header.Clone()
		contentType := "application/json"
		model := gjson.GetBytes(body, "model").String()
		response := `{"id":"msg_claude55","type":"message","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":17,"output_tokens":1}}`
		if strings.HasSuffix(req.URL.Path, "/count_tokens") {
			response = `{"input_tokens":17}`
		} else if gjson.GetBytes(body, "stream").Bool() {
			contentType, response = "text/event-stream", strings.ReplaceAll(claudeIssue6698SSE, "claude-opus-5-5", model)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	switch method {
	case "Execute":
		if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute != nil {
			t.Fatalf("Execute: %v", errExecute)
		}
	case "ExecuteStream":
		result, errStream := executor.ExecuteStream(ctx, auth, request, options)
		if errStream != nil {
			t.Fatalf("ExecuteStream: %v", errStream)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk: %v", chunk.Err)
			}
		}
	case "CountTokens":
		response, errCount := executor.CountTokens(ctx, auth, request, options)
		if errCount != nil {
			t.Fatalf("CountTokens: %v", errCount)
		}
		if got := gjson.GetBytes(response.Payload, "input_tokens").Int(); got != 17 {
			t.Fatalf("upstream input_tokens = %d, want 17", got)
		}
	default:
		t.Fatalf("unknown method %q", method)
	}
	if !gjson.ValidBytes(captured) {
		t.Fatalf("expected captured upstream JSON, got %q", captured)
	}
	return captured, headers
}

// TestClaudeExecutor_ForcedToolChoiceNormalized pins the user-visible fix: a
// forced choice Claude 5.5 rejects reaches the upstream as auto, with the
// parallel-tool setting and the tool declarations untouched.
func TestClaudeExecutor_ForcedToolChoiceNormalized(t *testing.T) {
	const auto = `{"type":"auto","disable_parallel_tool_use":true}`
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5"} {
		for _, choice := range []string{`{"type":"any","disable_parallel_tool_use":true}`, `{"type":"tool","name":"lookup_weather","disable_parallel_tool_use":true}`} {
			for _, method := range []string{"Execute", "ExecuteStream"} {
				t.Run(model+" "+method+" "+choice, func(t *testing.T) {
					body, _ := captureClaude55Body(t, NewClaudeExecutor(&config.Config{}), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-claude55"}}, cliproxyexecutor.Request{Model: model, Payload: claude55ForcedPayload(model, choice)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, method)
					if got, want := gjson.GetBytes(body, "tool_choice").Value(), gjson.Parse(auto).Value(); !reflect.DeepEqual(got, want) {
						t.Fatalf("tool_choice = %s, want %s; body=%s", gjson.GetBytes(body, "tool_choice").Raw, auto, body)
					}
					if gjson.GetBytes(body, "tool_choice.name").Exists() {
						t.Fatalf("forced tool name survived: %s", body)
					}
					if got := gjson.GetBytes(body, "tools.0.name").String(); got != "lookup_weather" {
						t.Fatalf("tools rewritten: %s", body)
					}
				})
			}
		}
	}
}

// TestClaudeExecutor_ForcedToolChoiceOlderModelUnchanged is the byte-compat pin:
// a model outside the 5.5 families keeps the forced choice it sent.
func TestClaudeExecutor_ForcedToolChoiceOlderModelUnchanged(t *testing.T) {
	for _, tc := range []struct{ model, choice string }{
		{"claude-opus-5", `{"type":"tool","name":"lookup_weather","disable_parallel_tool_use":false}`},
		{"claude-sonnet-4-6", `{"type":"any","disable_parallel_tool_use":true}`},
		{"claude-haiku-5-5", `{"type":"tool","name":"lookup_weather","disable_parallel_tool_use":false}`},
	} {
		t.Run(tc.model, func(t *testing.T) {
			body, _ := captureClaude55Body(t, NewClaudeExecutor(&config.Config{}), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-claude55"}}, cliproxyexecutor.Request{Model: tc.model, Payload: claude55ForcedPayload(tc.model, tc.choice)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, "Execute")
			if got, want := gjson.GetBytes(body, "tool_choice").Value(), gjson.Parse(tc.choice).Value(); !reflect.DeepEqual(got, want) {
				t.Fatalf("tool_choice = %s, want %s byte-identical; body=%s", gjson.GetBytes(body, "tool_choice").Raw, tc.choice, body)
			}
		})
	}
}

// TestClaudeExecutor_ForcedToolChoiceUsesRoutedModel proves the predicate reads
// the routed model, not the model the client put in the body.
func TestClaudeExecutor_ForcedToolChoiceUsesRoutedModel(t *testing.T) {
	const forced = `{"type":"tool","name":"lookup_weather"}`
	for _, tc := range []struct{ name, route, bodyModel, want string }{
		{name: "body cannot spoof a 5.5 route", route: "claude-opus-5", bodyModel: "claude-opus-5-5", want: forced},
		{name: "body cannot hide a 5.5 route", route: "claude-opus-5-5", bodyModel: "custom-alias", want: `{"type":"auto"}`},
		{name: "thinking suffix on the route", route: "claude-opus-5-5(high)", bodyModel: "claude-opus-5-5(high)", want: `{"type":"auto"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := captureClaude55Body(t, NewClaudeExecutor(&config.Config{}), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-claude55"}}, cliproxyexecutor.Request{Model: tc.route, Payload: claude55ForcedPayload(tc.bodyModel, forced)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, "Execute")
			if got, want := gjson.GetBytes(body, "tool_choice").Value(), gjson.Parse(tc.want).Value(); !reflect.DeepEqual(got, want) {
				t.Fatalf("tool_choice = %s, want %s; body=%s", gjson.GetBytes(body, "tool_choice").Raw, tc.want, body)
			}
		})
	}
}

// TestClaudeExecutor_ForcedToolChoiceKeepsThinkingAndEffort is the ordering
// proof for "before thinking preparation": the local forced-mode cleanup
// (disableThinkingIfToolChoiceForced, claude_executor.go) deletes thinking and
// output_config.effort for any request that still forces a choice, and it runs
// after payload rules. Moving the normalization to that stage fails both
// assertions — verified by moving it there and back.
func TestClaudeExecutor_ForcedToolChoiceKeepsThinkingAndEffort(t *testing.T) {
	for _, source := range []struct {
		name, prefix, choice, method string
		format                       sdktranslator.Format
	}{
		{
			name:   "chat",
			format: sdktranslator.FormatOpenAI,
			prefix: `"messages":[{"role":"user","content":"Weather in Paris?"}],"reasoning_effort":"high","tools":[{"type":"function","function":{"name":"lookup_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`,
			choice: `{"type":"function","function":{"name":"lookup_weather"}}`,
			method: "Execute",
		},
		{
			name:   "responses",
			format: sdktranslator.FormatOpenAIResponse,
			prefix: `"input":[{"role":"user","content":[{"type":"input_text","text":"Weather in Paris?"}]}],"reasoning":{"effort":"high"},"tools":[{"type":"function","name":"lookup_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`,
			choice: `{"type":"function","name":"lookup_weather"}`,
			method: "ExecuteStream",
		},
	} {
		for _, choice := range []string{`"required"`, source.choice} {
			t.Run(source.name+"/"+choice, func(t *testing.T) {
				payload := `{"model":"claude-sonnet-5-5",` + source.prefix + `,"tool_choice":` + choice + `,"parallel_tool_calls":false}`
				body, _ := captureClaude55Body(t, NewClaudeExecutor(&config.Config{}), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-claude55"}}, cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: []byte(payload)}, cliproxyexecutor.Options{SourceFormat: source.format}, source.method)
				for path, want := range map[string]any{
					"tool_choice.type":                "auto",
					"tools.0.name":                    "lookup_weather",
					"tools.0.input_schema.required.0": "city",
					"thinking.type":                   "adaptive",
					"output_config.effort":            "high",
				} {
					if got := gjson.GetBytes(body, path).Value(); !reflect.DeepEqual(got, want) {
						t.Errorf("%s = %#v, want %#v; body=%s", path, got, want, body)
					}
				}
				if gjson.GetBytes(body, "tool_choice.name").Exists() {
					t.Errorf("forced tool name survived: %s", body)
				}
			})
		}
	}
}

// TestClaudeExecutor_ForcedToolChoicePreservesSignedHistory is the second
// ordering proof: applyClaudePreservedThinkingBinding
// (claude_executor_diagnostics.go) skips its binding when the payload still
// forces a tool choice, so a normalization placed after it loses the binding
// and this test fails. Signed thinking blocks survive the downgrade on every
// path that carries them.
func TestClaudeExecutor_ForcedToolChoicePreservesSignedHistory(t *testing.T) {
	for _, method := range []string{"Execute", "ExecuteStream", "CountTokens"} {
		t.Run(method, func(t *testing.T) {
			payload := preservedThinkingReplayPayload("claude-opus-5-5")
			payload, _ = sjson.SetRawBytes(payload, "tools", []byte(claude55ForcedTools))
			payload, _ = sjson.SetRawBytes(payload, "tool_choice", []byte(`{"type":"tool","name":"lookup_weather","disable_parallel_tool_use":true}`))
			body, _ := captureClaude55Body(t, NewClaudeExecutor(&config.Config{}), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-claude55"}}, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, method)
			if got := gjson.GetBytes(body, "tool_choice.type").String(); got != "auto" {
				t.Fatalf("tool_choice.type = %q, want auto; body=%s", got, body)
			}
			if gjson.GetBytes(body, "tool_choice.name").Exists() {
				t.Fatalf("forced tool name survived: %s", body)
			}
			if got := gjson.GetBytes(body, "thinking.type").String(); got != "adaptive" {
				t.Fatalf("thinking.type = %q, want adaptive (forced-mode cleanup did not fire); body=%s", got, body)
			}
			if got := gjson.GetBytes(body, "thinking.block_binding.prefix_mismatch_behavior").String(); got != "drop_block" {
				t.Fatalf("thinking.block_binding.prefix_mismatch_behavior = %q, want drop_block; body=%s", got, body)
			}
			if !strings.Contains(string(body), preservedThinkingTestSignature) {
				t.Fatalf("signed thinking history was lost: %s", body)
			}
		})
	}
}

// TestClaudeExecutor_ForcedToolChoicePayloadRulesRemainFinal is the ordering
// proof for "rules stay authoritative": a rule whose condition matches
// tool_choice.type=auto only fires if the downgrade already happened, and its
// effect is the last word on the payload.
func TestClaudeExecutor_ForcedToolChoicePayloadRulesRemainFinal(t *testing.T) {
	const model = "claude-opus-5-5"
	const forced = `{"type":"tool","name":"lookup_weather","disable_parallel_tool_use":true}`
	models := []config.PayloadModelRule{{Name: model, Exist: []string{"thinking"}, Match: []map[string]any{{"tool_choice.type": "auto"}}}}
	for _, method := range []string{"Execute", "ExecuteStream"} {
		for _, rules := range []string{"override", "filter"} {
			t.Run(method+"/"+rules, func(t *testing.T) {
				cfg := &config.Config{}
				if rules == "override" {
					cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{
						"tool_choice":      map[string]any{"type": "tool", "name": "user_override"},
						"observed_builtin": true,
					}}}}
				} else {
					cfg.Payload = config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"tool_choice", "thinking", "output_config.effort", "tools"}}}}
				}
				payload := claude55ForcedPayload(model, forced)
				payload, _ = sjson.SetRawBytes(payload, "thinking", []byte(`{"type":"adaptive"}`))
				payload, _ = sjson.SetRawBytes(payload, "output_config", []byte(`{"effort":"high"}`))
				body, _ := captureClaude55Body(t, NewClaudeExecutor(cfg), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-claude55"}}, cliproxyexecutor.Request{Model: model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, method)
				if rules == "filter" {
					for _, path := range []string{"tool_choice", "thinking", "output_config.effort", "tools"} {
						if gjson.GetBytes(body, path).Exists() {
							t.Errorf("filtered %s was restored: %s", path, body)
						}
					}
					return
				}
				if got := gjson.GetBytes(body, "tool_choice.type").String(); got != "tool" {
					t.Fatalf("tool_choice.type = %q, want the override's tool; body=%s", got, body)
				}
				if got := gjson.GetBytes(body, "tool_choice.name").String(); got != "user_override" {
					t.Fatalf("tool_choice.name = %q, want user_override; body=%s", got, body)
				}
				if !gjson.GetBytes(body, "observed_builtin").Bool() {
					t.Fatalf("override payload fields missing: %s", body)
				}
			})
		}
	}
}

// TestClaudeExecutor_ForcedToolChoiceCountTokens proves the token path counts
// the same downgraded shape the messages path forwards.
func TestClaudeExecutor_ForcedToolChoiceCountTokens(t *testing.T) {
	const model = "claude-sonnet-5-5"
	for _, choice := range []string{`{"type":"any","disable_parallel_tool_use":true}`, `{"type":"tool","name":"lookup_weather","disable_parallel_tool_use":true}`} {
		t.Run(choice, func(t *testing.T) {
			body, headers := captureClaude55Body(t, NewClaudeExecutor(&config.Config{}), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-claude55"}}, cliproxyexecutor.Request{Model: model, Payload: claude55ForcedPayload("spoofed-body-model", choice)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, "CountTokens")
			if got, want := gjson.GetBytes(body, "tool_choice").Value(), gjson.Parse(`{"type":"auto","disable_parallel_tool_use":true}`).Value(); !reflect.DeepEqual(got, want) {
				t.Fatalf("upstream count choice = %s, want auto with parallel flag", gjson.GetBytes(body, "tool_choice").Raw)
			}
			if got := gjson.GetBytes(body, "model").String(); got != model {
				t.Fatalf("upstream count model = %q, want routed %q", got, model)
			}
			if headers.Get("X-Api-Key") != "key-claude55" {
				t.Fatalf("api key auth not preserved: %v", headers)
			}
		})
	}
}
