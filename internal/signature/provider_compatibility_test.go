package signature

import (
	"strings"
	"testing"
)

func TestSignatureProviderFromModelName_MetaAndNamespacedTail(t *testing.T) {
	// The first block is the pre-existing classification: the namespaced-tail
	// rule must not reclassify a live route.
	tests := map[string]SignatureProvider{
		"":                          SignatureProviderUnknown,
		"gpt-5.4":                   SignatureProviderGPT,
		"claude-sonnet-4-6":         SignatureProviderClaude,
		"gemini-3-pro":              SignatureProviderGemini,
		"gemini-cli/gemini-2.5-pro": SignatureProviderGemini,
		"learnlm-2":                 SignatureProviderGemini,
		"o3-mini":                   SignatureProviderGPT,
		"o5-mini":                   SignatureProviderGPT,

		// Meta Muse, native and behind a router.
		"muse":                             SignatureProviderMeta,
		"muse-spark-1.3":                   SignatureProviderMeta,
		"meta/muse-wonder":                 SignatureProviderMeta,
		"openrouter/meta/muse-wonder":      SignatureProviderMeta,
		"openai-compatible/muse-spark-1.3": SignatureProviderMeta,
		"meta-muse-wonder":                 SignatureProviderMeta,
		"museum-tour":                      SignatureProviderUnknown,
		"amuse-bouche":                     SignatureProviderUnknown,

		// Grok is a target-only family; the tail segment decides it.
		"grok-4.5":               SignatureProviderGrok,
		"openrouter/x-ai/grok-4": SignatureProviderGrok,
		"muse-proxy/gpt-5.4":     SignatureProviderGPT,

		// The last segment wins over a vendor prefix that names another family.
		"openrouter/anthropic/claude-sonnet-4-6": SignatureProviderClaude,
		"google/gemini-3-flash":                  SignatureProviderGemini,
	}
	for model, want := range tests {
		if got := SignatureProviderFromModelName(model); got != want {
			t.Errorf("SignatureProviderFromModelName(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestDecideSignatureCompatibility_MetaTargetGating(t *testing.T) {
	native := testMetaEnvelope(1024)
	routed := native + "." + testMetaRouterSuffix()
	gpt := testGPTReasoningSignature()

	t.Run("native envelope replays to Meta", func(t *testing.T) {
		decision := DecideSignatureCompatibility(SignatureProviderMeta, native, SignatureBlockKindGPTReasoning)
		if !decision.Compatible || decision.Action != SignatureActionPreserve {
			t.Fatalf("decision = %+v, want preserve", decision)
		}
		if decision.NormalizedSignature != native {
			t.Fatalf("NormalizedSignature = %q, want the native envelope", decision.NormalizedSignature)
		}
	})

	t.Run("router-suffixed envelope replays verbatim to a Meta target", func(t *testing.T) {
		decision := DecideSignatureCompatibilityForModel(SignatureProviderUnknown, "openrouter/meta/muse-wonder", routed, SignatureBlockKindGPTReasoning)
		if !decision.Compatible || decision.Action != SignatureActionPreserve {
			t.Fatalf("decision = %+v, want preserve", decision)
		}
		if decision.NormalizedSignature != routed {
			t.Fatalf("NormalizedSignature = %q, want the routed value with its suffix", decision.NormalizedSignature)
		}
		if decision.TargetProvider != SignatureProviderMeta {
			t.Fatalf("TargetProvider = %q, want %q", decision.TargetProvider, SignatureProviderMeta)
		}
	})

	t.Run("foreign signatures are dropped with the Meta-specific reason", func(t *testing.T) {
		for name, raw := range map[string]string{
			"gpt fernet envelope": gpt,
			"unknown opaque blob": "opaque-reasoning-token-xyz",
		} {
			decision := DecideSignatureCompatibility(SignatureProviderMeta, raw, SignatureBlockKindGPTReasoning)
			if decision.Compatible || decision.Action != SignatureActionDropBlock {
				t.Fatalf("%s: decision = %+v, want drop_block", name, decision)
			}
			if !strings.Contains(decision.Reason, "Meta Muse requires its native encrypted_content envelope") {
				t.Fatalf("%s: reason = %q, want the Meta-specific explanation", name, decision.Reason)
			}
			if decision.Action == SignatureActionNoCompatibleReplacement {
				t.Fatalf("%s: Meta target fell through to no_compatible_replacement", name)
			}
		}
	})

	t.Run("meta envelope is dropped for a non-Meta target", func(t *testing.T) {
		for name, raw := range map[string]string{"native": native, "routed": routed} {
			decision := DecideSignatureCompatibility(SignatureProviderGPT, raw, SignatureBlockKindGPTReasoning)
			if decision.Compatible || decision.Action != SignatureActionDropBlock {
				t.Fatalf("%s: decision = %+v, want drop_block", name, decision)
			}
		}
	})

	t.Run("routed gpt envelope is preserved for a gpt target", func(t *testing.T) {
		routedGPT := gpt + "." + strings.Repeat("Ab3", 20)[:59]
		decision := DecideSignatureCompatibility(SignatureProviderGPT, routedGPT, SignatureBlockKindGPTReasoning)
		if !decision.Compatible || decision.Action != SignatureActionPreserve {
			t.Fatalf("decision = %+v, want preserve", decision)
		}
		if decision.NormalizedSignature != routedGPT {
			t.Fatalf("NormalizedSignature = %q, want the routed value (suffix stripping belongs to the executor)", decision.NormalizedSignature)
		}
	})

	// No producer emits a "meta#" cache prefix, so it must not become trusted
	// provenance by accident.
	if got := SignatureProviderFromCachePrefix("meta"); got != SignatureProviderUnknown {
		t.Fatalf("SignatureProviderFromCachePrefix(meta) = %q, want unknown", got)
	}
}

func TestDecideSignatureCompatibilityForTarget_VertexBareTinkGating(t *testing.T) {
	sig := testGeminiBareTinkSignature(405)
	if !IsGeminiVertexBareTinkSignature(sig) {
		t.Fatal("expected test signature to be recognized as Vertex bare-Tink")
	}

	wrappedSig := testGemini3ThoughtSignature([]byte{0x01, 0x8f, 0x3d, 0x6b, 0x5f, 0xAA, 0xBB, 0xCC, 0xDD})
	if IsGeminiVertexBareTinkSignature(wrappedSig) {
		t.Fatal("wrapped envelope must not be recognized as bare-Tink")
	}

	t.Run("Vertex target preserves bare-Tink", func(t *testing.T) {
		target := SignatureTarget{Provider: SignatureProviderGemini, Endpoint: TargetEndpointVertex}
		decision := DecideSignatureCompatibilityForTarget(target, sig, SignatureBlockKindGeminiFunctionCall)
		if !decision.Compatible || decision.Action != SignatureActionPreserve {
			t.Fatalf("decision = %+v, want preserve", decision)
		}
		if decision.NormalizedSignature != sig {
			t.Fatalf("NormalizedSignature = %q, want verbatim %q", decision.NormalizedSignature, sig)
		}

		got, ok := CompatibleSignatureForTarget(target, sig)
		if !ok || got != sig {
			t.Fatalf("CompatibleSignatureForTarget = (%q, %v), want (%q, true)", got, ok, sig)
		}
	})

	t.Run("Antigravity target replaces bare-Tink with bypass sentinel", func(t *testing.T) {
		target := SignatureTarget{Provider: SignatureProviderGemini, Endpoint: TargetEndpointAntigravity}
		decision := DecideSignatureCompatibilityForTarget(target, sig, SignatureBlockKindGeminiFunctionCall)
		if !decision.Compatible || decision.Action != SignatureActionReplaceWithGeminiBypass {
			t.Fatalf("decision = %+v, want replace_with_gemini_bypass", decision)
		}
		if decision.ReplacementSignature != GeminiThoughtSignatureBypass {
			t.Fatalf("ReplacementSignature = %q, want %q", decision.ReplacementSignature, GeminiThoughtSignatureBypass)
		}

		got, ok := CompatibleSignatureForTarget(target, sig)
		if !ok || got != GeminiThoughtSignatureBypass {
			t.Fatalf("CompatibleSignatureForTarget = (%q, %v), want (%q, true)", got, ok, GeminiThoughtSignatureBypass)
		}
	})

	t.Run("Antigravity target preserves wrapped Gemini signature", func(t *testing.T) {
		target := SignatureTarget{Provider: SignatureProviderGemini, Endpoint: TargetEndpointAntigravity}
		decision := DecideSignatureCompatibilityForTarget(target, wrappedSig, SignatureBlockKindGeminiFunctionCall)
		if !decision.Compatible || decision.Action != SignatureActionPreserve {
			t.Fatalf("decision = %+v, want preserve", decision)
		}
		if decision.NormalizedSignature != wrappedSig {
			t.Fatalf("NormalizedSignature = %q, want verbatim wrapped signature", decision.NormalizedSignature)
		}

		got, ok := CompatibleSignatureForTarget(target, wrappedSig)
		if !ok || got != wrappedSig {
			t.Fatalf("CompatibleSignatureForTarget = (%q, %v), want (%q, true)", got, ok, wrappedSig)
		}
	})

	t.Run("Default provider compatibility preserves bare-Tink", func(t *testing.T) {
		decision := DecideSignatureCompatibility(SignatureProviderGemini, sig, SignatureBlockKindGeminiFunctionCall)
		if !decision.Compatible || decision.Action != SignatureActionPreserve {
			t.Fatalf("decision = %+v, want preserve", decision)
		}
		got, ok := CompatibleSignatureForProvider(SignatureProviderGemini, sig)
		if !ok || got != sig {
			t.Fatalf("CompatibleSignatureForProvider = (%q, %v), want (%q, true)", got, ok, sig)
		}
	})
}
