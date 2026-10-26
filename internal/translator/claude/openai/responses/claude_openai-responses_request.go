package responses

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"

	log "github.com/sirupsen/logrus"

	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	common "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	defaultClaudeResponsesMaxTokens = 32000
	defaultFableResponsesMaxTokens  = 64000
)

// ConvertOpenAIResponsesRequestToClaude transforms an OpenAI Responses API request
// into a Claude Messages API request using only gjson/sjson for JSON handling.
func ConvertOpenAIResponsesRequestToClaude(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertOpenAIResponsesRequestToClaude(modelName, inputRawJSON, stream)
}

// ConvertOpenAIResponsesRequestToClaudeWithCompat preserves reasoning items
// whose encrypted content is empty for configured compatibility endpoints.
func ConvertOpenAIResponsesRequestToClaudeWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertOpenAIResponsesRequestToClaude(modelName, inputRawJSON, stream)
}

func convertOpenAIResponsesRequestToClaude(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON

	userID := common.DeriveClaudeUserID(rawJSON)

	// Base Claude message payload
	out := []byte(`{"model":"","max_tokens":32000,"messages":[],"metadata":{}}`)
	out, _ = sjson.SetBytes(out, "metadata.user_id", userID)
	out, _ = sjson.SetBytes(out, "max_tokens", defaultClaudeResponsesMaxTokensForModel(modelName))

	root := gjson.ParseBytes(rawJSON)

	// Convert OpenAI Responses reasoning.effort to Claude thinking config.
	if v := root.Get("reasoning.effort"); v.Exists() {
		effort := strings.ToLower(strings.TrimSpace(v.String()))
		if effort != "" {
			mi := registry.LookupModelInfo(modelName, "claude")
			supportsAdaptive := mi != nil && mi.Thinking != nil && len(mi.Thinking.Levels) > 0
			supportsMax := supportsAdaptive && thinking.HasLevel(mi.Thinking.Levels, string(thinking.LevelMax))

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

	// Helper for generating tool call IDs when missing
	genToolCallID := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		var b strings.Builder
		for range 24 {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			b.WriteByte(letters[n.Int64()])
		}
		return "toolu_" + b.String()
	}

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Max tokens
	if mot := root.Get("max_output_tokens"); mot.Exists() && mot.Type != gjson.Null {
		val := mot.Int()
		if info := registry.LookupModelInfo(modelName, "claude"); info != nil && info.MaxCompletionTokens > 0 && val > int64(info.MaxCompletionTokens) {
			val = int64(info.MaxCompletionTokens)
		}
		out, _ = sjson.SetBytes(out, "max_tokens", val)
	}

	// Service Tier -> Speed
	if st := root.Get("service_tier"); st.Type == gjson.String && st.String() == "priority" {
		out, _ = sjson.SetBytes(out, "speed", "fast")
	}

	// Stream
	out, _ = sjson.SetBytes(out, "stream", stream)

	var messageBlocks [][]byte

	// instructions -> as a leading message (use role user for Claude API compatibility)
	instructionsText := ""
	extractedFromSystem := false
	if instr := root.Get("instructions"); instr.Exists() && instr.Type == gjson.String {
		instructionsText = instr.String()
		if instructionsText != "" {
			sysMsg := []byte(`{"role":"user","content":""}`)
			sysMsg, _ = sjson.SetBytes(sysMsg, "content", instructionsText)
			messageBlocks = append(messageBlocks, sysMsg)
		}
	}

	if instructionsText == "" {
		if input := root.Get("input"); input.Exists() && input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				if strings.EqualFold(item.Get("role").String(), "system") {
					var builder strings.Builder
					if parts := item.Get("content"); parts.Exists() && parts.IsArray() {
						parts.ForEach(func(_, part gjson.Result) bool {
							textResult := part.Get("text")
							text := textResult.String()
							if builder.Len() > 0 && text != "" {
								builder.WriteByte('\n')
							}
							builder.WriteString(text)
							return true
						})
					} else if parts.Type == gjson.String {
						builder.WriteString(parts.String())
					}
					instructionsText = builder.String()
					if instructionsText != "" {
						sysMsg := []byte(`{"role":"user","content":""}`)
						sysMsg, _ = sjson.SetBytes(sysMsg, "content", instructionsText)
						messageBlocks = append(messageBlocks, sysMsg)
						extractedFromSystem = true
					}
				}
				return instructionsText == ""
			})
		}
	}

	// input array processing with contiguous role grouping
	var pendingRole string
	var pendingParts [][]byte
	var pendingToolUseParts [][]byte

	// Tool-call pairing state. Outputs pair against the raw call id so two
	// distinct ids that sanitize to the same Claude id are not mistaken for
	// the same call.
	emittedToolResults := map[string]struct{}{}
	emittedRawToolUses := map[string]struct{}{}
	unmappedItemTypes := map[string]int{}

	appendMessage := func(msg []byte) {
		messageBlocks = append(messageBlocks, msg)
	}

	flushPendingMessage := func() {
		if pendingRole == "" {
			return
		}

		parts := pendingParts
		if pendingRole == "assistant" && len(pendingToolUseParts) > 0 {
			combined := make([][]byte, 0, len(pendingParts)+len(pendingToolUseParts)+1)
			combined = append(combined, pendingParts...)
			if separator := claudeThinkingSeparatorForToolUse(pendingParts); separator != nil {
				combined = append(combined, separator)
			}
			combined = append(combined, pendingToolUseParts...)
			parts = combined
		}
		if len(parts) > 0 {
			msg := []byte(`{"role":"","content":[]}`)
			msg, _ = sjson.SetBytes(msg, "role", pendingRole)
			if len(parts) == 1 {
				part := gjson.ParseBytes(parts[0])
				if part.Get("type").String() == "text" && !part.Get("cache_control").Exists() && !part.Get("citations").Exists() {
					msg, _ = sjson.SetBytes(msg, "content", part.Get("text").String())
				} else {
					msg, _ = sjson.SetRawBytes(msg, "content", common.JoinRawArray(parts))
				}
			} else {
				msg, _ = sjson.SetRawBytes(msg, "content", common.JoinRawArray(parts))
			}
			appendMessage(msg)
		}

		pendingRole = ""
		pendingParts = nil
		pendingToolUseParts = nil
	}

	appendParts := func(role string, parts ...[]byte) {
		if role == "" || len(parts) == 0 {
			return
		}
		if pendingRole != "" && pendingRole != role {
			flushPendingMessage()
		}
		pendingRole = role
		pendingParts = append(pendingParts, parts...)
	}

	appendToolUse := func(toolUse []byte) {
		if len(toolUse) == 0 {
			return
		}
		if pendingRole != "" && pendingRole != "assistant" {
			flushPendingMessage()
		}
		pendingRole = "assistant"
		pendingToolUseParts = append(pendingToolUseParts, toolUse)
	}

	if input := root.Get("input"); input.Exists() && input.Type == gjson.String {
		// Responses API allows `input` as a plain string (upstream b989e34881c7):
		// translate it into a single user text message.
		contentPart := []byte(`{"type":"text","text":""}`)
		contentPart, _ = sjson.SetBytes(contentPart, "text", input.String())
		appendParts("user", contentPart)
	}
	lastToolResult := map[string]gjson.Result{}
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "function_call_output", "custom_tool_call_output":
				rawID := common.ExtractResponsesCallID(item)
				if rawID != "" {
					lastToolResult[rawID] = item
				}
			}
			return true
		})
	}
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if extractedFromSystem && strings.EqualFold(item.Get("role").String(), "system") {
				return true
			}
			typ := item.Get("type").String()
			if typ == "" && item.Get("role").String() != "" {
				typ = "message"
			}
			switch typ {
			case "message":
				var role string
				var partsJSON [][]byte
				if parts := item.Get("content"); parts.Exists() && parts.IsArray() {
					parts.ForEach(func(_, part gjson.Result) bool {
						ptype := part.Get("type").String()
						switch ptype {
						case "input_text", "output_text":
							if t := part.Get("text"); t.Exists() {
								txt := t.String()
								contentPart := []byte(`{"type":"text","text":""}`)
								contentPart, _ = sjson.SetBytes(contentPart, "text", txt)
								contentPart = attachClaudeCitations(contentPart, part.Get("annotations"))
								partsJSON = append(partsJSON, contentPart)
							}
							if ptype == "input_text" {
								role = "user"
							} else {
								role = "assistant"
							}
						case "refusal":
							if t := part.Get("refusal"); t.Exists() && t.String() != "" {
								contentPart := []byte(`{"type":"text","text":""}`)
								contentPart, _ = sjson.SetBytes(contentPart, "text", t.String())
								partsJSON = append(partsJSON, contentPart)
							}
							role = "assistant"
						case "input_image":
							url := part.Get("image_url").String()
							if url == "" {
								url = part.Get("url").String()
							}
							if url != "" {
								var contentPart []byte
								if strings.HasPrefix(url, "data:") {
									mediaType, data, ok := parseResponsesBase64DataURL(url)
									if ok {
										contentPart = []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
										contentPart, _ = sjson.SetBytes(contentPart, "source.media_type", mediaType)
										contentPart, _ = sjson.SetBytes(contentPart, "source.data", data)
									}
								} else {
									contentPart = []byte(`{"type":"image","source":{"type":"url","url":""}}`)
									contentPart, _ = sjson.SetBytes(contentPart, "source.url", url)
								}
								if len(contentPart) > 0 {
									partsJSON = append(partsJSON, contentPart)
									if role == "" {
										role = "user"
									}
								}
							}
						case "input_file":
							fileData := part.Get("file_data").String()
							if fileData != "" {
								mediaType := "application/octet-stream"
								data := fileData
								if strings.HasPrefix(fileData, "data:") {
									if m, d, ok := parseResponsesBase64DataURL(fileData); ok {
										mediaType = m
										data = d
									}
								}
								contentPart := []byte(`{"type":"document","source":{"type":"base64","media_type":"","data":""}}`)
								contentPart, _ = sjson.SetBytes(contentPart, "source.media_type", mediaType)
								contentPart, _ = sjson.SetBytes(contentPart, "source.data", data)
								partsJSON = append(partsJSON, contentPart)
								if role == "" {
									role = "user"
								}
							}
						}
						return true
					})
				} else if parts.Type == gjson.String && parts.String() != "" {
					contentPart := []byte(`{"type":"text","text":""}`)
					contentPart, _ = sjson.SetBytes(contentPart, "text", parts.String())
					partsJSON = append(partsJSON, contentPart)
				}

				if role == "" {
					r := item.Get("role").String()
					switch r {
					case "user", "assistant", "system":
						role = r
					default:
						role = "user"
					}
				}

				if len(partsJSON) > 0 {
					appendParts(role, partsJSON...)
				}

			case "web_search_call":
				if blocks := convertResponsesWebSearchCallToClaudeBlocks(item); len(blocks) > 0 {
					appendParts("assistant", blocks...)
				}

			case "reasoning":
				if thinkingPart := convertResponsesReasoningToClaudeThinking(item); len(thinkingPart) > 0 {
					appendParts("assistant", thinkingPart)
				}

			case "function_call", "custom_tool_call":
				// Map to assistant tool_use. Freeform custom input is wrapped in
				// an object because Claude tool_use input must be a JSON object.
				rawCallID := common.ExtractResponsesCallID(item)
				callID := rawCallID
				if callID == "" {
					callID = genToolCallID()
				}
				callID = util.SanitizeClaudeToolID(callID)
				if rawCallID != "" {
					emittedRawToolUses[rawCallID] = struct{}{}
				}
				name := item.Get("name").String()
				if namespaceName := strings.TrimSpace(item.Get("namespace").String()); namespaceName != "" {
					// Rebuild the qualified name emitted by the previous Responses turn.
					name = qualifyResponsesNamespaceToolName(namespaceName, name)
				}
				isCustomToolCall := typ == "custom_tool_call"

				toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
				toolUse, _ = sjson.SetBytes(toolUse, "id", callID)
				toolUse, _ = sjson.SetBytes(toolUse, "name", util.SanitizeClaudeFunctionName(name))
				if isCustomToolCall {
					toolUse, _ = sjson.SetBytes(toolUse, "input.input", item.Get("input").String())
				} else {
					argsStr := item.Get("arguments").String()
					if argsStr != "" && gjson.Valid(argsStr) {
						argsJSON := gjson.Parse(argsStr)
						if argsJSON.IsObject() {
							toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(argsJSON.Raw))
						}
					}
				}

				appendToolUse(toolUse)
			case "function_call_output", "custom_tool_call_output":
				rawID := common.ExtractResponsesCallID(item)
				if rawID != "" {
					if _, exists := emittedToolResults[rawID]; exists {
						return true
					}
					emittedToolResults[rawID] = struct{}{}
				}
				outputItem := item
				if rawID != "" {
					if lastItem, exists := lastToolResult[rawID]; exists {
						outputItem = lastItem
					}
				}
				output := outputItem.Get("output")
				// Standalone outputs (no call_id, or one that never paired with
				// a function_call in this input) have no tool_use to attach to.
				// Claude rejects orphan tool_result blocks, so surface them as
				// plain user text instead. Pairing is decided on the raw id so
				// two distinct ids that sanitize to the same Claude id are not
				// mistaken for the same call.
				if _, paired := emittedRawToolUses[rawID]; rawID == "" || !paired {
					appendParts("user", convertResponsesStandaloneToolOutputToClaudeText(output)...)
					return true
				}
				callID := util.SanitizeClaudeToolID(rawID)
				toolResult := []byte(`{"type":"tool_result","tool_use_id":"","content":""}`)
				toolResult, _ = sjson.SetBytes(toolResult, "tool_use_id", callID)
				// Responses reports an unfinished tool output as status
				// "incomplete"; Claude's only in-band equivalent is is_error
				// on tool_result, so carry the signal over instead of dropping it.
				if strings.EqualFold(strings.TrimSpace(outputItem.Get("status").String()), "incomplete") {
					toolResult, _ = sjson.SetBytes(toolResult, "is_error", true)
				}
				toolResult = applyResponsesToolResultContent(toolResult, output)

				appendParts("user", toolResult)

			default:
				// A new item type means the client gained a capability whose
				// Claude counterpart still has to be decided, so make the gap
				// visible instead of dropping the turn content in silence.
				if typ := item.Get("type").String(); typ != "" && typ != "additional_tools" {
					// additional_tools is consumed later when building tools[], so it
					// is not dropped turn content and must not warn.
					unmappedItemTypes[typ]++
				}
			}
			return true
		})
	}
	flushPendingMessage()
	if len(unmappedItemTypes) > 0 {
		log.Warnf("responses->claude: dropped input items of unmapped types %v (model=%s)", unmappedItemTypes, modelName)
	}
	// Answer dangling tool_use blocks so an interrupted turn ends with a
	// synthesized user tool_result instead of an assistant prefill Anthropic
	// rejects.
	messageBlocks = repairClaudeToolPairing(messageBlocks)
	if problems := claudeMessageInvariantProblems(messageBlocks); len(problems) > 0 {
		log.Warnf("responses->claude: message invariants violated after repair (model=%s): %s", modelName, strings.Join(problems, "; "))
	}

	// Claude compatibility normalizations (adapted from upstream 07d815637,
	// c350d3f52, 6f25b9a14): collapse consecutive thinking blocks to the
	// latest, strip trailing thinking from the final assistant message, and
	// drop trailing assistant prefill for models that reject it.
	messageBlocks = collapseConsecutiveClaudeThinkingBlocks(messageBlocks)
	messageBlocks = stripTrailingClaudeThinkingBlocks(messageBlocks)
	messageBlocks = dropUnsupportedClaudeAssistantPrefill(modelName, messageBlocks)

	if len(messageBlocks) > 0 {
		out, _ = sjson.SetRawBytes(out, "messages", common.JoinRawArray(messageBlocks))
	}

	includedToolNames := map[string]struct{}{}

	// Responses Lite puts tool definitions in input[].additional_tools alongside
	// the top-level tools array. The shared descriptor table picks one winning
	// declaration per final (qualified) name — top-level tools ahead of
	// additional_tools, direct declarations ahead of namespace children — while
	// the ordered walk below emits the survivors in their original order and
	// still converts provider-specific entries such as web_search in place.
	winners := util.CollectResponsesToolWinners(root)
	winningTool := make(map[int]struct{}, len(winners))
	for _, descriptor := range util.CollectResponsesToolDescriptors(root) {
		winner, ok := winners[descriptor.Name]
		if !ok || winner.Order != descriptor.Order {
			continue
		}
		winningTool[descriptor.Tool.Index] = struct{}{}
	}

	var toolItems [][]byte
	// Non-callable entries (web_search, unknown named tools) are not part of the
	// shared descriptor table, so they dedupe here: first declaration wins and a
	// name already claimed by a winning function/custom is never emitted twice.
	emittedOtherToolNames := map[string]struct{}{}
	appendToolItem := func(tJSON []byte) {
		if name := gjson.GetBytes(tJSON, "name").String(); name != "" {
			includedToolNames[name] = struct{}{}
		}
		toolItems = append(toolItems, tJSON)
	}
	appendOtherTool := func(tJSON []byte) bool {
		name := gjson.GetBytes(tJSON, "name").String()
		if name == "" {
			return false
		}
		if _, claimed := winners[name]; claimed {
			return false
		}
		if _, dup := emittedOtherToolNames[name]; dup {
			return false
		}
		emittedOtherToolNames[name] = struct{}{}
		appendToolItem(tJSON)
		return true
	}
	var collectClaudeTools func(tools gjson.Result, namespaceName string)
	collectClaudeTools = func(tools gjson.Result, namespaceName string) {
		if !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			toolType := strings.TrimSpace(tool.Get("type").String())
			if toolType == "namespace" {
				children := tool.Get("tools")
				if !children.Exists() || !children.IsArray() {
					children = tool.Get("children")
				}
				collectClaudeTools(children, strings.TrimSpace(tool.Get("name").String()))
				return true
			}
			if namespaceName != "" {
				// Only callable children of a namespace become Claude tools; the
				// winner table already picked the surviving declaration per
				// qualified name.
				if _, ok := winningTool[tool.Index]; !ok {
					return true
				}
				qualifiedName := qualifyResponsesNamespaceToolName(namespaceName, responsesToolName(tool))
				var tJSON []byte
				var ok bool
				switch toolType {
				case "", "function":
					tJSON, ok = convertResponsesFunctionToolToClaude(tool, qualifiedName)
				case "custom":
					if !isOpenAIResponsesApplyPatchCustomTool(toolType, tool) {
						tJSON, ok = convertResponsesCustomToolToClaude(tool, qualifiedName)
					}
				}
				if ok {
					appendToolItem(tJSON)
				}
				return true
			}
			switch toolType {
			case "", "function", "custom":
				if isOpenAIResponsesApplyPatchCustomTool(toolType, tool) {
					return true
				}
				if _, ok := winningTool[tool.Index]; !ok {
					return true
				}
				var tJSON []byte
				var ok bool
				if toolType == "custom" {
					tJSON, ok = convertResponsesCustomToolToClaude(tool, "")
				} else {
					tJSON, ok = convertResponsesFunctionToolToClaude(tool, "")
				}
				if ok {
					appendToolItem(tJSON)
				}
			case "web_search":
				if tJSON, ok := convertResponsesWebSearchToolToClaude(tool); ok {
					appendOtherTool(tJSON)
				}
			default:
				if isUnsupportedOpenAIBuiltinToolType(toolType) {
					return true
				}
				if tool.Get("name").String() == "" {
					return true
				}
				appendOtherTool([]byte(tool.Raw))
			}
			return true
		})
	}
	collectClaudeTools(root.Get("tools"), "")
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "additional_tools" {
				collectClaudeTools(item.Get("tools"), "")
			}
			return true
		})
	}
	toolNameMap := responsesToolNameMap(root, includedToolNames)
	if len(toolItems) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", common.JoinRawArray(toolItems))
	}

	// Map tool_choice similar to Chat Completions translator (optional in docs, safe to handle)
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		switch toolChoice.Type {
		case gjson.String:
			switch toolChoice.String() {
			case "auto":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
			case "none":
				// Leave unset; implies no tools
			case "required":
				if len(includedToolNames) > 0 {
					out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
				}
			}
		case gjson.JSON:
			choiceType := toolChoice.Get("type").String()
			if choiceType == "function" || choiceType == "custom" {
				fn := toolChoice.Get("function.name").String()
				if fn == "" {
					fn = toolChoice.Get("custom.name").String()
				}
				if fn == "" {
					fn = toolChoice.Get("name").String()
				}
				namespaceName := toolChoice.Get("namespace").String()
				if namespaceName == "" {
					namespaceName = toolChoice.Get("function.namespace").String()
				}
				if namespaceName == "" {
					namespaceName = toolChoice.Get("custom.namespace").String()
				}
				if namespaceName != "" {
					fn = qualifyResponsesNamespaceToolName(namespaceName, fn)
				}
				if mappedName := toolNameMap[fn]; mappedName != "" {
					fn = mappedName
				}
				if _, ok := includedToolNames[fn]; ok {
					toolChoiceJSON := []byte(`{"name":"","type":"tool"}`)
					toolChoiceJSON, _ = sjson.SetBytes(toolChoiceJSON, "name", util.SanitizeClaudeFunctionName(fn))
					out, _ = sjson.SetRawBytes(out, "tool_choice", toolChoiceJSON)
				}
			}
		default:

		}
	}

	return thinking.ApplyTranslatedSummaryToClaude(out, inputRawJSON, "openai-response", modelName)
}

func defaultClaudeResponsesMaxTokensForModel(modelName string) int {
	maxTokens := defaultClaudeResponsesMaxTokens
	if strings.Contains(strings.ToLower(strings.TrimSpace(modelName)), "fable") {
		maxTokens = defaultFableResponsesMaxTokens
	}
	if info := registry.LookupModelInfo(modelName, "claude"); info != nil && info.MaxCompletionTokens > 0 && info.MaxCompletionTokens < maxTokens {
		return info.MaxCompletionTokens
	}
	return maxTokens
}

// collapseConsecutiveClaudeThinkingBlocks replaces runs of consecutive
// thinking/redacted_thinking blocks within an assistant message with the
// latest block. Adapted from upstream 07d815637: only the most recent
// reasoning item in a run is kept; tool_use and text blocks break runs.
func collapseConsecutiveClaudeThinkingBlocks(messages [][]byte) [][]byte {
	for i, msg := range messages {
		parsed := gjson.ParseBytes(msg)
		if !strings.EqualFold(strings.TrimSpace(parsed.Get("role").String()), "assistant") {
			continue
		}
		content := parsed.Get("content")
		if !content.IsArray() {
			continue
		}
		parts := content.Array()
		if len(parts) < 2 {
			continue
		}
		collapsed := make([][]byte, 0, len(parts))
		for _, part := range parts {
			partType := strings.TrimSpace(part.Get("type").String())
			isThinking := partType == "thinking" || partType == "redacted_thinking"
			if isThinking && len(collapsed) > 0 {
				prevType := strings.TrimSpace(gjson.ParseBytes(collapsed[len(collapsed)-1]).Get("type").String())
				if prevType == "thinking" || prevType == "redacted_thinking" {
					collapsed[len(collapsed)-1] = []byte(part.Raw)
					continue
				}
			}
			collapsed = append(collapsed, []byte(part.Raw))
		}
		if len(collapsed) == len(parts) {
			continue
		}
		updated, err := sjson.SetRawBytes(msg, "content", common.JoinRawArray(collapsed))
		if err != nil {
			continue
		}
		messages[i] = updated
	}
	return messages
}

// stripTrailingClaudeThinkingBlocks removes trailing thinking and
// redacted_thinking blocks from the final assistant message. Adapted from
// upstream c350d3f52: Anthropic rejects a final assistant block that is
// thinking. The message is dropped when nothing remains.
func stripTrailingClaudeThinkingBlocks(messages [][]byte) [][]byte {
	if len(messages) == 0 {
		return messages
	}
	lastIdx := len(messages) - 1
	last := gjson.ParseBytes(messages[lastIdx])
	if !strings.EqualFold(strings.TrimSpace(last.Get("role").String()), "assistant") {
		return messages
	}
	content := last.Get("content")
	if !content.IsArray() {
		return messages
	}
	parts := content.Array()
	end := len(parts)
	for end > 0 {
		partType := strings.TrimSpace(parts[end-1].Get("type").String())
		if partType == "thinking" || partType == "redacted_thinking" {
			end--
		} else {
			break
		}
	}
	if end == len(parts) {
		return messages
	}
	if end == 0 {
		return messages[:lastIdx]
	}
	remaining := make([][]byte, end)
	for i := 0; i < end; i++ {
		remaining[i] = []byte(parts[i].Raw)
	}
	updated, err := sjson.SetRawBytes(messages[lastIdx], "content", common.JoinRawArray(remaining))
	if err != nil {
		return messages
	}
	messages[lastIdx] = updated
	return messages
}

// claudeModelRejectsAssistantPrefill reports whether a Claude model family
// disallows trailing assistant prefill. Adapted from upstream 6f25b9a14.
func claudeModelRejectsAssistantPrefill(modelName string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	if index := strings.LastIndexByte(normalized, '/'); index >= 0 {
		normalized = normalized[index+1:]
	}
	normalized = strings.TrimPrefix(normalized, "claude-")
	tokens := strings.Split(strings.ReplaceAll(normalized, ".", "-"), "-")
	if len(tokens) == 0 {
		return false
	}
	if tokens[0] == "fable" {
		return true
	}
	if len(tokens) < 2 || (tokens[0] != "opus" && tokens[0] != "sonnet" && tokens[0] != "haiku") {
		return false
	}
	parseVersion := func(token string) int {
		if token == "" || len(token) >= 8 {
			return -1
		}
		for i := range token {
			if token[i] < '0' || token[i] > '9' {
				return -1
			}
		}
		version, errAtoi := strconv.Atoi(token)
		if errAtoi != nil {
			return -1
		}
		return version
	}
	major := parseVersion(tokens[1])
	if major >= 5 {
		return true
	}
	return tokens[0] == "sonnet" && major == 4 && len(tokens) > 2 && parseVersion(tokens[2]) >= 6
}

// dropUnsupportedClaudeAssistantPrefill removes a trailing assistant prefill
// message for models that reject it. Only text/thinking-only trailing
// assistants are dropped; trailing tool_use turns are preserved as real tool
// calls rather than prefill.
func dropUnsupportedClaudeAssistantPrefill(modelName string, messages [][]byte) [][]byte {
	if !claudeModelRejectsAssistantPrefill(modelName) || len(messages) == 0 {
		return messages
	}
	last := gjson.ParseBytes(messages[len(messages)-1])
	if !strings.EqualFold(strings.TrimSpace(last.Get("role").String()), "assistant") {
		return messages
	}
	content := last.Get("content")
	if content.Type == gjson.String {
		return messages[:len(messages)-1]
	}
	if content.IsArray() {
		for _, part := range content.Array() {
			if strings.TrimSpace(part.Get("type").String()) == "tool_use" {
				return messages
			}
		}
		return messages[:len(messages)-1]
	}
	return messages
}

func convertResponsesReasoningToClaudeThinking(item gjson.Result) []byte {
	signature := item.Get("encrypted_content").String()
	if signature == "" {
		return nil
	}

	thinkingText := responsesReasoningSummaryText(item)
	if thinkingText == "" {
		thinkingText = responsesReasoningText(item)
	}
	thinkingPart := []byte(`{"type":"thinking","thinking":"","signature":""}`)
	thinkingPart, _ = sjson.SetBytes(thinkingPart, "thinking", thinkingText)
	thinkingPart, _ = sjson.SetBytes(thinkingPart, "signature", signature)
	return thinkingPart
}

func responsesReasoningText(item gjson.Result) string {
	if text := responsesReasoningPartsText(item.Get("summary")); text != "" {
		return text
	}
	return responsesReasoningPartsText(item.Get("content"))
}

func responsesReasoningPartsText(parts gjson.Result) string {
	if !parts.Exists() || !parts.IsArray() {
		return ""
	}
	var builder strings.Builder
	parts.ForEach(func(_, part gjson.Result) bool {
		if text := part.Get("text"); text.Exists() {
			builder.WriteString(text.String())
		} else if part.Type == gjson.String {
			builder.WriteString(part.String())
		}
		return true
	})
	return builder.String()
}

func responsesReasoningSummaryText(item gjson.Result) string {
	var builder strings.Builder
	if summary := item.Get("summary"); summary.Exists() && summary.IsArray() {
		summary.ForEach(func(_, part gjson.Result) bool {
			if text := part.Get("text"); text.Exists() {
				builder.WriteString(text.String())
			} else if part.Type == gjson.String {
				builder.WriteString(part.String())
			}
			return true
		})
	}
	return builder.String()
}

// claudeThinkingSeparatorForToolUse returns the most recent thinking block when
// the buffered assistant content ends with a server tool result, so the
// tool-use run that follows keeps a thinking block of its own. Upstreams that
// enforce Anthropic's thinking replay rules reject a tool_use glued directly
// onto a web_search_tool_result, while native Claude output always carries a
// fresh thinking block before the continued segment.
// Ported from upstream CLIProxyAPI commit fbc8e443.
func claudeThinkingSeparatorForToolUse(parts [][]byte) []byte {
	if len(parts) == 0 {
		return nil
	}
	if gjson.GetBytes(parts[len(parts)-1], "type").String() != "web_search_tool_result" {
		return nil
	}
	for index := len(parts) - 1; index >= 0; index-- {
		if gjson.GetBytes(parts[index], "type").String() == "thinking" {
			return parts[index]
		}
	}
	return nil
}

func applyResponsesToolResultContent(toolResult []byte, output gjson.Result) []byte {
	if output.Type == gjson.String {
		toolResult, _ = sjson.SetBytes(toolResult, "content", output.String())
		return toolResult
	}
	if output.IsArray() {
		var partsJSON [][]byte
		valid := true
		output.ForEach(func(_, part gjson.Result) bool {
			partJSON := convertResponsesToolResultContentPartToClaude(part)
			if len(partJSON) == 0 {
				valid = false
				return false
			}
			partsJSON = append(partsJSON, partJSON)
			return true
		})
		if valid {
			toolResult, _ = sjson.SetRawBytes(toolResult, "content", common.JoinRawArray(partsJSON))
			return toolResult
		}
	}
	toolResult, _ = sjson.SetBytes(toolResult, "content", compactResponsesJSONText(output))
	return toolResult
}

func convertResponsesToolResultContentPartToClaude(part gjson.Result) []byte {
	if !part.IsObject() {
		return nil
	}

	switch part.Get("type").String() {
	case "input_text", "output_text":
		if text := part.Get("text"); text.Type == gjson.String {
			contentPart := []byte(`{"type":"text","text":""}`)
			contentPart, _ = sjson.SetBytes(contentPart, "text", text.String())
			return contentPart
		}
	case "input_image":
		url := part.Get("image_url").String()
		if url == "" {
			url = part.Get("url").String()
		}
		if url == "" {
			return nil
		}
		if strings.HasPrefix(url, "data:") {
			mediaType, data, ok := parseResponsesBase64DataURL(url)
			if !ok || !validClaudeImageMediaType(mediaType) {
				return nil
			}
			contentPart := []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
			contentPart, _ = sjson.SetBytes(contentPart, "source.media_type", mediaType)
			contentPart, _ = sjson.SetBytes(contentPart, "source.data", data)
			return contentPart
		}
		contentPart := []byte(`{"type":"image","source":{"type":"url","url":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "source.url", url)
		return contentPart
	case "input_file":
		fileData := part.Get("file_data").String()
		if fileData == "" {
			return nil
		}
		mediaType := part.Get("media_type").String()
		data := fileData
		if strings.HasPrefix(fileData, "data:") {
			var ok bool
			mediaType, data, ok = parseResponsesBase64DataURL(fileData)
			if !ok {
				return nil
			}
		} else if mediaType == "" && strings.HasSuffix(strings.ToLower(part.Get("filename").String()), ".pdf") {
			mediaType = "application/pdf"
		}
		if mediaType != "application/pdf" || !validResponsesBase64(data) {
			return nil
		}
		contentPart := []byte(`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "source.data", data)
		return contentPart
	case "text":
		if part.Get("text").Type == gjson.String {
			return compactResponsesJSON([]byte(part.Raw))
		}
	case "image":
		if validClaudeToolResultImageSource(part.Get("source")) {
			return compactResponsesJSON([]byte(part.Raw))
		}
	case "document":
		if validClaudeToolResultDocumentSource(part.Get("source")) {
			return compactResponsesJSON([]byte(part.Raw))
		}
	case "search_result":
		if validClaudeToolResultSearchResult(part) {
			return compactResponsesJSON([]byte(part.Raw))
		}
	case "tool_reference":
		if part.Get("tool_name").Type == gjson.String && part.Get("tool_name").String() != "" {
			return compactResponsesJSON([]byte(part.Raw))
		}
	}
	return nil
}

// claudeMessageInvariantProblems reports shapes Anthropic rejects. It only
// describes; repairClaudeToolPairing already fixes the cases it knows about,
// so anything reported here is a new history shape worth investigating from
// the proxy log instead of from an upstream 400.
// Ported from upstream CLIProxyAPI commit 2bcebaa89c98 ("repair tool call
// pairing and handle standalone tool outputs").
func claudeMessageInvariantProblems(messages [][]byte) []string {
	var problems []string
	if len(messages) > 0 && gjson.GetBytes(messages[0], "role").String() != "user" {
		problems = append(problems, "first message is not user")
	}
	for i, msg := range messages {
		role := gjson.GetBytes(msg, "role").String()
		content := gjson.GetBytes(msg, "content")
		switch role {
		case "assistant":
			var toolUseIDs []string
			content.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "tool_use" {
					toolUseIDs = append(toolUseIDs, block.Get("id").String())
				}
				return true
			})
			if len(toolUseIDs) == 0 {
				continue
			}
			if i+1 >= len(messages) || gjson.GetBytes(messages[i+1], "role").String() != "user" {
				problems = append(problems, "messages["+strconv.Itoa(i)+"] tool_use has no following user message")
				continue
			}
			next := gjson.GetBytes(messages[i+1], "content")
			answered := map[string]struct{}{}
			next.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "tool_result" {
					answered[block.Get("tool_use_id").String()] = struct{}{}
				}
				return true
			})
			for _, id := range toolUseIDs {
				if _, ok := answered[id]; !ok {
					problems = append(problems, "messages["+strconv.Itoa(i)+"] tool_use "+id+" has no tool_result in messages["+strconv.Itoa(i+1)+"]")
				}
			}
		case "user":
			var previous gjson.Result
			if i > 0 {
				previous = gjson.GetBytes(messages[i-1], "content")
			}
			toolUses := map[string]struct{}{}
			previous.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "tool_use" {
					toolUses[block.Get("id").String()] = struct{}{}
				}
				return true
			})
			leading := true
			content.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "tool_result" {
					if !leading {
						problems = append(problems, "messages["+strconv.Itoa(i)+"] tool_result after non-tool_result block")
					}
					if _, ok := toolUses[block.Get("tool_use_id").String()]; !ok {
						problems = append(problems, "messages["+strconv.Itoa(i)+"] tool_result "+block.Get("tool_use_id").String()+" has no tool_use in the previous message")
					}
				} else {
					leading = false
				}
				return true
			})
		}
	}
	return problems
}

// repairClaudeToolPairing enforces the Anthropic invariant that every
// assistant tool_use is answered by a tool_result at the start of the very
// next user message, and that every tool_result references a tool_use in the
// immediately preceding assistant message. Histories break it in practice
// when a session dies while a tool runs (tool_use with no output), when
// standalone context is injected ahead of a real tool output (text before
// tool_result), or when a delayed output lands after an intervening
// assistant message. Missing results are synthesized as errors and orphan
// results fold into plain text so the model can carry on.
func repairClaudeToolPairing(messages [][]byte) [][]byte {
	if len(messages) == 0 {
		return messages
	}
	prevToolUseIDs := map[string]struct{}{}
	out := make([][]byte, 0, len(messages)+1)
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		role := gjson.GetBytes(msg, "role").String()
		content := gjson.GetBytes(msg, "content")

		if role == "user" {
			// Fold tool_result blocks that do not answer a tool_use in the
			// immediately preceding assistant message into plain text, and move
			// the real ones ahead of any other content.
			rebuilt, changed := normalizeClaudeToolResultMessage(msg, prevToolUseIDs)
			if changed {
				msg = rebuilt
				content = gjson.GetBytes(msg, "content")
				if i < len(messages) {
					messages[i] = msg
				}
			}
		}

		out = append(out, msg)

		prevToolUseIDs = map[string]struct{}{}
		if role == "assistant" {
			var toolUseIDs []string
			content.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "tool_use" {
					if id := block.Get("id").String(); id != "" {
						toolUseIDs = append(toolUseIDs, id)
						prevToolUseIDs[id] = struct{}{}
					}
				}
				return true
			})
			if len(toolUseIDs) == 0 {
				continue
			}

			hasNextUser := i+1 < len(messages) && gjson.GetBytes(messages[i+1], "role").String() == "user"
			answered := map[string]struct{}{}
			if hasNextUser {
				gjson.GetBytes(messages[i+1], "content").ForEach(func(_, block gjson.Result) bool {
					if block.Get("type").String() == "tool_result" {
						answered[block.Get("tool_use_id").String()] = struct{}{}
					}
					return true
				})
			}

			var synthesized [][]byte
			for _, id := range toolUseIDs {
				if _, ok := answered[id]; ok {
					continue
				}
				part := []byte(`{"type":"tool_result","tool_use_id":"","is_error":true,"content":"Tool call was interrupted before any output was recorded."}`)
				part, _ = sjson.SetBytes(part, "tool_use_id", id)
				synthesized = append(synthesized, part)
			}
			if len(synthesized) == 0 {
				continue
			}

			if hasNextUser {
				// Prepend the missing results so tool_result blocks still lead
				// the existing user message.
				next := messages[i+1]
				nextContent := gjson.GetBytes(next, "content")
				parts := synthesized
				if nextContent.IsArray() {
					nextContent.ForEach(func(_, block gjson.Result) bool {
						parts = append(parts, []byte(block.Raw))
						return true
					})
				} else if nextContent.Type == gjson.String {
					textPart := []byte(`{"type":"text","text":""}`)
					textPart, _ = sjson.SetBytes(textPart, "text", nextContent.String())
					parts = append(parts, textPart)
				}
				userMsg := []byte(`{"role":"user","content":[]}`)
				userMsg, _ = sjson.SetRawBytes(userMsg, "content", common.JoinRawArray(parts))
				messages[i+1] = userMsg
			} else {
				userMsg := []byte(`{"role":"user","content":[]}`)
				userMsg, _ = sjson.SetRawBytes(userMsg, "content", common.JoinRawArray(synthesized))
				out = append(out, userMsg)
			}
		}
	}
	return out
}

// normalizeClaudeToolResultMessage rebuilds a user message so tool_result
// blocks lead the content and any tool_result that does not answer a tool_use
// in the immediately preceding assistant message folds into plain text.
// answeredIDs lists the ids allowed to stay tool_result blocks.
func normalizeClaudeToolResultMessage(msg []byte, answeredIDs map[string]struct{}) ([]byte, bool) {
	content := gjson.GetBytes(msg, "content")
	if !content.IsArray() {
		return msg, false
	}
	var resultParts, otherParts [][]byte
	seenOther := false
	changed := false
	content.ForEach(func(_, block gjson.Result) bool {
		raw := []byte(block.Raw)
		if block.Get("type").String() == "tool_result" {
			if _, ok := answeredIDs[block.Get("tool_use_id").String()]; !ok {
				changed = true
				seenOther = true
				if textParts := toolResultTextParts(block); len(textParts) > 0 {
					otherParts = append(otherParts, textParts...)
				} else {
					// An empty orphan result folds to nothing, so keep an
					// explicit marker instead of producing an empty user
					// message that Anthropic also rejects.
					otherParts = append(otherParts, []byte(`{"type":"text","text":"Tool result was empty."}`))
				}
				return true
			}
			resultParts = append(resultParts, raw)
			if seenOther {
				changed = true
			}
			return true
		}
		seenOther = true
		otherParts = append(otherParts, raw)
		return true
	})
	if !changed {
		return msg, false
	}
	parts := make([][]byte, 0, len(resultParts)+len(otherParts))
	parts = append(parts, resultParts...)
	parts = append(parts, otherParts...)
	userMsg := []byte(`{"role":"user","content":[]}`)
	userMsg, _ = sjson.SetRawBytes(userMsg, "content", common.JoinRawArray(parts))
	return userMsg, true
}

// toolResultTextParts folds an orphan tool_result block into plain text parts
// so it can ride along as ordinary user content. Empty text parts are
// filtered out; when nothing visible remains it returns nil so the caller
// can substitute a marker instead of emitting empty text blocks that
// Anthropic rejects.
func toolResultTextParts(block gjson.Result) [][]byte {
	content := block.Get("content")
	if content.IsArray() {
		var parts [][]byte
		content.ForEach(func(_, part gjson.Result) bool {
			raw := []byte(part.Raw)
			if part.Get("type").String() == "" {
				raw, _ = sjson.SetBytes(raw, "type", "text")
			}
			if !contentPartHasVisibleContent(raw) {
				return true
			}
			parts = append(parts, raw)
			return true
		})
		if len(parts) > 0 {
			return parts
		}
		return nil
	}
	text := content.String()
	if strings.TrimSpace(text) == "" {
		return nil
	}
	part := []byte(`{"type":"text","text":""}`)
	part, _ = sjson.SetBytes(part, "text", text)
	return [][]byte{part}
}

// convertResponsesStandaloneToolOutputToClaudeText renders a tool output that
// has no matching tool_use as ordinary user content blocks.
func convertResponsesStandaloneToolOutputToClaudeText(output gjson.Result) [][]byte {
	if output.Exists() && output.IsArray() {
		var partsJSON [][]byte
		output.ForEach(func(_, part gjson.Result) bool {
			if partJSON := convertResponsesToolResultContentPartToClaude(part); len(partJSON) > 0 {
				// Drop empty text parts so a mixed array never emits blocks
				// Anthropic rejects; images and documents always count.
				if !contentPartHasVisibleContent(partJSON) {
					return true
				}
				partsJSON = append(partsJSON, partJSON)
			}
			return true
		})
		if len(partsJSON) > 0 {
			return partsJSON
		}
	}
	text := output.String()
	if output.IsArray() || strings.TrimSpace(text) == "" {
		// An empty standalone output still needs a block so the user message
		// it joins never degenerates to an empty content array. The IsArray
		// guard keeps the raw array dump from leaking in as literal text when
		// every converted part was empty.
		return [][]byte{[]byte(`{"type":"text","text":"Tool result was empty."}`)}
	}
	contentPart := []byte(`{"type":"text","text":""}`)
	contentPart, _ = sjson.SetBytes(contentPart, "text", text)
	return [][]byte{contentPart}
}

// contentPartHasVisibleContent reports whether a converted Claude content
// block carries user-visible payload (non-empty text, image, or document).
func contentPartHasVisibleContent(part []byte) bool {
	switch gjson.GetBytes(part, "type").String() {
	case "text":
		return strings.TrimSpace(gjson.GetBytes(part, "text").String()) != ""
	default:
		// Non-text blocks (image, document, ...) always carry content.
		return true
	}
}

func parseResponsesBase64DataURL(value string) (string, string, bool) {
	mediaAndData := strings.SplitN(strings.TrimPrefix(value, "data:"), ";base64,", 2)
	if len(mediaAndData) != 2 || mediaAndData[0] == "" || !validResponsesBase64(mediaAndData[1]) {
		return "", "", false
	}
	return mediaAndData[0], mediaAndData[1], true
}

func validResponsesBase64(data string) bool {
	if data == "" {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(data)
	return err == nil
}

func validClaudeImageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func validClaudeToolResultImageSource(source gjson.Result) bool {
	if !source.IsObject() {
		return false
	}
	switch source.Get("type").String() {
	case "base64":
		return validClaudeImageMediaType(source.Get("media_type").String()) &&
			source.Get("data").Type == gjson.String && validResponsesBase64(source.Get("data").String())
	case "url":
		return source.Get("url").Type == gjson.String && source.Get("url").String() != ""
	default:
		return false
	}
}

func validClaudeToolResultDocumentSource(source gjson.Result) bool {
	if !source.IsObject() {
		return false
	}
	switch source.Get("type").String() {
	case "base64":
		return source.Get("media_type").String() == "application/pdf" &&
			source.Get("data").Type == gjson.String && validResponsesBase64(source.Get("data").String())
	case "url":
		return source.Get("url").Type == gjson.String && source.Get("url").String() != ""
	case "text":
		return source.Get("media_type").String() == "text/plain" && source.Get("data").Type == gjson.String
	case "content":
		content := source.Get("content")
		return content.Type == gjson.String || validClaudeToolResultDocumentContent(content)
	default:
		return false
	}
}

func validClaudeToolResultDocumentContent(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	valid := true
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "text":
			valid = block.IsObject() && block.Get("text").Type == gjson.String
		case "image":
			valid = block.IsObject() && validClaudeToolResultImageSource(block.Get("source"))
		default:
			valid = false
		}
		return valid
	})
	return valid
}

func validClaudeToolResultSearchResult(part gjson.Result) bool {
	return part.Get("source").Type == gjson.String && part.Get("source").String() != "" &&
		part.Get("title").Type == gjson.String && part.Get("title").String() != "" &&
		validClaudeToolResultTextBlocks(part.Get("content"))
}

func validClaudeToolResultTextBlocks(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	valid := true
	content.ForEach(func(_, block gjson.Result) bool {
		valid = block.IsObject() && block.Get("type").String() == "text" && block.Get("text").Type == gjson.String
		return valid
	})
	return valid
}

func compactResponsesJSONText(value gjson.Result) string {
	raw := strings.TrimSpace(value.Raw)
	if raw == "" {
		return value.String()
	}
	return string(compactResponsesJSON([]byte(raw)))
}

func compactResponsesJSON(raw []byte) []byte {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return raw
	}
	return compact.Bytes()
}

// responsesToolNameMap builds short-name -> qualified-name aliases for tool
// resolution (tool_choice, replayed calls). Winning direct declarations always
// claim their own name; namespace children contribute their local name as an
// alias only when no winning direct tool already owns it.
func responsesToolNameMap(root gjson.Result, acceptedToolNames map[string]struct{}) map[string]string {
	toolNameMap := map[string]string{}
	descriptors := util.CollectResponsesToolDescriptors(root)
	winners := util.CollectResponsesToolWinners(root)

	// Direct tool names are canonical aliases and must win over namespace
	// child aliases, regardless of declaration order.
	for _, descriptor := range descriptors {
		winner, ok := winners[descriptor.Name]
		if !ok || winner.Order != descriptor.Order || !descriptor.Direct {
			continue
		}
		if _, accepted := acceptedToolNames[descriptor.Name]; !accepted {
			continue
		}
		toolNameMap[descriptor.Name] = descriptor.Name
	}

	// Namespace aliases fill only names that are not already owned by a
	// winning direct function/custom tool.
	for _, descriptor := range descriptors {
		winner, ok := winners[descriptor.Name]
		if !ok || winner.Order != descriptor.Order || descriptor.Direct || descriptor.LocalName == "" {
			continue
		}
		if _, accepted := acceptedToolNames[descriptor.Name]; !accepted {
			continue
		}
		if _, exists := toolNameMap[descriptor.LocalName]; exists {
			continue
		}
		toolNameMap[descriptor.LocalName] = descriptor.Name
	}
	return toolNameMap
}

// responsesCustomToolNamesFromWinners derives the qualified names whose winning
// declaration is a Responses custom tool, so a replayed Claude tool_use can be
// classified back into custom_tool_call. It takes precomputed winners so a
// response stream does not rebuild them per chunk.
func responsesCustomToolNamesFromWinners(winners map[string]util.ResponsesToolDescriptor) map[string]struct{} {
	names := make(map[string]struct{}, len(winners))
	for name, descriptor := range winners {
		if descriptor.ToolType == "custom" && !isOpenAIResponsesApplyPatchCustomTool(descriptor.ToolType, descriptor.Tool) {
			names[name] = struct{}{}
		}
	}
	return names
}

// responsesCustomToolNames returns the custom tool-name set for a request body.
func responsesCustomToolNames(requestRawJSON []byte) map[string]struct{} {
	if len(requestRawJSON) == 0 || !gjson.ValidBytes(requestRawJSON) {
		return make(map[string]struct{})
	}
	return responsesCustomToolNamesFromWinners(util.CollectResponsesToolWinners(gjson.ParseBytes(requestRawJSON)))
}

// isOpenAIResponsesApplyPatchCustomTool reports whether a Responses tool entry
// is the built-in apply_patch custom tool, which Claude does not accept as a
// declared tool.
func isOpenAIResponsesApplyPatchCustomTool(toolType string, tool gjson.Result) bool {
	return toolType == "custom" && strings.TrimSpace(tool.Get("name").String()) == "apply_patch"
}

// unwrapCustomToolInput extracts the freeform string a Claude tool_use carries
// under input.input back into the Responses custom_tool_call input. A
// truncated JSON wrapper (the stream ended mid-arguments) still yields the
// partial string.
func unwrapCustomToolInput(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if v := gjson.Get(trimmed, "input"); v.Exists() {
		if v.Type == gjson.String {
			return v.String()
		}
		return v.Raw
	}
	idx := strings.Index(trimmed, `"input"`)
	if idx >= 0 {
		rest := strings.TrimSpace(trimmed[idx+7:])
		if strings.HasPrefix(rest, ":") {
			rest = strings.TrimSpace(rest[1:])
			if strings.HasPrefix(rest, `"`) {
				content := rest[1:]
				var unescaped strings.Builder
				inEscape := false
				for i := 0; i < len(content); i++ {
					c := content[i]
					if inEscape {
						switch c {
						case '"', '\\', '/':
							unescaped.WriteByte(c)
						case 'b':
							unescaped.WriteByte('\b')
						case 'f':
							unescaped.WriteByte('\f')
						case 'n':
							unescaped.WriteByte('\n')
						case 'r':
							unescaped.WriteByte('\r')
						case 't':
							unescaped.WriteByte('\t')
						case 'u':
							if i+4 < len(content) {
								if r, err := strconv.ParseUint(content[i+1:i+5], 16, 16); err == nil {
									if utf16.IsSurrogate(rune(r)) && i+10 < len(content) && content[i+5:i+7] == `\u` {
										if r2, err2 := strconv.ParseUint(content[i+7:i+11], 16, 16); err2 == nil {
											unescaped.WriteRune(utf16.DecodeRune(rune(r), rune(r2)))
											i += 10
											inEscape = false
											continue
										}
									}
									unescaped.WriteRune(rune(r))
									i += 4
									inEscape = false
									continue
								}
							}
							unescaped.WriteByte('\\')
							unescaped.WriteByte('u')
						default:
							unescaped.WriteByte('\\')
							unescaped.WriteByte(c)
						}
						inEscape = false
					} else if c == '\\' {
						inEscape = true
					} else if c == '"' {
						break
					} else {
						unescaped.WriteByte(c)
					}
				}
				if inEscape {
					unescaped.WriteByte('\\')
				}
				return unescaped.String()
			}
		}
	}
	return arguments
}

func convertResponsesFunctionToolToClaude(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := strings.TrimSpace(overrideName)
	if name == "" {
		name = responsesToolName(tool)
	}
	if name == "" {
		return nil, false
	}

	tJSON := []byte(`{"name":"","description":"","input_schema":{"type":"object","properties":{}}}`)
	tJSON, _ = sjson.SetBytes(tJSON, "name", util.SanitizeClaudeFunctionName(name))
	if d := responsesToolDescription(tool); d != "" {
		tJSON, _ = sjson.SetBytes(tJSON, "description", d)
	}
	tJSON, _ = sjson.SetRawBytes(tJSON, "input_schema", util.NormalizeClaudeToolInputSchema([]byte(responsesToolParameters(tool).Raw)))
	return tJSON, true
}

// convertResponsesCustomToolToClaude converts a Responses custom tool
// declaration into a Claude tool whose input schema accepts the freeform input
// wrapped as {"input": <string>}.
func convertResponsesCustomToolToClaude(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := strings.TrimSpace(overrideName)
	if name == "" {
		name = responsesToolName(tool)
	}
	if name == "" {
		return nil, false
	}

	tJSON := []byte(`{"name":"","description":"","input_schema":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}`)
	tJSON, _ = sjson.SetBytes(tJSON, "name", util.SanitizeClaudeFunctionName(name))
	if description := responsesToolDescription(tool); description != "" {
		tJSON, _ = sjson.SetBytes(tJSON, "description", description)
	}
	return tJSON, true
}

func convertResponsesWebSearchToolToClaude(tool gjson.Result) ([]byte, bool) {
	if externalWebAccess := tool.Get("external_web_access"); externalWebAccess.Exists() && !externalWebAccess.Bool() {
		return nil, false
	}

	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		name = "web_search"
	}
	tJSON := []byte(`{"type":"web_search_20250305","name":""}`)
	tJSON, _ = sjson.SetBytes(tJSON, "name", name)
	if maxUses := tool.Get("max_uses"); maxUses.Exists() {
		tJSON, _ = sjson.SetBytes(tJSON, "max_uses", maxUses.Int())
	}
	if allowedDomains := tool.Get("filters.allowed_domains"); allowedDomains.Exists() && allowedDomains.IsArray() {
		tJSON, _ = sjson.SetRawBytes(tJSON, "allowed_domains", []byte(allowedDomains.Raw))
	}
	if userLocation := tool.Get("user_location"); userLocation.Exists() && userLocation.IsObject() {
		tJSON, _ = sjson.SetRawBytes(tJSON, "user_location", []byte(userLocation.Raw))
	}
	return tJSON, true
}

func responsesToolName(tool gjson.Result) string {
	if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
		return name
	}
	return strings.TrimSpace(tool.Get("function.name").String())
}

func responsesToolDescription(tool gjson.Result) string {
	if description := tool.Get("description").String(); description != "" {
		return description
	}
	return tool.Get("function.description").String()
}

func responsesToolParameters(tool gjson.Result) gjson.Result {
	for _, path := range []string{
		"parameters",
		"parametersJsonSchema",
		"input_schema",
		"function.parameters",
		"function.parametersJsonSchema",
	} {
		if parameters := tool.Get(path); parameters.Exists() {
			return parameters
		}
	}
	return gjson.Result{}
}

// qualifyResponsesNamespaceToolName joins a namespace child name to its
// namespace; names already qualified (equal to the namespace or carrying the
// exact "namespace__" prefix, not just a shared prefix) are returned as-is.
func qualifyResponsesNamespaceToolName(namespaceName, childName string) string {
	return util.QualifyResponsesNamespaceToolName(namespaceName, childName)
}

func isUnsupportedOpenAIBuiltinToolType(toolType string) bool {
	switch toolType {
	case "image_generation", "file_search", "code_interpreter", "computer_use_preview":
		return true
	default:
		return false
	}
}
