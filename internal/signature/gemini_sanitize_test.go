package signature

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func testGeminiThoughtSignature(payload []byte) string {
	return base64.StdEncoding.EncodeToString(payload)
}

func testGemini3ThoughtSignature(payload []byte) string {
	var inner []byte
	inner = protowire.AppendTag(inner, 1, protowire.BytesType)
	inner = protowire.AppendBytes(inner, payload)

	var outer []byte
	outer = protowire.AppendTag(outer, 2, protowire.BytesType)
	outer = protowire.AppendBytes(outer, inner)
	return testGeminiThoughtSignature(outer)
}

func newSignatureDebugHook(t *testing.T) *test.Hook {
	t.Helper()

	previousLevel := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(previousLevel)
	})
	return hook
}

func assertSignatureDebugDoesNotLeak(t *testing.T, hook *test.Hook, forbidden string) {
	t.Helper()

	if forbidden == "" {
		return
	}
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, forbidden) {
			t.Fatalf("debug log leaked signature in message: %q", entry.Message)
		}
		for key, value := range entry.Data {
			if strings.Contains(fmt.Sprint(value), forbidden) {
				t.Fatalf("debug log leaked signature in field %q: %v", key, value)
			}
		}
	}
}

var benchmarkSanitizeGeminiRequestOutput []byte

func TestSanitizeGeminiRequestThoughtSignaturesPreservesGeminiSignature(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sig + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want %q. Output: %s", got, sig, string(out))
	}
	if &out[0] != &input[0] {
		t.Fatal("compatible canonical signature payload was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesNormalizesDuplicateCanonicalField(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `","thoughtSignature":"bad","thoughtSignature":"worse"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, out)
	}
	if count := strings.Count(string(out), `"thoughtSignature"`); count != 1 {
		t.Fatalf("thoughtSignature field count = %d, want 1. Output: %s", count, out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesParallelSyntheticOnlyFirstGetsBypass(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}}},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("second parallel call should remain unsigned; output=%s", out)
	}
}

// assertGeminiFirstTurnPartSignatures pins the thoughtSignature carried by each
// part of the first content turn. An empty want means the part must stay
// unsigned (no thoughtSignature field at all).
func assertGeminiFirstTurnPartSignatures(t *testing.T, out []byte, want ...string) {
	t.Helper()

	parts := gjson.GetBytes(out, "contents.0.parts")
	if got := int(parts.Get("#").Int()); got != len(want) {
		t.Fatalf("part count = %d, want %d; output=%s", got, len(want), out)
	}
	for index, expected := range want {
		signature := gjson.GetBytes(out, fmt.Sprintf("contents.0.parts.%d.thoughtSignature", index))
		if expected == "" {
			if signature.Exists() {
				t.Fatalf("part %d signature = %q, want unsigned; output=%s", index, signature.String(), out)
			}
			continue
		}
		if !signature.Exists() || signature.String() != expected {
			t.Fatalf("part %d signature = %q (exists=%t), want %q; output=%s", index, signature.String(), signature.Exists(), expected, out)
		}
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesCallAfterTextGetsBypass(t *testing.T) {
	t.Run("text_then_parallel_calls", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"text":"thinking out loud"},` +
			`{"functionCall":{"name":"first","args":{}}},` +
			`{"functionCall":{"name":"second","args":{}}}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		assertGeminiFirstTurnPartSignatures(t, out, "", GeminiSkipThoughtSignatureValidator, "")
	})

	t.Run("text_between_calls_keeps_trailing_sibling_unsigned", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"functionCall":{"name":"first","args":{}}},` +
			`{"text":"more"},` +
			`{"functionCall":{"name":"second","args":{}}},` +
			`{"functionCall":{"name":"third","args":{}}}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		assertGeminiFirstTurnPartSignatures(t, out, GeminiSkipThoughtSignatureValidator, "", GeminiSkipThoughtSignatureValidator, "")
	})

	t.Run("unsigned_calls_separated_by_text", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}}},{"functionCall":{"name":"second","args":{}}},{"text":"more"},{"functionCall":{"name":"third","args":{}}}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
		}
		if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
			t.Fatalf("second parallel call should remain unsigned; output=%s", out)
		}
		if got := gjson.GetBytes(out, "contents.0.parts.3.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("call after text signature = %q, want bypass sentinel; output=%s", got, out)
		}
	})

	t.Run("bypass_signed_calls_separated_by_text", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"},` +
			`{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"},` +
			`{"text":"more"},` +
			`{"functionCall":{"name":"third","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
		}
		if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
			t.Fatalf("second parallel call should have bypass stripped; output=%s", out)
		}
		if got := gjson.GetBytes(out, "contents.0.parts.3.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("call after text signature = %q, want bypass sentinel preserved; output=%s", got, out)
		}
	})

	t.Run("thought_part_does_not_end_parallel_group", func(t *testing.T) {
		nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},` +
			`{"text":"hidden","thought":true},` +
			`{"functionCall":{"name":"second","args":{}}}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		assertGeminiFirstTurnPartSignatures(t, out, nativeSignature, "", "")
	})
}

func TestSanitizeGeminiRequestThoughtSignaturesNonTextPartStartsNewGroup(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	// Every shape is a model turn whose first call carries a native signature and
	// whose trailing sibling is unsigned: only a part between them that is not a
	// thought part can put the second call at the head of a new group, where it
	// gains the bypass sentinel. The third call then must stay unsigned.
	shapes := map[string]struct {
		carrier string
		want    []string
	}{
		"bare_signature_carrier": {
			carrier: `{"thoughtSignature":"` + nativeSignature + `"}`,
			want:    []string{nativeSignature, nativeSignature, GeminiSkipThoughtSignatureValidator, ""},
		},
		"inline_data_part": {
			carrier: `{"inlineData":{"mimeType":"image/png","data":"AAAA"}}`,
			want:    []string{nativeSignature, "", GeminiSkipThoughtSignatureValidator, ""},
		},
		"executable_code_part": {
			carrier: `{"executableCode":{"language":"PYTHON","code":"1+1"}}`,
			want:    []string{nativeSignature, "", GeminiSkipThoughtSignatureValidator, ""},
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			input := []byte(`{"contents":[{"role":"model","parts":[` +
				`{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},` +
				shape.carrier + `,` +
				`{"functionCall":{"name":"second","args":{}}},` +
				`{"functionCall":{"name":"third","args":{}}}]}]}`)

			out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

			assertGeminiFirstTurnPartSignatures(t, out, shape.want...)
		})
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesNoTextPartHistoryIsByteIdentical(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	inputs := map[string][]byte{
		"single_signed_call":    []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"}]}]}`),
		"parallel_never_signed": []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}}}]}]}`),
		"thought_between_calls": []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"text":"hidden","thought":true},{"functionCall":{"name":"second","args":{}}}]}]}`),
		"other_turn_shapes":     []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"text":"hidden","thought":true},{"functionCall":{"name":"second","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"first","response":{}}}]}]}`),
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			out := SanitizeGeminiRequestThoughtSignatures(input, "contents")
			if !bytes.Equal(out, input) || &out[0] != &input[0] {
				t.Fatalf("history without an interleaved text part was not left byte-identical; input=%s output=%s", input, out)
			}
		})
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesNativeParallelPreservesUnsignedSibling(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("native unsigned sibling should remain unsigned; output=%s", out)
	}
	if &out[0] != &input[0] {
		t.Fatal("already-native parallel history was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesPollutedSiblingBypass(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("polluted sibling bypass should be removed; output=%s", out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesPrefixedSiblingBypass(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	for _, prefix := range []string{"gemini", "google"} {
		t.Run(prefix, func(t *testing.T) {
			input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + prefix + `#` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

			out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

			if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
				t.Fatalf("prefixed sibling bypass should be removed; output=%s", out)
			}
		})
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesLeavesUnsignedThoughtUnsigned(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"hidden","thought":true}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if signature := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature"); signature.Exists() {
		t.Fatalf("unsigned thought should remain unsigned; output=%s", out)
	}
	if &out[0] != &input[0] {
		t.Fatal("unsigned thought payload was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesReusesUnsignedFunctionResponsePayload(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if &out[0] != &input[0] {
		t.Fatal("unsigned function response payload was copied")
	}
	if string(out) != string(input) {
		t.Fatalf("payload changed:\n got: %s\nwant: %s", out, input)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesReplacesBase64UUIDFunctionCall(t *testing.T) {
	sig := testGeminiThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"thoughtSignature":"` + sig + `"}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "contents.0.parts.0.functionCall.thoughtSignature").Exists() {
		t.Fatalf("nested functionCall thoughtSignature should be removed. Output: %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesLogsBypassReplacement(t *testing.T) {
	hook := newSignatureDebugHook(t)
	sig := testGeminiThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"thoughtSignature":"` + sig + `"}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")
	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, string(out))
	}

	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.DebugLevel {
			continue
		}
		if entry.Data["component"] != "signature_sanitizer" ||
			entry.Data["target_provider"] != string(SignatureProviderGemini) ||
			entry.Data["action"] != "replace_with_gemini_bypass" {
			continue
		}
		if entry.Data["block_kind"] != string(SignatureBlockKindGeminiFunctionCall) {
			t.Fatalf("block_kind = %v, want %s", entry.Data["block_kind"], SignatureBlockKindGeminiFunctionCall)
		}
		found = true
	}
	if !found {
		t.Fatal("expected debug log for Gemini thoughtSignature bypass replacement")
	}
	assertSignatureDebugDoesNotLeak(t, hook, sig)
}

func TestSanitizeGeminiRequestThoughtSignaturesPreservesField2WrappedUUIDFunctionCall(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sig + `"}]}]}}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "request.contents")

	if got := gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want wrapped UUID signature preserved. Output: %s", got, string(out))
	}
}

func BenchmarkSanitizeGeminiRequestThoughtSignaturesNormalizedHistory(b *testing.B) {
	for _, turns := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("turns_%d", turns), func(b *testing.B) {
			input := normalizedGeminiSignatureHistory(turns, 8<<20)
			output := SanitizeGeminiRequestThoughtSignatures(input, "contents")
			if &output[0] != &input[0] {
				b.Fatal("normalized payload was copied")
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()

			for b.Loop() {
				benchmarkSanitizeGeminiRequestOutput = SanitizeGeminiRequestThoughtSignatures(input, "contents")
			}
		})
	}
}

func normalizedGeminiSignatureHistory(turns, totalPayloadBytes int) []byte {
	payload := strings.Repeat("x", totalPayloadBytes/turns)
	var builder strings.Builder
	builder.Grow(totalPayloadBytes + turns*256)
	builder.WriteString(`{"contents":[`)
	for i := 0; i < turns; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"value":"`)
		builder.WriteString(payload)
		builder.WriteString(`"}},"thoughtSignature":"`)
		builder.WriteString(GeminiSkipThoughtSignatureValidator)
		builder.WriteString(`"}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}`)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesFunctionResponseSignature(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"},"thoughtSignature":"bad","thoughtSignature":"worse"},"thoughtSignature":"bad"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("functionResponse top-level thoughtSignature should be removed. Output: %s", string(out))
	}
	if gjson.GetBytes(out, "contents.0.parts.0.functionResponse.thoughtSignature").Exists() {
		t.Fatalf("functionResponse nested thoughtSignature should be removed. Output: %s", string(out))
	}
}
