package helps

import (
	"strings"

	"github.com/therealtinhtute/llmhub/internal/thinking"
	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PrepareAntigravityClaudeStructuredOutput replaces native output schemas with
// best-effort prompt instructions for Claude 5.5, which rejects forced tool use
// (upstream 1d45a5f33290).
//
// Call it inside the Antigravity request build, after the payload rules have
// run and before schema sanitization: the conversion is the last word for the
// three native-output paths it rewrites, which is where upstream leaves it too.
// The boolean tells the request builder not to restore VALIDATED tool mode.
func PrepareAntigravityClaudeStructuredOutput(model string, payload []byte) ([]byte, bool) {
	model = strings.ToLower(thinking.ParseSuffix(strings.TrimSpace(model)).ModelName)
	if !claude55Model.MatchString(model) {
		return payload, false
	}

	containers := []string{"request.generationConfig", "request.generation_config"}
	schemaKeys := []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"}
	var schema gjson.Result
	for _, container := range containers {
		for _, key := range schemaKeys {
			if candidate := gjson.GetBytes(payload, container+"."+key); candidate.IsObject() {
				schema = candidate
				break
			}
		}
		if schema.Exists() {
			break
		}
	}
	if !schema.Exists() {
		return payload, false
	}

	// Wrap the untouched schema as a format, not as format settings themselves.
	format := gjson.Parse(`{"type":"json_schema","schema":` + schema.Raw + `}`)
	instruction := translatorcommon.BuildClaudeStructuredOutputInstruction(format)
	systemPath := "request.systemInstruction"
	if !gjson.GetBytes(payload, systemPath).Exists() && gjson.GetBytes(payload, "request.system_instruction").Exists() {
		systemPath = "request.system_instruction"
	}
	updated, errSet := sjson.SetBytes(payload, systemPath+".parts.-1.text", instruction)
	if errSet != nil {
		return payload, false
	}
	payload = updated

	// Remove only native output leaves; other config fields and aliases stay put.
	for _, container := range containers {
		for _, key := range append(schemaKeys, "responseMimeType", "response_mime_type") {
			payload, _ = sjson.DeleteBytes(payload, container+"."+key)
		}
	}
	for _, container := range []string{"request.toolConfig", "request.tool_config"} {
		for _, key := range []string{"functionCallingConfig", "function_calling_config"} {
			path := container + "." + key
			switch strings.ToUpper(gjson.GetBytes(payload, path+".mode").String()) {
			case "ANY", "VALIDATED":
				payload, _ = sjson.SetBytes(payload, path+".mode", "AUTO")
				payload, _ = sjson.DeleteBytes(payload, path+".allowedFunctionNames")
				payload, _ = sjson.DeleteBytes(payload, path+".allowed_function_names")
			}
		}
	}
	return payload, true
}
