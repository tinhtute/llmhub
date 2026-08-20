package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToAntigravityTranslatesVideoURL(t *testing.T) {
	input := []byte(`{
		"model": "gemini-3.7-flash-high",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "Name the colours in order"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}},
				{"type": "video_url", "video_url": {"url": "data:video/mp4;base64,AAAAIGZ0eXBtcDQy"}}
			]
		}]
	}`)

	output := ConvertOpenAIRequestToAntigravity("gemini-3.7-flash-high", input, false)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 3 {
		t.Fatalf("parts length = %d, want 3. Output: %s", len(parts), output)
	}

	if got := parts[0].Get("text").String(); got != "Name the colours in order" {
		t.Fatalf("parts[0].text = %q, want text content", got)
	}
	if got := parts[1].Get("inlineData.mimeType").String(); got != "image/png" {
		t.Fatalf("parts[1].inlineData.mimeType = %q, want image/png", got)
	}
	if got := parts[2].Get("inlineData.mimeType").String(); got != "video/mp4" {
		t.Fatalf("parts[2].inlineData.mimeType = %q, want video/mp4", got)
	}
	if got := parts[2].Get("inlineData.data").String(); got != "AAAAIGZ0eXBtcDQy" {
		t.Fatalf("parts[2].inlineData.data = %q, want video payload", got)
	}
	if parts[2].Get("thoughtSignature").Exists() {
		t.Fatal("video part should not receive an image thought signature")
	}
}
