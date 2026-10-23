package gemini

import (
	"testing"

	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

func geminiThoughtSignatureRoundTripRequest() []byte {
	return []byte(`{
		"contents": [
			{"role": "user", "parts": [{"text": "weather in Paris and London?"}]},
			{
				"role": "model",
				"parts": [
					{
						"functionCall": {"name": "get_weather", "args": {"city": "Paris"}},
						"thoughtSignature": "SIG_PARIS"
					},
					{"functionCall": {"name": "get_weather", "args": {"city": "London"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "get_weather", "response": {"t": 20}}},
					{"functionResponse": {"name": "get_weather", "response": {"t": 15}}}
				]
			}
		]
	}`)
}

// TestConvertGeminiRequestToOpenAI_PreservesFunctionCallThoughtSignature is the
// R3 production path: a Gemini client replays a model functionCall carrying a
// thoughtSignature, and the openai-chat request the upstream receives must still
// carry it (upstream CLIProxyAPI 387063909692).
func TestConvertGeminiRequestToOpenAI_PreservesFunctionCallThoughtSignature(t *testing.T) {
	out := sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatOpenAI, "google/gemini-3-pro", geminiThoughtSignatureRoundTripRequest(), false)

	if got := gjson.GetBytes(out, "messages.1.tool_calls.0.extra_content.google.thought_signature").String(); got != "SIG_PARIS" {
		t.Fatalf("messages.1.tool_calls.0.extra_content.google.thought_signature = %q, want SIG_PARIS; out=%s", got, string(out))
	}
	if gjson.GetBytes(out, "messages.1.tool_calls.1.extra_content").Exists() {
		t.Fatalf("expected the unsigned sibling tool call not to have extra_content; out=%s", string(out))
	}
}

// TestConvertGeminiRequestToOpenAI_ReadsNestedThoughtSignatureSpellings pins
// that every wire spelling of the field is accepted, so a client that echoes the
// signature under functionCall or snake_case still round-trips.
func TestConvertGeminiRequestToOpenAI_ReadsNestedThoughtSignatureSpellings(t *testing.T) {
	for name, part := range map[string]string{
		"part snake_case":         `{"functionCall":{"name":"get_weather","args":{}},"thought_signature":"SIG_A"}`,
		"nested functionCall":     `{"functionCall":{"name":"get_weather","args":{},"thoughtSignature":"SIG_B"}}`,
		"nested openai extension": `{"functionCall":{"name":"get_weather","args":{}},"extra_content":{"google":{"thought_signature":"SIG_C"}}}`,
		"functionCall snake_case": `{"functionCall":{"name":"get_weather","args":{},"thought_signature":"SIG_D"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			payload := []byte(`{"contents":[{"role":"model","parts":[` + part + `]}]}`)
			out := sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatOpenAI, "google/gemini-3-pro", payload, false)
			if got := gjson.GetBytes(out, "messages.0.tool_calls.0.extra_content.google.thought_signature").String(); got == "" {
				t.Fatalf("signature not carried for %s; out=%s", name, string(out))
			}
		})
	}
}
