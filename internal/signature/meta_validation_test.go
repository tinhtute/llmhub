package signature

import (
	"encoding/base64"
	"strings"
	"testing"
)

// testMetaEnvelope builds a shape-valid synthetic Meta Muse envelope whose
// decoded length is exactly 45 + payloadLen bytes, so the power-of-two bucket
// rule sees a real bucket. No captured signature is embedded in tests.
func testMetaEnvelope(payloadLen int) string {
	decoded := make([]byte, metaReasoningSignatureOverheadLen+payloadLen)
	copy(decoded, metaReasoningSignatureHeader)
	return base64.RawURLEncoding.EncodeToString(decoded)
}

func testMetaRouterSuffix() string {
	return strings.Repeat("Ab3", 21)[:62]
}

func TestInspectMetaReasoningSignature_ShapeAndBuckets(t *testing.T) {
	for _, bucket := range []struct {
		payloadLen int
		exponent   int
	}{
		{1024, 0},
		{2048, 1},
		{4096, 2},
	} {
		envelope := testMetaEnvelope(bucket.payloadLen)
		info, err := InspectMetaReasoningSignature(envelope)
		if err != nil {
			t.Fatalf("InspectMetaReasoningSignature(%d-byte payload) error = %v", bucket.payloadLen, err)
		}
		if info.PayloadLen != bucket.payloadLen || info.BucketExponent != bucket.exponent {
			t.Fatalf("info = %+v, want PayloadLen=%d BucketExponent=%d", info, bucket.payloadLen, bucket.exponent)
		}
		if info.DecodedLen != metaReasoningSignatureOverheadLen+bucket.payloadLen {
			t.Fatalf("DecodedLen = %d, want %d", info.DecodedLen, metaReasoningSignatureOverheadLen+bucket.payloadLen)
		}
		if info.RouterSuffix != "" {
			t.Fatalf("native envelope reported RouterSuffix = %q, want empty", info.RouterSuffix)
		}
		if got := DetectSignatureProvider(envelope); got != SignatureProviderMeta {
			t.Fatalf("DetectSignatureProvider(native Meta envelope) = %q, want %q", got, SignatureProviderMeta)
		}
	}

	// A header that still yields the "Q-PaDg" literal prefix but does not match
	// the observed five-byte value must be rejected.
	wrongHeader := make([]byte, metaReasoningSignatureOverheadLen+1024)
	copy(wrongHeader, metaReasoningSignatureHeader)
	wrongHeader[len(metaReasoningSignatureHeader)-1] = 0x02
	wrongEnvelope := base64.RawURLEncoding.EncodeToString(wrongHeader)
	if !strings.HasPrefix(wrongEnvelope, "Q-PaDg") {
		t.Fatalf("fixture does not start with the literal prefix: %q", wrongEnvelope[:8])
	}

	rejects := map[string]string{
		"empty":             "",
		"wrong prefix":      "A" + testMetaEnvelope(1024)[1:],
		"unexpected header": wrongEnvelope,
		"payload below min": testMetaEnvelope(512),
		"non-power-of-two":  testMetaEnvelope(1000),
		"trailing padding":  testMetaEnvelope(1024) + "=",
		"non-base64url":     testMetaEnvelope(1024)[:20] + "!" + testMetaEnvelope(1024)[21:],
	}
	if info, err := InspectMetaReasoningSignature(" " + testMetaEnvelope(1024) + "\t"); err != nil || info.PayloadLen != 1024 {
		t.Fatalf("surrounding whitespace should be trimmed, got info=%+v err=%v", info, err)
	}

	for name, raw := range rejects {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectMetaReasoningSignature(raw); err == nil {
				t.Fatalf("InspectMetaReasoningSignature(%q) unexpectedly succeeded", raw)
			}
			if IsValidMetaReasoningSignature(raw) {
				t.Fatalf("IsValidMetaReasoningSignature(%q) = true, want false", raw)
			}
		})
	}
}

func TestInspectMetaReasoningSignature_RouterSuffixed(t *testing.T) {
	native := testMetaEnvelope(1024)
	suffix := testMetaRouterSuffix()
	routed := native + "." + suffix

	info, err := InspectMetaReasoningSignature(routed)
	if err != nil {
		t.Fatalf("InspectMetaReasoningSignature(routed) error = %v", err)
	}
	if info.RouterSuffix != suffix {
		t.Fatalf("RouterSuffix = %q, want %q", info.RouterSuffix, suffix)
	}
	if info.PayloadLen != 1024 {
		t.Fatalf("PayloadLen = %d, want 1024 (suffix must not enter the envelope math)", info.PayloadLen)
	}
	if got := DetectSignatureProvider(routed); got != SignatureProviderMeta {
		t.Fatalf("DetectSignatureProvider(routed) = %q, want %q", got, SignatureProviderMeta)
	}

	// A Meta-family router serves the routed value verbatim: the compatibility
	// table must preserve the suffix, not normalize it away.
	if got, ok := CompatibleSignatureForProvider(SignatureProviderMeta, routed); !ok || got != routed {
		t.Fatalf("CompatibleSignatureForProvider(Meta, routed) = (%q, %v), want verbatim routed value", got, ok)
	}
	if got, ok := CompatibleSignatureForProvider(SignatureProviderMeta, native); !ok || got != native {
		t.Fatalf("CompatibleSignatureForProvider(Meta, native) = (%q, %v), want verbatim native value", got, ok)
	}

	for _, bad := range []string{native + ".", native + ".bad-dash", native + ".a.b", native + "." + strings.Repeat("a", maxMetaRouterSuffixLen+1)} {
		if _, err := InspectMetaReasoningSignature(bad); err == nil {
			t.Fatalf("InspectMetaReasoningSignature(%q) unexpectedly succeeded", bad)
		}
	}
}
