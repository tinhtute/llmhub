package signature

import (
	"encoding/base64"
	"strings"
)

type SignatureProvider string

const (
	SignatureProviderUnknown      SignatureProvider = "unknown"
	SignatureProviderClaude       SignatureProvider = "claude"
	SignatureProviderGemini       SignatureProvider = "gemini"
	SignatureProviderGeminiBypass SignatureProvider = "gemini_bypass"
	SignatureProviderGPT          SignatureProvider = "gpt"
	// SignatureProviderGrok is a target-only family. DetectSignatureProvider
	// never returns it: xAI emits no envelope, no version byte and no fixed
	// length, and its ciphertext is statistically indistinguishable from uniform
	// random bytes, so any positive claim would also capture every other opaque
	// blob. Grok handling is provenance-first - establish the target from the
	// model or route, then use InspectGrokEncryptedContent as a replay-safety
	// shape check.
	//
	// Ported from upstream CLIProxyAPI (be9fbd942929).
	SignatureProviderGrok SignatureProvider = "grok"
	// SignatureProviderMeta is identified by the observed Meta Muse envelope: a
	// fixed five-byte header plus a power-of-two padded payload length,
	// optionally followed by a router-appended (e.g. OpenRouter) dot suffix that
	// replays verbatim. See meta_validation.go for the capture basis and its
	// limits.
	//
	// Ported from upstream CLIProxyAPI (7a0b7563e9a2, cc5682c563dc).
	SignatureProviderMeta SignatureProvider = "meta"
)

// TargetEndpoint identifies the backend endpoint variant for provider families
// whose signature replay rules differ across hosting environments (notably
// Gemini on Vertex AI vs non-Vertex endpoints such as Antigravity and AI Studio).
type TargetEndpoint string

const (
	TargetEndpointDefault     TargetEndpoint = ""
	TargetEndpointVertex      TargetEndpoint = "vertex"
	TargetEndpointAntigravity TargetEndpoint = "antigravity"
)

// SignatureTarget specifies the target provider family and endpoint variant
// for signature compatibility decisions.
type SignatureTarget struct {
	Provider SignatureProvider
	Endpoint TargetEndpoint
}

// IsVertex reports whether the target endpoint is Vertex AI.
func (t SignatureTarget) IsVertex() bool {
	return strings.EqualFold(string(t.Endpoint), string(TargetEndpointVertex))
}

type SignatureBlockKind string

const (
	SignatureBlockKindUnknown            SignatureBlockKind = "unknown"
	SignatureBlockKindClaudeThinking     SignatureBlockKind = "claude_thinking"
	SignatureBlockKindGeminiModelPart    SignatureBlockKind = "gemini_model_part"
	SignatureBlockKindGeminiFunctionCall SignatureBlockKind = "gemini_function_call"
	SignatureBlockKindGPTReasoning       SignatureBlockKind = "gpt_reasoning"
)

type SignatureCompatibilityAction string

const (
	SignatureActionPreserve                SignatureCompatibilityAction = "preserve"
	SignatureActionDropBlock               SignatureCompatibilityAction = "drop_block"
	SignatureActionDropSignature           SignatureCompatibilityAction = "drop_signature"
	SignatureActionReplaceWithGeminiBypass SignatureCompatibilityAction = "replace_with_gemini_bypass"
	SignatureActionNoCompatibleReplacement SignatureCompatibilityAction = "no_compatible_replacement"
)

type SignatureCompatibilityDecision struct {
	TargetProvider       SignatureProvider
	TargetEndpoint       TargetEndpoint
	DetectedProvider     SignatureProvider
	BlockKind            SignatureBlockKind
	Compatible           bool
	Action               SignatureCompatibilityAction
	ReplacementSignature string
	NormalizedSignature  string
	Reason               string
}

const GeminiThoughtSignatureBypass = "skip_thought_signature_validator"

func SignatureProviderFromModelName(modelName string) SignatureProvider {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	if lower == "" {
		return SignatureProviderUnknown
	}
	// A namespaced ID's canonical model is its last path segment, so a
	// vendor/router prefix such as "openai-compatible/" must not outrank the
	// actual model family ("openai-compatible/muse-spark-1.3" is Meta, not
	// GPT). Only when the last segment names no family does the full string get
	// the legacy substring treatment. Ported from upstream CLIProxyAPI
	// (a4660260f894).
	candidate := lower
	if idx := strings.LastIndexAny(lower, "/:"); idx >= 0 && idx < len(lower)-1 {
		candidate = lower[idx+1:]
	}
	if provider := signatureProviderFromModelNameValue(candidate); provider != SignatureProviderUnknown {
		return provider
	}
	if candidate != lower {
		return signatureProviderFromModelNameValue(lower)
	}
	return SignatureProviderUnknown
}

func signatureProviderFromModelNameValue(model string) SignatureProvider {
	switch {
	case strings.Contains(model, "claude"):
		return SignatureProviderClaude
	case strings.Contains(model, "gemini") || strings.Contains(model, "learnlm"):
		return SignatureProviderGemini
	case strings.Contains(model, "gpt") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") || strings.HasPrefix(model, "o5"):
		return SignatureProviderGPT
	case strings.Contains(model, "grok"):
		return SignatureProviderGrok
	case isMetaMuseModelName(model):
		return SignatureProviderMeta
	default:
		return SignatureProviderUnknown
	}
}

// isMetaMuseModelName reports whether lower names a Meta Muse model on a native
// or third-party provider ("muse", "muse-*", "muse_*", "meta-muse-*") without
// matching unrelated substrings such as "museum" or "amuse".
func isMetaMuseModelName(lower string) bool {
	if lower == "muse" ||
		strings.HasPrefix(lower, "muse-") ||
		strings.HasPrefix(lower, "muse_") ||
		strings.HasSuffix(lower, "/muse") ||
		strings.Contains(lower, "/muse-") ||
		strings.Contains(lower, "/muse_") {
		return true
	}
	for _, prefix := range []string{"meta-", "meta_"} {
		if idx := strings.Index(lower, prefix+"muse"); idx >= 0 {
			rest := lower[idx+len(prefix+"muse"):]
			if rest == "" || rest[0] == '-' || rest[0] == '_' || rest[0] == '.' || rest[0] == ':' {
				return true
			}
		}
	}
	return false
}

func DetectSignatureProvider(rawSignature string) SignatureProvider {
	return DetectSignatureProviderForBlock(rawSignature, SignatureBlockKindUnknown)
}

func DetectSignatureProviderForBlock(rawSignature string, blockKind SignatureBlockKind) SignatureProvider {
	prefixProvider, payload, hasProviderPrefix := SplitSignatureProviderPrefix(rawSignature)
	if hasProviderPrefix && prefixProvider != SignatureProviderUnknown && strings.TrimSpace(payload) == "" {
		return SignatureProviderUnknown
	}
	if hasProviderPrefix && prefixProvider == SignatureProviderGPT {
		return SignatureProviderGPT
	}
	if payload == "" {
		payload = strings.TrimSpace(rawSignature)
	}
	payload = signaturePayloadAfterAnyPrefix(payload)

	if IsGeminiThoughtSignatureBypass(payload) {
		return SignatureProviderGeminiBypass
	}
	// GPT carries the literal "gAAAA" Fernet prefix, which pins both the version
	// byte and the high timestamp bytes — the strongest marker, probed first.
	if IsValidGPTReasoningSignature(payload) {
		return SignatureProviderGPT
	}
	// Meta Muse pins a fixed five-byte decoded header plus an exact
	// power-of-two padded payload length, so it cannot collide with any other
	// family. Probe it before the Gemini/Claude wire-shape checks that would
	// otherwise accept part of a Muse payload.
	if IsValidMetaReasoningSignature(payload) {
		return SignatureProviderMeta
	}
	// Gemini wire-shape matching runs before the Claude probes: Gemini's
	// exact single-record envelope requirement keeps the families separable,
	// while a loose Claude decodability probe can otherwise claim a native
	// Gemini 3.x field-2 signature.
	if isGeminiNativeSignature(payload) {
		return SignatureProviderGemini
	}
	if IsValidClaudeCAISSignature(payload) || HasDecodableClaudeThinkingSignature(payload) {
		return SignatureProviderClaude
	}
	if hasProviderPrefix && prefixProvider != SignatureProviderUnknown {
		return prefixProvider
	}
	if blockKind == SignatureBlockKindGPTReasoning && payload != "" {
		return SignatureProviderGPT
	}
	return SignatureProviderUnknown
}

func DecideSignatureCompatibilityForTarget(target SignatureTarget, rawSignature string, blockKind SignatureBlockKind) SignatureCompatibilityDecision {
	detected := DetectSignatureProviderForBlock(rawSignature, blockKind)
	decision := SignatureCompatibilityDecision{
		TargetProvider:   target.Provider,
		TargetEndpoint:   target.Endpoint,
		DetectedProvider: detected,
		BlockKind:        blockKind,
		Action:           SignatureActionNoCompatibleReplacement,
		Reason:           "signature is not compatible with target provider",
	}

	if strings.TrimSpace(rawSignature) == "" {
		decision.Reason = "empty signature"
		return decision
	}

	switch target.Provider {
	case SignatureProviderClaude:
		if detected != SignatureProviderClaude {
			decision.Action = SignatureActionDropBlock
			return decision
		}
		if blockKind == SignatureBlockKindClaudeThinking || blockKind == SignatureBlockKindUnknown {
			if normalized, ok := CompatibleAntigravityClaudeThinkingSignature(rawSignature); ok {
				decision.Compatible = true
				decision.Action = SignatureActionPreserve
				decision.NormalizedSignature = normalized
				decision.Reason = "Claude thinking signature is compatible"
				return decision
			}
			if IsValidClaudeCAISSignature(rawSignature) {
				decision.Compatible = true
				decision.Action = SignatureActionPreserve
				decision.NormalizedSignature = signaturePayloadAfterAnyPrefix(rawSignature)
				decision.Reason = "Claude CAIS signature is compatible with Claude upstream"
				return decision
			}
		}
		decision.Action = SignatureActionDropBlock
	case SignatureProviderGemini:
		// Vertex AI bare-Tink signatures are only valid on Vertex AI targets.
		// Non-Vertex endpoints (Antigravity, AI Studio, Gemini API) reject them
		// with 400 "Corrupted thought signature", so they degrade to the bypass sentinel.
		if IsGeminiVertexBareTinkSignature(rawSignature) && !target.IsVertex() {
			decision.Compatible = true
			decision.Action = SignatureActionReplaceWithGeminiBypass
			decision.ReplacementSignature = GeminiThoughtSignatureBypass
			decision.NormalizedSignature = GeminiThoughtSignatureBypass
			decision.Reason = "Vertex bare-Tink signature is not accepted by this Gemini endpoint"
			return decision
		}
		if detected == SignatureProviderGemini || detected == SignatureProviderGeminiBypass {
			decision.Compatible = true
			decision.Action = SignatureActionPreserve
			decision.NormalizedSignature = SignaturePayloadWithoutProviderPrefix(rawSignature)
			decision.Reason = "Gemini signature is compatible"
			return decision
		}
		if blockKind == SignatureBlockKindGeminiModelPart || blockKind == SignatureBlockKindGeminiFunctionCall {
			decision.Compatible = true
			decision.Action = SignatureActionReplaceWithGeminiBypass
			decision.ReplacementSignature = GeminiThoughtSignatureBypass
			decision.NormalizedSignature = GeminiThoughtSignatureBypass
			decision.Reason = "Gemini accepts the bypass sentinel for incompatible replay signatures"
			return decision
		}
		decision.Action = SignatureActionDropBlock
	case SignatureProviderGPT:
		if detected == SignatureProviderGPT {
			decision.Compatible = true
			decision.Action = SignatureActionPreserve
			decision.NormalizedSignature = SignaturePayloadWithoutProviderPrefix(rawSignature)
			decision.Reason = "GPT signature is compatible"
			return decision
		}
		decision.Action = SignatureActionDropBlock
	case SignatureProviderMeta:
		// Meta has no cross-provider bypass sentinel: only a positively
		// recognized Muse envelope may replay to a Meta target, and it replays
		// verbatim (including a router suffix a Meta-family router served).
		if detected == SignatureProviderMeta {
			if payload := SignaturePayloadWithoutProviderPrefix(rawSignature); IsValidMetaReasoningSignature(payload) {
				decision.Compatible = true
				decision.Action = SignatureActionPreserve
				decision.NormalizedSignature = payload
				decision.Reason = "Meta Muse signature is compatible"
				return decision
			}
		}
		decision.Action = SignatureActionDropBlock
		decision.Reason = "Meta Muse requires its native encrypted_content envelope and has no cross-provider bypass sentinel"
	case SignatureProviderGrok:
		// xAI decrypts encrypted_content and rejects the request with 400
		// "Could not decrypt" when the blob is foreign or mutated, so a
		// non-matching value has to leave with the block. Detection never
		// yields Grok, so the target alone can never authorize a preserve here:
		// callers that own provenance use InspectGrokEncryptedContent.
		decision.Action = SignatureActionDropBlock
		decision.Reason = "xAI verifies encrypted_content on replay and rejects foreign or mutated blobs"
	default:
		decision.Action = SignatureActionNoCompatibleReplacement
	}
	return decision
}

func DecideSignatureCompatibility(targetProvider SignatureProvider, rawSignature string, blockKind SignatureBlockKind) SignatureCompatibilityDecision {
	return DecideSignatureCompatibilityForTarget(SignatureTarget{Provider: targetProvider, Endpoint: TargetEndpointVertex}, rawSignature, blockKind)
}

func DecideSignatureCompatibilityForModel(targetProvider SignatureProvider, targetModel string, rawSignature string, blockKind SignatureBlockKind) SignatureCompatibilityDecision {
	if targetProvider == SignatureProviderUnknown {
		targetProvider = SignatureProviderFromModelName(targetModel)
	}
	return DecideSignatureCompatibility(targetProvider, rawSignature, blockKind)
}

func DecideSignatureCompatibilityForTargetModel(target SignatureTarget, targetModel string, rawSignature string, blockKind SignatureBlockKind) SignatureCompatibilityDecision {
	if target.Provider == SignatureProviderUnknown {
		target.Provider = SignatureProviderFromModelName(targetModel)
	}
	return DecideSignatureCompatibilityForTarget(target, rawSignature, blockKind)
}

func CompatibleSignatureForTarget(target SignatureTarget, rawSignature string) (string, bool) {
	decision := DecideSignatureCompatibilityForTarget(target, rawSignature, SignatureBlockKindUnknown)
	if !decision.Compatible {
		return "", false
	}
	if decision.ReplacementSignature != "" {
		return decision.ReplacementSignature, true
	}
	return decision.NormalizedSignature, true
}

func CompatibleSignatureForTargetBlock(target SignatureTarget, rawSignature string, blockKind SignatureBlockKind) (string, bool) {
	decision := DecideSignatureCompatibilityForTarget(target, rawSignature, blockKind)
	if !decision.Compatible {
		return "", false
	}
	if decision.ReplacementSignature != "" {
		return decision.ReplacementSignature, true
	}
	return decision.NormalizedSignature, true
}

func CompatibleSignatureForProvider(targetProvider SignatureProvider, rawSignature string) (string, bool) {
	return CompatibleSignatureForTarget(SignatureTarget{Provider: targetProvider, Endpoint: TargetEndpointVertex}, rawSignature)
}

func CompatibleSignatureForProviderBlock(targetProvider SignatureProvider, rawSignature string, blockKind SignatureBlockKind) (string, bool) {
	return CompatibleSignatureForTargetBlock(SignatureTarget{Provider: targetProvider, Endpoint: TargetEndpointVertex}, rawSignature, blockKind)
}

func CompatibleAntigravityClaudeThinkingSignature(rawSignature string) (string, bool) {
	normalized, err := NormalizeClaudeThinkingSignature(rawSignature, ClaudeSignatureValidationOptions{Strict: true})
	if err != nil {
		return "", false
	}
	return normalized, true
}

func SplitSignatureProviderPrefix(rawSignature string) (SignatureProvider, string, bool) {
	sig := strings.TrimSpace(rawSignature)
	idx := strings.IndexByte(sig, '#')
	if idx < 0 {
		return SignatureProviderUnknown, sig, false
	}
	prefix := strings.ToLower(strings.TrimSpace(sig[:idx]))
	payload := strings.TrimSpace(sig[idx+1:])
	provider := SignatureProviderFromCachePrefix(prefix)
	return provider, payload, provider != SignatureProviderUnknown
}

func SignatureProviderFromCachePrefix(prefix string) SignatureProvider {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "claude", "anthropic", "antigravity-claude":
		return SignatureProviderClaude
	case "gemini", "google", "antigravity-gemini":
		return SignatureProviderGemini
	case "gemini_bypass", "gemini-bypass":
		return SignatureProviderGeminiBypass
	case "gpt", "openai":
		return SignatureProviderGPT
	default:
		return SignatureProviderUnknown
	}
}

func SignaturePayloadWithoutProviderPrefix(rawSignature string) string {
	return signaturePayloadAfterAnyPrefix(rawSignature)
}

func signaturePayloadAfterAnyPrefix(rawSignature string) string {
	sig := strings.TrimSpace(rawSignature)
	if idx := strings.IndexByte(sig, '#'); idx >= 0 {
		return strings.TrimSpace(sig[idx+1:])
	}
	return sig
}

func isGeminiNativeSignature(rawSignature string) bool {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(decoded) == 0 {
		return false
	}
	if decoded[0] == 0x0a {
		return true
	}
	// Vertex AI carries the Tink primitive output unwrapped; it is the same
	// inner payload the field-2 envelope wraps. Recognition is pinned to the
	// observed key ids (see isGeminiBareTinkEnvelope), so an arbitrary 0x01
	// blob from another family stays unclaimed.
	if isGeminiBareTinkEnvelope(decoded) {
		return true
	}
	return isGeminiField2WrappedSignature(decoded)
}
