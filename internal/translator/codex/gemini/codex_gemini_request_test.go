package gemini

import (
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestConvertGeminiRequestToCodex_DeterministicCallIDs(t *testing.T) {
	raw := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "first_tool", "args": {"q": "one"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "first_tool", "response": {"result": "ok1"}}}
				]
			},
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "second_tool", "args": {"q": "two"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "second_tool", "response": {"result": "ok2"}}}
				]
			}
		]
	}`)

	out1 := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)
	out2 := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)

	if string(out1) != string(out2) {
		t.Fatalf("expected deterministic output across multiple conversions, got different outputs:\nout1=%s\nout2=%s", string(out1), string(out2))
	}

	wantID1 := "call_gemini_0000000000000001"
	wantID2 := "call_gemini_0000000000000002"

	gotCall1 := gjson.GetBytes(out1, "input.0.call_id").String()
	gotResp1 := gjson.GetBytes(out1, "input.1.call_id").String()
	gotCall2 := gjson.GetBytes(out1, "input.2.call_id").String()
	gotResp2 := gjson.GetBytes(out1, "input.3.call_id").String()

	if gotCall1 != wantID1 || gotResp1 != wantID1 {
		t.Fatalf("expected first tool pair to have id %q, got call=%q, resp=%q", wantID1, gotCall1, gotResp1)
	}
	if gotCall2 != wantID2 || gotResp2 != wantID2 {
		t.Fatalf("expected second tool pair to have id %q, got call=%q, resp=%q", wantID2, gotCall2, gotResp2)
	}
}

func testCodexGeminiMetaSignature() string {
	decoded := make([]byte, 45+1024)
	copy(decoded, []byte{0x43, 0xe3, 0xda, 0x0e, 0x01})
	return base64.RawURLEncoding.EncodeToString(decoded)
}

func testCodexGeminiGPTSignature() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func TestConvertGeminiRequestToCodex_PreservesMetaThoughtSignatureForMuseTarget(t *testing.T) {
	signature := testCodexGeminiMetaSignature()
	payload := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":""},{"text":"visible answer"}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
	payload, _ = sjson.SetBytes(payload, "contents.0.parts.0.thoughtSignature", signature)

	out := ConvertGeminiRequestToCodex("muse-spark-1.3", payload, false)
	input := gjson.GetBytes(out, "input").Array()

	var reasoning []string
	for _, item := range input {
		if item.Get("type").String() == "reasoning" {
			reasoning = append(reasoning, item.Get("encrypted_content").String())
			if got := item.Get("summary.0.text").String(); got != "internal reasoning" {
				t.Fatalf("reasoning summary = %q, want the thought text; output=%s", got, out)
			}
		}
	}
	if len(reasoning) != 1 || reasoning[0] != signature {
		t.Fatalf("reasoning encrypted_content = %v, want the Muse envelope; output=%s", reasoning, out)
	}
	// The thought text travels as reasoning, not as visible content.
	for _, item := range input {
		for _, part := range item.Get("content").Array() {
			if part.Get("text").String() == "internal reasoning" {
				t.Fatalf("thought text leaked into visible content; output=%s", out)
			}
		}
	}
	visible := false
	for _, item := range input {
		for _, part := range item.Get("content").Array() {
			if part.Get("text").String() == "visible answer" {
				visible = true
			}
		}
	}
	if !visible {
		t.Fatalf("visible answer missing; output=%s", out)
	}
}

func TestConvertGeminiRequestToCodex_DropsMetaThoughtSignatureForNonMetaTargets(t *testing.T) {
	signature := testCodexGeminiMetaSignature()
	payload := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":""},{"text":"visible answer"}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
	payload, _ = sjson.SetBytes(payload, "contents.0.parts.0.thoughtSignature", signature)

	for _, modelName := range []string{"gpt-5.1-codex", "claude-sonnet-4-6", "grok-4.5"} {
		t.Run(modelName, func(t *testing.T) {
			out := ConvertGeminiRequestToCodex(modelName, payload, false)
			for _, item := range gjson.GetBytes(out, "input").Array() {
				if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").String() == signature {
					t.Fatalf("Muse envelope leaked to %s; output=%s", modelName, out)
				}
			}
		})
	}
}

func TestConvertGeminiRequestToCodex_NormalizesCarrierAndPrefixedSignatures(t *testing.T) {
	gptSignature := testCodexGeminiGPTSignature()
	metaSignature := testCodexGeminiMetaSignature()
	cases := []struct {
		name      string
		model     string
		echoed    string
		want      string
		wantFound bool
	}{
		{"gpt cache prefixed", "gpt-5.1-codex", "gpt#" + gptSignature, gptSignature, true},
		{"carrier wrapped gpt prefixed", "gpt-5.1-codex", encodeGeminiCodexThoughtSignature("gpt#" + gptSignature), gptSignature, true},
		{"carrier wrapped meta", "muse-spark-1.3", encodeGeminiCodexThoughtSignature(metaSignature), metaSignature, true},
		{"carrier wrapped meta to gpt target", "gpt-5.1-codex", encodeGeminiCodexThoughtSignature(metaSignature), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":""},{"text":"visible answer"}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
			payload, _ = sjson.SetBytes(payload, "contents.0.parts.0.thoughtSignature", tc.echoed)

			out := ConvertGeminiRequestToCodex(tc.model, payload, false)
			found := ""
			for _, item := range gjson.GetBytes(out, "input").Array() {
				if item.Get("type").String() == "reasoning" {
					found = item.Get("encrypted_content").String()
				}
			}
			if tc.wantFound && found != tc.want {
				t.Fatalf("encrypted_content = %q, want %q; output=%s", found, tc.want, out)
			}
			if !tc.wantFound && found != "" {
				t.Fatalf("foreign signature leaked as a reasoning item; output=%s", out)
			}
		})
	}
}
