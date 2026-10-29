package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

type geminiTerminalDisconnectUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *geminiTerminalDisconnectUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil || record.Provider != "gemini" || record.AuthID != c.authID {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type geminiTerminalDisconnectUsageNoop struct{}

func (geminiTerminalDisconnectUsageNoop) HandleUsage(context.Context, usage.Record) {}

type geminiTerminalOutcome struct {
	record      usage.Record
	gotRecord   bool
	terminalOut bool
}

// runGeminiTerminalDisconnect drives one Gemini stream: the upstream writes a
// single frame (with or without usage), optionally keeps the body open, and the
// request context is cancelled as soon as the downstream sees cancelAfter.
func runGeminiTerminalDisconnect(t *testing.T, model, finishReason string, withUsage, hold bool, cancelAfter string) geminiTerminalOutcome {
	t.Helper()
	const usageJSON = `"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":4,"totalTokenCount":15}`
	finishJSON := ""
	if finishReason != "" {
		finishJSON = fmt.Sprintf(`,"finishReason":%q`, finishReason)
	}
	usagePart := ""
	if withUsage {
		usagePart = "," + usageJSON
	}
	sse := fmt.Sprintf("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"OK\"}]}%s}]%s}\n\n", finishJSON, usagePart)

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, errWrite := io.WriteString(w, sse); errWrite != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if !hold {
			return
		}
		// Keep EOF behind the downstream event and the cancellation.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	capture := &geminiTerminalDisconnectUsageCapture{authID: t.Name(), records: make(chan usage.Record, 4)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), geminiTerminalDisconnectUsageNoop{})
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, errExecute := NewGeminiExecutor(&config.Config{}).ExecuteStream(ctx, &cliproxyauth.Auth{
		ID: t.Name(),
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(fmt.Sprintf(`{"model":%q,"input":"Reply only OK.","stream":true,"max_output_tokens":32}`, model)),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Stream:         true,
	})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	outcome := geminiTerminalOutcome{}
streamLoop:
	for {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				break streamLoop
			}
			if strings.Contains(string(chunk.Payload), "response.completed") || strings.Contains(string(chunk.Payload), "response.incomplete") {
				outcome.terminalOut = true
			}
			if cancelAfter != "" && strings.Contains(string(chunk.Payload), cancelAfter) {
				cancel()
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for stream closure")
		}
	}
	select {
	case record := <-capture.records:
		outcome.record = record
		outcome.gotRecord = true
	case <-time.After(2 * time.Second):
	}
	return outcome
}

// TestGeminiStream_TerminalCancelKeepsUsage pins that a client that cancels the
// request once the terminal frame has been delivered still gets a successful
// usage record carrying the tokens Gemini reported.
func TestGeminiStream_TerminalCancelKeepsUsage(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6", "gemini-3.8-flash-high"} {
		for _, tc := range []struct {
			name         string
			finishReason string
			hold         bool
			withUsage    bool
			cancelAfter  string
			wantUsage    bool
		}{
			{name: "completed_cancel", finishReason: "STOP", hold: true, withUsage: true, cancelAfter: "response.completed", wantUsage: true},
			// A truncated upstream ends downstream in response.incomplete (R5
			// maps MAX_TOKENS to an incomplete response), so the cancel trigger
			// is that terminal event.
			{name: "truncated_upstream_cancel", finishReason: "MAX_TOKENS", hold: true, withUsage: true, cancelAfter: "response.incomplete", wantUsage: true},
			// No usage was reported before the cancel: the cancel must not turn
			// the record into a failure, and it must not drop the record either.
			{name: "terminal_cancel_without_usage", finishReason: "STOP", hold: true, cancelAfter: "response.completed"},
			{name: "completed_eof", finishReason: "STOP", withUsage: true, wantUsage: true},
		} {
			t.Run(model+"/"+tc.name, func(t *testing.T) {
				outcome := runGeminiTerminalDisconnect(t, model, tc.finishReason, tc.withUsage, tc.hold, tc.cancelAfter)
				if !outcome.terminalOut {
					t.Fatalf("expected a terminal frame on the downstream stream")
				}
				if !outcome.gotRecord {
					t.Fatal("expected a usage record after a terminal-then-cancel stream")
				}
				if outcome.record.Failed {
					t.Fatalf("usage record marked failed after a delivered terminal frame: status=%d body=%q", outcome.record.Fail.StatusCode, outcome.record.Fail.Body)
				}
				got := outcome.record.Detail
				if tc.wantUsage {
					if got.InputTokens != 11 || got.OutputTokens != 4 || got.TotalTokens != 15 {
						t.Fatalf("reported usage = %+v, want input 11, output 4, total 15", got)
					}
				} else if got.InputTokens != 0 || got.OutputTokens != 0 || got.TotalTokens != 0 {
					t.Fatalf("reported usage = %+v, want no tokens for a stream that reported none", got)
				}
			})
		}
	}
}

// TestGeminiStream_CancelBeforeTerminalIsNotASuccess is the paired control: the
// terminal-frame rule must not turn every cancellation into a completed request.
func TestGeminiStream_CancelBeforeTerminalIsNotASuccess(t *testing.T) {
	outcome := runGeminiTerminalDisconnect(t, "gemini-3.8-flash-high", "", false, true, "response.output_text.delta")
	if outcome.terminalOut {
		t.Fatalf("a stream cancelled before completion must not deliver a terminal frame")
	}
	if outcome.gotRecord && !outcome.record.Failed {
		t.Fatalf("record reported success for a stream cancelled before the terminal frame: %+v", outcome.record.Detail)
	}
}
