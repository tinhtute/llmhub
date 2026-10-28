// Package claude provides request translation functionality for Anthropic to OpenAI API.
// It handles parsing and transforming Anthropic API requests into OpenAI Chat Completions API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Anthropic API format and OpenAI API's expected format.
package claude

import (
	"bytes"
	"encoding/json"
	"strings"

	sigcompat "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeRequestToOpenAI parses and transforms an Anthropic API request into OpenAI Chat Completions API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the OpenAI API.
func ConvertClaudeRequestToOpenAI(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToOpenAI(modelName, inputRawJSON, stream, false)
}

// ConvertClaudeRequestToOpenAIWithCompat preserves assistant thinking text
// for configured compatibility endpoints. Ported from upstream CLIProxyAPI
// (openai_claude_request.go).
func ConvertClaudeRequestToOpenAIWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToOpenAI(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToOpenAI(modelName string, inputRawJSON []byte, stream bool, preserveThinkingBlocks bool) []byte {
	// The internal commentary marker must be restored as assistant text before any
	// thinking handling, so it is never dropped or mapped to reasoning_content.
	rawJSON, _ := sigcompat.RestoreCodexCommentaryMarkers(inputRawJSON)
	// Base OpenAI Chat Completions API template
	out := []byte(`{"model":"","messages":[]}`)

	root := gjson.ParseBytes(rawJSON)

	// Model mapping
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Max tokens
	if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	// Temperature
	if temp := root.Get("temperature"); temp.Exists() {
		out, _ = sjson.SetBytes(out, "temperature", temp.Float())
	} else if topP := root.Get("top_p"); topP.Exists() { // Top P
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}

	// Stop sequences -> stop
	if stopSequences := root.Get("stop_sequences"); stopSequences.Exists() {
		if stopSequences.IsArray() {
			var stops []string
			stopSequences.ForEach(func(_, value gjson.Result) bool {
				stops = append(stops, value.String())
				return true
			})
			if len(stops) > 0 {
				out, _ = sjson.SetBytes(out, "stop", stops)
			}
		}
	}

	// Stream
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Thinking: Convert Claude thinking.budget_tokens to OpenAI reasoning_effort
	if thinkingConfig := root.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		if thinkingType := thinkingConfig.Get("type"); thinkingType.Exists() {
			switch thinkingType.String() {
			case "enabled":
				if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
					budget := int(budgetTokens.Int())
					if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				} else if v := root.Get("output_config.effort"); v.Exists() && v.Type == gjson.String && strings.TrimSpace(v.String()) != "" {
					// Some Claude-compatible clients pair manual thinking with output_config.effort.
					// Preserve that explicit level when there is no legacy token budget to map.
					out, _ = sjson.SetBytes(out, "reasoning_effort", strings.ToLower(strings.TrimSpace(v.String())))
				} else {
					// No budget_tokens or explicit effort specified; preserve the enabled-thinking default.
					if effort, ok := thinking.ConvertBudgetToLevel(-1); ok && effort != "" {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				}
			case "adaptive", "auto":
				// Adaptive thinking can carry an explicit effort in output_config.effort (Claude 4.6).
				// Pass through directly; ApplyThinking handles clamping to target model's levels.
				effort := ""
				if v := root.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
					effort = strings.ToLower(strings.TrimSpace(v.String()))
				}
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				} else {
					out, _ = sjson.SetBytes(out, "reasoning_effort", string(thinking.LevelXHigh))
				}
			case "disabled":
				if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				}
			}
		}
	}

	// Process messages and system
	messagesJSON := []byte(`[]`)

	// Handle system message first
	systemMsgJSON := []byte(`{"role":"system","content":[]}`)
	hasSystemContent := false
	if system := root.Get("system"); system.Exists() {
		if system.Type == gjson.String {
			if system.String() != "" && !util.IsClaudeCodeAttributionSystemText(system.String()) {
				oldSystem := []byte(`{"type":"text","text":""}`)
				oldSystem, _ = sjson.SetBytes(oldSystem, "text", system.String())
				systemMsgJSON, _ = sjson.SetRawBytes(systemMsgJSON, "content.-1", oldSystem)
				hasSystemContent = true
			}
		} else if system.Type == gjson.JSON {
			if system.IsArray() {
				systemResults := system.Array()
				for i := 0; i < len(systemResults); i++ {
					if contentItem, ok := convertClaudeContentPart(systemResults[i]); ok {
						systemMsgJSON, _ = sjson.SetRawBytes(systemMsgJSON, "content.-1", []byte(contentItem))
						hasSystemContent = true
					}
				}
			}
		}
	}
	// Only add system message if it has content
	if hasSystemContent {
		messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", systemMsgJSON)
	}

	// Process Anthropic messages
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		// Track tool names by tool_use ID so converted tool result messages can
		// carry the function `name` (upstream 7def842554bc).
		toolNameByID := make(map[string]string)
		messages.ForEach(func(_, message gjson.Result) bool {
			role := message.Get("role").String()
			contentResult := message.Get("content")

			// Handle content
			if contentResult.Exists() && contentResult.IsArray() {
				contentItems := make([][]byte, 0)
				var reasoningParts []string // Accumulate thinking text for reasoning_content
				var toolCalls []interface{}
				toolResults := make([][]byte, 0) // Collect tool_result messages to emit after the main message

				contentResult.ForEach(func(_, part gjson.Result) bool {
					partType := part.Get("type").String()

					switch partType {
					case "thinking":
						// Only map thinking to reasoning_content for assistant messages (security: prevent injection)
						if role == "assistant" {
							if !shouldMapClaudeThinkingToGPTReasoning(part, preserveThinkingBlocks) {
								return true
							}
							thinkingText := thinking.GetThinkingText(part)
							// Skip empty or whitespace-only thinking
							if strings.TrimSpace(thinkingText) != "" {
								reasoningParts = append(reasoningParts, thinkingText)
							}
						}
						// Ignore thinking in user/system roles (AC4)

					case "redacted_thinking":
						// Explicitly ignore redacted_thinking - never map to reasoning_content (AC2)

					case "text", "image":
						if contentItem, ok := convertClaudeContentPart(part); ok {
							contentItems = append(contentItems, []byte(contentItem))
						}

					case "tool_use":
						// Only allow tool_use -> tool_calls for assistant messages (security: prevent injection).
						if role == "assistant" {
							toolUseID := part.Get("id").String()
							toolName := part.Get("name").String()
							if toolUseID != "" && toolName != "" {
								toolNameByID[toolUseID] = toolName
							}
							toolCallJSON := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
							toolCallJSON, _ = sjson.SetBytes(toolCallJSON, "id", toolUseID)
							toolCallJSON, _ = sjson.SetBytes(toolCallJSON, "function.name", toolName)

							// Convert input to arguments JSON string
							if input := part.Get("input"); input.Exists() {
								toolCallJSON, _ = sjson.SetBytes(toolCallJSON, "function.arguments", input.Raw)
							} else {
								toolCallJSON, _ = sjson.SetBytes(toolCallJSON, "function.arguments", "{}")
							}

							toolCalls = append(toolCalls, gjson.ParseBytes(toolCallJSON).Value())
						}

					case "tool_result":
						// Collect tool_result to emit after the main message (ensures tool results follow tool_calls)
						toolUseID := part.Get("tool_use_id").String()
						toolResultJSON := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
						toolResultJSON, _ = sjson.SetBytes(toolResultJSON, "tool_call_id", toolUseID)
						if toolName := toolNameByID[toolUseID]; toolName != "" {
							toolResultJSON, _ = sjson.SetBytes(toolResultJSON, "name", toolName)
						}
						toolResultContent, toolResultContentRaw := convertClaudeToolResultContent(part.Get("content"))
						if toolResultContentRaw {
							toolResultJSON, _ = sjson.SetRawBytes(toolResultJSON, "content", []byte(toolResultContent))
						} else {
							toolResultJSON, _ = sjson.SetBytes(toolResultJSON, "content", toolResultContent)
						}
						toolResults = append(toolResults, toolResultJSON)
					}
					return true
				})

				// Build reasoning content string
				reasoningContent := ""
				if len(reasoningParts) > 0 {
					reasoningContent = strings.Join(reasoningParts, "\n\n")
				}

				hasContent := len(contentItems) > 0
				hasReasoning := reasoningContent != ""
				hasToolCalls := len(toolCalls) > 0
				hasToolResults := len(toolResults) > 0

				// OpenAI requires: tool messages MUST immediately follow the assistant message with tool_calls.
				// Therefore, we emit tool_result messages FIRST (they respond to the previous assistant's tool_calls),
				// then emit the current message's content.
				for _, toolResultJSON := range toolResults {
					messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", toolResultJSON)
				}

				// For assistant messages: emit a single unified message with content, tool_calls, and reasoning_content
				// This avoids splitting into multiple assistant messages which breaks OpenAI tool-call adjacency
				if role == "assistant" {
					if hasContent || hasReasoning || hasToolCalls {
						msgJSON := []byte(`{"role":"assistant"}`)

						// Add content (as array if we have items, empty string if reasoning-only)
						if hasContent {
							contentArrayJSON := []byte(`[]`)
							for _, contentItem := range contentItems {
								contentArrayJSON, _ = sjson.SetRawBytes(contentArrayJSON, "-1", contentItem)
							}
							msgJSON, _ = sjson.SetRawBytes(msgJSON, "content", contentArrayJSON)
						} else {
							// Ensure content field exists for OpenAI compatibility
							msgJSON, _ = sjson.SetBytes(msgJSON, "content", "")
						}

						// Add reasoning_content if present
						if hasReasoning {
							msgJSON, _ = sjson.SetBytes(msgJSON, "reasoning_content", reasoningContent)
						}

						// Add tool_calls if present (in same message as content)
						if hasToolCalls {
							msgJSON, _ = sjson.SetBytes(msgJSON, "tool_calls", toolCalls)
						}

						messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", msgJSON)
					}
				} else {
					// For non-assistant roles: emit content message if we have content
					// If the message only contains tool_results (no text/image), we still processed them above
					if hasContent {
						msgJSON := []byte(`{"role":""}`)
						msgJSON, _ = sjson.SetBytes(msgJSON, "role", role)

						contentArrayJSON := []byte(`[]`)
						for _, contentItem := range contentItems {
							contentArrayJSON, _ = sjson.SetRawBytes(contentArrayJSON, "-1", contentItem)
						}
						msgJSON, _ = sjson.SetRawBytes(msgJSON, "content", contentArrayJSON)

						messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", msgJSON)
					} else if hasToolResults && !hasContent {
						// tool_results already emitted above, no additional user message needed
					}
				}

			} else if contentResult.Exists() && contentResult.Type == gjson.String {
				// Simple string content
				msgJSON := []byte(`{"role":"","content":""}`)
				msgJSON, _ = sjson.SetBytes(msgJSON, "role", role)
				msgJSON, _ = sjson.SetBytes(msgJSON, "content", contentResult.String())
				messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", msgJSON)
			}

			return true
		})
	}

	// Set messages. Tool result messages are aligned to immediately follow the
	// assistant message that issued their matching tool_calls (upstream cc545cbf).
	if msgs := gjson.ParseBytes(messagesJSON); msgs.IsArray() && len(msgs.Array()) > 0 {
		messageItems := make([][]byte, 0, len(msgs.Array()))
		for _, m := range msgs.Array() {
			messageItems = append(messageItems, []byte(m.Raw))
		}
		messageItems = translatorcommon.AlignOpenAIToolCallMessages(messageItems)
		out = translatorcommon.SetRawArrayItems(out, "messages", messageItems)
	}

	// Process tools - convert Anthropic tools to OpenAI functions
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		toolsJSON := []byte(`[]`)

		tools.ForEach(func(_, tool gjson.Result) bool {
			openAIToolJSON := []byte(`{"type":"function","function":{"name":"","description":""}}`)
			openAIToolJSON, _ = sjson.SetBytes(openAIToolJSON, "function.name", tool.Get("name").String())
			openAIToolJSON, _ = sjson.SetBytes(openAIToolJSON, "function.description", tool.Get("description").String())

			// Convert Anthropic input_schema to OpenAI function parameters.
			// Default to an empty object schema when input_schema is omitted or null.
			// Regex patterns with unsupported Unicode property escapes are stripped
			// (shared util predicate) since strict upstream validators reject them.
			if inputSchema := tool.Get("input_schema"); inputSchema.Exists() && inputSchema.Type != gjson.Null {
				openAIToolJSON, _ = sjson.SetRawBytes(openAIToolJSON, "function.parameters", []byte(stripUnsupportedUnicodePatternsFromSchemaJSON(inputSchema.Raw)))
			} else {
				openAIToolJSON, _ = sjson.SetRawBytes(openAIToolJSON, "function.parameters", []byte(`{"type":"object","properties":{}}`))
			}

			toolsJSON, _ = sjson.SetRawBytes(toolsJSON, "-1", openAIToolJSON)
			return true
		})

		if parsed := gjson.ParseBytes(toolsJSON); parsed.IsArray() && len(parsed.Array()) > 0 {
			out, _ = sjson.SetRawBytes(out, "tools", toolsJSON)
		}
	}

	// Tool choice mapping - convert Anthropic tool_choice to OpenAI format
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() && toolChoice.Type != gjson.Null {
		choiceType := toolChoice.Get("type").String()
		if choiceType == "" && toolChoice.Type == gjson.String {
			choiceType = toolChoice.String()
		}
		switch choiceType {
		case "auto":
			out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		case "any":
			out, _ = sjson.SetBytes(out, "tool_choice", "required")
		case "none":
			out, _ = sjson.SetBytes(out, "tool_choice", "none")
		case "tool":
			// Specific tool choice
			toolName := toolChoice.Get("name").String()
			if toolName != "" {
				toolChoiceJSON := []byte(`{"type":"function","function":{"name":""}}`)
				toolChoiceJSON, _ = sjson.SetBytes(toolChoiceJSON, "function.name", toolName)
				out, _ = sjson.SetRawBytes(out, "tool_choice", toolChoiceJSON)
			} else {
				out, _ = sjson.SetBytes(out, "tool_choice", "none")
			}
		default:
			// Fail closed: unrecognized tool_choice values must not turn into permission
			out, _ = sjson.SetBytes(out, "tool_choice", "none")
		}

		// Map Claude disable_parallel_tool_use to OpenAI parallel_tool_calls=false
		// (upstream 49eec664).
		if disableParallel := toolChoice.Get("disable_parallel_tool_use"); disableParallel.Type == gjson.True {
			out, _ = sjson.SetBytes(out, "parallel_tool_calls", false)
		}
	}

	// Handle user parameter (for tracking)
	if user := root.Get("user"); user.Exists() {
		out, _ = sjson.SetBytes(out, "user", user.String())
	}

	return out
}

// stripUnsupportedUnicodePatternsFromSchemaJSON drops `pattern` entries whose
// value contains unsupported Unicode property escapes (\p{...} / \P{...}) and
// removes patternProperties entries keyed by such patterns. Adapted from
// upstream e56abd56f142 / 37ce368c5002. The walk is schema-aware: only
// subschemas under known JSON Schema keyword locations (util.SchemaMapKeywords
// / util.SchemaValueKeywords) are visited, so `pattern` keys inside user data
// (e.g. description, default, enum) are preserved. The input is returned
// unchanged when no incompatible pattern is present.
func stripUnsupportedUnicodePatternsFromSchemaJSON(raw string) string {
	if !strings.Contains(raw, `\p{`) && !strings.Contains(raw, `\P{`) && !strings.Contains(raw, `\u`) {
		return raw
	}
	var root any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil || root == nil {
		return raw
	}
	if !stripUnsupportedUnicodePatterns(root) {
		return raw
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return raw
	}
	return strings.TrimSpace(buf.String())
}

func stripUnsupportedUnicodePatterns(v any) bool {
	changed := false
	switch schema := v.(type) {
	case map[string]any:
		if patternVal, ok := schema["pattern"].(string); ok && util.HasUnsupportedUnicodePropertyEscape(patternVal) {
			delete(schema, "pattern")
			changed = true
		}
		if patternProps, ok := schema["patternProperties"].(map[string]any); ok {
			for patternKey, subSchema := range patternProps {
				if util.HasUnsupportedUnicodePropertyEscape(patternKey) {
					delete(patternProps, patternKey)
					changed = true
				} else if stripUnsupportedUnicodePatterns(subSchema) {
					changed = true
				}
			}
		}
		for _, mapKey := range util.SchemaMapKeywords {
			if mapKey == "patternProperties" {
				continue
			}
			if subMap, ok := schema[mapKey].(map[string]any); ok {
				for _, subSchema := range subMap {
					if stripUnsupportedUnicodePatterns(subSchema) {
						changed = true
					}
				}
			}
		}
		for _, valKey := range util.SchemaValueKeywords {
			if val, exists := schema[valKey]; exists {
				switch sub := val.(type) {
				case map[string]any:
					if stripUnsupportedUnicodePatterns(sub) {
						changed = true
					}
				case []any:
					for _, item := range sub {
						if stripUnsupportedUnicodePatterns(item) {
							changed = true
						}
					}
				}
			}
		}
	case []any:
		for _, item := range schema {
			if stripUnsupportedUnicodePatterns(item) {
				changed = true
			}
		}
	}
	return changed
}

func convertClaudeContentPart(part gjson.Result) (string, bool) {
	partType := part.Get("type").String()

	switch partType {
	case "text":
		text := part.Get("text").String()
		if strings.TrimSpace(text) == "" || util.IsClaudeCodeAttributionSystemText(text) {
			return "", false
		}
		textContent := []byte(`{"type":"text","text":""}`)
		textContent, _ = sjson.SetBytes(textContent, "text", text)
		return string(textContent), true

	case "image":
		var imageURL string

		if source := part.Get("source"); source.Exists() {
			sourceType := source.Get("type").String()
			switch sourceType {
			case "base64":
				mediaType := source.Get("media_type").String()
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				data := source.Get("data").String()
				if data != "" {
					imageURL = "data:" + mediaType + ";base64," + data
				}
			case "url":
				imageURL = source.Get("url").String()
			}
		}

		if imageURL == "" {
			imageURL = part.Get("url").String()
		}

		if imageURL == "" {
			return "", false
		}

		imageContent := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		imageContent, _ = sjson.SetBytes(imageContent, "image_url.url", imageURL)

		return string(imageContent), true

	default:
		return "", false
	}
}

func convertClaudeToolResultContent(content gjson.Result) (string, bool) {
	if !content.Exists() {
		return "", false
	}

	if content.Type == gjson.String {
		return content.String(), false
	}

	if content.IsArray() {
		var parts []string
		contentJSON := []byte(`[]`)
		hasImagePart := false
		content.ForEach(func(_, item gjson.Result) bool {
			switch {
			case item.Type == gjson.String:
				text := item.String()
				parts = append(parts, text)
				textContent := []byte(`{"type":"text","text":""}`)
				textContent, _ = sjson.SetBytes(textContent, "text", text)
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "-1", textContent)
			case item.IsObject() && item.Get("type").String() == "text":
				text := item.Get("text").String()
				parts = append(parts, text)
				textContent := []byte(`{"type":"text","text":""}`)
				textContent, _ = sjson.SetBytes(textContent, "text", text)
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "-1", textContent)
			case item.IsObject() && item.Get("type").String() == "image":
				contentItem, ok := convertClaudeContentPart(item)
				if ok {
					contentJSON, _ = sjson.SetRawBytes(contentJSON, "-1", []byte(contentItem))
					hasImagePart = true
				} else {
					parts = append(parts, item.Raw)
				}
			case item.IsObject() && item.Get("text").Exists() && item.Get("text").Type == gjson.String:
				parts = append(parts, item.Get("text").String())
			default:
				parts = append(parts, item.Raw)
			}
			return true
		})

		if hasImagePart {
			return string(contentJSON), true
		}

		joined := strings.Join(parts, "\n\n")
		if strings.TrimSpace(joined) != "" {
			return joined, false
		}
		return content.Raw, false
	}

	if content.IsObject() {
		if content.Get("type").String() == "image" {
			contentItem, ok := convertClaudeContentPart(content)
			if ok {
				contentJSON := []byte(`[]`)
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "-1", []byte(contentItem))
				return string(contentJSON), true
			}
		}
		if text := content.Get("text"); text.Exists() && text.Type == gjson.String {
			return text.String(), false
		}
		return content.Raw, false
	}

	return content.Raw, false
}

// shouldMapClaudeThinkingToGPTReasoning reports whether an assistant thinking
// part may be mapped into OpenAI reasoning_content. Compat endpoints preserve
// the text unconditionally; otherwise a GPT-replayable signature is required.
// Ported from upstream CLIProxyAPI (openai_claude_request.go).
func shouldMapClaudeThinkingToGPTReasoning(part gjson.Result, preserveThinkingBlocks ...bool) bool {
	if len(preserveThinkingBlocks) > 0 && preserveThinkingBlocks[0] {
		return true
	}
	signature := part.Get("signature")
	if !signature.Exists() || strings.TrimSpace(signature.String()) == "" {
		return false
	}
	_, ok := sigcompat.CompatibleSignatureForProvider(sigcompat.SignatureProviderGPT, signature.String())
	return ok
}
