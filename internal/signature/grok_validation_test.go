package signature

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// validGrokSample synthesizes a high-entropy unpadded standard-base64 blob the
// same shape as native xAI encrypted_content (uniform random bytes, decoded
// length well above the floor).
func validGrokSample(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 256)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.RawStdEncoding.EncodeToString(buf)
}

func TestInspectGrokEncryptedContent_ValidatesTransportShape(t *testing.T) {
	valid := validGrokSample(t)

	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"valid high-entropy blob", valid, false},
		{"empty", "", true},
		{"leading whitespace", " " + valid, true},
		{"padded base64", valid + "=", true},
		{"non-base64 character", valid[:20] + "!" + valid[21:], true},
		{"provider prefix", "gpt#" + valid, true},
		{"gpt fernet envelope", validGPTSample(t), true},
		{"decoded too short", base64.RawStdEncoding.EncodeToString([]byte("tiny")), true},
		{"low entropy", base64.RawStdEncoding.EncodeToString(make([]byte, 256)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := InspectGrokEncryptedContent(tt.raw)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// validGPTSample builds a Fernet-shaped GPT reasoning signature (0x80 version
// byte, AES-block-multiple ciphertext) the way upstream tests do.
func validGPTSample(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 1+8+16+16+32)
	raw[0] = 0x80
	raw[8] = 1
	for i := 9; i < len(raw); i++ {
		raw[i] = byte(i)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func TestDetectSignatureProvider_GPTFernet(t *testing.T) {
	sig := validGPTSample(t)
	if got := DetectSignatureProviderForBlock(sig, SignatureBlockKindUnknown); got != SignatureProviderGPT {
		t.Fatalf("fernet-shaped signature detected as %q, want %q", got, SignatureProviderGPT)
	}
	// gpt#-prefixed variant also detects as GPT and the payload round-trips.
	if _, ok := CompatibleSignatureForProvider(SignatureProviderGPT, "gpt#"+sig); !ok {
		t.Fatal("gpt#-prefixed signature should be GPT-compatible")
	}
}

func TestIsValidGPTReasoningSignature(t *testing.T) {
	if !IsValidGPTReasoningSignature(validGPTSample(t)) {
		t.Fatal("valid fernet-shaped signature rejected")
	}
	for _, bad := range []string{"", "  ", "not-base64!!!", "gAAAA-invalid%%%", strings.Repeat("A", 40)} {
		if IsValidGPTReasoningSignature(bad) {
			t.Fatalf("invalid signature %q accepted", bad)
		}
	}
}

func TestInspectGrokEncryptedContent_RouterSuffixed(t *testing.T) {
	rawBytes := make([]byte, 135)
	for i := range rawBytes {
		rawBytes[i] = byte((i*73 + 19) & 0xff)
	}
	native := base64.RawStdEncoding.EncodeToString(rawBytes)
	suffix := strings.Repeat("Ab3", 21)[:62]
	routed := native + "." + suffix

	info, err := InspectGrokEncryptedContent(routed)
	if err != nil {
		t.Fatalf("InspectGrokEncryptedContent(routed) error = %v", err)
	}
	if info.RouterSuffix != suffix || info.DecodedLen != len(rawBytes) {
		t.Fatalf("info = %+v, want DecodedLen=%d RouterSuffix=%q", info, len(rawBytes), suffix)
	}
	for _, bad := range []string{native + ".", native + ".bad-dash", native + ".a.b"} {
		if _, err := InspectGrokEncryptedContent(bad); err == nil {
			t.Fatalf("InspectGrokEncryptedContent(%q) unexpectedly succeeded", bad)
		}
	}
}
