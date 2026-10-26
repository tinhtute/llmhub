package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type openAIResponsesStreamErrorChunk struct {
	Type           string `json:"type"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	SequenceNumber int    `json:"sequence_number"`
}

// openAIResponsesStreamErrorClass pairs the OpenAI error code and the OpenAI
// error type that a given HTTP status is reported as.
//
// The two are kept together deliberately. They are halves of one classification
// and a client reads both: the code names the condition, the type says whose
// fault it was and therefore whether a retry is allowed. Deriving them in two
// places let them contradict each other, which is how a Codex stream that was
// cut in transit came to be reported with the retryable code "request_timeout"
// alongside the type "invalid_request_error" — a type that tells every
// downstream consumer the request was malformed and must not be replayed.
//
// Ported from upstream CLIProxyAPI commit cb62a6748b99.
type openAIResponsesStreamErrorClass struct {
	code      string
	errorType string
}

const (
	openAIResponsesClientErrorType = "invalid_request_error"
	openAIResponsesServerErrorType = "server_error"
)

// openAIResponsesStreamErrorClasses holds the statuses that do not follow the
// plain "4xx is the caller's fault, 5xx is ours" split.
//
// StatusRequestTimeout is the reason this table exists. A Codex stream that ends
// before response.completed is surfaced with that status, and the request itself
// was well formed: the connection failed, so the caller may replay it.
var openAIResponsesStreamErrorClasses = map[int]openAIResponsesStreamErrorClass{
	http.StatusUnauthorized:    {code: "invalid_api_key", errorType: openAIResponsesClientErrorType},
	http.StatusForbidden:       {code: "insufficient_quota", errorType: openAIResponsesClientErrorType},
	http.StatusTooManyRequests: {code: "rate_limit_exceeded", errorType: openAIResponsesClientErrorType},
	http.StatusNotFound:        {code: "model_not_found", errorType: openAIResponsesClientErrorType},
	http.StatusRequestTimeout:  {code: "request_timeout", errorType: openAIResponsesServerErrorType},
}

func openAIResponsesStreamErrorClassFor(status int) openAIResponsesStreamErrorClass {
	if class, ok := openAIResponsesStreamErrorClasses[status]; ok {
		return class
	}
	if status >= http.StatusInternalServerError {
		return openAIResponsesStreamErrorClass{code: "internal_server_error", errorType: openAIResponsesServerErrorType}
	}
	if status >= http.StatusBadRequest {
		return openAIResponsesStreamErrorClass{code: "invalid_request_error", errorType: openAIResponsesClientErrorType}
	}
	return openAIResponsesStreamErrorClass{code: "unknown_error", errorType: openAIResponsesClientErrorType}
}

func openAIResponsesStreamErrorCode(status int) string {
	return openAIResponsesStreamErrorClassFor(status).code
}

func openAIResponsesStreamErrorType(status int) string {
	return openAIResponsesStreamErrorClassFor(status).errorType
}

// BuildOpenAIResponsesStreamErrorChunk builds an OpenAI Responses streaming error chunk.
//
// Important: OpenAI's HTTP error bodies are shaped like {"error":{...}}; those are valid for
// non-streaming responses, but streaming clients validate SSE `data:` payloads against a union
// of chunks that requires a top-level `type` field.
func BuildOpenAIResponsesStreamErrorChunk(status int, errText string, sequenceNumber int) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if sequenceNumber < 0 {
		sequenceNumber = 0
	}

	message := strings.TrimSpace(errText)
	if message == "" {
		message = http.StatusText(status)
	}

	code := openAIResponsesStreamErrorCode(status)

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		var payload map[string]any
		if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
			if t, ok := payload["type"].(string); ok && strings.TrimSpace(t) == "error" {
				if m, ok := payload["message"].(string); ok && strings.TrimSpace(m) != "" {
					message = strings.TrimSpace(m)
				}
				if v, ok := payload["code"]; ok && v != nil {
					if c, ok := v.(string); ok && strings.TrimSpace(c) != "" {
						code = strings.TrimSpace(c)
					} else {
						code = strings.TrimSpace(fmt.Sprint(v))
					}
				}
				if v, ok := payload["sequence_number"].(float64); ok && sequenceNumber == 0 {
					sequenceNumber = int(v)
				}
			}
			if e, ok := payload["error"].(map[string]any); ok {
				e = applyOpenAIResponsesNestedErrorCode(e)
				if m, ok := e["message"].(string); ok && strings.TrimSpace(m) != "" {
					message = strings.TrimSpace(m)
				}
				if v, ok := e["code"]; ok && v != nil {
					if c, ok := v.(string); ok && strings.TrimSpace(c) != "" {
						code = strings.TrimSpace(c)
					} else {
						code = strings.TrimSpace(fmt.Sprint(v))
					}
				}
			}
		}
	}

	if strings.TrimSpace(code) == "" {
		code = "unknown_error"
	}

	data, err := json.Marshal(openAIResponsesStreamErrorChunk{
		Type:           "error",
		Code:           code,
		Message:        message,
		SequenceNumber: sequenceNumber,
	})
	if err == nil {
		return data
	}

	// Extremely defensive fallback.
	data, _ = json.Marshal(openAIResponsesStreamErrorChunk{
		Type:           "error",
		Code:           "internal_server_error",
		Message:        message,
		SequenceNumber: sequenceNumber,
	})
	if len(data) > 0 {
		return data
	}
	return []byte(`{"type":"error","code":"internal_server_error","message":"internal error","sequence_number":0}`)
}

type openAIResponsesStreamFailedChunk struct {
	Type           string                              `json:"type"`
	SequenceNumber int                                 `json:"sequence_number"`
	Response       openAIResponsesStreamFailedResponse `json:"response"`
}

type openAIResponsesStreamFailedResponse struct {
	Status string         `json:"status"`
	Error  map[string]any `json:"error"`
}

// applyOpenAIResponsesNestedErrorCode fills in the code of an error detail that
// the upstream reported by message only. Anthropic reports input overflow as an
// invalid_request_error whose message starts with "prompt is too long" and no
// code, while Responses clients key on context_length_exceeded to tell a
// too-long prompt (do not retry unchanged) from another bad request.
func applyOpenAIResponsesNestedErrorCode(errorDetail map[string]any) map[string]any {
	if existing, ok := errorDetail["code"]; ok && existing != nil && strings.TrimSpace(fmt.Sprint(existing)) != "" {
		return errorDetail
	}
	if m, _ := errorDetail["message"].(string); errorDetail["type"] == "invalid_request_error" && strings.HasPrefix(m, "prompt is too long") {
		errorDetail["code"] = "context_length_exceeded"
	}
	return errorDetail
}

func openAIResponsesStreamFailedErrorDetail(status int, errText, code, message string) map[string]any {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal([]byte(strings.TrimSpace(errText)), &payload); errUnmarshal == nil {
		if errorDetail, ok := payload["error"].(map[string]any); ok {
			return applyOpenAIResponsesNestedErrorCode(errorDetail)
		}
		if response, ok := payload["response"].(map[string]any); ok {
			if errorDetail, ok := response["error"].(map[string]any); ok {
				return applyOpenAIResponsesNestedErrorCode(errorDetail)
			}
		}
	}

	errorType := openAIResponsesStreamErrorType(status)
	return map[string]any{
		"type":    errorType,
		"code":    code,
		"message": message,
	}
}

// BuildOpenAIResponsesStreamFailedChunk builds the terminal Responses event used by official Codex clients.
// It is intentionally separate from BuildOpenAIResponsesStreamErrorChunk so existing clients keep the legacy shape.
func BuildOpenAIResponsesStreamFailedChunk(status int, errText string, sequenceNumber int) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if sequenceNumber < 0 {
		sequenceNumber = 0
	}

	legacyChunk := BuildOpenAIResponsesStreamErrorChunk(status, errText, sequenceNumber)
	var legacyPayload openAIResponsesStreamErrorChunk
	if errUnmarshal := json.Unmarshal(legacyChunk, &legacyPayload); errUnmarshal != nil {
		legacyPayload.Code = openAIResponsesStreamErrorCode(status)
		legacyPayload.Message = http.StatusText(status)
		legacyPayload.SequenceNumber = sequenceNumber
	}
	if sequenceNumber == 0 && legacyPayload.SequenceNumber > 0 {
		sequenceNumber = legacyPayload.SequenceNumber
	}

	data, errMarshal := json.Marshal(openAIResponsesStreamFailedChunk{
		Type:           "response.failed",
		SequenceNumber: sequenceNumber,
		Response: openAIResponsesStreamFailedResponse{
			Status: "failed",
			Error:  openAIResponsesStreamFailedErrorDetail(status, errText, legacyPayload.Code, legacyPayload.Message),
		},
	})
	if errMarshal == nil {
		return data
	}

	return []byte(`{"type":"response.failed","sequence_number":0,"response":{"status":"failed","error":{"type":"server_error","code":"internal_server_error","message":"internal error"}}}`)
}
