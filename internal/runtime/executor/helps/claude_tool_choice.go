package helps

import (
	"regexp"
	"strings"

	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claude55Model matches the Claude 5.5 routes that reject forced tool use
// (upstream 1d45a5f33290). This is the routed-model family boundary, not the
// cloak/display grouping in the Claude executor: isClaudeSonnet55Model also
// accepts claude-sonnet-5-5-custom, which this boundary deliberately excludes.
var claude55Model = regexp.MustCompile(`^claude-(opus|sonnet)-5-5(?:-(high|reasoning))?(?:-[0-9]{8})?(?:\[[^\[\]]+\])?$`)

// NormalizeClaudeToolChoice downgrades forced tool use on Claude 5.5 to auto.
//
// Call it with the routed upstream model once the model is on the payload and
// before thinking preparation, so the request the API receives is the one the
// thinking stage and the signed history were prepared against. User payload
// rules run afterwards on the Claude path and stay authoritative: they see the
// downgraded value and can still force a choice on purpose.
//
// Every other model keeps its payload unchanged, byte for byte.
func NormalizeClaudeToolChoice(model string, payload []byte) []byte {
	model = strings.ToLower(thinking.ParseSuffix(strings.TrimSpace(model)).ModelName)
	if !claude55Model.MatchString(model) {
		return payload
	}
	switch gjson.GetBytes(payload, "tool_choice.type").String() {
	case "any", "tool":
		payload, _ = sjson.SetBytes(payload, "tool_choice.type", "auto")
		payload, _ = sjson.DeleteBytes(payload, "tool_choice.name")
	}
	return payload
}
