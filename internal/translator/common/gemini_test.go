package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

// Ported from upstream CLIProxyAPI commit 0fe19ede90a4 ("preserve Gemini prompt
// cache by demoting mid-session developer messages").
func TestMergeAdjacentGeminiUserContents(t *testing.T) {
	t.Run("merges consecutive pure text user turns", func(t *testing.T) {
		contents := [][]byte{
			[]byte(`{"role":"user","parts":[{"text":"prompt 1"}]}`),
			[]byte(`{"role":"user","parts":[{"text":"prompt 2"}]}`),
		}
		merged := MergeAdjacentGeminiUserContents(contents)
		if len(merged) != 1 {
			t.Fatalf("expected 1 merged user turn, got %d", len(merged))
		}
		parts := gjson.GetBytes(merged[0], "parts").Array()
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
	})

	t.Run("does not merge across functionResponse boundaries", func(t *testing.T) {
		contents := [][]byte{
			[]byte(`{"role":"user","parts":[{"functionResponse":{"name":"test","response":{"result":"ok"}}}]}`),
			[]byte(`{"role":"user","parts":[{"text":"user note"}]}`),
			[]byte(`{"role":"user","parts":[{"function_response":{"name":"test2","response":{"result":"ok2"}}}]}`),
		}
		merged := MergeAdjacentGeminiUserContents(contents)
		if len(merged) != 3 {
			t.Fatalf("expected 3 separate turns preserving functionResponse, got %d", len(merged))
		}
	})
}

// Ported from upstream CLIProxyAPI commit 4fde97f4144a ("reorder trailing text
// before function responses in user turns").
func TestReorderGeminiUserParts(t *testing.T) {
	t.Run("text after functionResponse is moved before it", func(t *testing.T) {
		parts := [][]byte{
			[]byte(`{"functionResponse":{"name":"run","response":{"result":"ok"}}}`),
			[]byte(`{"text":"system reminder"}`),
		}
		reordered := ReorderGeminiUserParts(parts)
		if len(reordered) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(reordered))
		}
		if !gjson.GetBytes(reordered[0], "text").Exists() {
			t.Fatalf("expected text part first, got %s", reordered[0])
		}
		if !gjson.GetBytes(reordered[1], "functionResponse").Exists() {
			t.Fatalf("expected functionResponse part last, got %s", reordered[1])
		}
	})

	t.Run("no reordering needed without trailing text", func(t *testing.T) {
		parts := [][]byte{
			[]byte(`{"text":"hello"}`),
			[]byte(`{"functionResponse":{"name":"run","response":{"result":"ok"}}}`),
		}
		reordered := ReorderGeminiUserParts(parts)
		if !gjson.GetBytes(reordered[0], "text").Exists() || !gjson.GetBytes(reordered[1], "functionResponse").Exists() {
			t.Fatalf("unexpected reorder: %v", reordered)
		}
	})
}

// Ported from upstream CLIProxyAPI v7.3.3 (internal/translator/common/gemini.go,
// MergeAdjacentGeminiContents): consecutive user turns merge, and the merge
// reorders text before functionResponse parts.
func TestMergeAdjacentGeminiContents(t *testing.T) {
	t.Run("merges consecutive user turns", func(t *testing.T) {
		contents := [][]byte{
			[]byte(`{"role":"user","parts":[{"text":"prompt 1"}]}`),
			[]byte(`{"role":"user","parts":[{"text":"prompt 2"}]}`),
		}
		merged := MergeAdjacentGeminiContents(contents)
		if len(merged) != 1 {
			t.Fatalf("expected 1 merged user turn, got %d", len(merged))
		}
	})

	t.Run("does not merge consecutive model turns", func(t *testing.T) {
		contents := [][]byte{
			[]byte(`{"role":"model","parts":[{"text":"a"}]}`),
			[]byte(`{"role":"model","parts":[{"text":"b"}]}`),
		}
		merged := MergeAdjacentGeminiContents(contents)
		if len(merged) != 2 {
			t.Fatalf("model turns must stay unmerged, got %d", len(merged))
		}
	})

	t.Run("merged user turn reorders text before functionResponse", func(t *testing.T) {
		contents := [][]byte{
			[]byte(`{"role":"user","parts":[{"functionResponse":{"name":"run","response":{"result":"ok"}}}]}`),
			[]byte(`{"role":"user","parts":[{"text":"reminder"}]}`),
		}
		merged := MergeAdjacentGeminiContents(contents)
		if len(merged) != 1 {
			t.Fatalf("expected merged user turn, got %d", len(merged))
		}
		parts := gjson.GetBytes(merged[0], "parts").Array()
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
		if !parts[0].Get("text").Exists() || !parts[1].Get("functionResponse").Exists() {
			t.Fatalf("expected text before functionResponse, got %s", gjson.GetBytes(merged[0], "parts").Raw)
		}
	})
}
func TestSplitGeminiFunctionResponseTurns(t *testing.T) {
	t.Run("splits mixed user turn with response first", func(t *testing.T) {
		input := [][]byte{
			[]byte(`{"role":"model","parts":[{"functionCall":{"name":"Bash"}}]}`),
			[]byte(`{"role":"user","parts":[{"text":"reminder"},{"functionResponse":{"name":"Bash"}}]}`),
		}
		got := SplitGeminiFunctionResponseTurns(input)
		if len(got) != 3 {
			t.Fatalf("got %d turns, want 3", len(got))
		}
		if !ContentHasGeminiFunctionResponse(got[1]) || ContentHasGeminiFunctionResponse(got[2]) {
			t.Fatalf("function response was not isolated before text: %s / %s", got[1], got[2])
		}
	})

	t.Run("keeps function response only turn intact", func(t *testing.T) {
		input := [][]byte{[]byte(`{"role":"user","parts":[{"functionResponse":{"name":"Bash"}}]}`)}
		got := SplitGeminiFunctionResponseTurns(input)
		if len(got) != 1 || string(got[0]) != string(input[0]) {
			t.Fatalf("got %s, want original turn", JoinRawArray(got))
		}
	})

	t.Run("handles multiple responses and alternate key", func(t *testing.T) {
		input := [][]byte{[]byte(`{"role":"user","parts":[{"text":"before"},{"functionResponse":{"name":"one"}},{"function_response":{"name":"two"}},{"text":"after"}]}`)}
		got := SplitGeminiFunctionResponseTurns(input)
		if len(got) != 2 || len(gjson.GetBytes(got[0], "parts").Array()) != 2 || len(gjson.GetBytes(got[1], "parts").Array()) != 2 {
			t.Fatalf("unexpected split: %s", JoinRawArray(got))
		}
		if !ContentHasGeminiFunctionResponse(got[0]) || ContentHasGeminiFunctionResponse(got[1]) {
			t.Fatalf("responses were not isolated: %s", JoinRawArray(got))
		}
	})

	t.Run("preserves non-user and empty turns", func(t *testing.T) {
		input := [][]byte{
			[]byte(`{"role":"model","parts":[{"functionResponse":{"name":"model-response"}}]}`),
			[]byte(`{"role":"user","parts":[]}`),
		}
		got := SplitGeminiFunctionResponseTurns(input)
		if len(got) != len(input) || string(got[0]) != string(input[0]) || string(got[1]) != string(input[1]) {
			t.Fatalf("turns were changed: %s", JoinRawArray(got))
		}
	})

	t.Run("splits each mixed turn independently", func(t *testing.T) {
		input := [][]byte{
			[]byte(`{"role":"user","parts":[{"text":"one"},{"functionResponse":{"name":"one"}}]}`),
			[]byte(`{"role":"user","parts":[{"text":"two"},{"functionResponse":{"name":"two"}}]}`),
		}
		got := SplitGeminiFunctionResponseTurns(input)
		if len(got) != 4 {
			t.Fatalf("got %d turns, want 4: %s", len(got), JoinRawArray(got))
		}
		for i := range got {
			if ContentHasGeminiFunctionResponse(got[i]) != (i%2 == 0) {
				t.Fatalf("unexpected turn %d: %s", i, got[i])
			}
		}
	})

	t.Run("hoists function response before intervening text reminder", func(t *testing.T) {
		input := [][]byte{
			[]byte(`{"role":"model","parts":[{"functionCall":{"name":"Bash"}}]}`),
			[]byte(`{"role":"user","parts":[{"text":"system reminder"}]}`),
			[]byte(`{"role":"user","parts":[{"functionResponse":{"name":"Bash"}}]}`),
		}
		got := SplitGeminiFunctionResponseTurns(input)
		if len(got) != 3 {
			t.Fatalf("got %d turns, want 3", len(got))
		}
		if !ContentHasGeminiFunctionResponse(got[1]) {
			t.Fatalf("expected function response immediately after model turn, got %s", got[1])
		}
		if gotText := gjson.GetBytes(got[2], "parts.0.text").String(); gotText != "system reminder" {
			t.Fatalf("expected text reminder after function response, got %s", got[2])
		}
	})

	t.Run("merges multiple function response turns into single turn following model", func(t *testing.T) {
		input := [][]byte{
			[]byte(`{"role":"model","parts":[{"functionCall":{"name":"A"}},{"functionCall":{"name":"B"}}]}`),
			[]byte(`{"role":"user","parts":[{"functionResponse":{"name":"A"}}]}`),
			[]byte(`{"role":"user","parts":[{"functionResponse":{"name":"B"}}]}`),
		}
		got := SplitGeminiFunctionResponseTurns(input)
		if len(got) != 2 {
			t.Fatalf("got %d turns, want 2", len(got))
		}
		if len(gjson.GetBytes(got[1], "parts").Array()) != 2 {
			t.Fatalf("expected 2 parts in single function response turn, got %s", got[1])
		}
	})
}

func TestContainsJSONRef(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "top-level $ref object",
			json: `{"$ref": "#/components/schemas/ErrorModel"}`,
			want: true,
		},
		{
			name: "nested $ref in object",
			json: `{"responses":{"400":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/ErrorModel"}}}}}}`,
			want: true,
		},
		{
			name: "nested $ref in array",
			json: `[{"schema":{"$ref":"#/components/schemas/ErrorModel"}}]`,
			want: true,
		},
		{
			name: "plain object without $ref",
			json: `{"temperature": 72, "city": "Seattle"}`,
			want: false,
		},
		{
			name: "plain array without $ref",
			json: `[1, 2, {"name": "test"}]`,
			want: false,
		},
		{
			name: "$ref with non-string value is not a schema ref",
			json: `{"$ref": 123}`,
			want: false,
		},
		{
			name: "primitive string containing $ref text is not a schema ref",
			json: `"this is just a string containing $ref"`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainsJSONRef(gjson.Parse(tt.json))
			if got != tt.want {
				t.Errorf("ContainsJSONRef() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetGeminiFunctionResponseResult(t *testing.T) {
	t.Run("preserves $ref as string under result", func(t *testing.T) {
		part := []byte(`{"functionResponse":{"name":"test"}}`)
		res := gjson.Parse(`{"schema":{"$ref":"#/components/schemas/ErrorModel"}}`)
		updated := SetGeminiFunctionResponseResult(part, "functionResponse.response.result", res)
		val := gjson.GetBytes(updated, "functionResponse.response.result")
		if val.Type != gjson.String {
			t.Fatalf("expected string type, got %s (raw: %s)", val.Type, val.Raw)
		}
	})

	t.Run("keeps non-$ref object as raw JSON under result", func(t *testing.T) {
		part := []byte(`{"functionResponse":{"name":"test"}}`)
		res := gjson.Parse(`{"ok":true,"code":200}`)
		updated := SetGeminiFunctionResponseResult(part, "functionResponse.response.result", res)
		val := gjson.GetBytes(updated, "functionResponse.response.result")
		if !val.IsObject() {
			t.Fatalf("expected object type, got %s (raw: %s)", val.Type, val.Raw)
		}
		if !val.Get("ok").Bool() {
			t.Fatalf("expected ok=true, got %v", val.Get("ok"))
		}
	})

	t.Run("places stringified $ref under .result when path ends with response", func(t *testing.T) {
		part := []byte(`{"functionResponse":{"name":"test"}}`)
		res := gjson.Parse(`{"schema":{"$ref":"#/components/schemas/ErrorModel"}}`)
		updated := SetGeminiFunctionResponseResult(part, "functionResponse.response", res)
		val := gjson.GetBytes(updated, "functionResponse.response.result")
		if val.Type != gjson.String {
			t.Fatalf("expected string type under response.result, got %s (raw: %s)", val.Type, val.Raw)
		}
	})
}

func TestNormalizeGeminiTurnRole(t *testing.T) {
	parts := func(raw string) gjson.Result {
		return gjson.Parse(raw).Get("parts")
	}
	cases := []struct {
		name         string
		rawRole      string
		parts        gjson.Result
		previousRole string
		want         string
	}{
		{name: "model is assistant", rawRole: "model", parts: parts(`{"parts":[]}`), want: "assistant"},
		{name: "function is user", rawRole: "function", parts: parts(`{"parts":[]}`), want: "user"},
		{name: "tool is user", rawRole: "tool", parts: parts(`{"parts":[]}`), want: "user"},
		{name: "omitted with functionCall is assistant", rawRole: "", parts: parts(`{"parts":[{"functionCall":{"name":"x"}}]}`), want: "assistant"},
		{name: "omitted with snake_case call is assistant", rawRole: "", parts: parts(`{"parts":[{"function_call":{"name":"x"}}]}`), want: "assistant"},
		{name: "omitted with functionResponse is user", rawRole: "", parts: parts(`{"parts":[{"functionResponse":{"name":"x"}}]}`), want: "user"},
		{name: "omitted alternates after user", rawRole: "", parts: parts(`{"parts":[{"text":"hi"}]}`), previousRole: "user", want: "assistant"},
		{name: "omitted starts as user", rawRole: "", parts: parts(`{"parts":[{"text":"hi"}]}`), want: "user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeGeminiTurnRole(tc.rawRole, tc.parts, tc.previousRole); got != tc.want {
				t.Fatalf("NormalizeGeminiTurnRole(%q) = %q, want %q", tc.rawRole, got, tc.want)
			}
		})
	}
}
