package openai

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	coreexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdkconfig "github.com/therealtinhtute/llmhub/sdk/config"
	"github.com/tidwall/gjson"
)

func performImagesEndpointRequest(t *testing.T, endpointPath string, contentType string, body io.Reader, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(endpointPath, handler)

	req := httptest.NewRequest(http.MethodPost, endpointPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func assertUnsupportedImagesModelResponse(t *testing.T, resp *httptest.ResponseRecorder, model string) {
	t.Helper()

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}

	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	expectedMessage := "Model " + model + " is not supported on " + imagesGenerationsPath + " or " + imagesEditsPath + ". Use " + gptImage15Model + ", " + defaultImagesToolModel + ", " + gptImage25FlareModel + ", " + gptImage25SunburstModel + ", " + gptImage25Model + ", " + defaultXAIImagesModel + ", " + xaiImagesQualityModel + ", " + xaiImages20Model + ", or a configured openai-compatibility image model."
	if message != expectedMessage {
		t.Fatalf("error message = %q, want %q", message, expectedMessage)
	}
	if errorType := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); errorType != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", errorType)
	}
}

func TestImagesModelValidationAllowsGPTImage2AndXAIModels(t *testing.T) {
	for _, model := range []string{
		"gpt-image-1.5",
		"codex/gpt-image-1.5",
		"gpt-image-2",
		"codex/gpt-image-2",
		"gpt-image-2.5-flare",
		"codex/gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		"codex/gpt-image-2.5-sunburst",
		"gpt-image-2.5",
		"codex/gpt-image-2.5",
		"grok-imagine-image",
		"xai/grok-imagine-image",
		"grok-imagine-image-quality",
		"xai/grok-imagine-image-quality",
		"grok-imagine-image-2.0",
		"xai/grok-imagine-image-2.0",
	} {
		if !isSupportedImagesModel(model) {
			t.Fatalf("expected %s to be supported", model)
		}
	}
	if isSupportedImagesModel("gpt-5.4-mini") {
		t.Fatal("expected gpt-5.4-mini to be rejected")
	}
	if isSupportedImagesModel("codex/grok-imagine-image") {
		t.Fatal("expected codex/grok-imagine-image to be rejected")
	}
}

func TestImagesModelValidationAllowsOpenAICompatImageModels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-openai-compat-image-model-validation"
	modelRegistry.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{
		{ID: "compat-image-model", Object: "model", OwnedBy: "compat", Type: registry.OpenAIImageModelType},
		{ID: "compat-chat-model", Object: "model", OwnedBy: "compat", Type: "openai-compatibility"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	if !isSupportedImagesModel("compat-image-model") {
		t.Fatal("expected configured openai-compatibility image model to be supported")
	}
	if isSupportedImagesModel("compat-chat-model") {
		t.Fatal("expected non-image openai-compatibility model to be rejected")
	}
}

func TestBuildXAIImagesGenerationsRequest(t *testing.T) {
	rawJSON := []byte(`{"model":"xai/grok-imagine-image-quality","prompt":"abstract art","aspect_ratio":"landscape","resolution":"2k","n":2,"response_format":"url"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "xai/grok-imagine-image-quality", "url")

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image-quality" {
		t.Fatalf("model = %q, want grok-imagine-image-quality", got)
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "abstract art" {
		t.Fatalf("prompt = %q, want abstract art", got)
	}
	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "16:9" {
		t.Fatalf("aspect_ratio = %q, want 16:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
	if got := gjson.GetBytes(req, "response_format").String(); got != "url" {
		t.Fatalf("response_format = %q, want url", got)
	}
	if got := gjson.GetBytes(req, "n").Int(); got != 2 {
		t.Fatalf("n = %d, want 2", got)
	}
}

func TestBuildXAIImagesGenerationsRequestNineByTwenty(t *testing.T) {
	rawJSON := []byte(`{"model":"grok-imagine-image-quality","prompt":"kitten","aspect_ratio":"9:20","resolution":"2k","n":1,"response_format":"b64_json"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "grok-imagine-image-quality", "b64_json")

	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "9:20" {
		t.Fatalf("aspect_ratio = %q, want 9:20", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
}

func TestBuildXAIImagesGenerationsRequestNineByTwentyFromSize(t *testing.T) {
	rawJSON := []byte(`{"model":"grok-imagine-image-quality","prompt":"kitten","size":"9:20","resolution":"2k"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "grok-imagine-image-quality", "b64_json")

	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "9:20" {
		t.Fatalf("aspect_ratio from size = %q, want 9:20", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
}

func TestBuildXAIImagesGenerationsRequestTwentyByNine(t *testing.T) {
	rawJSON := []byte(`{"model":"grok-imagine-image-quality","prompt":"kitten","aspect_ratio":"20:9","resolution":"2k","n":1,"response_format":"b64_json"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "grok-imagine-image-quality", "b64_json")

	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "20:9" {
		t.Fatalf("aspect_ratio = %q, want 20:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
}

func TestBuildXAIImagesGenerationsRequestTwentyByNineFromSize(t *testing.T) {
	rawJSON := []byte(`{"model":"grok-imagine-image-quality","prompt":"kitten","size":"20:9","resolution":"2k"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "grok-imagine-image-quality", "b64_json")

	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "20:9" {
		t.Fatalf("aspect_ratio from size = %q, want 20:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
}

func TestXAIImagesAspectRatioNineByTwenty(t *testing.T) {
	if got := xaiImagesAspectRatio("9:20", "1:1"); got != "9:20" {
		t.Fatalf("xaiImagesAspectRatio(9:20) = %q, want 9:20", got)
	}
	if got := xaiImagesAspectRatio("20:9", "1:1"); got != "20:9" {
		t.Fatalf("xaiImagesAspectRatio(20:9) = %q, want 20:9", got)
	}
	if got := xaiImagesAspectRatio("9:21", "1:1"); got != "1:1" {
		t.Fatalf("unknown ratio should keep fallback, got %q", got)
	}
	if got := xaiImagesAspectRatioFromSize("9:20", ""); got != "9:20" {
		t.Fatalf("xaiImagesAspectRatioFromSize(9:20) = %q, want 9:20", got)
	}
	if got := xaiImagesAspectRatioFromSize("20:9", ""); got != "20:9" {
		t.Fatalf("xaiImagesAspectRatioFromSize(20:9) = %q, want 20:9", got)
	}
}

func TestBuildXAIImagesEditRequest(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"data:image/png;base64,AA==", "https://example.com/image.png"}, "b64_json", "3:2", "1k", 0)

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image" {
		t.Fatalf("model = %q, want grok-imagine-image", got)
	}
	if got := gjson.GetBytes(req, "images.0.type").String(); got != "image_url" {
		t.Fatalf("images.0.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "images.0.url").String(); got != "data:image/png;base64,AA==" {
		t.Fatalf("images.0.url = %q", got)
	}
	if got := gjson.GetBytes(req, "images.1.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("images.1.url = %q", got)
	}
	if gjson.GetBytes(req, "image").Exists() {
		t.Fatalf("multiple image edits must use images array: %s", string(req))
	}
}

func TestBuildXAIImagesEditRequestSingleImage(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"https://example.com/image.png"}, "url", "", "", 0)

	if got := gjson.GetBytes(req, "image.type").String(); got != "image_url" {
		t.Fatalf("image.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "image.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("image.url = %q", got)
	}
	if gjson.GetBytes(req, "images").Exists() {
		t.Fatalf("single image edit must use image object: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestPreservesStreamForStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":false}`), "upstream-image", true)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if !gjson.GetBytes(req, "stream").Bool() {
		t.Fatalf("stream flag missing: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestDropsStreamForNonStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":true}`), "upstream-image", false)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if gjson.GetBytes(req, "stream").Exists() {
		t.Fatalf("stream flag should be removed from non-streaming request: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesMultipartRequestPreservesStreamAndFileContentType(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "compat-image"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatalf("write stream field: %v", errWrite)
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

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read source form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove source form files: %v", errRemove)
		}
	}()

	out, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, "upstream-image", true)
	if errBuild != nil {
		t.Fatalf("buildOpenAICompatImagesMultipartRequest error: %v", errBuild)
	}
	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil {
		t.Fatalf("parse content type: %v", errParse)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	rewrittenReader := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	rewrittenForm, errRead := rewrittenReader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read rewritten form: %v", errRead)
	}
	defer func() {
		if errRemove := rewrittenForm.RemoveAll(); errRemove != nil {
			t.Fatalf("remove rewritten form files: %v", errRemove)
		}
	}()
	if got := rewrittenForm.Value["model"]; len(got) != 1 || got[0] != "upstream-image" {
		t.Fatalf("model values = %#v, want upstream-image", got)
	}
	if got := rewrittenForm.Value["stream"]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("stream values = %#v, want true", got)
	}
	if got := rewrittenForm.Value["prompt"]; len(got) != 1 || got[0] != "edit" {
		t.Fatalf("prompt values = %#v, want edit", got)
	}
	if got := rewrittenForm.File["image"]; len(got) != 1 || got[0].Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image headers = %#v, want image/png", got)
	}
}

func TestBuildImagesAPIResponseFromXAI(t *testing.T) {
	payload := []byte(`{"created":123,"data":[{"b64_json":"AA==","revised_prompt":"refined","mime_type":"image/png"}],"usage":{"total_tokens":0}}`)

	out, err := buildImagesAPIResponseFromXAI(payload, "b64_json")
	if err != nil {
		t.Fatalf("buildImagesAPIResponseFromXAI() error = %v", err)
	}

	if got := gjson.GetBytes(out, "created").Int(); got != 123 {
		t.Fatalf("created = %d, want 123", got)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "AA==" {
		t.Fatalf("data.0.b64_json = %q, want AA==", got)
	}
	if got := gjson.GetBytes(out, "data.0.revised_prompt").String(); got != "refined" {
		t.Fatalf("data.0.revised_prompt = %q, want refined", got)
	}
	if !gjson.GetBytes(out, "usage").Exists() {
		t.Fatalf("usage missing: %s", string(out))
	}
}

func TestImagesGenerationsRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsJSONRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsMultipartRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-5.4-mini"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := writer.WriteField("prompt", "edit this"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesGenerations_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesGenerations_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

// xaiImagesCaptureCall records one executor invocation: which credential the
// auth manager routed to, the requested model carried in the request metadata,
// and the canonical model sent upstream.
type xaiImagesCaptureCall struct {
	authID         string
	requestedModel string
	payloadModel   string
}

type xaiImagesCaptureExecutor struct {
	mu    sync.Mutex
	calls []xaiImagesCaptureCall
}

func (e *xaiImagesCaptureExecutor) Identifier() string { return "xai" }

func (e *xaiImagesCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	call := xaiImagesCaptureCall{payloadModel: strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())}
	if auth != nil {
		call.authID = auth.ID
	}
	if opts.Metadata != nil {
		if v, ok := opts.Metadata[coreexecutor.RequestedModelMetadataKey].(string); ok {
			call.requestedModel = v
		}
	}
	e.mu.Lock()
	e.calls = append(e.calls, call)
	e.mu.Unlock()
	return coreexecutor.Response{
		Payload: []byte(`{"created":123,"data":[{"b64_json":"aW1hZ2U=","revised_prompt":"ok","mime_type":"image/png"}]}`),
	}, nil
}

func (e *xaiImagesCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "ExecuteStream not implemented"}
}

func (e *xaiImagesCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *xaiImagesCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *xaiImagesCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

func (e *xaiImagesCaptureExecutor) Calls() []xaiImagesCaptureCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]xaiImagesCaptureCall, len(e.calls))
	copy(out, e.calls)
	return out
}

// newXAIImagesRoutingTestHandler wires one xAI credential whose registered
// models are the given IDs, so a test can drive routing through the real auth
// manager instead of asserting on a helper.
func newXAIImagesRoutingTestHandler(t *testing.T, forceModelPrefix bool, authID string, prefix string, modelIDs []string) (*OpenAIAPIHandler, *xaiImagesCaptureExecutor) {
	t.Helper()

	executor := &xaiImagesCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "xai",
		Prefix:   prefix,
		Status:   coreauth.StatusActive,
	}); errRegister != nil {
		t.Fatalf("manager.Register: %v", errRegister)
	}

	models := make([]*registry.ModelInfo, 0, len(modelIDs))
	for _, id := range modelIDs {
		models = append(models, &registry.ModelInfo{ID: id, Object: "model", OwnedBy: "xai", Type: "xai"})
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "xai", models)
	manager.RefreshSchedulerEntry(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	return NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{ForceModelPrefix: forceModelPrefix}, manager)), executor
}

func TestXAIImagesRoutingPreservesCredentialPrefix(t *testing.T) {
	handler, executor := newXAIImagesRoutingTestHandler(t, true, "xai-prefixed-auth", "xai", []string{"xai/" + defaultXAIImagesModel})
	requested := "xai/" + defaultXAIImagesModel

	// assertRouted proves the credentials prefix survived routing while the
	// upstream payload still carries the canonical model.
	assertRouted := func(t *testing.T, before int) {
		t.Helper()
		calls := executor.Calls()
		if len(calls) != before+1 {
			t.Fatalf("executor calls = %d, want %d", len(calls), before+1)
		}
		call := calls[len(calls)-1]
		if call.authID != "xai-prefixed-auth" {
			t.Fatalf("authID = %q, want xai-prefixed-auth", call.authID)
		}
		if call.requestedModel != requested {
			t.Fatalf("requested model = %q, want %q", call.requestedModel, requested)
		}
		if call.payloadModel != defaultXAIImagesModel {
			t.Fatalf("upstream payload model = %q, want %s", call.payloadModel, defaultXAIImagesModel)
		}
	}

	t.Run("generations", func(t *testing.T) {
		before := len(executor.Calls())
		body := strings.NewReader(`{"model":"` + requested + `","prompt":"a red square","response_format":"b64_json"}`)
		resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
		}
		if got := gjson.GetBytes(resp.Body.Bytes(), "data.0.b64_json").String(); got != "aW1hZ2U=" {
			t.Fatalf("data.0.b64_json = %q, want aW1hZ2U=", got)
		}
		assertRouted(t, before)
	})

	t.Run("generations stream", func(t *testing.T) {
		before := len(executor.Calls())
		body := strings.NewReader(`{"model":"` + requested + `","prompt":"a red square","response_format":"b64_json","stream":true}`)
		resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), "image_generation.completed") {
			t.Fatalf("stream body = %q, want an image_generation.completed event", resp.Body.String())
		}
		assertRouted(t, before)
	})

	t.Run("edits json", func(t *testing.T) {
		before := len(executor.Calls())
		body := strings.NewReader(`{"model":"` + requested + `","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)
		resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
		}
		assertRouted(t, before)
	})

	t.Run("edits multipart", func(t *testing.T) {
		before := len(executor.Calls())
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("model", requested); err != nil {
			t.Fatalf("write model field: %v", err)
		}
		if err := writer.WriteField("prompt", "edit this"); err != nil {
			t.Fatalf("write prompt field: %v", err)
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.png"))
		header.Set("Content-Type", "image/png")
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create image part: %v", err)
		}
		if _, err := part.Write([]byte("png-bytes")); err != nil {
			t.Fatalf("write image part: %v", err)
		}
		if errClose := writer.Close(); errClose != nil {
			t.Fatalf("close multipart writer: %v", errClose)
		}

		resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
		}
		assertRouted(t, before)
	})
}

func TestXAIImagesRoutingFallsBackForVendorAliasUnderForceModelPrefix(t *testing.T) {
	handler, executor := newXAIImagesRoutingTestHandler(t, true, "xai-unprefixed-auth", "", []string{defaultXAIImagesModel})

	body := strings.NewReader(`{"model":"xai/grok-imagine-image","prompt":"a red square","response_format":"b64_json"}`)
	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(calls))
	}
	if calls[0].authID != "xai-unprefixed-auth" || calls[0].payloadModel != defaultXAIImagesModel {
		t.Fatalf("call = %+v, want authID=xai-unprefixed-auth payloadModel=%s", calls[0], defaultXAIImagesModel)
	}
}

func TestXAIImagesRoutingAcceptsCustomCredentialPrefix(t *testing.T) {
	handler, executor := newXAIImagesRoutingTestHandler(t, true, "xai-custom-auth", "my-xai", []string{"my-xai/" + defaultXAIImagesModel})

	body := strings.NewReader(`{"model":"my-xai/grok-imagine-image","prompt":"a red square","response_format":"b64_json"}`)
	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(calls))
	}
	if calls[0].authID != "xai-custom-auth" || calls[0].payloadModel != defaultXAIImagesModel {
		t.Fatalf("call = %+v, want authID=xai-custom-auth payloadModel=%s", calls[0], defaultXAIImagesModel)
	}
}

func TestXAIImagesRoutingKeepsUnprefixedModelAwayFromPrefixedCredential(t *testing.T) {
	handler, executor := newXAIImagesRoutingTestHandler(t, true, "xai-prefixed-only-auth", "xai", []string{"xai/" + defaultXAIImagesModel})

	body := strings.NewReader(`{"model":"grok-imagine-image","prompt":"a red square"}`)
	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)
	if resp.Code == http.StatusOK {
		t.Fatalf("status = %d, want a routing failure: %s", resp.Code, resp.Body.String())
	}
	if calls := executor.Calls(); len(calls) != 0 {
		t.Fatalf("executor calls = %d, want 0", len(calls))
	}
}
