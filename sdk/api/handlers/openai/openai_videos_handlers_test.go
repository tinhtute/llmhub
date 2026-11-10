package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	coreexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdkconfig "github.com/therealtinhtute/llmhub/sdk/config"
	"github.com/tidwall/gjson"
)

func performVideosEndpointRequest(t *testing.T, method string, endpointPath string, contentType string, body io.Reader, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	switch method {
	case http.MethodGet:
		router.GET(endpointPath, handler)
	default:
		router.POST(endpointPath, handler)
	}

	req := httptest.NewRequest(method, endpointPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func TestVideosModelValidationAllowsXAIVideoModel(t *testing.T) {
	for _, model := range []string{"grok-imagine-video", "xai/grok-imagine-video", "x-ai/grok-imagine-video", "grok/grok-imagine-video", "grok-imagine-video-1.5-preview", "grok-imagine-video-1.5-lite", "xai/grok-imagine-video-1.5-lite", "x-ai/grok-imagine-video-1.5-lite", "grok/grok-imagine-video-1.5-lite", "sora-2", "sora-2-pro"} {
		if !isSupportedVideosModel(model) {
			t.Fatalf("expected %s to be supported", model)
		}
	}
	if isSupportedVideosModel("codex/grok-imagine-video") {
		t.Fatal("expected codex/grok-imagine-video to be rejected")
	}
	if isSupportedVideosModel("codex/grok-imagine-video-1.5-lite") {
		t.Fatal("expected codex/grok-imagine-video-1.5-lite to be rejected")
	}
}

func TestBuildXAIVideosCreateRequest(t *testing.T) {
	rawJSON := []byte(`{"model":"xai/grok-imagine-video","prompt":"a cat playing piano","seconds":"8","size":"1280x720","input_reference":{"image_url":"https://example.com/cat.png"}}`)

	req, meta, err := buildXAIVideosCreateRequest(rawJSON, "xai/grok-imagine-video")
	if err != nil {
		t.Fatalf("buildXAIVideosCreateRequest() error = %v", err)
	}

	if got := gjson.GetBytes(req, "model").String(); got != defaultXAIVideosModel {
		t.Fatalf("model = %q, want %s", got, defaultXAIVideosModel)
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "a cat playing piano" {
		t.Fatalf("prompt = %q", got)
	}
	if got := gjson.GetBytes(req, "duration").Int(); got != 8 {
		t.Fatalf("duration = %d, want 8", got)
	}
	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "16:9" {
		t.Fatalf("aspect_ratio = %q, want 16:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "720p" {
		t.Fatalf("resolution = %q, want 720p", got)
	}
	if got := gjson.GetBytes(req, "image.url").String(); got != "https://example.com/cat.png" {
		t.Fatalf("image.url = %q", got)
	}
	if meta.Seconds != "8" || meta.Size != "1280x720" || meta.Prompt != "a cat playing piano" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
}

func TestBuildXAIVideosCreateRequestPreservesResolution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolution string
		want       string
	}{
		{name: "480p", resolution: "480p", want: "480p"},
		{name: "720p", resolution: "720p", want: "720p"},
		{name: "1080p", resolution: "1080p", want: "1080p"},
		{name: "uppercase", resolution: "1080P", want: "1080p"},
		{name: "default", want: "720p"},
		{name: "unknown", resolution: "4k", want: "720p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"grok-imagine-video-1.5-lite","prompt":"make a video"`
			if tc.resolution != "" {
				body += `,"resolution":` + strconv.Quote(tc.resolution)
			}
			body += `}`
			req, _, err := buildXAIVideosCreateRequest([]byte(body), "grok-imagine-video-1.5-lite")
			if err != nil {
				t.Fatalf("buildXAIVideosCreateRequest() error = %v", err)
			}
			if got := gjson.GetBytes(req, "resolution").String(); got != tc.want {
				t.Fatalf("resolution = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildXAIVideosCreateRequestAllowsCustomSeconds(t *testing.T) {
	rawJSON := []byte(`{"model":"grok-imagine-video","prompt":"a cat playing piano","seconds":"6"}`)

	req, meta, err := buildXAIVideosCreateRequest(rawJSON, "grok-imagine-video")
	if err != nil {
		t.Fatalf("buildXAIVideosCreateRequest() error = %v", err)
	}

	if got := gjson.GetBytes(req, "duration").Int(); got != 6 {
		t.Fatalf("duration = %d, want 6", got)
	}
	if meta.Seconds != "6" {
		t.Fatalf("meta seconds = %q, want 6", meta.Seconds)
	}
}

func TestBuildXAIVideosCreateRequestPreservesMultiReferenceDuration(t *testing.T) {
	rawJSON := []byte(`{"model":"grok-imagine-video","prompt":"animate","seconds":"15","reference_images":["https://example.com/a.png","https://example.com/b.png"]}`)

	req, meta, err := buildXAIVideosCreateRequest(rawJSON, "grok-imagine-video")
	if err != nil {
		t.Fatalf("buildXAIVideosCreateRequest() error = %v", err)
	}
	if got := gjson.GetBytes(req, "duration").Int(); got != 15 {
		t.Fatalf("duration = %d, want 15", got)
	}
	if meta.Seconds != "15" {
		t.Fatalf("meta seconds = %q, want 15", meta.Seconds)
	}
}

func TestBuildXAIVideosCreateRequestRejectsFileIDReference(t *testing.T) {
	rawJSON := []byte(`{"prompt":"animate","input_reference":{"file_id":"file_123"}}`)

	_, _, err := buildXAIVideosCreateRequest(rawJSON, defaultXAIVideosModel)
	if err == nil || !strings.Contains(err.Error(), "input_reference.file_id is not supported") {
		t.Fatalf("error = %v, want unsupported file_id error", err)
	}
}

func TestBuildVideosCreateAPIResponseFromXAI(t *testing.T) {
	meta := xaiVideoCreateMetadata{
		Model:     defaultXAIVideosModel,
		Prompt:    "animate",
		Seconds:   "4",
		Size:      "720x1280",
		CreatedAt: 123,
	}
	out, err := buildVideosCreateAPIResponseFromXAI([]byte(`{"request_id":"vid_123"}`), meta)
	if err != nil {
		t.Fatalf("buildVideosCreateAPIResponseFromXAI() error = %v", err)
	}

	if got := gjson.GetBytes(out, "id").String(); got != "vid_123" {
		t.Fatalf("id = %q, want vid_123", got)
	}
	if got := gjson.GetBytes(out, "object").String(); got != "video" {
		t.Fatalf("object = %q, want video", got)
	}
	if got := gjson.GetBytes(out, "status").String(); got != "queued" {
		t.Fatalf("status = %q, want queued", got)
	}
	if got := gjson.GetBytes(out, "created_at").Int(); got != 123 {
		t.Fatalf("created_at = %d, want 123", got)
	}
}

func TestBuildVideosRetrieveAPIResponseFromXAI(t *testing.T) {
	payload := []byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4","duration":6,"respect_moderation":true},"model":"grok-imagine-video","usage":{"cost_in_usd_ticks":500000000},"progress":100}`)

	out, err := buildVideosRetrieveAPIResponseFromXAI("vid_123", payload, defaultXAIVideosModel)
	if err != nil {
		t.Fatalf("buildVideosRetrieveAPIResponseFromXAI() error = %v", err)
	}

	if got := gjson.GetBytes(out, "id").String(); got != "vid_123" {
		t.Fatalf("id = %q, want vid_123", got)
	}
	if got := gjson.GetBytes(out, "status").String(); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
	if got := gjson.GetBytes(out, "seconds").String(); got != "6" {
		t.Fatalf("seconds = %q, want 6", got)
	}
	if got := gjson.GetBytes(out, "video_url").String(); got != "https://vidgen.x.ai/video.mp4" {
		t.Fatalf("video_url = %q", got)
	}
}

func TestVideosCreateRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"not-a-video-model","prompt":"make a video"}`)

	resp := performVideosEndpointRequest(t, http.MethodPost, videosPath, "application/json", body, handler.VideosCreate)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	expectedMessage := "Model not-a-video-model is not supported on " + videosPath + ". Use " + defaultOpenAIVideosModel + "."
	if message != expectedMessage {
		t.Fatalf("error message = %q, want %q", message, expectedMessage)
	}
}

func TestXAIVideosNativeRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"sora-2","prompt":"make a video"}`)

	resp := performVideosEndpointRequest(t, http.MethodPost, xaiVideosGenerationsAPI, "application/json", body, handler.XAIVideosGenerations)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	expectedMessage := "Model sora-2 is not supported on " + xaiVideosGenerationsAPI + ", " + xaiVideosEditsAPI + ", or " + xaiVideosExtensionsAPI + ". Use " + defaultXAIVideosModel + "."
	if message != expectedMessage {
		t.Fatalf("error message = %q, want %q", message, expectedMessage)
	}
}

func TestXAIVideosNativeRejectsInvalidJSON(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":`)

	resp := performVideosEndpointRequest(t, http.MethodPost, xaiVideosEditsAPI, "application/json", body, handler.XAIVideosEdits)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", got)
	}
}

func TestVideosCreateFormRequest(t *testing.T) {
	rawJSON, err := videosCreateRequestFromFormContext("model=grok-imagine-video&prompt=make+a+video&seconds=4&size=720x1280&input_reference%5Bimage_url%5D=https%3A%2F%2Fexample.com%2Fa.png")
	if err != nil {
		t.Fatalf("videosCreateRequestFromFormContext() error = %v", err)
	}

	if got := gjson.GetBytes(rawJSON, "input_reference.image_url").String(); got != "https://example.com/a.png" {
		t.Fatalf("input_reference.image_url = %q", got)
	}
}

func TestVideoContentHTTPClientUsesBoundedTimeout(t *testing.T) {
	handler := &OpenAIAPIHandler{}

	client := handler.videoContentHTTPClient(nil)

	if client.Timeout != defaultVideoContentDownloadLimit {
		t.Fatalf("client timeout = %v, want %v", client.Timeout, defaultVideoContentDownloadLimit)
	}
}

func videosCreateRequestFromFormContext(body string) ([]byte, error) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	var rawJSON []byte
	var err error
	router.POST(videosPath, func(c *gin.Context) {
		rawJSON, err = videosCreateRequestFromForm(c)
	})
	req := httptest.NewRequest(http.MethodPost, videosPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return rawJSON, err
}

// videosCaptureCall records one executor invocation: which credential the auth
// manager routed to, the model the handler asked for, and the model carried in
// the upstream payload.
type videosCaptureCall struct {
	authID       string
	model        string
	payloadModel string
}

type videosCaptureExecutor struct {
	mu        sync.Mutex
	requestID string
	calls     []videosCaptureCall
}

func (e *videosCaptureExecutor) Identifier() string { return "xai" }

func (e *videosCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	call := videosCaptureCall{
		model:        req.Model,
		payloadModel: strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String()),
	}
	if auth != nil {
		call.authID = auth.ID
	}
	e.mu.Lock()
	e.calls = append(e.calls, call)
	e.mu.Unlock()
	return coreexecutor.Response{Payload: []byte(`{"request_id":` + strconv.Quote(e.requestID) + `,"status":"queued"}`)}, nil
}

func (e *videosCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "ExecuteStream not implemented"}
}

func (e *videosCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *videosCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *videosCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

func (e *videosCaptureExecutor) Calls() []videosCaptureCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]videosCaptureCall, len(e.calls))
	copy(out, e.calls)
	return out
}

// performVideosParamRequest drives a handler registered under a gin route with
// path parameters, which performVideosEndpointRequest cannot express.
func performVideosParamRequest(t *testing.T, method string, routePattern string, requestPath string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	switch method {
	case http.MethodGet:
		router.GET(routePattern, handler)
	default:
		router.POST(routePattern, handler)
	}
	req := httptest.NewRequest(method, requestPath, nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

// Lite is a distinct xAI video model, not an alias: every accepted prefix must
// canonicalize to Lite itself, the create payload must ask upstream for Lite,
// and a retrieve whose upstream payload omits "model" must report the bound
// Lite model instead of the sora-2 default.
func TestVideosLiteCreateRetrievePreservesModel(t *testing.T) {
	const liteModel = "grok-imagine-video-1.5-lite"
	for _, model := range []string{liteModel, "xai/" + liteModel, "x-ai/" + liteModel, "grok/" + liteModel} {
		t.Run(model, func(t *testing.T) {
			executor := &videosCaptureExecutor{requestID: "video-lite-" + strings.NewReplacer("/", "-", ".", "-").Replace(model)}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			auth := &coreauth.Auth{ID: "video-lite-auth", Provider: "xai", Status: coreauth.StatusActive}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("manager.Register(): %v", errRegister)
			}
			modelRegistry := registry.GetGlobalRegistry()
			modelRegistry.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: liteModel, Object: "model", OwnedBy: "xai", Type: "xai"}})
			manager.RefreshSchedulerEntry(auth.ID)
			t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })
			handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))

			body := strings.NewReader(`{"model":` + strconv.Quote(model) + `,"prompt":"make a video"}`)
			createResp := performVideosEndpointRequest(t, http.MethodPost, openAIVideosPath, "application/json", body, handler.VideosCreate)
			if createResp.Code != http.StatusOK {
				t.Fatalf("create status = %d, want %d: %s", createResp.Code, http.StatusOK, createResp.Body.String())
			}
			if got := gjson.GetBytes(createResp.Body.Bytes(), "model").String(); got != liteModel {
				t.Fatalf("create response model = %q, want %s", got, liteModel)
			}
			if got := gjson.GetBytes(createResp.Body.Bytes(), "id").String(); got != executor.requestID {
				t.Fatalf("create id = %q, want %q", got, executor.requestID)
			}

			calls := executor.Calls()
			if len(calls) != 1 {
				t.Fatalf("executor calls = %d, want 1", len(calls))
			}
			if calls[0].authID != auth.ID {
				t.Fatalf("create authID = %q, want %q", calls[0].authID, auth.ID)
			}
			if calls[0].model != liteModel {
				t.Fatalf("create model = %q, want %s", calls[0].model, liteModel)
			}
			if calls[0].payloadModel != liteModel {
				t.Fatalf("create payload model = %q, want %s", calls[0].payloadModel, liteModel)
			}

			retrieveResp := performVideosParamRequest(t, http.MethodGet, openAIVideosPath+"/:video_id", openAIVideosPath+"/"+executor.requestID, handler.VideosRetrieve)
			if retrieveResp.Code != http.StatusOK {
				t.Fatalf("retrieve status = %d, want %d: %s", retrieveResp.Code, http.StatusOK, retrieveResp.Body.String())
			}
			if got := gjson.GetBytes(retrieveResp.Body.Bytes(), "model").String(); got != liteModel {
				t.Fatalf("retrieve response model = %q, want %s", got, liteModel)
			}
			calls = executor.Calls()
			if len(calls) != 2 {
				t.Fatalf("executor calls after retrieve = %d, want 2", len(calls))
			}
			if calls[1].authID != auth.ID || calls[1].model != liteModel {
				t.Fatalf("retrieve call = %+v, want auth %q and model %s", calls[1], auth.ID, liteModel)
			}
		})
	}
}

func TestVideosLiteRejectsForeignProviderPrefix(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	for _, route := range []struct {
		path    string
		handler gin.HandlerFunc
	}{
		{openAIVideosPath, handler.VideosCreate},
		{xaiVideosGenerationsAPI, handler.XAIVideosGenerations},
		{xaiVideosEditsAPI, handler.XAIVideosEdits},
		{xaiVideosExtensionsAPI, handler.XAIVideosExtensions},
	} {
		t.Run(route.path, func(t *testing.T) {
			body := strings.NewReader(`{"model":"codex/grok-imagine-video-1.5-lite","prompt":"make a video"}`)
			resp := performVideosEndpointRequest(t, http.MethodPost, route.path, "application/json", body, route.handler)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
			}
			errorField := "error.type"
			if route.path == openAIVideosPath {
				errorField = "error.code"
			}
			if got := gjson.GetBytes(resp.Body.Bytes(), errorField).String(); got != "invalid_request_error" {
				t.Fatalf("%s = %q, want invalid_request_error", errorField, got)
			}
		})
	}
}
