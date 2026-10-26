package common

import (
	"strings"

	sigcompat "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/therealtinhtute/llmhub/internal/thinking"
)

// CodexReplaySignatureForTarget returns the exact value that may be written as a
// Responses reasoning encrypted_content for the Codex-format upstream serving
// modelName, or false when rawSignature must not replay there.
//
// Positively detected families must match the target family exactly (an unmapped
// target defaults to the Codex GPT family, mirroring the Codex translators'
// purpose) and go through the shared compatibility normalization, so
// cache-prefixed values such as "gpt#..." are stripped to their native payload
// instead of being forwarded verbatim.
//
// A Grok target is resolved before detection, not after: xAI emits no
// envelope, and the fork's detection keeps a loose Claude decodability probe, so
// a uniform-random xAI blob is routinely *claimed* as Claude. Provenance for a
// Grok target can therefore only come from the target itself; the shape
// inspector then decides replay safety, as grok_validation.go documents.
//
// Ported from upstream CLIProxyAPI internal/translator/common/codex_replay_signature.go
// (772df04b30d5), adapted to the families this fork resolves (no Kimi/SWE
// providers) and to the local detection order.
func CodexReplaySignatureForTarget(modelName, rawSignature string) (string, bool) {
	rawSignature = strings.TrimSpace(rawSignature)
	if rawSignature == "" {
		return "", false
	}
	target := sigcompat.SignatureProviderFromModelName(thinking.ParseSuffix(modelName).ModelName)
	if target == sigcompat.SignatureProviderGrok {
		if _, err := sigcompat.InspectGrokEncryptedContent(rawSignature); err == nil {
			return rawSignature, true
		}
		return "", false
	}
	detected := sigcompat.DetectSignatureProvider(rawSignature)
	if detected != sigcompat.SignatureProviderUnknown {
		effectiveTarget := target
		if effectiveTarget == sigcompat.SignatureProviderUnknown {
			effectiveTarget = sigcompat.SignatureProviderGPT
		}
		if detected != effectiveTarget {
			return "", false
		}
		normalized, ok := sigcompat.CompatibleSignatureForProvider(effectiveTarget, rawSignature)
		return normalized, ok && normalized != ""
	}
	return "", false
}
