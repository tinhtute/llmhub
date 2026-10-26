// Package claude provides response translation functionality for Claude API.
// This package handles the conversion of backend client responses into Claude-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Params holds parameters for response conversion.
type Params struct {
	IsGlAPIKey                 bool
	HasFirstResponse           bool
	ResponseType               int
	ResponseIndex              int
	HasContent                 bool // Tracks whether any content (text, thinking, or tool use) has been output
	ToolNameMap                map[string]string
	SanitizedNameMap           map[string]string
	SawToolCall                bool
	CurrentToolName            string // Client-facing name of the currently open tool_use block
	CurrentToolIsParameterless bool   // Whether the open tool_use block was declared parameterless
	CurrentToolInputEmitted    bool   // Whether cleaned input JSON was already emitted for the open tool_use block
	FinishReason               string
	HasFinalEvents             bool
	InputTokens                int64
	OutputTokens               int64
	CachedTokens               int64
}

const geminiMalformedFunctionCallErrorPayload = `{"type":"error","error":{"type":"api_error","message":"Gemini response finished with MALFORMED_FUNCTION_CALL"}}`

func isGeminiMalformedFunctionCall(finishReason string, sawToolCall bool) bool {
	return finishReason == "MALFORMED_FUNCTION_CALL" && !sawToolCall
}

func resolveGeminiClaudeStopReason(finishReason string, sawToolCall bool) string {
	if sawToolCall {
		return "tool_use"
	}
	switch finishReason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "SPII", "BLOCKLIST", "IMAGE_SAFETY":
		return "refusal"
	case "STOP", "FINISH_REASON_UNSPECIFIED", "UNKNOWN", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// toolUseIDCounter provides a process-wide unique counter for tool use identifiers.
var toolUseIDCounter uint64

// ConvertGeminiResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates backend client responses
// into Claude-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the Gemini API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - [][]byte: A slice of bytes, each containing a Claude-compatible SSE payload.
func ConvertGeminiResponseToClaude(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &Params{
			IsGlAPIKey:       false,
			HasFirstResponse: false,
			ResponseType:     0,
			ResponseIndex:    0,
			ToolNameMap:      util.UnambiguousToolNameMapFromClaudeRequest(originalRequestRawJSON),
			SanitizedNameMap: util.SanitizedToolNameMap(originalRequestRawJSON),
			SawToolCall:      false,
		}
	}

	p := (*param).(*Params)

	output := make([]byte, 0, 1024)
	appendEvent := func(event, payload string) {
		output = translatorcommon.AppendSSEEventString(output, event, payload, 3)
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		if isGeminiMalformedFunctionCall(p.FinishReason, p.SawToolCall) {
			if p.ResponseType != 0 {
				appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, p.ResponseIndex))
				p.ResponseType = 0
			}
			if !p.HasFinalEvents {
				appendEvent("error", geminiMalformedFunctionCallErrorPayload)
				p.HasFinalEvents = true
			}
			if len(output) > 0 {
				return [][]byte{output}
			}
			return [][]byte{}
		}
		// Only send message_stop if we have actually output content
		if p.HasContent {
			return [][]byte{translatorcommon.AppendSSEEventString(nil, "message_stop", `{"type":"message_stop"}`, 3)}
		}
		return [][]byte{}
	}

	if isGeminiMalformedFunctionCall(p.FinishReason, p.SawToolCall) && p.HasFinalEvents {
		return [][]byte{}
	}

	// Initialize the streaming session with a message_start event
	// This is only sent for the very first response chunk
	if !(*param).(*Params).HasFirstResponse {
		// Create the initial message structure with default values
		// This follows the Claude API specification for streaming message initialization
		messageStartTemplate := []byte(`{"type":"message_start","message":{"id":"msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet-20241022","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)

		// Override default values with actual response metadata if available
		if modelVersionResult := gjson.GetBytes(rawJSON, "modelVersion"); modelVersionResult.Exists() {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.model", modelVersionResult.String())
		}
		if responseIDResult := gjson.GetBytes(rawJSON, "responseId"); responseIDResult.Exists() {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.id", responseIDResult.String())
		}
		appendEvent("message_start", string(messageStartTemplate))

		(*param).(*Params).HasFirstResponse = true
	}

	// Process the response parts array from the backend client
	// Each part can contain text content, thinking content, or function calls
	partsResult := gjson.GetBytes(rawJSON, "candidates.0.content.parts")
	if partsResult.IsArray() {
		partResults := partsResult.Array()
		for i := 0; i < len(partResults); i++ {
			partResult := partResults[i]

			// Extract the different types of content from each part
			partTextResult := partResult.Get("text")
			functionCallResult := partResult.Get("functionCall")

			// Handle text content (both regular content and thinking)
			if partTextResult.Exists() {
				// Process thinking content (internal reasoning)
				if partResult.Get("thought").Bool() {
					// Continue existing thinking block
					if (*param).(*Params).ResponseType == 2 {
						data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, (*param).(*Params).ResponseIndex)), "delta.thinking", partTextResult.String())
						appendEvent("content_block_delta", string(data))
						(*param).(*Params).HasContent = true
					} else {
						// Transition from another state to thinking
						// First, close any existing content block
						if (*param).(*Params).ResponseType != 0 {
							if (*param).(*Params).ResponseType == 2 {
								// output = output + "event: content_block_delta\n"
								// output = output + fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":null}}`, (*param).(*Params).ResponseIndex)
								// output = output + "\n\n\n"
							}
							appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, (*param).(*Params).ResponseIndex))
							(*param).(*Params).ResponseIndex++
						}

						// Start a new thinking content block
						appendEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, (*param).(*Params).ResponseIndex))
						data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, (*param).(*Params).ResponseIndex)), "delta.thinking", partTextResult.String())
						appendEvent("content_block_delta", string(data))
						(*param).(*Params).ResponseType = 2 // Set state to thinking
						(*param).(*Params).HasContent = true
					}
				} else {
					// Process regular text content (user-visible output)
					// Continue existing text block
					if (*param).(*Params).ResponseType == 1 {
						data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, (*param).(*Params).ResponseIndex)), "delta.text", partTextResult.String())
						appendEvent("content_block_delta", string(data))
						(*param).(*Params).HasContent = true
					} else {
						// Transition from another state to text content
						// First, close any existing content block
						if (*param).(*Params).ResponseType != 0 {
							if (*param).(*Params).ResponseType == 2 {
								// output = output + "event: content_block_delta\n"
								// output = output + fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":null}}`, (*param).(*Params).ResponseIndex)
								// output = output + "\n\n\n"
							}
							appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, (*param).(*Params).ResponseIndex))
							(*param).(*Params).ResponseIndex++
						}

						// Start a new text content block
						appendEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, (*param).(*Params).ResponseIndex))
						data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, (*param).(*Params).ResponseIndex)), "delta.text", partTextResult.String())
						appendEvent("content_block_delta", string(data))
						(*param).(*Params).ResponseType = 1 // Set state to content
						(*param).(*Params).HasContent = true
					}
				}
			} else if functionCallResult.Exists() {
				// Handle function/tool calls from the AI model
				// This processes tool usage requests and formats them for Claude API compatibility
				(*param).(*Params).SawToolCall = true
				upstreamToolName := functionCallResult.Get("name").String()
				upstreamToolName = util.RestoreSanitizedToolName((*param).(*Params).SanitizedNameMap, upstreamToolName)
				clientToolName := util.MapToolName((*param).(*Params).ToolNameMap, upstreamToolName)

				// FIX: Handle streaming split/delta where name might be empty in subsequent chunks.
				// If we are already in tool use mode and name is empty, treat as continuation (delta).
				if (*param).(*Params).ResponseType == 3 && upstreamToolName == "" {
					if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
						partialJSON := util.CleanClaudeToolArgumentsWithTranslated(fcArgsResult.Raw, (*param).(*Params).CurrentToolName, originalRequestRawJSON, requestRawJSON)
						if (*param).(*Params).CurrentToolIsParameterless {
							if (*param).(*Params).CurrentToolInputEmitted {
								partialJSON = ""
							} else {
								(*param).(*Params).CurrentToolInputEmitted = true
							}
						} else if partialJSON != "" {
							(*param).(*Params).CurrentToolInputEmitted = true
						}
						data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, (*param).(*Params).ResponseIndex)), "delta.partial_json", partialJSON)
						appendEvent("content_block_delta", string(data))
					}
					// Continue to next part without closing/opening logic
					continue
				}

				// Handle state transitions when switching to function calls
				// Close any existing function call block first
				if (*param).(*Params).ResponseType == 3 {
					appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, (*param).(*Params).ResponseIndex))
					(*param).(*Params).ResponseIndex++
					(*param).(*Params).ResponseType = 0
				}

				// Special handling for thinking state transition
				if (*param).(*Params).ResponseType == 2 {
					// output = output + "event: content_block_delta\n"
					// output = output + fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":null}}`, (*param).(*Params).ResponseIndex)
					// output = output + "\n\n\n"
				}

				// Close any other existing content block
				if (*param).(*Params).ResponseType != 0 {
					appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, (*param).(*Params).ResponseIndex))
					(*param).(*Params).ResponseIndex++
				}

				(*param).(*Params).CurrentToolName = clientToolName
				(*param).(*Params).CurrentToolIsParameterless = util.ClaudeToolIsParameterless(clientToolName, originalRequestRawJSON)
				(*param).(*Params).CurrentToolInputEmitted = false

				// Start a new tool use content block
				// This creates the structure for a function call in Claude format
				// Create the tool use block with unique ID and function details
				data := []byte(fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`, (*param).(*Params).ResponseIndex))
				data, _ = sjson.SetBytes(data, "content_block.id", util.SanitizeClaudeToolID(fmt.Sprintf("%s-%d", upstreamToolName, atomic.AddUint64(&toolUseIDCounter, 1))))
				data, _ = sjson.SetBytes(data, "content_block.name", clientToolName)
				appendEvent("content_block_start", string(data))

				if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
					partialJSON := util.CleanClaudeToolArgumentsWithTranslated(fcArgsResult.Raw, clientToolName, originalRequestRawJSON, requestRawJSON)
					if (*param).(*Params).CurrentToolIsParameterless {
						if (*param).(*Params).CurrentToolInputEmitted {
							partialJSON = ""
						} else {
							(*param).(*Params).CurrentToolInputEmitted = true
						}
					} else if partialJSON != "" {
						(*param).(*Params).CurrentToolInputEmitted = true
					}
					data, _ = sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, (*param).(*Params).ResponseIndex)), "delta.partial_json", partialJSON)
					appendEvent("content_block_delta", string(data))
				}
				(*param).(*Params).ResponseType = 3
				(*param).(*Params).HasContent = true
			}
		}
	}
	if finish := gjson.GetBytes(rawJSON, "candidates.0.finishReason"); finish.Exists() && finish.String() != "" {
		p.FinishReason = finish.String()
	}

	if isGeminiMalformedFunctionCall(p.FinishReason, p.SawToolCall) && !p.HasFinalEvents {
		if p.ResponseType != 0 {
			appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, p.ResponseIndex))
			p.ResponseType = 0
		}
		appendEvent("error", geminiMalformedFunctionCallErrorPayload)
		p.HasFinalEvents = true
		return [][]byte{output}
	}

	usageResult := gjson.GetBytes(rawJSON, "usageMetadata")
	if usageResult.Exists() && bytes.Contains(rawJSON, []byte(`"finishReason"`)) && !p.HasFinalEvents {
		if candidatesTokenCountResult := usageResult.Get("candidatesTokenCount"); candidatesTokenCountResult.Exists() {
			// Only send final events if we have actually output content
			if p.HasContent {
				if p.ResponseType != 0 {
					appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, p.ResponseIndex))
					p.ResponseType = 0
				}

				stopReason := resolveGeminiClaudeStopReason(p.FinishReason, p.SawToolCall)
				template := []byte(fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`, stopReason))

				thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
				template, _ = sjson.SetBytes(template, "usage.output_tokens", candidatesTokenCountResult.Int()+thoughtsTokenCount)
				cachedTokenCount := usageResult.Get("cachedContentTokenCount").Int()
				promptTokenCount := usageResult.Get("promptTokenCount").Int() - cachedTokenCount
				if promptTokenCount < 0 {
					promptTokenCount = 0
				}
				template, _ = sjson.SetBytes(template, "usage.input_tokens", promptTokenCount)
				if cachedTokenCount > 0 {
					template, _ = sjson.SetBytes(template, "usage.cache_read_input_tokens", cachedTokenCount)
				}

				p.HasFinalEvents = true
				appendEvent("message_delta", string(template))
			}
		}
	}

	return [][]byte{output}
}

// ConvertGeminiResponseToClaudeNonStream converts a non-streaming Gemini response to a non-streaming Claude response.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the Gemini API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - []byte: A Claude-compatible JSON response.
func ConvertGeminiResponseToClaudeNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	_ = requestRawJSON

	root := gjson.ParseBytes(rawJSON)
	toolNameMap := util.UnambiguousToolNameMapFromClaudeRequest(originalRequestRawJSON)
	sanitizedNameMap := util.SanitizedToolNameMap(originalRequestRawJSON)

	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", root.Get("responseId").String())
	out, _ = sjson.SetBytes(out, "model", root.Get("modelVersion").String())

	cachedTokens := root.Get("usageMetadata.cachedContentTokenCount").Int()
	rawPromptTokens := root.Get("usageMetadata.promptTokenCount").Int()
	inputTokens := rawPromptTokens - cachedTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	outputTokens := root.Get("usageMetadata.candidatesTokenCount").Int() + root.Get("usageMetadata.thoughtsTokenCount").Int()
	if outputTokens == 0 && root.Get("usageMetadata.totalTokenCount").Int() > 0 {
		outputTokens = root.Get("usageMetadata.totalTokenCount").Int() - rawPromptTokens
		if outputTokens < 0 {
			outputTokens = 0
		}
	}
	out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
	if cachedTokens > 0 {
		out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cachedTokens)
	}

	parts := root.Get("candidates.0.content.parts")
	textBuilder := strings.Builder{}
	thinkingBuilder := strings.Builder{}
	var thinkingSignature string
	hasToolCall := false

	flushText := func() {
		if textBuilder.Len() == 0 {
			return
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", textBuilder.String())
		out, _ = sjson.SetRawBytes(out, "content.-1", block)
		textBuilder.Reset()
	}

	flushThinking := func() {
		if thinkingBuilder.Len() == 0 && thinkingSignature == "" {
			return
		}
		block := []byte(`{"type":"thinking","thinking":""}`)
		block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
		if thinkingSignature != "" {
			block, _ = sjson.SetBytes(block, "signature", thinkingSignature)
		}
		out, _ = sjson.SetRawBytes(out, "content.-1", block)
		thinkingBuilder.Reset()
		thinkingSignature = ""
	}

	if parts.IsArray() {
		for _, part := range parts.Array() {
			thoughtSignatureResult := part.Get("thoughtSignature")
			if !thoughtSignatureResult.Exists() {
				thoughtSignatureResult = part.Get("thought_signature")
			}
			hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != ""
			if hasThoughtSignature {
				thinkingSignature = thoughtSignatureResult.String()
			}

			text := part.Get("text")
			functionCall := part.Get("functionCall")

			if hasThoughtSignature && (!text.Exists() || text.String() == "") && !functionCall.Exists() {
				continue
			}

			if text.Exists() && text.String() != "" {
				if part.Get("thought").Bool() || hasThoughtSignature {
					flushText()
					thinkingBuilder.WriteString(text.String())
					continue
				}
				flushThinking()
				textBuilder.WriteString(text.String())
				continue
			}

			if functionCall.Exists() {
				flushThinking()
				flushText()
				hasToolCall = true

				upstreamToolName := functionCall.Get("name").String()
				upstreamToolName = util.RestoreSanitizedToolName(sanitizedNameMap, upstreamToolName)
				clientToolName := util.MapToolName(toolNameMap, upstreamToolName)
				toolBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
				toolBlock, _ = sjson.SetBytes(toolBlock, "id", util.SanitizeClaudeToolID(fmt.Sprintf("%s-%d", upstreamToolName, atomic.AddUint64(&toolUseIDCounter, 1))))
				toolBlock, _ = sjson.SetBytes(toolBlock, "name", clientToolName)
				inputRaw := "{}"
				if args := functionCall.Get("args"); args.Exists() && gjson.Valid(args.Raw) && args.IsObject() {
					inputRaw = util.CleanClaudeToolArgumentsWithTranslated(args.Raw, clientToolName, originalRequestRawJSON, requestRawJSON)
				}
				toolBlock, _ = sjson.SetRawBytes(toolBlock, "input", []byte(inputRaw))
				out, _ = sjson.SetRawBytes(out, "content.-1", toolBlock)
				continue
			}
		}
	}

	flushThinking()
	flushText()
	var finishReason string
	if finish := root.Get("candidates.0.finishReason"); finish.Exists() {
		finishReason = finish.String()
	}
	if isGeminiMalformedFunctionCall(finishReason, hasToolCall) {
		return []byte(geminiMalformedFunctionCallErrorPayload)
	}
	out, _ = sjson.SetBytes(out, "stop_reason", resolveGeminiClaudeStopReason(finishReason, hasToolCall))

	if inputTokens == int64(0) && outputTokens == int64(0) && !root.Get("usageMetadata").Exists() {
		out, _ = sjson.DeleteBytes(out, "usage")
	}

	return out
}

func ClaudeTokenCount(ctx context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}
