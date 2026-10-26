// Package openai provides request translation functionality for OpenAI to Claude Code API compatibility.
// It handles parsing and transforming OpenAI Chat Completions API requests into Claude Code API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between OpenAI API format and Claude Code API's expected format.
package chat_completions

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	common "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	defaultClaudeChatMaxTokens = 32000
	defaultFableChatMaxTokens  = 64000
)

// ConvertOpenAIRequestToClaude parses and transforms an OpenAI Chat Completions API request into Claude Code API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Claude Code API.
// The function performs comprehensive transformation including:
// 1. Model name mapping and parameter extraction (max_tokens, temperature, top_p, etc.)
// 2. Message content conversion from OpenAI to Claude Code format
// 3. Tool call and tool result handling with proper ID mapping
// 4. Image data conversion from OpenAI data URLs to Claude Code base64 format
// 5. Stop sequence and streaming configuration handling
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in Claude Code API format
func ConvertOpenAIRequestToClaude(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertOpenAIRequestToClaude(modelName, inputRawJSON, stream, false)
}

// ConvertOpenAIRequestToClaudeWithCompat preserves assistant reasoning content
// as an unsigned thinking block for configured compatibility endpoints.
// Ported from upstream CLIProxyAPI (claude_openai_request.go).
func ConvertOpenAIRequestToClaudeWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertOpenAIRequestToClaude(modelName, inputRawJSON, stream, true)
}

func convertOpenAIRequestToClaude(modelName string, inputRawJSON []byte, stream, preserveEmptyThinkingBlocks bool) []byte {
	rawJSON := inputRawJSON

	userID := common.DeriveClaudeUserID(rawJSON)

	// Base Claude Code API template with default max_tokens value
	out := []byte(`{"model":"","max_tokens":32000,"messages":[],"metadata":{}}`)
	out, _ = sjson.SetBytes(out, "metadata.user_id", userID)
	out, _ = sjson.SetBytes(out, "max_tokens", defaultClaudeChatMaxTokensForModel(modelName))

	root := gjson.ParseBytes(rawJSON)

	// Convert OpenAI reasoning_effort to Claude thinking config.
	if v := root.Get("reasoning_effort"); v.Exists() {
		effort := strings.ToLower(strings.TrimSpace(v.String()))
		if effort != "" {
			mi := registry.LookupModelInfo(modelName, "claude")
			supportsAdaptive := mi != nil && mi.Thinking != nil && len(mi.Thinking.Levels) > 0
			supportsMax := supportsAdaptive && thinking.HasLevel(mi.Thinking.Levels, string(thinking.LevelMax))

			// Claude 4.6 supports adaptive thinking with output_config.effort.
			// MapToClaudeEffort normalizes levels (e.g. minimal→low, xhigh→high) to avoid
			// validation errors since validate treats same-provider unsupported levels as errors.
			if supportsAdaptive {
				switch effort {
				case "none":
					out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.DeleteBytes(out, "output_config.effort")
				case "auto":
					out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.DeleteBytes(out, "output_config.effort")
				default:
					if !thinking.HasLevel(mi.Thinking.Levels, effort) {
						if mapped, ok := thinking.MapToClaudeEffort(effort, supportsMax); ok {
							effort = mapped
						}
					}
					out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.SetBytes(out, "output_config.effort", effort)
				}
			} else {
				// Legacy/manual thinking (budget_tokens).
				budget, ok := thinking.ConvertLevelToBudget(effort)
				if ok {
					switch budget {
					case 0:
						out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
					case -1:
						out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
					default:
						if budget > 0 {
							out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
							out, _ = sjson.SetBytes(out, "thinking.budget_tokens", budget)
						}
					}
				}
			}
		}
	}

	// Helper for generating tool call IDs in the form: toolu_<alphanum>
	// This ensures unique identifiers for tool calls in the Claude Code format
	genToolCallID := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		var b strings.Builder
		// 24 chars random suffix for uniqueness
		for i := 0; i < 24; i++ {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			b.WriteByte(letters[n.Int64()])
		}
		return "toolu_" + b.String()
	}

	// Model mapping to specify which Claude Code model to use
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Max tokens configuration with fallback to default value.
	// OpenAI Chat Completions deprecated max_tokens in favor of
	// max_completion_tokens, so accept either spelling.
	if maxTokens := firstExisting(root.Get("max_tokens"), root.Get("max_completion_tokens")); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	// Temperature setting for controlling response randomness
	if temp := root.Get("temperature"); temp.Exists() {
		out, _ = sjson.SetBytes(out, "temperature", temp.Float())
	} else if topP := root.Get("top_p"); topP.Exists() {
		// Top P setting for nucleus sampling (filtered out if temperature is set)
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}

	// Stop sequences configuration for custom termination conditions
	if stop := root.Get("stop"); stop.Exists() {
		if stop.IsArray() {
			var stopSequences []string
			stop.ForEach(func(_, value gjson.Result) bool {
				stopSequences = append(stopSequences, value.String())
				return true
			})
			if len(stopSequences) > 0 {
				out, _ = sjson.SetBytes(out, "stop_sequences", stopSequences)
			}
		} else {
			out, _ = sjson.SetBytes(out, "stop_sequences", []string{stop.String()})
		}
	}

	// Stream configuration to enable or disable streaming responses
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Process messages and transform them to Claude Code format
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		messageIndex := 0
		previousRole := ""
		messages.ForEach(func(_, message gjson.Result) bool {
			role := message.Get("role").String()
			contentResult := message.Get("content")

			switch role {
			case "system":
				if contentResult.Exists() && contentResult.Type == gjson.String && strings.TrimSpace(contentResult.String()) != "" {
					textPart := []byte(`{"type":"text","text":""}`)
					textPart, _ = sjson.SetBytes(textPart, "text", contentResult.String())
					out, _ = sjson.SetRawBytes(out, "system.-1", textPart)
				} else if contentResult.Exists() && contentResult.IsArray() {
					contentResult.ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "text" {
							text := part.Get("text").String()
							if strings.TrimSpace(text) == "" {
								return true
							}
							textPart := []byte(`{"type":"text","text":""}`)
							textPart, _ = sjson.SetBytes(textPart, "text", text)
							out, _ = sjson.SetRawBytes(out, "system.-1", textPart)
						}
						return true
					})
				}
			case "user", "assistant":
				msg := []byte(`{"role":"","content":[]}`)
				msg, _ = sjson.SetBytes(msg, "role", role)

				if preserveEmptyThinkingBlocks && role == "assistant" {
					if reasoningContent := message.Get("reasoning_content"); reasoningContent.Type == gjson.String && strings.TrimSpace(reasoningContent.String()) != "" {
						part := []byte(`{"type":"thinking","thinking":"","signature":""}`)
						part, _ = sjson.SetBytes(part, "thinking", reasoningContent.String())
						msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
					}
				}

				// Handle content based on its type (string or array)
				if contentResult.Exists() && contentResult.Type == gjson.String && strings.TrimSpace(contentResult.String()) != "" {
					part := []byte(`{"type":"text","text":""}`)
					part, _ = sjson.SetBytes(part, "text", contentResult.String())
					msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
				} else if contentResult.Exists() && contentResult.IsArray() {
					contentResult.ForEach(func(_, part gjson.Result) bool {
						claudePart := convertOpenAIContentPartToClaudePart(part)
						if claudePart != "" {
							msg, _ = sjson.SetRawBytes(msg, "content.-1", []byte(claudePart))
						}
						return true
					})
				}

				// Handle tool calls (for assistant messages)
				if toolCalls := message.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() && role == "assistant" {
					toolCalls.ForEach(func(_, toolCall gjson.Result) bool {
						if toolCall.Get("type").String() == "function" {
							toolCallID := toolCall.Get("id").String()
							if toolCallID == "" {
								toolCallID = genToolCallID()
							}
							toolCallID = util.SanitizeClaudeToolID(toolCallID)

							function := toolCall.Get("function")
							toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
							toolUse, _ = sjson.SetBytes(toolUse, "id", toolCallID)
							toolUse, _ = sjson.SetBytes(toolUse, "name", util.SanitizeClaudeFunctionName(function.Get("name").String()))

							// Parse arguments for the tool call
							if args := function.Get("arguments"); args.Exists() {
								argsStr := args.String()
								if argsStr != "" && gjson.Valid(argsStr) {
									argsJSON := gjson.Parse(argsStr)
									if argsJSON.IsObject() {
										toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(argsJSON.Raw))
									} else {
										toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte("{}"))
									}
								} else {
									toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte("{}"))
								}
							} else {
								toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte("{}"))
							}

							msg, _ = sjson.SetRawBytes(msg, "content.-1", toolUse)
						}
						return true
					})
				}

				out, _ = sjson.SetRawBytes(out, "messages.-1", msg)
				messageIndex++

			case "tool":
				// Handle tool result messages conversion
				toolCallID := util.SanitizeClaudeToolID(message.Get("tool_call_id").String())
				toolContentResult := message.Get("content")

				msg := []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"","content":""}]}`)
				msg, _ = sjson.SetBytes(msg, "content.0.tool_use_id", toolCallID)
				toolResultContent, toolResultContentRaw := convertOpenAIToolResultContent(toolContentResult)
				if toolResultContentRaw {
					msg, _ = sjson.SetRawBytes(msg, "content.0.content", []byte(toolResultContent))
				} else {
					msg, _ = sjson.SetBytes(msg, "content.0.content", toolResultContent)
				}
				if previousRole == "tool" && messageIndex > 0 {
					toolResult := gjson.GetBytes(msg, "content.0")
					out, _ = sjson.SetRawBytes(out, fmt.Sprintf("messages.%d.content.-1", messageIndex-1), []byte(toolResult.Raw))
				} else {
					out, _ = sjson.SetRawBytes(out, "messages.-1", msg)
					messageIndex++
				}
			}
			previousRole = role
			return true
		})

		// Preserve a minimal conversational turn for system-only inputs.
		// Claude payloads with top-level system instructions but no messages are risky for downstream validation.
		if messageIndex == 0 {
			system := gjson.GetBytes(out, "system")
			if system.Exists() && system.IsArray() && len(system.Array()) > 0 {
				fallbackMsg := []byte(`{"role":"user","content":[{"type":"text","text":""}]}`)
				out, _ = sjson.SetRawBytes(out, "messages.-1", fallbackMsg)
			}
		}
	}

	// Tools mapping: OpenAI tools -> Claude Code tools
	// tool_choice "allowed_tools" subset filtering (upstream 49eec664).
	allowedToolNames := make(map[string]struct{})
	isAllowedTools := false
	allowedMode := "auto"
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() && toolChoice.IsObject() && toolChoice.Get("type").String() == "allowed_tools" {
		isAllowedTools = true
		toolList := toolChoice.Get("allowed_tools.tools").Array()
		if len(toolList) == 0 {
			toolList = toolChoice.Get("tools").Array()
		}
		for _, t := range toolList {
			fnName := strings.TrimSpace(t.Get("function.name").String())
			if fnName == "" {
				fnName = strings.TrimSpace(t.Get("name").String())
			}
			if fnName != "" {
				allowedToolNames[fnName] = struct{}{}
				allowedToolNames[util.SanitizeClaudeFunctionName(fnName)] = struct{}{}
			}
		}
		modeVal := strings.ToLower(strings.TrimSpace(toolChoice.Get("allowed_tools.mode").String()))
		if modeVal == "" {
			modeVal = strings.ToLower(strings.TrimSpace(toolChoice.Get("mode").String()))
		}
		if modeVal != "" {
			allowedMode = modeVal
		}
	}

	var anthropicTools [][]byte
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "function" {
				function := tool.Get("function")
				fnName := function.Get("name").String()
				sanitizedFnName := util.SanitizeClaudeFunctionName(fnName)
				if isAllowedTools {
					if _, ok := allowedToolNames[fnName]; !ok {
						if _, okSanitized := allowedToolNames[sanitizedFnName]; !okSanitized {
							return true
						}
					}
				}
				anthropicTool := []byte(`{"name":"","description":""}`)
				anthropicTool, _ = sjson.SetBytes(anthropicTool, "name", sanitizedFnName)
				anthropicTool, _ = sjson.SetBytes(anthropicTool, "description", function.Get("description").String())

				// Convert parameters schema for the tool
				if parameters := function.Get("parameters"); parameters.Exists() {
					anthropicTool, _ = sjson.SetRawBytes(anthropicTool, "input_schema", util.NormalizeClaudeToolInputSchema([]byte(parameters.Raw)))
				} else if parameters := function.Get("parametersJsonSchema"); parameters.Exists() {
					anthropicTool, _ = sjson.SetRawBytes(anthropicTool, "input_schema", util.NormalizeClaudeToolInputSchema([]byte(parameters.Raw)))
				} else {
					anthropicTool, _ = sjson.SetRawBytes(anthropicTool, "input_schema", util.NormalizeClaudeToolInputSchema(nil))
				}

				// Preserve the OpenAI strict tool flag on the Claude tool
				// (upstream f247e2b0).
				strict := function.Get("strict")
				if !strict.Exists() {
					strict = tool.Get("strict")
				}
				if strict.Exists() {
					if strict.Type == gjson.True {
						anthropicTool, _ = sjson.SetBytes(anthropicTool, "strict", true)
					} else if strict.Type == gjson.False {
						anthropicTool, _ = sjson.SetBytes(anthropicTool, "strict", false)
					}
				}

				anthropicTools = append(anthropicTools, anthropicTool)
			}
			return true
		})

		if len(anthropicTools) > 0 {
			out, _ = sjson.SetRawBytes(out, "tools", common.JoinRawArray(anthropicTools))
		} else {
			out, _ = sjson.DeleteBytes(out, "tools")
		}
	}

	// Tool choice mapping from OpenAI format to Claude Code format
	// (fail-closed semantics ported from upstream 49eec664).
	if isAllowedTools {
		if len(anthropicTools) == 0 {
			out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
		} else if allowedMode == "required" {
			out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
		} else {
			out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
		}
	} else if toolChoice := root.Get("tool_choice"); toolChoice.Exists() && toolChoice.Type != gjson.Null {
		switch toolChoice.Type {
		case gjson.String:
			choice := toolChoice.String()
			switch choice {
			case "none":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
			case "auto":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
			case "required":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
			}
		case gjson.JSON:
			choiceType := toolChoice.Get("type").String()
			switch choiceType {
			case "none":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
			case "auto":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
			case "required", "any":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
			case "function":
				functionName := toolChoice.Get("function.name").String()
				if functionName == "" {
					functionName = toolChoice.Get("name").String()
				}
				if functionName != "" {
					toolChoiceJSON := []byte(`{"type":"tool","name":""}`)
					toolChoiceJSON, _ = sjson.SetBytes(toolChoiceJSON, "name", util.SanitizeClaudeFunctionName(functionName))
					out, _ = sjson.SetRawBytes(out, "tool_choice", toolChoiceJSON)
				} else {
					out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
				}
			}
		default:
		}
	}

	if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Type == gjson.False {
		if gjson.GetBytes(out, "tool_choice").Exists() {
			if gjson.GetBytes(out, "tool_choice.type").String() != "none" {
				out, _ = sjson.SetBytes(out, "tool_choice.disable_parallel_tool_use", true)
			}
		} else if gjson.GetBytes(out, "tools").Exists() {
			out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto","disable_parallel_tool_use":true}`))
		}
	}

	return thinking.ApplyTranslatedSummaryToClaude(out, inputRawJSON, "openai", modelName)
}

// defaultClaudeChatMaxTokensForModel determines the default max_tokens for a
// Claude model. Adapted from upstream 0d18d3b365be: registered models use
// their output limit, Fable keeps its conservative ceiling, and unknown models
// fall back to the historical 32000 default.
func defaultClaudeChatMaxTokensForModel(modelName string) int {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	maxTokens := defaultClaudeChatMaxTokens
	if strings.Contains(normalized, "fable") {
		maxTokens = defaultFableChatMaxTokens
	}
	info := registry.LookupModelInfo(modelName, "claude")
	if info == nil || info.MaxCompletionTokens <= 0 {
		return maxTokens
	}
	if strings.Contains(normalized, "fable") && info.MaxCompletionTokens >= maxTokens {
		return maxTokens
	}
	return info.MaxCompletionTokens
}

// firstExisting returns the first non-null result that exists, or an empty result.
func firstExisting(values ...gjson.Result) gjson.Result {
	for _, value := range values {
		if value.Exists() && value.Type != gjson.Null {
			return value
		}
	}
	return gjson.Result{}
}

func convertOpenAIContentPartToClaudePart(part gjson.Result) string {
	switch part.Get("type").String() {
	case "text":
		// Anthropic rejects cache_control on an empty text block, and an empty
		// text block carries nothing: drop it at the translator.
		text := part.Get("text").String()
		if strings.TrimSpace(text) == "" {
			return ""
		}
		textPart := []byte(`{"type":"text","text":""}`)
		textPart, _ = sjson.SetBytes(textPart, "text", text)
		return string(textPart)

	case "image_url":
		return convertOpenAIImageURLToClaudePart(part.Get("image_url.url").String())

	case "file":
		fileData := part.Get("file.file_data").String()
		if strings.HasPrefix(fileData, "data:") {
			semicolonIdx := strings.Index(fileData, ";")
			commaIdx := strings.Index(fileData, ",")
			if semicolonIdx != -1 && commaIdx != -1 && commaIdx > semicolonIdx {
				mediaType := strings.TrimPrefix(fileData[:semicolonIdx], "data:")
				data := fileData[commaIdx+1:]
				docPart := []byte(`{"type":"document","source":{"type":"base64","media_type":"","data":""}}`)
				docPart, _ = sjson.SetBytes(docPart, "source.media_type", mediaType)
				docPart, _ = sjson.SetBytes(docPart, "source.data", data)
				return string(docPart)
			}
		}
	}

	return ""
}

func convertOpenAIImageURLToClaudePart(imageURL string) string {
	if imageURL == "" {
		return ""
	}

	if strings.HasPrefix(imageURL, "data:") {
		parts := strings.SplitN(imageURL, ",", 2)
		if len(parts) != 2 {
			return ""
		}

		mediaTypePart := strings.SplitN(parts[0], ";", 2)[0]
		mediaType := strings.TrimPrefix(mediaTypePart, "data:")
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}

		imagePart := []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
		imagePart, _ = sjson.SetBytes(imagePart, "source.media_type", mediaType)
		imagePart, _ = sjson.SetBytes(imagePart, "source.data", parts[1])
		return string(imagePart)
	}

	imagePart := []byte(`{"type":"image","source":{"type":"url","url":""}}`)
	imagePart, _ = sjson.SetBytes(imagePart, "source.url", imageURL)
	return string(imagePart)
}

func convertOpenAIToolResultContent(content gjson.Result) (string, bool) {
	if !content.Exists() {
		return "", false
	}

	if content.Type == gjson.String {
		return content.String(), false
	}

	if content.IsArray() {
		claudeContent := []byte("[]")
		partCount := 0
		droppedEmptyText := false

		content.ForEach(func(_, part gjson.Result) bool {
			if part.Type == gjson.String {
				if strings.TrimSpace(part.String()) == "" {
					droppedEmptyText = true
					return true
				}
				textPart := []byte(`{"type":"text","text":""}`)
				textPart, _ = sjson.SetBytes(textPart, "text", part.String())
				claudeContent, _ = sjson.SetRawBytes(claudeContent, "-1", textPart)
				partCount++
				return true
			}

			claudePart := convertOpenAIContentPartToClaudePart(part)
			if claudePart != "" {
				claudeContent, _ = sjson.SetRawBytes(claudeContent, "-1", []byte(claudePart))
				partCount++
			} else if part.Get("type").String() == "text" {
				droppedEmptyText = true
			}
			return true
		})

		if partCount > 0 || len(content.Array()) == 0 {
			return string(claudeContent), true
		}
		if droppedEmptyText {
			return "", false
		}

		return content.Raw, false
	}

	if content.IsObject() {
		claudePart := convertOpenAIContentPartToClaudePart(content)
		if claudePart != "" {
			claudeContent := []byte("[]")
			claudeContent, _ = sjson.SetRawBytes(claudeContent, "-1", []byte(claudePart))
			return string(claudeContent), true
		}
		return content.Raw, false
	}

	return content.Raw, false
}
