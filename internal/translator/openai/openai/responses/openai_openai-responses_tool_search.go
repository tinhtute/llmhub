package responses

import (
	"strconv"
	"strings"

	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Chat Completions has no tool search, so the client-executed Responses
// tool_search declaration is bridged as an ordinary function. Calls to it are
// restored as tool_search_call items, the client runs the search, and the
// tools it returns in tool_search_output stay declared as functions on every
// later request.
// Ported from upstream CLIProxyAPI commit 8c9a78518e39.

const responsesToolSearchDefaultParameters = `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`

// responsesToolSearchBridge resolves the synthetic Chat Completions function
// name standing in for client-executed tool search and rewrites replayed
// tool_search_call/tool_search_output items into the function_call pair the
// Chat Completions conversion already groups and pairs.
type responsesToolSearchBridge struct {
	Name        string
	Declared    bool
	Declaration gjson.Result
}

func newResponsesToolSearchBridge(root gjson.Result) responsesToolSearchBridge {
	declared := false
	replayed := false
	var declaration gjson.Result
	scanTools := func(tools gjson.Result) {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if util.IsResponsesClientToolSearchDeclaration(tool) {
				declared = true
				if !declaration.Exists() {
					declaration = tool
				}
				return false
			}
			return true
		})
	}
	scanTools(root.Get("tools"))
	input := root.Get("input")
	if input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "additional_tools":
				scanTools(item.Get("tools"))
			case util.ResponsesToolSearchCallType, util.ResponsesToolSearchOutputType:
				if util.IsResponsesClientToolSearchItem(item) {
					replayed = true
				}
			}
			return true
		})
	}
	if !declared && !replayed {
		return responsesToolSearchBridge{}
	}

	// Reserve user identities so the shim never shadows a real tool.
	reserved := make(map[string]bool)
	reserveTools := func(tools gjson.Result) {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if name := strings.TrimSpace(responsesToolName(tool)); name != "" {
				reserved[name] = true
			}
			return true
		})
	}
	reserveTools(root.Get("tools"))
	if input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "additional_tools":
				reserveTools(item.Get("tools"))
			case util.ResponsesToolSearchOutputType:
				if util.IsResponsesClientToolSearchItem(item) {
					reserveTools(item.Get("tools"))
				}
			case "function_call", "custom_tool_call":
				if name := strings.TrimSpace(item.Get("name").String()); name != "" {
					reserved[name] = true
				}
			}
			return true
		})
	}
	name := util.ResponsesToolSearchFunctionName
	for suffix := 1; reserved[name]; suffix++ {
		name = util.ResponsesToolSearchFunctionName + "_" + strconv.Itoa(suffix)
	}
	return responsesToolSearchBridge{Name: name, Declared: declared, Declaration: declaration}
}

// rewriteInput replaces client tool_search_call and tool_search_output items
// with the function_call/function_call_output pair the Chat Completions
// conversion understands. Hosted (server) search items are left as they are.
func (b responsesToolSearchBridge) rewriteInput(items []gjson.Result) []gjson.Result {
	if b.Name == "" {
		return items
	}
	var out []gjson.Result
	for i, item := range items {
		itemType := item.Get("type").String()
		if (itemType != util.ResponsesToolSearchCallType && itemType != util.ResponsesToolSearchOutputType) ||
			!util.IsResponsesClientToolSearchItem(item) {
			if out != nil {
				out = append(out, item)
			}
			continue
		}
		if out == nil {
			out = append(make([]gjson.Result, 0, len(items)), items[:i]...)
		}
		if itemType == util.ResponsesToolSearchCallType {
			out = append(out, responsesToolSearchCallAsFunctionCall(item, b.Name))
		} else {
			out = append(out, responsesToolSearchOutputAsFunctionCallOutput(item))
		}
	}
	if out == nil {
		return items
	}
	return out
}

func responsesToolSearchCallAsFunctionCall(item gjson.Result, searchName string) gjson.Result {
	call := []byte(`{"type":"function_call","call_id":"","name":"","arguments":"{}"}`)
	call, _ = sjson.SetBytes(call, "call_id", translatorcommon.ExtractResponsesCallID(item))
	call, _ = sjson.SetBytes(call, "name", searchName)
	if arguments := item.Get("arguments"); arguments.Type == gjson.String && strings.TrimSpace(arguments.String()) != "" {
		call, _ = translatorcommon.SetStringWithoutHTMLEscape(call, "arguments", arguments.String())
	} else if arguments.IsObject() {
		call, _ = translatorcommon.SetStringWithoutHTMLEscape(call, "arguments", arguments.Raw)
	}
	return gjson.ParseBytes(call)
}

func responsesToolSearchOutputAsFunctionCallOutput(item gjson.Result) gjson.Result {
	names := make([]string, 0)
	seen := make(map[string]struct{})
	appendName := func(name string) {
		if _, exists := seen[name]; name == "" || exists {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	item.Get("tools").ForEach(func(_, tool gjson.Result) bool {
		if strings.TrimSpace(tool.Get("type").String()) == "namespace" {
			namespaceName := strings.TrimSpace(tool.Get("name").String())
			tool.Get("tools").ForEach(func(_, child gjson.Result) bool {
				if childName := responsesToolName(child); childName != "" {
					appendName(qualifyResponsesNamespaceToolName(namespaceName, childName))
				}
				return true
			})
		} else if name := responsesToolName(tool); name != "" {
			appendName(name)
		}
		return true
	})

	summary := []byte(`{"status":"completed","tools":[]}`)
	if status := strings.TrimSpace(item.Get("status").String()); status != "" {
		summary, _ = sjson.SetBytes(summary, "status", status)
	}
	summary, _ = sjson.SetBytes(summary, "tools", names)
	output := []byte(`{"type":"function_call_output","call_id":"","output":""}`)
	output, _ = sjson.SetBytes(output, "call_id", translatorcommon.ExtractResponsesCallID(item))
	output, _ = translatorcommon.SetStringWithoutHTMLEscape(output, "output", string(summary))
	return gjson.ParseBytes(output)
}

// convertResponsesToolSearchToolToOpenAIChat exposes the client tool_search
// declaration as an ordinary Chat Completions function.
func convertResponsesToolSearchToolToOpenAIChat(tool gjson.Result, name string) ([]byte, bool) {
	chatTool := []byte(`{"type":"function","function":{"name":"","description":"Search for deferred tools and load the matching tools.","parameters":{}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	if description := util.ResponsesToolDescription(tool); description != "" {
		chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	}
	parameters := responsesToolSearchDefaultParameters
	if declared := util.ResponsesToolParameters(tool); declared.Exists() && declared.IsObject() {
		parameters = declared.Raw
	}
	chatTool, _ = sjson.SetRawBytes(chatTool, "function.parameters", []byte(parameters))
	return chatTool, true
}

// responsesToolSearchNameFromRequest resolves the bridged function name for a
// request, or "" when the request has no client tool search.
func responsesToolSearchNameFromRequest(rawJSON []byte) string {
	if len(rawJSON) == 0 {
		return ""
	}
	root := gjson.ParseBytes(rawJSON)
	return newResponsesToolSearchBridge(root).Name
}

// responsesToolSearchCallItem renders a bridged search function call as a
// client-executed Responses tool_search_call with object arguments.
func responsesToolSearchCallItem(callID, args, status string) []byte {
	trimmed := strings.TrimSpace(args)
	parsedArgs := gjson.Parse(trimmed)
	validArgs := gjson.Valid(trimmed) && parsedArgs.IsObject()
	if status == "completed" && !validArgs {
		status = "incomplete"
	}
	item := []byte(`{"id":"","type":"tool_search_call","status":"","execution":"client","call_id":"","arguments":{}}`)
	item, _ = sjson.SetBytes(item, "id", util.ResponsesToolSearchIDPrefix+callID)
	item, _ = sjson.SetBytes(item, "status", status)
	item, _ = sjson.SetBytes(item, "call_id", callID)
	if validArgs {
		item, _ = sjson.SetRawBytes(item, "arguments", []byte(parsedArgs.Raw))
	}
	return item
}

func responsesToolSearchCallPlaceholder(callID string) []byte {
	item := []byte(`{"id":"","type":"tool_search_call","status":"in_progress","execution":"client","call_id":"","arguments":{}}`)
	item, _ = sjson.SetBytes(item, "id", util.ResponsesToolSearchIDPrefix+callID)
	item, _ = sjson.SetBytes(item, "call_id", callID)
	return item
}
