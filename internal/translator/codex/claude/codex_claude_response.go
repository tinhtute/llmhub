// Package claude provides response translation functionality for Codex to Claude Code API compatibility.
// This package handles the conversion of Codex API responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"math"
	"strings"

	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// ConvertCodexResponseToClaudeParams holds parameters for response conversion.
type ConvertCodexResponseToClaudeParams struct {
	HasToolCall            bool
	BlockIndex             int
	HasTextDelta           bool
	ActiveTextBlockKey     string
	TextBlocks             map[string]*codexTextBlockState
	ThinkingBlockOpen      bool
	ThinkingBlockIndex     int
	ThinkingBlockIndexSet  bool
	ThinkingStopPending    bool
	ThinkingSignature      string
	ThinkingSummarySeen    bool
	ActiveThinkingItemKey  string
	FinishedThinkingItems  map[string]struct{}
	ActiveFunctionBlockKey string
	FunctionBlocks         map[string]*codexFunctionBlockState
	FunctionBlockQueue     []string
}

type codexTextBlockState struct {
	Index int
	Open  bool
}

type codexFunctionBlockState struct {
	Index                     int
	Open                      bool
	Started                   bool
	Done                      bool
	Closed                    bool
	CallID                    string
	Name                      string
	BufferedArgumentDeltas    []string
	HasReceivedArgumentsDelta bool
}

// ConvertCodexResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates Codex API responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of Claude Code-compatible JSON responses
func ConvertCodexResponseToClaude(_ context.Context, _ string, originalRequestRawJSON, _ []byte, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &ConvertCodexResponseToClaudeParams{
			HasToolCall: false,
			BlockIndex:  0,
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	output := make([]byte, 0, 512)
	rootResult := gjson.ParseBytes(rawJSON)
	params := (*param).(*ConvertCodexResponseToClaudeParams)
	if params.ThinkingBlockOpen && params.ThinkingStopPending {
		switch rootResult.Get("type").String() {
		case "response.content_part.added", "response.completed", "response.incomplete":
			output = append(output, finalizeCodexThinkingBlock(params)...)
		}
	}

	typeResult := rootResult.Get("type")
	typeStr := typeResult.String()
	var template []byte

	if typeStr == "response.created" {
		template = []byte(`{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"claude-opus-4-1-20250805","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}`)
		template, _ = sjson.SetBytes(template, "message.model", rootResult.Get("response.model").String())
		template, _ = sjson.SetBytes(template, "message.id", rootResult.Get("response.id").String())

		output = translatorcommon.AppendSSEEventBytes(output, "message_start", template, 2)
	} else if typeStr == "response.reasoning_summary_part.added" {
		if codexThinkingItemIsFinished(params, rootResult) {
			return [][]byte{output}
		}
		output = append(output, stopActiveCodexTextBlock(params)...)
		output = append(output, selectCodexThinkingItem(params, rootResult)...)
		if params.ThinkingBlockOpen && params.ThinkingStopPending {
			output = append(output, finalizeCodexThinkingBlock(params)...)
		}
		params.ThinkingSummarySeen = true
		output = append(output, startCodexThinkingBlock(params, rootResult)...)
	} else if typeStr == "response.reasoning_summary_text.delta" || typeStr == "response.reasoning_text.delta" {
		// Muse and routed Responses providers stream reasoning_text rather than
		// reasoning_summary_text, and the signature that arrives on the finished
		// item belongs to that text (upstream CLIProxyAPI a68c7d8ed7c2).
		if codexThinkingItemIsFinished(params, rootResult) {
			return [][]byte{output}
		}
		output = append(output, startCodexThinkingBlock(params, rootResult)...)
		template = []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
		template, _ = sjson.SetBytes(template, "index", codexThinkingBlockIndex(params, rootResult))
		template, _ = sjson.SetBytes(template, "delta.thinking", rootResult.Get("delta").String())

		params.ThinkingSummarySeen = true
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
	} else if typeStr == "response.reasoning_summary_part.done" {
		if codexThinkingItemIsActive(params, rootResult) {
			params.ThinkingStopPending = true
		}
	} else if typeStr == "response.content_part.added" {
		output = append(output, startCodexTextBlock(params, rootResult)...)
	} else if typeStr == "response.output_text.delta" {
		params.HasTextDelta = true
		textBlockKey := codexTextBlockKey(rootResult)
		output = append(output, startCodexTextBlock(params, rootResult)...)
		if textBlock := params.TextBlocks[textBlockKey]; textBlock != nil && textBlock.Open {
			template = []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
			template, _ = sjson.SetBytes(template, "index", textBlock.Index)
			template, _ = sjson.SetBytes(template, "delta.text", rootResult.Get("delta").String())

			output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
		}
	} else if typeStr == "response.content_part.done" {
		output = append(output, stopCodexTextBlock(params, codexTextBlockKey(rootResult))...)
	} else if typeStr == "response.completed" || typeStr == "response.incomplete" {
		output = append(output, stopActiveCodexTextBlock(params)...)
		output = append(output, stopActiveCodexFunctionBlock(params)...)
		template = []byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`)
		responseData := rootResult.Get("response")
		template, _ = sjson.SetBytes(template, "delta.stop_reason", mapCodexStopReasonToClaude(codexStopReason(responseData), params.HasToolCall))
		template = setClaudeStopSequence(template, "delta.stop_sequence", responseData)
		inputTokens, outputTokens, cachedTokens, cacheWriteTokens := extractResponsesUsage(responseData.Get("usage"))
		template, _ = sjson.SetBytes(template, "usage.input_tokens", inputTokens)
		template, _ = sjson.SetBytes(template, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			template, _ = sjson.SetBytes(template, "usage.cache_read_input_tokens", cachedTokens)
		}
		if cacheWriteTokens > 0 {
			template, _ = sjson.SetBytes(template, "usage.cache_creation_input_tokens", cacheWriteTokens)
		}
		template = setClaudeReasoningUsage(template, responseData.Get("usage"))

		output = translatorcommon.AppendSSEEventBytes(output, "message_delta", template, 2)
		output = translatorcommon.AppendSSEEventBytes(output, "message_stop", []byte(`{"type":"message_stop"}`), 2)
	} else if typeStr == "response.output_item.added" {
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		if itemType == "function_call" {
			output = append(output, finalizeCodexThinkingBlock(params)...)
			output = append(output, stopActiveCodexTextBlock(params)...)
			functionBlock, key := ensureCodexFunctionBlock(params, rootResult)
			updateCodexFunctionBlockIdentity(functionBlock, itemResult)
			enqueueCodexFunctionBlock(params, key)
			output = appendCodexFunctionBlockQueue(output, params, originalRequestRawJSON)
		} else if itemType == "reasoning" {
			if codexThinkingItemIsFinished(params, rootResult) {
				return [][]byte{output}
			}
			output = append(output, selectCodexThinkingItem(params, rootResult)...)
			params.ThinkingSummarySeen = false
			params.ThinkingSignature = itemResult.Get("encrypted_content").String()
		}
	} else if typeStr == "response.output_item.done" {
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		if itemType == "message" {
			if params.HasTextDelta {
				return [][]byte{output}
			}
			contentResult := itemResult.Get("content")
			if !contentResult.Exists() || !contentResult.IsArray() {
				return [][]byte{output}
			}
			var textBuilder strings.Builder
			contentResult.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() != "output_text" {
					return true
				}
				if txt := part.Get("text").String(); txt != "" {
					textBuilder.WriteString(txt)
				}
				return true
			})
			text := textBuilder.String()
			if text == "" {
				return [][]byte{output}
			}

			output = append(output, finalizeCodexThinkingBlock(params)...)
			textBlockKey := codexTextBlockKey(rootResult)
			output = append(output, startCodexTextBlock(params, rootResult)...)
			if textBlock := params.TextBlocks[textBlockKey]; textBlock != nil && textBlock.Open {
				template = []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
				template, _ = sjson.SetBytes(template, "index", textBlock.Index)
				template, _ = sjson.SetBytes(template, "delta.text", text)
				output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
			}

			params.HasTextDelta = true
			output = append(output, stopCodexTextBlock(params, textBlockKey)...)
		} else if itemType == "function_call" {
			functionBlock, key := ensureCodexFunctionBlock(params, rootResult)
			updateCodexFunctionBlockIdentity(functionBlock, itemResult)
			if args := itemResult.Get("arguments").String(); args != "" && !functionBlock.HasReceivedArgumentsDelta {
				appendCodexFunctionBlockArguments(functionBlock, args)
			}
			functionBlock.Done = true
			enqueueCodexFunctionBlock(params, key)
			output = appendCodexFunctionBlockQueue(output, params, originalRequestRawJSON)
		} else if itemType == "reasoning" {
			if !codexThinkingItemIsActive(params, rootResult) {
				return [][]byte{output}
			}
			output = append(output, selectCodexThinkingItem(params, rootResult)...)
			if signature := itemResult.Get("encrypted_content").String(); signature != "" {
				params.ThinkingSignature = signature
			}
			if !params.ThinkingSummarySeen {
				if doneText := extractCodexReasoningItemText(itemResult); doneText != "" {
					output = append(output, startCodexThinkingBlock(params, rootResult)...)
					deltaTemplate := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
					deltaTemplate, _ = sjson.SetBytes(deltaTemplate, "index", codexThinkingBlockIndex(params, rootResult))
					deltaTemplate, _ = sjson.SetBytes(deltaTemplate, "delta.thinking", doneText)
					output = append(output, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", deltaTemplate, 2)...)
					params.ThinkingSummarySeen = true
				}
			}
			if params.ThinkingSummarySeen {
				output = append(output, finalizeCodexThinkingBlock(params)...)
			} else {
				output = append(output, finalizeCodexSignatureOnlyThinkingBlock(params, rootResult)...)
			}
			markActiveCodexThinkingItemFinished(params)
			resetCodexThinkingItem(params)
		}
	} else if typeStr == "response.function_call_arguments.delta" {
		if functionBlock := codexFunctionBlock(params, rootResult); functionBlock != nil {
			functionBlock.HasReceivedArgumentsDelta = true
			appendCodexFunctionBlockArguments(functionBlock, rootResult.Get("delta").String())
			output = appendCodexFunctionBlockQueue(output, params, originalRequestRawJSON)
		}
	} else if typeStr == "response.function_call_arguments.done" {
		if functionBlock := codexFunctionBlock(params, rootResult); functionBlock != nil && !functionBlock.HasReceivedArgumentsDelta {
			if args := rootResult.Get("arguments").String(); args != "" {
				functionBlock.HasReceivedArgumentsDelta = true
				appendCodexFunctionBlockArguments(functionBlock, args)
				output = appendCodexFunctionBlockQueue(output, params, originalRequestRawJSON)
			}
		}
	}

	return [][]byte{output}
}

// ConvertCodexResponseToClaudeNonStream converts a non-streaming Codex response to a non-streaming Claude Code response.
// This function processes the complete Codex response and transforms it into a single Claude Code-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Claude Code API format.
func ConvertCodexResponseToClaudeNonStream(_ context.Context, _ string, originalRequestRawJSON, _ []byte, rawJSON []byte, _ *any) []byte {
	revNames := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)

	rootResult := gjson.ParseBytes(rawJSON)
	typeStr := rootResult.Get("type").String()
	if typeStr != "response.completed" && typeStr != "response.incomplete" {
		return []byte{}
	}

	responseData := rootResult.Get("response")
	if !responseData.Exists() {
		return []byte{}
	}

	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", responseData.Get("id").String())
	out, _ = sjson.SetBytes(out, "model", responseData.Get("model").String())
	inputTokens, outputTokens, cachedTokens, cacheWriteTokens := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
	if cachedTokens > 0 {
		out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cachedTokens)
	}
	if cacheWriteTokens > 0 {
		out, _ = sjson.SetBytes(out, "usage.cache_creation_input_tokens", cacheWriteTokens)
	}
	out = setClaudeReasoningUsage(out, responseData.Get("usage"))

	hasToolCall := false
	var contentBlocks [][]byte

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "reasoning":
				thinkingText := extractCodexReasoningItemText(item)
				signature := item.Get("encrypted_content").String()
				if thinkingText != "" || signature != "" {
					block := []byte(`{"type":"thinking","thinking":""}`)
					block, _ = sjson.SetBytes(block, "thinking", thinkingText)
					if signature != "" {
						block, _ = sjson.SetBytes(block, "signature", signature)
					}
					contentBlocks = append(contentBlocks, block)
				}
			case "message":
				if content := item.Get("content"); content.Exists() {
					if content.IsArray() {
						content.ForEach(func(_, part gjson.Result) bool {
							if part.Get("type").String() == "output_text" {
								text := part.Get("text").String()
								if text != "" {
									block := []byte(`{"type":"text","text":""}`)
									block, _ = sjson.SetBytes(block, "text", text)
									contentBlocks = append(contentBlocks, block)
								}
							}
							return true
						})
					} else {
						text := content.String()
						if text != "" {
							block := []byte(`{"type":"text","text":""}`)
							block, _ = sjson.SetBytes(block, "text", text)
							contentBlocks = append(contentBlocks, block)
						}
					}
				}
			case "function_call":
				hasToolCall = true
				name := item.Get("name").String()
				if original, ok := revNames[name]; ok {
					name = original
				}

				toolBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
				toolBlock, _ = sjson.SetBytes(toolBlock, "id", shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(item.Get("call_id").String())))
				toolBlock, _ = sjson.SetBytes(toolBlock, "name", name)
				inputRaw := "{}"
				if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
					argsJSON := gjson.Parse(argsStr)
					if argsJSON.IsObject() {
						inputRaw = argsJSON.Raw
					}
				}
				toolBlock, _ = sjson.SetRawBytes(toolBlock, "input", []byte(inputRaw))
				contentBlocks = append(contentBlocks, toolBlock)
			}
			return true
		})
	}

	if len(contentBlocks) > 0 {
		out = translatorcommon.SetRawArrayItems(out, "content", contentBlocks)
	}

	out, _ = sjson.SetBytes(out, "stop_reason", mapCodexStopReasonToClaude(codexStopReason(responseData), hasToolCall))
	out = setClaudeStopSequence(out, "stop_sequence", responseData)

	return out
}

func codexStopReason(responseData gjson.Result) string {
	if stopReason := responseData.Get("stop_reason"); stopReason.Exists() && stopReason.String() != "" {
		if stopReason.String() == "stop" && codexStopSequence(responseData).String() != "" {
			return "stop_sequence"
		}
		return stopReason.String()
	}
	if reason := responseData.Get("incomplete_details.reason"); reason.Exists() && reason.String() != "" {
		return reason.String()
	}
	if codexStopSequence(responseData).String() != "" {
		return "stop_sequence"
	}
	return ""
}

func mapCodexStopReasonToClaude(stopReason string, hasToolCall bool) string {
	if hasToolCall {
		return "tool_use"
	}

	switch stopReason {
	case "", "stop", "completed":
		return "end_turn"
	case "max_tokens", "max_output_tokens":
		return "max_tokens"
	case "tool_use", "tool_calls", "function_call":
		return "tool_use"
	case "end_turn", "stop_sequence", "pause_turn", "refusal", "model_context_window_exceeded":
		return stopReason
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func codexStopSequence(responseData gjson.Result) gjson.Result {
	return responseData.Get("stop_sequence")
}

func setClaudeStopSequence(out []byte, path string, responseData gjson.Result) []byte {
	if stopSequence := codexStopSequence(responseData); stopSequence.Exists() && stopSequence.String() != "" {
		out, _ = sjson.SetRawBytes(out, path, []byte(stopSequence.Raw))
	}
	return out
}

func extractResponsesUsage(usage gjson.Result) (int64, int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0, 0
	}

	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()
	cacheWriteTokens := usage.Get("input_tokens_details.cache_write_tokens").Int()
	if cacheWriteTokens <= 0 {
		cacheWriteTokens = usage.Get("input_tokens_details.cache_creation_tokens").Int()
	}

	// Deduct both cache-read and cache-write tokens from input_tokens so the
	// Anthropic Messages format does not double-count them downstream
	// (upstream 883660fb). Guard against int64 overflow and clamp negative
	// input_tokens to 0.
	deductTokens := int64(0)
	if cachedTokens > 0 {
		deductTokens += cachedTokens
	}
	if cacheWriteTokens > 0 {
		if math.MaxInt64-deductTokens < cacheWriteTokens {
			deductTokens = math.MaxInt64
		} else {
			deductTokens += cacheWriteTokens
		}
	}

	if deductTokens > 0 {
		if inputTokens >= deductTokens {
			inputTokens -= deductTokens
		} else {
			inputTokens = 0
		}
	}
	if inputTokens < 0 {
		inputTokens = 0
	}

	return inputTokens, outputTokens, cachedTokens, cacheWriteTokens
}

// setClaudeReasoningUsage maps Codex reasoning tokens to Claude thinking tokens.
// Adapted from upstream e365ab0cc988: valid non-negative reasoning_tokens are
// clamped to output_tokens and reported as usage.output_tokens_details.thinking_tokens.
func setClaudeReasoningUsage(out []byte, usage gjson.Result) []byte {
	detail := usage.Get("output_tokens_details.reasoning_tokens")
	if !detail.Exists() || detail.Type != gjson.Number {
		return out
	}
	if strings.HasPrefix(strings.TrimSpace(detail.Raw), "-") || detail.Num < 0 {
		return out
	}
	outputTokens := usage.Get("output_tokens").Int()
	if outputTokens < 0 {
		outputTokens = 0
	}
	var tokens int64
	if detail.Num >= float64(outputTokens) {
		tokens = outputTokens
	} else {
		tokens = detail.Int()
	}
	updated, errSetBytes := sjson.SetBytes(out, "usage.output_tokens_details.thinking_tokens", tokens)
	if errSetBytes != nil {
		return out
	}
	return updated
}

// buildReverseMapFromClaudeOriginalShortToOriginal builds a map[short]original from original Claude request tools.
func buildReverseMapFromClaudeOriginalShortToOriginal(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
	}
	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		n := arr[i].Get("name").String()
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

func ClaudeTokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}

func codexTextBlockKey(root gjson.Result) string {
	if itemID := root.Get("item_id").String(); itemID != "" {
		return "item_id:" + itemID
	}
	if itemID := root.Get("item.id").String(); itemID != "" {
		return "item_id:" + itemID
	}
	if outputIndex := root.Get("output_index"); outputIndex.Exists() {
		return "output_index:" + outputIndex.String()
	}
	return "default"
}

func codexSelectedBlockIndex(root gjson.Result, fallback int) int {
	if outputIndex := root.Get("output_index"); outputIndex.Exists() {
		return int(outputIndex.Int())
	}
	return fallback
}

func codexThinkingBlockIndex(params *ConvertCodexResponseToClaudeParams, root gjson.Result) int {
	if !params.ThinkingBlockIndexSet {
		params.ThinkingBlockIndex = codexSelectedBlockIndex(root, params.BlockIndex)
		params.ThinkingBlockIndexSet = true
	}
	return params.ThinkingBlockIndex
}

func codexThinkingItemKey(root gjson.Result) string {
	if itemID := root.Get("item_id").String(); itemID != "" {
		return "item_id:" + itemID
	}
	if itemID := root.Get("item.id").String(); itemID != "" {
		return "item_id:" + itemID
	}
	if outputIndex := root.Get("output_index"); outputIndex.Exists() {
		return "output_index:" + outputIndex.String()
	}
	return "default"
}

func selectCodexThinkingItem(params *ConvertCodexResponseToClaudeParams, root gjson.Result) []byte {
	if params == nil {
		return nil
	}
	key := codexThinkingItemKey(root)
	if params.ActiveThinkingItemKey == "" {
		params.ActiveThinkingItemKey = key
		_ = codexThinkingBlockIndex(params, root)
		return nil
	}
	if params.ActiveThinkingItemKey == key || key == "default" {
		return nil
	}

	output := finalizeCodexThinkingBlock(params)
	if !params.ThinkingSummarySeen && params.ThinkingSignature != "" {
		output = append(output, finalizeCodexSignatureOnlyThinkingBlock(params, root)...)
	}
	markActiveCodexThinkingItemFinished(params)
	resetCodexThinkingItem(params)
	params.ActiveThinkingItemKey = key
	_ = codexThinkingBlockIndex(params, root)
	return output
}

func codexThinkingItemIsActive(params *ConvertCodexResponseToClaudeParams, root gjson.Result) bool {
	if codexThinkingItemIsFinished(params, root) {
		return false
	}
	if params == nil || params.ActiveThinkingItemKey == "" {
		return true
	}
	key := codexThinkingItemKey(root)
	return key == "default" || key == params.ActiveThinkingItemKey
}

func codexThinkingItemIsFinished(params *ConvertCodexResponseToClaudeParams, root gjson.Result) bool {
	if params == nil || params.FinishedThinkingItems == nil {
		return false
	}
	key := codexThinkingItemKey(root)
	if key == "default" {
		return false
	}
	_, finished := params.FinishedThinkingItems[key]
	return finished
}

func markActiveCodexThinkingItemFinished(params *ConvertCodexResponseToClaudeParams) {
	if params == nil || params.ActiveThinkingItemKey == "" || params.ActiveThinkingItemKey == "default" {
		return
	}
	if params.FinishedThinkingItems == nil {
		params.FinishedThinkingItems = make(map[string]struct{})
	}
	params.FinishedThinkingItems[params.ActiveThinkingItemKey] = struct{}{}
}

func resetCodexThinkingItem(params *ConvertCodexResponseToClaudeParams) {
	params.ThinkingBlockOpen = false
	params.ThinkingBlockIndexSet = false
	params.ThinkingStopPending = false
	params.ThinkingSignature = ""
	params.ThinkingSummarySeen = false
	params.ActiveThinkingItemKey = ""
}

func codexFunctionBlockKey(root gjson.Result) string {
	if outputIndex := root.Get("output_index"); outputIndex.Exists() {
		return "output_index:" + outputIndex.String()
	}
	if itemID := root.Get("item_id").String(); itemID != "" {
		return "item_id:" + itemID
	}
	if itemID := root.Get("item.id").String(); itemID != "" {
		return "item_id:" + itemID
	}
	if callID := root.Get("item.call_id").String(); callID != "" {
		return "call_id:" + callID
	}
	return "default"
}

func codexFunctionBlock(params *ConvertCodexResponseToClaudeParams, root gjson.Result) *codexFunctionBlockState {
	if params == nil || params.FunctionBlocks == nil {
		return nil
	}
	return params.FunctionBlocks[codexFunctionBlockKey(root)]
}

func ensureCodexFunctionBlock(params *ConvertCodexResponseToClaudeParams, root gjson.Result) (*codexFunctionBlockState, string) {
	key := codexFunctionBlockKey(root)
	if params.FunctionBlocks == nil {
		params.FunctionBlocks = make(map[string]*codexFunctionBlockState)
	}
	if functionBlock := params.FunctionBlocks[key]; functionBlock != nil {
		return functionBlock, key
	}

	functionBlock := &codexFunctionBlockState{Index: codexSelectedBlockIndex(root, params.BlockIndex)}
	params.FunctionBlocks[key] = functionBlock
	return functionBlock, key
}

func updateCodexFunctionBlockIdentity(functionBlock *codexFunctionBlockState, item gjson.Result) {
	if functionBlock == nil {
		return
	}
	if callID := item.Get("call_id").String(); callID != "" {
		functionBlock.CallID = callID
	}
	if name := item.Get("name").String(); name != "" {
		functionBlock.Name = name
	}
}

func enqueueCodexFunctionBlock(params *ConvertCodexResponseToClaudeParams, key string) {
	if params == nil || key == "" {
		return
	}
	for _, queued := range params.FunctionBlockQueue {
		if queued == key {
			return
		}
	}
	params.FunctionBlockQueue = append(params.FunctionBlockQueue, key)
}

func appendCodexFunctionBlockArguments(functionBlock *codexFunctionBlockState, delta string) {
	if functionBlock == nil || delta == "" || functionBlock.Closed {
		return
	}
	functionBlock.BufferedArgumentDeltas = append(functionBlock.BufferedArgumentDeltas, delta)
}

func appendCodexFunctionBlockQueue(output []byte, params *ConvertCodexResponseToClaudeParams, originalRequestRawJSON []byte) []byte {
	if params == nil {
		return output
	}
	for {
		if params.ActiveFunctionBlockKey != "" {
			active := params.FunctionBlocks[params.ActiveFunctionBlockKey]
			output = appendCodexFunctionBlockBufferedArguments(output, active)
			if active == nil || !active.Done {
				return output
			}
			output = append(output, stopCodexFunctionBlock(params, params.ActiveFunctionBlockKey)...)
			continue
		}

		for len(params.FunctionBlockQueue) > 0 {
			queued := params.FunctionBlockQueue[0]
			functionBlock := params.FunctionBlocks[queued]
			if functionBlock != nil && !functionBlock.Closed {
				break
			}
			params.FunctionBlockQueue = params.FunctionBlockQueue[1:]
		}
		if len(params.FunctionBlockQueue) == 0 {
			return output
		}

		key := params.FunctionBlockQueue[0]
		functionBlock := params.FunctionBlocks[key]
		if functionBlock == nil || functionBlock.Name == "" {
			return output
		}
		if functionBlock.Index < 0 {
			functionBlock.Index = params.BlockIndex
		}
		functionBlock.Open = true
		functionBlock.Started = true
		params.ActiveFunctionBlockKey = key
		params.HasToolCall = true
		output = appendCodexFunctionBlockStart(output, originalRequestRawJSON, functionBlock)
		output = appendCodexFunctionBlockArgumentDelta(output, functionBlock.Index, "")
		output = appendCodexFunctionBlockBufferedArguments(output, functionBlock)
	}
}

func appendCodexFunctionBlockStart(output []byte, originalRequestRawJSON []byte, functionBlock *codexFunctionBlockState) []byte {
	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`)
	template, _ = sjson.SetBytes(template, "index", functionBlock.Index)
	template, _ = sjson.SetBytes(template, "content_block.id", shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(functionBlock.CallID)))
	name := functionBlock.Name
	rev := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)
	if orig, ok := rev[name]; ok {
		name = orig
	}
	template, _ = sjson.SetBytes(template, "content_block.name", name)
	return translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)
}

func appendCodexFunctionBlockBufferedArguments(output []byte, functionBlock *codexFunctionBlockState) []byte {
	if functionBlock == nil || !functionBlock.Started || functionBlock.Closed {
		return output
	}
	for _, delta := range functionBlock.BufferedArgumentDeltas {
		output = appendCodexFunctionBlockArgumentDelta(output, functionBlock.Index, delta)
	}
	functionBlock.BufferedArgumentDeltas = nil
	return output
}

func appendCodexFunctionBlockArgumentDelta(output []byte, index int, delta string) []byte {
	template := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
	template, _ = sjson.SetBytes(template, "index", index)
	template, _ = sjson.SetBytes(template, "delta.partial_json", delta)
	return translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
}

func stopActiveCodexFunctionBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if params == nil || params.ActiveFunctionBlockKey == "" {
		return nil
	}
	return stopCodexFunctionBlock(params, params.ActiveFunctionBlockKey)
}

func stopCodexFunctionBlock(params *ConvertCodexResponseToClaudeParams, key string) []byte {
	if params == nil || params.FunctionBlocks == nil {
		return nil
	}
	functionBlock := params.FunctionBlocks[key]
	if functionBlock == nil || !functionBlock.Open || functionBlock.Closed {
		return nil
	}

	output := appendCodexFunctionBlockBufferedArguments(nil, functionBlock)
	template := []byte(`{"type":"content_block_stop","index":0}`)
	template, _ = sjson.SetBytes(template, "index", functionBlock.Index)
	output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", template, 2)

	functionBlock.Open = false
	functionBlock.Closed = true
	if params.ActiveFunctionBlockKey == key {
		params.ActiveFunctionBlockKey = ""
	}
	if len(params.FunctionBlockQueue) > 0 && params.FunctionBlockQueue[0] == key {
		params.FunctionBlockQueue = params.FunctionBlockQueue[1:]
	}
	if params.BlockIndex <= functionBlock.Index {
		params.BlockIndex = functionBlock.Index + 1
	}
	return output
}

func startCodexTextBlock(params *ConvertCodexResponseToClaudeParams, root gjson.Result) []byte {
	if params == nil {
		return nil
	}
	key := codexTextBlockKey(root)

	output := stopActiveCodexFunctionBlock(params)
	if params.ActiveTextBlockKey != "" && params.ActiveTextBlockKey != key {
		output = append(output, stopCodexTextBlock(params, params.ActiveTextBlockKey)...)
	}
	if params.TextBlocks == nil {
		params.TextBlocks = make(map[string]*codexTextBlockState)
	}
	if params.TextBlocks[key] != nil {
		return output
	}

	textBlock := &codexTextBlockState{
		Index: codexSelectedBlockIndex(root, params.BlockIndex),
		Open:  true,
	}
	params.TextBlocks[key] = textBlock
	params.ActiveTextBlockKey = key

	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	template, _ = sjson.SetBytes(template, "index", textBlock.Index)
	return translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)
}

func stopActiveCodexTextBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if params == nil || params.ActiveTextBlockKey == "" {
		return nil
	}
	return stopCodexTextBlock(params, params.ActiveTextBlockKey)
}

func stopCodexTextBlock(params *ConvertCodexResponseToClaudeParams, key string) []byte {
	if params == nil || params.TextBlocks == nil {
		return nil
	}
	textBlock := params.TextBlocks[key]
	if textBlock == nil || !textBlock.Open {
		return nil
	}

	template := []byte(`{"type":"content_block_stop","index":0}`)
	template, _ = sjson.SetBytes(template, "index", textBlock.Index)
	output := translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", template, 2)

	textBlock.Open = false
	if params.ActiveTextBlockKey == key {
		params.ActiveTextBlockKey = ""
	}
	params.BlockIndex++
	return output
}

// extractCodexReasoningItemText collects a Responses reasoning item's text, in
// the order the item exposes it: summary parts first, then content when the
// summary is empty. Muse and routed providers put their reasoning in
// reasoning_text content rather than summary, so both must be read.
func extractCodexReasoningItemText(item gjson.Result) string {
	thinkingBuilder := strings.Builder{}
	if summary := item.Get("summary"); summary.Exists() {
		if summary.IsArray() {
			summary.ForEach(func(_, part gjson.Result) bool {
				if txt := part.Get("text"); txt.Exists() {
					thinkingBuilder.WriteString(txt.String())
				} else {
					thinkingBuilder.WriteString(part.String())
				}
				return true
			})
		} else {
			thinkingBuilder.WriteString(summary.String())
		}
	}
	if thinkingBuilder.Len() == 0 {
		if content := item.Get("content"); content.Exists() {
			if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if txt := part.Get("text"); txt.Exists() {
						thinkingBuilder.WriteString(txt.String())
					} else {
						thinkingBuilder.WriteString(part.String())
					}
					return true
				})
			} else {
				thinkingBuilder.WriteString(content.String())
			}
		}
	}
	return thinkingBuilder.String()
}

func startCodexThinkingBlock(params *ConvertCodexResponseToClaudeParams, root gjson.Result) []byte {
	output := stopActiveCodexFunctionBlock(params)
	if params.ThinkingBlockOpen {
		return output
	}

	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
	template, _ = sjson.SetBytes(template, "index", codexThinkingBlockIndex(params, root))
	params.ThinkingBlockOpen = true
	params.ThinkingStopPending = false

	return translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)
}

func finalizeCodexSignatureOnlyThinkingBlock(params *ConvertCodexResponseToClaudeParams, root gjson.Result) []byte {
	if params.ThinkingSignature == "" {
		return nil
	}

	output := stopActiveCodexTextBlock(params)
	output = append(output, startCodexThinkingBlock(params, root)...)
	output = append(output, finalizeCodexThinkingBlock(params)...)
	return output
}

func finalizeCodexThinkingBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if !params.ThinkingBlockOpen {
		return nil
	}

	output := make([]byte, 0, 256)
	if params.ThinkingSignature != "" {
		signatureDelta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`)
		signatureDelta, _ = sjson.SetBytes(signatureDelta, "index", params.ThinkingBlockIndex)
		signatureDelta, _ = sjson.SetBytes(signatureDelta, "delta.signature", params.ThinkingSignature)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", signatureDelta, 2)
	}

	contentBlockStop := []byte(`{"type":"content_block_stop","index":0}`)
	contentBlockStop, _ = sjson.SetBytes(contentBlockStop, "index", params.ThinkingBlockIndex)
	output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", contentBlockStop, 2)

	params.BlockIndex++
	params.ThinkingBlockOpen = false
	params.ThinkingBlockIndexSet = false
	params.ThinkingStopPending = false

	return output
}
