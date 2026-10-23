package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The shared OpenAI-Responses reasoning sanitizer tests live in
// meta_executor_signature_test.go (ported from upstream
// openai_responses_signature_test.go before this file existed). This file
// carries the compat-mode coverage added by upstream commit 81d6ba774621.

func TestSanitizeOpenAIResponsesReasoningEncryptedContentWithCompat_PreservesReasoningContentAndID(t *testing.T) {
	body := []byte(`{"store":false,"input":[` +
		`{"id":"rs_compat","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"keep cleartext thinking"}],"encrypted_content":null},` +
		`{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}` +
		`]}`)

	got := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompat(context.Background(), "test", body, true)

	reasoningItem := gjson.GetBytes(got, "input.0")
	if gotID := reasoningItem.Get("id").String(); gotID != "rs_compat" {
		t.Fatalf("reasoning id = %q, want rs_compat; body=%s", gotID, got)
	}
	content := reasoningItem.Get("content")
	if !content.Exists() || !content.IsArray() || len(content.Array()) != 1 {
		t.Fatalf("content should have 1 item, got %s body=%s", content.Raw, got)
	}
	if gotType := reasoningItem.Get("content.0.type").String(); gotType != "reasoning_text" {
		t.Fatalf("content.0.type = %q, want reasoning_text; body=%s", gotType, got)
	}
	if gotText := reasoningItem.Get("content.0.text").String(); gotText != "keep cleartext thinking" {
		t.Fatalf("content.0.text = %q, want keep cleartext thinking; body=%s", gotText, got)
	}
	if gotLen := len(reasoningItem.Get("summary").Array()); gotLen != 0 {
		t.Fatalf("summary should remain empty for compat, got %d items; body=%s", gotLen, got)
	}
	if reasoningItem.Get("encrypted_content").Exists() {
		t.Fatalf("null encrypted_content should still be stripped: %s", got)
	}
}

// The carriage tests below cover the v8.0.34 signature work: Meta Muse target
// gating, OpenRouter router-suffix stripping and multi-family Responses targets
// (upstream CLIProxyAPI 7a0b7563e9a2, 674dbe8fd1be, be9fbd942929,
// cc5682c563dc, a68c7d8ed7c2).

// validMetaReasoningEncryptedContentForTest builds a shape-valid synthetic Meta
// envelope: "Q-PaDgE" pins the observed five-byte header, and 1426 unpadded
// base64url chars decode to 45 + 1024 bytes. No captured signature is embedded.
func validMetaReasoningEncryptedContentForTest() string {
	return "Q-PaDgE" + strings.Repeat("A", 1419)
}

// routedMetaReasoningEncryptedContentForTest appends a synthetic
// OpenRouter-style suffix: one '.' plus 67 alphanumeric characters.
func routedMetaReasoningEncryptedContentForTest() string {
	return validMetaReasoningEncryptedContentForTest() + "." + strings.Repeat("Ab3", 23)[:67]
}

// validGeminiThoughtSignatureForTest builds a standard-base64 protobuf-shaped
// Gemini envelope (leading field tag 0x0a), which is the only marker the Gemini
// probe requires.
func validGeminiThoughtSignatureForTest() string {
	payload := make([]byte, 64)
	payload[0] = 0x0a
	for i := 1; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(payload)
}

// validGrokEncryptedContentForTest builds the high-entropy unpadded
// standard-base64 blob shape native xAI encrypted_content has.
func validGrokEncryptedContentForTest() string {
	payload := make([]byte, 135)
	for i := range payload {
		payload[i] = byte((i*73 + 19) & 0xff)
	}
	return base64.RawStdEncoding.EncodeToString(payload)
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModel_RoutedMetaTarget(t *testing.T) {
	native := validMetaReasoningEncryptedContentForTest()
	routed := routedMetaReasoningEncryptedContentForTest()
	gptSig := validOpenAIResponsesReasoningEncryptedContentForTest()
	const unknownSig = "opaque-encrypted-reasoning-token-xyz"
	buildBody := func() []byte {
		return []byte(`{"store":false,"input":[` +
			`{"type":"reasoning","summary":[],"encrypted_content":"` + native + `"},` +
			`{"type":"reasoning","summary":[],"encrypted_content":"` + routed + `"},` +
			`{"type":"reasoning","summary":[],"encrypted_content":"` + gptSig + `"},` +
			`{"type":"reasoning","summary":[],"encrypted_content":"` + unknownSig + `"}` +
			`]}`)
	}
	assertRoutedTarget := func(t *testing.T, got []byte) {
		t.Helper()
		if gotSig := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotSig != native {
			t.Fatalf("native Meta encrypted_content not preserved; body=%s", got)
		}
		if gotSig := gjson.GetBytes(got, "input.1.encrypted_content").String(); gotSig != routed {
			t.Fatalf("routed Meta encrypted_content = %q, want verbatim; body=%s", gotSig, got)
		}
		for index, item := range gjson.GetBytes(got, "input").Array() {
			if index < 2 {
				continue
			}
			if item.Get("encrypted_content").Exists() {
				t.Fatalf("input[%d] foreign encrypted_content should be stripped on a Meta target; body=%s", index, got)
			}
		}
	}

	// An OpenRouter Meta route preserves routed envelopes verbatim regardless of
	// the compat flag, while a non-OpenRouter third-party base URL strips
	// OpenRouter's private suffix down to the native envelope.
	for _, isCompat := range []bool{false, true} {
		got := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(context.Background(), "test", buildBody(), isCompat, "meta/muse-wonder", "https://openrouter.ai/api/v1")
		assertRoutedTarget(t, got)
	}

	// Namespaced router model IDs resolve to their Meta tail segment.
	got := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(context.Background(), "test", buildBody(), false, "openrouter/meta/muse-wonder", "https://openrouter.ai/api/v1")
	assertRoutedTarget(t, got)

	otherProviderMeta := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(context.Background(), "test", buildBody(), false, "meta/muse-wonder", "https://example-proxy.internal/v1")
	if gotSig := gjson.GetBytes(otherProviderMeta, "input.1.encrypted_content").String(); gotSig != native {
		t.Fatalf("non-OpenRouter provider should strip the OpenRouter suffix to the native Meta envelope, got %q want %q", gotSig, native)
	}

	// The Meta executor's own gate: an OpenRouter Meta base URL keeps routed
	// envelopes, the native api.meta.ai upstream refuses them.
	routedMetaURL := sanitizeOpenAIResponsesReasoningEncryptedContentForMetaBaseURL(context.Background(), "test", buildBody(), "https://openrouter.ai/api/v1")
	assertRoutedTarget(t, routedMetaURL)

	nativeMetaURL := sanitizeOpenAIResponsesReasoningEncryptedContentForMetaBaseURL(context.Background(), "test", buildBody(), "https://api.meta.ai/api/v1")
	if gotSig := gjson.GetBytes(nativeMetaURL, "input.0.encrypted_content").String(); gotSig != native {
		t.Fatalf("native Meta upstream should preserve the native envelope; body=%s", nativeMetaURL)
	}
	if gjson.GetBytes(nativeMetaURL, "input.1.encrypted_content").Exists() {
		t.Fatalf("native Meta upstream should strip the router-suffixed envelope; body=%s", nativeMetaURL)
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContentForMeta_StripsRouterSuffixed(t *testing.T) {
	native := validMetaReasoningEncryptedContentForTest()
	routed := routedMetaReasoningEncryptedContentForTest()
	body := []byte(`{"store":false,"input":[` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + native + `"},` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + routed + `"}` +
		`]}`)

	got := sanitizeOpenAIResponsesReasoningEncryptedContentForMeta(context.Background(), "test", body)

	if gotSig := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotSig != native {
		t.Fatalf("native Meta encrypted_content not preserved; body=%s", got)
	}
	if gjson.GetBytes(got, "input.1.encrypted_content").Exists() {
		t.Fatalf("router-suffixed Meta encrypted_content should be stripped on the native Meta target: %s", got)
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModel_StripsMetaForNonMetaTargets(t *testing.T) {
	native := validMetaReasoningEncryptedContentForTest()
	routed := routedMetaReasoningEncryptedContentForTest()
	body := []byte(`{"store":false,"input":[` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + native + `"},` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + routed + `"}` +
		`]}`)

	for _, tc := range []struct {
		model    string
		isCompat bool
	}{
		{"gpt-5.4", false},
		{"deepseek-v4", true},
	} {
		got := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModel(context.Background(), "test", body, tc.isCompat, tc.model)
		for index, item := range gjson.GetBytes(got, "input").Array() {
			if item.Get("encrypted_content").Exists() {
				t.Fatalf("model=%s isCompat=%v: input[%d] Meta encrypted_content should be stripped; body=%s", tc.model, tc.isCompat, index, got)
			}
		}
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContent_StripsRouterSuffixOnNonOpenRouterUpstreams(t *testing.T) {
	gptNative := validOpenAIResponsesReasoningEncryptedContentForTest()
	suffix := strings.Repeat("Ab3", 27)[:79]
	gptRouted := gptNative + "." + suffix

	body := []byte(`{"store":false,"input":[{"type":"reasoning","summary":[],"encrypted_content":"` + gptRouted + `"}]}`)

	gotOfficial := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(
		context.Background(), "test", body, false, "gpt-5.4", "https://chatgpt.com/backend-api/codex")
	if gotSig := gjson.GetBytes(gotOfficial, "input.0.encrypted_content").String(); gotSig != gptNative {
		t.Fatalf("official Codex encrypted_content = %q, want stripped native %q", gotSig, gptNative)
	}

	gotNoBaseURL := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModel(
		context.Background(), "test", body, false, "gpt-5.4")
	if gotSig := gjson.GetBytes(gotNoBaseURL, "input.0.encrypted_content").String(); gotSig != gptNative {
		t.Fatalf("unknown upstream encrypted_content = %q, want stripped native %q", gotSig, gptNative)
	}

	gotOpenRouter := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(
		context.Background(), "test", body, false, "openrouter/openai/o4-mini", "https://openrouter.ai/api/v1")
	if gotSig := gjson.GetBytes(gotOpenRouter, "input.0.encrypted_content").String(); gotSig != gptRouted {
		t.Fatalf("OpenRouter encrypted_content = %q, want verbatim routed %q", gotSig, gptRouted)
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContent_PreservesRouterSuffixOnOpenRouterUpstream(t *testing.T) {
	// A Meta-family model on OpenRouter is the one place a routed envelope must
	// survive untouched, including the cache prefix normalization that applies to
	// a plain GPT value.
	gptNative := validOpenAIResponsesReasoningEncryptedContentForTest()
	gptRouted := gptNative + "." + strings.Repeat("Ab3", 20)[:59]
	body := []byte(`{"store":false,"input":[{"type":"reasoning","summary":[],"encrypted_content":"gpt#` + gptRouted + `"}]}`)

	got := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(
		context.Background(), "test", body, false, "openrouter/openai/gpt-5.6-luna-pro", "https://openrouter.ai/api/v1")
	if gotSig := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotSig != gptRouted {
		t.Fatalf("OpenRouter encrypted_content = %q, want the routed value without the cache prefix %q", gotSig, gptRouted)
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContent_MultiFamilyResponsesTarget(t *testing.T) {
	geminiSig := validGeminiThoughtSignatureForTest()
	grokSig := validGrokEncryptedContentForTest()
	gptSig := validOpenAIResponsesReasoningEncryptedContentForTest()
	body := []byte(`{"store":false,"input":[` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + geminiSig + `"},` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + gptSig + `"},` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + grokSig + `"}` +
		`]}`)

	// A Gemini target served through a Responses router keeps only its own family.
	gotGemini := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(
		context.Background(), "test", body, false, "openrouter/google/gemini-3-pro", "https://openrouter.ai/api/v1")
	if gotSig := gjson.GetBytes(gotGemini, "input.0.encrypted_content").String(); gotSig != geminiSig {
		t.Fatalf("gemini target encrypted_content = %q, want the Gemini envelope preserved; body=%s", gotSig, gotGemini)
	}
	for _, index := range []int{1, 2} {
		if gjson.GetBytes(gotGemini, "input."+fmt.Sprint(index)+".encrypted_content").Exists() {
			t.Fatalf("gemini target should strip a foreign signature at input[%d]; body=%s", index, gotGemini)
		}
	}

	// A Grok target keeps only a Grok-shaped blob; xAI rejects foreign values
	// with a hard 400, so the foreign ones must leave with the block.
	gotGrok := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(
		context.Background(), "test", body, false, "x-ai/grok-4.5", "https://openrouter.ai/api/v1")
	if gotSig := gjson.GetBytes(gotGrok, "input.2.encrypted_content").String(); gotSig != grokSig {
		t.Fatalf("grok target encrypted_content = %q, want the Grok blob preserved; body=%s", gotSig, gotGrok)
	}
	for _, index := range []int{0, 1} {
		if gjson.GetBytes(gotGrok, "input."+fmt.Sprint(index)+".encrypted_content").Exists() {
			t.Fatalf("grok target should strip a foreign signature at input[%d]; body=%s", index, gotGrok)
		}
	}
}

// TestSanitizeOpenAIResponsesReasoningEncryptedContent_NoopBytesUnchanged pins
// the non-Muse invariant: a body whose signatures carry no router suffix and
// belong to the target's own family is returned byte-identical, on the same
// slice, so nothing about the existing providers' replay changes.
func TestSanitizeOpenAIResponsesReasoningEncryptedContent_NoopBytesUnchanged(t *testing.T) {
	gptSig := validOpenAIResponsesReasoningEncryptedContentForTest()
	geminiSig := validGeminiThoughtSignatureForTest()
	buildBody := func(signature string) []byte {
		return []byte(`{"store":false,"input":[` +
			`{"type":"reasoning","summary":[],"encrypted_content":"` + signature + `"},` +
			`{"id":"rs_keep","type":"reasoning","summary":[{"type":"summary_text","text":"kept"}],"encrypted_content":"` + signature + `"}` +
			`]}`)
	}

	for _, tc := range []struct {
		name      string
		target    string
		baseURL   string
		signature string
	}{
		{"gpt target", "gpt-5.4", "https://chatgpt.com/backend-api/codex", gptSig},
		{"gemini target", "gemini-3-pro", "https://generativelanguage.googleapis.com", geminiSig},
		{"openrouter gpt target", "openrouter/openai/gpt-5.6-luna-pro", "https://openrouter.ai/api/v1", gptSig},
	} {
		body := buildBody(tc.signature)
		got := sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(
			context.Background(), "test", body, false, tc.target, tc.baseURL)
		if !bytes.Equal(got, body) {
			t.Fatalf("%s: sanitizer rewrote a body whose signatures carry no router suffix: got=%s", tc.name, got)
		}
		if &got[0] != &body[0] {
			t.Fatalf("%s: sanitizer rebuilt the input array instead of returning the original slice", tc.name)
		}
	}
}
