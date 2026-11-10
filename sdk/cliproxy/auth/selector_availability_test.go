package auth

import (
	"net/http"
	"testing"
	"time"
)

// TestIsAuthAvailableForModel pins the availability predicate the responses
// websocket credential check delegates to.
func TestIsAuthAvailableForModel(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	cooling := &Auth{
		ID:       "cooling",
		Provider: "codex",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {Status: StatusActive, Unavailable: true, NextRetryAfter: now.Add(5 * time.Minute)},
		},
	}
	expired := &Auth{
		ID:       "expired",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token": "opaque-token",
			"expires_at":   now.Add(-time.Hour).Format(time.RFC3339),
		},
	}
	unauthorized := &Auth{
		ID:          "unauthorized",
		Provider:    "codex",
		Status:      StatusError,
		Unavailable: true,
		LastError:   &Error{Code: "unauthorized", Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
	}
	disabled := &Auth{ID: "disabled", Provider: "codex", Status: StatusDisabled}
	healthy := &Auth{ID: "healthy", Provider: "codex", Status: StatusActive}
	healthyWithState := &Auth{
		ID:       "healthy-state",
		Provider: "codex",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {Status: StatusActive},
		},
	}

	for _, tc := range []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "nil", auth: nil},
		{name: "cooling model", auth: cooling},
		{name: "expired access token", auth: expired},
		{name: "unauthorized failure", auth: unauthorized},
		{name: "disabled", auth: disabled},
		{name: "healthy", auth: healthy, want: true},
		{name: "healthy with model state", auth: healthyWithState, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAuthAvailableForModel(tc.auth, "gpt-5.4", now); got != tc.want {
				t.Fatalf("IsAuthAvailableForModel() = %v, want %v", got, tc.want)
			}
		})
	}
}
