package openai

import (
	"net/http"
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/interfaces"
)

const responsesModerationMetadataFrame = "event: response.metadata\ndata: {\"type\":\"response.metadata\",\"metadata\":{\"openai_chatgpt_moderation_metadata\":{\"prompt\":{}}}}\n\n"

func responsesMetadataTestFrames() [][]byte {
	return [][]byte{
		[]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n"),
		[]byte(responsesModerationMetadataFrame),
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\"}}\n\n"),
	}
}

// TestResponsesSSEFramerFiltersResponseMetadataForStandardClient pins that the
// Codex backend's moderation metadata never reaches a standard Responses client.
func TestResponsesSSEFramerFiltersResponseMetadataForStandardClient(t *testing.T) {
	var output strings.Builder
	framer := &responsesSSEFramer{}
	for _, frame := range responsesMetadataTestFrames() {
		framer.WriteChunk(&output, frame)
	}

	got := output.String()
	if strings.Contains(got, "response.metadata") || strings.Contains(got, "openai_chatgpt_moderation_metadata") {
		t.Fatalf("expected response.metadata to be stripped, got: %q", got)
	}
	if !strings.Contains(got, "event: response.created") || !strings.Contains(got, "event: response.completed") {
		t.Fatalf("expected surrounding events to survive, got: %q", got)
	}
}

// TestResponsesSSEFramerPreservesResponseMetadataForCodexClient pins that
// official Codex clients keep receiving the moderation metadata event.
func TestResponsesSSEFramerPreservesResponseMetadataForCodexClient(t *testing.T) {
	var output strings.Builder
	framer := &responsesSSEFramer{isCodexClient: true}
	for _, frame := range responsesMetadataTestFrames() {
		framer.WriteChunk(&output, frame)
	}

	got := output.String()
	if !strings.Contains(got, "event: response.metadata") || !strings.Contains(got, "openai_chatgpt_moderation_metadata") {
		t.Fatalf("expected response.metadata to be preserved for official Codex client, got: %q", got)
	}
	if !strings.Contains(got, "event: response.completed") {
		t.Fatalf("expected response.completed in output, got: %q", got)
	}
}

// TestForwardResponsesStreamMetadataFilterFollowsClientIdentity pins that both
// construction sites derive isCodexClient from the downstream request.
func TestForwardResponsesStreamMetadataFilterFollowsClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		headers   http.Header
		wantShown bool
	}{
		{name: "standard client", headers: http.Header{"User-Agent": {"standard-client/1.0"}}},
		{name: "codex user agent", headers: http.Header{"User-Agent": {"codex_cli_rs/1.0"}}, wantShown: true},
		{name: "codex originator", headers: http.Header{"Originator": {"codex_cli_rs"}}, wantShown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, recorder, c, flusher := newResponsesStreamTestHandler(t)
			for key, values := range tc.headers {
				for _, value := range values {
					c.Request.Header.Add(key, value)
				}
			}

			data := make(chan []byte, 3)
			errs := make(chan *interfaces.ErrorMessage)
			for _, frame := range responsesMetadataTestFrames() {
				data <- frame
			}
			close(data)
			close(errs)

			h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)

			got := recorder.Body.String()
			shown := strings.Contains(got, "response.metadata") && strings.Contains(got, "openai_chatgpt_moderation_metadata")
			if shown != tc.wantShown {
				t.Fatalf("response.metadata shown = %v, want %v: %q", shown, tc.wantShown, got)
			}
			if !strings.Contains(got, "event: response.completed") {
				t.Fatalf("expected response.completed in output, got: %q", got)
			}
		})
	}
}
