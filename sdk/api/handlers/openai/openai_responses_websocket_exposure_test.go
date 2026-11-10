package openai

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/therealtinhtute/llmhub/internal/interfaces"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	sdkconfig "github.com/therealtinhtute/llmhub/sdk/config"
	"github.com/tidwall/gjson"
)

// websocketRetryHintError mirrors the upstream test error: it carries a retry
// deadline hint without exposing safe response headers.
type websocketRetryHintError struct {
	status int
	msg    string
	hint   *time.Duration
}

func (e websocketRetryHintError) Error() string { return e.msg }

func (e websocketRetryHintError) StatusCode() int { return e.status }

func (e websocketRetryHintError) RetryAfter() *time.Duration { return e.hint }

func TestShouldExposeResponsesUpstreamErrorForTerminalQuota(t *testing.T) {
	for _, tc := range []struct {
		name   string
		errMsg *interfaces.ErrorMessage
		want   bool
	}{
		{name: "nil", errMsg: nil},
		{name: "plain 429", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("rate limited")}, want: true},
		{name: "usage limit body at 500", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New(`{"error":{"type":"usage_limit_reached","message":"limit"}}`)}, want: true},
		{name: "model cooldown code at 500", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New(`{"error":{"code":"model_cooldown","message":"cooling"}}`)}, want: true},
		{name: "typed model cooldown", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: coreauth.NewModelCooldownError("gpt-5.4", "codex", time.Minute)}, want: true},
		{name: "request fault", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New("bad request")}, want: true},
		{name: "unauthorized", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusUnauthorized, Error: errors.New("unauthorized")}},
		{name: "forbidden", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusForbidden, Error: errors.New("forbidden")}},
		{name: "request timeout", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusRequestTimeout, Error: errors.New("timeout")}},
		{name: "plain 500", errMsg: &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("boom")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldExposeResponsesUpstreamError(tc.errMsg); got != tc.want {
				t.Fatalf("shouldExposeResponsesUpstreamError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildResponsesWebsocketErrorPayloadRewritesCooldownTo429(t *testing.T) {
	payload, errBuild := buildResponsesWebsocketErrorPayload(&interfaces.ErrorMessage{
		StatusCode: http.StatusInternalServerError,
		Error:      coreauth.NewModelCooldownError("gpt-5.4", "codex", 30*time.Second),
	})
	if errBuild != nil {
		t.Fatalf("buildResponsesWebsocketErrorPayload() error = %v", errBuild)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeError {
		t.Fatalf("type = %q, want %q: %s", got, wsEventTypeError, payload)
	}
	if got := int(gjson.GetBytes(payload, "status").Int()); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", got, http.StatusTooManyRequests, payload)
	}
	if got := gjson.GetBytes(payload, "error.code").String(); got != "model_cooldown" {
		t.Fatalf("error.code = %q, want model_cooldown: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, "headers.Retry-After").String(); got != "30" {
		t.Fatalf("headers.Retry-After = %q, want 30: %s", got, payload)
	}
}

func TestBuildResponsesWebsocketErrorPayloadMergesTrustedHeaders(t *testing.T) {
	busy := coreauth.NewHomeConcurrencyBusyError("busy", 5*time.Second)
	payload, errBuild := buildResponsesWebsocketErrorPayload(&interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      busy,
		Addon:      http.Header{"Retry-After": {"30"}, "X-Request-Id": {"req-1"}},
	})
	if errBuild != nil {
		t.Fatalf("buildResponsesWebsocketErrorPayload() error = %v", errBuild)
	}
	if got := gjson.GetBytes(payload, "headers.Retry-After").String(); got != "5" {
		t.Fatalf("headers.Retry-After = %q, want the trusted 5: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, "headers.X-Request-Id").String(); got != "req-1" {
		t.Fatalf("headers.X-Request-Id = %q, want req-1: %s", got, payload)
	}
}

func TestBuildResponsesWebsocketErrorPayloadEmitsTrustedHeadersWithoutAddon(t *testing.T) {
	busy := coreauth.NewHomeConcurrencyBusyError("busy", 5*time.Second)
	payload, errBuild := buildResponsesWebsocketErrorPayload(&interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      busy,
	})
	if errBuild != nil {
		t.Fatalf("buildResponsesWebsocketErrorPayload() error = %v", errBuild)
	}
	if got := gjson.GetBytes(payload, "headers.Retry-After").String(); got != "5" {
		t.Fatalf("headers.Retry-After = %q, want 5: %s", got, payload)
	}
}

func TestBuildResponsesWebsocketErrorPayloadSuppressesStaleRetryAfter(t *testing.T) {
	zero := time.Duration(0)
	hintErr := websocketRetryHintError{
		status: http.StatusTooManyRequests,
		msg:    `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`,
		hint:   &zero,
	}
	if !responsesWebsocketHasTrustedRetryDeadline(hintErr) {
		t.Fatal("expected a non-nil RetryAfter hint to count as a trusted deadline")
	}
	if got := coreauth.SafeResponseHeaders(hintErr).Get("Retry-After"); got != "" {
		t.Fatalf("expected empty SafeResponseHeaders Retry-After, got %q", got)
	}

	payload, errBuild := buildResponsesWebsocketErrorPayload(&interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      hintErr,
		Addon:      http.Header{"Retry-After": {"30"}, "X-Request-Id": {"req-1"}},
	})
	if errBuild != nil {
		t.Fatalf("buildResponsesWebsocketErrorPayload() error = %v", errBuild)
	}
	if got := gjson.GetBytes(payload, "headers.Retry-After").String(); got != "" {
		t.Fatalf("stale Addon Retry-After leaked into websocket error payload: %q (%s)", got, payload)
	}
	if got := gjson.GetBytes(payload, "headers.X-Request-Id").String(); got != "req-1" {
		t.Fatalf("headers.X-Request-Id = %q, want req-1: %s", got, payload)
	}
	if got := int(gjson.GetBytes(payload, "status").Int()); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", got, http.StatusTooManyRequests, payload)
	}
}

func TestResponsesWebsocketHasTrustedRetryDeadline(t *testing.T) {
	positive := 256 * time.Second
	zero := time.Duration(0)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil},
		{name: "plain error", err: errors.New("boom")},
		{name: "positive hint", err: websocketRetryHintError{hint: &positive}, want: true},
		{name: "zero hint", err: websocketRetryHintError{hint: &zero}, want: true},
		{name: "safe headers", err: coreauth.NewHomeConcurrencyBusyError("busy", 5*time.Second), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesWebsocketHasTrustedRetryDeadline(tc.err); got != tc.want {
				t.Fatalf("responsesWebsocketHasTrustedRetryDeadline() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestForwardResponsesWebsocketExposesTerminal429OnDuplexTurn pins that a
// terminal quota failure reaches the client as an error frame instead of a
// silent close once credential failover is exhausted.
func TestForwardResponsesWebsocketExposesTerminal429OnDuplexTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			serverErrCh <- errUpgrade
			return
		}
		defer func() {
			if errClose := conn.Close(); errClose != nil {
				serverErrCh <- errClose
			}
		}()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r
		h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

		data := make(chan []byte)
		errs := make(chan *interfaces.ErrorMessage, 1)
		errs <- &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      errors.New(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`),
		}
		close(errs)
		close(data)

		_, errMsg, errForward := h.forwardResponsesWebsocket(
			ctx,
			conn,
			func(...interface{}) {},
			data,
			errs,
			newInMemoryWebsocketTimelineLog(),
			"session-429",
			nil,
			nil,
			func() bool { return true },
			nil,
		)
		if !errors.Is(errForward, websocket.ErrCloseSent) {
			serverErrCh <- fmt.Errorf("forward error = %v, want ErrCloseSent", errForward)
			return
		}
		if errMsg == nil {
			serverErrCh <- errors.New("terminal 429 was not reported")
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("terminal 429 was hidden: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeError {
		t.Fatalf("type = %q, want %q: %s", got, wsEventTypeError, payload)
	}
	if got := int(gjson.GetBytes(payload, "status").Int()); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", got, http.StatusTooManyRequests, payload)
	}
	if got := gjson.GetBytes(payload, "error.type").String(); got != "usage_limit_reached" {
		t.Fatalf("error.type = %q, want usage_limit_reached: %s", got, payload)
	}
	if strings.Contains(string(payload), "response.incomplete") {
		t.Fatalf("terminal error fabricated a response.incomplete frame: %s", payload)
	}

	if errServer := <-serverErrCh; errServer != nil {
		t.Fatalf("server error: %v", errServer)
	}
}
