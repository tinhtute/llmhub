package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// GeminiExecutor with ResponseFormat: FormatClaude must fail the stream on clean EOF
// when no chunk carried a finishReason, instead of synthesizing [DONE] and emitting a fake message_stop.
func TestGeminiExecutorClaudeStreamTruncationReportsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial gemini\"}]}}]}\n\n")
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":  "test-gemini-key",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var output bytes.Buffer
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		output.Write(chunk.Payload)
	}
	if streamErr == nil {
		t.Fatalf("expected error chunk on truncated Gemini-to-Claude stream missing finishReason, got nil; output=%q", output.String())
	}
	if !strings.Contains(streamErr.Error(), "finishReason") {
		t.Fatalf("expected error to mention finishReason, got: %v", streamErr)
	}
	if strings.Contains(output.String(), "message_stop") {
		t.Fatalf("truncated Gemini-to-Claude stream must not synthesize message_stop: %q", output.String())
	}
}

// GeminiExecutor must still complete FormatClaude streams
// when an upstream chunk carries finishReason or promptFeedback.blockReason.
func TestGeminiExecutorClaudeStreamWithFinishSignalCompletes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chunk string
	}{
		{
			name:  "finishReason STOP",
			chunk: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1}}\n\n",
		},
		{
			name:  "promptFeedback blockReason",
			chunk: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"blocked\"}]}}],\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.chunk)
			}))
			defer server.Close()

			exec := NewGeminiExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				Provider: "gemini",
				Attributes: map[string]string{
					"api_key":  "test-gemini-key",
					"base_url": server.URL,
				},
			}
			result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gemini-2.5-flash",
				Payload: []byte(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
			}, cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatClaude,
				ResponseFormat: sdktranslator.FormatClaude,
				Stream:         true,
			})
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			var output bytes.Buffer
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("unexpected error chunk: %v", chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if !strings.Contains(output.String(), "message_stop") {
				t.Fatalf("expected message_stop on completed Gemini stream; output=%q", output.String())
			}
		})
	}
}

// GeminiVertexExecutor with ResponseFormat: FormatClaude must fail the stream on clean EOF
// when no chunk carried a finishReason, instead of synthesizing [DONE] and emitting a fake message_stop.
func TestGeminiVertexExecutorClaudeStreamTruncationReportsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial vertex\"}]}}]}\n\n")
	}))
	defer server.Close()

	exec := NewGeminiVertexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "vertex",
		Attributes: map[string]string{
			"api_key":  "test-vertex-key",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var output bytes.Buffer
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		output.Write(chunk.Payload)
	}
	if streamErr == nil {
		t.Fatalf("expected error chunk on truncated Vertex-to-Claude stream missing finishReason, got nil; output=%q", output.String())
	}
	if !strings.Contains(streamErr.Error(), "finishReason") {
		t.Fatalf("expected error to mention finishReason, got: %v", streamErr)
	}
	if strings.Contains(output.String(), "message_stop") {
		t.Fatalf("truncated Vertex-to-Claude stream must not synthesize message_stop: %q", output.String())
	}
}

// GeminiVertexExecutor must complete FormatClaude streams when finishReason is present.
func TestGeminiVertexExecutorClaudeStreamWithFinishSignalCompletes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1}}\n\n")
	}))
	defer server.Close()

	exec := NewGeminiVertexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "vertex",
		Attributes: map[string]string{
			"api_key":  "test-vertex-key",
			"base_url": server.URL,
		},
	}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected error chunk: %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), "message_stop") {
		t.Fatalf("expected message_stop on completed Vertex stream; output=%q", output.String())
	}
}

// OpenAICompatExecutor with ResponseFormat: FormatClaude must fail the stream on clean EOF
// when no chunk carried finish_reason, rather than synthesizing [DONE] and emitting a fake message_stop.
func TestOpenAICompatExecutorClaudeEOFBeforeFinishReasonFailsStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-truncated\",\"object\":\"chat.completion.chunk\",\"created\":1773896263,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "compat",
			Models: []config.OpenAICompatibilityModel{{
				Name:  "test-model",
				Alias: "test-model",
			}},
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	request := []byte(`{"model":"test-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`)

	result, errExecuteStream := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "test-model", Payload: request}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
		OriginalRequest: request,
		Stream:          true,
	})
	if errExecuteStream != nil {
		t.Fatal(errExecuteStream)
	}

	var output bytes.Buffer
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), `"type":"content_block_delta"`) {
		t.Fatalf("truncated Claude stream did not deliver its valid content frame: %q", output.String())
	}
	if streamErr == nil {
		t.Fatalf("expected error chunk on truncated OpenAI-to-Claude stream missing finish_reason, got nil; output=%q", output.String())
	}
	if !strings.Contains(streamErr.Error(), "[DONE]") {
		t.Fatalf("expected error to mention [DONE], got: %v", streamErr)
	}
	if strings.Contains(output.String(), "message_stop") {
		t.Fatalf("truncated OpenAI-to-Claude stream must not synthesize message_stop: %q", output.String())
	}
}

// OpenAICompatExecutor with ResponseFormat: FormatClaude must still finalize the stream on clean EOF
// when a chunk already carried a non-empty finish_reason (compatibility with providers omitting [DONE]).
func TestOpenAICompatExecutorClaudeEOFAfterFinishReasonCompletesStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-eof\",\"object\":\"chat.completion.chunk\",\"created\":1773896263,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-eof\",\"object\":\"chat.completion.chunk\",\"created\":1773896263,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "compat",
			Models: []config.OpenAICompatibilityModel{{
				Name:  "test-model",
				Alias: "test-model",
			}},
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}
	request := []byte(`{"model":"test-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`)

	result, errExecuteStream := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "test-model", Payload: request}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
		OriginalRequest: request,
		Stream:          true,
	})
	if errExecuteStream != nil {
		t.Fatal(errExecuteStream)
	}

	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error after finish_reason was received: %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), "message_stop") {
		t.Fatalf("expected message_stop when finish_reason was received before clean EOF: %q", output.String())
	}
}

type truncationFailingReader struct {
	prefix io.Reader
	err    error
}

func (r *truncationFailingReader) Read(p []byte) (int, error) {
	if r.prefix != nil {
		n, err := r.prefix.Read(p)
		if err == io.EOF {
			r.prefix = nil
			if n > 0 {
				return n, nil
			}
			return 0, r.err
		}
		return n, err
	}
	return 0, r.err
}

func (r *truncationFailingReader) Close() error { return nil }

// KimiExecutor.ExecuteStream must check scanner.Err() before synthesizing [DONE]
// so a mid-stream read error does not emit a synthetic terminal completion chunk first.
func TestKimiExecutorStreamReadErrorDoesNotSynthesizeDoneBeforeScanError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-kimi\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"kimi-k2.5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"part\"},\"finish_reason\":null}]}\n\n")
		// Force connection drop without [DONE]
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer server.Close()

	exec := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{"access_token": "test-kimi-key"},
	}
	req := cliproxyexecutor.Request{
		Model:   "kimi-k2.5",
		Payload: []byte(`{"model":"kimi-k2.5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var output bytes.Buffer
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		output.Write(chunk.Payload)
	}
	if streamErr == nil {
		t.Fatalf("expected scan error from KimiExecutor stream, got nil; output=%q", output.String())
	}
	if strings.Contains(output.String(), "response.completed") {
		t.Fatalf("KimiExecutor must not synthesize [DONE] (response.completed) when scanner.Err() != nil; output=%q", output.String())
	}
}

func TestClaudeDiagnosticsObservation_MultiLineAndErrors(t *testing.T) {
	multiLineSuccess := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\n" +
		"data: \"message\":{\"id\":\"msg_multi_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\n" +
		"data: \"message_stop\"}\n\n"

	if gotID := claudeMessageIDFromSSE([]byte(multiLineSuccess)); gotID != "msg_multi_1" {
		t.Fatalf("claudeMessageIDFromSSE() on multi-line SSE = %q, want %q", gotID, "msg_multi_1")
	}

	nestedMultiLineTruncated := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_nested_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"emit\",\"input\":\n" +
		"data: {\"type\":\"message_stop\"}\n" +
		"data: }}\n\n"

	if gotID := claudeMessageIDFromSSE([]byte(nestedMultiLineTruncated)); gotID != "" {
		t.Fatalf("claudeMessageIDFromSSE() on truncated stream with nested message_stop line = %q, want empty string", gotID)
	}

	sseWithError := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_err_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
		"event: error\n" +
		"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"

	if gotID := claudeMessageIDFromSSE([]byte(sseWithError)); gotID != "" {
		t.Fatalf("claudeMessageIDFromSSE() on error stream = %q, want empty string so failed turn is not committed", gotID)
	}

	errorEvent := "event: error\n" +
		"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"

	var messageID string
	var completed bool
	var streamErr error
	observeClaudeSSEEvent([]byte(errorEvent), &messageID, &completed, &streamErr)
	if completed {
		t.Fatalf("expected stream with error event to not be completed")
	}
	if streamErr == nil {
		t.Fatalf("expected streamErr to be set on error event")
	}
}

// ClaudeExecutor must surface a clean upstream EOF without message_stop as a
// 502 truncation error on both its passthrough and translated stream loops, so
// consumers other than the /v1/messages proxy (e.g. the OpenAI handlers) do not
// mistake a truncated response for a completed one (upstream 4f2e52709).
func TestClaudeExecutorStreamTruncationReportsError(t *testing.T) {
	for _, tc := range []struct {
		name string
		from sdktranslator.Format
	}{
		{name: "passthrough", from: sdktranslator.FormatClaude},
		{name: "translated to OpenAI Responses", from: sdktranslator.FormatOpenAIResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: message_start\n"+
					`data: {"type":"message_start","message":{"id":"msg_truncated","type":"message","role":"assistant","content":[],"model":"claude-opus-5","stop_reason":null,"usage":{"input_tokens":4,"output_tokens":1}}}`+"\n\n"+
					"event: content_block_delta\n"+
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`+"\n\n")
			}))
			defer server.Close()

			exec := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				Provider: "claude",
				Attributes: map[string]string{
					"api_key":  "test-claude-key",
					"base_url": server.URL,
				},
			}
			result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-opus-5",
				Payload: []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
			}, cliproxyexecutor.Options{
				SourceFormat:   tc.from,
				ResponseFormat: tc.from,
				Stream:         true,
			})
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}

			var streamErr error
			for chunk := range result.Chunks {
				if chunk.Err != nil && streamErr == nil {
					streamErr = chunk.Err
				}
			}
			if streamErr == nil {
				t.Fatal("expected truncation error chunk on clean EOF without message_stop")
			}
			status, ok := streamErr.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusBadGateway {
				t.Fatalf("truncation error = %T (%v), want status %d", streamErr, streamErr, http.StatusBadGateway)
			}
			if !strings.Contains(streamErr.Error(), "upstream stream closed before message_stop") {
				t.Fatalf("truncation error = %v, want message_stop truncation message", streamErr)
			}
		})
	}
}

// A stream that reaches message_stop must keep closing cleanly and committing
// continuity for both the passthrough and translated loops.
func TestClaudeExecutorStreamWithMessageStopCompletes(t *testing.T) {
	for _, tc := range []struct {
		name string
		from sdktranslator.Format
	}{
		{name: "passthrough", from: sdktranslator.FormatClaude},
		{name: "translated to OpenAI Responses", from: sdktranslator.FormatOpenAIResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, claudeTerminalStreamData)
			}))
			defer server.Close()

			exec := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				Provider: "claude",
				Attributes: map[string]string{
					"api_key":  "test-claude-key",
					"base_url": server.URL,
				},
			}
			result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-opus-5",
				Payload: []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
			}, cliproxyexecutor.Options{
				SourceFormat:   tc.from,
				ResponseFormat: tc.from,
				Stream:         true,
			})
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}

			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("unexpected error chunk on completed stream: %v", chunk.Err)
				}
			}
		})
	}
}

// streamTruncatedUsageCapture records every usage record the sink receives.
type streamTruncatedUsageCapture struct {
	records chan usage.Record
}

func (c *streamTruncatedUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type streamTruncatedUsageNoop struct{}

func (streamTruncatedUsageNoop) HandleUsage(context.Context, usage.Record) {}

type streamTruncatedOutcome struct {
	generationPOSTs int
	cooledAuths     int
	record          usage.Record
	hasRecord       bool
	records         int
	streamErr       error
}

// runStreamTruncatedThroughManager drives one streaming request through the auth
// manager with two credentials registered for the same model, so a second
// generation POST is observable exactly when the conductor replayed the request
// on another credential.
func runStreamTruncatedThroughManager(t *testing.T, provider string, exec cliproxyauth.ProviderExecutor, model string, requestTimeout time.Duration, wantRecords int, upstream func(http.ResponseWriter, *http.Request, <-chan struct{})) streamTruncatedOutcome {
	t.Helper()

	var posts atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		upstream(w, r, release)
	}))
	// Release any handler still holding the connection before Close waits on it.
	defer func() {
		close(release)
		server.Close()
	}()

	cfg := &config.Config{}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(exec)

	reg := registry.GetGlobalRegistry()
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("%s-%d", t.Name(), i)
		reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), &cliproxyauth.Auth{
			ID:       id,
			Provider: provider,
			Status:   cliproxyauth.StatusActive,
			Attributes: map[string]string{
				"api_key":  fmt.Sprintf("synthetic-key-%d", i),
				"base_url": server.URL,
			},
		}); err != nil {
			t.Fatalf("manager.Register(%s) error = %v", id, err)
		}
	}

	capture := &streamTruncatedUsageCapture{records: make(chan usage.Record, 8)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), streamTruncatedUsageNoop{}) })

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	result, errStream := manager.ExecuteStream(ctx, []string{provider}, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(fmt.Sprintf(`{"model":%q,"max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Say hello"}]}`, model)),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
		Stream:         true,
	})

	outcome := streamTruncatedOutcome{streamErr: errStream}
	if result != nil {
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				outcome.streamErr = chunk.Err
			}
		}
	}
	outcome.generationPOSTs = int(posts.Load())

	now := time.Now()
	for _, auth := range manager.List() {
		if state := auth.ModelStates[model]; state != nil && state.Unavailable && state.NextRetryAfter.After(now) {
			outcome.cooledAuths++
			continue
		}
		if auth.Unavailable && auth.NextRetryAfter.After(now) {
			outcome.cooledAuths++
		}
	}

	// Every upstream attempt publishes its own record; collect exactly that many
	// (the sink is asynchronous) so a duplicate from a second publish path is
	// visible instead of being mistaken for the next attempt's record.
	wait := time.After(2 * time.Second)
	for outcome.records < wantRecords {
		select {
		case record := <-capture.records:
			if !outcome.hasRecord {
				outcome.record = record
				outcome.hasRecord = true
			}
			outcome.records++
		case <-wait:
			return outcome
		}
	}
	return outcome
}

// TestGeminiStreamTruncatedMissingTerminalSkipsCooldownAndReplay pins upstream
// aa4130e6 (#7005) end to end through the auth manager: an accepted HTTP 200
// stream that ends without a terminal frame stays a public 502, records a
// failure carrying the truncation code, and neither cools the credential nor
// replays the generation on the second credential. A genuine upstream 5xx and a
// timeout keep today's behaviour.
func TestGeminiStreamTruncatedMissingTerminalSkipsCooldownAndReplay(t *testing.T) {
	const model = "gemini-3.8-flash"
	partial := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial answer"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}` + "\n\n"
	complete := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"complete answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}` + "\n\n"
	rejection := `{"error":{"code":502,"message":"mock upstream rejection","status":"UNAVAILABLE"}}`
	// Gemini and Vertex report a rejected generation inside the accepted 200
	// stream. The frame names the failure, so the stream is not a truncation:
	// the credential cools and the failure is not tagged for replay suppression,
	// as before the truncation class existed. The failure surfaces after the
	// translator's message_start, so this layer has no replay to observe; the
	// cooldown and the record's failure code are what the classification decides.
	inBandError := `data: {"error":{"code":429,"message":"mock quota exhausted","status":"RESOURCE_EXHAUSTED"}}` + "\n\n"
	multiFrame := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}` + "\n\n" +
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"b"}]}}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":5,"totalTokenCount":17}}` + "\n\n" +
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"c"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":8,"totalTokenCount":28}}` + "\n\n"

	type testCase struct {
		name           string
		status         int
		body           string
		hang           bool
		timeout        time.Duration
		wantErr        string
		wantPOSTs      int
		wantCooled     int
		wantFailed     bool
		wantFailStatus int
		wantCode       string
		wantDetail     usage.Detail
		wantRecords    int
	}
	cases := []testCase{
		{name: "normal_terminal", status: http.StatusOK, body: complete, wantPOSTs: 1, wantDetail: usage.Detail{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}},
		{name: "multi_frame_usage_publishes_once", status: http.StatusOK, body: multiFrame, wantPOSTs: 1, wantDetail: usage.Detail{InputTokens: 20, OutputTokens: 8, TotalTokens: 28}},
		{name: "partial_then_missing_terminal", status: http.StatusOK, body: partial, wantErr: "before finishReason", wantPOSTs: 1, wantFailed: true, wantFailStatus: http.StatusBadGateway, wantCode: usage.FailureStreamTruncatedCode, wantDetail: usage.Detail{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}},
		{name: "empty_200_missing_terminal", status: http.StatusOK, wantErr: "before finishReason", wantPOSTs: 1, wantFailed: true, wantFailStatus: http.StatusBadGateway, wantCode: usage.FailureStreamTruncatedCode},
		{name: "in_band_error_frame", status: http.StatusOK, body: inBandError, wantErr: "before finishReason", wantPOSTs: 1, wantCooled: 1, wantFailed: true, wantFailStatus: http.StatusBadGateway},
		{name: "real_http_502_control", status: http.StatusBadGateway, body: rejection, wantErr: "mock upstream rejection", wantPOSTs: 2, wantCooled: 2, wantFailed: true, wantFailStatus: http.StatusBadGateway, wantRecords: 2},
		{name: "timeout_control", hang: true, timeout: 200 * time.Millisecond, wantErr: "deadline exceeded", wantPOSTs: -1, wantCooled: 0, wantFailed: true, wantFailStatus: 0},
	}

	for _, execName := range []string{"gemini", "vertex"} {
		for _, tc := range cases {
			if tc.wantRecords == 0 {
				tc.wantRecords = 1
			}
			t.Run(execName+"/"+tc.name, func(t *testing.T) {
				requestTimeout := 5 * time.Second
				if tc.timeout > 0 {
					requestTimeout = tc.timeout
				}
				upstream := func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
					if tc.hang {
						select {
						case <-r.Context().Done():
						case <-release:
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(tc.status)
					if _, errWrite := fmt.Fprint(w, tc.body); errWrite != nil {
						t.Errorf("write mock response: %v", errWrite)
					}
				}
				var provider string
				var exec cliproxyauth.ProviderExecutor
				switch execName {
				case "gemini":
					provider = "gemini"
					exec = NewGeminiExecutor(&config.Config{})
				default:
					provider = "vertex"
					exec = NewGeminiVertexExecutor(&config.Config{})
				}

				outcome := runStreamTruncatedThroughManager(t, provider, exec, model, requestTimeout, tc.wantRecords, upstream)
				t.Logf("status=%d generation_POSTs=%d cooled_auth_models=%d record=%+v streamErr=%v", tc.status, outcome.generationPOSTs, outcome.cooledAuths, outcome.record, outcome.streamErr)

				if tc.wantErr == "" {
					if outcome.streamErr != nil {
						t.Errorf("stream error = %v, want none", outcome.streamErr)
					}
				} else if outcome.streamErr == nil || !strings.Contains(strings.ToLower(outcome.streamErr.Error()), strings.ToLower(tc.wantErr)) {
					t.Errorf("stream error = %v, want text %q", outcome.streamErr, tc.wantErr)
				}
				if tc.name != "timeout_control" && tc.wantErr != "" {
					status, ok := outcome.streamErr.(interface{ StatusCode() int })
					if !ok || status.StatusCode() != http.StatusBadGateway {
						t.Errorf("stream error status = %v, want %d", outcome.streamErr, http.StatusBadGateway)
					}
				}
				if tc.wantPOSTs >= 0 && outcome.generationPOSTs != tc.wantPOSTs {
					t.Errorf("generation POSTs = %d, want %d", outcome.generationPOSTs, tc.wantPOSTs)
				}
				if outcome.cooledAuths != tc.wantCooled {
					t.Errorf("cooled auth/model pairs = %d, want %d", outcome.cooledAuths, tc.wantCooled)
				}
				if !outcome.hasRecord {
					t.Fatalf("no usage record reached the sink")
				}
				if outcome.record.Failed != tc.wantFailed {
					t.Errorf("usage record Failed = %v, want %v (record=%+v)", outcome.record.Failed, tc.wantFailed, outcome.record)
				}
				if tc.wantFailed && outcome.record.Fail.StatusCode != tc.wantFailStatus {
					t.Errorf("usage record failure status = %d, want %d", outcome.record.Fail.StatusCode, tc.wantFailStatus)
				}
				if outcome.record.Fail.Code != tc.wantCode {
					t.Errorf("usage record failure code = %q, want %q", outcome.record.Fail.Code, tc.wantCode)
				}
				if tc.wantDetail != (usage.Detail{}) {
					if got := outcome.record.Detail; got.InputTokens != tc.wantDetail.InputTokens || got.OutputTokens != tc.wantDetail.OutputTokens || got.TotalTokens != tc.wantDetail.TotalTokens {
						t.Errorf("usage record detail = %+v, want %+v", got, tc.wantDetail)
					}
				}
				if outcome.records != tc.wantRecords {
					t.Errorf("sink received %d usage records, want %d (one per upstream attempt)", outcome.records, tc.wantRecords)
				}
			})
		}
	}
}
