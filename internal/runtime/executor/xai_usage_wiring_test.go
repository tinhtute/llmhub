package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

const xaiUsageWiringCompleted = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"grok-4.5\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"

func registerXAIUsageCapture(t *testing.T) *multiProviderUsageCapture {
	t.Helper()
	capture := &multiProviderUsageCapture{alias: t.Name(), records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
	})
	return capture
}

func newXAIUsageTestAuth(serverURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "xai-usage-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":  serverURL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
}

func assertNoAdditionalXAIUsageRecord(t *testing.T, capture *multiProviderUsageCapture) {
	t.Helper()
	select {
	case extra := <-capture.records:
		t.Fatalf("unexpected additional usage record: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestXAIExecutorExecuteStreamPublishesUsageOnce proves the stream observes the
// terminal frame into one buffer and publishes a single record when the
// goroutine finishes.
func TestXAIExecutorExecuteStreamPublishesUsageOnce(t *testing.T) {
	capture := registerXAIUsageCapture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
		_, _ = w.Write([]byte(xaiUsageWiringCompleted))
	}))
	defer server.Close()

	result, err := NewXAIExecutor(&config.Config{}).ExecuteStream(
		coreusage.WithRequestedModelAlias(context.Background(), t.Name()),
		newXAIUsageTestAuth(server.URL),
		cliproxyexecutor.Request{
			Model:   "grok-4.5",
			Payload: []byte(`{"model":"grok-4.5","input":"hi","stream":true}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	record := capture.await(t)
	if record.Failed {
		t.Fatalf("failed = true, want false; failure=%+v", record.Fail)
	}
	if record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 10 {
		t.Fatalf("detail = %+v, want input/output/total = 7/3/10", record.Detail)
	}
	assertNoAdditionalXAIUsageRecord(t, capture)
}

// TestXAIExecutorExecutePublishesUsage proves the non-stream path buffers the
// SSE frames and publishes the terminal usage once.
func TestXAIExecutorExecutePublishesUsage(t *testing.T) {
	capture := registerXAIUsageCapture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
		_, _ = w.Write([]byte(xaiUsageWiringCompleted))
	}))
	defer server.Close()

	_, err := NewXAIExecutor(&config.Config{}).Execute(
		coreusage.WithRequestedModelAlias(context.Background(), t.Name()),
		newXAIUsageTestAuth(server.URL),
		cliproxyexecutor.Request{
			Model:   "grok-4.5",
			Payload: []byte(`{"model":"grok-4.5","input":"hi"}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	record := capture.await(t)
	if record.Failed {
		t.Fatalf("failed = true, want false; failure=%+v", record.Fail)
	}
	if record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 10 {
		t.Fatalf("detail = %+v, want input/output/total = 7/3/10", record.Detail)
	}
	assertNoAdditionalXAIUsageRecord(t, capture)
}

// TestXAIExecutorExecuteImagesPublishesImageUsage proves media responses
// publish structured image usage instead of an empty record.
func TestXAIExecutorExecuteImagesPublishesImageUsage(t *testing.T) {
	capture := registerXAIUsageCapture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"AA=="}]}`))
	}))
	defer server.Close()

	_, err := NewXAIExecutor(&config.Config{}).Execute(
		coreusage.WithRequestedModelAlias(context.Background(), t.Name()),
		newXAIUsageTestAuth(server.URL),
		cliproxyexecutor.Request{
			Model:   "grok-imagine-image",
			Payload: []byte(`{"model":"grok-imagine-image","prompt":"draw"}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-image"),
			Metadata: map[string]any{
				cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
			},
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	record := capture.await(t)
	if record.Failed {
		t.Fatalf("failed = true, want false; failure=%+v", record.Fail)
	}
	if record.Detail.ImageCount == nil || *record.Detail.ImageCount != 1 {
		t.Fatalf("detail = %+v, want ImageCount=1", record.Detail)
	}
	if record.Detail.TotalTokens != 0 {
		t.Fatalf("detail = %+v, want zero token usage", record.Detail)
	}
}
