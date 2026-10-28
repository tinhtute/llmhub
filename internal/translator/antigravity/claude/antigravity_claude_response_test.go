package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/cache"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ============================================================================
// Signature Caching Tests
// ============================================================================

func TestConvertAntigravityResponseToClaude_ParamsInitialized(t *testing.T) {
	cache.ClearSignatureCache("")

	// Request with user message - should initialize params
	requestJSON := []byte(`{
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello world"}]}
		]
	}`)

	// First response chunk with thinking
	responseJSON := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "Let me think...", "thought": true}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, responseJSON, &param)

	params := param.(*Params)
	if !params.HasFirstResponse {
		t.Error("HasFirstResponse should be set after first chunk")
	}
	if params.CurrentThinkingText.Len() == 0 {
		t.Error("Thinking text should be accumulated")
	}
}

func TestConvertAntigravityResponseToClaude_ThinkingTextAccumulated(t *testing.T) {
	cache.ClearSignatureCache("")

	requestJSON := []byte(`{
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Test"}]}]
	}`)

	// First thinking chunk
	chunk1 := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "First part of thinking...", "thought": true}]
				}
			}]
		}
	}`)

	// Second thinking chunk (continuation)
	chunk2 := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": " Second part of thinking...", "thought": true}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()

	// Process first chunk - starts new thinking block
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk1, &param)
	params := param.(*Params)

	if params.CurrentThinkingText.Len() == 0 {
		t.Error("Thinking text should be accumulated after first chunk")
	}

	// Process second chunk - continues thinking block
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk2, &param)

	text := params.CurrentThinkingText.String()
	if !strings.Contains(text, "First part") || !strings.Contains(text, "Second part") {
		t.Errorf("Thinking text should accumulate both parts, got: %s", text)
	}
}

func TestConvertAntigravityResponseToClaude_SignatureCached(t *testing.T) {
	cache.ClearSignatureCache("")

	requestJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Cache test"}]}]
	}`)

	// Thinking chunk
	thinkingChunk := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "My thinking process here", "thought": true}]
				}
			}]
		}
	}`)

	// Signature chunk
	validSignature := "abc123validSignature1234567890123456789012345678901234567890"
	signatureChunk := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "", "thought": true, "thoughtSignature": "` + validSignature + `"}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()

	// Process thinking chunk
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, thinkingChunk, &param)
	params := param.(*Params)
	thinkingText := params.CurrentThinkingText.String()

	if thinkingText == "" {
		t.Fatal("Thinking text should be accumulated")
	}

	// Process signature chunk - should cache the signature
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, signatureChunk, &param)

	// Verify signature was cached
	cachedSig := cache.GetCachedSignature("claude-sonnet-4-5-thinking", thinkingText)
	if cachedSig != validSignature {
		t.Errorf("Expected cached signature '%s', got '%s'", validSignature, cachedSig)
	}

	// Verify thinking text was reset after caching
	if params.CurrentThinkingText.Len() != 0 {
		t.Error("Thinking text should be reset after signature is cached")
	}
}

func TestConvertAntigravityResponseToClaude_MultipleThinkingBlocks(t *testing.T) {
	cache.ClearSignatureCache("")

	requestJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Multi block test"}]}]
	}`)

	validSig1 := "signature1_12345678901234567890123456789012345678901234567"
	validSig2 := "signature2_12345678901234567890123456789012345678901234567"

	// First thinking block with signature
	block1Thinking := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "First thinking block", "thought": true}]
				}
			}]
		}
	}`)
	block1Sig := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "", "thought": true, "thoughtSignature": "` + validSig1 + `"}]
				}
			}]
		}
	}`)

	// Text content (breaks thinking)
	textBlock := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "Regular text output"}]
				}
			}]
		}
	}`)

	// Second thinking block with signature
	block2Thinking := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "Second thinking block", "thought": true}]
				}
			}]
		}
	}`)
	block2Sig := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "", "thought": true, "thoughtSignature": "` + validSig2 + `"}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()

	// Process first thinking block
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block1Thinking, &param)
	params := param.(*Params)
	firstThinkingText := params.CurrentThinkingText.String()

	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block1Sig, &param)

	// Verify first signature cached
	if cache.GetCachedSignature("claude-sonnet-4-5-thinking", firstThinkingText) != validSig1 {
		t.Error("First thinking block signature should be cached")
	}

	// Process text (transitions out of thinking)
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, textBlock, &param)

	// Process second thinking block
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block2Thinking, &param)
	secondThinkingText := params.CurrentThinkingText.String()

	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block2Sig, &param)

	// Verify second signature cached
	if cache.GetCachedSignature("claude-sonnet-4-5-thinking", secondThinkingText) != validSig2 {
		t.Error("Second thinking block signature should be cached")
	}
}

func TestConvertAntigravityResponseToClaude_TextAndSignatureInSameChunk(t *testing.T) {
	cache.ClearSignatureCache("")

	requestJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Test"}]}]
	}`)

	validSignature := "RtestSig1234567890123456789012345678901234567890123456789"

	// Chunk 1: thinking text only (no signature)
	chunk1 := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "First part.", "thought": true}]
				}
			}]
		}
	}`)

	// Chunk 2: thinking text AND signature in the same part
	chunk2 := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": " Second part.", "thought": true, "thoughtSignature": "` + validSignature + `"}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()

	result1 := ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk1, &param)
	result2 := ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk2, &param)

	allOutput := string(bytes.Join(result1, nil)) + string(bytes.Join(result2, nil))

	// The text " Second part." must appear as a thinking_delta, not be silently dropped
	if !strings.Contains(allOutput, "Second part.") {
		t.Error("Text co-located with signature must be emitted as thinking_delta before the signature")
	}

	// The signature must also be emitted
	if !strings.Contains(allOutput, "signature_delta") {
		t.Error("Signature delta must still be emitted")
	}

	// Verify the cached signature covers the FULL text (both parts)
	fullText := "First part. Second part."
	cachedSig := cache.GetCachedSignature("claude-sonnet-4-5-thinking", fullText)
	if cachedSig != validSignature {
		t.Errorf("Cached signature should cover full text %q, got sig=%q", fullText, cachedSig)
	}
}

func TestConvertAntigravityResponseToClaude_SignatureOnlyChunk(t *testing.T) {
	cache.ClearSignatureCache("")

	requestJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Test"}]}]
	}`)

	validSignature := "RtestSig1234567890123456789012345678901234567890123456789"

	// Chunk 1: thinking text
	chunk1 := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "Full thinking text.", "thought": true}]
				}
			}]
		}
	}`)

	// Chunk 2: signature only (empty text) — the normal case
	chunk2 := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "", "thought": true, "thoughtSignature": "` + validSignature + `"}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()

	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk1, &param)
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk2, &param)

	cachedSig := cache.GetCachedSignature("claude-sonnet-4-5-thinking", "Full thinking text.")
	if cachedSig != validSignature {
		t.Errorf("Signature-only chunk should still cache correctly, got %q", cachedSig)
	}
}

// Ported from upstream CLIProxyAPI commit 15231e9fdc93 ("support native thinking
// signatures without prefixes in claude translator"): formatClaudeSignatureValue
// emits provider-native signatures without "<group>#" prefixes, and the
// request-side round-trip accepts them in cache mode. The local translator does
// not emit signatures on tool_use content blocks, so only thinking signatures
// are covered here.
func TestConvertAntigravityResponseToClaude_EmitsNativeSignaturesWithoutProviderPrefixes(t *testing.T) {
	cache.ClearSignatureCache("")
	previousCache := cache.SignatureCacheEnabled()
	cache.SetSignatureCacheEnabled(true)
	t.Cleanup(func() {
		cache.SetSignatureCacheEnabled(previousCache)
		cache.ClearSignatureCache("")
	})

	nativeSig := testAnthropicNativeSignature(t)
	upstreamSig := base64.StdEncoding.EncodeToString([]byte(nativeSig))
	requestJSON := []byte(`{"model":"claude-sonnet-4-6"}`)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"thought content","thought":true,"thoughtSignature":"` + upstreamSig + `"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"thoughtsTokenCount":1,"totalTokenCount":3},"modelVersion":"claude-sonnet-4-6-thinking","responseId":"resp-claude-native-sigs"}}`)

	nonStream := ConvertAntigravityResponseToClaudeNonStream(context.Background(), "claude-sonnet-4-6", requestJSON, requestJSON, responseJSON, nil)
	thinkingSig := gjson.GetBytes(nonStream, "content.0.signature").String()
	if strings.Contains(thinkingSig, "#") {
		t.Fatalf("thinking signature contains prefix: %q, want provider-native %q", thinkingSig, nativeSig)
	}
	if thinkingSig != nativeSig {
		t.Fatalf("thinking signature = %q, want provider-native %q", thinkingSig, nativeSig)
	}

	var param any
	stream := bytes.Join(ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-6", requestJSON, requestJSON, responseJSON, &param), nil)
	streamText := string(stream)
	if strings.Contains(streamText, `"signature":"claude#`) {
		t.Fatalf("streaming response contains claude# prefix: %s", streamText)
	}
	if !strings.Contains(streamText, `"signature":"`+nativeSig+`"`) {
		t.Fatalf("streaming thinking signature missing or mismatched: %s", streamText)
	}

	// Verify multi-turn replay with native signatures in cache mode.
	replayRequest := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[]},{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
	replayRequest, _ = sjson.SetRawBytes(replayRequest, "messages.0.content", []byte(gjson.GetBytes(nonStream, "content").Raw))
	replayRequest = StripEmptySignatureThinkingBlocks(replayRequest)
	translated := ConvertClaudeRequestToAntigravity("claude-sonnet-4-6", replayRequest, false)
	parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
	if len(parts) != 1 || parts[0].Get("thoughtSignature").String() != upstreamSig {
		t.Fatalf("Claude native thought signature did not round-trip in cache mode: %s", translated)
	}
}

func sseDataForEvent(t *testing.T, output string, eventName string) string {
	t.Helper()

	currentEvent := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if currentEvent == eventName && strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}

	t.Fatalf("event %q not found in:\n%s", eventName, output)
	return ""
}

func TestConvertAntigravityResponseToClaude_CachedToken_DoesNotCountCachedAsOutput(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.8-flash-high"}`)
	responseJSON := []byte(`{
		"response": {
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": "ok"}]},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount": 142871, "cachedContentTokenCount": 140830, "candidatesTokenCount": 0, "thoughtsTokenCount": 0, "totalTokenCount": 142871},
			"modelVersion": "gemini-3.8-flash-n",
			"responseId": "resp_cached_zero_candidates"
		}
	}`)

	var param any
	output := bytes.Join(ConvertAntigravityResponseToClaude(context.Background(), "gemini-3.8-flash-high", requestJSON, requestJSON, responseJSON, &param), nil)
	output = append(output, bytes.Join(ConvertAntigravityResponseToClaude(context.Background(), "gemini-3.8-flash-high", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)

	messageDelta := sseDataForEvent(t, string(output), "message_delta")
	if got := gjson.Get(messageDelta, "usage.input_tokens").Int(); got != 2041 {
		t.Fatalf("input_tokens = %d, want 2041: %s", got, messageDelta)
	}
	if outputTokens := gjson.Get(messageDelta, "usage.output_tokens"); !outputTokens.Exists() || outputTokens.Int() != 0 {
		t.Fatalf("output_tokens = %s, want 0: %s", outputTokens.Raw, messageDelta)
	}
	if got := gjson.Get(messageDelta, "usage.cache_read_input_tokens").Int(); got != 140830 {
		t.Fatalf("cache_read_input_tokens = %d, want 140830: %s", got, messageDelta)
	}
}

func TestConvertAntigravityResponseToClaude_CachedToken_EmptyResponseEmitsZeroEventsOnDone(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.8-flash-high"}`)
	responseJSON := []byte(`{
		"response": {
			"candidates": [{
				"content": {"role": "model", "parts": []},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount": 142871, "cachedContentTokenCount": 140830, "candidatesTokenCount": 0, "thoughtsTokenCount": 0, "totalTokenCount": 142871},
			"modelVersion": "gemini-3.8-flash-n",
			"responseId": "resp_cached_empty_candidates"
		}
	}`)

	var param any
	output := bytes.Join(ConvertAntigravityResponseToClaude(context.Background(), "gemini-3.8-flash-high", requestJSON, requestJSON, responseJSON, &param), nil)
	doneOutput := bytes.Join(ConvertAntigravityResponseToClaude(context.Background(), "gemini-3.8-flash-high", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)

	if len(doneOutput) != 0 {
		t.Fatalf("expected 0 bytes on [DONE] for empty response, got %q", string(doneOutput))
	}
	if strings.Contains(string(output), "message_delta") {
		t.Fatalf("unexpected message_delta emitted for response with no content: %s", string(output))
	}
}

func TestConvertAntigravityResponseToClaude_CachedToken_AppendFinalEventsFallbackUsesRawPrompt(t *testing.T) {
	params := &Params{
		HasUsageMetadata:     true,
		HasContent:           true,
		PromptTokenCount:     2041,
		CachedTokenCount:     140830,
		CandidatesTokenCount: 0,
		ThoughtsTokenCount:   0,
		TotalTokenCount:      142871,
	}
	output := make([]byte, 0, 256)
	appendFinalEvents(params, &output, true)

	messageDelta := sseDataForEvent(t, string(output), "message_delta")
	if outputTokens := gjson.Get(messageDelta, "usage.output_tokens"); !outputTokens.Exists() || outputTokens.Int() != 0 {
		t.Fatalf("output_tokens = %s, want 0: %s", outputTokens.Raw, messageDelta)
	}
	if got := gjson.Get(messageDelta, "usage.cache_read_input_tokens").Int(); got != 140830 {
		t.Fatalf("cache_read_input_tokens = %d, want 140830: %s", got, messageDelta)
	}
}

func TestConvertAntigravityResponseToClaude_ClaudeTool_SanitizesParameterlessToolArguments(t *testing.T) {
	origRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"EnterPlanMode","input_schema":{"type":"object","properties":{}}}]}`)
	translatedRequest := []byte(`{"model":"gemini-3.8-flash-high"}`)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"EnterPlanMode","args":{"reason":"Switching to plan mode to inspect files"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-1"}}`)

	nonStream := ConvertAntigravityResponseToClaudeNonStream(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, responseJSON, nil)
	if got := gjson.GetBytes(nonStream, "content.0.input").Raw; got != "{}" {
		t.Fatalf("expected non-streaming input {}, got %s", got)
	}

	var param any
	streamChunks := ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, responseJSON, &param)
	streamText := string(bytes.Join(streamChunks, nil))
	var deltaPartial strings.Builder
	for _, line := range strings.Split(streamText, "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if gjson.Get(payload, "delta.type").String() == "input_json_delta" {
				deltaPartial.WriteString(gjson.Get(payload, "delta.partial_json").String())
			}
		}
	}
	if got := deltaPartial.String(); got != "{}" {
		t.Fatalf("expected streaming partial_json {}, got %q (full output: %s)", got, streamText)
	}
}

func TestConvertAntigravityResponseToClaude_ClaudeTool_StripsExecutorInjectedUnderscore(t *testing.T) {
	origRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"WrappedPlaceholder","input_schema":{"type":"object","properties":{"flag":{"type":"string"}},"additionalProperties":false}}]}`)
	translatedRequest := ConvertClaudeRequestToAntigravity("claude-sonnet-4-6", origRequest, false)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"WrappedPlaceholder","args":{"flag":"x","_":true}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-wrapped"}}`)

	nonStream := ConvertAntigravityResponseToClaudeNonStream(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, responseJSON, nil)
	if got := gjson.GetBytes(nonStream, "content.0.input").Raw; got != `{"flag":"x"}` {
		t.Fatalf("expected executor-injected underscore stripped, got %s (translated: %s)", got, string(translatedRequest))
	}
}

func TestConvertAntigravityResponseToClaude_ClaudeTool_ParameterlessContinuationDoesNotDuplicateEmptyObject(t *testing.T) {
	origRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"EnterPlanMode","input_schema":{"type":"object","properties":{}}}]}`)
	translatedRequest := []byte(`{"model":"gemini-3.8-flash-high"}`)
	firstChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"EnterPlanMode","args":{"reason":"Switching to plan mode"}}}]}}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-1"}}`)
	continuationChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"reason":"more"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-1"}}`)

	var param any
	streamBytes := bytes.Join(ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, firstChunk, &param), nil)
	streamBytes = append(streamBytes, bytes.Join(ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, continuationChunk, &param), nil)...)
	streamText := string(streamBytes)
	var deltaPartial strings.Builder
	toolUseStarts := 0
	for _, line := range strings.Split(streamText, "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if gjson.Get(payload, "type").String() == "content_block_start" && gjson.Get(payload, "content_block.type").String() == "tool_use" {
				toolUseStarts++
			}
			if gjson.Get(payload, "delta.type").String() == "input_json_delta" {
				deltaPartial.WriteString(gjson.Get(payload, "delta.partial_json").String())
			}
		}
	}
	if toolUseStarts != 1 {
		t.Fatalf("expected exactly one tool_use block, got %d (full output: %s)", toolUseStarts, streamText)
	}
	if got := deltaPartial.String(); got != "{}" {
		t.Fatalf("expected concatenated partial_json {}, got %q (full output: %s)", got, streamText)
	}
	if strings.Contains(streamText, `"name":""`) {
		t.Fatalf("continuation must not open an unnamed tool_use block: %s", streamText)
	}
}

func TestConvertAntigravityResponseToClaude_ClaudeTool_KeepsReasonWhenTranslatedSchemaHasNoPlaceholder(t *testing.T) {
	origRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"BashOptions","input_schema":{"type":"object","properties":{"flag":{"type":"string"}},"required":["flag"]}}]}`)
	translatedRequest := ConvertClaudeRequestToAntigravity("claude-sonnet-4-6", origRequest, false)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"BashOptions","args":{"flag":"x","reason":"why"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-bash-reason"}}`)

	nonStream := ConvertAntigravityResponseToClaudeNonStream(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, responseJSON, nil)
	if got := gjson.GetBytes(nonStream, "content.0.input").Raw; got != `{"flag":"x","reason":"why"}` {
		t.Fatalf("expected non-streaming input preserved on translated branch, got %s (translated: %s)", got, string(translatedRequest))
	}

	// Verify discriminating behavior: fallback without translated schema strips "reason".
	fallbackOnly := ConvertAntigravityResponseToClaudeNonStream(context.Background(), "claude-sonnet-4-6", origRequest, nil, responseJSON, nil)
	if got := gjson.GetBytes(fallbackOnly, "content.0.input").Raw; got != `{"flag":"x"}` {
		t.Fatalf("expected fallback to strip reason, got %s", got)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_ZeroCandidatesDoesNotCountCachedAsOutput(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.8-flash-high"}`)
	responseJSON := []byte(`{
		"response": {
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": "hi"}]},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount": 142871, "cachedContentTokenCount": 140830, "candidatesTokenCount": 0, "thoughtsTokenCount": 0, "totalTokenCount": 142871},
			"modelVersion": "gemini-3.8-flash-n",
			"responseId": "resp_cached_zero_candidates"
		}
	}`)

	ctx := context.Background()
	out := ConvertAntigravityResponseToClaudeNonStream(ctx, "gemini-3.8-flash-high", requestJSON, requestJSON, responseJSON, nil)
	usageJSON := gjson.GetBytes(out, "usage")
	if got := usageJSON.Get("output_tokens").Int(); got != 0 {
		t.Fatalf("output_tokens = %d, want 0 when candidatesTokenCount is 0. Output: %s", got, string(out))
	}
	if got := usageJSON.Get("input_tokens").Int(); got != 2041 {
		t.Fatalf("input_tokens = %d, want 2041 (142871 - 140830). Output: %s", got, string(out))
	}
	if got := usageJSON.Get("cache_read_input_tokens").Int(); got != 140830 {
		t.Fatalf("cache_read_input_tokens = %d, want 140830. Output: %s", got, string(out))
	}
}

// --- R6 byte-compat pin ------------------------------------------------------
// Hashes captured from the pre-port implementation and held identical across the
// per-stream request-metadata cache port in this package (plan v8040-p3,
// upstream c0e619d).

var byteCompatHashes = map[string]string{
	"stream":    "c6538ac17caa760b626063e8f3a32f9e41e2b5d13a794c5f4d33cce96cb8fe56",
	"nonstream": "d545bc910a7e66c53db600dbae67b15ab286ef58e034145dc1116bb3f6baa597",
}

// byteCompatVolatile masks timestamp and synthesized-id byte runs (Unix seconds,
// UnixNano decimal or hex) so the pin holds everything else exactly; no field
// order, name, namespace or echo field is masked.
var byteCompatVolatile = regexp.MustCompile(`[0-9a-f]{10,}`)

func byteCompatHash(frames [][]byte) string {
	h := sha256.New()
	for _, frame := range frames {
		masked := byteCompatVolatile.ReplaceAll(frame, []byte("<volatile>"))
		fmt.Fprintf(h, "%d:", len(masked))
		_, _ = h.Write(masked)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func checkByteCompat(t *testing.T, name string, frames [][]byte) {
	t.Helper()
	got := byteCompatHash(frames)
	want := byteCompatHashes[name]
	if want == "" {
		t.Fatalf("bytecompat capture %s: %s", name, got)
	}
	if got != want {
		t.Fatalf("%s byte-compat hash = %s, want %s", name, got, want)
	}
}

func TestByteCompatAntigravityClaude(t *testing.T) {
	cache.ClearSignatureCache("")
	atomic.StoreUint64(&toolUseIDCounter, 0)
	request := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	signature := testAnthropicNativeSignature(t)

	var param any
	var stream [][]byte
	for _, frame := range []string{
		`{"response":{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true,"thoughtSignature":"` + signature + `"}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7},"modelVersion":"claude-sonnet-4-5","responseId":"r1"}}`,
		`[DONE]`,
	} {
		stream = append(stream, ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-5", request, request, []byte(frame), &param)...)
	}
	checkByteCompat(t, "stream", stream)

	nonStream := ConvertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"claude-sonnet-4-5",
		request,
		request,
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"modelVersion":"claude-sonnet-4-5","responseId":"r2"}}`),
		nil,
	)
	checkByteCompat(t, "nonstream", [][]byte{nonStream})
}

// TestConvertAntigravityResponseToClaude_ImplicitOpenToolContinuationOrdering is the
// Antigravity twin of the Gemini ordering pin: a tool schema that only permits dynamic
// properties keeps the arguments the model sent, and an empty "{}" chunk never
// concatenates with or precedes the real arguments as an extra object.
func TestConvertAntigravityResponseToClaude_ImplicitOpenToolContinuationOrdering(t *testing.T) {
	origRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"echo","input_schema":{"type":"object","properties":{}}}]}`)
	// The Antigravity executor re-sanitizes the sent tool schema, which injects the
	// "reason" placeholder as a required property; the response translator therefore sees
	// it and must strip it. Shape the translated request the way the executor sends it.
	injectedSchema := util.CleanJSONSchemaForAntigravity(`{"type":"object","properties":{}}`)
	translatedRequest := []byte(`{"model":"gemini-3.8-flash-high","request":{"tools":[{"functionDeclarations":[{"name":"echo","parametersJsonSchema":` + injectedSchema + `}]}]}}`)
	ctx := context.Background()

	deltas := func(stream [][]byte) string {
		var partial strings.Builder
		for _, line := range strings.Split(string(bytes.Join(stream, nil)), "\n") {
			if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				if gjson.Get(payload, "delta.type").String() == "input_json_delta" {
					partial.WriteString(gjson.Get(payload, "delta.partial_json").String())
				}
			}
		}
		return partial.String()
	}

	for _, tc := range []struct {
		name      string
		firstArgs string
		contArgs  string
		want      string
	}{
		{"empty_then_real_args", `{}`, `{"user":"v"}`, `{"user":"v"}`},
		{"placeholder_then_real_args", `{"reason":"thinking"}`, `{"user":"v"}`, `{"user":"v"}`},
		{"real_args_then_empty", `{"user":"v"}`, `{}`, `{"user":"v"}`},
		{"real_args_then_placeholder", `{"user":"v"}`, `{"reason":"done"}`, `{"user":"v"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":` + tc.firstArgs + `}}]}}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-echo"}}`)
			continuationChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":` + tc.contArgs + `}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-echo"}}`)

			var param any
			stream := ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-6", origRequest, translatedRequest, firstChunk, &param)
			stream = append(stream, ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-6", origRequest, translatedRequest, continuationChunk, &param)...)
			if got := deltas(stream); got != tc.want {
				t.Fatalf("expected concatenated partial_json %s, got %q (stream: %s)", tc.want, got, string(bytes.Join(stream, nil)))
			}
		})
	}

	t.Run("explicitly_closed_schema_still_normalizes", func(t *testing.T) {
		closedRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"echo","input_schema":{"type":"object","additionalProperties":false}}]}`)
		closedTranslated := ConvertClaudeRequestToAntigravity("claude-sonnet-4-6", closedRequest, false)
		chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":{"user":"v"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-closed"}}`)
		var param any
		stream := ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-6", closedRequest, closedTranslated, chunk, &param)
		if got := deltas(stream); got != `{}` {
			t.Fatalf("expected explicitly closed schema to normalize to {}, got %q (stream: %s)", got, string(bytes.Join(stream, nil)))
		}
	})

	// The non-streaming caller takes the same decision through the same helpers: the
	// injected placeholder is stripped and the real argument is kept.
	t.Run("non_stream_strips_placeholder_and_keeps_user_arg", func(t *testing.T) {
		chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":{"user":"v","reason":"why"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-nonstream"}}`)
		nonStream := ConvertAntigravityResponseToClaudeNonStream(ctx, "claude-sonnet-4-6", origRequest, translatedRequest, chunk, nil)
		if got := gjson.GetBytes(nonStream, "content.0.input").Raw; got != `{"user":"v"}` {
			t.Fatalf("expected non-streaming input {\"user\":\"v\"}, got %s", got)
		}
	})
}
