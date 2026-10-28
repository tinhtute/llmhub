package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"time"
)

// The following tests cover the transient pre-HTTP transport retry semantics
// ported from upstream CLIProxyAPI commit bef1f65c6c1d ("feat(auth): add retry
// logic for pre-HTTP transport failures"). Local symbols under test:
// isTransientTransportError, isTransientTransportResultError,
// isTransientSyscallError, isTransientTransportMessage, isRequestRetryRoundError,
// resultErrorFromError, shouldSkipCredentialCooldown,
// Manager.shouldRetryAfterError, ErrorCodeTransientTransport.

func TestIsTransientTransportError_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"http status stays on credential path", &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}, false},
		{"context canceled excluded", context.Canceled, false},
		{"deadline exceeded excluded", context.DeadlineExceeded, false},
		{"io.EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"dns timeout", &net.DNSError{Err: "timeout", Name: "example.com", IsTimeout: true}, true},
		{"dns temporary", &net.DNSError{Err: "temporary", Name: "example.com", IsTemporary: true}, true},
		{"net timeout", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, true},
		{"op error", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"syscall conn reset", syscall.ECONNRESET, true},
		{"syscall conn refused", syscall.ECONNREFUSED, true},
		{"syscall timed out", syscall.ETIMEDOUT, true},
		{"syscall host unreachable", syscall.EHOSTUNREACH, true},
		{"syscall broken pipe", syscall.EPIPE, true},
		{"wrapped url error", &url.Error{Op: "Get", URL: "https://example.com", Err: syscall.ECONNRESET}, true},
		{"tls record header message", &tls.RecordHeaderError{Msg: "tls handshake timeout"}, true},
		{"message connection reset", errors.New("read tcp 1.2.3.4: connection reset by peer"), true},
		{"message no such host", errors.New("dial tcp: lookup api.example.com: no such host"), true},
		{"message use of closed connection", errors.New("use of closed network connection"), true},
		{"generic error not classified", errors.New("some random failure"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientTransportError(tc.err); got != tc.want {
				t.Fatalf("isTransientTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestResultErrorFromError_TransientTransportCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"connection refused op error", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, ErrorCodeTransientTransport},
		{"tls message", errors.New("tls handshake timeout"), ErrorCodeTransientTransport},
		{"eof is lifecycle first", io.EOF, ErrorCodeConnectionLifecycle},
		{"http status untouched", &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit"}, ""},
		{"generic error untouched", errors.New("unrelated failure"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resultErrorFromError(tc.err)
			if got == nil {
				t.Fatal("resultErrorFromError() = nil")
			}
			if got.Code != tc.want {
				t.Fatalf("resultErrorFromError().Code = %q, want %q", got.Code, tc.want)
			}
		})
	}
}

func TestShouldSkipCredentialCooldown_TransientTransport(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want bool
	}{
		{"coded transport error", &Error{Code: ErrorCodeTransientTransport, Message: "dial tcp: connection refused"}, true},
		{"message-only transport error", &Error{Message: "read tcp: connection reset by peer"}, true},
		{"statused error not skipped by transport", &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit"}, false},
		{"force cooldown wins", &Error{Code: ErrorCodeForceCooldown, Message: "connection reset"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSkipCredentialCooldown(tc.err); got != tc.want {
				t.Fatalf("shouldSkipCredentialCooldown(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// Transport failures must participate in request-retry rounds without waiting
// on credential cooldown, matching upstream isRequestRetryRoundError usage.
func TestShouldRetryAfterError_TransientTransportRetriesWithoutCooldown(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(3, time.Minute, 0)

	auth := &Auth{ID: "transport-retry", Provider: "codex", Metadata: map[string]any{"access_token": "tok"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	transportErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	wait, shouldRetry := manager.shouldRetryAfterError(transportErr, 0, []string{"codex"}, "model", time.Minute)
	if !shouldRetry {
		t.Fatal("shouldRetryAfterError() = false, want true for transient transport error")
	}
	if wait != 0 {
		t.Fatalf("wait = %v, want immediate retry (0)", wait)
	}

	// The credential must not be cooled by the retry decision.
	updated, _ := manager.GetByID("transport-retry")
	if updated.Unavailable {
		t.Fatal("credential cooled by transient transport retry decision")
	}
}

func TestShouldRetryAfterError_TransientTransportHonorsRetryLimit(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(1, time.Minute, 0)
	if _, err := manager.Register(context.Background(), &Auth{ID: "transport-cap", Provider: "codex", Metadata: map[string]any{"access_token": "tok"}}); err != nil {
		t.Fatalf("register: %v", err)
	}

	transportErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNRESET}
	if _, shouldRetry := manager.shouldRetryAfterError(transportErr, 5, []string{"codex"}, "model", time.Minute); shouldRetry {
		t.Fatal("shouldRetryAfterError() = true past retry limit, want false")
	}
}

func TestIsRequestRetryRoundError_IncludesTransport(t *testing.T) {
	if !isRequestRetryRoundError(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}) {
		t.Fatal("isRequestRetryRoundError() = false for transport error, want true")
	}
	if !isRequestRetryRoundError(&Error{HTTPStatus: http.StatusServiceUnavailable, Message: "unavailable"}) {
		t.Fatal("isRequestRetryRoundError() = false for 503, want true")
	}
	if isRequestRetryRoundError(&Error{HTTPStatus: http.StatusBadRequest, Message: "bad request"}) {
		t.Fatal("isRequestRetryRoundError() = true for 400, want false")
	}
	if isRequestRetryRoundError(nil) {
		t.Fatal("isRequestRetryRoundError(nil) = true, want false")
	}
}

// End-to-end: MarkResult must not cool the credential for transport failures.
func TestMarkResult_TransientTransportSkipsCooldown(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{ID: "transport-mark", Provider: "codex", Metadata: map[string]any{"access_token": "tok"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "transport-mark",
		Provider: "codex",
		Model:    "model",
		Success:  false,
		Error:    &Error{Code: ErrorCodeTransientTransport, Message: fmt.Sprintf("dial tcp: %v", syscall.ECONNREFUSED)},
	})

	updated, _ := manager.GetByID("transport-mark")
	if updated.Unavailable {
		t.Fatal("MarkResult cooled credential for transient transport error")
	}
	if updated.Status == StatusError {
		t.Fatalf("Status = %v, want not StatusError for transient transport", updated.Status)
	}
}

// streamTruncatedExecutorError is the executor contract for a locally
// synthesized failure on an accepted stream that never received its terminal
// frame: public status 502, classified as truncated (upstream aa4130e6).
type streamTruncatedExecutorError struct {
	msg string
}

func (e streamTruncatedExecutorError) Error() string           { return e.msg }
func (e streamTruncatedExecutorError) StatusCode() int         { return http.StatusBadGateway }
func (e streamTruncatedExecutorError) IsStreamTruncated() bool { return true }

// plainExecutorGatewayError has the same status and text without the
// classification, so the table proves the classification drives the outcome.
type plainExecutorGatewayError struct {
	msg string
}

func (e plainExecutorGatewayError) Error() string   { return e.msg }
func (e plainExecutorGatewayError) StatusCode() int { return http.StatusBadGateway }

func TestStreamTruncatedCooldownClassification(t *testing.T) {
	const msg = "upstream stream closed before finishReason"
	cases := []struct {
		name             string
		err              error
		wantCode         string
		wantSkipCooldown bool
		wantRequestFault bool
	}{
		{
			name:             "truncated accepted stream",
			err:              streamTruncatedExecutorError{msg: msg},
			wantCode:         ErrorCodeStreamTruncated,
			wantSkipCooldown: true,
			wantRequestFault: true,
		},
		{
			name:             "real upstream 5xx keeps cooling",
			err:              plainExecutorGatewayError{msg: "mock upstream rejection"},
			wantSkipCooldown: false,
			wantRequestFault: false,
		},
		{
			name:             "truncation result code alone is enough",
			err:              &Error{Code: ErrorCodeStreamTruncated, Message: msg, HTTPStatus: http.StatusBadGateway},
			wantCode:         ErrorCodeStreamTruncated,
			wantSkipCooldown: true,
			wantRequestFault: true,
		},
		{
			name:             "timeout keeps its lifecycle handling",
			err:              fmt.Errorf("post stream: %w", context.DeadlineExceeded),
			wantCode:         ErrorCodeConnectionLifecycle,
			wantSkipCooldown: true,
			wantRequestFault: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resultErr := resultErrorFromError(tc.err)
			if resultErr == nil {
				t.Fatal("resultErrorFromError() = nil")
			}
			if resultErr.Code != tc.wantCode {
				t.Fatalf("result code = %q, want %q", resultErr.Code, tc.wantCode)
			}
			if got := shouldSkipCredentialCooldown(resultErr); got != tc.wantSkipCooldown {
				t.Fatalf("shouldSkipCredentialCooldown(%+v) = %v, want %v", resultErr, got, tc.wantSkipCooldown)
			}
			if got := isRequestInvalidError(tc.err); got != tc.wantRequestFault {
				t.Fatalf("isRequestInvalidError(%v) = %v, want %v", tc.err, got, tc.wantRequestFault)
			}
		})
	}
}

// MarkResult is the only writer of credential cooldown state; a truncated
// accepted stream must leave the credential usable.
func TestMarkResult_StreamTruncatedSkipsCooldown(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, err := manager.Register(context.Background(), &Auth{ID: "trunc-mark", Provider: "gemini"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "trunc-mark",
		Provider: "gemini",
		Model:    "gemini-3.8-flash",
		Success:  false,
		Error:    resultErrorFromError(streamTruncatedExecutorError{msg: "upstream stream closed before finishReason"}),
	})

	updated, _ := manager.GetByID("trunc-mark")
	if updated.Unavailable {
		t.Fatal("MarkResult cooled the credential for a truncated accepted stream")
	}
	if state := updated.ModelStates["gemini-3.8-flash"]; state != nil && state.Unavailable && state.NextRetryAfter.After(time.Now()) {
		t.Fatalf("model state cooled for a truncated accepted stream: %+v", state)
	}
}
