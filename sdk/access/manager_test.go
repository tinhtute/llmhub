package access

import (
	"context"
	"net/http"
	"testing"
)

type funcProvider struct {
	id   string
	auth func(context.Context, *http.Request) (*Result, *AuthError)
}

func (p funcProvider) Identifier() string { return p.id }

func (p funcProvider) Authenticate(ctx context.Context, r *http.Request) (*Result, *AuthError) {
	return p.auth(ctx, r)
}

// TestManagerAuthenticateDeniedErrorEndsProviderChainAndPreservesStatus covers
// the terminal-refusal contract: a denied provider ends the chain (a later
// provider must not authorize a principal the denier already refused) and the
// status it reported reaches the caller (upstream CLIProxyAPI commit b9bb031dcbb0).
func TestManagerAuthenticateDeniedErrorEndsProviderChainAndPreservesStatus(t *testing.T) {
	laterCalled := false
	manager := NewManager()
	manager.SetProviders([]Provider{
		funcProvider{
			id: "denier",
			auth: func(context.Context, *http.Request) (*Result, *AuthError) {
				return nil, NewDeniedError("quota exhausted", http.StatusTooManyRequests)
			},
		},
		funcProvider{
			id: "fallback",
			auth: func(context.Context, *http.Request) (*Result, *AuthError) {
				laterCalled = true
				return &Result{Provider: "fallback", Principal: "user-1"}, nil
			},
		},
	})

	req, errReq := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	result, authErr := manager.Authenticate(context.Background(), req)
	if result != nil {
		t.Fatalf("Authenticate() result = %#v, want nil", result)
	}
	if !IsAuthErrorCode(authErr, AuthErrorCodeDenied) {
		t.Fatalf("Authenticate() error = %v, want code %q", authErr, AuthErrorCodeDenied)
	}
	if got := authErr.HTTPStatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatusCode() = %d, want %d", got, http.StatusTooManyRequests)
	}
	if authErr.Message != "quota exhausted" {
		t.Fatalf("Message = %q, want %q", authErr.Message, "quota exhausted")
	}
	if laterCalled {
		t.Fatal("later provider ran after a denied provider ended the chain")
	}
}

// TestNewDeniedErrorDefaultsAndFallbackStatus pins the two defaults a websocket
// turn relies on: an empty denial message still reads as an HTTP status text and
// a denial without an explicit status is a 403, not a 500.
func TestNewDeniedErrorDefaultsAndFallbackStatus(t *testing.T) {
	denied := NewDeniedError("", 0)
	if denied.Code != AuthErrorCodeDenied {
		t.Fatalf("Code = %q, want %q", denied.Code, AuthErrorCodeDenied)
	}
	if got := denied.HTTPStatusCode(); got != http.StatusForbidden {
		t.Fatalf("HTTPStatusCode() = %d, want %d", got, http.StatusForbidden)
	}
	if got := denied.Message; got != http.StatusText(http.StatusForbidden) {
		t.Fatalf("Message = %q, want %q", got, http.StatusText(http.StatusForbidden))
	}
	if IsAuthErrorCode(denied, AuthErrorCodeInvalidCredential) {
		t.Fatal("a denial must not be reported as an invalid credential: retry and failover treat them differently")
	}

	notDenied := newAuthError(AuthErrorCodeInternal, "boom", 0, nil)
	if got := notDenied.HTTPStatusCode(); got != http.StatusInternalServerError {
		t.Fatalf("non-denied fallback status = %d, want %d", got, http.StatusInternalServerError)
	}
}
