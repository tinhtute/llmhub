package responses

import (
	"fmt"
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToClaude_ReasoningItemToThinkingBlock(t *testing.T) {
	signature := "claude_sig_request"
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + signature + `",
				"summary":[{"type":"summary_text","text":"internal reasoning"}]
			},
			{
				"type":"message",
				"role":"assistant",
				"content":[{"type":"output_text","text":"visible answer"}]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	assistant := root.Get("messages.0")
	if got := assistant.Get("role").String(); got != "assistant" {
		t.Fatalf("first message role = %q, want assistant. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.signature").String(); got != signature {
		t.Fatalf("thinking signature = %q, want %q", got, signature)
	}
	if got := assistant.Get("content.0.thinking").String(); got != "internal reasoning" {
		t.Fatalf("thinking text = %q, want internal reasoning", got)
	}
	if got := assistant.Get("content.1.type").String(); got != "text" {
		t.Fatalf("second content type = %q, want text. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.1.text").String(); got != "visible answer" {
		t.Fatalf("assistant text = %q, want visible answer", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SignatureOnlyReasoningFlushesBeforeUser(t *testing.T) {
	signature := "claude_sig_only"
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + signature + `",
				"summary":[]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	thinking := root.Get("messages.0.content.0")
	if got := thinking.Get("type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := thinking.Get("signature").String(); got != signature {
		t.Fatalf("thinking signature = %q, want %q", got, signature)
	}
	if got := thinking.Get("thinking").String(); got != "" {
		t.Fatalf("thinking text = %q, want empty", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_FunctionCallOutputContent(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		want       string
		structured bool
	}{
		{
			name:   "string remains string",
			output: `"plain text"`,
			want:   "plain text",
		},
		{
			name:       "empty array remains structured",
			output:     `[]`,
			want:       `[]`,
			structured: true,
		},
		{
			name: "OpenAI content parts map to structured Claude blocks",
			output: `[
				{"type":"input_text","text":"ok"},
				{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="},
				{"type":"input_file","file_data":"data:application/pdf;base64,JVBERi0x"}
			]`,
			want:       `[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}]`,
			structured: true,
		},
		{
			name: "valid Claude blocks remain structured",
			output: `[
				{"type":"text","text":"ok"},
				{"type":"image","source":{"type":"url","url":"https://example.com/image.png"}},
				{"type":"document","source":{"type":"text","media_type":"text/plain","data":"document"}}
			]`,
			want:       `[{"type":"text","text":"ok"},{"type":"image","source":{"type":"url","url":"https://example.com/image.png"}},{"type":"document","source":{"type":"text","media_type":"text/plain","data":"document"}}]`,
			structured: true,
		},
		{
			name:       "search result with empty content remains structured",
			output:     `[{"type":"search_result","source":"https://example.com","title":"Example","content":[]}]`,
			want:       `[{"type":"search_result","source":"https://example.com","title":"Example","content":[]}]`,
			structured: true,
		},
		{
			name:       "document content with image remains structured",
			output:     `[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"caption"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}}]`,
			want:       `[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"caption"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}}]`,
			structured: true,
		},
		{
			name:       "document string content remains structured",
			output:     `[{"type":"document","source":{"type":"content","content":"document"}}]`,
			want:       `[{"type":"document","source":{"type":"content","content":"document"}}]`,
			structured: true,
		},
		{
			name:       "document content empty array remains structured",
			output:     `[{"type":"document","source":{"type":"content","content":[]}}]`,
			want:       `[{"type":"document","source":{"type":"content","content":[]}}]`,
			structured: true,
		},
		{
			name:       "tool reference remains structured",
			output:     `[{"type":"tool_reference","tool_name":"search"}]`,
			want:       `[{"type":"tool_reference","tool_name":"search"}]`,
			structured: true,
		},
		{
			name:       "raw PDF file with PDF filename remains structured",
			output:     `[{"type":"input_file","filename":"report.pdf","file_data":"JVBERi0x"}]`,
			want:       `[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}]`,
			structured: true,
		},
		{
			name:   "unsupported input image media type uses compact JSON text",
			output: `[{"type":"input_image","image_url":"data:application/pdf;base64,JVBERi0x"}]`,
			want:   `[{"type":"input_image","image_url":"data:application/pdf;base64,JVBERi0x"}]`,
		},
		{
			name:   "malformed input image base64 uses compact JSON text",
			output: `[{"type":"input_image","image_url":"data:image/png;base64,not-base64"}]`,
			want:   `[{"type":"input_image","image_url":"data:image/png;base64,not-base64"}]`,
		},
		{
			name:   "unsupported input file media type uses compact JSON text",
			output: `[{"type":"input_file","file_data":"data:text/plain;base64,aGVsbG8="}]`,
			want:   `[{"type":"input_file","file_data":"data:text/plain;base64,aGVsbG8="}]`,
		},
		{
			name:   "raw non-base64 input file uses compact JSON text",
			output: `[{"type":"input_file","filename":"report.pdf","file_data":"not base64"}]`,
			want:   `[{"type":"input_file","filename":"report.pdf","file_data":"not base64"}]`,
		},
		{
			name:   "text document without text plain media type uses compact JSON text",
			output: `[{"type":"document","source":{"type":"text","data":"document"}}]`,
			want:   `[{"type":"document","source":{"type":"text","data":"document"}}]`,
		},
		{
			name:   "native image without source uses compact JSON text",
			output: `[{"type":"image"}]`,
			want:   `[{"type":"image"}]`,
		},
		{
			name:   "native image with unsupported media type uses compact JSON text",
			output: `[{"type":"image","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}]`,
			want:   `[{"type":"image","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}]`,
		},
		{
			name:   "native image with malformed base64 uses compact JSON text",
			output: `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"not-base64"}}]`,
			want:   `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"not-base64"}}]`,
		},
		{
			name:   "native document without source uses compact JSON text",
			output: `[{"type":"document"}]`,
			want:   `[{"type":"document"}]`,
		},
		{
			name:   "native document with unsupported base64 media type uses compact JSON text",
			output: `[{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"aGVsbG8="}}]`,
			want:   `[{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"aGVsbG8="}}]`,
		},
		{
			name:   "native document with malformed base64 uses compact JSON text",
			output: `[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"not-base64"}}]`,
			want:   `[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"not-base64"}}]`,
		},
		{
			name:   "search result without title uses compact JSON text",
			output: `[{"type":"search_result","source":"https://example.com","content":[]}]`,
			want:   `[{"type":"search_result","source":"https://example.com","content":[]}]`,
		},
		{
			name:   "search result with invalid content uses compact JSON text",
			output: `[{"type":"search_result","source":"https://example.com","title":"Example","content":[{"type":"image"}]}]`,
			want:   `[{"type":"search_result","source":"https://example.com","title":"Example","content":[{"type":"image"}]}]`,
		},
		{
			name:   "tool reference without tool name uses compact JSON text",
			output: `[{"type":"tool_reference","tool_name":""}]`,
			want:   `[{"type":"tool_reference","tool_name":""}]`,
		},
		{
			name:   "arbitrary object uses compact JSON text",
			output: `{ "b": 2, "a": [1, true] }`,
			want:   `{"b":2,"a":[1,true]}`,
		},
		{
			name:   "number uses JSON text",
			output: `42`,
			want:   `42`,
		},
		{
			name:   "boolean uses JSON text",
			output: `true`,
			want:   `true`,
		},
		{
			name:   "null uses JSON text",
			output: `null`,
			want:   `null`,
		},
		{
			name:   "unknown array uses compact JSON text",
			output: `[ { "type": "audio", "data": "abc" } ]`,
			want:   `[{"type":"audio","data":"abc"}]`,
		},
		{
			// A text part with no text is an empty text block: it is dropped
			// rather than embedded as JSON, because Anthropic rejects a
			// cache_control breakpoint on an empty text block and the block
			// carries nothing. This supersedes the pre-port expectation that the
			// blank part survived as compact JSON text.
			name:   "blank text block is dropped",
			output: `[ { "type": "text" } ]`,
			want:   ``,
		},
		{
			name:   "mixed array uses compact JSON text",
			output: `[ { "type": "input_text", "text": "ok" }, { "type": "unknown" } ]`,
			want:   `[{"type":"input_text","text":"ok"},{"type":"unknown"}]`,
		},
		{
			name:   "nested array uses compact JSON text",
			output: `[[ { "type": "text", "text": "nested" } ]]`,
			want:   `[[{"type":"text","text":"nested"}]]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"model":"claude-test","input":[{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":` + tt.output + `}]}`)
			out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
			content := gjson.GetBytes(out, "messages.1.content.0.content")

			if tt.structured {
				if !content.IsArray() {
					t.Fatalf("content = %s, want structured array. Output: %s", content.Raw, string(out))
				}
				if content.Raw != tt.want {
					t.Fatalf("content = %s, want %s", content.Raw, tt.want)
				}
				return
			}
			if content.Type != gjson.String {
				t.Fatalf("content type = %v, want string. Output: %s", content.Type, string(out))
			}
			if content.String() != tt.want {
				t.Fatalf("content = %q, want %q", content.String(), tt.want)
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToClaude_PreservesCallerSuppliedMetadataUserID(t *testing.T) {
	testCases := []struct {
		name     string
		rawJSON  string
		expected string
	}{
		{
			name:     "plain string",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"custom-resp-user-123"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			expected: "custom-resp-user-123",
		},
		{
			name:     "special characters and json string",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"foo\"bar\nbaz\\qux"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			expected: "foo\"bar\nbaz\\qux",
		},
		{
			name:     "claude code json format",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"{\"device_id\":\"0000000000000000000000000000000000000000000000000000000000000000\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			expected: `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","session_id":"11111111-2222-4333-8444-555555555555"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			out := ConvertOpenAIResponsesRequestToClaude("claude-test", []byte(tc.rawJSON), false)
			if !gjson.ValidBytes(out) {
				t.Fatalf("output is invalid json: %s", string(out))
			}
			got := gjson.GetBytes(out, "metadata.user_id").String()
			if got != tc.expected {
				t.Fatalf("metadata.user_id = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToClaude_PreservesUserField(t *testing.T) {
	raw := []byte(`{"model":"claude-test","user":"openai-resp-user-456","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	if !gjson.ValidBytes(out) {
		t.Fatalf("output is invalid json: %s", string(out))
	}
	got := gjson.GetBytes(out, "metadata.user_id").String()
	if got != "openai-resp-user-456" {
		t.Fatalf("metadata.user_id = %q, want %q", got, "openai-resp-user-456")
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DifferentSessionsProduceDifferentUserIDs(t *testing.T) {
	a := []byte(`{"model":"claude-test","prompt_cache_key":"resp-session-a","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	b := []byte(`{"model":"claude-test","prompt_cache_key":"resp-session-b","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	outA := ConvertOpenAIResponsesRequestToClaude("claude-test", a, false)
	outB := ConvertOpenAIResponsesRequestToClaude("claude-test", b, false)
	idA := gjson.GetBytes(outA, "metadata.user_id").String()
	idB := gjson.GetBytes(outB, "metadata.user_id").String()
	if idA == idB {
		t.Fatalf("different prompt_cache_key produced identical metadata.user_id: %q", idA)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DifferentUserContentWithSameSystemPrompt(t *testing.T) {
	rawA := []byte(`{
		"model": "claude-test",
		"instructions": "global instruction",
		"input": [
			{"type": "message", "role": "system", "content": "system context"},
			{"type": "message", "role": "user", "content": "user question A"}
		]
	}`)
	rawB := []byte(`{
		"model": "claude-test",
		"instructions": "global instruction",
		"input": [
			{"type": "message", "role": "system", "content": "system context"},
			{"type": "message", "role": "user", "content": "user question B"}
		]
	}`)
	outA := ConvertOpenAIResponsesRequestToClaude("claude-test", rawA, false)
	outB := ConvertOpenAIResponsesRequestToClaude("claude-test", rawB, false)
	idA := gjson.GetBytes(outA, "metadata.user_id").String()
	idB := gjson.GetBytes(outB, "metadata.user_id").String()
	if idA == "" || idB == "" || idA == "unknown" || idB == "unknown" {
		t.Fatalf("expected valid derived user_id, got idA=%q idB=%q", idA, idB)
	}
	if idA == idB {
		t.Fatalf("different user questions with same system prompt produced identical metadata.user_id: %q", idA)
	}
}

// Ported from upstream CLIProxyAPI commit ba2cdea3b919 (handle incomplete status
// and terminal state on max_tokens); covers local symbol
// defaultClaudeResponsesMaxTokensForModel via convertOpenAIResponsesRequestToClaude.
func TestConvertOpenAIResponsesRequestToClaude_FableMaxTokens(t *testing.T) {
	t.Run("defaults to 64k", func(t *testing.T) {
		out := ConvertOpenAIResponsesRequestToClaude(
			"claude-fable-5-1",
			[]byte(`{"model":"claude-fable-5-1","input":"hello"}`),
			true,
		)
		if got := gjson.GetBytes(out, "max_tokens").Int(); got != 64000 {
			t.Fatalf("max_tokens = %d, want %d; output=%s", got, 64000, out)
		}
	})

	t.Run("preserves explicit 128k limit", func(t *testing.T) {
		out := ConvertOpenAIResponsesRequestToClaude(
			"claude-fable-5-1",
			[]byte(`{"model":"claude-fable-5-1","max_output_tokens":128000,"input":"hello"}`),
			true,
		)
		if got := gjson.GetBytes(out, "max_tokens").Int(); got != 128000 {
			t.Fatalf("max_tokens = %d, want 128000; output=%s", got, out)
		}
	})

	t.Run("does not exceed registered model maximum", func(t *testing.T) {
		out := ConvertOpenAIResponsesRequestToClaude(
			"claude-3-5-haiku-20241022",
			[]byte(`{"model":"claude-3-5-haiku-20241022","input":"hello"}`),
			true,
		)
		if got := gjson.GetBytes(out, "max_tokens").Int(); got != 8192 {
			t.Fatalf("max_tokens = %d, want 8192; output=%s", got, out)
		}
	})

	t.Run("clamps explicit limit exceeding registered model maximum", func(t *testing.T) {
		out := ConvertOpenAIResponsesRequestToClaude(
			"claude-3-5-haiku-20241022",
			[]byte(`{"model":"claude-3-5-haiku-20241022","max_output_tokens":128000,"input":"hello"}`),
			true,
		)
		if got := gjson.GetBytes(out, "max_tokens").Int(); got != 8192 {
			t.Fatalf("max_tokens = %d, want 8192; output=%s", got, out)
		}
	})

	t.Run("null max_output_tokens retains default 64k", func(t *testing.T) {
		out := ConvertOpenAIResponsesRequestToClaude(
			"claude-fable-5-1",
			[]byte(`{"model":"claude-fable-5-1","max_output_tokens":null,"input":"hello"}`),
			true,
		)
		if got := gjson.GetBytes(out, "max_tokens").Int(); got != 64000 {
			t.Fatalf("max_tokens = %d, want 64000; output=%s", got, out)
		}
	})
}

func TestConvertOpenAIResponsesRequestToClaude_StandaloneToolOutputBecomesUserText(t *testing.T) {
	// Codex create_thread seeds a new thread with a function_call_output whose
	// call_id never appears as a function_call in the same input. Claude
	// rejects tool_result blocks without a matching tool_use, so it must
	// degrade to text.
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"function_call_output",
				"call_id":"toolu_1789312108939888000_16",
				"output":"<codex_delegation>Launched from another task.</codex_delegation>"
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"Reply with PROBE_OK."}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "tool_result" {
				t.Fatalf("unexpected tool_result for standalone output. Output: %s", string(out))
			}
			return true
		})
		return true
	})
	if got := root.Get("messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user. Output: %s", got, string(out))
	}
	if got := root.Get("messages.0.content.0.type").String(); got != "text" {
		t.Fatalf("messages.0.content.0.type = %q, want text. Output: %s", got, string(out))
	}
	if got := root.Get("messages.0.content.0.text").String(); got != "<codex_delegation>Launched from another task.</codex_delegation>" {
		t.Fatalf("messages.0.content.0.text = %q. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SynthesizesResultForDanglingToolUse(t *testing.T) {
	// A session that died mid-tool leaves a function_call with no output.
	// Anthropic requires a tool_result at the start of the next user message,
	// so one is synthesized.
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run the tests."}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"npm test\"}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Session was restarted; carry on."}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.#").Int(); got != 3 {
		t.Fatalf("message count = %d, want 3. Output: %s", got, string(out))
	}
	if got := root.Get("messages.1.content.0.type").String(); got != "tool_use" {
		t.Fatalf("messages.1.content.0.type = %q, want tool_use. Output: %s", got, string(out))
	}
	if got := root.Get("messages.2.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages.2.content.0.type = %q, want tool_result. Output: %s", got, string(out))
	}
	if got := root.Get("messages.2.content.0.tool_use_id").String(); got != "call_1" {
		t.Fatalf("synthesized tool_use_id = %q, want call_1. Output: %s", got, string(out))
	}
	if !root.Get("messages.2.content.0.is_error").Bool() {
		t.Fatalf("synthesized tool_result should be is_error. Output: %s", string(out))
	}
	if got := root.Get("messages.2.content.1.text").String(); got != "Session was restarted; carry on." {
		t.Fatalf("messages.2.content.1.text = %q, want 'Session was restarted; carry on.'. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SynthesizesResultForTrailingToolUse(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run ls."}]},
			{"type":"function_call","call_id":"call_tail","name":"exec","arguments":"{}"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.#").Int(); got != 3 {
		t.Fatalf("message count = %d, want 3. Output: %s", got, string(out))
	}
	if got := root.Get("messages.2.role").String(); got != "user" {
		t.Fatalf("messages.2.role = %q, want user. Output: %s", got, string(out))
	}
	if got := root.Get("messages.2.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages.2.content.0.type = %q, want tool_result. Output: %s", got, string(out))
	}
	if got := root.Get("messages.2.content.0.tool_use_id").String(); got != "call_tail" {
		t.Fatalf("messages.2.content.0.tool_use_id = %q, want call_tail. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_MovesToolResultsAheadOfInjectedText(t *testing.T) {
	// A heartbeat seed can land between a tool call and its output. Anthropic
	// requires tool_result blocks to lead the user message.
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run ls."}]},
			{"type":"function_call","call_id":"call_hb","name":"exec","arguments":"{}"},
			{"type":"function_call_output","id":"fco_seed","name":"automation_update","namespace":"codex_app","output":"<heartbeat>tick</heartbeat>"},
			{"type":"function_call_output","call_id":"call_hb","output":"a.txt"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.#").Int(); got != 3 {
		t.Fatalf("message count = %d, want 3. Output: %s", got, string(out))
	}
	blocks := root.Get("messages.2.content").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks in messages.2, got %d. Output: %s", len(blocks), string(out))
	}
	if got := blocks[0].Get("type").String(); got != "tool_result" || blocks[0].Get("tool_use_id").String() != "call_hb" {
		t.Fatalf("blocks[0] = %s, want tool_result for call_hb. Output: %s", blocks[0].Raw, string(out))
	}
	if got := blocks[0].Get("content").String(); got != "a.txt" {
		t.Fatalf("blocks[0].content = %q, want a.txt", got)
	}
	if got := blocks[1].Get("text").String(); got != "<heartbeat>tick</heartbeat>" {
		t.Fatalf("blocks[1].text = %q, want heartbeat text. Output: %s", got, string(out))
	}
	if got := blocks[2].Get("text").String(); got != "Continue." {
		t.Fatalf("blocks[2].text = %q, want Continue. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_LateOrphanToolResultFoldsToText(t *testing.T) {
	// An output can pass the raw-id gate but still land after an intervening
	// assistant message, where its tool_result no longer answers the
	// immediately preceding tool_use. The repair must fold it into text and
	// synthesize the missing answer for the real dangling call.
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run ls."}]},
			{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"wait"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"interlude"}]},
			{"type":"function_call_output","call_id":"call_a","output":"a.txt"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	// messages: user | assistant(tool_use call_a) | user | assistant(text) | user
	if got := root.Get("messages.#").Int(); got != 5 {
		t.Fatalf("message count = %d, want 5. Output: %s", got, string(out))
	}
	// The user message right after the dangling tool_use must lead with a
	// synthesized error tool_result for call_a.
	if got := root.Get("messages.2.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages.2.content.0.type = %q, want tool_result. Output: %s", got, string(out))
	}
	if got := root.Get("messages.2.content.0.tool_use_id").String(); got != "call_a" {
		t.Fatalf("messages.2.content.0.tool_use_id = %q, want call_a. Output: %s", got, string(out))
	}
	if !root.Get("messages.2.content.0.is_error").Bool() {
		t.Fatalf("synthesized tool_result should be is_error. Output: %s", string(out))
	}
	// The late real output must not remain a tool_result after the interlude
	// assistant message; it folds into plain text.
	last := root.Get("messages.4")
	if got := last.Get("role").String(); got != "user" {
		t.Fatalf("messages.4.role = %q, want user. Output: %s", got, string(out))
	}
	last.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_result" {
			t.Fatalf("late output must not remain a tool_result. Output: %s", string(out))
		}
		return true
	})
	if got := last.Get("content.0.text").String(); got != "a.txt" {
		t.Fatalf("messages.4.content.0.text = %q, want a.txt. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_EmptyStandaloneToolOutputKeepsMarker(t *testing.T) {
	// A standalone output with empty content must still produce a non-empty
	// user message; Anthropic rejects empty content arrays too. Both the
	// string form and the structured array form with an empty text part
	// collapse to the marker text.
	for _, output := range []string{`""`, `[{"type":"input_text","text":""}]`} {
		raw := []byte(`{
			"model":"claude-test",
			"input":[
				{"type":"function_call_output","call_id":"orphan","output":` + output + `}
			]
		}`)

		out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
		root := gjson.ParseBytes(out)

		if got := root.Get("messages.#").Int(); got != 1 {
			t.Fatalf("message count = %d, want 1 for output %s. Output: %s", got, output, string(out))
		}
		content := root.Get("messages.0.content")
		if content.IsArray() {
			if len(content.Array()) == 0 {
				t.Fatalf("empty content array in messages.0 for output %s. Output: %s", output, string(out))
			}
			if got := content.Get("0.type").String(); got != "text" {
				t.Fatalf("messages.0.content.0.type = %q, want text for output %s. Output: %s", got, output, string(out))
			}
			if got := content.Get("0.text").String(); strings.TrimSpace(got) == "" {
				t.Fatalf("empty text block in messages.0 for output %s. Output: %s", output, string(out))
			}
			continue
		}
		if got := content.String(); strings.TrimSpace(got) == "" {
			t.Fatalf("empty string content in messages.0 for output %s. Output: %s", output, string(out))
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SanitizedIDCollisionKeepsOrphanAsText(t *testing.T) {
	// "call.custom:1" and "call_custom_1" sanitize to the same Claude id, but
	// they are distinct calls. The unpaired raw id must degrade to text while
	// the real pairing stays a tool_result.
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run."}]},
			{"type":"function_call","call_id":"call.custom:1","name":"exec","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_custom_1","output":"unrelated context"},
			{"type":"function_call_output","call_id":"call.custom:1","output":"real result"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	blocks := root.Get("messages.2.content").Array()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks in messages.2, got %d. Output: %s", len(blocks), string(out))
	}
	if got := blocks[0].Get("type").String(); got != "tool_result" {
		t.Fatalf("blocks[0].type = %q, want tool_result. Output: %s", got, string(out))
	}
	if got := blocks[0].Get("content").String(); got != "real result" {
		t.Fatalf("blocks[0].content = %q, want 'real result'. Output: %s", got, string(out))
	}
	if got := blocks[1].Get("type").String(); got != "text" {
		t.Fatalf("blocks[1].type = %q, want text. Output: %s", got, string(out))
	}
	if got := blocks[1].Get("text").String(); got != "unrelated context" {
		t.Fatalf("blocks[1].text = %q, want 'unrelated context'. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_EmptyLateOrphanToolResultKeepsNonEmptyUserMessage(t *testing.T) {
	// An empty late orphan output folds to nothing; the user message must
	// still carry a marker instead of degenerating to content:[] which
	// Anthropic also rejects.
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run the tool."}]},
			{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Wait."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Interlude."}]},
			{"type":"function_call_output","call_id":"call_a","output":""}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() && len(content.Array()) == 0 {
			t.Fatalf("empty content array in message. Output: %s", string(out))
		}
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "tool_result" && block.Get("tool_use_id").String() == "call_a" && !block.Get("is_error").Bool() {
				t.Fatalf("late empty orphan must not remain a tool_result. Output: %s", string(out))
			}
			return true
		})
		return true
	})
	last := root.Get("messages.4")
	if got := last.Get("role").String(); got != "user" {
		t.Fatalf("messages.4.role = %q, want user. Output: %s", got, string(out))
	}
	if got := last.Get("content.0.text").String(); got != "Tool result was empty." {
		t.Fatalf("messages.4.content.0.text = %q, want marker text. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_StandaloneToolOutputDropsEmptyTextInMixedArray(t *testing.T) {
	// A standalone output whose array mixes an empty text part with a real
	// one must not carry the empty block into the user message.
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"function_call_output","call_id":"orphan","output":[{"type":"input_text","text":""},{"type":"input_text","text":"context"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "text" && strings.TrimSpace(block.Get("text").String()) == "" {
				t.Fatalf("empty text block emitted. Output: %s", string(out))
			}
			if block.Get("type").String() == "tool_result" {
				t.Fatalf("standalone output must not remain a tool_result. Output: %s", string(out))
			}
			return true
		})
		return true
	})
	if got := root.Get("messages.0.content").String(); got != "context" {
		t.Fatalf("messages.0.content = %q, want 'context'. Output: %s", got, string(out))
	}
}

func TestClaudeMessageInvariantProblems(t *testing.T) {
	msg := func(raw string) []byte { return []byte(raw) }
	clean := [][]byte{
		msg(`{"role":"user","content":"hi"}`),
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"exec","input":{}}]}`),
		msg(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"go on"}]}`),
	}
	if got := claudeMessageInvariantProblems(clean); len(got) != 0 {
		t.Fatalf("clean history reported problems: %v", got)
	}
	broken := [][]byte{
		msg(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t0","content":"orphan"}]}`),
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"exec","input":{}}]}`),
		msg(`{"role":"user","content":[{"type":"text","text":"x"},{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}`),
	}
	got := claudeMessageInvariantProblems(broken)
	want := []string{"tool_result t0 has no tool_use", "tool_result after non-tool_result block", "tool_use t1 has no tool_result", "tool_result t2 has no tool_use"}
	for _, fragment := range want {
		found := false
		for _, problem := range got {
			if strings.Contains(problem, fragment) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a problem containing %q, got %v", fragment, got)
		}
	}
	firstNotUser := [][]byte{
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"exec","input":{}}]}`),
	}
	got = claudeMessageInvariantProblems(firstNotUser)
	if len(got) == 0 || !strings.Contains(got[0], "first message is not user") {
		t.Fatalf("expected first-message problem, got %v", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DropsApplyPatchCustomTool(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[
			{
				"type":"custom",
				"name":"apply_patch",
				"description":"Use the apply_patch tool to edit files.",
				"format":{"type":"grammar","syntax":"lark","definition":"start: patch"}
			},
			{
				"type":"function",
				"name":"exec_command",
				"description":"Runs a command.",
				"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("tools.#").Int(); got != 1 {
		t.Fatalf("tools count = %d, want 1. Output: %s", got, string(out))
	}
	if got := root.Get("tools.0.name").String(); got != "exec_command" {
		t.Fatalf("tools.0.name = %q, want exec_command. Output: %s", got, string(out))
	}
	if got := root.Get("tools.#(name==\"apply_patch\")").Raw; got != "" {
		t.Fatalf("apply_patch custom tool should be dropped. Output: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_NormalizesRootToolSchemaUnion(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{
			"type":"function",
			"name":"lookup",
			"parameters":{
				"type":"object",
				"properties":{"query":{"type":"string"},"id":{"type":"string"}},
				"oneOf":[{"required":["query"]},{"required":["id"]}]
			}
		}]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	schema := gjson.GetBytes(out, "tools.0.input_schema")

	if got := schema.Get("type").String(); got != "object" {
		t.Fatalf("input_schema.type = %q, want object. Output: %s", got, string(out))
	}
	if schema.Get("oneOf").Exists() {
		t.Fatalf("input_schema should not contain root oneOf. Output: %s", string(out))
	}
	if !schema.Get("properties.query").Exists() || !schema.Get("properties.id").Exists() {
		t.Fatalf("input_schema should preserve query and id properties. Output: %s", string(out))
	}
	if schema.Get("required").Exists() {
		t.Fatalf("input_schema should not merge alternative required fields. Output: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_MergesAdditionalToolsAndPrefersTopLevel(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"tools":[
			{
				"type":"function",
				"name":"exec",
				"description":"top-level exec",
				"parameters":{"type":"object","properties":{"command":{"type":"string"}}}
			},
			{
				"type":"namespace",
				"name":"collaboration",
				"tools":[{"type":"function","name":"spawn","description":"top-level spawn","parameters":{"type":"object","properties":{}}}]
			}
		],
		"input":[
			{
				"type":"additional_tools",
				"role":"developer",
				"tools":[
					{"type":"custom","name":"exec","description":"additional exec"},
					{"type":"function","name":"wait","parameters":{"type":"object","properties":{}}},
					{"type":"namespace","name":"collaboration","tools":[
						{"type":"function","name":"spawn","parameters":{"type":"object","properties":{}}},
						{"type":"custom","name":"send","description":"send a message"}
					]}
				]
			},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`)

	root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false))
	if got := root.Get("tools.#").Int(); got != 4 {
		t.Fatalf("tools count = %d, want 4; output=%s", got, root.Raw)
	}
	if got := root.Get(`tools.#(name=="exec").description`).String(); got != "top-level exec" {
		t.Fatalf("exec description = %q, want top-level exec", got)
	}
	if got := root.Get(`tools.#(name=="wait").name`).String(); got != "wait" {
		t.Fatalf("additional function name = %q, want wait", got)
	}
	if got := root.Get(`tools.#(name=="collaboration__spawn").name`).String(); got != "collaboration__spawn" {
		t.Fatalf("namespace function name = %q, want collaboration__spawn", got)
	}
	custom := root.Get(`tools.#(name=="collaboration__send")`)
	if !custom.Exists() {
		t.Fatal("missing namespace custom tool")
	}
	if got := custom.Get("input_schema.properties.input.type").String(); got != "string" {
		t.Fatalf("custom input schema type = %q, want string", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DeduplicatesExpandedToolNames(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"tools":[{"type":"function","name":"collaboration__send","description":"top-level send","parameters":{"type":"object","properties":{}}}],
		"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[
			{"type":"function","name":"send","description":"additional send","parameters":{"type":"object","properties":{}}},
			{"type":"function","name":"other","parameters":{"type":"object","properties":{}}}
		]}]}]
	}`)

	root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false))
	if got := root.Get("tools.#").Int(); got != 2 {
		t.Fatalf("tools count = %d, want 2; output=%s", got, root.Raw)
	}
	if got := root.Get(`tools.#(name=="collaboration__send").description`).String(); got != "top-level send" {
		t.Fatalf("duplicate final name description = %q, want top-level send", got)
	}
	if !root.Get(`tools.#(name=="collaboration__other")`).Exists() {
		t.Fatal("unique namespace child was dropped")
	}
	customNames := responsesCustomToolNames(raw)
	if _, ok := customNames["collaboration__send"]; ok {
		t.Fatal("final-name collision should keep the top-level function type")
	}
	name, namespace := resolveResponsesFunctionCallIdentity(util.CollectResponsesToolWinners(gjson.ParseBytes(raw)), "collaboration__send")
	if name != "collaboration__send" || namespace != "" {
		t.Fatalf("final-name collision namespace = (%q, %q), want (collaboration__send, empty)", name, namespace)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DirectToolWinsOverEarlierNamespaceCollision(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"tools":[
			{"type":"namespace","name":"n","tools":[{"type":"function","name":"x","parameters":{"type":"object","properties":{}}}]},
			{"type":"custom","name":"n__x"}
		],
		"tool_choice":{"type":"custom","name":"n__x"}
	}`)

	root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false))
	if got := root.Get("tools.#").Int(); got != 1 {
		t.Fatalf("tools count = %d, want 1; output=%s", got, root.Raw)
	}
	if got := root.Get("tools.0.name").String(); got != "n__x" {
		t.Fatalf("winning tool name = %q, want n__x", got)
	}
	if got := root.Get("tools.0.input_schema.properties.input.type").String(); got != "string" {
		t.Fatalf("winning tool schema type = %q, want string for custom tool", got)
	}
	if got := root.Get("tool_choice.name").String(); got != "n__x" {
		t.Fatalf("tool_choice.name = %q, want n__x; output=%s", got, root.Raw)
	}
	if _, ok := responsesCustomToolNames(raw)["n__x"]; !ok {
		t.Fatal("winning direct custom tool was not classified as custom")
	}
}

func TestConvertOpenAIResponsesRequestToClaude_PrefersDirectToolAcrossAdditionalSources(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"x","description":"namespace x","parameters":{"type":"object","properties":{}}}]}]},
			{"type":"additional_tools","tools":[{"type":"custom","name":"n__x","description":"direct x"}]}
		]
	}`)

	root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false))
	if got := root.Get("tools.#").Int(); got != 1 {
		t.Fatalf("tools count = %d, want 1; output=%s", got, root.Raw)
	}
	tool := root.Get("tools.0")
	if got := tool.Get("name").String(); got != "n__x" {
		t.Fatalf("winning tool name = %q, want n__x", got)
	}
	if got := tool.Get("description").String(); got != "direct x" {
		t.Fatalf("winning tool description = %q, want direct x", got)
	}
	if got := tool.Get("input_schema.properties.input.type").String(); got != "string" {
		t.Fatalf("winning tool schema type = %q, want string for custom tool", got)
	}
	if _, ok := responsesCustomToolNames(raw)["n__x"]; !ok {
		t.Fatal("direct custom tool should win classification across additional sources")
	}
}

func TestConvertOpenAIResponsesRequestToClaude_PreservesToolDeclarationOrder(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"tools":[
			{"type":"function","name":"first","parameters":{"type":"object","properties":{}}},
			{"type":"namespace","name":"n","tools":[{"type":"function","name":"middle","parameters":{"type":"object","properties":{}}}]},
			{"type":"function","name":"last","parameters":{"type":"object","properties":{}}}
		]
	}`)

	root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false))
	want := []string{"first", "n__middle", "last"}
	got := root.Get("tools.#.name").Array()
	if len(got) != len(want) {
		t.Fatalf("tools count = %d, want %d; output=%s", len(got), len(want), root.Raw)
	}
	for i, wantName := range want {
		if got[i].String() != wantName {
			t.Errorf("tools[%d].name = %q, want %q", i, got[i].String(), wantName)
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ReplaysCustomToolCallHistory(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"custom_tool_call","call_id":"call.custom:1","name":"exec","input":"pwd"},
			{"type":"custom_tool_call_output","call_id":"call.custom:1","output":"/workspace"}
		]
	}`)

	root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false))
	toolUse := root.Get("messages.0.content.0")
	if got := toolUse.Get("type").String(); got != "tool_use" {
		t.Fatalf("tool use type = %q, want tool_use; output=%s", got, root.Raw)
	}
	if got := toolUse.Get("id").String(); got != "call_custom_1" {
		t.Fatalf("tool use id = %q, want call_custom_1", got)
	}
	if got := toolUse.Get("input.input").String(); got != "pwd" {
		t.Fatalf("custom tool input = %q, want pwd", got)
	}
	toolResult := root.Get("messages.1.content.0")
	if got := toolResult.Get("type").String(); got != "tool_result" {
		t.Fatalf("tool result type = %q, want tool_result", got)
	}
	if got := toolResult.Get("tool_use_id").String(); got != "call_custom_1" {
		t.Fatalf("tool result id = %q, want call_custom_1", got)
	}
	if got := toolResult.Get("content").String(); got != "/workspace" {
		t.Fatalf("tool result content = %q, want /workspace", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ReplaysNamespacedFunctionCallHistory(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp__node_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object","properties":{}}}]}]},
			{"type":"function_call","call_id":"call.namespace","name":"js","namespace":"mcp__node_repl","arguments":"{\"code\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call.namespace","output":"ok"}
		]
	}`)

	root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false))
	if !root.Get(`tools.#(name=="mcp__node_repl__js")`).Exists() {
		t.Fatal("missing qualified namespace tool declaration")
	}
	toolUse := root.Get("messages.0.content.0")
	if got := toolUse.Get("name").String(); got != "mcp__node_repl__js" {
		t.Fatalf("historical tool_use name = %q, want mcp__node_repl__js", got)
	}
	if got := root.Get("messages.1.content.0.tool_use_id").String(); got != "call_namespace" {
		t.Fatalf("historical tool_result id = %q, want call_namespace", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_MapsCustomAndNamespacedToolChoice(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantToolName string
	}{
		{
			name: "custom",
			raw: `{
				"model":"claude-test",
				"tools":[{"type":"custom","name":"exec"}],
				"tool_choice":{"type":"custom","name":"exec"}
			}`,
			wantToolName: "exec",
		},
		{
			name: "namespace",
			raw: `{
				"model":"claude-test",
				"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp__node_repl","tools":[{"type":"function","name":"js"}]}]}],
				"tool_choice":{"type":"function","name":"js","namespace":"mcp__node_repl"}
			}`,
			wantToolName: "mcp__node_repl__js",
		},
		{
			name: "top-level-short-name-wins",
			raw: `{
				"model":"claude-test",
				"tools":[{"type":"function","name":"foo"}],
				"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp__tools","tools":[{"type":"function","name":"foo"}]}]}],
				"tool_choice":{"type":"function","name":"foo"}
			}`,
			wantToolName: "foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", []byte(tt.raw), false))
			if got := root.Get("tool_choice.type").String(); got != "tool" {
				t.Fatalf("tool_choice.type = %q, want tool; output=%s", got, root.Raw)
			}
			if got := root.Get("tool_choice.name").String(); got != tt.wantToolName {
				t.Fatalf("tool_choice.name = %q, want %q", got, tt.wantToolName)
			}
		})
	}
}

func TestQualifyResponsesNamespaceToolNameAvoidsPrefixCollision(t *testing.T) {
	tests := []struct {
		namespace string
		child     string
		want      string
	}{
		{namespace: "collab", child: "collaboration", want: "collab__collaboration"},
		{namespace: "collab", child: "collab__send", want: "collab__send"},
		{namespace: "collab__", child: "send", want: "collab__send"},
		{namespace: "mcp__node_repl", child: "mcp__node_repl__js", want: "mcp__node_repl__js"},
	}

	for _, tt := range tests {
		got := qualifyResponsesNamespaceToolName(tt.namespace, tt.child)
		if got != tt.want {
			t.Errorf("qualifyResponsesNamespaceToolName(%q, %q) = %q, want %q", tt.namespace, tt.child, got, tt.want)
		}
	}

	raw := []byte(`{
		"tools":[{"type":"namespace","name":"collab","tools":[{"type":"function","name":"collaboration"}]}]
	}`)
	root := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false))
	if got := root.Get("tools.0.name").String(); got != "collab__collaboration" {
		t.Fatalf("qualified tool declaration = %q, want collab__collaboration", got)
	}
}

func TestResolveResponsesFunctionCallIdentityFromAdditionalTools(t *testing.T) {
	raw := []byte(`{
		"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp__node_repl","tools":[{"type":"function","name":"js"}]}]}]
	}`)

	name, namespace := resolveResponsesFunctionCallIdentity(util.CollectResponsesToolWinners(gjson.ParseBytes(raw)), "mcp__node_repl__js")
	if name != "js" {
		t.Fatalf("name = %q, want js", name)
	}
	if namespace != "mcp__node_repl" {
		t.Fatalf("namespace = %q, want mcp__node_repl", namespace)
	}
}

// Ported from upstream b989e34881c7 (issue #6028), adapted to local semantics:
// llmhub emits `instructions` as a leading user message rather than a Claude
// `system` array.
func TestConvertOpenAIResponsesRequestToClaude_StringInput(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-6",
		"input": "hi",
		"max_output_tokens": 16,
		"stream": false
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message in translated Claude request, got %d. Output: %s", len(messages), string(out))
	}

	msg := messages[0]
	if got := msg.Get("role").String(); got != "user" {
		t.Fatalf("expected message role %q, got %q", "user", got)
	}

	content := msg.Get("content")
	var text string
	if content.IsArray() {
		parts := content.Array()
		if len(parts) != 1 {
			t.Fatalf("expected 1 content part, got %d", len(parts))
		}
		if got := parts[0].Get("type").String(); got != "text" {
			t.Fatalf("expected block type text, got %s", got)
		}
		text = parts[0].Get("text").String()
	} else {
		text = content.String()
	}
	if text != "hi" {
		t.Fatalf("expected user message content %q, got %q", "hi", text)
	}

	// String input alongside instructions keeps both messages, instructions first.
	withInstructions := []byte(`{
		"model": "claude-sonnet-4-6",
		"instructions": "Be concise.",
		"input": "hello world",
		"max_output_tokens": 16,
		"stream": false
	}`)
	outWithInstr := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", withInstructions, false)
	messagesWithInstr := gjson.GetBytes(outWithInstr, "messages").Array()
	if len(messagesWithInstr) != 2 {
		t.Fatalf("expected 2 messages (instructions + user input), got %d. Output: %s", len(messagesWithInstr), string(outWithInstr))
	}
	if messagesWithInstr[0].Get("role").String() != "user" || messagesWithInstr[0].Get("content").String() != "Be concise." {
		t.Fatalf("unexpected instructions message in output: %s", string(outWithInstr))
	}
	if messagesWithInstr[1].Get("role").String() != "user" || messagesWithInstr[1].Get("content").String() != "hello world" {
		t.Fatalf("unexpected user message in output: %s", string(outWithInstr))
	}

	// Also verify string input with quotes, newlines, and unicode.
	complexInput := []byte(`{
		"model": "claude-sonnet-4-6",
		"input": "line 1\n\"line 2\"\n你好，世界 🌍",
		"max_output_tokens": 16,
		"stream": false
	}`)
	outComplex := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", complexInput, false)
	complexMsgs := gjson.GetBytes(outComplex, "messages").Array()
	if len(complexMsgs) != 1 {
		t.Fatalf("expected 1 message in translated Claude request with complex input, got %d. Output: %s", len(complexMsgs), string(outComplex))
	}
	if got := complexMsgs[0].Get("content").String(); got != "line 1\n\"line 2\"\n你好，世界 🌍" {
		t.Fatalf("unexpected content for complex input, got %q", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_UnsupportedPrefillModelsStripTrailingAssistant(t *testing.T) {
	unsupportedModels := []string{
		"claude-fable-5",
		"claude-opus-5",
		"claude-sonnet-4-6",
		"fable",
		"opus-5",
		"sonnet-4.6",
		"anthropic/claude-opus-5-thinking",
		"claude-sonnet-4.6",
		"claude-sonnet-4-7",
		"claude-sonnet-4-10",
		"claude-sonnet-5",
		"claude-opus-6",
		"claude-opus-5.1",
		"claude-sonnet-4-6-20260217",
		" ANTHROPIC/CLAUDE-OPUS-5-THINKING ",
		"claude-haiku-5-5",
		"claude-haiku-5",
		"claude-haiku-5.5",
		"haiku-5-5",
		"anthropic/claude-haiku-5-5",
	}
	for _, model := range unsupportedModels {
		t.Run(model, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{
				"model": %q,
				"input": [
					{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hello"}]},
					{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "progress update"}]}
				]
			}`, model))
			out := ConvertOpenAIResponsesRequestToClaude(model, raw, false)
			messages := gjson.GetBytes(out, "messages").Array()
			if len(messages) != 1 {
				t.Fatalf("expected 1 message after stripping trailing assistant prefill for model %s, got %d: %s", model, len(messages), string(out))
			}
			if got := messages[0].Get("role").String(); got != "user" {
				t.Fatalf("expected final message role = %q, got %q", "user", got)
			}
			if got := messages[0].Get("content").String(); got != "hello" {
				t.Fatalf("expected content = %q, got %q", "hello", got)
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SupportedPrefillModelsPreserveAssistantPrefill(t *testing.T) {
	supportedModels := []string{
		"claude-sonnet-4-5",
		"claude-haiku-4-5",
		"claude-haiku-5foo",
		"claude-haiku-20260217",
		"claude-3-opus-20240229",
		"claude-opus-20240229",
		"claude-sonnet-4-20260217",
		"claude-sonnet-4.5",
		"not-a-fable-model",
		"my-custom-opus-5-wrapper",
		"my-sonnet-4-6-wrapper",
		"claude-fabled-5",
	}
	for _, model := range supportedModels {
		t.Run(model, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{
				"model": %q,
				"input": [
					{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hello"}]},
					{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "progress update"}]}
				]
			}`, model))
			out := ConvertOpenAIResponsesRequestToClaude(model, raw, false)
			messages := gjson.GetBytes(out, "messages").Array()
			if len(messages) != 2 {
				t.Fatalf("expected 2 messages for supported prefill model %s, got %d: %s", model, len(messages), string(out))
			}
			if got := messages[1].Get("role").String(); got != "assistant" {
				t.Fatalf("expected trailing message role = %q, got %q", "assistant", got)
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToClaude_IncompleteToolOutputIsError(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4o",
		"tools": [{"type":"function","name":"get_weather","description":"Get weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
		"input": [
			{"type":"message","role":"user","content":"Weather in Atlantis?"},
			{"type":"function_call","call_id":"call_err1","name":"get_weather","arguments":"{\"city\":\"Atlantis\"}"},
			{"type":"function_call_output","call_id":"call_err1","status":"incomplete","output":"City not found"},
			{"type":"message","role":"user","content":"Did it fail?"}
		],
		"max_output_tokens": 128,
		"stream": false
	}`

	result := ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	var toolResult gjson.Result
	found := false
	resultJSON.Get("messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "tool_result" && block.Get("tool_use_id").String() == "call_err1" {
				toolResult = block
				found = true
				return false
			}
			return true
		})
		return !found
	})
	if !found {
		t.Fatalf("expected tool_result for call_err1. Output: %s", string(result))
	}
	if got := toolResult.Get("content").String(); got != "City not found" {
		t.Fatalf("tool_result content = %q, want %q. Output: %s", got, "City not found", string(result))
	}
	if !toolResult.Get("is_error").Bool() {
		t.Fatalf("tool_result is_error should be true for status incomplete. Output: %s", string(result))
	}
}

// TestConvertOpenAIResponsesRequestToClaudeDropsEmptyTextBlocks pins that empty
// and whitespace-only text from an OpenAI Responses request never becomes a
// Claude text block.
func TestConvertOpenAIResponsesRequestToClaudeDropsEmptyTextBlocks(t *testing.T) {
	raw := []byte(`{"model":"claude-test","input":[
		{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_text","text":"  "}]},
		{"role":"assistant","content":[{"type":"output_text","text":""}]}
	]}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages")
	texts := 0
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.Type == gjson.String {
			texts++
			if strings.TrimSpace(content.String()) == "" {
				t.Fatalf("blank string content survived: %s", out)
			}
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			texts++
			if strings.TrimSpace(part.Get("text").String()) == "" {
				t.Fatalf("blank text block survived: %s", out)
			}
			return true
		})
		return true
	})
	if texts != 1 {
		t.Fatalf("text blocks = %d, want 1: %s", texts, out)
	}
}
