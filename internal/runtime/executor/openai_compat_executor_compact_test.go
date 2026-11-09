package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorCompactPassthrough(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"gpt-5.1-codex-max","input":[{"role":"user","content":"hi"}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.1-codex-max",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses/compact" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/responses/compact")
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("expected input in body")
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("unexpected messages in body")
	}
	if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorPayloadOverrideWinsOverThinkingSuffix(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{
						{Name: "custom-openai", Protocol: "openai"},
					},
					Params: map[string]any{
						"reasoning_effort": "low",
					},
				},
			},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"custom-openai(high)","messages":[{"role":"user","content":"hi"}]}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "custom-openai(high)",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != "low" {
		t.Fatalf("reasoning_effort = %q, want %q; body=%s", got, "low", string(gotBody))
	}
}

func TestOpenAICompatExecutorImagesGenerationsPassthrough(t *testing.T) {
	var gotPath string
	var gotBody []byte
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"AA=="}],"usage":{"total_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "upstream-image",
		Payload: []byte(`{"model":"compat-image","prompt":"draw"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-image"),
		Stream:       false,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/images/generations")
	}
	if gotContentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", gotContentType)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "data.0.b64_json").String(); got != "AA==" {
		t.Fatalf("response payload = %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorImagesGenerationsStreamsUpstream(t *testing.T) {
	var gotPath string
	var gotBody []byte
	var gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: image_generation.partial\ndata: {\"type\":\"image_generation.partial\"}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	streamResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "upstream-image",
		Payload: []byte(`{"model":"compat-image","prompt":"draw","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-image"),
		Stream:       true,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var streamed bytes.Buffer
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/images/generations")
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("accept = %q, want text/event-stream", gotAccept)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(gotBody))
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("stream flag missing from upstream body: %s", string(gotBody))
	}
	if !strings.Contains(streamed.String(), "event: image_generation.partial") || !strings.Contains(streamed.String(), "data: [DONE]") {
		t.Fatalf("streamed body = %q", streamed.String())
	}
}

func TestOpenAICompatExecutorImagesEditsMultipartRewritesModel(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "compat-image"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "edit"); errWrite != nil {
		t.Fatalf("write prompt field: %v", errWrite)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.png"))
	header.Set("Content-Type", "image/png")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}
	contentType := writer.FormDataContentType()

	var gotPath string
	var gotModel string
	var gotPrompt string
	var gotFile string
	var gotFileContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if errParse := r.ParseMultipartForm(32 << 20); errParse != nil {
			t.Fatalf("parse multipart form: %v", errParse)
		}
		gotModel = r.FormValue("model")
		gotPrompt = r.FormValue("prompt")
		file, fileHeader, errFile := r.FormFile("image")
		if errFile != nil {
			t.Fatalf("read image file: %v", errFile)
		}
		gotFileContentType = fileHeader.Header.Get("Content-Type")
		data, errRead := io.ReadAll(file)
		if errClose := file.Close(); errClose != nil {
			t.Fatalf("close image file: %v", errClose)
		}
		if errRead != nil {
			t.Fatalf("read image file: %v", errRead)
		}
		gotFile = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"AA=="}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "upstream-image",
		Payload: body.Bytes(),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-image"),
		Stream:       false,
		Headers: http.Header{
			"Content-Type": []string{contentType},
		},
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/edits",
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/images/edits" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/images/edits")
	}
	if gotModel != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image", gotModel)
	}
	if gotPrompt != "edit" {
		t.Fatalf("prompt = %q, want edit", gotPrompt)
	}
	if gotFile != "png-data" {
		t.Fatalf("file = %q, want png-data", gotFile)
	}
	if gotFileContentType != "image/png" {
		t.Fatalf("file content type = %q, want image/png", gotFileContentType)
	}
}

func TestRewriteOpenAICompatImagesMultipartPayloadPreservesStreamAndFileContentType(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "compat-image"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatalf("write stream field: %v", errWrite)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.webp"))
	header.Set("Content-Type", "image/webp")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte("webp-data")); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	out, contentType, err := prepareOpenAICompatImagesPayload(body.Bytes(), "upstream-image", writer.FormDataContentType(), true)
	if err != nil {
		t.Fatalf("prepareOpenAICompatImagesPayload error: %v", err)
	}
	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil {
		t.Fatalf("parse content type: %v", errParse)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	reader := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read rewritten form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove form files: %v", errRemove)
		}
	}()
	if got := form.Value["model"]; len(got) != 1 || got[0] != "upstream-image" {
		t.Fatalf("model values = %#v, want upstream-image", got)
	}
	if got := form.Value["stream"]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("stream values = %#v, want true", got)
	}
	if got := form.File["image"]; len(got) != 1 || got[0].Header.Get("Content-Type") != "image/webp" {
		t.Fatalf("image headers = %#v, want image/webp", got)
	}
}

func TestOpenAICompatExecutorStreamRejectsPlainJSONAfterBlankLines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("\n\n: openrouter processing\n\nevent: error\n"))
		_, _ = w.Write([]byte(`{"error":{"message":"upstream failed","type":"server_error"}}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openrouter-model",
		Payload: []byte(`{"model":"openrouter-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var gotErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
			break
		}
	}
	if gotErr == nil {
		t.Fatalf("expected plain JSON stream error")
	}
	if status, ok := gotErr.(interface{ StatusCode() int }); !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("stream error status = %v, want %d", gotErr, http.StatusBadGateway)
	}
	if !strings.Contains(gotErr.Error(), "upstream failed") {
		t.Fatalf("stream error = %v", gotErr)
	}
}

func TestOpenAICompatExecutorStreamSkipsKeepAliveUntilDataLine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("\n\n: openrouter processing\n\nevent: ping\nid: 1\nretry: 1000\n"))
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openrouter-model",
		Payload: []byte(`{"model":"openrouter-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if gjson.Get(got.String(), "choices.0.delta.content").String() != "hello" {
		t.Fatalf("stream payload = %s", got.String())
	}
}

const openAICompatCompactionTestModel = "cline-pass/deepseek-v4.1-flash"

// openAICompatCompactionRecorder captures upstream requests and serves the
// compaction summary fixture. Set incomplete to force a truncated summary.
type openAICompatCompactionRecorder struct {
	server     *httptest.Server
	paths      []string
	bodies     [][]byte
	incomplete bool
}

func (r *openAICompatCompactionRecorder) Close() {
	if r.server != nil {
		r.server.Close()
	}
}

func (r *openAICompatCompactionRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		r.paths = append(r.paths, req.URL.Path)
		r.bodies = append(r.bodies, body)

		if req.URL.Path == "/v1/responses/compact" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_cmp_native","object":"response.compaction","status":"completed","output":[{"id":"cmp_native","type":"compaction","status":"completed","encrypted_content":"gAAAA_native"}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`)
			return
		}
		if r.incomplete {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl_inc","object":"chat.completion","created":1773896263,"model":"cline-pass/deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"thinking","content":"Partial truncated summary."},"finish_reason":"length"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18}}`)
			return
		}
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","created":1773896263,"model":"cline-pass/deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Checking context..."},"finish_reason":null}]}`+"\n\n"+
					`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","created":1773896263,"model":"cline-pass/deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"COBALT-731"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`+"\n\n"+
					"data: [DONE]\n\n",
			)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_sum","object":"chat.completion","created":1773896263,"model":"cline-pass/deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"Summarizing session context.","content":"Summary retaining COBALT-731."},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}}`)
	}
}

func (r *openAICompatCompactionRecorder) lastBody() []byte {
	if len(r.bodies) == 0 {
		return nil
	}
	return r.bodies[len(r.bodies)-1]
}

func (r *openAICompatCompactionRecorder) lastPath() string {
	if len(r.paths) == 0 {
		return ""
	}
	return r.paths[len(r.paths)-1]
}

func newOpenAICompatCompactionTestExecutor() (*OpenAICompatExecutor, *cliproxyauth.Auth, *openAICompatCompactionRecorder) {
	recorder := &openAICompatCompactionRecorder{}
	recorder.server = httptest.NewServer(recorder.handler())
	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: openAICompatCompactionTestModel, Protocol: "openai"}},
				Params: map[string]any{
					"max_tokens": 2048,
				},
			}},
		},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url": recorder.server.URL + "/v1",
			"api_key":  "test-cline-key",
		},
	}
	return executor, auth, recorder
}

func openAICompatCompactionTriggerPayload() []byte {
	return []byte(`{
		"model": "cline-pass/deepseek-v4.1-flash",
		"stream": true,
		"input": [
			{
				"type": "additional_tools",
				"tools": [
					{
						"type": "function",
						"name": "exec_command",
						"description": "Execute a shell command",
						"parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}
					}
				]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Remember our synthetic project marker COBALT-731 and reply with ACK."}]
			},
			{"type": "compaction_trigger"}
		]
	}`)
}

// openAICompatCompactionCompletedResponse scans SSE frames for the
// response.completed event and returns its "response" object.
func openAICompatCompactionCompletedResponse(t *testing.T, stream string) gjson.Result {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if gjson.Get(data, "type").String() != "response.completed" {
			continue
		}
		resp := gjson.Get(data, "response")
		if !resp.Exists() {
			t.Fatalf("response.completed frame missing response object: %s", data)
		}
		return resp
	}
	t.Fatalf("no response.completed frame in stream: %s", stream)
	return gjson.Result{}
}

func TestOpenAICompatExecutorCompactionTriggerSynthesizesCapsule(t *testing.T) {
	executor, auth, recorder := newOpenAICompatCompactionTestExecutor()
	defer recorder.Close()

	triggerPayload := openAICompatCompactionTriggerPayload()
	resp, errExec := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: triggerPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: triggerPayload,
	})
	if errExec != nil {
		t.Fatalf("Execute(compaction_trigger) error = %v", errExec)
	}

	items := gjson.GetBytes(resp.Payload, "output").Array()
	if len(items) != 1 || items[0].Get("type").String() != "compaction" {
		t.Fatalf("expected single compaction item in non-stream response, got: %s", resp.Payload)
	}
	capsule := items[0].Get("encrypted_content").String()
	if !strings.HasPrefix(capsule, "cpa-ag-compact-v1:") {
		t.Fatalf("expected CPA compaction capsule prefix, got %q", capsule)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.input_tokens_details.cached_tokens").Int(); got != 4 {
		t.Fatalf("cached_tokens = %d, want 4; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.output_tokens_details.reasoning_tokens").Int(); got != 2 {
		t.Fatalf("reasoning_tokens = %d, want 2; payload=%s", got, resp.Payload)
	}

	if got := recorder.lastPath(); got != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", got)
	}
	summaryBody := recorder.lastBody()
	if gjson.GetBytes(summaryBody, "stream").Bool() {
		t.Fatalf("expected non-stream summary request upstream, got: %s", summaryBody)
	}
	if gjson.GetBytes(summaryBody, "tools").Exists() {
		t.Fatalf("expected tools (including additional_tools) to be stripped from summary request, got: %s", summaryBody)
	}
	if got := gjson.GetBytes(summaryBody, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want payload rule 2048; body=%s", got, summaryBody)
	}
}

func TestOpenAICompatExecutorCompactionTriggerStreamSynthesizesSSE(t *testing.T) {
	executor, auth, recorder := newOpenAICompatCompactionTestExecutor()
	defer recorder.Close()

	triggerPayload := openAICompatCompactionTriggerPayload()
	streamResult, errStream := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: triggerPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		Stream:          true,
		OriginalRequest: triggerPayload,
	})
	if errStream != nil {
		t.Fatalf("ExecuteStream(compaction_trigger) error = %v", errStream)
	}

	var streamBuf bytes.Buffer
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamBuf.Write(chunk.Payload)
	}
	if got := streamResult.Headers.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if !strings.Contains(streamBuf.String(), "response.created") {
		t.Fatalf("expected response.created frame, got: %s", streamBuf.String())
	}

	completed := openAICompatCompactionCompletedResponse(t, streamBuf.String())
	outputItems := completed.Get("output").Array()
	if len(outputItems) != 1 || outputItems[0].Get("type").String() != "compaction" {
		t.Fatalf("expected single compaction item in stream response, got: %s", completed.Raw)
	}
	capsule := outputItems[0].Get("encrypted_content").String()
	if !strings.HasPrefix(capsule, "cpa-ag-compact-v1:") {
		t.Fatalf("expected CPA compaction capsule prefix, got %q", capsule)
	}
	if got := completed.Get("usage.input_tokens_details.cached_tokens").Int(); got != 4 {
		t.Fatalf("cached_tokens = %d, want 4", got)
	}
	if got := completed.Get("usage.output_tokens_details.reasoning_tokens").Int(); got != 2 {
		t.Fatalf("reasoning_tokens = %d, want 2", got)
	}

	for _, body := range recorder.bodies {
		if gjson.GetBytes(body, "stream").Bool() {
			t.Fatalf("expected no streaming upstream call for compaction synthesis, got: %s", body)
		}
	}
}

func TestOpenAICompatExecutorCompactionReplayExpandsCapsule(t *testing.T) {
	executor, auth, recorder := newOpenAICompatCompactionTestExecutor()
	defer recorder.Close()

	capsule, errSeal := helps.SealAntigravityCompaction("Summary retaining COBALT-731.", openAICompatCompactionTestModel)
	if errSeal != nil {
		t.Fatalf("SealAntigravityCompaction failed: %v", errSeal)
	}
	replayPayload := []byte(fmt.Sprintf(`{
		"model": %q,
		"stream": true,
		"input": [
			{"type": "compaction", "encrypted_content": "foreign-opaque-token"},
			{"type": "compaction", "id": "cmp_ag_compact_1", "encrypted_content": %q},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "What is the project marker?"}]}
		]
	}`, openAICompatCompactionTestModel, capsule))

	streamResult, errStream := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: replayPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		Stream:          true,
		OriginalRequest: replayPayload,
	})
	if errStream != nil {
		t.Fatalf("ExecuteStream(replay) error = %v", errStream)
	}
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("continuation stream chunk error = %v", chunk.Err)
		}
	}

	replayBody := recorder.lastBody()
	if !bytes.Contains(replayBody, []byte("Summary retaining COBALT-731.")) {
		t.Fatalf("expected expanded summary in continuation upstream request, got: %s", replayBody)
	}
	if bytes.Contains(replayBody, []byte("foreign-opaque-token")) {
		t.Fatalf("expected foreign compaction item to be dropped, got: %s", replayBody)
	}

	if _, errNonStreamReplay := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: replayPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: replayPayload,
	}); errNonStreamReplay != nil {
		t.Fatalf("Execute(replay) error = %v", errNonStreamReplay)
	}
	nonStreamReplaySeen := recorder.lastBody()
	if !bytes.Contains(nonStreamReplaySeen, []byte("Summary retaining COBALT-731.")) || bytes.Contains(nonStreamReplaySeen, []byte("foreign-opaque-token")) {
		t.Fatalf("unexpected non-stream replay upstream body: %s", nonStreamReplaySeen)
	}

	triggerPayload := openAICompatCompactionTriggerPayload()
	strippedTriggerPayload := []byte(`{"model":"cline-pass/deepseek-v4.1-flash","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Remember our synthetic project marker COBALT-731 and reply with ACK."}]}]}`)
	origOnlyResp, errOrigOnly := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: strippedTriggerPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: triggerPayload,
	})
	if errOrigOnly != nil {
		t.Fatalf("Execute(OriginalRequest-only trigger) error = %v", errOrigOnly)
	}
	if got := gjson.GetBytes(origOnlyResp.Payload, "output.0.type").String(); got != "compaction" {
		t.Fatalf("OriginalRequest-only trigger output[0].type = %q, want compaction; payload=%s", got, origOnlyResp.Payload)
	}
}

func TestOpenAICompatExecutorCompactionCorruptedCapsuleReturns400(t *testing.T) {
	executor, auth, recorder := newOpenAICompatCompactionTestExecutor()
	defer recorder.Close()

	corruptedPayload := []byte(`{
		"model": "cline-pass/deepseek-v4.1-flash",
		"stream": false,
		"input": [
			{"type": "compaction", "encrypted_content": "cpa-ag-compact-v1:not-valid"},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "continue"}]}
		]
	}`)
	_, errCorrupt := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: corruptedPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: corruptedPayload,
	})
	if errCorrupt == nil {
		t.Fatal("expected error for corrupted compaction capsule, got nil")
	}
	assertStatusErr(t, errCorrupt, http.StatusBadRequest)
}

func TestOpenAICompatExecutorCompactAltExpandsRecognizedOnly(t *testing.T) {
	executor, auth, recorder := newOpenAICompatCompactionTestExecutor()
	defer recorder.Close()

	capsule, errSeal := helps.SealAntigravityCompaction("Summary retaining COBALT-731.", openAICompatCompactionTestModel)
	if errSeal != nil {
		t.Fatalf("SealAntigravityCompaction failed: %v", errSeal)
	}
	compactPassthroughPayload := []byte(fmt.Sprintf(`{
		"model": %q,
		"input": [
			{"type": "compaction", "encrypted_content": "foreign-native-token"},
			{"type": "compaction", "id": "cmp_ag_compact_1", "encrypted_content": %q},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Compact again."}]}
		]
	}`, openAICompatCompactionTestModel, capsule))

	if _, errCompactEndpoint := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: compactPassthroughPayload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Alt:          "responses/compact",
	}); errCompactEndpoint != nil {
		t.Fatalf("Execute(responses/compact) error = %v", errCompactEndpoint)
	}

	if got := recorder.lastPath(); got != "/v1/responses/compact" {
		t.Fatalf("responses/compact path = %q, want /v1/responses/compact", got)
	}
	compactBody := recorder.lastBody()
	if !bytes.Contains(compactBody, []byte("Summary retaining COBALT-731.")) {
		t.Fatalf("expected CPA capsule expanded on /responses/compact passthrough, got: %s", compactBody)
	}
	if !bytes.Contains(compactBody, []byte("foreign-native-token")) {
		t.Fatalf("expected foreign compaction item preserved on /responses/compact passthrough, got: %s", compactBody)
	}
}

func TestOpenAICompatExecutorCompactionIncompleteSummaryReturns502(t *testing.T) {
	executor, auth, recorder := newOpenAICompatCompactionTestExecutor()
	defer recorder.Close()
	recorder.incomplete = true

	triggerPayload := openAICompatCompactionTriggerPayload()
	_, errExec := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: triggerPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: triggerPayload,
	})
	if errExec == nil {
		t.Fatal("expected Execute(compaction_trigger) to fail on incomplete summary, got nil")
	}
	assertStatusErr(t, errExec, http.StatusBadGateway)

	_, errStream := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   openAICompatCompactionTestModel,
		Payload: triggerPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		Stream:          true,
		OriginalRequest: triggerPayload,
	})
	if errStream == nil {
		t.Fatal("expected ExecuteStream(compaction_trigger) to fail on incomplete summary, got nil")
	}
	assertStatusErr(t, errStream, http.StatusBadGateway)
}

func TestOpenAICompatExecutorCountTokensExpandsCapsules(t *testing.T) {
	executor, auth, recorder := newOpenAICompatCompactionTestExecutor()
	defer recorder.Close()

	capsule, errSeal := helps.SealAntigravityCompaction("Summary retaining COBALT-731.", openAICompatCompactionTestModel)
	if errSeal != nil {
		t.Fatalf("SealAntigravityCompaction failed: %v", errSeal)
	}
	replayPayload := []byte(fmt.Sprintf(`{
		"model": "gpt-4o",
		"input": [
			{"type": "compaction", "encrypted_content": "foreign-opaque-token"},
			{"type": "compaction", "id": "cmp_ag_compact_1", "encrypted_content": %q},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "continue"}]}
		]
	}`, capsule))
	expandedPayload := []byte(`{
		"model": "gpt-4o",
		"input": [
			{"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "Context summary from previous turns:\nSummary retaining COBALT-731."}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "continue"}]}
		]
	}`)

	replayResp, errReplay := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-4o",
		Payload: replayPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: replayPayload,
	})
	if errReplay != nil {
		t.Fatalf("CountTokens(replay) error = %v", errReplay)
	}
	expandedResp, errExpanded := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-4o",
		Payload: expandedPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	})
	if errExpanded != nil {
		t.Fatalf("CountTokens(expanded) error = %v", errExpanded)
	}

	replayCount := gjson.GetBytes(replayResp.Payload, "usage.total_tokens").Int()
	if replayCount <= 0 {
		t.Fatalf("CountTokens(replay) total_tokens = %d, want > 0; payload=%s", replayCount, replayResp.Payload)
	}
	if expandedCount := gjson.GetBytes(expandedResp.Payload, "usage.total_tokens").Int(); expandedCount != replayCount {
		t.Fatalf("CountTokens(replay) = %d, want %d (expanded developer context); replay=%s expanded=%s", replayCount, expandedCount, replayResp.Payload, expandedResp.Payload)
	}
}
