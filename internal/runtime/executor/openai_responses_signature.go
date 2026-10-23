package executor

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/therealtinhtute/llmhub/internal/util"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file ports upstream CLIProxyAPI's OpenAI-Responses reasoning
// encrypted_content sanitizer (internal/runtime/executor/
// openai_responses_signature.go, upstream checkpoint v7.3.9 including commit
// 81d6ba774621), extended with the signature-carriage work from v8.0.34:
// Meta Muse target gating (7a0b7563e9a2, 674dbe8fd1be, cc5682c563dc),
// router-suffix stripping (a68c7d8ed7c2) and multi-family Responses targets
// (be9fbd942929). These helpers are shared by the codex, meta and websocket
// codex executors.
//
// Every family probe routes through internal/signature, which is the single
// owner of envelope validation.

func openaiResponsesReasoningSummaryIsEmpty(summary gjson.Result) bool {
	if !summary.Exists() || summary.Type == gjson.Null {
		return true
	}
	return summary.IsArray() && len(summary.Array()) == 0
}

func promoteOpenAIResponsesReasoningTextToSummary(itemRaw string, content gjson.Result) (string, error) {
	var b strings.Builder
	b.WriteByte('[')
	n := 0
	for _, part := range content.Array() {
		if strings.TrimSpace(part.Get("type").String()) != "reasoning_text" {
			continue
		}
		text := part.Get("text").String()
		if text == "" {
			continue
		}
		partJSON, err := sjson.Set(`{"type":"summary_text"}`, "text", text)
		if err != nil {
			return itemRaw, err
		}
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(partJSON)
		n++
	}
	b.WriteByte(']')
	if n == 0 {
		return itemRaw, nil
	}
	return sjson.SetRaw(itemRaw, "summary", b.String())
}

type metaSignaturePolicy int

const (
	// metaSignatureForeign: the target is not Meta; a recognized Meta envelope is foreign.
	metaSignatureForeign metaSignaturePolicy = iota
	// metaSignatureNativeOnly: native Meta upstream (api.meta.ai); only unsuffixed envelopes replay.
	metaSignatureNativeOnly
	// metaSignatureAllowRouted: a Meta-family model served by a router or by another third-party
	// Responses provider; native and router-suffixed envelopes replay verbatim.
	metaSignatureAllowRouted
)

func sanitizeOpenAIResponsesReasoningEncryptedContent(ctx context.Context, provider string, body []byte) []byte {
	return sanitizeOpenAIResponsesReasoningEncryptedContentWithOptions(ctx, provider, body, false, false, metaSignatureForeign, signature.SignatureProviderUnknown, false)
}

// sanitizeOpenAIResponsesReasoningEncryptedContentWithCompat ports upstream
// commit 81d6ba774621: when isCompat is true, third-party Responses models
// (such as DeepSeek) require the original reasoning_text inside
// reasoning.content and the reasoning item ids to be replayed, so the wipe and
// orphan-id stripping are skipped. It carries no target model, so Meta envelopes
// stay foreign and router suffixes are stripped (fail-closed for an unknown
// upstream); call sites that know their target use WithCompatModel*.
func sanitizeOpenAIResponsesReasoningEncryptedContentWithCompat(ctx context.Context, provider string, body []byte, isCompat bool) []byte {
	return sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModel(ctx, provider, body, isCompat, "")
}

// resolveResponsesSignatureTargetModel returns the canonical model name to use
// for reasoning-signature target checks. baseModel already carries the model the
// route will actually serve; when it names no family (an alias or a bare vendor
// ID) the client-requested model is the only remaining evidence.
//
// Ported from upstream CLIProxyAPI (674dbe8fd1be).
func resolveResponsesSignatureTargetModel(baseModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if signature.SignatureProviderFromModelName(baseModel) != signature.SignatureProviderUnknown {
		return baseModel
	}
	requested := thinking.ParseSuffix(helps.PayloadRequestedModel(opts, req.Model)).ModelName
	if signature.SignatureProviderFromModelName(requested) != signature.SignatureProviderUnknown {
		return requested
	}
	return baseModel
}

// sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModel is the compat
// variant that knows its target model: a positively recognized Meta Muse envelope
// is preserved whenever the upstream actually serves a Meta-family model, whether
// or not the route is flagged compat (a router key can serve meta/muse-* through
// the Codex executor without isCompat). For any other target the recognized Meta
// envelope is known-foreign and stripped like other providers'.
func sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModel(ctx context.Context, provider string, body []byte, isCompat bool, targetModel string) []byte {
	return sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(ctx, provider, body, isCompat, targetModel, "")
}

// sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL
// additionally knows the credential's base URL, which decides two things the
// model name cannot: whether a Meta target is the native upstream (native-only
// suffix policy) and whether router suffixes may be replayed (only OpenRouter
// accepts the values it minted).
func sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(ctx context.Context, provider string, body []byte, isCompat bool, targetModel string, baseURL string) []byte {
	targetProvider := signature.SignatureProviderFromModelName(targetModel)
	metaPolicy := metaSignatureForeign
	if targetProvider == signature.SignatureProviderMeta {
		if strings.TrimSpace(baseURL) != "" && isNativeMetaUpstreamURL(baseURL) {
			metaPolicy = metaSignatureNativeOnly
		} else {
			metaPolicy = metaSignatureAllowRouted
		}
	}
	return sanitizeOpenAIResponsesReasoningEncryptedContentWithOptions(ctx, provider, body, isCompat, isCompat, metaPolicy, targetProvider, isOpenRouterUpstreamURL(baseURL))
}

// sanitizeOpenAIResponsesReasoningEncryptedContentForMeta is the Meta-target
// variant and is deliberately native-only: only positively recognized Meta Muse
// envelopes without a router suffix may replay to api.meta.ai, whose suffix
// acceptance is unverified. GPT, other known-provider, unknown-format and
// router-suffixed values are all stripped, so a wrong signature can never reach
// the Meta upstream through this path. A future Meta envelope revision will be
// stripped until the inspector learns it (fail-closed).
func sanitizeOpenAIResponsesReasoningEncryptedContentForMeta(ctx context.Context, provider string, body []byte) []byte {
	return sanitizeOpenAIResponsesReasoningEncryptedContentWithOptions(ctx, provider, body, false, true, metaSignatureNativeOnly, signature.SignatureProviderMeta, false)
}

// sanitizeOpenAIResponsesReasoningEncryptedContentForMetaBaseURL gates Meta
// reasoning signatures by whether baseURL is the native Meta upstream (which
// accepts only unsuffixed envelopes) or a third-party Responses provider
// configured under the Meta credential (which replayed the suffix verbatim only
// when it is OpenRouter, and otherwise expects the OpenRouter suffix stripped).
func sanitizeOpenAIResponsesReasoningEncryptedContentForMetaBaseURL(ctx context.Context, provider string, body []byte, baseURL string) []byte {
	if isNativeMetaUpstreamURL(baseURL) {
		return sanitizeOpenAIResponsesReasoningEncryptedContentForMeta(ctx, provider, body)
	}
	return sanitizeOpenAIResponsesReasoningEncryptedContentWithOptions(ctx, provider, body, false, true, metaSignatureAllowRouted, signature.SignatureProviderMeta, isOpenRouterUpstreamURL(baseURL))
}

func isNativeMetaUpstreamURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "" || host == "api.meta.ai" || strings.HasSuffix(host, ".meta.ai")
}

// isOpenRouterUpstreamURL reports whether the request will be served by
// OpenRouter, the only upstream observed to mint and expect the private router
// suffix on reasoning signatures.
func isOpenRouterUpstreamURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

// stripRouterSuffix drops the dot-delimited router suffix reported by an
// envelope inspector, leaving the native value an official upstream expects.
func stripRouterSuffix(rawSignature, suffix string) string {
	if suffix == "" || len(rawSignature) <= len(suffix)+1 {
		return rawSignature
	}
	return rawSignature[:len(rawSignature)-len(suffix)-1]
}

func sanitizeOpenAIResponsesReasoningEncryptedContentWithOptions(ctx context.Context, provider string, body []byte, isCompat bool, keepForeign bool, metaPolicy metaSignaturePolicy, targetProvider signature.SignatureProvider, allowRouterSuffix bool) []byte {
	inputResult := util.GetGJSONBytesNoCopy(body, "input")
	if !inputResult.Exists() || !inputResult.IsArray() {
		return body
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai responses upstream"
	}

	// Codex backend rejects store=true and does not persist items when store=false.
	// A reasoning item that still carries an id without usable encrypted_content is
	// treated as a store lookup and returns:
	//   Item with id '...' not found. Items are not persisted when `store` is set to false.
	// Strip those orphan ids unless the request explicitly opts into store=true.
	stripOrphanReasoningIDs := !gjson.GetBytes(body, "store").Bool()

	items := inputResult.Array()

	// rebuilt accumulates the edited "input" array as JSON array bytes. It
	// stays nil while no item needs editing so the common case (nothing to
	// sanitize) does no allocation or rebuilding. Edits are applied directly
	// to each item's own raw JSON rather than re-parsing the whole body,
	// keeping the cost proportional to the item being edited.
	var rebuilt []byte
	itemsWritten := 0
	keep := func(raw string) {
		if rebuilt == nil {
			return
		}
		if itemsWritten > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, raw...)
		itemsWritten++
	}
	startRebuild := func(index int) {
		if rebuilt != nil {
			return
		}
		// First item that needs editing: start the buffer and backfill
		// it with the raw JSON of every preceding item.
		rebuilt = make([]byte, 0, len(inputResult.Raw))
		rebuilt = append(rebuilt, '[')
		for i := range index {
			keep(items[i].Raw)
		}
	}

	for index, item := range items {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			keep(item.Raw)
			continue
		}

		encryptedContent := item.Get("encrypted_content")
		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			itemID = fmt.Sprintf("input[%d]", index)
		}

		nextItem := item.Raw
		changed := false

		// Official Codex schema sets maxItems: 0 on reasoning.content. Third-party
		// channels replay cleartext thinking there; promote it into summary when
		// summary is empty, then force content to [].
		// When isCompat is true, third-party Responses models (such as DeepSeek)
		// require original reasoning_text inside reasoning.content to be replayed.
		content := item.Get("content")
		if !isCompat && content.IsArray() && len(content.Array()) > 0 {
			if openaiResponsesReasoningSummaryIsEmpty(item.Get("summary")) {
				promoted, errPromote := promoteOpenAIResponsesReasoningTextToSummary(nextItem, content)
				if errPromote != nil {
					helps.LogWithRequestID(ctx).Debugf("%s: failed to promote reasoning_text into summary at input[%d]: %v", provider, index, errPromote)
				} else {
					nextItem = promoted
				}
			}
			cleared, errClear := sjson.SetRaw(nextItem, "content", "[]")
			if errClear != nil {
				helps.LogWithRequestID(ctx).Debugf("%s: failed to clear reasoning content at input[%d]: %v", provider, index, errClear)
			} else {
				nextItem = cleared
				changed = true
				helps.LogWithRequestID(ctx).Debugf("%s: cleared reasoning content at input[%d] item_id=%q", provider, index, itemID)
			}
		}

		if !encryptedContent.Exists() {
			if !isCompat && stripOrphanReasoningIDs && item.Get("id").Exists() {
				dropped, err := sjson.Delete(nextItem, "id")
				if err != nil {
					helps.LogWithRequestID(ctx).Debugf("%s: failed to drop orphan reasoning id at input[%d]: %v", provider, index, err)
				} else {
					nextItem = dropped
					changed = true
					helps.LogWithRequestID(ctx).Debugf("%s: dropped orphan reasoning id at input[%d] item_id=%q reason=missing encrypted_content with store disabled", provider, index, itemID)
				}
			}
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}

		reason := ""
		rewrittenSignature := ""
		switch encryptedContent.Type {
		case gjson.String:
			rawSignature := encryptedContent.String()
			if rawSignature != strings.TrimSpace(rawSignature) {
				reason = "encrypted_content has leading or trailing whitespace"
			} else if metaPolicy != metaSignatureForeign {
				// Meta targets never reach the GPT-first gate: a GPT,
				// foreign-provider or unknown-format blob must not replay to
				// Meta. Only a positively recognized Meta envelope survives, and
				// the native Meta upstream additionally refuses the router suffix
				// a router such as OpenRouter appends.
				if info, err := signature.InspectMetaReasoningSignature(rawSignature); err != nil {
					reason = err.Error()
				} else if info.RouterSuffix != "" {
					if metaPolicy == metaSignatureNativeOnly {
						reason = "router-suffixed Meta reasoning signature is not accepted by the native Meta upstream"
					} else if !allowRouterSuffix {
						rewrittenSignature = stripRouterSuffix(rawSignature, info.RouterSuffix)
					}
				}
			} else if targetProvider == signature.SignatureProviderUnknown || targetProvider == signature.SignatureProviderGPT {
				candidate := rawSignature
				hasGPTPrefix := false
				if prefixedProvider, unprefixed, ok := signature.SplitSignatureProviderPrefix(rawSignature); ok && prefixedProvider == signature.SignatureProviderGPT {
					candidate = unprefixed
					hasGPTPrefix = true
				}
				if gptInfo, err := signature.InspectGPTReasoningSignature(candidate); err == nil {
					if gptInfo.RouterSuffix != "" && !allowRouterSuffix {
						rewrittenSignature = stripRouterSuffix(candidate, gptInfo.RouterSuffix)
					} else if hasGPTPrefix {
						rewrittenSignature = candidate
					}
				} else if !(keepForeign && rawSignature != "" && signature.DetectSignatureProvider(rawSignature) == signature.SignatureProviderUnknown) {
					reason = err.Error()
				}
			} else {
				// The target model belongs to a known non-GPT, non-Meta family
				// (such as Grok, Gemini or Claude served through a Responses
				// router). Only preserve signatures that match that target family
				// (or unknown-format blobs when keepForeign is enabled); foreign
				// GPT or other known-provider envelopes are stripped.
				keepSignature := false
				if rawSignature != "" {
					switch targetProvider {
					case signature.SignatureProviderGrok:
						if grokInfo, errGrok := signature.InspectGrokEncryptedContent(rawSignature); errGrok == nil {
							keepSignature = true
							if grokInfo.RouterSuffix != "" && !allowRouterSuffix {
								rewrittenSignature = stripRouterSuffix(rawSignature, grokInfo.RouterSuffix)
							}
						}
					case signature.SignatureProviderGemini, signature.SignatureProviderClaude:
						var normalized string
						normalized, keepSignature = signature.CompatibleSignatureForProvider(targetProvider, rawSignature)
						if keepSignature && normalized != "" && normalized != rawSignature {
							rewrittenSignature = normalized
						}
					}
					if !keepSignature && keepForeign {
						keepSignature = signature.DetectSignatureProvider(rawSignature) == signature.SignatureProviderUnknown
					}
				}
				if !keepSignature {
					reason = fmt.Sprintf("reasoning encrypted_content is not compatible with target provider %s", targetProvider)
				}
			}
		case gjson.Null:
			reason = "encrypted_content is null"
		default:
			reason = fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
		}
		if reason == "" {
			if rewrittenSignature != "" {
				if updatedItem, errSet := sjson.Set(nextItem, "encrypted_content", rewrittenSignature); errSet == nil {
					nextItem = updatedItem
					changed = true
					helps.LogWithRequestID(ctx).Debugf("%s: rewrote reasoning encrypted_content at input[%d] item_id=%q", provider, index, itemID)
				}
			}
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}

		dropped, err := sjson.Delete(nextItem, "encrypted_content")
		if err != nil {
			helps.LogWithRequestID(ctx).Debugf("%s: failed to drop invalid reasoning encrypted_content at input[%d]: %v", provider, index, err)
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}
		nextItem = dropped
		changed = true
		if !isCompat && stripOrphanReasoningIDs && item.Get("id").Exists() {
			if nextID, errID := sjson.Delete(nextItem, "id"); errID != nil {
				helps.LogWithRequestID(ctx).Debugf("%s: failed to drop reasoning id after invalid encrypted_content at input[%d]: %v", provider, index, errID)
			} else {
				nextItem = nextID
			}
		}

		startRebuild(index)
		keep(nextItem)

		helps.LogWithRequestID(ctx).Debugf("%s: dropped invalid reasoning encrypted_content at input[%d] item_id=%q reason=%s", provider, index, itemID, reason)
	}

	if rebuilt == nil {
		return body
	}
	rebuilt = append(rebuilt, ']')

	updated, err := sjson.SetRawBytes(body, "input", rebuilt)
	if err != nil {
		helps.LogWithRequestID(ctx).Debugf("%s: failed to rebuild input array while sanitizing reasoning encrypted_content: %v", provider, err)
		return body
	}
	return updated
}
