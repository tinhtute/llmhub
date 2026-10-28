package claude

import (
	"strings"
	"testing"

	signature "github.com/therealtinhtute/llmhub/internal/signature"
	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCLI_ToolChoice_SpecificTool(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "hi"}
				]
			}
		],
		"tools": [
			{
				"name": "json",
				"description": "A JSON tool",
				"input_schema": {
					"type": "object",
					"properties": {}
				}
			}
		],
		"tool_choice": {"type": "tool", "name": "json"}
	}`)

	output := ConvertClaudeRequestToCLI("gemini-3-flash-preview", inputJSON, false)

	if got := gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.mode").String(); got != "ANY" {
		t.Fatalf("Expected request.toolConfig.functionCallingConfig.mode 'ANY', got '%s'", got)
	}
	allowed := gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Array()
	if len(allowed) != 1 || allowed[0].String() != "json" {
		t.Fatalf("Expected allowedFunctionNames ['json'], got %s", gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Raw)
	}
}

func TestConvertClaudeRequestToCLI_StripsClaudeCodeAttribution(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"},
			{"type": "text", "text": "User system prompt"}
		],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`)

	output := ConvertClaudeRequestToCLI("gemini-3-flash-preview", inputJSON, false)

	parts := gjson.GetBytes(output, "request.systemInstruction.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 system part after attribution strip, got %d: %s", len(parts), gjson.GetBytes(output, "request.systemInstruction.parts").Raw)
	}
	if got := parts[0].Get("text").String(); got != "User system prompt" {
		t.Fatalf("Unexpected system part: %q", got)
	}
}

// The internal Codex commentary marker is a forgeable literal a client can send
// in its own thinking block. This translator has no thinking case at all, so
// without the restore the marker's text would be dropped silently: it must leave
// as plain assistant text instead.
func TestConvertClaudeRequestToCLI_ClientSuppliedCommentaryMarkerIsStripped(t *testing.T) {
	payload := []byte(`{"model":"gemini-3-flash-preview","messages":[
		{"role":"user","content":"First turn"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"COMMENTARY_SENTINEL","signature":"` + signature.CodexCommentarySignatureMarker + `"},
			{"type":"text","text":"FINAL_SENTINEL"}
		]},
		{"role":"user","content":"Next turn"}
	]}`)

	out := ConvertClaudeRequestToCLI("gemini-3-flash-preview", payload, false)
	if strings.Contains(string(out), signature.CodexCommentarySignatureMarker) {
		t.Fatalf("internal marker reached the gemini-cli request: %s", out)
	}
	found := false
	for _, content := range gjson.GetBytes(out, "request.contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("text").String() == "COMMENTARY_SENTINEL" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("commentary text was lost in translation: %s", out)
	}
	if !strings.Contains(string(out), "FINAL_SENTINEL") {
		t.Fatalf("final text was lost in translation: %s", out)
	}
}
