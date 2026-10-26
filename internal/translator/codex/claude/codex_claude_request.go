// Package claude provides request translation functionality for Claude Code API compatibility.
// It handles parsing and transforming Claude Code API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Claude Code API format and the internal client's expected format.
package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sigcompat "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeRequestToCodex parses and transforms a Claude Code API request into the internal client format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the internal client.
// The function performs the following transformations:
// 1. Sets up a template with the model name and empty instructions field
// 2. Processes system messages and converts them to developer input content
// 3. Transforms message contents (text, image, tool_use, tool_result) to appropriate formats
// 4. Converts tools declarations to the expected format
// 5. Adds additional configuration parameters for the Codex API
// 6. Maps Claude thinking configuration to Codex reasoning settings
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Claude Code API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in internal client format
func ConvertClaudeRequestToCodex(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToCodex(modelName, inputRawJSON, stream, false)
}

// ConvertClaudeRequestToCodexWithCompat preserves assistant thinking blocks with
// empty or unknown-format signatures for configured compatibility endpoints.
// Ported from upstream CLIProxyAPI (codex_claude_request.go).
func ConvertClaudeRequestToCodexWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToCodex(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToCodex(modelName string, inputRawJSON []byte, _ bool, preserveEmptyThinkingBlocks bool) []byte {
	rawJSON := inputRawJSON

	template := []byte(`{"model":"","instructions":"","input":[]}`)

	rootResult := gjson.ParseBytes(rawJSON)
	toolNameMap := buildReverseMapFromClaudeOriginalToShort(rawJSON)
	template, _ = sjson.SetBytes(template, "model", modelName)

	// Process system messages and convert them to input content format.
	systemsResult := rootResult.Get("system")
	if systemsResult.Exists() {
		message := []byte(`{"type":"message","role":"developer","content":[]}`)
		contentIndex := 0

		appendSystemText := func(text string) {
			if text == "" || util.IsClaudeCodeAttributionSystemText(text) {
				return
			}

			message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), "input_text")
			message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.text", contentIndex), text)
			contentIndex++
		}

		if systemsResult.Type == gjson.String {
			appendSystemText(systemsResult.String())
		} else if systemsResult.IsArray() {
			systemResults := systemsResult.Array()
			for i := 0; i < len(systemResults); i++ {
				systemResult := systemResults[i]
				if systemResult.Get("type").String() == "text" {
					appendSystemText(systemResult.Get("text").String())
				}
			}
		}

		if contentIndex > 0 {
			template, _ = sjson.SetRawBytes(template, "input.-1", message)
		}
	}

	// Process messages and transform their contents to appropriate formats.
	messagesResult := rootResult.Get("messages")
	if messagesResult.IsArray() {
		messageResults := messagesResult.Array()

		for i := 0; i < len(messageResults); i++ {
			messageResult := messageResults[i]
			messageRole := messageResult.Get("role").String()
			if messageRole == "system" {
				messageRole = "developer"
			}

			newMessage := func() []byte {
				msg := []byte(`{"type":"message","role":"","content":[]}`)
				msg, _ = sjson.SetBytes(msg, "role", messageRole)
				return msg
			}

			message := newMessage()
			contentIndex := 0
			hasContent := false

			flushMessage := func() {
				if hasContent {
					template, _ = sjson.SetRawBytes(template, "input.-1", message)
					message = newMessage()
					contentIndex = 0
					hasContent = false
				}
			}

			appendTextContent := func(text string) {
				partType := "input_text"
				if messageRole == "assistant" {
					partType = "output_text"
				}
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), partType)
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.text", contentIndex), text)
				contentIndex++
				hasContent = true
			}

			appendImageContent := func(dataURL string) {
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), "input_image")
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.image_url", contentIndex), dataURL)
				contentIndex++
				hasContent = true
			}

			appendDocumentContent := func(dataURL string) {
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), "input_file")
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.file_data", contentIndex), dataURL)
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.filename", contentIndex), "document.pdf")
				contentIndex++
				hasContent = true
			}

			appendReasoningContent := func(part gjson.Result) {
				if messageRole != "assistant" {
					return
				}

				rawSignature := part.Get("signature").String()
				// The target decides which family may replay: a Muse envelope
				// survives for a muse-* target and is dropped for every other one,
				// and the same rule covers GPT, Claude and Grok targets (upstream
				// CLIProxyAPI 674dbe8fd1be).
				signature, ok := translatorcommon.CodexReplaySignatureForTarget(modelName, rawSignature)
				if !ok && preserveEmptyThinkingBlocks && part.Get("signature").Type == gjson.String && strings.TrimSpace(rawSignature) != "" &&
					sigcompat.DetectSignatureProviderForBlock(rawSignature, sigcompat.SignatureBlockKindClaudeThinking) == sigcompat.SignatureProviderUnknown {
					signature = rawSignature
					ok = true
				}
				if !ok {
					if preserveEmptyThinkingBlocks && strings.TrimSpace(rawSignature) == "" {
						signature = rawSignature
					} else {
						return
					}
				}

				flushMessage()
				reasoningItem := []byte(`{"type":"reasoning","summary":[],"content":null}`)
				reasoningItem, _ = sjson.SetBytes(reasoningItem, "encrypted_content", signature)
				template, _ = sjson.SetRawBytes(template, "input.-1", reasoningItem)
			}

			messageContentsResult := messageResult.Get("content")
			if messageContentsResult.IsArray() {
				messageContentResults := messageContentsResult.Array()
				for j := 0; j < len(messageContentResults); j++ {
					messageContentResult := messageContentResults[j]
					contentType := messageContentResult.Get("type").String()

					switch contentType {
					case "text":
						appendTextContent(messageContentResult.Get("text").String())
					case "thinking":
						appendReasoningContent(messageContentResult)
					case "image":
						sourceResult := messageContentResult.Get("source")
						if sourceResult.Exists() {
							data := sourceResult.Get("data").String()
							if data == "" {
								data = sourceResult.Get("base64").String()
							}
							if data != "" {
								mediaType := sourceResult.Get("media_type").String()
								if mediaType == "" {
									mediaType = sourceResult.Get("mime_type").String()
								}
								if mediaType == "" {
									mediaType = "application/octet-stream"
								}
								dataURL := fmt.Sprintf("data:%s;base64,%s", mediaType, data)
								appendImageContent(dataURL)
							}
						}
					case "document":
						sourceResult := messageContentResult.Get("source")
						if sourceResult.Get("type").String() != "base64" {
							continue
						}
						mediaType := strings.TrimSpace(sourceResult.Get("media_type").String())
						if !strings.EqualFold(mediaType, "application/pdf") {
							continue
						}
						data := sourceResult.Get("data").String()
						if data == "" {
							data = sourceResult.Get("base64").String()
						}
						if data != "" {
							appendDocumentContent(fmt.Sprintf("data:%s;base64,%s", mediaType, data))
						}
					case "tool_use":
						flushMessage()
						functionCallMessage := []byte(`{"type":"function_call"}`)
						functionCallMessage, _ = sjson.SetBytes(functionCallMessage, "call_id", shortenCodexCallIDIfNeeded(messageContentResult.Get("id").String()))
						{
							name := messageContentResult.Get("name").String()
							if short, ok := toolNameMap[name]; ok {
								name = short
							} else {
								name = shortenNameIfNeeded(name)
							}
							functionCallMessage, _ = sjson.SetBytes(functionCallMessage, "name", name)
						}
						functionCallMessage, _ = sjson.SetBytes(functionCallMessage, "arguments", messageContentResult.Get("input").Raw)
						template, _ = sjson.SetRawBytes(template, "input.-1", functionCallMessage)
					case "tool_result":
						flushMessage()
						functionCallOutputMessage := []byte(`{"type":"function_call_output"}`)
						functionCallOutputMessage, _ = sjson.SetBytes(functionCallOutputMessage, "call_id", shortenCodexCallIDIfNeeded(messageContentResult.Get("tool_use_id").String()))

						contentResult := messageContentResult.Get("content")
						if contentResult.IsArray() {
							toolResultContentIndex := 0
							toolResultContent := []byte(`[]`)
							contentResults := contentResult.Array()
							for k := 0; k < len(contentResults); k++ {
								toolResultContentType := contentResults[k].Get("type").String()
								if toolResultContentType == "image" {
									sourceResult := contentResults[k].Get("source")
									if sourceResult.Exists() {
										data := sourceResult.Get("data").String()
										if data == "" {
											data = sourceResult.Get("base64").String()
										}
										if data != "" {
											mediaType := sourceResult.Get("media_type").String()
											if mediaType == "" {
												mediaType = sourceResult.Get("mime_type").String()
											}
											if mediaType == "" {
												mediaType = "application/octet-stream"
											}
											dataURL := fmt.Sprintf("data:%s;base64,%s", mediaType, data)

											toolResultContent, _ = sjson.SetBytes(toolResultContent, fmt.Sprintf("%d.type", toolResultContentIndex), "input_image")
											toolResultContent, _ = sjson.SetBytes(toolResultContent, fmt.Sprintf("%d.image_url", toolResultContentIndex), dataURL)
											toolResultContentIndex++
										}
									}
								} else if toolResultContentType == "text" {
									toolResultContent, _ = sjson.SetBytes(toolResultContent, fmt.Sprintf("%d.type", toolResultContentIndex), "input_text")
									toolResultContent, _ = sjson.SetBytes(toolResultContent, fmt.Sprintf("%d.text", toolResultContentIndex), contentResults[k].Get("text").String())
									toolResultContentIndex++
								}
							}
							if toolResultContentIndex > 0 {
								functionCallOutputMessage, _ = sjson.SetRawBytes(functionCallOutputMessage, "output", toolResultContent)
							} else {
								functionCallOutputMessage, _ = sjson.SetBytes(functionCallOutputMessage, "output", messageContentResult.Get("content").String())
							}
						} else {
							functionCallOutputMessage, _ = sjson.SetBytes(functionCallOutputMessage, "output", messageContentResult.Get("content").String())
						}

						template, _ = sjson.SetRawBytes(template, "input.-1", functionCallOutputMessage)
					}
				}
				flushMessage()
			} else if messageContentsResult.Type == gjson.String {
				appendTextContent(messageContentsResult.String())
				flushMessage()
			}
		}

	}

	// Convert tools declarations to the expected format for the Codex API.
	toolsResult := rootResult.Get("tools")
	if toolsResult.IsArray() {
		template, _ = sjson.SetRawBytes(template, "tools", []byte(`[]`))
		webSearchToolNames := buildClaudeWebSearchToolNameSet(toolsResult)
		template, _ = sjson.SetRawBytes(template, "tool_choice", convertClaudeToolChoiceToCodex(rootResult.Get("tool_choice"), toolNameMap, webSearchToolNames))
		toolResults := toolsResult.Array()
		for i := 0; i < len(toolResults); i++ {
			toolResult := toolResults[i]
			// Special handling: map Claude web search tool to Codex web_search
			if isClaudeWebSearchToolType(toolResult.Get("type").String()) {
				template, _ = sjson.SetRawBytes(template, "tools.-1", convertClaudeWebSearchToolToCodex(toolResult))
				continue
			}
			tool := []byte(toolResult.Raw)
			tool, _ = sjson.SetBytes(tool, "type", "function")
			// Apply shortened name if needed
			if v := toolResult.Get("name"); v.Exists() {
				name := v.String()
				if short, ok := toolNameMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				tool, _ = sjson.SetBytes(tool, "name", name)
			}
			tool, _ = sjson.SetRawBytes(tool, "parameters", []byte(normalizeToolParameters(toolResult.Get("input_schema").Raw)))
			tool, _ = sjson.DeleteBytes(tool, "input_schema")
			tool, _ = sjson.DeleteBytes(tool, "parameters.$schema")
			tool, _ = sjson.DeleteBytes(tool, "cache_control")
			tool, _ = sjson.DeleteBytes(tool, "defer_loading")
			tool, _ = sjson.SetBytes(tool, "strict", false)
			template, _ = sjson.SetRawBytes(template, "tools.-1", tool)
		}
	}

	// Default to parallel tool calls unless tool_choice explicitly disables them.
	parallelToolCalls := true
	if disableParallelToolUse := rootResult.Get("tool_choice.disable_parallel_tool_use"); disableParallelToolUse.Exists() {
		parallelToolCalls = !disableParallelToolUse.Bool()
	}

	// Add additional configuration parameters for the Codex API.
	template, _ = sjson.SetBytes(template, "parallel_tool_calls", parallelToolCalls)

	// Convert Claude thinking budgets and effort to reasoning.effort.
	reasoningEffort := "medium"
	effort := ""
	if v := rootResult.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
		effort = strings.ToLower(strings.TrimSpace(v.String()))
	}
	if thinkingConfig := rootResult.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		switch thinkingConfig.Get("type").String() {
		case "enabled":
			if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
				budget := int(budgetTokens.Int())
				if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
					reasoningEffort = effort
				}
			}
		case "adaptive", "auto":
			// Adaptive thinking can carry an explicit effort in output_config.effort (Claude 4.6).
			// Pass through directly; ApplyThinking handles clamping to target model's levels.
			if effort != "" {
				reasoningEffort = effort
			} else {
				reasoningEffort = string(thinking.LevelXHigh)
			}
		case "disabled":
			if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
				reasoningEffort = effort
			}
		}
	} else if !thinkingConfig.Exists() && effort != "" {
		// A standalone output_config.effort (no thinking object) is still an
		// explicit request; thinking semantics above always take precedence.
		reasoningEffort = effort
	}
	template, _ = sjson.SetBytes(template, "reasoning.effort", reasoningEffort)
	template, _ = sjson.SetBytes(template, "reasoning.summary", "auto")
	serviceTier := normalizeCodexServiceTier(rootResult.Get("service_tier"))
	if speed := rootResult.Get("speed"); speed.Type == gjson.String && speed.String() == "fast" {
		serviceTier = "priority"
	}
	if serviceTier != "" {
		template, _ = sjson.SetBytes(template, "service_tier", serviceTier)
	}
	template, _ = sjson.SetBytes(template, "stream", true)
	template, _ = sjson.SetBytes(template, "store", false)
	template, _ = sjson.SetBytes(template, "include", []string{"reasoning.encrypted_content"})

	if format := rootResult.Get("output_config.format"); format.IsObject() && format.Get("type").String() == "json_schema" && format.Get("schema").IsObject() {
		name := "cli_proxy_structured_output"
		if n := format.Get("name").String(); n != "" {
			name = n
		}
		strict := true
		if s := format.Get("strict"); s.Exists() && s.Type == gjson.False {
			strict = false
		}
		// OpenAI strict mode requires every declared property to be listed
		// in required. Downgrade instead of emitting an unsatisfiable
		// strict schema that strict backends reject with HTTP 400.
		if strict && codexSchemaMissesRequired(format.Get("schema")) {
			strict = false
		}
		translatedFormat := []byte(`{"type":"json_schema","name":"","strict":true,"schema":{}}`)
		translatedFormat, _ = sjson.SetBytes(translatedFormat, "name", name)
		translatedFormat, _ = sjson.SetBytes(translatedFormat, "strict", strict)
		translatedFormat, _ = sjson.SetRawBytes(translatedFormat, "schema", []byte(format.Get("schema").Raw))
		template, _ = sjson.SetRawBytes(template, "text.format", translatedFormat)
	}

	return template
}

// shortenCodexCallIDIfNeeded keeps Claude tool IDs within the OpenAI Responses
// API call_id limit while preserving a stable, low-collision mapping.
func shortenCodexCallIDIfNeeded(id string) string {
	const limit = 64
	if len(id) <= limit {
		return id
	}

	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLen := limit - len(suffix)
	if prefixLen <= 0 {
		return suffix[len(suffix)-limit:]
	}
	return id[:prefixLen] + suffix
}

func isClaudeWebSearchToolType(toolType string) bool {
	return toolType == "web_search_20250305" || toolType == "web_search_20260209"
}

func buildClaudeWebSearchToolNameSet(tools gjson.Result) map[string]struct{} {
	names := map[string]struct{}{}
	if !tools.IsArray() {
		return names
	}

	tools.ForEach(func(_, tool gjson.Result) bool {
		toolType := tool.Get("type").String()
		if !isClaudeWebSearchToolType(toolType) {
			return true
		}

		if name := tool.Get("name").String(); name != "" {
			names[name] = struct{}{}
		}
		return true
	})

	return names
}

func convertClaudeToolChoiceToCodex(toolChoice gjson.Result, toolNameMap map[string]string, webSearchToolNames map[string]struct{}) []byte {
	if !toolChoice.Exists() || toolChoice.Type == gjson.Null {
		return []byte(`"auto"`)
	}

	choiceType := toolChoice.Get("type").String()
	if choiceType == "" && toolChoice.Type == gjson.String {
		choiceType = toolChoice.String()
	}

	switch choiceType {
	case "auto", "":
		return []byte(`"auto"`)
	case "any":
		return []byte(`"required"`)
	case "none":
		return []byte(`"none"`)
	case "tool":
		name := toolChoice.Get("name").String()
		if _, ok := webSearchToolNames[name]; ok {
			return []byte(`{"type":"web_search"}`)
		}
		if short, ok := toolNameMap[name]; ok {
			name = short
		} else {
			name = shortenNameIfNeeded(name)
		}
		if name == "" {
			return []byte(`"auto"`)
		}

		choice := []byte(`{"type":"function","name":""}`)
		choice, _ = sjson.SetBytes(choice, "name", name)
		return choice
	default:
		return []byte(`"auto"`)
	}
}

func normalizeCodexServiceTier(result gjson.Result) string {
	if !result.Exists() || result.Type != gjson.String {
		return ""
	}

	switch strings.ToLower(strings.TrimSpace(result.String())) {
	case "fast", "priority":
		return "priority"
	default:
		return ""
	}
}

func convertClaudeWebSearchToolToCodex(tool gjson.Result) []byte {
	out := []byte(`{"type":"web_search"}`)
	if allowedDomains := tool.Get("allowed_domains"); allowedDomains.Exists() && allowedDomains.IsArray() {
		out, _ = sjson.SetRawBytes(out, "filters.allowed_domains", []byte(allowedDomains.Raw))
	}
	if userLocation := tool.Get("user_location"); userLocation.Exists() && userLocation.IsObject() {
		out, _ = sjson.SetRawBytes(out, "user_location", []byte(userLocation.Raw))
	}
	return out
}

// shortenNameIfNeeded applies a simple shortening rule for a single name.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if strings.HasPrefix(name, "mcp__") || len(name) <= limit {
		return name
	}
	return name[:limit]
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if strings.HasPrefix(n, "mcp__") || len(n) <= limit {
			return n
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

// buildReverseMapFromClaudeOriginalToShort builds original->short map, used to map tool_use names to short.
func buildReverseMapFromClaudeOriginalToShort(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	m := map[string]string{}
	if !tools.IsArray() {
		return m
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
		m = buildShortNameMap(names)
	}
	return m
}

// normalizeToolParameters ensures object schemas contain at least an empty properties map,
// and drops regex patterns containing unsupported Unicode property escapes
// (\p{...} / \P{...}) that cause upstream schema validation failures.
func normalizeToolParameters(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !gjson.Valid(raw) {
		return `{"type":"object","properties":{}}`
	}
	raw = stripUnsupportedUnicodePatternsFromSchemaJSON(raw)
	result := gjson.Parse(raw)
	schema := []byte(raw)
	schemaType := result.Get("type").String()
	if schemaType == "" {
		schema, _ = sjson.SetBytes(schema, "type", "object")
		schemaType = "object"
	}
	if schemaType == "object" && !result.Get("properties").Exists() {
		schema, _ = sjson.SetRawBytes(schema, "properties", []byte(`{}`))
	}
	return string(schema)
}

// codexSchemaMapKeywords holds JSON Schema keywords whose values are maps of
// subschemas; codexSchemaValueKeywords holds keywords with a single nested
// schema or a list of schemas.
var codexSchemaMapKeywords = [...]string{
	"properties",
	"$defs",
	"definitions",
	"patternProperties",
	"dependentSchemas",
	"dependencies",
}

var codexSchemaValueKeywords = [...]string{
	"items",
	"prefixItems",
	"contains",
	"additionalProperties",
	"propertyNames",
	"unevaluatedProperties",
	"unevaluatedItems",
	"additionalItems",
	"contentSchema",
	"anyOf",
	"oneOf",
	"allOf",
	"not",
	"if",
	"then",
	"else",
}

// codexSchemaMissesRequired reports whether a JSON Schema has any declared
// property missing from its sibling required list (recursively). OpenAI
// strict mode rejects such schemas with HTTP 400, so callers downgrade
// text.format.strict instead of emitting an unsatisfiable schema.
// Ported from upstream CLIProxyAPI commit aa3652775225 ("downgrade strict
// mode when schema misses required properties").
func codexSchemaMissesRequired(schema gjson.Result) bool {
	if !schema.IsObject() {
		if schema.IsArray() {
			miss := false
			schema.ForEach(func(_, child gjson.Result) bool {
				if codexSchemaMissesRequired(child) {
					miss = true
					return false
				}
				return true
			})
			return miss
		}
		return false
	}
	if properties := schema.Get("properties"); properties.IsObject() {
		required := schema.Get("required")
		if !required.IsArray() {
			return len(properties.Map()) > 0
		}
		names := make(map[string]struct{}, len(required.Array()))
		for _, item := range required.Array() {
			if item.Type == gjson.String {
				names[item.String()] = struct{}{}
			}
		}
		for name := range properties.Map() {
			if _, ok := names[name]; !ok {
				return true
			}
		}
	}
	for _, keyword := range codexSchemaMapKeywords {
		children := schema.Get(keyword)
		if !children.IsObject() {
			continue
		}
		miss := false
		children.ForEach(func(_, child gjson.Result) bool {
			if codexSchemaMissesRequired(child) {
				miss = true
				return false
			}
			return true
		})
		if miss {
			return true
		}
	}
	for _, keyword := range codexSchemaValueKeywords {
		if child := schema.Get(keyword); child.Exists() && codexSchemaMissesRequired(child) {
			return true
		}
	}
	return false
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
