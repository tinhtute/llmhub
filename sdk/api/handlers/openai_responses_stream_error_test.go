package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestBuildOpenAIResponsesStreamErrorChunk(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusInternalServerError, "unexpected EOF", 0)
	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	if payload["code"] != "internal_server_error" {
		t.Fatalf("code = %v, want %q", payload["code"], "internal_server_error")
	}
	if payload["message"] != "unexpected EOF" {
		t.Fatalf("message = %v, want %q", payload["message"], "unexpected EOF")
	}
	if payload["sequence_number"] != float64(0) {
		t.Fatalf("sequence_number = %v, want %v", payload["sequence_number"], 0)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkExtractsHTTPErrorBody(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(
		http.StatusInternalServerError,
		`{"error":{"message":"oops","type":"server_error","code":"internal_server_error"}}`,
		0,
	)
	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	if payload["code"] != "internal_server_error" {
		t.Fatalf("code = %v, want %q", payload["code"], "internal_server_error")
	}
	if payload["message"] != "oops" {
		t.Fatalf("message = %v, want %q", payload["message"], "oops")
	}
}

// TestBuildOpenAIResponsesStreamErrorChunkRequestTimeoutIsServerError ports
// upstream CLIProxyAPI commit cb62a6748b99's regression test. The fork's
// legacy error chunk is flat ({type,code,message}) so the error-type
// classification is only observable on the nested response.failed detail.
func TestBuildOpenAIResponsesStreamErrorChunkRequestTimeoutIsServerError(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusRequestTimeout, "stream disconnected before completion", 0)
	var payload struct {
		Type           string         `json:"type"`
		Code           string         `json:"code"`
		Error          map[string]any `json:"error"`
		SequenceNumber int            `json:"sequence_number"`
	}
	if errUnmarshal := json.Unmarshal(chunk, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if payload.Type != "error" {
		t.Fatalf("type = %q, want %q", payload.Type, "error")
	}
	if payload.Code != "request_timeout" {
		t.Fatalf("code = %v, want %q", payload.Code, "request_timeout")
	}
	// When a nested error detail is present it must classify the timeout as a
	// server error so clients know the request is safe to replay.
	if payload.Error != nil {
		if got := payload.Error["type"]; got != "server_error" {
			t.Fatalf("error.type = %v, want %q", got, "server_error")
		}
	}

	failedChunk := BuildOpenAIResponsesStreamFailedChunk(http.StatusRequestTimeout, "stream disconnected before completion", 0)
	var failedPayload struct {
		Type     string `json:"type"`
		Response struct {
			Status string         `json:"status"`
			Error  map[string]any `json:"error"`
		} `json:"response"`
	}
	if errUnmarshal := json.Unmarshal(failedChunk, &failedPayload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if got := failedPayload.Response.Error["code"]; got != "request_timeout" {
		t.Fatalf("failed.response.error.code = %v, want %q", got, "request_timeout")
	}
	if got := failedPayload.Response.Error["type"]; got != "server_error" {
		t.Fatalf("failed.response.error.type = %v, want %q", got, "server_error")
	}
}

// TestBuildOpenAIResponsesStreamChunksMapAnthropicPromptTooLong pins the code a
// Claude prompt overflow must carry on both stream-error shapes, while an error
// that already names its code and an unrelated invalid_request_error stay as the
// upstream reported them.
func TestBuildOpenAIResponsesStreamChunksMapAnthropicPromptTooLong(t *testing.T) {
	promptTooLong := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`

	errorChunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadRequest, promptTooLong, 0)
	var errorPayload struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(errorChunk, &errorPayload); err != nil {
		t.Fatalf("unmarshal error chunk: %v", err)
	}
	if errorPayload.Code != "context_length_exceeded" {
		t.Fatalf("error chunk code = %q, want context_length_exceeded; raw=%s", errorPayload.Code, errorChunk)
	}
	if !strings.HasPrefix(errorPayload.Message, "prompt is too long") {
		t.Fatalf("error chunk message = %q, want the prompt-overflow message", errorPayload.Message)
	}

	failedChunk := BuildOpenAIResponsesStreamFailedChunk(http.StatusBadRequest, promptTooLong, 0)
	var failedPayload struct {
		Type     string `json:"type"`
		Response struct {
			Status string         `json:"status"`
			Error  map[string]any `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(failedChunk, &failedPayload); err != nil {
		t.Fatalf("unmarshal failed chunk: %v", err)
	}
	if failedPayload.Type != "response.failed" || failedPayload.Response.Status != "failed" {
		t.Fatalf("failed chunk shape = %s", failedChunk)
	}
	if got := failedPayload.Response.Error["code"]; got != "context_length_exceeded" {
		t.Fatalf("failed.response.error.code = %v, want context_length_exceeded; raw=%s", got, failedChunk)
	}
	if got := failedPayload.Response.Error["type"]; got != "invalid_request_error" {
		t.Fatalf("failed.response.error.type = %v, want invalid_request_error", got)
	}

	withCode := BuildOpenAIResponsesStreamFailedChunk(http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","code":"custom_code","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`, 0)
	var withCodePayload struct {
		Response struct {
			Error map[string]any `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(withCode, &withCodePayload); err != nil {
		t.Fatalf("unmarshal withCode: %v", err)
	}
	if got := withCodePayload.Response.Error["code"]; got != "custom_code" {
		t.Fatalf("existing code = %v, want custom_code preserved", got)
	}

	otherError := BuildOpenAIResponsesStreamFailedChunk(http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content: Field required"}}`, 0)
	var otherPayload struct {
		Response struct {
			Error map[string]any `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(otherError, &otherPayload); err != nil {
		t.Fatalf("unmarshal other: %v", err)
	}
	if code, hasCode := otherPayload.Response.Error["code"]; hasCode {
		t.Fatalf("unrelated invalid_request_error gained code %v", code)
	}
}
