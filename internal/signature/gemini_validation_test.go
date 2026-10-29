package signature

import (
	"strings"
	"testing"
)

func TestValidateGeminiFunctionCallPairingValidParallelGroup(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"id": "call-1", "name": "weather", "args": {"city": "Paris"}}},
					{"functionCall": {"id": "call-2", "name": "weather", "args": {"city": "London"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"id": "call-1", "name": "weather", "response": {"temp": "15C"}}},
					{"functionResponse": {"id": "call-2", "name": "weather", "response": {"temp": "12C"}}}
				]
			}
		]
	}`)

	if err := ValidateGeminiFunctionCallPairing(input); err != nil {
		t.Fatalf("valid pairing failed: %v", err)
	}
}

// Ported from upstream CLIProxyAPI commit 0fe19ede90a4 ("preserve Gemini prompt
// cache by demoting mid-session developer messages"): intervening user turns
// before pending functionResponse parts are now accepted; model turns are not.
func TestValidateGeminiFunctionCallPairingAllowsUserBoundaryBeforeResponse(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"run","args":{}}}]},{"role":"user","parts":[{"text":"boundary"}]},{"role":"user","parts":[{"functionResponse":{"id":"call-1","name":"run","response":{"result":"ok"}}}]}]}}`)

	if err := ValidateGeminiFunctionCallPairing(payload); err != nil {
		t.Fatalf("user boundary before function response should be accepted: %v", err)
	}
}

func TestValidateGeminiFunctionCallPairingRejectsModelBoundaryBeforeResponse(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"run","args":{}}}]},{"role":"model","parts":[{"text":"boundary"}]},{"role":"user","parts":[{"functionResponse":{"id":"call-1","name":"run","response":{"result":"ok"}}}]}]}}`)

	err := ValidateGeminiFunctionCallPairing(payload)
	if err == nil {
		t.Fatal("model boundary before function response should be rejected")
	}
	if !strings.Contains(err.Error(), "model content appears before") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGeminiFunctionCallPairingRejectsIDMismatch(t *testing.T) {
	input := []byte(`{
		"contents": [
			{"role": "model", "parts": [{"functionCall": {"id": "call-1", "name": "weather", "args": {}}}]},
			{"role": "user", "parts": [{"functionResponse": {"id": "call-other", "name": "weather", "response": {}}}]}
		]
	}`)

	err := ValidateGeminiFunctionCallPairing(input)
	if err == nil {
		t.Fatal("id mismatch should fail")
	}
	if !strings.Contains(err.Error(), "does not match functionCall.id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// testGeminiBareTinkSignature builds a synthetic Vertex-style bare-Tink
// signature: one TINK prefix byte, a four-byte big-endian key id, then filler
// ciphertext up to total length n. The shape mirrors the 2026-10-26 Vertex
// captures without embedding any real provider payload.
func testGeminiBareTinkSignature(n int) string {
	if n < geminiBareTinkMinLen {
		n = geminiBareTinkMinLen
	}
	payload := []byte{0x01, 0x8f, 0x3d, 0x6b, 0x5f}
	for len(payload) < n {
		payload = append(payload, byte(0xA0+len(payload)%0x20))
	}
	return testGeminiThoughtSignature(payload)
}

func TestGeminiVertexBareTinkSignature_IsRecognizedAndReplayed(t *testing.T) {
	sig := testGeminiBareTinkSignature(606)
	if !IsGeminiVertexBareTinkSignature(sig) {
		t.Fatal("Vertex key id must be recognized")
	}

	if provider := DetectSignatureProviderForBlock(sig, SignatureBlockKindGeminiModelPart); provider != SignatureProviderGemini {
		t.Fatalf("DetectSignatureProviderForBlock(model part) = %q, want %q", provider, SignatureProviderGemini)
	}
	if provider := DetectSignatureProviderForBlock(sig, SignatureBlockKindGeminiFunctionCall); provider != SignatureProviderGemini {
		t.Fatalf("DetectSignatureProviderForBlock(functionCall) = %q, want %q", provider, SignatureProviderGemini)
	}

	decision := DecideSignatureCompatibility(SignatureProviderGemini, sig, SignatureBlockKindGeminiFunctionCall)
	if decision.Action != SignatureActionPreserve {
		t.Fatalf("action = %q (%s), want %q", decision.Action, decision.Reason, SignatureActionPreserve)
	}
	if got := GeminiReplaySignatureOrBypass(sig, SignatureBlockKindGeminiFunctionCall); got != sig {
		t.Fatalf("replay signature was rewritten: %q", got[:16])
	}
}

func TestGeminiVertexBareTinkSignature_RejectsShortPayload(t *testing.T) {
	// Below prefix + key id + auth-tag floor the payload is not a bounded
	// bare-Tink envelope and must degrade to the bypass decision instead of
	// being replayed verbatim.
	sig := testGeminiThoughtSignature(append([]byte{0x01, 0x8f, 0x3d}, make([]byte, 10)...))

	if IsGeminiVertexBareTinkSignature(sig) {
		t.Fatal("short payload should not be a bare-Tink envelope")
	}
	decision := DecideSignatureCompatibility(SignatureProviderGemini, sig, SignatureBlockKindGeminiFunctionCall)
	if decision.Action != SignatureActionReplaceWithGeminiBypass {
		t.Fatalf("action = %q, want %q", decision.Action, SignatureActionReplaceWithGeminiBypass)
	}
	if decision.ReplacementSignature != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("replacement = %q, want %q", decision.ReplacementSignature, GeminiSkipThoughtSignatureValidator)
	}
}

func TestGeminiVertexBareTinkSignature_RequiresVertexKeyID(t *testing.T) {
	// The prefix byte alone matches ~1/256 of uniform random payloads, so an
	// unenveloped 0x01 blob with an unknown key id must not be claimed.
	payload := []byte{0x01, 0x69, 0x14, 0x7d, 0x13}
	for len(payload) < 405 {
		payload = append(payload, byte(0xA0+len(payload)%0x20))
	}
	sig := testGeminiThoughtSignature(payload)

	if IsGeminiVertexBareTinkSignature(sig) {
		t.Fatal("0x01 payload with a non-Vertex key id must not be a bare-Tink envelope")
	}
	if got := DetectSignatureProvider(sig); got == SignatureProviderGemini {
		t.Fatalf("DetectSignatureProvider = %q, must not claim Gemini", got)
	}
	if !IsGeminiVertexBareTinkSignature("gemini#" + testGeminiBareTinkSignature(405)) {
		t.Fatal("provider-prefixed Vertex signature must be recognized")
	}
}

func TestGeminiVertexBareTinkSignature_NeverClaimsOtherFamilies(t *testing.T) {
	sig := testGeminiBareTinkSignature(405)
	// Block-kind scoping: on Claude targets the blob must not be read as a
	// Claude envelope; it stays a Gemini-shaped payload and Claude's own
	// decision path drops it.
	if got := DetectSignatureProviderForBlock(sig, SignatureBlockKindClaudeThinking); got != SignatureProviderGemini {
		t.Fatalf("Claude-thinking block detection = %q, want %q (bare-Tink stays Gemini-shaped)", got, SignatureProviderGemini)
	}
	if IsValidClaudeCAISSignature(sig) {
		t.Fatal("bare-Tink payload must not satisfy the Claude CAIS validator")
	}
	if decision := DecideSignatureCompatibility(SignatureProviderClaude, sig, SignatureBlockKindClaudeThinking); decision.Compatible {
		t.Fatalf("Claude target accepted a bare-Tink signature: %+v", decision)
	}
}

func TestGeminiVertexBareTinkSignature_FloorBoundary(t *testing.T) {
	// floor = 1 prefix + 4 key id + 16 auth tag = 21. A 20-byte blob carrying
	// the right key id is still not a bounded Tink output.
	short := make([]byte, geminiBareTinkMinLen-1)
	copy(short, []byte{0x01, 0x8f, 0x3d, 0x6b, 0x5f})
	if IsGeminiVertexBareTinkSignature(testGeminiThoughtSignature(short)) {
		t.Fatalf("%d-byte payload must not be a bare-Tink envelope", len(short))
	}

	exact := make([]byte, geminiBareTinkMinLen)
	copy(exact, []byte{0x01, 0x8f, 0x3d, 0x6b, 0x5f})
	sig := testGeminiThoughtSignature(exact)
	if !IsGeminiVertexBareTinkSignature(sig) {
		t.Fatalf("%d-byte payload at the floor must be a bare-Tink envelope", len(exact))
	}
	if got := DetectSignatureProvider(sig); got != SignatureProviderGemini {
		t.Fatalf("DetectSignatureProvider = %q, want %q", got, SignatureProviderGemini)
	}
}
