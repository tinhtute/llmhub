package executor

import (
	"strings"
	"testing"

	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// A relay chunk boundary can fall anywhere, including inside an SSE field name
// or inside a large thought signature. Buffering the frames must reach the
// client exactly as if the relay had sent whole frames.
func TestAIStudioStreamReassemblesFragmentedFrames(t *testing.T) {
	terminal := strings.Replace(multiFrameB, `"text":"BBB"`, `"text":"BBB","thoughtSignature":"`+strings.Repeat("signature", 512)+`"`, 1)
	frame := "data: " + terminal + "\n\n"
	crlf := "data: " + terminal + "\r\n\r\n"
	for _, tc := range []struct {
		format  sdktranslator.Format
		markers []string
	}{
		{
			format:  sdktranslator.FormatOpenAI,
			markers: []string{"AAA", "BBB", `"finish_reason":"stop"`},
		},
		{
			format:  sdktranslator.FormatOpenAIResponse,
			markers: []string{"AAA", "BBB", "event:response.completed"},
		},
		{
			format:  sdktranslator.FormatClaude,
			markers: []string{"AAA", "BBB", `"stop_reason":"end_turn"`},
		},
	} {
		t.Run(tc.format.String(), func(t *testing.T) {
			for _, variant := range []struct {
				name      string
				fragments []string
			}{
				{name: "large signature", fragments: []string{frame[:1181], frame[1181:2362], frame[2362:3543], frame[3543:]}},
				{name: "split data prefix", fragments: []string{frame[:2], frame[2:4], frame[4:]}},
				{name: "split crlf", fragments: []string{crlf[:len(crlf)-3], crlf[len(crlf)-3 : len(crlf)-1], crlf[len(crlf)-1:]}},
				{name: "unterminated tail", fragments: []string{frame[:1181], strings.TrimSuffix(frame[1181:], "\n\n")}},
			} {
				t.Run(variant.name, func(t *testing.T) {
					chunks := append([]string{"data: " + multiFrameA + "\n\n"}, variant.fragments...)
					got, _, _ := streamThroughRelay(t, tc.format, chunks)
					assertFramesReachedClient(t, variant.name, got, spaceFree(got), tc.markers)
				})
			}
		})
	}
}
