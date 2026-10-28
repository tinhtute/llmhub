package helps

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

// TestNormalizeClaudeToolChoiceClaude55Boundaries pins the routed-model family
// boundary of the Claude 5.5 forced-tool downgrade (upstream 1d45a5f33290): the
// five 5.5 route shapes become auto, and every other shape keeps its payload
// byte for byte.
func TestNormalizeClaudeToolChoiceClaude55Boundaries(t *testing.T) {
	const forced = `{"type":"tool","name":"lookup_weather","disable_parallel_tool_use":false}`
	for _, tc := range []struct {
		name   string
		model  string
		choice string
		want   string
	}{
		{name: "opus date", model: "claude-opus-5-5-20260901", choice: forced, want: `{"type":"auto","disable_parallel_tool_use":false}`},
		{name: "sonnet bracket", model: "claude-sonnet-5-5[1m]", choice: forced, want: `{"type":"auto","disable_parallel_tool_use":false}`},
		{name: "high date bracket thinking suffix", model: "claude-opus-5-5-high-20260901[1m](high)", choice: forced, want: `{"type":"auto","disable_parallel_tool_use":false}`},
		{name: "reasoning variant", model: "claude-sonnet-5-5-reasoning", choice: forced, want: `{"type":"auto","disable_parallel_tool_use":false}`},
		{name: "any choice keeps parallel flag", model: "claude-opus-5-5", choice: `{"type":"any","disable_parallel_tool_use":true}`, want: `{"type":"auto","disable_parallel_tool_use":true}`},
		{name: "old opus", model: "claude-opus-5", choice: forced, want: forced},
		{name: "old sonnet", model: "claude-sonnet-4-6", choice: forced, want: forced},
		{name: "haiku", model: "claude-haiku-5-5", choice: forced, want: forced},
		{name: "near match opus", model: "claude-opus-5-50", choice: forced, want: forced},
		{name: "near match sonnet", model: "claude-sonnet-5-50", choice: forced, want: forced},
		{name: "unknown family variant", model: "claude-opus-5-5-custom", choice: forced, want: forced},
		{name: "auto stays", model: "claude-opus-5-5", choice: `{"type":"auto"}`, want: `{"type":"auto"}`},
		{name: "none stays", model: "claude-sonnet-5-5", choice: `{"type":"none"}`, want: `{"type":"none"}`},
		{name: "omitted stays", model: "claude-opus-5-5", choice: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"` + tc.model + `","tools":[{"name":"lookup_weather"}]`
			if tc.choice != "" {
				body += `,"tool_choice":` + tc.choice
			}
			payload := []byte(body + `}`)

			out := NormalizeClaudeToolChoice(tc.model, payload)

			got := gjson.GetBytes(out, "tool_choice")
			want := gjson.Parse(tc.want)
			if !reflect.DeepEqual(got.Value(), want.Value()) {
				t.Fatalf("tool_choice = %s, want %s; body=%s", got.Raw, want.Raw, out)
			}
			if tc.choice == tc.want && !bytes.Equal(out, payload) {
				t.Fatalf("non-5.5 payload changed: got %s, want %s", out, payload)
			}
			if gjson.GetBytes(out, "tools.0.name").String() != "lookup_weather" {
				t.Fatalf("tools rewritten: %s", out)
			}
		})
	}
}
