package helps

// Ported from upstream CLIProxyAPI commit 70f456045222
// (feat(antigravity): add conversation compaction support and capsule encryption).
// Local symbols under test: SealAntigravityCompaction, UnsealAntigravityCompaction,
// PrepareAntigravityCompactionSummaryPayload, ExpandAntigravityCompactionCapsules,
// ExtractAntigravitySummaryText, BuildAntigravityCompactionResponse,
// BuildAntigravityCompactionStreamChunks.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestAntigravityCompactionSealAndUnseal(t *testing.T) {
	summary := "This is a test summary of the previous turns with ANSI: \x1b[31mred\x1b[0m and quotes \"hello\" and newlines \n line2."
	model := "gemini-3.7-flash"

	capsule, errSeal := SealAntigravityCompaction(summary, model)
	if errSeal != nil {
		t.Fatalf("SealAntigravityCompaction failed: %v", errSeal)
	}

	if !strings.HasPrefix(capsule, antigravityCompactionCapsulePrefix) {
		t.Fatalf("capsule missing expected prefix: %s", capsule)
	}

	gotSummary, errUnseal := UnsealAntigravityCompaction(capsule)
	if errUnseal != nil {
		t.Fatalf("UnsealAntigravityCompaction failed: %v", errUnseal)
	}

	if gotSummary != summary {
		t.Fatalf("summary mismatch: got %q, want %q", gotSummary, summary)
	}

	// Test tampered capsule
	tampered := capsule[:len(capsule)-4] + "AAAA"
	_, errTampered := UnsealAntigravityCompaction(tampered)
	if errTampered == nil {
		t.Fatal("expected error when decrypting tampered capsule, got nil")
	}

	// Test invalid prefix
	_, errPrefix := UnsealAntigravityCompaction("invalid-prefix:abc")
	if errPrefix == nil {
		t.Fatal("expected error for invalid prefix, got nil")
	}
}

func TestAntigravityCompactionPersistentSecretAcrossAccountSwitch(t *testing.T) {
	// 1. Account A seals a capsule using the fixed secret
	capsule, errSeal := SealAntigravityCompaction("State before account failover", "gemini-3.7-flash")
	if errSeal != nil {
		t.Fatalf("seal before failover: %v", errSeal)
	}

	// 2. Account failover: account switches, capsule must successfully unseal without being bound to old account ID
	summaryAfterSwitch, errUnseal := UnsealAntigravityCompaction(capsule)
	if errUnseal != nil {
		t.Fatalf("failed to unseal capsule after account switch: %v", errUnseal)
	}
	if summaryAfterSwitch != "State before account failover" {
		t.Fatalf("summary mismatch after failover: got %q, want 'State before account failover'", summaryAfterSwitch)
	}
}

func TestPrepareAntigravityCompactionSummaryPayload(t *testing.T) {
	payload := []byte(`{
		"model": "gemini-3.7-flash",
		"stream": true,
		"tools": [{"type": "function", "name": "bash"}],
		"tool_choice": "auto",
		"previous_response_id": "resp-old",
		"input": [
			{"type": "additional_tools", "tools": [{"type": "function", "name": "exec_command"}]},
			{"type": "message", "role": "user", "content": "hi"},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hello"}]},
			{"type": "compaction_trigger"}
		]
	}`)

	prepared := PrepareAntigravityCompactionSummaryPayload(payload, "gemini-3.7-flash")

	if !json.Valid(prepared) {
		t.Fatalf("prepared payload is not valid JSON: %s", string(prepared))
	}
	if gjson.GetBytes(prepared, "stream").Bool() {
		t.Fatal("expected stream to be false")
	}
	if gjson.GetBytes(prepared, "tools").Exists() {
		t.Fatal("expected tools to be removed")
	}
	if gjson.GetBytes(prepared, "tool_choice").Exists() {
		t.Fatal("expected tool_choice to be removed")
	}
	if gjson.GetBytes(prepared, "previous_response_id").Exists() {
		t.Fatal("expected previous_response_id to be removed")
	}

	input := gjson.GetBytes(prepared, "input").Array()
	if len(input) != 3 {
		t.Fatalf("expected 3 items in input, got %d", len(input))
	}

	for _, item := range input {
		if item.Get("type").String() == "compaction_trigger" {
			t.Fatal("compaction_trigger must be removed from summary payload")
		}
		if item.Get("type").String() == "additional_tools" {
			t.Fatal("additional_tools must be removed from summary payload")
		}
	}

	lastItem := input[len(input)-1]
	if lastItem.Get("role").String() != "user" {
		t.Fatalf("expected last turn to be role 'user', got %s", lastItem.Get("role").String())
	}
}

func TestPrepareAntigravityCompactionSummaryPayload_StringInputWithControlChars(t *testing.T) {
	rawJSON := `{"model":"gemini-3.7-flash","input":"before\u001b[31mafter\nnewline and \"quotes\""}`
	if !json.Valid([]byte(rawJSON)) {
		t.Fatal("test rawJSON must be valid JSON")
	}

	prepared := PrepareAntigravityCompactionSummaryPayload([]byte(rawJSON), "gemini-3.7-flash")
	if !json.Valid(prepared) {
		t.Fatalf("prepared output with control chars must be valid JSON, got: %s", string(prepared))
	}

	input := gjson.GetBytes(prepared, "input").Array()
	if len(input) != 2 {
		t.Fatalf("expected 2 items in input for string input, got %d", len(input))
	}

	gotText := input[0].Get("content.0.text").String()
	wantText := "before\x1b[31mafter\nnewline and \"quotes\""
	if gotText != wantText {
		t.Fatalf("string input was truncated or corrupted: got %q, want %q", gotText, wantText)
	}
	if input[1].Get("role").String() != "user" {
		t.Fatalf("expected second item to be user summary prompt, got %s", input[1].Get("role").String())
	}
}

func TestExpandAntigravityCompactionCapsules(t *testing.T) {
	originalSummary := "Previous work done with \x1b[32mcontrol chars\x1b[0m and quotes: \"test\""
	capsule, errSeal := SealAntigravityCompaction(originalSummary, "gemini-3.7-flash")
	if errSeal != nil {
		t.Fatalf("seal: %v", errSeal)
	}

	payload := []byte(`{
		"model": "gemini-3.7-flash",
		"input": [
			{"type": "compaction", "id": "cmp-1", "encrypted_content": "` + capsule + `"},
			{"type": "message", "role": "user", "content": "next task"}
		]
	}`)

	expanded, errExpand := ExpandAntigravityCompactionCapsules(payload)
	if errExpand != nil {
		t.Fatalf("ExpandAntigravityCompactionCapsules failed: %v", errExpand)
	}
	if !json.Valid(expanded) {
		t.Fatalf("expanded payload is not valid JSON: %s", string(expanded))
	}

	input := gjson.GetBytes(expanded, "input").Array()
	if len(input) != 2 {
		t.Fatalf("expected 2 items, got %d", len(input))
	}

	firstItem := input[0]
	if firstItem.Get("type").String() != "message" {
		t.Fatalf("expected first item type 'message', got %s", firstItem.Get("type").String())
	}
	if firstItem.Get("role").String() != "developer" {
		t.Fatalf("expected first item role 'developer', got %s", firstItem.Get("role").String())
	}
	expandedText := firstItem.Get("content.0.text").String()
	if !strings.Contains(expandedText, originalSummary) {
		t.Fatalf("expected original summary preserved without corruption, got: %q", expandedText)
	}

	// Test invalid capsule returns error
	badPayload := []byte(`{
		"model": "gemini-3.7-flash",
		"input": [
			{"type": "compaction", "id": "cmp-1", "encrypted_content": "cpa-ag-compact-v1:corrupted"},
			{"type": "message", "role": "user", "content": "next task"}
		]
	}`)
	_, errBad := ExpandAntigravityCompactionCapsules(badPayload)
	if errBad == nil {
		t.Fatal("expected error for corrupted capsule, got nil")
	}
}

func TestExtractAntigravitySummaryText_ReasoningBeforeMessage(t *testing.T) {
	// Responses format where output.0 is reasoning and output.1 is the actual message
	resp := []byte(`{
		"output": [
			{
				"type": "reasoning",
				"summary": [{"type": "summary_text", "text": "thinking..."}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "output_text", "text": "This is the actual summary text."}
				]
			}
		]
	}`)

	text, err := ExtractAntigravitySummaryText(resp)
	if err != nil {
		t.Fatalf("ExtractAntigravitySummaryText failed: %v", err)
	}
	if text != "This is the actual summary text." {
		t.Fatalf("expected actual summary text, got %q", text)
	}

	// Empty output must return an error rather than placeholder
	emptyResp := []byte(`{"output": [{"type": "reasoning"}]}`)
	_, errEmpty := ExtractAntigravitySummaryText(emptyResp)
	if errEmpty == nil {
		t.Fatal("expected error when no summary text exists, got nil")
	}
}

func TestBuildAntigravityCompactionResponseAndChunks_ValidJSON(t *testing.T) {
	respJSON := BuildAntigravityCompactionResponse("gemini-3.7-flash", "cpa-ag-compact-v1:abc", 10, 20, 30)
	if !json.Valid(respJSON) {
		t.Fatalf("BuildAntigravityCompactionResponse produced invalid JSON: %s", string(respJSON))
	}

	chunks := BuildAntigravityCompactionStreamChunks("gemini-3.7-flash", "cpa-ag-compact-v1:abc", 10, 20, 30)
	if len(chunks) != 5 {
		t.Fatalf("expected 5 SSE frames, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		lines := strings.Split(string(chunk), "\n")
		var dataLine string
		for _, line := range lines {
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
				break
			}
		}
		if dataLine == "" || !json.Valid([]byte(dataLine)) {
			t.Fatalf("chunk[%d] data payload is not valid JSON: %q", i, dataLine)
		}
	}
}

func TestPrepareAntigravityCompactionSummaryPayloadStripsAdditionalToolsInputItem(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-5.1-codex-max",
		"stream": true,
		"tools": [{"type": "function", "name": "bash"}],
		"input": [
			{"type": "additional_tools", "tools": [{"type": "function", "name": "exec_command"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}
		]
	}`)

	prepared := PrepareAntigravityCompactionSummaryPayload(payload, "gpt-5.1-codex-max")
	if !json.Valid(prepared) {
		t.Fatalf("prepared payload is not valid JSON: %s", string(prepared))
	}
	if gjson.GetBytes(prepared, "tools").Exists() {
		t.Fatalf("expected top-level tools to be removed, got: %s", string(prepared))
	}
	input := gjson.GetBytes(prepared, "input").Array()
	if len(input) != 2 {
		t.Fatalf("expected 2 input items (user message + summary prompt), got %d: %s", len(input), string(prepared))
	}
	for _, item := range input {
		if item.Get("type").String() == "additional_tools" {
			t.Fatalf("additional_tools input item must be removed from summary payload: %s", string(prepared))
		}
	}
}

func TestExpandResponsesCompactionCapsulesDropsForeignAndExpandsCPA(t *testing.T) {
	capsule, errSeal := SealAntigravityCompaction("State before compaction", "gpt-5.1-codex-max")
	if errSeal != nil {
		t.Fatalf("SealAntigravityCompaction failed: %v", errSeal)
	}
	payload := []byte(`{
		"model": "gpt-5.1-codex-max",
		"input": [
			{"type": "compaction", "encrypted_content": "foreign-opaque-token"},
			{"type": "compaction", "id": "cmp_ag_compact_1", "encrypted_content": ""},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "continue"}]}
		]
	}`)
	payload, errSet := sjson.SetBytes(payload, "input.1.encrypted_content", capsule)
	if errSet != nil {
		t.Fatalf("sjson.SetBytes failed: %v", errSet)
	}

	req := cliproxyexecutor.Request{Model: "gpt-5.1-codex-max", Payload: payload}
	opts := cliproxyexecutor.Options{OriginalRequest: payload}
	if errExpand := ExpandResponsesCompactionCapsules(&req, &opts, "openai-compatibility"); errExpand != nil {
		t.Fatalf("ExpandResponsesCompactionCapsules failed: %v", errExpand)
	}

	if !bytes.Equal(req.Payload, opts.OriginalRequest) {
		t.Fatalf("expected req.Payload and opts.OriginalRequest to stay byte-identical\nreq=%s\nopts=%s", req.Payload, opts.OriginalRequest)
	}
	if bytes.Contains(req.Payload, []byte("foreign-opaque-token")) {
		t.Fatalf("expected foreign compaction item to be dropped, got: %s", req.Payload)
	}
	input := gjson.GetBytes(req.Payload, "input").Array()
	if len(input) != 2 {
		t.Fatalf("expected 2 input items after expansion, got %d: %s", len(input), req.Payload)
	}
	if input[0].Get("type").String() != "message" || input[0].Get("role").String() != "developer" {
		t.Fatalf("expected expanded developer message, got: %s", input[0].Raw)
	}
	if got := input[0].Get("content.0.text").String(); !strings.Contains(got, "State before compaction") {
		t.Fatalf("expected summary text in developer message, got %q", got)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("role").String() != "user" {
		t.Fatalf("expected unrelated user message preserved, got: %s", input[1].Raw)
	}
}

func TestExpandRecognizedResponsesCompactionCapsulesPreservesForeign(t *testing.T) {
	capsule, errSeal := SealAntigravityCompaction("Recognized summary", "gpt-5.1-codex-max")
	if errSeal != nil {
		t.Fatalf("SealAntigravityCompaction failed: %v", errSeal)
	}
	payload := []byte(`{
		"model": "gpt-5.1-codex-max",
		"input": [
			{"type": "compaction", "encrypted_content": "foreign-native-token"},
			{"type": "compaction", "encrypted_content": ""}
		]
	}`)
	payload, errSet := sjson.SetBytes(payload, "input.1.encrypted_content", capsule)
	if errSet != nil {
		t.Fatalf("sjson.SetBytes failed: %v", errSet)
	}

	req := cliproxyexecutor.Request{Model: "gpt-5.1-codex-max", Payload: payload}
	opts := cliproxyexecutor.Options{Alt: "responses/compact", OriginalRequest: payload}
	if errExpand := ExpandRecognizedResponsesCompactionCapsules(&req, &opts); errExpand != nil {
		t.Fatalf("ExpandRecognizedResponsesCompactionCapsules failed: %v", errExpand)
	}
	if !bytes.Contains(req.Payload, []byte("foreign-native-token")) {
		t.Fatalf("expected foreign compaction item preserved, got: %s", req.Payload)
	}
	input := gjson.GetBytes(req.Payload, "input").Array()
	if len(input) != 2 {
		t.Fatalf("expected 2 input items, got %d: %s", len(input), req.Payload)
	}
	if input[0].Get("type").String() != "compaction" {
		t.Fatalf("expected foreign compaction item first, got: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("role").String() != "developer" {
		t.Fatalf("expected CPA capsule expanded to developer message, got: %s", input[1].Raw)
	}
	if got := input[1].Get("content.0.text").String(); !strings.Contains(got, "Recognized summary") {
		t.Fatalf("expected summary text in developer message, got %q", got)
	}
}

func TestExpandResponsesCompactionCapsulesRejectsCorruptedCapsule(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-5.1-codex-max",
		"input": [
			{"type": "compaction", "encrypted_content": "cpa-ag-compact-v1:not-valid"},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "continue"}]}
		]
	}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.1-codex-max", Payload: payload}
	opts := cliproxyexecutor.Options{OriginalRequest: payload}
	errExpand := ExpandResponsesCompactionCapsules(&req, &opts, "openai-compatibility")
	if errExpand == nil {
		t.Fatal("expected error for corrupted compaction capsule, got nil")
	}
	if !strings.Contains(errExpand.Error(), "invalid compaction capsule") {
		t.Fatalf("unexpected error: %v", errExpand)
	}
}

func TestValidateResponsesCompactionSummaryStatus(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "completed", payload: `{"status":"completed","output":[{"type":"message","status":"completed"}]}`},
		{name: "completed without output status", payload: `{"status":"completed","output":[{"type":"message"}]}`},
		{name: "nested response completed", payload: `{"type":"response.completed","response":{"status":"completed"}}`},
		{name: "null error accepted", payload: `{"status":"completed","error":null,"incomplete_details":null}`},
		{name: "incomplete status", payload: `{"status":"incomplete"}`, wantErr: true},
		{name: "failed terminal event", payload: `{"type":"response.failed","status":"completed"}`, wantErr: true},
		{name: "incomplete terminal event", payload: `{"type":"response.incomplete","status":"completed"}`, wantErr: true},
		{name: "incomplete details", payload: `{"status":"completed","incomplete_details":{"reason":"max_output_tokens"}}`, wantErr: true},
		{name: "non completed output item", payload: `{"status":"completed","output":[{"type":"message","status":"in_progress"}]}`, wantErr: true},
		{name: "missing status", payload: `{"output":[]}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errValidate := ValidateResponsesCompactionSummaryStatus([]byte(tc.payload))
			if tc.wantErr && errValidate == nil {
				t.Fatalf("expected error for payload %s, got nil", tc.payload)
			}
			if !tc.wantErr && errValidate != nil {
				t.Fatalf("unexpected error for payload %s: %v", tc.payload, errValidate)
			}
		})
	}
}

func TestBuildResponsesCompactionStreamChunksFromResponseCarriesUsageDetails(t *testing.T) {
	capsule, errSeal := SealAntigravityCompaction("Streamed summary", "gpt-5.1-codex-max")
	if errSeal != nil {
		t.Fatalf("SealAntigravityCompaction failed: %v", errSeal)
	}
	compaction := []byte(`{
		"object": "response.compaction",
		"status": "completed",
		"output": [{"id": "cmp_ag_compact_1", "type": "compaction", "status": "completed", "encrypted_content": ""}],
		"usage": {
			"input_tokens": 12,
			"output_tokens": 6,
			"total_tokens": 18,
			"input_tokens_details": {"cached_tokens": 4},
			"output_tokens_details": {"reasoning_tokens": 2}
		}
	}`)
	compaction, errSet := sjson.SetBytes(compaction, "output.0.encrypted_content", capsule)
	if errSet != nil {
		t.Fatalf("sjson.SetBytes failed: %v", errSet)
	}

	chunks, errChunks := BuildResponsesCompactionStreamChunksFromResponse("gpt-5.1-codex-max", compaction)
	if errChunks != nil {
		t.Fatalf("BuildResponsesCompactionStreamChunksFromResponse failed: %v", errChunks)
	}
	if len(chunks) != 5 {
		t.Fatalf("expected 5 SSE frames, got %d", len(chunks))
	}

	var completedData string
	for _, chunk := range chunks {
		for _, line := range strings.Split(string(chunk), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if gjson.Get(data, "type").String() == "response.completed" {
				completedData = data
			}
		}
	}
	if completedData == "" {
		t.Fatalf("no response.completed frame found in chunks")
	}
	if got := gjson.Get(completedData, "response.usage.input_tokens_details.cached_tokens").Int(); got != 4 {
		t.Fatalf("cached_tokens = %d, want 4; frame=%s", got, completedData)
	}
	if got := gjson.Get(completedData, "response.usage.output_tokens_details.reasoning_tokens").Int(); got != 2 {
		t.Fatalf("reasoning_tokens = %d, want 2; frame=%s", got, completedData)
	}
	if got := gjson.Get(completedData, "response.usage.input_tokens").Int(); got != 12 {
		t.Fatalf("input_tokens = %d, want 12; frame=%s", got, completedData)
	}
	if got := gjson.Get(completedData, "response.usage.output_tokens").Int(); got != 6 {
		t.Fatalf("output_tokens = %d, want 6; frame=%s", got, completedData)
	}
	if got := gjson.Get(completedData, "response.usage.total_tokens").Int(); got != 18 {
		t.Fatalf("total_tokens = %d, want 18; frame=%s", got, completedData)
	}
	if got := gjson.Get(completedData, "response.output.0.encrypted_content").String(); got != capsule {
		t.Fatalf("capsule mismatch in completed frame: %q", got)
	}
}
