package util

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ResponsesRequestEcho carries the typed values of the Responses request fields
// echoed into response objects.
//
// Extraction runs exactly the gjson.Get lookups the per-event code used to run,
// but once per stream; Apply writes the fields in the same fixed order with the
// same typed conversions. That keeps every emitted byte identical while the
// request body is no longer retained or re-scanned per event (plan v8040-p3,
// upstream 3b1531453e54 and f4a2d0408b1d).
type ResponsesRequestEcho struct {
	instructions          string
	hasInstructions       bool
	maxOutputTokens       int64
	hasMaxOutputTokens    bool
	maxToolCalls          int64
	hasMaxToolCalls       bool
	model                 string
	hasModel              bool
	parallelToolCalls     bool
	hasParallelToolCalls  bool
	previousResponseID    string
	hasPreviousResponseID bool
	promptCacheKey        string
	hasPromptCacheKey     bool
	reasoning             any
	hasReasoning          bool
	safetyIdentifier      string
	hasSafetyIdentifier   bool
	serviceTier           string
	hasServiceTier        bool
	store                 bool
	hasStore              bool
	temperature           float64
	hasTemperature        bool
	text                  any
	hasText               bool
	toolChoice            any
	hasToolChoice         bool
	tools                 any
	hasTools              bool
	topLogprobs           int64
	hasTopLogprobs        bool
	topP                  float64
	hasTopP               bool
	truncation            string
	hasTruncation         bool
	user                  any
	hasUser               bool
	metadata              any
	hasMetadata           bool
}

// ExtractResponsesRequestEcho reads the echoed fields from a request root. Pass
// the exact root the per-event code used (unwrap a "request" wrapper first when
// the caller did).
func ExtractResponsesRequestEcho(root gjson.Result) ResponsesRequestEcho {
	var e ResponsesRequestEcho
	if v := root.Get("instructions"); v.Exists() {
		e.instructions, e.hasInstructions = v.String(), true
	}
	if v := root.Get("max_output_tokens"); v.Exists() {
		e.maxOutputTokens, e.hasMaxOutputTokens = v.Int(), true
	}
	if v := root.Get("max_tool_calls"); v.Exists() {
		e.maxToolCalls, e.hasMaxToolCalls = v.Int(), true
	}
	if v := root.Get("model"); v.Exists() {
		e.model, e.hasModel = v.String(), true
	}
	if v := root.Get("parallel_tool_calls"); v.Exists() {
		e.parallelToolCalls, e.hasParallelToolCalls = v.Bool(), true
	}
	if v := root.Get("previous_response_id"); v.Exists() {
		e.previousResponseID, e.hasPreviousResponseID = v.String(), true
	}
	if v := root.Get("prompt_cache_key"); v.Exists() {
		e.promptCacheKey, e.hasPromptCacheKey = v.String(), true
	}
	if v := root.Get("reasoning"); v.Exists() {
		e.reasoning, e.hasReasoning = v.Value(), true
	}
	if v := root.Get("safety_identifier"); v.Exists() {
		e.safetyIdentifier, e.hasSafetyIdentifier = v.String(), true
	}
	if v := root.Get("service_tier"); v.Exists() {
		e.serviceTier, e.hasServiceTier = v.String(), true
	}
	if v := root.Get("store"); v.Exists() {
		e.store, e.hasStore = v.Bool(), true
	}
	if v := root.Get("temperature"); v.Exists() {
		e.temperature, e.hasTemperature = v.Float(), true
	}
	if v := root.Get("text"); v.Exists() {
		e.text, e.hasText = v.Value(), true
	}
	if v := root.Get("tool_choice"); v.Exists() {
		e.toolChoice, e.hasToolChoice = v.Value(), true
	}
	if v := root.Get("tools"); v.Exists() {
		e.tools, e.hasTools = v.Value(), true
	}
	if v := root.Get("top_logprobs"); v.Exists() {
		e.topLogprobs, e.hasTopLogprobs = v.Int(), true
	}
	if v := root.Get("top_p"); v.Exists() {
		e.topP, e.hasTopP = v.Float(), true
	}
	if v := root.Get("truncation"); v.Exists() {
		e.truncation, e.hasTruncation = v.String(), true
	}
	if v := root.Get("user"); v.Exists() {
		e.user, e.hasUser = v.Value(), true
	}
	if v := root.Get("metadata"); v.Exists() {
		e.metadata, e.hasMetadata = v.Value(), true
	}
	return e
}

// Apply writes the extracted fields onto dst under prefix in the fixed order the
// pre-cache code used.
func (e ResponsesRequestEcho) Apply(dst []byte, prefix string) []byte {
	if e.hasInstructions {
		dst, _ = sjson.SetBytes(dst, prefix+"instructions", e.instructions)
	}
	if e.hasMaxOutputTokens {
		dst, _ = sjson.SetBytes(dst, prefix+"max_output_tokens", e.maxOutputTokens)
	}
	if e.hasMaxToolCalls {
		dst, _ = sjson.SetBytes(dst, prefix+"max_tool_calls", e.maxToolCalls)
	}
	if e.hasModel {
		dst, _ = sjson.SetBytes(dst, prefix+"model", e.model)
	}
	if e.hasParallelToolCalls {
		dst, _ = sjson.SetBytes(dst, prefix+"parallel_tool_calls", e.parallelToolCalls)
	}
	if e.hasPreviousResponseID {
		dst, _ = sjson.SetBytes(dst, prefix+"previous_response_id", e.previousResponseID)
	}
	if e.hasPromptCacheKey {
		dst, _ = sjson.SetBytes(dst, prefix+"prompt_cache_key", e.promptCacheKey)
	}
	if e.hasReasoning {
		dst, _ = sjson.SetBytes(dst, prefix+"reasoning", e.reasoning)
	}
	if e.hasSafetyIdentifier {
		dst, _ = sjson.SetBytes(dst, prefix+"safety_identifier", e.safetyIdentifier)
	}
	if e.hasServiceTier {
		dst, _ = sjson.SetBytes(dst, prefix+"service_tier", e.serviceTier)
	}
	if e.hasStore {
		dst, _ = sjson.SetBytes(dst, prefix+"store", e.store)
	}
	if e.hasTemperature {
		dst, _ = sjson.SetBytes(dst, prefix+"temperature", e.temperature)
	}
	if e.hasText {
		dst, _ = sjson.SetBytes(dst, prefix+"text", e.text)
	}
	if e.hasToolChoice {
		dst, _ = sjson.SetBytes(dst, prefix+"tool_choice", e.toolChoice)
	}
	if e.hasTools {
		dst, _ = sjson.SetBytes(dst, prefix+"tools", e.tools)
	}
	if e.hasTopLogprobs {
		dst, _ = sjson.SetBytes(dst, prefix+"top_logprobs", e.topLogprobs)
	}
	if e.hasTopP {
		dst, _ = sjson.SetBytes(dst, prefix+"top_p", e.topP)
	}
	if e.hasTruncation {
		dst, _ = sjson.SetBytes(dst, prefix+"truncation", e.truncation)
	}
	if e.hasUser {
		dst, _ = sjson.SetBytes(dst, prefix+"user", e.user)
	}
	if e.hasMetadata {
		dst, _ = sjson.SetBytes(dst, prefix+"metadata", e.metadata)
	}
	return dst
}
