// Package openai provides request translation functionality for OpenAI to Gemini API compatibility.
// It converts OpenAI Chat Completions requests into Gemini compatible JSON using gjson/sjson only.
package chat_completions

import (
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/translator/gemini/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiFunctionThoughtSignature = "skip_thought_signature_validator"

// ConvertOpenAIRequestToGemini converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertOpenAIRequestToGemini(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	// Base envelope (no default thinkingConfig)
	out := []byte(`{"contents":[]}`)

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Let user-provided generationConfig pass through
	if genConfig := gjson.GetBytes(rawJSON, "generationConfig"); genConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, "generationConfig", []byte(genConfig.Raw))
	}

	// Apply thinking configuration: convert OpenAI reasoning_effort to Gemini thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := gjson.GetBytes(rawJSON, "reasoning_effort")
	if re.Exists() {
		effort := strings.ToLower(strings.TrimSpace(re.String()))
		if effort != "" {
			thinkingPath := "generationConfig.thinkingConfig"
			if effort == "auto" {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingBudget", -1)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", true)
			} else {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingLevel", effort)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", effort != "none")
			}
		}
	}

	// Temperature/top_p/top_k
	if tr := gjson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.temperature", tr.Num)
	}
	if tpr := gjson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topP", tpr.Num)
	}
	if tkr := gjson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topK", tkr.Num)
	}

	// Candidate count (OpenAI 'n' parameter)
	if n := gjson.GetBytes(rawJSON, "n"); n.Exists() && n.Type == gjson.Number {
		if val := n.Int(); val > 1 {
			out, _ = sjson.SetBytes(out, "generationConfig.candidateCount", val)
		}
	}

	// Map OpenAI response_format to Gemini structured output settings.
	out = applyOpenAIResponseFormatToGemini(out, rawJSON)

	// Map OpenAI modalities -> Gemini generationConfig.responseModalities
	// e.g. "modalities": ["image", "text"] -> ["IMAGE", "TEXT"]
	if mods := gjson.GetBytes(rawJSON, "modalities"); mods.Exists() && mods.IsArray() {
		var responseMods []string
		for _, m := range mods.Array() {
			switch strings.ToLower(m.String()) {
			case "text":
				responseMods = append(responseMods, "TEXT")
			case "image":
				responseMods = append(responseMods, "IMAGE")
			}
		}
		if len(responseMods) > 0 {
			out, _ = sjson.SetBytes(out, "generationConfig.responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	// If the input uses top-level image_config.aspect_ratio, map it into generationConfig.imageConfig.aspectRatio.
	if imgCfg := gjson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "generationConfig.imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "generationConfig.imageConfig.imageSize", size.Str)
		}
	}

	// messages -> systemInstruction + contents
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		arr := messages.Array()
		systemPartIndex := 0
		// Leading system/developer messages hoist into systemInstruction;
		// mid-session ones demote to user turns to keep the prompt-cache
		// prefix immutable.
		// Ported from upstream CLIProxyAPI commit 0fe19ede90a4.
		hasEncounteredConversation := false
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()
			content := m.Get("content")

			if (role == "system" || role == "developer") && len(arr) > 1 && !hasEncounteredConversation {
				// system -> systemInstruction as a user message style
				if content.Type == gjson.String {
					out, _ = sjson.SetBytes(out, "systemInstruction.role", "user")
					out, _ = sjson.SetBytes(out, fmt.Sprintf("systemInstruction.parts.%d.text", systemPartIndex), content.String())
					systemPartIndex++
				} else if content.IsObject() && content.Get("type").String() == "text" {
					out, _ = sjson.SetBytes(out, "systemInstruction.role", "user")
					out, _ = sjson.SetBytes(out, fmt.Sprintf("systemInstruction.parts.%d.text", systemPartIndex), content.Get("text").String())
					systemPartIndex++
				} else if content.IsArray() {
					contents := content.Array()
					if len(contents) > 0 {
						out, _ = sjson.SetBytes(out, "systemInstruction.role", "user")
						for j := 0; j < len(contents); j++ {
							out, _ = sjson.SetBytes(out, fmt.Sprintf("systemInstruction.parts.%d.text", systemPartIndex), contents[j].Get("text").String())
							systemPartIndex++
						}
					}
				}
			} else if role == "user" || role == "system" || role == "developer" {
				hasEncounteredConversation = true
				// Mid-session system/developer messages demote to user turns; wrap
				// their text in the system-reminder envelope so upstream models
				// treat them as directives (upstream b681a1e0f7b8).
				isDemotedSystem := role == "system" || role == "developer"
				// Build single user content node to avoid splitting into multiple contents
				node := []byte(`{"role":"user","parts":[]}`)
				hasParts := false
				if content.Type == gjson.String {
					node, _ = sjson.SetBytes(node, "parts.0.text", geminiDemotedSystemText(content.String(), isDemotedSystem))
					hasParts = true
				} else if content.IsObject() && content.Get("type").String() == "text" {
					node, _ = sjson.SetBytes(node, "parts.0.text", geminiDemotedSystemText(content.Get("text").String(), isDemotedSystem))
					hasParts = true
				} else if content.IsArray() {
					items := content.Array()
					p := 0
					for _, item := range items {
						switch item.Get("type").String() {
						case "text":
							text := item.Get("text").String()
							if text != "" {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", geminiDemotedSystemText(text, isDemotedSystem))
								hasParts = true
							}
							p++
						case "image_url":
							imageURL := item.Get("image_url.url").String()
							if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData("", "", imageURL); ok {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thoughtSignature", geminiFunctionThoughtSignature)
								hasParts = true
								p++
							}
						case "file":
							filename := item.Get("file.filename").String()
							fileData := item.Get("file.file_data").String()
							if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData(filename, "", fileData); ok {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
								hasParts = true
								p++
							} else {
								log.Warn("Invalid file data or unknown file name extension in user message, skip")
							}
						}
					}
				}
				// Guard against empty parts (e.g. content carried only
				// unsupported item types). Ported from upstream 0fe19ede90a4.
				if hasParts {
					out, _ = sjson.SetRawBytes(out, "contents.-1", node)
				}
			} else if role == "assistant" {
				hasEncounteredConversation = true
				node := []byte(`{"role":"model","parts":[]}`)
				p := 0
				if content.Type == gjson.String {
					// Assistant text -> single model content
					node, _ = sjson.SetBytes(node, "parts.-1.text", content.String())
					p++
				} else if content.IsArray() {
					// Assistant multimodal content (e.g. text + image) -> single model content with parts
					for _, item := range content.Array() {
						switch item.Get("type").String() {
						case "text":
							text := item.Get("text").String()
							if text != "" {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", text)
							}
							p++
						case "image_url":
							// If the assistant returned an inline data URL, preserve it for history fidelity.
							imageURL := item.Get("image_url.url").String()
							if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData("", "", imageURL); ok {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thoughtSignature", geminiFunctionThoughtSignature)
								p++
							}
						}
					}
				}

				// Tool calls -> single model content with functionCall parts.
				// Tool responses are collected per assistant turn so repeated
				// tool call IDs across turns keep the correct name/result
				// pairing (upstream c2ea2684).
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					type assistantToolCall struct {
						id   string
						name string
					}
					toolCalls := make([]assistantToolCall, 0)
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() != "function" {
							continue
						}
						fid := tc.Get("id").String()
						fname := util.SanitizeFunctionName(tc.Get("function.name").String())
						if fname == "" {
							continue
						}
						fargs := tc.Get("function.arguments").String()
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.name", fname)
						node, _ = sjson.SetRawBytes(node, "parts."+itoa(p)+".functionCall.args", []byte(fargs))
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thoughtSignature", geminiFunctionThoughtSignature)
						p++
						toolCalls = append(toolCalls, assistantToolCall{id: fid, name: fname})
					}
					out, _ = sjson.SetRawBytes(out, "contents.-1", node)

					// Collect tool responses scoped to this assistant turn.
					turnToolResponses := map[string]string{}
					for j := i + 1; j < len(arr); j++ {
						nextRole := arr[j].Get("role").String()
						if nextRole == "assistant" {
							break
						}
						if nextRole == "tool" {
							callID := arr[j].Get("tool_call_id").String()
							if callID != "" {
								c := arr[j].Get("content")
								turnToolResponses[callID] = c.Raw
							}
						}
					}

					// Append a single tool content combining name + response per function
					toolNode := []byte(`{"role":"user","parts":[]}`)
					pp := 0
					for _, call := range toolCalls {
						toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.name", call.name)
						resp := turnToolResponses[call.id]
						if resp == "" {
							resp = "{}"
						}
						toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.response.result", []byte(resp))
						pp++
					}
					if pp > 0 {
						out, _ = sjson.SetRawBytes(out, "contents.-1", toolNode)
					}
				} else {
					out, _ = sjson.SetRawBytes(out, "contents.-1", node)
				}
			}
		}
	}

	// tools -> tools[].functionDeclarations + tools[].googleSearch/codeExecution/urlContext passthrough
	// tool_choice "allowed_tools" subset filtering and strict/VALIDATED mapping
	// ported from upstream CLIProxyAPI (f247e2b0, 49eec664).
	allowedToolNames := make(map[string]struct{})
	isAllowedTools := false
	allowedMode := "auto"
	if toolChoice := gjson.GetBytes(rawJSON, "tool_choice"); toolChoice.Exists() && toolChoice.IsObject() && toolChoice.Get("type").String() == "allowed_tools" {
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

	declaredOriginalToSanitized := make(map[string]string)
	sanitizedToOriginalCounts := make(map[string]int)
	var functionDeclarations [][]byte
	hasStrictTool := false
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		functionDeclarations = make([][]byte, 0, len(tools.Array()))
		googleSearchNodes := make([][]byte, 0)
		codeExecutionNodes := make([][]byte, 0)
		urlContextNodes := make([][]byte, 0)
		for _, t := range tools.Array() {
			if t.Get("type").String() == "function" {
				fn := t.Get("function")
				if fn.Exists() && fn.IsObject() {
					nameResult := fn.Get("name")
					originalName := nameResult.String()
					if isAllowedTools {
						if _, ok := allowedToolNames[originalName]; !ok {
							continue
						}
					}
					sanitizedName := util.SanitizeFunctionName(originalName)
					sanitizedToOriginalCounts[sanitizedName]++
					declaredOriginalToSanitized[originalName] = sanitizedName
					fnRaw := fn.Raw
					if fn.Get("parameters").Exists() {
						renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
						if errRename != nil {
							log.Warnf("Failed to rename parameters for tool '%s': %v", fn.Get("name").String(), errRename)
							var errSet error
							fnRawBytes := []byte(fnRaw)
							fnRawBytes, errSet = sjson.SetBytes(fnRawBytes, "parametersJsonSchema.type", "object")
							if errSet != nil {
								log.Warnf("Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet)
								continue
							}
							fnRawBytes, errSet = sjson.SetRawBytes(fnRawBytes, "parametersJsonSchema.properties", []byte(`{}`))
							if errSet != nil {
								log.Warnf("Failed to set default schema properties for tool '%s': %v", fn.Get("name").String(), errSet)
								continue
							}
							fnRaw = string(fnRawBytes)
						} else {
							fnRaw = renamed
						}
					} else {
						var errSet error
						fnRawBytes := []byte(fnRaw)
						fnRawBytes, errSet = sjson.SetBytes(fnRawBytes, "parametersJsonSchema.type", "object")
						if errSet != nil {
							log.Warnf("Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRawBytes, errSet = sjson.SetRawBytes(fnRawBytes, "parametersJsonSchema.properties", []byte(`{}`))
						if errSet != nil {
							log.Warnf("Failed to set default schema properties for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRaw = string(fnRawBytes)
					}
					fnRawBytes := []byte(fnRaw)
					if nameResult.Type != gjson.String || sanitizedName != originalName {
						fnRawBytes, _ = sjson.SetBytes(fnRawBytes, "name", sanitizedName)
					}
					// parametersJsonSchema carries a full JSON Schema contract:
					// preserve standard constraints and additionalProperties
					// (upstream b532db9c).
					if parameters := gjson.GetBytes(fnRawBytes, "parametersJsonSchema"); parameters.Exists() {
						cleanedParameters := util.CleanJSONSchemaForGeminiJSONSchema(parameters.Raw)
						if cleanedParameters != parameters.Raw {
							fnRawBytes, _ = sjson.SetRawBytes(fnRawBytes, "parametersJsonSchema", []byte(cleanedParameters))
						}
					}
					// strict flag may live on the function object or the tool
					// envelope; a strict tool maps to VALIDATED mode
					// (upstream f247e2b0).
					strictVal := gjson.GetBytes(fnRawBytes, "strict")
					if !strictVal.Exists() {
						strictVal = fn.Get("strict")
						if !strictVal.Exists() {
							strictVal = t.Get("strict")
						}
					}
					if strictVal.Exists() {
						if strictVal.Type == gjson.True {
							hasStrictTool = true
						}
						if gjson.GetBytes(fnRawBytes, "strict").Exists() {
							fnRawBytes, _ = sjson.DeleteBytes(fnRawBytes, "strict")
						}
					}
					functionDeclarations = append(functionDeclarations, fnRawBytes)
				}
			}
			if gs := t.Get("google_search"); gs.Exists() {
				googleToolNode := []byte(`{}`)
				var errSet error
				googleToolNode, errSet = sjson.SetRawBytes(googleToolNode, "googleSearch", []byte(gs.Raw))
				if errSet != nil {
					log.Warnf("Failed to set googleSearch tool: %v", errSet)
					continue
				}
				googleSearchNodes = append(googleSearchNodes, googleToolNode)
			}
			if ce := t.Get("code_execution"); ce.Exists() {
				codeToolNode := []byte(`{}`)
				var errSet error
				codeToolNode, errSet = sjson.SetRawBytes(codeToolNode, "codeExecution", []byte(ce.Raw))
				if errSet != nil {
					log.Warnf("Failed to set codeExecution tool: %v", errSet)
					continue
				}
				codeExecutionNodes = append(codeExecutionNodes, codeToolNode)
			}
			if uc := t.Get("url_context"); uc.Exists() {
				urlToolNode := []byte(`{}`)
				var errSet error
				urlToolNode, errSet = sjson.SetRawBytes(urlToolNode, "urlContext", []byte(uc.Raw))
				if errSet != nil {
					log.Warnf("Failed to set urlContext tool: %v", errSet)
					continue
				}
				urlContextNodes = append(urlContextNodes, urlToolNode)
			}
		}
		if len(functionDeclarations) > 0 || len(googleSearchNodes) > 0 || len(codeExecutionNodes) > 0 || len(urlContextNodes) > 0 {
			toolItems := make([][]byte, 0, 1+len(googleSearchNodes)+len(codeExecutionNodes)+len(urlContextNodes))
			if len(functionDeclarations) > 0 {
				functionToolNode := []byte(`{"functionDeclarations":[]}`)
				functionToolNode, _ = sjson.SetRawBytes(functionToolNode, "functionDeclarations", translatorcommon.JoinRawArray(functionDeclarations))
				toolItems = append(toolItems, functionToolNode)
			}
			toolItems = append(toolItems, googleSearchNodes...)
			toolItems = append(toolItems, codeExecutionNodes...)
			toolItems = append(toolItems, urlContextNodes...)
			out, _ = sjson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(toolItems))
			// Gemini rejects built-in tools combined with functionDeclarations
			// unless server-side tool invocations are declared.
			if len(functionDeclarations) > 0 && (len(googleSearchNodes) > 0 || len(codeExecutionNodes) > 0 || len(urlContextNodes) > 0) {
				out, _ = sjson.SetBytes(out, "toolConfig.includeServerSideToolInvocations", true)
			}
		}
	}

	hasSanitizedCollision := false
	for _, count := range sanitizedToOriginalCounts {
		if count > 1 {
			hasSanitizedCollision = true
			break
		}
	}

	// tool_choice mapping (upstream 49eec664): fail-closed when the requested
	// restriction cannot be faithfully expressed.
	if hasSanitizedCollision {
		// Ambiguous collision in function names: fail-closed to prevent invoking unintended tools.
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
	} else if isAllowedTools {
		if len(functionDeclarations) == 0 {
			// Fail-closed when no allowed tools match or subset is empty.
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		} else if allowedMode == "required" || allowedMode == "any" {
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
			allowedList := make([]string, 0, len(functionDeclarations))
			for _, fnRaw := range functionDeclarations {
				allowedList = append(allowedList, gjson.GetBytes(fnRaw, "name").String())
			}
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames", allowedList)
		} else if hasStrictTool {
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "VALIDATED")
		} else {
			// Mode AUTO: functionDeclarations contains only allowed tools, mode is AUTO without allowedFunctionNames.
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "AUTO")
		}
	} else if toolChoice := gjson.GetBytes(rawJSON, "tool_choice"); toolChoice.Exists() && toolChoice.Type != gjson.Null {
		toolChoiceType := ""
		if toolChoice.Type == gjson.String {
			toolChoiceType = strings.ToLower(strings.TrimSpace(toolChoice.String()))
		} else if toolChoice.IsObject() {
			toolChoiceType = strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String()))
		}

		switch toolChoiceType {
		case "auto":
			if hasStrictTool {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "VALIDATED")
			} else {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "AUTO")
			}
		case "none":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		case "required", "any":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
		case "function", "tool":
			fnName := strings.TrimSpace(toolChoice.Get("function.name").String())
			if fnName == "" {
				fnName = strings.TrimSpace(toolChoice.Get("name").String())
			}
			sanitized, declared := declaredOriginalToSanitized[fnName]
			if declared && sanitizedToOriginalCounts[sanitized] == 1 {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames", []string{sanitized})
			} else {
				// Missing, undeclared, or ambiguous: fail-closed.
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
			}
		default:
			// Unrecognized tool_choice type: fail-closed.
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		}
	} else if hasStrictTool && len(functionDeclarations) > 0 {
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "VALIDATED")
	}

	// parallel_tool_calls handling:
	// Gemini function calling has no parameter to disable parallel tool calls while keeping tools enabled.
	// As a safe fail-closed measure when explicit restrictions cannot be faithfully expressed,
	// when parallel_tool_calls is explicitly false, mode is set to NONE.
	if parallelToolCalls := gjson.GetBytes(rawJSON, "parallel_tool_calls"); parallelToolCalls.Type == gjson.False {
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		out, _ = sjson.DeleteBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames")
	}

	out = common.AttachDefaultSafetySettings(out, "safetySettings")

	return out
}

// applyOpenAIResponseFormatToGemini maps OpenAI Chat Completions structured output settings to Gemini.
func applyOpenAIResponseFormatToGemini(out []byte, rawJSON []byte) []byte {
	responseFormat := gjson.GetBytes(rawJSON, "response_format")
	if !responseFormat.Exists() {
		return out
	}

	switch strings.ToLower(strings.TrimSpace(responseFormat.Get("type").String())) {
	case "json_object":
		out, _ = sjson.SetBytes(out, "generationConfig.responseMimeType", "application/json")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseSchema")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseJsonSchema")
	case "json_schema":
		out, _ = sjson.SetBytes(out, "generationConfig.responseMimeType", "application/json")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseSchema")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseJsonSchema")
		if schema := responseFormat.Get("json_schema.schema"); schema.Exists() {
			out, _ = sjson.SetRawBytes(out, "generationConfig.responseJsonSchema", []byte(schema.Raw))
		}
	}

	return out
}

// itoa converts int to string without strconv import for few usages.
func itoa(i int) string { return fmt.Sprintf("%d", i) }

// geminiDemotedSystemText wraps a demoted mid-session system or developer
// message in the <system-reminder> envelope so non-Claude upstream models treat it
// as a directive rather than user speech (upstream b681a1e0f7b8).
func geminiDemotedSystemText(text string, isDemoted bool) string {
	if !isDemoted || strings.TrimSpace(text) == "" {
		return text
	}
	return translatorcommon.SystemReminderText(text)
}
