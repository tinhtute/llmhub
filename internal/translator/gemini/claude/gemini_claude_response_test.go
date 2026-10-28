package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiResponseToClaudeNonStream_PreservesThoughtSignature(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "thinking step 1\n", "thought": true},
					{"text": "thinking step 2", "thought": true, "thoughtSignature": "sig-xyz-123"},
					{"text": "visible answer"}
				]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 5
		},
		"modelVersion": "gemini-2.5-pro",
		"responseId": "resp-non-stream"
	}`)

	ctx := context.Background()
	output := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-2.5-pro", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d: %s", len(blocks), string(output))
	}

	thinkingBlock := blocks[0]
	if thinkingBlock.Get("type").String() != "thinking" {
		t.Fatalf("expected first block to be thinking, got %s", thinkingBlock.Get("type").String())
	}
	if thinkingBlock.Get("thinking").String() != "thinking step 1\nthinking step 2" {
		t.Fatalf("unexpected thinking content: %s", thinkingBlock.Get("thinking").String())
	}
	if thinkingBlock.Get("signature").String() != "sig-xyz-123" {
		t.Fatalf("expected signature 'sig-xyz-123', got %q. Output: %s", thinkingBlock.Get("signature").String(), string(output))
	}

	textBlock := blocks[1]
	if textBlock.Get("type").String() != "text" || textBlock.Get("text").String() != "visible answer" {
		t.Fatalf("unexpected text block: %s", textBlock.Raw)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_PartWithThoughtSignatureWithoutThoughtBool(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "inferred reasoning", "thought_signature": "sig-snake-case"},
					{"text": "final answer"}
				]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 5
		},
		"modelVersion": "gemini-2.5-pro",
		"responseId": "resp-non-stream-2"
	}`)

	ctx := context.Background()
	output := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-2.5-pro", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d: %s", len(blocks), string(output))
	}

	thinkingBlock := blocks[0]
	if thinkingBlock.Get("type").String() != "thinking" {
		t.Fatalf("expected first block to be thinking, got %s", thinkingBlock.Get("type").String())
	}
	if thinkingBlock.Get("thinking").String() != "inferred reasoning" {
		t.Fatalf("unexpected thinking content: %s", thinkingBlock.Get("thinking").String())
	}
	if thinkingBlock.Get("signature").String() != "sig-snake-case" {
		t.Fatalf("expected signature 'sig-snake-case', got %q. Output: %s", thinkingBlock.Get("signature").String(), string(output))
	}

	textBlock := blocks[1]
	if textBlock.Get("type").String() != "text" || textBlock.Get("text").String() != "final answer" {
		t.Fatalf("unexpected text block: %s", textBlock.Raw)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_TrailingSignatureOnlyPart(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "thinking step 1\n", "thought": true},
					{"text": "", "thoughtSignature": "sig-trailing"},
					{"text": "visible answer"}
				]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 5
		},
		"modelVersion": "gemini-2.5-pro",
		"responseId": "resp-non-stream-trailing"
	}`)

	ctx := context.Background()
	output := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-2.5-pro", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d: %s", len(blocks), string(output))
	}

	thinkingBlock := blocks[0]
	if thinkingBlock.Get("type").String() != "thinking" {
		t.Fatalf("expected first block to be thinking, got %s", thinkingBlock.Get("type").String())
	}
	if thinkingBlock.Get("thinking").String() != "thinking step 1\n" {
		t.Fatalf("unexpected thinking content: %s", thinkingBlock.Get("thinking").String())
	}
	if thinkingBlock.Get("signature").String() != "sig-trailing" {
		t.Fatalf("expected signature 'sig-trailing', got %q. Output: %s", thinkingBlock.Get("signature").String(), string(output))
	}

	textBlock := blocks[1]
	if textBlock.Get("type").String() != "text" || textBlock.Get("text").String() != "visible answer" {
		t.Fatalf("unexpected text block: %s", textBlock.Raw)
	}
}

func geminiClaudeInputJSONDeltas(t *testing.T, streamText string) string {
	t.Helper()
	var builder strings.Builder
	for _, line := range strings.Split(streamText, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if gjson.Get(payload, "delta.type").String() == "input_json_delta" {
			builder.WriteString(gjson.Get(payload, "delta.partial_json").String())
		}
	}
	return builder.String()
}

func TestConvertGeminiResponseToClaude_ClaudeTool_SanitizesParameterlessToolArguments(t *testing.T) {
	origRequest := []byte(`{"model":"gemini-3-flash","tools":[{"name":"EnterPlanMode","input_schema":{"type":"object","properties":{}}}]}`)
	rawResponse := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"EnterPlanMode","args":{"reason":"Plan mode switch"}}}],"role":"model"},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","responseId":"resp-zero-arg"}`)

	ctx := context.Background()
	translatedRequest := ConvertClaudeRequestToGemini("gemini-3-flash", origRequest, false)

	nonStream := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-3-flash", origRequest, translatedRequest, rawResponse, nil)
	if got := gjson.GetBytes(nonStream, "content.0.input").Raw; got != "{}" {
		t.Fatalf("expected non-streaming input {}, got %s", got)
	}

	var param any
	streamBytes := bytes.Join(ConvertGeminiResponseToClaude(ctx, "gemini-3-flash", origRequest, translatedRequest, rawResponse, &param), nil)
	if got := geminiClaudeInputJSONDeltas(t, string(streamBytes)); got != "{}" {
		t.Fatalf("expected streaming partial_json {}, got %q (stream: %s)", got, string(streamBytes))
	}
}

func TestConvertGeminiResponseToClaude_ClaudeTool_CanonicalNameCollisionKeepsExactToolArguments(t *testing.T) {
	origRequest := []byte(`{"model":"gemini-3-flash","tools":[{"name":"Read","input_schema":{"type":"object","properties":{}}},{"name":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`)
	readWithArgs := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{"path":"/tmp/a"}}}],"role":"model"},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","responseId":"resp-collision"}`)
	readParameterless := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"Read","args":{"reason":"x"}}}],"role":"model"},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","responseId":"resp-collision-2"}`)

	ctx := context.Background()
	translatedRequest := ConvertClaudeRequestToGemini("gemini-3-flash", origRequest, false)

	nonStream := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-3-flash", origRequest, translatedRequest, readWithArgs, nil)
	if got := gjson.GetBytes(nonStream, "content.0.input").Raw; got != `{"path":"/tmp/a"}` {
		t.Fatalf("expected exact read tool args preserved, got %s (%s)", got, string(nonStream))
	}
	if got := gjson.GetBytes(nonStream, "content.0.name").String(); got != "read" {
		t.Fatalf("expected exact tool name read, got %q (%s)", got, string(nonStream))
	}

	var param any
	streamBytes := bytes.Join(ConvertGeminiResponseToClaude(ctx, "gemini-3-flash", origRequest, translatedRequest, readWithArgs, &param), nil)
	if got := geminiClaudeInputJSONDeltas(t, string(streamBytes)); got != `{"path":"/tmp/a"}` {
		t.Fatalf("expected streaming exact read args preserved, got %q (stream: %s)", got, string(streamBytes))
	}

	nonStreamParameterless := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-3-flash", origRequest, translatedRequest, readParameterless, nil)
	if got := gjson.GetBytes(nonStreamParameterless, "content.0.input").Raw; got != "{}" {
		t.Fatalf("expected exact Read tool normalized to {}, got %s", got)
	}
}

func TestConvertGeminiResponseToClaude_ClaudeTool_AmbiguousCanonicalNameKeepsUpstreamNameAndArguments(t *testing.T) {
	origRequest := []byte(`{"model":"gemini-3-flash","tools":[{"name":"Read","input_schema":{"type":"object","properties":{}}},{"name":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`)
	ambiguousResponse := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"READ","args":{"path":"/tmp/a"}}}],"role":"model"},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","responseId":"resp-ambiguous"}`)
	translatedRequest := ConvertClaudeRequestToGemini("gemini-3-flash", origRequest, false)
	nonStream := ConvertGeminiResponseToClaudeNonStream(context.Background(), "gemini-3-flash", origRequest, translatedRequest, ambiguousResponse, nil)
	if got := gjson.GetBytes(nonStream, "content.0.name").String(); got != "READ" {
		t.Fatalf("expected ambiguous upstream name READ preserved, got %q (%s)", got, string(nonStream))
	}
	if got := gjson.GetBytes(nonStream, "content.0.input").Raw; got != `{"path":"/tmp/a"}` {
		t.Fatalf("expected ambiguous tool args preserved, got %s", got)
	}
}

func TestConvertGeminiResponseToClaude_ClaudeTool_KeepsReasonWhenTranslatedSchemaHasNoPlaceholder(t *testing.T) {
	origRequest := []byte(`{"model":"gemini-3-flash","tools":[{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]}`)
	ctx := context.Background()
	translatedRequest := ConvertClaudeRequestToGemini("gemini-3-flash", origRequest, false)
	rawResponse := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"Bash","args":{"command":"ls","reason":"why"}}}],"role":"model"},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","responseId":"resp-bash-reason"}`)

	nonStream := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-3-flash", origRequest, translatedRequest, rawResponse, nil)
	if got := gjson.GetBytes(nonStream, "content.0.input").Raw; got != `{"command":"ls","reason":"why"}` {
		t.Fatalf("expected non-streaming input preserved, got %s (translated: %s)", got, string(translatedRequest))
	}
}

func TestConvertGeminiResponseToClaude_ClaudeTool_ParameterlessToolContinuationDoesNotDuplicateEmptyObject(t *testing.T) {
	origRequest := []byte(`{"model":"gemini-3-flash","tools":[{"name":"EnterPlanMode","input_schema":{"type":"object","properties":{}}}]}`)
	firstChunk := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"EnterPlanMode","args":{"reason":"Plan mode switch"}}}],"role":"model"}}],"modelVersion":"gemini-3-flash","responseId":"resp-zero-arg"}`)
	continuationChunk := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"reason":"more"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","responseId":"resp-zero-arg"}`)

	ctx := context.Background()
	translatedRequest := ConvertClaudeRequestToGemini("gemini-3-flash", origRequest, false)
	var param any
	streamBytes := bytes.Join(ConvertGeminiResponseToClaude(ctx, "gemini-3-flash", origRequest, translatedRequest, firstChunk, &param), nil)
	streamBytes = append(streamBytes, bytes.Join(ConvertGeminiResponseToClaude(ctx, "gemini-3-flash", origRequest, translatedRequest, continuationChunk, &param), nil)...)
	if got := geminiClaudeInputJSONDeltas(t, string(streamBytes)); got != "{}" {
		t.Fatalf("expected concatenated partial_json {}, got %q (stream: %s)", got, string(streamBytes))
	}
}

func TestConvertGeminiResponseToClaudeStream_MalformedFunctionCallReturnsAPIError(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	rawJSON := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": []},
			"index": 0,
			"finishReason": "MALFORMED_FUNCTION_CALL"
		}],
		"modelVersion": "m",
		"responseId": "resp-malformed"
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, rawJSON, &param), nil)
	output = append(output, bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	if strings.Contains(outputText, `"stop_reason":"refusal"`) {
		t.Fatalf("unexpected refusal stop_reason in output:\n%s", outputText)
	}
	if strings.Contains(outputText, `"type":"message_delta"`) {
		t.Fatalf("unexpected message_delta in output:\n%s", outputText)
	}
	if strings.Contains(outputText, `"type":"message_stop"`) {
		t.Fatalf("unexpected message_stop in output:\n%s", outputText)
	}
	if !strings.Contains(outputText, "event: error\n") {
		t.Fatalf("expected SSE error event in output:\n%s", outputText)
	}

	var foundError bool
	for _, line := range strings.Split(outputText, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"error"`) {
			foundError = true
			errJSON := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if got := errJSON.Get("error.type").String(); got != "api_error" {
				t.Fatalf("error.type = %q, want api_error. Payload: %s", got, line)
			}
			if !strings.Contains(errJSON.Get("error.message").String(), "MALFORMED_FUNCTION_CALL") {
				t.Fatalf("error.message = %q, want mention of MALFORMED_FUNCTION_CALL. Payload: %s", errJSON.Get("error.message").String(), line)
			}
		}
	}
	if !foundError {
		t.Fatalf("failed to find error event data in output:\n%s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeStream_MalformedFunctionCallAfterThinkingEmitsError(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	chunk1 := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": [{"thought": true, "text": "planning tool call"}]},
			"index": 0
		}],
		"modelVersion": "m"
	}`)
	chunk2 := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": []},
			"index": 0,
			"finishReason": "MALFORMED_FUNCTION_CALL"
		}],
		"modelVersion": "m",
		"usageMetadata": {
			"promptTokenCount": 50,
			"candidatesTokenCount": 10,
			"thoughtsTokenCount": 313,
			"totalTokenCount": 373
		}
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, chunk1, &param), nil)
	output = append(output, bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, chunk2, &param), nil)...)
	output = append(output, bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	stopIdx := strings.Index(outputText, "event: content_block_stop\n")
	errIdx := strings.Index(outputText, "event: error\n")
	if stopIdx < 0 || errIdx < 0 || stopIdx >= errIdx {
		t.Fatalf("expected content_block_stop before error event in output:\n%s", outputText)
	}
	if strings.Contains(outputText, `"type":"message_delta"`) || strings.Contains(outputText, `"type":"message_stop"`) {
		t.Fatalf("unexpected normal completion events after MALFORMED_FUNCTION_CALL:\n%s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeStream_MalformedFunctionCallWithoutUsageAndDuplicateTerminalChunk(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	chunkWithoutUsage := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": []},
			"index": 0,
			"finishReason": "MALFORMED_FUNCTION_CALL"
		}],
		"modelVersion": "m"
	}`)
	duplicateTerminalChunk := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": "late text"}]},
			"index": 0,
			"finishReason": "MALFORMED_FUNCTION_CALL"
		}],
		"modelVersion": "m",
		"usageMetadata": {
			"promptTokenCount": 50,
			"candidatesTokenCount": 10,
			"totalTokenCount": 60
		}
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, chunkWithoutUsage, &param), nil)
	lateOutput := bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, duplicateTerminalChunk, &param), nil)
	doneOutput := bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)
	if len(lateOutput) != 0 {
		t.Fatalf("expected duplicate terminal chunk after error to emit nothing, got:\n%s", string(lateOutput))
	}
	if len(doneOutput) != 0 {
		t.Fatalf("expected [DONE] after error to emit nothing, got:\n%s", string(doneOutput))
	}
	outputText := string(output)
	if got := strings.Count(outputText, "event: error\n"); got != 1 {
		t.Fatalf("error event count = %d, want 1. Output:\n%s", got, outputText)
	}
}

func TestConvertGeminiResponseToClaude_MalformedFunctionCallWithToolUsePreservesToolUse(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`)
	rawJSON := []byte(`{
		"candidates": [{
			"content": {
				"role": "model",
				"parts": [{"functionCall": {"name": "Bash", "args": {"command": "ls"}}}]
			},
			"index": 0,
			"finishReason": "MALFORMED_FUNCTION_CALL"
		}],
		"usageMetadata": {
			"promptTokenCount": 50,
			"candidatesTokenCount": 10,
			"totalTokenCount": 60
		},
		"modelVersion": "m",
		"responseId": "resp-tool"
	}`)

	ctx := context.Background()
	nonStream := ConvertGeminiResponseToClaudeNonStream(ctx, "claude-opus-5-5", requestJSON, requestJSON, rawJSON, nil)
	nonStreamJSON := gjson.ParseBytes(nonStream)
	if got := nonStreamJSON.Get("stop_reason").String(); got != "tool_use" {
		t.Fatalf("non-stream stop_reason = %q, want tool_use. Output: %s", got, string(nonStream))
	}

	var param any
	streamOut := bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, rawJSON, &param), nil)
	streamOut = append(streamOut, bytes.Join(ConvertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	streamText := string(streamOut)
	if strings.Contains(streamText, "event: error\n") {
		t.Fatalf("unexpected error event when valid functionCall is present:\n%s", streamText)
	}
	if !strings.Contains(streamText, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason tool_use in stream output:\n%s", streamText)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_MalformedFunctionCallReturnsAPIError(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	rawJSON := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": []},
			"finishReason": "MALFORMED_FUNCTION_CALL"
		}],
		"usageMetadata": {
			"promptTokenCount": 120,
			"candidatesTokenCount": 37,
			"totalTokenCount": 157
		},
		"modelVersion": "m",
		"responseId": "resp-test"
	}`)

	output := ConvertGeminiResponseToClaudeNonStream(context.Background(), "claude-opus-5-5", requestJSON, requestJSON, rawJSON, nil)
	outputJSON := gjson.ParseBytes(output)
	if got := outputJSON.Get("stop_reason").String(); got == "refusal" {
		t.Fatalf("unexpected stop_reason = %q. Output: %s", got, string(output))
	}
	if got := outputJSON.Get("type").String(); got != "error" {
		t.Fatalf("type = %q, want error. Output: %s", got, string(output))
	}
	if got := outputJSON.Get("error.type").String(); got != "api_error" {
		t.Fatalf("error.type = %q, want api_error. Output: %s", got, string(output))
	}
	if !strings.Contains(outputJSON.Get("error.message").String(), "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("error.message = %q, want mention of MALFORMED_FUNCTION_CALL. Output: %s", outputJSON.Get("error.message").String(), string(output))
	}
}

func TestConvertGeminiResponseToClaudeNonStream_UniqueToolUseIDsAcrossTurns(t *testing.T) {
	atomic.StoreUint64(&toolUseIDCounter, 0)

	requestJSON := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"run command"}]}`)
	rawResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"command": "ls"}}}
				],
				"role": "model"
			},
			"finishReason": "STOP"
		}],
		"modelVersion": "gemini-4-argon",
		"responseId": "resp-tool"
	}`)

	ctx := context.Background()

	var param any
	streamBytes := bytes.Join(ConvertGeminiResponseToClaude(ctx, "gemini-4-argon", requestJSON, requestJSON, rawResponse, &param), nil)
	streamBytes = append(streamBytes, bytes.Join(ConvertGeminiResponseToClaude(ctx, "gemini-4-argon", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	var streamToolID string
	for _, line := range strings.Split(string(streamBytes), "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"tool_use"`) {
			streamToolID = gjson.Get(strings.TrimPrefix(line, "data: "), "content_block.id").String()
			break
		}
	}
	if streamToolID == "" {
		t.Fatalf("expected tool_use id in streamed response, got: %s", string(streamBytes))
	}

	nonStream1 := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-4-argon", requestJSON, requestJSON, rawResponse, nil)
	nonStreamID1 := gjson.GetBytes(nonStream1, "content.0.id").String()
	if nonStreamID1 == "" {
		t.Fatalf("expected tool_use id in first non-stream response, got: %s", string(nonStream1))
	}

	nonStream2 := ConvertGeminiResponseToClaudeNonStream(ctx, "gemini-4-argon", requestJSON, requestJSON, rawResponse, nil)
	nonStreamID2 := gjson.GetBytes(nonStream2, "content.0.id").String()
	if nonStreamID2 == "" {
		t.Fatalf("expected tool_use id in second non-stream response, got: %s", string(nonStream2))
	}

	if nonStreamID1 == streamToolID {
		t.Fatalf("non-stream tool_use id %q collided with prior streamed tool_use id %q", nonStreamID1, streamToolID)
	}
	if nonStreamID1 == nonStreamID2 {
		t.Fatalf("non-stream tool_use ids repeated across turns: first=%q second=%q", nonStreamID1, nonStreamID2)
	}
}

// --- R6 byte-compat pin ------------------------------------------------------
// Hashes captured from the pre-port implementation and held identical across the
// unambiguous-name-map port in this package (plan v8040-p3, upstream 83189e5).

var byteCompatHashes = map[string]string{
	"stream":    "bf5eb00c5416597455df446ee7f459a94d5131930abb2fa9e2828aaee1ad0e1e",
	"nonstream": "3c1bfea1c239c927b3c9b625049727a04ecd67f541b7bc11d0631182e670a9ed",
}

// byteCompatVolatile masks timestamp and synthesized-id byte runs (Unix seconds,
// UnixNano decimal or hex) so the pin holds everything else exactly; no field
// order, name, namespace or echo field is masked.
var byteCompatVolatile = regexp.MustCompile(`[0-9a-f]{10,}`)

func byteCompatHash(frames [][]byte) string {
	h := sha256.New()
	for _, frame := range frames {
		masked := byteCompatVolatile.ReplaceAll(frame, []byte("<volatile>"))
		fmt.Fprintf(h, "%d:", len(masked))
		_, _ = h.Write(masked)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func checkByteCompat(t *testing.T, name string, frames [][]byte) {
	t.Helper()
	got := byteCompatHash(frames)
	want := byteCompatHashes[name]
	if want == "" {
		t.Fatalf("bytecompat capture %s: %s", name, got)
	}
	if got != want {
		t.Fatalf("%s byte-compat hash = %s, want %s", name, got, want)
	}
}

func TestByteCompatGeminiClaude(t *testing.T) {
	atomic.StoreUint64(&toolUseIDCounter, 0)
	request := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read"},{"name":"read"},{"name":"Write"}]}`)

	var param any
	var stream [][]byte
	for _, frame := range []string{
		`{"responseId":"r1","modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
		`{"responseId":"r1","modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{"p":1}}}]},"finishReason":"STOP"}]}`,
		`[DONE]`,
	} {
		stream = append(stream, ConvertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", request, request, []byte(frame), &param)...)
	}
	checkByteCompat(t, "stream", stream)

	nonStream := ConvertGeminiResponseToClaudeNonStream(
		context.Background(),
		"gemini-2.5-pro",
		request,
		request,
		[]byte(`{"responseId":"r2","modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"functionCall":{"name":"Write","args":{}}},{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`),
		nil,
	)
	checkByteCompat(t, "nonstream", [][]byte{nonStream})
}

// TestConvertGeminiResponseToClaude_ImplicitOpenToolContinuationOrdering pins the tool
// input a Claude client observes on the real Gemini->Claude response path when the tool
// schema only permits dynamic properties (additionalProperties omitted). The client
// concatenates every input_json_delta.partial_json into the tool input, so an empty "{}"
// chunk followed by the real arguments must not produce `{}{"user":"v"}`; an empty chunk
// must never re-emit after real arguments either.
func TestConvertGeminiResponseToClaude_ImplicitOpenToolContinuationOrdering(t *testing.T) {
	origRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"echo","input_schema":{"type":"object","properties":{}}}]}`)
	translatedRequest := ConvertClaudeRequestToGemini("gemini-3-flash", origRequest, false)
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		firstArgs string
		contArgs  string
		want      string
	}{
		{"empty_then_real_args", `{}`, `{"user":"v"}`, `{"user":"v"}`},
		{"placeholder_then_real_args", `{"reason":"thinking"}`, `{"user":"v"}`, `{"user":"v"}`},
		{"real_args_then_empty", `{"user":"v"}`, `{}`, `{"user":"v"}`},
		{"real_args_then_placeholder", `{"user":"v"}`, `{"reason":"done"}`, `{"user":"v"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstChunk := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":` + tc.firstArgs + `}}],"role":"model"}}],"modelVersion":"gemini-3-flash","responseId":"resp-echo"}`)
			continuationChunk := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"args":` + tc.contArgs + `}}],"role":"model"},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","responseId":"resp-echo"}`)

			var param any
			stream := ConvertGeminiResponseToClaude(ctx, "gemini-3-flash", origRequest, translatedRequest, firstChunk, &param)
			stream = append(stream, ConvertGeminiResponseToClaude(ctx, "gemini-3-flash", origRequest, translatedRequest, continuationChunk, &param)...)
			streamText := string(bytes.Join(stream, nil))
			if got := geminiClaudeInputJSONDeltas(t, streamText); got != tc.want {
				t.Fatalf("expected concatenated partial_json %s, got %q (stream: %s)", tc.want, got, streamText)
			}
			var toolInputs int
			for _, line := range strings.Split(streamText, "\n") {
				if strings.HasPrefix(line, "data: ") && gjson.Get(strings.TrimPrefix(line, "data: "), "content_block.type").String() == "tool_use" {
					toolInputs++
				}
			}
			if toolInputs != 1 {
				t.Fatalf("expected exactly one tool_use block, got %d (stream: %s)", toolInputs, streamText)
			}
		})
	}

	// The closed direction is unchanged behavior: additionalProperties:false declares a
	// genuinely parameterless tool, so its arguments still normalize to "{}".
	t.Run("explicitly_closed_schema_still_normalizes", func(t *testing.T) {
		closedRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"echo","input_schema":{"type":"object","additionalProperties":false}}]}`)
		closedTranslated := ConvertClaudeRequestToGemini("gemini-3-flash", closedRequest, false)
		chunk := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":{"user":"v"}}}],"role":"model"},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash","responseId":"resp-closed"}`)
		var param any
		stream := ConvertGeminiResponseToClaude(ctx, "gemini-3-flash", closedRequest, closedTranslated, chunk, &param)
		streamText := string(bytes.Join(stream, nil))
		if got := geminiClaudeInputJSONDeltas(t, streamText); got != `{}` {
			t.Fatalf("expected explicitly closed schema to normalize to {}, got %q (stream: %s)", got, streamText)
		}
	})
}
