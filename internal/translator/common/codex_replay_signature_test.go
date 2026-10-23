package common

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testCodexReplayMetaSignature() string {
	decoded := make([]byte, 45+1024)
	copy(decoded, []byte{0x43, 0xe3, 0xda, 0x0e, 0x01})
	return base64.RawURLEncoding.EncodeToString(decoded)
}

func testCodexReplayGPTSignature() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func testCodexReplayGrokSignature() string {
	payload := make([]byte, 135)
	for i := range payload {
		payload[i] = byte((i*73 + 19) & 0xff)
	}
	return base64.RawStdEncoding.EncodeToString(payload)
}

func TestCodexReplaySignatureForTarget(t *testing.T) {
	metaSig := testCodexReplayMetaSignature()
	routedSig := metaSig + "." + strings.Repeat("Ab3", 21)[:62]
	gptSig := testCodexReplayGPTSignature()
	gptRouted := gptSig + "." + strings.Repeat("Ab3", 27)[:79]
	grokSig := testCodexReplayGrokSignature()

	tests := []struct {
		name      string
		model     string
		raw       string
		want      string
		wantFound bool
	}{
		{"native meta on muse target", "muse-spark-1.3", metaSig, metaSig, true},
		{"routed meta on muse target", "meta/muse-wonder", routedSig, routedSig, true},
		{"gpt on gpt target", "gpt-5.4", gptSig, gptSig, true},
		{"routed gpt on openrouter gpt target", "openrouter/openai/gpt-5.6-luna-pro", gptRouted, gptRouted, true},
		{"gpt cache prefix normalized", "gpt-5.4", "gpt#" + gptSig, gptSig, true},
		{"meta sig on gpt target rejected", "gpt-5.4", metaSig, "", false},
		{"routed meta on gpt target rejected", "gpt-5.4", routedSig, "", false},
		{"grok blob on grok target", "grok-4.5", grokSig, grokSig, true},
		{"grok blob on gpt target rejected", "gpt-5.4", grokSig, "", false},
		{"unknown blob on unmapped target rejected", "custom-model", "opaque-reasoning-token", "", false},
		{"empty signature rejected", "muse-spark-1.3", "   ", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CodexReplaySignatureForTarget(tc.model, tc.raw)
			if ok != tc.wantFound {
				t.Fatalf("CodexReplaySignatureForTarget(%q, %q) ok = %v, want %v", tc.model, tc.raw, ok, tc.wantFound)
			}
			if got != tc.want {
				t.Fatalf("CodexReplaySignatureForTarget(%q, %q) = %q, want %q", tc.model, tc.raw, got, tc.want)
			}
		})
	}
}
