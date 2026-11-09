package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantDisp Disposition
		wantWait time.Duration
		wantOK   bool
	}{
		{
			name:     "500 with rate limit body retries with backoff",
			status:   http.StatusInternalServerError,
			body:     "rate limit exceeded",
			wantDisp: DispositionRetryBackoff,
			wantOK:   true,
		},
		{
			name:     "200 with quota exceeded body retries with backoff",
			status:   http.StatusOK,
			body:     "quota exceeded",
			wantDisp: DispositionRetryBackoff,
			wantOK:   true,
		},
		{
			name:     "400 invalid_request_error is not retried",
			status:   http.StatusBadRequest,
			body:     "invalid_request_error",
			wantDisp: DispositionNone,
			wantOK:   false,
		},
		{
			name:     "text rule beats a conflicting status rule",
			status:   http.StatusForbidden,
			body:     "rate limit",
			wantDisp: DispositionRetryBackoff,
			wantOK:   true,
		},
		{
			name:     "unmatched status and body do not match",
			status:   http.StatusTeapot,
			body:     "nothing recognizable",
			wantDisp: DispositionNone,
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disp, wait, ok := Classify(tt.status, tt.body)
			if disp != tt.wantDisp {
				t.Fatalf("Classify() disp = %v, want %v", disp, tt.wantDisp)
			}
			if ok != tt.wantOK {
				t.Fatalf("Classify() ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantWait != 0 && wait != tt.wantWait {
				t.Fatalf("Classify() wait = %v, want %v", wait, tt.wantWait)
			}
		})
	}
}

func TestClassifyCooldownDurations(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantWait time.Duration
	}{
		{name: "no credentials", status: 0, body: "no credentials", wantWait: 2 * time.Minute},
		{name: "credit balance too low", status: http.StatusBadRequest, body: "Your credit balance is too low to access the Anthropic API.", wantWait: 30 * time.Minute},
		{name: "request not allowed", status: 0, body: "request not allowed", wantWait: 5 * time.Second},
		{name: "improperly formed request", status: 0, body: "improperly formed request", wantWait: 2 * time.Minute},
		{name: "401 status", status: http.StatusUnauthorized, body: "", wantWait: 2 * time.Minute},
		{name: "402 status", status: http.StatusPaymentRequired, body: "", wantWait: 2 * time.Minute},
		{name: "403 status", status: http.StatusForbidden, body: "", wantWait: 2 * time.Minute},
		{name: "404 status", status: http.StatusNotFound, body: "", wantWait: 2 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disp, wait, ok := Classify(tt.status, tt.body)
			if !ok || disp != DispositionCooldown {
				t.Fatalf("Classify() = (%v, %v, %v), want DispositionCooldown", disp, wait, ok)
			}
			if wait != tt.wantWait {
				t.Fatalf("Classify() wait = %v, want %v", wait, tt.wantWait)
			}
		})
	}
}

func TestClassify429IsRetryBackoff(t *testing.T) {
	disp, _, ok := Classify(http.StatusTooManyRequests, "")
	if !ok || disp != DispositionRetryBackoff {
		t.Fatalf("Classify(429) = (%v, ok=%v), want DispositionRetryBackoff", disp, ok)
	}
}

func TestRetryBackoffWaitClampsProviderReportedReset(t *testing.T) {
	err := &retryAfterStatusError{status: http.StatusTooManyRequests, message: "quota", retryAfter: 6 * time.Hour}
	wait := retryBackoffWait(err, 0)
	if wait != quotaBackoffMax {
		t.Fatalf("retryBackoffWait() = %v, want clamp to %v", wait, quotaBackoffMax)
	}
}

func TestRetryBackoffWaitUsesAttemptLevelWithoutProviderReset(t *testing.T) {
	err := &Error{HTTPStatus: http.StatusInternalServerError, Message: "rate limit exceeded"}
	wait := retryBackoffWait(err, 0)
	want, _ := nextQuotaCooldown(0, false)
	if wait != want {
		t.Fatalf("retryBackoffWait() = %v, want %v", wait, want)
	}
}
