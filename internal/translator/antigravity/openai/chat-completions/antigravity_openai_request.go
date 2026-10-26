// Package openai provides request translation functionality for OpenAI to Gemini CLI API compatibility.
// It converts OpenAI Chat Completions requests into Gemini CLI compatible JSON using gjson/sjson only.
package chat_completions

import (
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/misc"
	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/translator/gemini/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiCLIFunctionThoughtSignature = "skip_thought_signature_validator"

// ConvertOpenAIRequestToAntigravity converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini CLI request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini CLI API format
func ConvertOpenAIRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	// Base envelope (no default thinkingConfig)
	out := []byte(`{"project":"","request":{"contents":[]},"model":"gemini-2.5-pro"}`)

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Let user-provided generationConfig pass through
	if genConfig := gjson.GetBytes(rawJSON, "generationConfig"); genConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, "request.generationConfig", []byte(genConfig.Raw))
	}

	// Apply thinking configuration: convert OpenAI reasoning_effort to Gemini CLI thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := gjson.GetBytes(rawJSON, "reasoning_effort")
	if re.Exists() {
		effort := strings.ToLower(strings.TrimSpace(re.String()))
		if effort != "" {
			thinkingPath := "request.generationConfig.thinkingConfig"
			if effort == "auto" {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingBudget", -1)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", true)
			} else {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingLevel", effort)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", effort != "none")
			}
		}
	}

	// Temperature/top_p/top_k/max_tokens/max_completion_tokens
	if tr := gjson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.temperature", tr.Num)
	}
	if tpr := gjson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topP", tpr.Num)
	}
	if tkr := gjson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topK", tkr.Num)
	}
	if maxTok := gjson.GetBytes(rawJSON, "max_tokens"); maxTok.Exists() && maxTok.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", maxTok.Num)
	} else if mct := gjson.GetBytes(rawJSON, "max_completion_tokens"); mct.Exists() && mct.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", mct.Num)
	}

	// Candidate count (OpenAI 'n' parameter)
	if n := gjson.GetBytes(rawJSON, "n"); n.Exists() && n.Type == gjson.Number {
		if val := n.Int(); val > 1 {
			out, _ = sjson.SetBytes(out, "request.generationConfig.candidateCount", val)
		}
	}

	// Map OpenAI modalities -> Gemini CLI request.generationConfig.responseModalities
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
			out, _ = sjson.SetBytes(out, "request.generationConfig.responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	// If the input uses top-level image_config.aspect_ratio, map it into request.generationConfig.imageConfig.aspectRatio.
	if imgCfg := gjson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "request.generationConfig.imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "request.generationConfig.imageConfig.imageSize", size.Str)
		}
	}

	// messages -> systemInstruction + contents
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		arr := messages.Array()
		systemPartIndex := 0
		// Leading system/developer messages hoist into request.systemInstruction;
		// mid-session ones demote to user turns to keep the prompt-cache
		// prefix immutable.
		// Ported from upstream CLIProxyAPI commit 0fe19ede90a4.
		hasEncounteredConversation := false
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()
			content := m.Get("content")

			if (role == "system" || role == "developer") && len(arr) > 1 && !hasEncounteredConversation {
				// system -> request.systemInstruction as a user message style
				if content.Type == gjson.String {
					out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
					out, _ = sjson.SetBytes(out, fmt.Sprintf("request.systemInstruction.parts.%d.text", systemPartIndex), content.String())
					systemPartIndex++
				} else if content.IsObject() && content.Get("type").String() == "text" {
					out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
					out, _ = sjson.SetBytes(out, fmt.Sprintf("request.systemInstruction.parts.%d.text", systemPartIndex), content.Get("text").String())
					systemPartIndex++
				} else if content.IsArray() {
					contents := content.Array()
					if len(contents) > 0 {
						out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
						for j := 0; j < len(contents); j++ {
							out, _ = sjson.SetBytes(out, fmt.Sprintf("request.systemInstruction.parts.%d.text", systemPartIndex), contents[j].Get("text").String())
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
					node, _ = sjson.SetBytes(node, "parts.0.text", antigravityDemotedSystemText(content.String(), isDemotedSystem))
					hasParts = true
				} else if content.IsObject() && content.Get("type").String() == "text" {
					node, _ = sjson.SetBytes(node, "parts.0.text", antigravityDemotedSystemText(content.Get("text").String(), isDemotedSystem))
					hasParts = true
				} else if content.IsArray() {
					items := content.Array()
					p := 0
					for _, item := range items {
						switch item.Get("type").String() {
						case "text":
							text := item.Get("text").String()
							if text != "" {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", antigravityDemotedSystemText(text, isDemotedSystem))
								hasParts = true
							}
							p++
						case "image_url":
							imageURL := item.Get("image_url.url").String()
							if len(imageURL) > 5 {
								pieces := strings.SplitN(imageURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									mime := pieces[0]
									data := pieces[1][7:]
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mimeType", mime)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thoughtSignature", geminiCLIFunctionThoughtSignature)
									hasParts = true
									p++
								}
							}
						case "video_url":
							videoURL := item.Get("video_url.url").String()
							if len(videoURL) > 5 {
								pieces := strings.SplitN(videoURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mimeType", pieces[0])
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", pieces[1][7:])
									hasParts = true
									p++
								}
							}
						case "file":
							filename := item.Get("file.filename").String()
							fileData := item.Get("file.file_data").String()
							ext := ""
							if sp := strings.Split(filename, "."); len(sp) > 1 {
								ext = sp[len(sp)-1]
							}
							if mimeType, ok := misc.MimeTypes[ext]; ok {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mimeType", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", fileData)
								hasParts = true
								p++
							} else {
								log.Warnf("Unknown file name extension '%s' in user message, skip", ext)
							}
						case "input_audio":
							audioData := item.Get("input_audio.data").String()
							audioFormat := item.Get("input_audio.format").String()
							if audioData != "" {
								audioMimeMap := map[string]string{
									"mp3":       "audio/mpeg",
									"wav":       "audio/wav",
									"ogg":       "audio/ogg",
									"flac":      "audio/flac",
									"aac":       "audio/aac",
									"webm":      "audio/webm",
									"pcm16":     "audio/pcm",
									"g711_ulaw": "audio/basic",
									"g711_alaw": "audio/basic",
								}
								mimeType := "audio/wav"
								if audioFormat != "" {
									if mapped, ok := audioMimeMap[audioFormat]; ok {
										mimeType = mapped
									} else {
										mimeType = "audio/" + audioFormat
									}
								}
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", audioData)
								hasParts = true
								p++
							}
						}
					}
				}
				// Guard against empty parts (e.g. content carried only
				// unsupported item types). Ported from upstream 0fe19ede90a4.
				if hasParts {
					out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)
				}
			} else if role == "assistant" {
				hasEncounteredConversation = true
				node := []byte(`{"role":"model","parts":[]}`)
				p := 0
				if content.Type == gjson.String && content.String() != "" {
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
							if len(imageURL) > 5 { // expect data:...
								pieces := strings.SplitN(imageURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									mime := pieces[0]
									data := pieces[1][7:]
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mimeType", mime)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thoughtSignature", geminiCLIFunctionThoughtSignature)
									p++
								}
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
						rawID string
						id    string
						name  string
					}
					toolCalls := make([]assistantToolCall, 0)
					usedToolCallIDs := make(map[string]struct{})
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() != "function" {
							continue
						}
						rawID := tc.Get("id").String()
						baseID := util.SanitizeClaudeToolID(rawID)
						fid := baseID
						suffix := 1
						for {
							if _, exists := usedToolCallIDs[fid]; !exists {
								usedToolCallIDs[fid] = struct{}{}
								break
							}
							fid = fmt.Sprintf("%s_%d", baseID, suffix)
							suffix++
						}
						fname := util.SanitizeFunctionName(tc.Get("function.name").String())
						if fname == "" {
							continue
						}
						fargs := tc.Get("function.arguments").String()
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.id", fid)
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.name", fname)
						if gjson.Valid(fargs) {
							node, _ = sjson.SetRawBytes(node, "parts."+itoa(p)+".functionCall.args", []byte(fargs))
						} else {
							node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.args.params", []byte(fargs))
						}
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thoughtSignature", geminiCLIFunctionThoughtSignature)
						p++
						toolCalls = append(toolCalls, assistantToolCall{rawID: rawID, id: fid, name: fname})
					}
					out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)

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
								turnToolResponses[callID] = arr[j].Get("content").String()
							}
						}
					}

					// Append a single tool content combining name + response per function
					toolNode := []byte(`{"role":"user","parts":[]}`)
					pp := 0
					for _, call := range toolCalls {
						toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.id", call.id)
						toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.name", call.name)
						resp := turnToolResponses[call.rawID]
						if resp == "" {
							resp = "{}"
						}
						if resp != "null" {
							toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.response.result", resp)
						}
						pp++
					}
					if pp > 0 {
						out, _ = sjson.SetRawBytes(out, "request.contents.-1", toolNode)
					}
				} else {
					out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)
				}
			}
		}
	}

	// tools -> request.tools[].functionDeclarations + request.tools[].googleSearch/codeExecution/urlContext passthrough
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		functionToolNode := []byte(`{}`)
		hasFunction := false
		googleSearchNodes := make([][]byte, 0)
		codeExecutionNodes := make([][]byte, 0)
		urlContextNodes := make([][]byte, 0)
		for _, t := range tools.Array() {
			if t.Get("type").String() == "function" {
				fn := t.Get("function")
				if fn.Exists() && fn.IsObject() {
					fnRaw := fn.Raw
					if fn.Get("parameters").Exists() {
						renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
						if errRename != nil {
							log.Warnf("Failed to rename parameters for tool '%s': %v", fn.Get("name").String(), errRename)
							var errSet error
							fnRawBytes, errSet := sjson.SetBytes([]byte(fnRaw), "parametersJsonSchema.type", "object")
							if errSet != nil {
								log.Warnf("Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet)
								continue
							}
							fnRaw = string(fnRawBytes)
							fnRawBytes, errSet = sjson.SetRawBytes([]byte(fnRaw), "parametersJsonSchema.properties", []byte(`{}`))
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
						fnRawBytes, errSet := sjson.SetBytes([]byte(fnRaw), "parametersJsonSchema.type", "object")
						if errSet != nil {
							log.Warnf("Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRaw = string(fnRawBytes)
						fnRawBytes, errSet = sjson.SetRawBytes([]byte(fnRaw), "parametersJsonSchema.properties", []byte(`{}`))
						if errSet != nil {
							log.Warnf("Failed to set default schema properties for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRaw = string(fnRawBytes)
					}
					fnRawBytes := []byte(fnRaw)
					fnRawBytes, _ = sjson.SetBytes(fnRawBytes, "name", util.SanitizeFunctionName(fn.Get("name").String()))
					fnRaw, _ = sjson.Delete(string(fnRawBytes), "strict")
					if !hasFunction {
						functionToolNode, _ = sjson.SetRawBytes(functionToolNode, "functionDeclarations", []byte("[]"))
					}
					tmp, errSet := sjson.SetRawBytes(functionToolNode, "functionDeclarations.-1", []byte(fnRaw))
					if errSet != nil {
						log.Warnf("Failed to append tool declaration for '%s': %v", fn.Get("name").String(), errSet)
						continue
					}
					functionToolNode = tmp
					hasFunction = true
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
		if hasFunction || len(googleSearchNodes) > 0 || len(codeExecutionNodes) > 0 || len(urlContextNodes) > 0 {
			toolsNode := []byte("[]")
			if hasFunction {
				toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", functionToolNode)
			}
			for _, googleNode := range googleSearchNodes {
				toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", googleNode)
			}
			for _, codeNode := range codeExecutionNodes {
				toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", codeNode)
			}
			for _, urlNode := range urlContextNodes {
				toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", urlNode)
			}
			out, _ = sjson.SetRawBytes(out, "request.tools", toolsNode)
		}
	}

	out = applyOpenAIToolChoiceToAntigravity(out, rawJSON)

	return common.AttachDefaultSafetySettings(out, "request.safetySettings")
}

// applyOpenAIToolChoiceToAntigravity maps OpenAI tool_choice onto Gemini
// toolConfig.functionCallingConfig. When tool_choice resolves to "none" the
// emitted request.tools are removed entirely.
// Ported from upstream CLIProxyAPI commit a76da7115486 ("omit tools when
// tool_choice is none").
func applyOpenAIToolChoiceToAntigravity(out, rawJSON []byte) []byte {
	toolChoice := gjson.GetBytes(rawJSON, "tool_choice")
	if !toolChoice.Exists() {
		return out
	}

	mode := ""
	allowedName := ""
	if toolChoice.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
		case "none":
			mode = "NONE"
		case "auto":
			mode = "AUTO"
		case "required", "any":
			mode = "ANY"
		}
	} else if toolChoice.IsObject() {
		switch strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String())) {
		case "none":
			mode = "NONE"
		case "function":
			mode = "ANY"
			allowedName = toolChoice.Get("function.name").String()
		}
	}
	if mode == "" {
		return out
	}

	out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", mode)
	if strings.TrimSpace(allowedName) != "" {
		out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.allowedFunctionNames", []string{util.SanitizeFunctionName(allowedName)})
	}
	return out
}

// itoa converts int to string without strconv import for few usages.
func itoa(i int) string { return fmt.Sprintf("%d", i) }

// antigravityDemotedSystemText wraps a demoted mid-session system or developer
// message in the <system-reminder> envelope so non-Claude upstream models treat it
// as a directive rather than user speech (upstream b681a1e0f7b8).
func antigravityDemotedSystemText(text string, isDemoted bool) string {
	if !isDemoted || strings.TrimSpace(text) == "" {
		return text
	}
	return translatorcommon.SystemReminderText(text)
}
