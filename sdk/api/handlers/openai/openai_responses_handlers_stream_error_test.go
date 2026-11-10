package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/interfaces"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	sdkconfig "github.com/therealtinhtute/llmhub/sdk/config"
)

func TestForwardResponsesStreamTerminalErrorUsesResponsesErrorChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("unexpected EOF")}
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected responses error chunk, got: %q", body)
	}
	if strings.Contains(body, `"error":{`) {
		t.Fatalf("expected streaming error chunk (top-level type), got HTTP error body: %q", body)
	}
}

// forwardResponsesStreamOverflowBody drives the handler's stream-error seam with
// a Claude prompt-overflow error and returns the SSE body it wrote.
func forwardResponsesStreamOverflowBody(t *testing.T, originator string) string {
	t.Helper()
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if originator != "" {
		c.Request.Header.Set("Originator", originator)
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`),
	}
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	return recorder.Body.String()
}

// TestForwardResponsesStreamPromptOverflowCarriesContextLengthExceeded asserts
// the wiring a client sees: the code is on the stream-error payload for both the
// default error event and the Codex response.failed event.
func TestForwardResponsesStreamPromptOverflowCarriesContextLengthExceeded(t *testing.T) {
	body := forwardResponsesStreamOverflowBody(t, "")
	if !strings.Contains(body, "event: error") {
		t.Fatalf("default path missing error event: %q", body)
	}
	if !strings.Contains(body, `"code":"context_length_exceeded"`) {
		t.Fatalf("default path missing context_length_exceeded: %q", body)
	}

	codexBody := forwardResponsesStreamOverflowBody(t, "codex_cli_rs/0.1.0")
	if !strings.Contains(codexBody, "event: response.failed") {
		t.Fatalf("codex path missing response.failed: %q", codexBody)
	}
	if !strings.Contains(codexBody, `"code":"context_length_exceeded"`) {
		t.Fatalf("codex path missing context_length_exceeded: %q", codexBody)
	}
	if !strings.Contains(codexBody, `"type":"invalid_request_error"`) || !strings.Contains(codexBody, "prompt is too long") {
		t.Fatalf("codex path lost the upstream error detail: %q", codexBody)
	}
}

func TestResponsesSSEPayloadErrorMessage_ImageGenerationUserErrorMapsToBadRequest(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"type":"response.failed","response":{"status":"failed","error":{"type":"image_generation_user_error","code":"invalid_value","message":"Complexity 'medium' is not supported for this image model.","param":"complexity"}}}`),
		[]byte(`{"type":"error","error":{"type":"image_generation_user_error","code":"invalid_value","message":"Complexity 'medium' is not supported for this image model.","param":"complexity"}}`),
	}
	for _, payload := range payloads {
		errMsg := responsesSSEPayloadErrorMessage(payload)
		if errMsg == nil {
			t.Fatalf("expected non-nil ErrorMessage for %s", payload)
		}
		if errMsg.StatusCode != http.StatusBadRequest {
			t.Fatalf("StatusCode = %d, want %d for payload %s", errMsg.StatusCode, http.StatusBadRequest, payload)
		}
		if !strings.Contains(errMsg.Error.Error(), "/v1/images/generations") || !strings.Contains(errMsg.Error.Error(), "gpt-5.4-mini") {
			t.Fatalf("expected remediation guidance in error message, got %q", errMsg.Error.Error())
		}
	}
}

func TestResponsesSSEPayloadErrorStatusMapping(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    int
	}{
		{name: "cyber policy", payload: `{"error":{"code":"cyber_policy"}}`, want: http.StatusBadRequest},
		{name: "model not found", payload: `{"error":{"code":"model_not_found"}}`, want: http.StatusNotFound},
		{name: "invalid api key", payload: `{"error":{"code":"invalid_api_key"}}`, want: http.StatusUnauthorized},
		{name: "permission denied", payload: `{"error":{"code":"permission_denied"}}`, want: http.StatusForbidden},
		{name: "rate limit", payload: `{"error":{"code":"rate_limit_exceeded"}}`, want: http.StatusTooManyRequests},
		{name: "image generation user error", payload: `{"response":{"error":{"type":"image_generation_user_error"}}}`, want: http.StatusBadRequest},
		{name: "unknown", payload: `{"error":{"type":"server_error"}}`, want: http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesSSEPayloadErrorStatus([]byte(tc.payload)); got != tc.want {
				t.Fatalf("responsesSSEPayloadErrorStatus() = %d, want %d", got, tc.want)
			}
		})
	}
}
