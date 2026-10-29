package gemini

import (
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/tidwall/gjson"
)

func levelModelInfo() *registry.ModelInfo {
	return &registry.ModelInfo{
		ID:       "gemini-3-pro",
		Thinking: &registry.ThinkingSupport{Min: 128, Max: 32768, Levels: []string{"low", "high"}},
	}
}

// TestApply_DeleteAliasByteCompatibility pins the exact output of the level and
// budget forms for a body that carries a conflicting alias spelling and one
// that does not; the absent alias is the case the existence check skips.
func TestApply_DeleteAliasByteCompatibility(t *testing.T) {
	tests := []struct {
		name   string
		config thinking.ThinkingConfig
		body   string
		want   string
	}{
		{
			name:   "level format absent aliases",
			config: thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelHigh},
			body:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`,
			want:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}}`,
		},
		{
			name:   "level format present aliases",
			config: thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelHigh},
			body:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","thinkingBudget":4096,"thinking_level":"high","include_thoughts":false}}}`,
			want:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":false}}}`,
		},
		{
			name:   "budget format absent aliases",
			config: thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: 8192},
			body:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`,
			want:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":8192,"includeThoughts":true}}}`,
		},
		{
			name:   "budget format present aliases",
			config: thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: 8192},
			body:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","thinkingBudget":4096,"thinking_level":"high","include_thoughts":false}}}`,
			want:   `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":8192,"includeThoughts":false}}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewApplier().Apply([]byte(test.body), test.config, levelModelInfo())
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("body = %s, want %s", got, test.want)
			}
		})
	}
}

// BenchmarkApply_LevelFormatLargeBody measures the level form on a 1 MiB
// request that carries none of the conflicting alias spellings.
func BenchmarkApply_LevelFormatLargeBody(b *testing.B) {
	body := []byte(`{"contents":[{"parts":[{"text":"` + strings.Repeat("x", 1<<20) +
		`"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`)
	if !gjson.ValidBytes(body) {
		b.Fatal("fixture is not valid JSON")
	}
	config := thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelHigh}
	modelInfo := levelModelInfo()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		_, _ = NewApplier().Apply(body, config, modelInfo)
	}
}
