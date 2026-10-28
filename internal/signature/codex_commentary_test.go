package signature

import (
	"testing"

	"github.com/tidwall/gjson"
)

// The marker is a plain, forgeable literal: a client can type it into its own
// thinking block. These tests therefore assert the restore from the payload
// alone, with no provenance signal anywhere in the input.
const clientSuppliedCommentaryPayload = `{"messages":[
	{"role":"user","content":"First turn"},
	{"role":"assistant","content":[
		{"type":"thinking","thinking":"COMMENTARY_SENTINEL","signature":"cpa:commentary:v1"},
		{"type":"text","text":"FINAL_SENTINEL"}
	]},
	{"role":"user","content":"Second turn"}
]}`

func TestRestoreCodexCommentaryMarkers_RestoresTextAndLeavesOtherBlocksUntouched(t *testing.T) {
	out, restored := RestoreCodexCommentaryMarkers([]byte(clientSuppliedCommentaryPayload))
	if restored != 1 {
		t.Fatalf("restored = %d, want 1: %s", restored, out)
	}
	if gjson.GetBytes(out, "messages.1.content.#").Int() != 2 {
		t.Fatalf("assistant content block count changed: %s", out)
	}
	first := gjson.GetBytes(out, "messages.1.content.0")
	if first.Get("type").String() != "text" || first.Get("text").String() != "COMMENTARY_SENTINEL" {
		t.Fatalf("marker block should become a plain text block: %s", first.Raw)
	}
	if first.Get("signature").Exists() {
		t.Fatalf("restored text block must not carry a signature: %s", first.Raw)
	}
	if got := gjson.GetBytes(out, "messages.1.content.1.text").String(); got != "FINAL_SENTINEL" {
		t.Fatalf("final text = %q, want FINAL_SENTINEL: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "First turn" {
		t.Fatalf("string content must pass through untouched, got %q", got)
	}
}

func TestRestoreCodexCommentaryMarkers_LeavesPayloadWithoutMarkerUnchanged(t *testing.T) {
	input := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"opaque-provider-id"}]}]}`)
	out, restored := RestoreCodexCommentaryMarkers(input)
	if restored != 0 {
		t.Fatalf("restored = %d, want 0", restored)
	}
	if string(out) != string(input) {
		t.Fatalf("payload without the marker must pass through byte-identical:\nin:  %s\nout: %s", input, out)
	}
}

func TestRestoreCodexCommentaryMarkers_DropsMarkedBlockWithNoText(t *testing.T) {
	// An empty marked block carries no history, and a message left with nothing
	// but that block carries none either.
	input := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"cpa:commentary:v1"}]},{"role":"user","content":"next"}]}`)
	out, restored := RestoreCodexCommentaryMarkers(input)
	if restored != 1 {
		t.Fatalf("restored = %d, want 1: %s", restored, out)
	}
	if gjson.GetBytes(out, "messages.#").Int() != 1 {
		t.Fatalf("empty commentary message should be dropped: %s", out)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "next" {
		t.Fatalf("remaining message = %q, want next", got)
	}
}

func TestIsCodexCommentarySignatureMarker_TrimsSurroundingWhitespace(t *testing.T) {
	for _, value := range []string{"cpa:commentary:v1", " cpa:commentary:v1\n", "\tcpa:commentary:v1 "} {
		if !IsCodexCommentarySignatureMarker(value) {
			t.Fatalf("%q should be recognized as the commentary marker", value)
		}
	}
	for _, value := range []string{"", "cpa:commentary:v2", "opaque-provider-id", "pcpa:commentary:v1"} {
		if IsCodexCommentarySignatureMarker(value) {
			t.Fatalf("%q must not be recognized as the commentary marker", value)
		}
	}
}
