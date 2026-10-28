package signature

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodexCommentarySignatureMarker is the internal marker carried in the
// signature field of the Claude thinking block that relays a Codex
// `phase: commentary` message. It is not a provider signature and must never be
// forwarded to any upstream: every Claude-history consumer either restores the
// block as assistant commentary text (this package) or rewrites it as an own
// commentary message item (the Claude-to-Codex request translator).
//
// The literal is forgeable by design — a client may send it in its own thinking
// block — so the restore is unconditional instead of being derived from provenance.
const CodexCommentarySignatureMarker = "cpa:commentary:v1"

// IsCodexCommentarySignatureMarker reports whether rawSignature is exactly the
// internal commentary marker, ignoring surrounding whitespace.
func IsCodexCommentarySignatureMarker(rawSignature string) bool {
	return strings.TrimSpace(rawSignature) == CodexCommentarySignatureMarker
}

// RestoreCodexCommentaryMarkers rewrites every thinking block that carries the
// internal commentary marker into a plain text block and reports how many blocks
// were restored. It is deliberately independent of provider signature
// compatibility policy: the marker must never reach any upstream, including the
// Claude-compatible endpoints whose model name maps to a non-Claude family.
//
// A marked block with empty thinking text is dropped because it carries no
// history, and an assistant message left with no content at all is dropped with
// it. When no marker is present the payload is returned unchanged.
func RestoreCodexCommentaryMarkers(payload []byte) ([]byte, int) {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload, 0
	}

	messageResults := messages.Array()
	keptMessages := make([]string, 0, len(messageResults))
	restored := 0
	for _, message := range messageResults {
		content := message.Get("content")
		if !content.IsArray() {
			keptMessages = append(keptMessages, message.Raw)
			continue
		}

		contentResults := content.Array()
		keptParts := make([]string, 0, len(contentResults))
		restoredInMessage := 0
		for _, part := range contentResults {
			if part.Get("type").String() != "thinking" || !IsCodexCommentarySignatureMarker(part.Get("signature").String()) {
				keptParts = append(keptParts, part.Raw)
				continue
			}
			restoredInMessage++
			if text := part.Get("thinking").String(); text != "" {
				keptParts = append(keptParts, commentaryTextBlockRaw(text))
			}
		}
		if restoredInMessage == 0 {
			keptMessages = append(keptMessages, message.Raw)
			continue
		}
		restored += restoredInMessage
		if len(keptParts) == 0 {
			continue
		}
		updated, err := sjson.SetRaw(message.Raw, "content", "["+strings.Join(keptParts, ",")+"]")
		if err != nil {
			keptMessages = append(keptMessages, message.Raw)
			continue
		}
		keptMessages = append(keptMessages, updated)
	}
	if restored == 0 {
		return payload, 0
	}

	output, err := sjson.SetRawBytes(payload, "messages", []byte("["+strings.Join(keptMessages, ",")+"]"))
	if err != nil {
		return payload, 0
	}
	return output, restored
}

// commentaryTextBlockRaw renders the plain assistant text block a marked
// commentary block is restored to.
func commentaryTextBlockRaw(text string) string {
	block := []byte(`{"type":"text","text":""}`)
	block, _ = sjson.SetBytes(block, "text", text)
	return string(block)
}
