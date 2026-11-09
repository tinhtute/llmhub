package claude

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertClaudeRequestToAntigravity_PreservesToolResultErrors pins the
// success/failure split for the Antigravity ingress: an explicit
// tool_result.is_error must land in functionResponse.response.error.
// Ported from upstream CLIProxyAPI commit 068fe153efa6.
func TestConvertClaudeRequestToAntigravity_PreservesToolResultErrors(t *testing.T) {
	cases := []struct {
		name         string
		fields       string
		wantResponse string
	}{
		{name: "success_without_flag", fields: `"content":"ok"`, wantResponse: `{"result":"ok"}`},
		{name: "explicit_success", fields: `"is_error":false,"content":"ok"`, wantResponse: `{"result":"ok"}`},
		{name: "failed_string", fields: `"is_error":true,"content":"permission denied"`, wantResponse: `{"error":"permission denied"}`},
		{name: "failed_object", fields: `"is_error":true,"content":{"code":"EACCES"}`, wantResponse: `{"error":{"code":"EACCES"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(fmt.Sprintf(`{"messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1",%s}]}
			]}`, tc.fields))
			output := ConvertClaudeRequestToAntigravity("claude-sonnet-4-5", input, false)
			if !gjson.ValidBytes(output) {
				t.Fatalf("invalid translated JSON: %s", output)
			}
			functionResponse := gjson.GetBytes(output, "request.contents.1.parts.0.functionResponse")
			if !functionResponse.Exists() {
				t.Fatalf("functionResponse missing: %s", output)
			}
			if functionResponse.Get("id").String() != "call_1" || functionResponse.Get("name").String() != "Read" {
				t.Fatalf("tool result lost its call identity: %s", functionResponse.Raw)
			}
			if got := functionResponse.Get("response").Raw; got != tc.wantResponse {
				t.Fatalf("functionResponse.response = %s, want %s", got, tc.wantResponse)
			}
		})
	}
}
