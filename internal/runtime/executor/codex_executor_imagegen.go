package executor

import (
	"fmt"
	"strings"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Ported from upstream CLIProxyAPI commit 8fb60326f405: Responses-Lite hosted
// image_generation compatibility helpers.

func normalizeCodexModelSlug(model string) string {
	slug := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if idx := strings.LastIndex(slug, "/"); idx >= 0 && idx < len(slug)-1 {
		slug = strings.TrimSpace(slug[idx+1:])
	}
	return slug
}

func isCodexResponsesLiteModel(model string) bool {
	slug := normalizeCodexModelSlug(model)
	if slug == "" {
		return false
	}
	if strings.HasPrefix(slug, "gpt-6") {
		return true
	}
	return registry.CodexClientModelUsesResponsesLite(slug)
}

func resolveCodexGPTImage2BaseModel(cfg *config.Config) string {
	if cfg == nil {
		return codexOpenAIImagesMainModel
	}
	model := strings.TrimSpace(cfg.GPTImage2BaseModel)
	if model == "" {
		return codexOpenAIImagesMainModel
	}
	if !strings.HasPrefix(strings.ToLower(model), "gpt-") || isCodexResponsesLiteModel(model) {
		return codexOpenAIImagesMainModel
	}
	return model
}

var codexDefaultLegacyThinkingLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

func codexFallbackModelInfoForThinking(model string) *registry.ModelInfo {
	slug := normalizeCodexModelSlug(model)
	levels := registry.CodexClientModelThinkingLevels(slug)
	if len(levels) == 0 {
		levels = append([]string(nil), codexDefaultLegacyThinkingLevels...)
	}
	return &registry.ModelInfo{
		ID:       slug,
		Type:     "openai",
		Thinking: &registry.ThinkingSupport{Levels: levels},
	}
}

// normalizeCodexImageGenerationTools strips parameters rejected by the hosted
// image_generation tool on Codex /responses (which binds to gpt-image-2-codex
// and rejects the complexity parameter).
func normalizeCodexImageGenerationTools(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	stripAtPath := func(payload []byte, toolsPath string) []byte {
		tools := gjson.GetBytes(payload, toolsPath)
		if !tools.IsArray() {
			return payload
		}
		for idx, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) != "image_generation" {
				continue
			}
			if tool.Get("complexity").Exists() {
				if updated, err := sjson.DeleteBytes(payload, fmt.Sprintf("%s.%d.complexity", toolsPath, idx)); err == nil {
					payload = updated
				}
			}
		}
		return payload
	}
	body = stripAtPath(body, "tools")
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for idx, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			body = stripAtPath(body, fmt.Sprintf("input.%d.tools", idx))
		}
	}
	return body
}

func codexCollectDeclaredTools(body []byte) []gjson.Result {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	var out []gjson.Result
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		out = append(out, tools.Array()...)
	}
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		for _, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			if extraTools := item.Get("tools"); extraTools.IsArray() {
				out = append(out, extraTools.Array()...)
			}
		}
	}
	return out
}

func codexRequestForcesImageGeneration(body []byte) bool {
	declaredTools := codexCollectDeclaredTools(body)
	hasImageGen := false
	allImageGen := len(declaredTools) > 0
	for _, tool := range declaredTools {
		if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
			hasImageGen = true
		} else {
			allImageGen = false
		}
	}
	if !hasImageGen {
		return false
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.Exists() {
		return false
	}
	if choice.Type == gjson.String {
		return strings.EqualFold(strings.TrimSpace(choice.String()), "required") && allImageGen
	}
	if !choice.IsObject() {
		return false
	}
	switch strings.TrimSpace(choice.Get("type").String()) {
	case "image_generation":
		return true
	case "allowed_tools":
		if !strings.EqualFold(strings.TrimSpace(choice.Get("mode").String()), "required") {
			return false
		}
		allowed := choice.Get("tools")
		if !allowed.IsArray() || len(allowed.Array()) == 0 {
			return false
		}
		for _, tool := range allowed.Array() {
			if strings.TrimSpace(tool.Get("type").String()) != "image_generation" {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func normalizeCodexHostedImageGenerationThinking(body []byte, imageBaseModel string, rawModel string) ([]byte, error) {
	if len(body) == 0 || imageBaseModel == "" {
		return body, nil
	}
	modelForThinking := imageBaseModel
	effectiveEffort := strings.ToLower(strings.TrimSpace(thinking.ExtractTranslatedReasoningEffort(body, "codex")))
	if suffix := thinking.ParseSuffix(rawModel); suffix.HasSuffix {
		rawSuffix := strings.TrimSpace(suffix.RawSuffix)
		if rawSuffix != "" {
			modelForThinking = imageBaseModel + "(" + rawSuffix + ")"
			effectiveEffort = strings.ToLower(rawSuffix)
		}
	}
	summaryCfg := thinking.ExtractSummaryConfig(body, "codex")
	modelInfo := registry.LookupModelInfo(imageBaseModel, "codex")
	if modelInfo == nil || modelInfo.Thinking == nil {
		modelInfo = codexFallbackModelInfoForThinking(imageBaseModel)
	}
	updated, errThinking := thinking.ApplyThinkingWithModelInfoAndSummary(body, nil, modelForThinking, "openai-response", "codex", "codex", modelInfo, summaryCfg, true)
	if errThinking == nil {
		return updated, nil
	}
	if effectiveEffort == "" {
		return body, errThinking
	}
	if effectiveEffort == "ultra" {
		effectiveEffort = string(thinking.LevelMax)
	}
	updated, errFallback := thinking.ApplyThinkingWithModelInfoAndSummary(body, nil, imageBaseModel+"("+effectiveEffort+")", codexOpenAIImageSourceFormat, "codex", "codex", modelInfo, summaryCfg)
	if errFallback != nil {
		return body, errFallback
	}
	return updated, nil
}

// applyCodexHostedImageGenerationModel rewrites forced hosted image_generation
// turns on Responses-Lite / GPT-6 models to the configured GPT-Image-2 base
// model (default "gpt-5.4-mini") because Codex /responses binds hosted
// image_generation to gpt-image-2-codex, which rejects complexity parameters
// emitted when those models drive the hosted tool.
func applyCodexHostedImageGenerationModel(cfg *config.Config, body []byte, baseModel string, rawModel ...string) ([]byte, string, error) {
	if len(body) == 0 || !isCodexResponsesLiteModel(baseModel) || !codexRequestForcesImageGeneration(body) {
		return body, baseModel, nil
	}
	if cfg != nil && cfg.DisableImageGeneration != config.DisableImageGenerationOff {
		return body, baseModel, nil
	}
	sourceModel := baseModel
	if len(rawModel) > 0 && strings.TrimSpace(rawModel[0]) != "" {
		sourceModel = rawModel[0]
	}
	imageBaseModel := resolveCodexGPTImage2BaseModel(cfg)
	body = helps.SetStringIfDifferent(body, "model", imageBaseModel)
	updated, errThinking := normalizeCodexHostedImageGenerationThinking(body, imageBaseModel, sourceModel)
	if errThinking != nil {
		return body, imageBaseModel, errThinking
	}
	return updated, imageBaseModel, nil
}
