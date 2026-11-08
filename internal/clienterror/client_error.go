// Package clienterror classifies upstream failures caused by the client request.
package clienterror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// StatusClientClosedRequest is the nginx-style status used when the client
// aborts the request before the proxy finishes (context.Canceled).
const StatusClientClosedRequest = 499

var requestFaultCodes = map[string]struct{}{
	"cyber_policy":                {},
	"context_length_exceeded":     {},
	"message_too_big":             {},
	"string_above_max_length":     {},
	"invalid_prompt":              {},
	"invalid_value":               {},
	"unsupported_value":           {},
	"invalid_request_error":       {},
	"previous_response_not_found": {},
}

var requestFaultTypes = map[string]struct{}{
	"invalid_request":       {},
	"invalid_request_error": {},
	"bad_request_error":     {},
	"invalid_prompt":        {},
}

// HTTPStatusFromError extracts an HTTP status from err.
// Explicit StatusCode() values win. Otherwise context.Canceled maps to 499
// and context.DeadlineExceeded maps to 504. Returns 0 when unknown.
func HTTPStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		if code := sc.StatusCode(); code > 0 {
			return code
		}
	}
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return 0
}

// HTTPStatusFromErrorOr is like HTTPStatusFromError but returns fallback when
// the error does not carry a known status.
func HTTPStatusFromErrorOr(err error, fallback int) int {
	if code := HTTPStatusFromError(err); code > 0 {
		return code
	}
	return fallback
}

// IsRequestFault reports whether an upstream failure is caused by the request
// and therefore must not rotate or penalize credentials.
func IsRequestFault(status int, err error) bool {
	if status <= 0 && err != nil {
		type statusCoder interface {
			StatusCode() int
		}
		var statusErr statusCoder
		if errors.As(err, &statusErr) && statusErr != nil {
			status = statusErr.StatusCode()
		}
	}
	// Payment and rate-limit statuses are authoritative even when an upstream
	// pairs them with a generic invalid_request_error body. The credential must
	// remain eligible for cooldown and rotation.
	if status == http.StatusPaymentRequired || status == http.StatusTooManyRequests {
		return false
	}
	// Anthropic reports an exhausted API credit balance as 400 with an
	// invalid_request_error body. Preserve credential cooldown and rotation
	// without weakening generic 400 request-fault handling.
	if IsCreditBalanceTooLow(status, err) {
		return false
	}
	if hasRequestFaultBody(err) {
		return true
	}
	if err != nil && (IsItemNotPersisted(err.Error()) || IsGeminiFilterBlockError(err) || IsClaudePolicyRefusalError(err)) {
		return true
	}
	switch status {
	case http.StatusBadRequest,
		http.StatusConflict,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// IsItemNotPersisted matches the upstream 404 raised when a request references a
// response item the upstream never stored because `store` was false. The upstream
// sends this as a plain-text message rather than a JSON body, so it cannot be
// recognized through the structured identifiers above.
//
// The request can only succeed once the client rebuilds it without the stale
// reference, so it is a request fault: rotating credentials cannot help, and the
// client must be told rather than left to retry the same broken input.
func IsItemNotPersisted(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

// GeminiFilterBlockMessagePrefix is the leading text of Gemini's mid-stream content filter block frame.
const GeminiFilterBlockMessagePrefix = "This request was blocked by Gemini's filters."

// GeminiFilterBlockPolicyURL is the policy URL embedded in Gemini's mid-stream content filter block frame.
const GeminiFilterBlockPolicyURL = "https://policies.google.com/terms/generative-ai/use-policy"

// GeminiFilterBlockMessage inspects a raw Gemini / Antigravity JSON payload and returns the filter
// block message when the payload is an injected mid-stream Gemini content filter frame.
//
// Upstream injects this frame inside an HTTP 200 SSE stream with only candidates[0].content.parts[0].text
// and without content.role, responseId, modelVersion, or usageMetadata (upstream c3938b72bdaa,
// be69162478da).
func GeminiFilterBlockMessage(payload []byte) (string, bool) {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(GeminiFilterBlockMessagePrefix)) || !gjson.ValidBytes(payload) {
		return "", false
	}
	for _, path := range []string{"responseId", "response.responseId", "modelVersion", "response.modelVersion", "usageMetadata", "response.usageMetadata", "cpaUsageMetadata", "response.cpaUsageMetadata"} {
		if gjson.GetBytes(payload, path).Exists() {
			return "", false
		}
	}
	for _, prefix := range []string{"candidates.0", "response.candidates.0"} {
		candidate := gjson.GetBytes(payload, prefix)
		if !candidate.Exists() {
			continue
		}
		if role := strings.TrimSpace(candidate.Get("content.role").String()); role != "" {
			return "", false
		}
		if finishReason := strings.TrimSpace(candidate.Get("finishReason").String()); finishReason != "" {
			return "", false
		}
		text := strings.TrimSpace(candidate.Get("content.parts.0.text").String())
		if isGeminiFilterBlockText(text) {
			return text, true
		}
	}
	return "", false
}

// IsGeminiFilterBlockError reports whether err carries a Gemini mid-stream content filter block message.
func IsGeminiFilterBlockError(err error) bool {
	if err == nil {
		return false
	}
	if isGeminiFilterBlockText(err.Error()) {
		return true
	}
	type responseBodyProvider interface {
		ResponseBody() []byte
	}
	var responseBody responseBodyProvider
	if errors.As(err, &responseBody) && responseBody != nil && len(responseBody.ResponseBody()) > 0 {
		if _, ok := GeminiFilterBlockMessage(responseBody.ResponseBody()); ok {
			return true
		}
		if isGeminiFilterBlockText(string(responseBody.ResponseBody())) {
			return true
		}
	}
	return false
}

func isGeminiFilterBlockText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, GeminiFilterBlockMessagePrefix) &&
		strings.Contains(trimmed, GeminiFilterBlockPolicyURL)
}

// ClaudePolicyRefusalMessage inspects a Claude Messages JSON payload (either a non-stream
// message object or a streaming message_delta event) and returns the refusal explanation
// when stop_details indicates a policy/cyber refusal block.
func ClaudePolicyRefusalMessage(payload []byte) (string, bool) {
	if len(payload) == 0 || !bytes.Contains(payload, []byte("stop_details")) || !gjson.ValidBytes(payload) {
		return "", false
	}
	for _, prefix := range []string{"delta", ""} {
		stopReasonPath := "stop_reason"
		stopDetailsPath := "stop_details"
		if prefix != "" {
			stopReasonPath = prefix + ".stop_reason"
			stopDetailsPath = prefix + ".stop_details"
		}
		stopReason := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, stopReasonPath).String()))
		details := gjson.GetBytes(payload, stopDetailsPath)
		if !details.Exists() || details.Type != gjson.JSON {
			continue
		}
		detailsType := strings.ToLower(strings.TrimSpace(details.Get("type").String()))
		category := strings.ToLower(strings.TrimSpace(details.Get("category").String()))
		explanation := strings.TrimSpace(details.Get("explanation").String())
		if stopReason == "refusal" || detailsType == "refusal" || category == "cyber" {
			if explanation != "" {
				if category != "" {
					return "claude refusal (" + category + "): " + explanation, true
				}
				return "claude refusal: " + explanation, true
			}
			if category != "" {
				return "claude refusal (" + category + ")", true
			}
			if raw := strings.TrimSpace(details.Raw); raw != "" {
				return "claude refusal: " + raw, true
			}
			return "claude refusal", true
		}
	}
	return "", false
}

// IsClaudePolicyRefusalError reports whether err represents a Claude policy/cyber refusal block.
func IsClaudePolicyRefusalError(err error) bool {
	if err == nil {
		return false
	}
	if isClaudePolicyRefusalText(err.Error()) {
		return true
	}
	if _, ok := ClaudePolicyRefusalMessage([]byte(err.Error())); ok {
		return true
	}
	type responseBodyProvider interface {
		ResponseBody() []byte
	}
	var responseBody responseBodyProvider
	if errors.As(err, &responseBody) && responseBody != nil && len(responseBody.ResponseBody()) > 0 {
		if _, ok := ClaudePolicyRefusalMessage(responseBody.ResponseBody()); ok {
			return true
		}
		if isClaudePolicyRefusalText(string(responseBody.ResponseBody())) {
			return true
		}
	}
	return false
}

func isClaudePolicyRefusalText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return lower == "claude refusal" ||
		strings.HasPrefix(lower, "claude refusal (") ||
		strings.HasPrefix(lower, "claude refusal:") ||
		(strings.Contains(lower, "triggered restrictions on violative cyber content") &&
			strings.Contains(lower, "anthropic's usage policy"))
}

// IsCreditBalanceTooLow reports the Anthropic 400 response returned when a
// credential's API credit balance is exhausted.
func IsCreditBalanceTooLow(status int, err error) bool {
	if status <= 0 && err != nil {
		status = HTTPStatusFromError(err)
	}
	if status != http.StatusBadRequest || err == nil {
		return false
	}
	return hasCreditBalanceTooLowBody(err)
}

func hasCreditBalanceTooLowBody(err error) bool {
	if err == nil {
		return false
	}
	body := strings.TrimSpace(err.Error())
	type responseBodyProvider interface {
		ResponseBody() []byte
	}
	var responseBody responseBodyProvider
	if errors.As(err, &responseBody) && responseBody != nil && len(responseBody.ResponseBody()) > 0 {
		body = strings.TrimSpace(string(responseBody.ResponseBody()))
	}
	if body == "" || !json.Valid([]byte(body)) {
		return false
	}
	for _, path := range []string{"error.message", "message", "response.error.message", "body.error.message"} {
		msg := strings.ToLower(strings.TrimSpace(gjson.Get(body, path).String()))
		if strings.HasPrefix(msg, "your credit balance is too low") {
			return true
		}
	}
	return false
}

func hasRequestFaultBody(err error) bool {
	if err == nil {
		return false
	}
	body := strings.TrimSpace(err.Error())
	if body == "" || !json.Valid([]byte(body)) {
		return false
	}
	for _, path := range []string{"error.code", "code", "response.error.code", "body.error.code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.Get(body, path).String()))
		if _, ok := requestFaultCodes[code]; ok {
			return true
		}
	}
	for _, path := range []string{"error.type", "type", "response.error.type", "body.error.type"} {
		errType := strings.ToLower(strings.TrimSpace(gjson.Get(body, path).String()))
		if _, ok := requestFaultTypes[errType]; ok {
			return true
		}
	}
	return false
}
