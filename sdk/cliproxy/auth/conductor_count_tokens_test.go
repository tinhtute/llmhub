package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/registry"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
)

type countTokensErrorExecutor struct {
	err error
}

func (e *countTokensErrorExecutor) Identifier() string { return "claude" }

func (e *countTokensErrorExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *countTokensErrorExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *countTokensErrorExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *countTokensErrorExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.err
}

func (e *countTokensErrorExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type countTokensStatusError struct {
	status  int
	message string
}

func (e *countTokensStatusError) Error() string   { return e.message }
func (e *countTokensStatusError) StatusCode() int { return e.status }

// countTokensCodeError carries a structured error code without an HTTP status,
// matching a plugin or auth error that only exposes ErrorCode().
type countTokensCodeError struct {
	code    string
	message string
}

func (e *countTokensCodeError) Error() string     { return e.message }
func (e *countTokensCodeError) ErrorCode() string { return e.code }

func TestIsCountTokensEndpointNotFoundError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "generic 404", err: &Error{HTTPStatus: http.StatusNotFound}, want: true},
		{name: "generic 501", err: &Error{HTTPStatus: http.StatusNotImplemented}, want: true},
		{name: "typed unsupported code", err: &countTokensCodeError{code: "count_tokens_unsupported"}, want: true},
		{name: "501 with model_not_found", err: &Error{HTTPStatus: http.StatusNotImplemented, Code: "model_not_found"}, want: false},
		{name: "401 unauthorized", err: &Error{HTTPStatus: http.StatusUnauthorized}, want: false},
		{name: "400 credit balance", err: &Error{HTTPStatus: http.StatusBadRequest, Message: "Your credit balance is too low"}, want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCountTokensEndpointNotFoundError(tc.err, "claude-sonnet-4-5"); got != tc.want {
				t.Fatalf("isCountTokensEndpointNotFoundError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManager_CountTokensErrorClassification(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	testCases := []struct {
		name               string
		err                error
		availabilityChange bool
	}{
		{
			name: "generic empty endpoint 404",
			err:  &Error{HTTPStatus: http.StatusNotFound},
		},
		{
			name: "generic unsupported endpoint 501",
			err:  &Error{HTTPStatus: http.StatusNotImplemented},
		},
		{
			name: "typed unsupported capability code",
			err: &countTokensCodeError{
				code:    "count_tokens_unsupported",
				message: "unsupported capability",
			},
		},
		{
			name: "generic unsupported endpoint 404",
			err: &Error{
				HTTPStatus: http.StatusNotFound,
				Message:    "Cannot POST /v1/messages/count_tokens",
			},
		},
		{
			name: "explicit top-level model_not_found",
			err: &Error{
				Code:       "model_not_found",
				HTTPStatus: http.StatusNotFound,
				Message:    "Not Found",
			},
			availabilityChange: true,
		},
		{
			name: "explicit nested model_not_found",
			err: &Error{
				HTTPStatus: http.StatusNotFound,
				Message:    `{"error":{"code":"model_not_found","message":"Not Found"}}`,
			},
			availabilityChange: true,
		},
		{
			name: "wrapped Error nested model_not_found",
			err: fmt.Errorf("count tokens failed: %w", &Error{
				HTTPStatus: http.StatusNotFound,
				Message:    `{"error":{"code":"model_not_found","message":"Not Found"}}`,
			}),
			availabilityChange: true,
		},
		{
			name: "wrapped status error nested model_not_found",
			err: fmt.Errorf("count tokens failed: %w", &countTokensStatusError{
				status:  http.StatusNotFound,
				message: `{"error":{"type":"model_not_found","message":"Not Found"}}`,
			}),
			availabilityChange: true,
		},
		{
			name: "joined status error nested model_not_found",
			err: errors.Join(
				errors.New("count tokens failed"),
				&countTokensStatusError{
					status:  http.StatusNotFound,
					message: `{"error":{"code":"model_not_found","message":"Not Found"}}`,
				},
			),
			availabilityChange: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			suffix := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
			model := "count-tokens-" + suffix
			authID := "count-tokens-auth-" + suffix

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })

			manager := NewManager(nil, nil, nil)
			manager.RegisterExecutor(&countTokensErrorExecutor{err: testCase.err})
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "claude"}); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			if _, errCount := manager.ExecuteCount(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errCount == nil {
				t.Fatal("expected CountTokens error")
			}

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatal("expected auth to remain registered")
			}
			if updated.Failed != 1 {
				t.Fatalf("failed count = %d, want 1", updated.Failed)
			}

			state := updated.ModelStates[model]
			if testCase.availabilityChange {
				if state == nil || !state.Unavailable {
					t.Fatalf("model state = %#v, want unavailable", state)
				}
				remaining := time.Until(state.NextRetryAfter)
				if remaining < 11*time.Hour || remaining > 12*time.Hour {
					t.Fatalf("model cooldown = %v, want about 12h", remaining)
				}
				if count := reg.GetModelCount(model); count != 0 {
					t.Fatalf("available model count = %d, want 0", count)
				}
				picked, errPick := manager.scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
				if errPick == nil {
					t.Fatal("scheduler error = nil, want unavailable model")
				}
				if picked != nil {
					t.Fatalf("scheduler picked auth = %#v, want nil", picked)
				}
				return
			}

			if state != nil {
				t.Fatalf("generic CountTokens endpoint error created model state: %#v", state)
			}
			if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
				t.Fatalf("generic CountTokens endpoint error changed auth availability: %#v", updated)
			}
			if count := reg.GetModelCount(model); count <= 0 {
				t.Fatalf("available model count = %d, want positive", count)
			}
			picked, errPick := manager.scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("scheduler error = %v, want nil", errPick)
			}
			if picked == nil || picked.ID != authID {
				t.Fatalf("scheduler picked auth = %#v, want %q", picked, authID)
			}
		})
	}
}
