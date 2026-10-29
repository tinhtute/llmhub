package signature

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

// MaxGeminiThoughtSignatureLen bounds an opaque Gemini thought signature blob.
// Ported from upstream CLIProxyAPI (internal/signature/gemini_validation.go).
const MaxGeminiThoughtSignatureLen = 32 * 1024 * 1024

type geminiFunctionCallRef struct {
	id   string
	name string
	path string
}

type geminiFunctionResponseRef struct {
	part gjson.Result
	path string
}

// ValidateGeminiFunctionCallPairing validates the replay shape around Gemini
// functionCall and functionResponse parts. It checks id/name pairing and
// prevents response parts from being interleaved inside the same content as
// function calls. It allows a final pending functionCall group because callers
// may validate a freshly returned model step before tool outputs exist.
func ValidateGeminiFunctionCallPairing(inputRawJSON []byte) error {
	contents, contentsPath := geminiContents(inputRawJSON)
	if !contents.IsArray() {
		return nil
	}

	var pending []geminiFunctionCallRef
	var validationErr error
	contents.ForEach(func(contentIndex, content gjson.Result) bool {
		i := int(contentIndex.Int())
		parts := content.Get("parts")
		// Empty/non-array parts are treated like a bare content boundary below.
		// Ported from upstream CLIProxyAPI commit 0fe19ede90a4 ("preserve Gemini
		// prompt cache by demoting mid-session developer messages").
		if !parts.IsArray() || parts.Raw == "[]" || !parts.Get("0").Exists() {
			if len(pending) > 0 {
				validationErr = fmt.Errorf(
					"%s[%d]: content appears before %d pending functionResponse part(s)",
					contentsPath,
					i,
					len(pending),
				)
			}
			return validationErr == nil
		}

		var calls []geminiFunctionCallRef
		var responses []geminiFunctionResponseRef
		parts.ForEach(func(partIndex, part gjson.Result) bool {
			j := int(partIndex.Int())
			partPath := fmt.Sprintf("%s[%d].parts[%d]", contentsPath, i, j)
			if call := part.Get("functionCall"); call.Exists() {
				if call.Get("name").String() == "" {
					validationErr = fmt.Errorf("%s: missing functionCall.name", partPath)
					return false
				}
				calls = append(calls, geminiFunctionCallRef{
					id:   call.Get("id").String(),
					name: call.Get("name").String(),
					path: partPath,
				})
			}
			if response := part.Get("functionResponse"); response.Exists() {
				responses = append(responses, geminiFunctionResponseRef{
					part: part,
					path: partPath,
				})
			}
			return true
		})
		if validationErr != nil {
			return false
		}

		switch {
		case len(calls) > 0 && len(responses) > 0:
			validationErr = fmt.Errorf(
				"%s[%d]: functionCall and functionResponse parts must not be interleaved in the same content",
				contentsPath,
				i,
			)
		case len(calls) > 0 && len(pending) > 0:
			validationErr = fmt.Errorf(
				"%s[%d]: functionCall appears before %d pending functionResponse part(s)",
				contentsPath,
				i,
				len(pending),
			)
		case len(calls) > 0:
			pending = calls
			return true
		case len(responses) == 0 && len(pending) > 0:
			// Allow intervening user content (such as system reminders, mid-session
			// developer notices, or user turns) to appear before the pending
			// functionResponse turn. Upstream Antigravity accepts this natively.
			// Reject only a model turn without responses, which breaks turn ownership.
			// Ported from upstream CLIProxyAPI commit 0fe19ede90a4 ("preserve Gemini
			// prompt cache by demoting mid-session developer messages").
			role := strings.ToLower(strings.TrimSpace(content.Get("role").String()))
			if role == "model" {
				validationErr = fmt.Errorf(
					"%s[%d]: model content appears before %d pending functionResponse part(s)",
					contentsPath,
					i,
					len(pending),
				)
			}
			return validationErr == nil
		case len(responses) == 0:
			return true
		case len(pending) == 0:
			validationErr = fmt.Errorf("%s[%d]: functionResponse without preceding functionCall", contentsPath, i)
		case len(responses) != len(pending):
			validationErr = fmt.Errorf(
				"%s[%d]: functionResponse count %d does not match pending functionCall count %d",
				contentsPath,
				i,
				len(responses),
				len(pending),
			)
		}
		if validationErr != nil {
			return false
		}

		for responseIndex, responseRef := range responses {
			partPath := responseRef.path
			response := responseRef.part.Get("functionResponse")
			call := pending[responseIndex]
			responseID := response.Get("id").String()
			responseName := response.Get("name").String()

			switch {
			case call.id != "" && responseID == "":
				validationErr = fmt.Errorf("%s: missing functionResponse.id for %s", partPath, call.path)
			case call.id != "" && responseID != call.id:
				validationErr = fmt.Errorf(
					"%s: functionResponse.id %q does not match functionCall.id %q at %s",
					partPath,
					responseID,
					call.id,
					call.path,
				)
			case responseName == "":
				validationErr = fmt.Errorf("%s: missing functionResponse.name", partPath)
			case call.name != "" && responseName != call.name:
				validationErr = fmt.Errorf(
					"%s: functionResponse.name %q does not match functionCall.name %q at %s",
					partPath,
					responseName,
					call.name,
					call.path,
				)
			}
			if validationErr != nil {
				return false
			}
		}

		pending = nil
		return true
	})
	return validationErr
}

func geminiContents(inputRawJSON []byte) (gjson.Result, string) {
	if contents := gjson.GetBytes(inputRawJSON, "contents"); contents.Exists() {
		return contents, "contents"
	}
	return gjson.GetBytes(inputRawJSON, "request.contents"), "request.contents"
}

func IsGeminiThoughtSignatureBypass(rawSignature string) bool {
	switch strings.TrimSpace(rawSignature) {
	case "skip_thought_signature_validator", "context_engineering_is_the_way_to_go":
		return true
	default:
		return false
	}
}

// isGeminiField2WrappedSignature reports whether decoded bytes match Gemini's
// field-2 outer protobuf envelope wrapping a single field-1 opaque payload
// (Tink-style primitive output or an ASCII UUID), as carried by Gemini 3.x models.
func isGeminiField2WrappedSignature(decoded []byte) bool {
	value, ok := consumeGeminiField2Field1Value(decoded)
	if !ok {
		return false
	}
	return isLikelyGeminiOpaquePayload(value) || isASCIIUUIDBytes(value)
}

// consumeGeminiField2Field1Value parses the outer field-2 length-delimited
// record and returns its inner field-1 payload when the envelope is exact.
func consumeGeminiField2Field1Value(decoded []byte) ([]byte, bool) {
	num, typ, n := protowire.ConsumeTag(decoded)
	if n < 0 || num != 2 || typ != protowire.BytesType {
		return nil, false
	}
	offset := n
	container, n := protowire.ConsumeBytes(decoded[offset:])
	if n < 0 {
		return nil, false
	}
	offset += n
	if offset != len(decoded) {
		return nil, false
	}

	num, typ, n = protowire.ConsumeTag(container)
	if n < 0 || num != 1 || typ != protowire.BytesType {
		return nil, false
	}
	containerOffset := n
	value, n := protowire.ConsumeBytes(container[containerOffset:])
	if n < 0 {
		return nil, false
	}
	containerOffset += n
	if containerOffset != len(container) {
		return nil, false
	}
	return value, true
}

// isLikelyGeminiOpaquePayload checks only the Tink prefix-type byte (0x01);
// the four-byte key id that follows is rotated key material, so pinning it
// would reject every signature whenever Google rotates keys.
func isLikelyGeminiOpaquePayload(value []byte) bool {
	return len(value) > 0 && value[0] == 0x01
}

// geminiBareTinkMinLen is the structural floor for a bare Tink payload: one
// prefix-type byte, a four-byte key id, and at least a 16-byte authentication
// tag.
const geminiBareTinkMinLen = 1 + 4 + 16

// geminiVertexBareTinkKeyIDs are the Tink key ids observed on the Vertex AI
// serve path. Vertex carries the Tink primitive output directly in the
// thoughtSignature field, with no protobuf wrapper (observed on gemini-4-argon
// and gemini-3.8-flash via the Vertex channel, 2026-10-26). Without the
// wrapper the ciphertext is indistinguishable from uniform random bytes, so
// the prefix byte alone would claim about 1/256 of every unenveloped payload;
// matching the key id as well raises that to 40 fixed bits. A rotated key is
// simply not recognized and falls back to the bypass sentinel, which is the
// behavior before bare-Tink recognition existed; add the new id when it is
// observed.
var geminiVertexBareTinkKeyIDs = [][4]byte{
	{0x8f, 0x3d, 0x6b, 0x5f},
}

// isGeminiBareTinkEnvelope reports whether decoded is the unwrapped Tink
// primitive output that the Vertex AI serve path emits as the whole thought
// signature: prefix-type byte 0x01, four-byte big-endian key id, ciphertext.
func isGeminiBareTinkEnvelope(decoded []byte) bool {
	if len(decoded) < geminiBareTinkMinLen || decoded[0] != 0x01 {
		return false
	}
	keyID := [4]byte(decoded[1:5])
	for _, known := range geminiVertexBareTinkKeyIDs {
		if keyID == known {
			return true
		}
	}
	return false
}

// IsGeminiVertexBareTinkSignature reports whether rawSignature is a Vertex AI
// bare-Tink thought signature, with or without the cache provider prefix.
// Callers sending to Gemini endpoints other than Vertex AI must treat such
// signatures as incompatible: Antigravity (cloudcode-pa) and AI Studio
// (generativelanguage) reject them with 400 "Corrupted thought signature",
// while Vertex AI accepts them natively.
func IsGeminiVertexBareTinkSignature(rawSignature string) bool {
	payload := strings.TrimSpace(SignaturePayloadWithoutProviderPrefix(rawSignature))
	if payload == "" {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	return isGeminiBareTinkEnvelope(decoded)
}

// isASCIIUUIDBytes reports whether decoded bytes form a canonical 36-char
// ASCII UUID (8-4-4-4-12 with hyphens at fixed offsets).
func isASCIIUUIDBytes(decoded []byte) bool {
	if len(decoded) != 36 {
		return false
	}
	for i, b := range decoded {
		switch i {
		case 8, 13, 18, 23:
			if b != '-' {
				return false
			}
		default:
			if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')) {
				return false
			}
		}
	}
	return true
}
