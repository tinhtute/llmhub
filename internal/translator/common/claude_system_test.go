package common

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Ported from upstream b681a1e0f7b8: SystemReminderText wraps directives in the
// <system-reminder> envelope for demoted mid-session system/developer messages.
func TestSystemReminderText(t *testing.T) {
	text := "Please call a tool now."
	got := SystemReminderText(text)
	want := "<system-reminder>\nPlease call a tool now.\n</system-reminder>"
	if got != want {
		t.Fatalf("SystemReminderText(%q) = %q, want %q", text, got, want)
	}
}

// Ported from upstream 1d45a5f33290: the structured-output prompt fallback that
// Antigravity Claude 5.5 receives instead of a native output schema.
func TestBuildClaudeStructuredOutputInstruction(t *testing.T) {
	if got := BuildClaudeStructuredOutputInstruction(gjson.Result{}); got != "" {
		t.Fatalf("empty format = %q, want empty", got)
	}
	if got := BuildClaudeStructuredOutputInstruction(gjson.Parse(`{"type":"text"}`)); got != "" {
		t.Fatalf("text format = %q, want empty", got)
	}

	jsonObject := BuildClaudeStructuredOutputInstruction(gjson.Parse(`{"type":"json_object"}`))
	if !strings.Contains(jsonObject, "valid JSON object") {
		t.Fatalf("json_object instruction = %q, want a whole-response JSON object directive", jsonObject)
	}

	const schema = `{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"],"additionalProperties":false}`
	got := BuildClaudeStructuredOutputInstruction(gjson.Parse(`{"type":"json_schema","json_schema":{"name":"codex_output_schema","description":"two fields","schema":` + schema + `}}`))
	for _, want := range []string{"codex_output_schema", "two fields", schema} {
		if !strings.Contains(got, want) {
			t.Fatalf("instruction dropped %q: %s", want, got)
		}
	}

	// Responses text.format carries the schema without the json_schema wrapper.
	got = BuildClaudeStructuredOutputInstruction(gjson.Parse(`{"type":"json_schema","schema":` + schema + `}`))
	if !strings.Contains(got, schema) {
		t.Fatalf("unwrapped schema dropped: %s", got)
	}

	// A declared json_schema without a schema, and an unknown type, still ask
	// for a JSON object rather than emitting a broken instruction.
	if got := BuildClaudeStructuredOutputInstruction(gjson.Parse(`{"type":"json_schema","json_schema":{}}`)); !strings.Contains(got, "valid JSON object") {
		t.Fatalf("empty schema instruction = %q", got)
	}
}
