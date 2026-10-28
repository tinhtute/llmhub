package chat_completions

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// A gemini-cli terminal chunk and the same payload as a non-stream body must
// report the same finish_reason/native_finish_reason pair. The non-stream path
// delegates to the Gemini OpenAI-chat translator, so the stream has to share its
// mapping: a filter reason becomes content_filter, an unmatched reason keeps its
// raw spelling, and STOP/MAX_TOKENS stay the OpenAI enum.
func TestCliStreamFinishReasonMatchesNonStream(t *testing.T) {
	for _, tc := range []struct{ upstream, wantFinish, wantNative string }{
		{"STOP", "stop", "stop"},
		{"MAX_TOKENS", "max_tokens", "max_tokens"},
		{"SAFETY", "content_filter", "safety"},
		{"OTHER", "other", "other"},
	} {
		t.Run(tc.upstream, func(t *testing.T) {
			raw := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"` + tc.upstream + `"}]}}`)

			var param any
			streamed := ConvertCliResponseToOpenAI(context.Background(), "gemini-cli", nil, nil, raw, &param)
			if len(streamed) != 1 {
				t.Fatalf("stream chunks = %d, want 1", len(streamed))
			}
			streamFinish := gjson.GetBytes(streamed[0], "choices.0.finish_reason")
			streamNative := gjson.GetBytes(streamed[0], "choices.0.native_finish_reason")
			if !streamFinish.Exists() || streamFinish.Type == gjson.Null {
				t.Fatalf("stream finish_reason is null; payload=%s", streamed[0])
			}
			if got := streamFinish.String(); got != tc.wantFinish {
				t.Fatalf("stream finish_reason = %q, want %q; payload=%s", got, tc.wantFinish, streamed[0])
			}
			if got := streamNative.String(); got != tc.wantNative {
				t.Fatalf("stream native_finish_reason = %q, want %q; payload=%s", got, tc.wantNative, streamed[0])
			}

			nonStream := ConvertCliResponseToOpenAINonStream(context.Background(), "gemini-cli", nil, nil, raw, nil)
			if got := gjson.GetBytes(nonStream, "choices.0.finish_reason").String(); got != tc.wantFinish {
				t.Fatalf("non-stream finish_reason = %q, want %q; payload=%s", got, tc.wantFinish, nonStream)
			}
			if got := gjson.GetBytes(nonStream, "choices.0.native_finish_reason").String(); got != tc.wantNative {
				t.Fatalf("non-stream native_finish_reason = %q, want %q; payload=%s", got, tc.wantNative, nonStream)
			}
			if streamFinish.String() != gjson.GetBytes(nonStream, "choices.0.finish_reason").String() {
				t.Fatalf("stream and non-stream disagree: stream=%s non-stream=%s", streamed[0], nonStream)
			}
		})
	}
}

// A chunk without a finishReason must keep finish_reason null rather than
// reporting a stop the upstream never sent.
func TestCliStreamNoFinishReasonStaysNull(t *testing.T) {
	var param any
	raw := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}}`)
	out := ConvertCliResponseToOpenAI(context.Background(), "gemini-cli", nil, nil, raw, &param)
	if len(out) != 1 {
		t.Fatalf("stream chunks = %d, want 1", len(out))
	}
	finish := gjson.GetBytes(out[0], "choices.0.finish_reason")
	if finish.Exists() && finish.Type != gjson.Null && strings.TrimSpace(finish.String()) != "" {
		t.Fatalf("finish_reason = %q, want null; payload=%s", finish.String(), out[0])
	}
}
