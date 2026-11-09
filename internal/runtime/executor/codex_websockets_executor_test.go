package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	sdkconfig "github.com/therealtinhtute/llmhub/sdk/config"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body)

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := gjson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

func TestCodexWebsocketsExecutePreservesPreviousResponseIDUpstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %s, want /responses", r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer func() { _ = conn.Close() }()

		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream websocket message: %v", err)
		}
		if msgType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", msgType)
		}
		capturedPayload <- bytes.Clone(payload)

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Fatalf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("upstream type = %s, want response.create; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-1" {
			t.Fatalf("upstream previous_response_id = %s, want resp-1; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}
}

func TestCodexAutoWebSocketDeclarationCollisionFailsBeforeDial(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		connections.Add(1)
	}))
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":    "sk-test",
		"base_url":   server.URL,
		"websockets": "true",
	}}
	req := cliproxyexecutor.Request{
		Model: "gpt-5-codex",
		Payload: []byte(`{
			"model":"gpt-5-codex",
			"input":"hello",
			"tools":[
				{"type":"function","name":"repo__read"},
				{"type":"namespace","name":"repo","tools":[{"type":"function","name":"read"}]}
			]
		}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	_, err := exec.Execute(ctx, auth, req, opts)
	assertCodexToolNameCollisionError(t, err)
	if got := connections.Load(); got != 0 {
		t.Fatalf("upstream connections after Execute = %d, want 0", got)
	}

	_, err = exec.ExecuteStream(ctx, auth, req, opts)
	assertCodexToolNameCollisionError(t, err)
	if got := connections.Load(); got != 0 {
		t.Fatalf("upstream connections after ExecuteStream = %d, want 0", got)
	}
}

func TestCodexAutoWebSocketRestoresNamespacedNonStreamCall(t *testing.T) {
	server := newCodexWebsocketResponseServer(t, []string{
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_read","name":"repo__read","arguments":"{}"}]}}`,
	})
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":    "sk-test",
		"base_url":   server.URL,
		"websockets": "true",
	}}
	payload := []byte(`{"model":"gpt-5-codex","input":"hello","tools":[{"type":"namespace","name":"repo","tools":[{"type":"function","name":"read"}]}]}`)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.name").String(); got != "read" {
		t.Fatalf("output name = %q, want read: %s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.namespace").String(); got != "repo" {
		t.Fatalf("output namespace = %q, want repo: %s", got, resp.Payload)
	}
}

func TestCodexAutoWebSocketRestoresNamespacedStreamingCall(t *testing.T) {
	server := newCodexWebsocketResponseServer(t, []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_read","name":"repo__read","arguments":""}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_read","name":"repo__read","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_read","name":"repo__read","arguments":"{}"}]}}`,
	})
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":    "sk-test",
		"base_url":   server.URL,
		"websockets": "true",
	}}
	payload := []byte(`{"model":"gpt-5-codex","input":"hello","tools":[{"type":"namespace","name":"repo","tools":[{"type":"function","name":"read"}]}]}`)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	got := collectCodexWebsocketStream(t, result)
	if !strings.Contains(got, `"name":"read"`) || !strings.Contains(got, `"namespace":"repo"`) {
		t.Fatalf("stream did not restore namespace/name: %s", got)
	}
	if strings.Contains(got, `"name":"repo__read"`) {
		t.Fatalf("stream retained effective name: %s", got)
	}
}

func TestCodexAutoWebSocketRestoresCustomToolStreamingLifecycle(t *testing.T) {
	server := newCodexWebsocketResponseServer(t, []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"input\":\"diff\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"input\":\"diff\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":"{\"input\":\"diff\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":"{\"input\":\"diff\"}"}]}}`,
	})
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":    "sk-test",
		"base_url":   server.URL,
		"websockets": "true",
	}}
	payload := []byte(`{"model":"gpt-5-codex","input":"hello","tools":[{"type":"custom","name":"patch"}]}`)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	got := collectCodexWebsocketStream(t, result)
	for _, want := range []string{
		`"type":"response.custom_tool_call_input.delta"`,
		`"type":"response.custom_tool_call_input.done"`,
		`"type":"custom_tool_call"`,
		`"item_id":"ctc_1"`,
		`"id":"ctc_1"`,
		`"input":"diff"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream missing %s: %s", want, got)
		}
	}
	for _, unwanted := range []string{
		`"type":"response.function_call_arguments.delta"`,
		`"type":"response.function_call_arguments.done"`,
		`"item_id":"fc_1"`,
		`"id":"fc_1"`,
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("stream retained %s: %s", unwanted, got)
		}
	}
}

func TestCodexAutoWebSocketRestoresFragmentedCustomToolStreamingLifecycle(t *testing.T) {
	server := newCodexWebsocketResponseServer(t, []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"in"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"put\":\"d"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"if"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"f\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"input\":\"diff\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":"{\"input\":\"diff\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":"{\"input\":\"diff\"}"}]}}`,
	})
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":    "sk-test",
		"base_url":   server.URL,
		"websockets": "true",
	}}
	payload := []byte(`{"model":"gpt-5-codex","input":"hello","tools":[{"type":"custom","name":"patch"}]}`)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	got := collectCodexWebsocketStream(t, result)
	if deltas := collectCustomToolInputDeltas(got); deltas != "diff" {
		t.Fatalf("custom input deltas = %q, want diff: %s", deltas, got)
	}
	for _, want := range []string{`"item_id":"ctc_1"`, `"id":"ctc_1"`, `"input":"diff"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream missing %s: %s", want, got)
		}
	}
}

func TestCodexAutoWebSocketPreservesRawBraceAndIncompleteCustomToolInput(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		wantInput string
	}{
		{name: "raw brace text", arguments: `{diff --git a/file b/file`, wantInput: `{diff --git a/file b/file`},
		{name: "incomplete wrapper", arguments: `{"input":"diff`, wantInput: `{"input":"diff`},
		{name: "raw JSON with extra member", arguments: `{"input":"literal","other":1}`, wantInput: `{"input":"literal","other":1}`},
		{name: "exact compatibility wrapper", arguments: `{"input":"literal"}`, wantInput: "literal"},
		{name: "leading whitespace exact wrapper", arguments: " \n { \t\"input\" : \"snow \\u96ea\" } \r", wantInput: "snow 雪"},
		{name: "null input remains raw", arguments: `{"input":null}`, wantInput: `{"input":null}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := []string{`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":""}}`}
			for i := 0; i < len(tt.arguments); i++ {
				event := []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":""}`)
				event, _ = sjson.SetBytes(event, "delta", tt.arguments[i:i+1])
				responses = append(responses, string(event))
			}
			done := []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":""}`)
			done, _ = sjson.SetBytes(done, "arguments", tt.arguments)
			responses = append(responses, string(done))
			itemDone := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":""}}`)
			itemDone, _ = sjson.SetBytes(itemDone, "item.arguments", tt.arguments)
			responses = append(responses, string(itemDone))
			completed := []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_patch","name":"patch","arguments":""}]}}`)
			completed, _ = sjson.SetBytes(completed, "response.output.0.arguments", tt.arguments)
			responses = append(responses, string(completed))

			server := newCodexWebsocketResponseServer(t, responses)
			defer server.Close()
			exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"api_key":    "sk-test",
				"base_url":   server.URL,
				"websockets": "true",
			}}
			payload := []byte(`{"model":"gpt-5-codex","input":"hello","tools":[{"type":"custom","name":"patch"}]}`)
			ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
			result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			assertCustomToolInputLifecycle(t, collectCodexWebsocketStream(t, result), tt.wantInput)
		})
	}
}

func newCodexWebsocketResponseServer(t *testing.T, responses []string) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read websocket request: %v", errRead)
			return
		}
		for _, response := range responses {
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(response)); errWrite != nil {
				t.Errorf("write websocket response: %v", errWrite)
				return
			}
		}
	}))
}

func collectCodexWebsocketStream(t *testing.T, result *cliproxyexecutor.StreamResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("stream result is nil")
	}
	var all bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		all.Write(chunk.Payload)
	}
	return all.String()
}

func assertCodexToolNameCollisionError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want tool_name_collision")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("error = %T %v, want HTTP 400", err, err)
	}
	if got := gjson.Get(err.Error(), "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error: %v", got, err)
	}
	if got := gjson.Get(err.Error(), "error.code").String(); got != "tool_name_collision" {
		t.Fatalf("error.code = %q, want tool_name_collision: %v", got, err)
	}
}

func TestCodexWebsocketsExecuteMaps1009MessageTooBig(t *testing.T) {
	server := newCodexWebsocketCloseServer(t, websocket.CloseMessageTooBig)
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-1", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	_, err := exec.Execute(context.Background(), auth, req, opts)
	assertCodexWebsocketMessageTooBigError(t, err)
}

func TestCodexWebsocketsExecuteStreamMaps1009MessageTooBig(t *testing.T) {
	server := newCodexWebsocketCloseServer(t, websocket.CloseMessageTooBig)
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-1", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[]}`)}
	sessionID := "message-too-big-stream-session"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("codex"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: sessionID,
		},
	}
	defer exec.CloseExecutionSession(sessionID)

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result == nil {
		t.Fatal("ExecuteStream() result is nil")
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
	}
	assertCodexWebsocketMessageTooBigError(t, streamErr)
	select {
	case disconnectErr, ok := <-disconnectCh:
		t.Fatalf("request-scoped 1009 notified session disconnect: error=%v open=%v", disconnectErr, ok)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestMapCodexWebsocketReadErrorNon1009Control(t *testing.T) {
	original := &websocket.CloseError{Code: websocket.CloseInternalServerErr, Text: "upstream failed"}
	if got := mapCodexWebsocketReadError(original); got != original {
		t.Fatalf("mapCodexWebsocketReadError() = %T %v, want original error", got, got)
	}
}

func TestMapCodexWebsocketWriteErrorObserved1009(t *testing.T) {
	sess := &codexWebsocketSession{}
	conn := &websocket.Conn{}
	sess.resetUpstreamDisconnectError(conn)
	sess.setUpstreamDisconnectError(conn, &websocket.CloseError{Code: websocket.CloseMessageTooBig, Text: "upstream close"})

	errMapped := mapCodexWebsocketWriteError(sess, conn, errors.New("write failed"))
	assertCodexWebsocketMessageTooBigError(t, errMapped)
	if shouldRetryCodexWebsocketSend(errMapped) {
		t.Fatal("request-scoped writer error must not retry")
	}
}

func TestMapCodexWebsocketWriteErrorObservedNon1009RetainsRetry(t *testing.T) {
	sess := &codexWebsocketSession{}
	conn := &websocket.Conn{}
	sess.resetUpstreamDisconnectError(conn)
	sess.setUpstreamDisconnectError(conn, &websocket.CloseError{Code: websocket.CloseInternalServerErr, Text: "upstream failed"})
	writeErr := errors.New("write failed")

	errMapped := mapCodexWebsocketWriteError(sess, conn, writeErr)
	if errMapped != writeErr {
		t.Fatalf("non-1009 writer error = %v, want original %v", errMapped, writeErr)
	}
	if !shouldRetryCodexWebsocketSend(errMapped) {
		t.Fatal("non-1009 writer error must retain retry behavior")
	}
}

func TestMapCodexWebsocketWriteErrorSilentReaderReturnsImmediately(t *testing.T) {
	sess := &codexWebsocketSession{}
	conn := &websocket.Conn{}
	sess.resetUpstreamDisconnectError(conn)
	readCh := sess.activate(conn)
	defer sess.clearActive(conn, readCh)
	writeErr := errors.New("write failed")

	mapped := make(chan error, 1)
	go func() {
		mapped <- mapCodexWebsocketWriteError(sess, conn, writeErr)
	}()

	select {
	case errMapped := <-mapped:
		if errMapped != writeErr {
			t.Fatalf("silent-reader writer error = %v, want original %v", errMapped, writeErr)
		}
		if !shouldRetryCodexWebsocketSend(errMapped) {
			t.Fatal("silent-reader writer error must retain retry behavior")
		}
	case <-time.After(time.Second):
		t.Fatal("writer waited for silent reader classification")
	}
}

func TestMapCodexWebsocketWriteErrorIgnoresPriorConnectionError(t *testing.T) {
	sess := &codexWebsocketSession{}
	previousConn := &websocket.Conn{}
	currentConn := &websocket.Conn{}
	sess.resetUpstreamDisconnectError(previousConn)
	sess.setUpstreamDisconnectError(previousConn, &websocket.CloseError{Code: websocket.CloseMessageTooBig, Text: "upstream close"})
	sess.resetUpstreamDisconnectError(currentConn)
	writeErr := errors.New("write failed")

	errMapped := mapCodexWebsocketWriteError(sess, currentConn, writeErr)
	if errMapped != writeErr {
		t.Fatalf("current connection writer error = %v, want original %v", errMapped, writeErr)
	}
	if !shouldRetryCodexWebsocketSend(errMapped) {
		t.Fatal("prior connection close must not suppress current connection retry")
	}
}

func TestDetachMismatchedWebsocketSessionConn(t *testing.T) {
	conn := &websocket.Conn{}
	sess := &codexWebsocketSession{
		conn:       conn,
		readerConn: conn,
		authID:     "auth-a",
		wsURL:      "wss://example.test/a",
	}

	staleConn, staleAuthID, staleWSURL, _ := detachMismatchedWebsocketSessionConn(sess, "auth-b", "wss://example.test/b")
	if staleConn != conn {
		t.Fatalf("stale conn = %p, want %p", staleConn, conn)
	}
	if staleAuthID != "auth-a" || staleWSURL != "wss://example.test/a" {
		t.Fatalf("stale target = (%q, %q), want auth-a and original URL", staleAuthID, staleWSURL)
	}
	if sess.conn != nil || sess.readerConn != nil {
		t.Fatalf("mismatched session retained stale connection: %#v", sess)
	}
}

func TestCodexWebsocket1009BackpressureDeliversTerminalError(t *testing.T) {
	closeNow := make(chan struct{})
	serverErr := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.Close() }()
		<-closeNow
		payload := websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "upstream close")
		serverErr <- conn.WriteControl(websocket.CloseMessage, payload, time.Now().Add(time.Second))
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "message-too-big-backpressure-session"
	sess := exec.getOrCreateSession(sessionID)
	defer exec.CloseExecutionSession(sessionID)
	sess.connMu.Lock()
	sess.conn = conn
	sess.readerConn = conn
	sess.wsURL = wsURL
	sess.authID = "auth-1"
	sess.connMu.Unlock()
	sess.configureConn(conn)

	readCh := make(chan codexWebsocketRead, 1)
	readCh <- codexWebsocketRead{conn: conn, msgType: websocket.TextMessage, payload: []byte("queued")}
	sess.setActive(conn, readCh)
	go exec.readUpstreamLoop(sess, conn)
	close(closeNow)

	waitForCodexWebsocketCondition(t, func() bool {
		sess.connMu.Lock()
		defer sess.connMu.Unlock()
		return sess.conn == nil
	}, "upstream connection was not detached before terminal delivery")

	queued := <-readCh
	if string(queued.payload) != "queued" {
		t.Fatalf("queued payload = %q, want queued", queued.payload)
	}
	terminal, ok := <-readCh
	if !ok {
		t.Fatal("terminal error was dropped when active channel was full")
	}
	assertCodexWebsocketMessageTooBigError(t, terminal.err)

	select {
	case errServer := <-serverErr:
		if errServer != nil {
			t.Fatalf("write websocket close: %v", errServer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket close write")
	}
}

func waitForCodexWebsocketCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func newCodexWebsocketCloseServer(t *testing.T, closeCode int) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read websocket request: %v", errRead)
			return
		}
		payload := websocket.FormatCloseMessage(closeCode, "upstream close")
		if errWrite := conn.WriteControl(websocket.CloseMessage, payload, time.Now().Add(time.Second)); errWrite != nil {
			t.Errorf("write websocket close: %v", errWrite)
		}
	}))
}

func assertCodexWebsocketMessageTooBigError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected message-too-big error")
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %#v, want %d", err, http.StatusRequestEntityTooLarge)
	}
	requestScoped, ok := err.(interface{ IsRequestScoped() bool })
	if !ok || !requestScoped.IsRequestScoped() {
		t.Fatalf("error = %T, want request-scoped error", err)
	}
	parsed := gjson.Parse(err.Error())
	if got := parsed.Get("error.message").String(); got != "upstream websocket message too big" {
		t.Fatalf("message = %q, want upstream websocket message too big", got)
	}
	if got := parsed.Get("error.type").String(); got != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", got)
	}
	if got := parsed.Get("error.code").String(); got != "message_too_big" {
		t.Fatalf("code = %q, want message_too_big", got)
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanSignalsOnInvalidate(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "sess-1"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	if disconnectCh == nil {
		t.Fatal("expected disconnect channel")
	}

	sess := exec.getOrCreateSession(sessionID)
	if sess == nil {
		t.Fatal("expected session")
	}
	sess.connMu.Lock()
	sess.conn = conn
	sess.authID = "auth-1"
	sess.wsURL = "ws://example.test/responses"
	sess.readerConn = conn
	sess.connMu.Unlock()

	upstreamErr := errors.New("upstream gone")
	exec.invalidateUpstreamConn(sess, conn, "test_invalidate", upstreamErr)

	select {
	case errRead, ok := <-disconnectCh:
		if !ok {
			t.Fatal("expected disconnect channel to deliver error before closing")
		}
		if errRead == nil || errRead.Error() != upstreamErr.Error() {
			t.Fatalf("disconnect error = %v, want %v", errRead, upstreamErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disconnect signal")
	}
}

func TestApplyCodexWebsocketHeadersDefaultsToCurrentResponsesBeta(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil, false)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want %s", got, codexUserAgent)
	}
	if !strings.HasPrefix(codexUserAgent, codexOriginator+"/") {
		t.Fatalf("default Codex User-Agent = %s, want prefix %s/", codexUserAgent, codexOriginator)
	}
	if strings.HasPrefix(codexUserAgent, "codex-tui/") {
		t.Fatalf("default Codex User-Agent = %s, must not use stale codex-tui prefix", codexUserAgent)
	}
	if strings.Contains(codexUserAgent, "(codex-tui;") {
		t.Fatalf("default Codex User-Agent = %s, must not include stale codex-tui suffix", codexUserAgent)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"User-Agent":            "codex_cli_rs/0.1.0",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
		"session_id":            "sess-client",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil, false)

	if got := headers.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := headers.Get("User-Agent"); got != "codex_cli_rs/0.1.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "codex_cli_rs/0.1.0")
	}
	if got := headers.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
	if got := headerValueCaseInsensitive(headers, "session_id"); got != "sess-client" {
		t.Fatalf("session_id = %s, want sess-client", got)
	}
	if _, ok := headers["session_id"]; !ok {
		t.Fatalf("expected lowercase session_id header key, got %#v", headers)
	}
}

func TestApplyCodexWebsocketHeadersUsesConfigDefaultsForOAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "my-codex-client/1.0",
			BetaFeatures: "feature-a,feature-b",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg, false)

	if got := headers.Get("User-Agent"); got != "my-codex-client/1.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "my-codex-client/1.0")
	}
	if got := headers.Get("x-codex-beta-features"); got != "feature-a,feature-b" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "feature-a,feature-b")
	}
	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
}

func TestApplyCodexWebsocketHeadersPrefersExistingHeadersOverClientAndConfig(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})
	headers := http.Header{}
	headers.Set("User-Agent", "existing-ua")
	headers.Set("X-Codex-Beta-Features", "existing-beta")

	got := applyCodexWebsocketHeaders(ctx, headers, auth, "", cfg, false)

	if gotVal := got.Get("User-Agent"); gotVal != "existing-ua" {
		t.Fatalf("User-Agent = %s, want %s", gotVal, "existing-ua")
	}
	if gotVal := got.Get("x-codex-beta-features"); gotVal != "existing-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", gotVal, "existing-beta")
	}
}

func TestApplyCodexWebsocketHeadersConfigUserAgentOverridesClientHeader(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", cfg, false)

	if got := headers.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := headers.Get("x-codex-beta-features"); got != "client-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "client-beta")
	}
}

func TestApplyCodexWebsocketHeadersDisablesInjectedCloaking(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct-1"},
	}
	ctx := cliproxyexecutor.WithCodexCloakingDisabled(context.Background())

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", cfg, false)

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("Originator"); got != "" {
		t.Fatalf("Originator = %s, want empty", got)
	}
	if got := headerValueCaseInsensitive(headers, "ChatGPT-Account-ID"); got != "" {
		t.Fatalf("ChatGPT-Account-ID = %s, want empty", got)
	}
	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
}

func TestApplyCodexWebsocketHeadersIgnoresConfigForAPIKeyAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "sk-test", cfg, false)

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("Originator"); got != "" {
		t.Fatalf("Originator = %s, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersPreservesExplicitAPIKeyUserAgent(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}
	ctx := contextWithGinHeaders(map[string]string{"User-Agent": "api-key-client/1.0", "Originator": "explicit-origin"})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "sk-test", nil, false)

	if got := headers.Get("User-Agent"); got != "api-key-client/1.0" {
		t.Fatalf("User-Agent = %s, want api-key-client/1.0", got)
	}
	if got := headers.Get("Originator"); got != "explicit-origin" {
		t.Fatalf("Originator = %s, want explicit-origin", got)
	}
}

func TestApplyCodexPromptCacheHeadersSetsLowercaseSessionAndLegacyConversation(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"prompt_cache_key":"cache-1"}`)}

	_, headers := applyCodexPromptCacheHeaders("openai-response", req, []byte(`{"model":"gpt-5-codex"}`))

	if got := headerValueCaseInsensitive(headers, "session_id"); got != "cache-1" {
		t.Fatalf("session_id = %s, want cache-1", got)
	}
	if _, ok := headers["session_id"]; !ok {
		t.Fatalf("expected lowercase session_id key, got %#v", headers)
	}
	if got := headers.Get("Conversation_id"); got != "cache-1" {
		t.Fatalf("Conversation_id = %s, want cache-1", got)
	}
}

func TestApplyCodexWebsocketHeadersUsesCanonicalAccountHeader(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-1"}}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", nil, false)

	if got := headerValueCaseInsensitive(headers, "ChatGPT-Account-ID"); got != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID = %s, want acct-1", got)
	}
	values, ok := headers["ChatGPT-Account-ID"]
	if !ok {
		t.Fatalf("expected exact ChatGPT-Account-ID key, got %#v", headers)
	}
	if len(values) != 1 || values[0] != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID values = %#v, want [acct-1]", values)
	}
}

func TestBuildCodexResponsesWebsocketURLRequiresHTTPURL(t *testing.T) {
	if got, err := buildCodexResponsesWebsocketURL("https://example.com/backend/responses"); err != nil || got != "wss://example.com/backend/responses" {
		t.Fatalf("https URL = %q, %v; want wss URL", got, err)
	}
	if _, err := buildCodexResponsesWebsocketURL("ftp://example.com/responses"); err == nil {
		t.Fatalf("expected unsupported scheme error")
	}
	if _, err := buildCodexResponsesWebsocketURL("https:///responses"); err == nil {
		t.Fatalf("expected empty host error")
	}
}

func TestParseCodexWebsocketErrorMarksConnectionLimitRetryable(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached","message":"too many websockets"},"headers":{"retry-after":"1"}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %#v, want 429", err)
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected retryable websocket connection limit error")
	}
	if got := *retryable.RetryAfter(); got != 0 {
		t.Fatalf("retryAfter = %v, want connection-limit fallback 0", got)
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("retry-after") != "1" {
		t.Fatalf("headers = %#v, want retry-after", err)
	}
}

func TestParseCodexWebsocketErrorUsesUsageLimitRetryMetadata(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_in_seconds":7}}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}

	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected retryable usage limit websocket error")
	}
	if got := *retryable.RetryAfter(); got != 7*time.Second {
		t.Fatalf("retryAfter = %v, want 7s", got)
	}
}

func TestParseCodexWebsocketErrorPreservesWrappedBodyAndHeaders(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":{"code":"websocket_connection_limit_reached","type":"server_error","message":"too many websocket connections"}},"headers":{"x-request-id":"req-1"}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}

	parsed := gjson.Parse(err.Error())
	if got := parsed.Get("status").Int(); got != http.StatusTooManyRequests {
		t.Fatalf("wrapped status = %d, want 429; payload=%s", got, err.Error())
	}
	if got := parsed.Get("body.error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf("wrapped body error code = %s, want websocket_connection_limit_reached; payload=%s", got, err.Error())
	}
	if got := parsed.Get("error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf("surface error code = %s, want websocket_connection_limit_reached; payload=%s", got, err.Error())
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected body.error.code websocket connection limit to be retryable")
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("x-request-id") != "req-1" {
		t.Fatalf("headers = %#v, want x-request-id", err)
	}
}

func TestApplyCodexHeadersDisablesInjectedCloaking(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent: "config-ua",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct-1"},
	}
	req = req.WithContext(cliproxyexecutor.WithCodexCloakingDisabled(req.Context()))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := req.Header.Get("Originator"); got != "" {
		t.Fatalf("Originator = %s, want empty", got)
	}
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "" {
		t.Fatalf("Chatgpt-Account-Id = %s, want empty", got)
	}
}

func TestApplyCodexHeadersUsesConfigUserAgentForOAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := req.Header.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
}

func TestApplyCodexHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := req.Header.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
}

func TestApplyCodexHeadersDoesNotInjectClientOnlyHeadersByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	applyCodexHeaders(req, nil, "oauth-token", true, nil)

	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func contextWithGinHeaders(headers map[string]string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ginCtx.Request.Header = make(http.Header, len(headers))
	for key, value := range headers {
		ginCtx.Request.Header.Set(key, value)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestNewProxyAwareWebsocketDialerDirectDisablesProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy != nil {
		t.Fatal("expected websocket proxy function to be nil for direct mode")
	}
}

// TestCodexWebsocketsExecuteStreamUsageUsesCollectedOutputItems proves the
// websocket stream publishes the buffered terminal usage and counts tool calls
// from the collected output_item.done items when response.output is empty.
func TestCodexWebsocketsExecuteStreamUsageUsesCollectedOutputItems(t *testing.T) {
	const alias = "codex-websockets-stream-output-item-usage"
	capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
	})

	server := newCodexWebsocketResponseServer(t, []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[],"usage":{"input_tokens":8,"output_tokens":28,"total_tokens":36}}}`,
	})
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":    "sk-test",
		"base_url":   server.URL,
		"websockets": "true",
	}}
	ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	_ = collectCodexWebsocketStream(t, result)

	record := capture.await(t)
	if record.Detail.InputTokens != 8 || record.Detail.OutputTokens != 28 || record.Detail.TotalTokens != 36 {
		t.Fatalf("usage = %+v, want input/output/total = 8/28/36", record.Detail)
	}
	if record.Detail.ToolUsageDetails == nil || record.Detail.ToolUsageDetails.ToolCallsRequested == nil || *record.Detail.ToolUsageDetails.ToolCallsRequested != 1 {
		t.Fatalf("tool usage = %+v, want 1 requested tool call from the collected output item", record.Detail.ToolUsageDetails)
	}
}
