package thinking

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// largeThinkingBody builds a request body whose 1 MiB of payload sits ahead of
// generationConfig, so a full-document copy made by a delete shows up in the
// allocation count instead of being masked by a small fixture. wrapper is the
// top-level Antigravity "request" object; "" builds the Gemini shape.
func largeThinkingBody(wrapper string, payloadBytes int) []byte {
	inner := `{"contents":[{"parts":[{"text":"` + strings.Repeat("x", payloadBytes) +
		`"}]}],"generationConfig":{"thinkingConfig":{}}}`
	if wrapper == "" {
		return []byte(inner)
	}
	return []byte(`{"` + wrapper + `":` + inner + `}`)
}

// summaryAliasCases are the two providers whose summary pass deletes alias
// spellings that are almost always absent from the request.
var summaryAliasCases = []struct {
	name      string
	format    string
	wrapper   string
	canonical string
}{
	{name: "gemini", format: "gemini", canonical: "generationConfig.thinkingConfig.includeThoughts"},
	{name: "antigravity", format: "antigravity", wrapper: "request", canonical: "request.generationConfig.thinkingConfig.includeThoughts"},
}

// TestApplySummaryConfig_DeleteAliasByteCompatibility pins the exact output for
// a body that carries an alias spelling and one that does not. The absent body
// is the case the existence check short-circuits, and it must stay byte for
// byte what sjson.DeleteBytes produced.
func TestApplySummaryConfig_DeleteAliasByteCompatibility(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
		want   string
	}{
		{
			name:   "gemini absent alias",
			format: "gemini",
			body:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{}}}`,
			want:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`,
		},
		{
			name:   "gemini present alias",
			format: "gemini",
			body:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"include_thoughts":true}}}`,
			want:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`,
		},
		{
			name:   "antigravity absent alias",
			format: "antigravity",
			body:   `{"request":{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{}}}}`,
			want:   `{"request":{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}}`,
		},
		{
			name:   "antigravity present alias",
			format: "antigravity",
			body:   `{"request":{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"include_thoughts":true}}}}`,
			want:   `{"request":{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := applySummaryConfigForProvider([]byte(test.body), test.format, "model", "", nil, SummaryConfig{Mode: SummaryEnabled})
			if string(got) != test.want {
				t.Fatalf("body = %s, want %s", got, test.want)
			}
		})
	}
}

// TestApplySummaryConfig_AbsentAliasDeleteDoesNotCopy holds the cost contract
// of the summary pass: an absent alias delete must not copy the document.
// sjson.DeleteBytes builds a discarded prefix copy and then copies the whole
// input again before returning it, so before the existence check a 1 MiB body
// cost several megabytes per request.
func TestApplySummaryConfig_AbsentAliasDeleteDoesNotCopy(t *testing.T) {
	// One 1 MiB write for the canonical includeThoughts field plus slack.
	const limitBytesPerOp = 2 << 20
	for _, test := range summaryAliasCases {
		t.Run(test.name, func(t *testing.T) {
			body := largeThinkingBody(test.wrapper, 1<<20)
			if !gjson.ValidBytes(body) {
				t.Fatal("fixture is not valid JSON: the applier would return early and allocate nothing")
			}
			out := applySummaryConfigForProvider(body, test.format, "model", "", nil, SummaryConfig{Mode: SummaryEnabled})
			if !gjson.GetBytes(out, test.canonical).Bool() {
				t.Fatalf("%s missing from output: %s", test.canonical, out)
			}

			allocated := testing.Benchmark(func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = applySummaryConfigForProvider(body, test.format, "model", "", nil, SummaryConfig{Mode: SummaryEnabled})
				}
			}).AllocedBytesPerOp()
			if allocated > limitBytesPerOp {
				t.Fatalf("absent alias delete allocated %d B/op on a 1 MiB body, want <= %d: the whole document is still copied",
					allocated, limitBytesPerOp)
			}
		})
	}
}

// BenchmarkApplySummaryConfig_LargeBody measures the summary pass on a 1 MiB
// request whose alias spellings are all absent, the shape every request has.
func BenchmarkApplySummaryConfig_LargeBody(b *testing.B) {
	for _, test := range summaryAliasCases {
		body := largeThinkingBody(test.wrapper, 1<<20)
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				_ = applySummaryConfigForProvider(body, test.format, "model", "", nil, SummaryConfig{Mode: SummaryEnabled})
			}
		})
	}
}
