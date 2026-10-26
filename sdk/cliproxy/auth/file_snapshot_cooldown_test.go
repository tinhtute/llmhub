package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFileSnapshotAuth writes a real auth file and returns a file snapshot
// bound to it, the way the watcher synthesizes one from disk.
func writeFileSnapshotAuth(t *testing.T, authID, provider string, attributes map[string]string) *Auth {
	t.Helper()
	path := filepath.Join(t.TempDir(), authID+".json")
	data := []byte(`{"type":"` + provider + `"}`)
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	snapshot := &Auth{ID: authID, Provider: provider, Status: StatusActive, Attributes: attributes}
	snapshot.Attributes[AttributePath] = path
	snapshot.BindFileSnapshot(path, data)
	return snapshot
}

// A credential file snapshot carries configuration, not runtime availability:
// applying one must not wipe a live cooldown the credential is serving out.
func TestFileSnapshotKeepsCooldown(t *testing.T) {
	const authID = "file-snapshot-cooldown-auth"
	manager := issue6643SeqTestAuth(t, authID)
	ctx := context.Background()

	manager.MarkResult(ctx, Result{
		AuthID: authID, Provider: "antigravity", Success: false, ExecutionSeq: manager.nextExecutionSeq(),
		Error: &Error{Code: "quota_exhausted", Message: "QUOTA_EXHAUSTED", Retryable: true, HTTPStatus: http.StatusTooManyRequests},
	})
	cooled, _ := manager.GetByID(authID)
	if !cooled.Quota.Exceeded || !cooled.NextRetryAfter.After(time.Now()) || cooled.cooldownRecordedSeq == 0 {
		t.Fatalf("precondition: expected a live cooldown with a watermark, got quota=%+v retry=%v seq=%d",
			cooled.Quota, cooled.NextRetryAfter, cooled.cooldownRecordedSeq)
	}

	snapshot := writeFileSnapshotAuth(t, authID, "antigravity", map[string]string{"excluded_models": "new-model"})
	if _, errUpdate := manager.Update(WithSkipPersist(ctx), snapshot); errUpdate != nil {
		t.Fatalf("snapshot update rejected: %v", errUpdate)
	}

	applied, _ := manager.GetByID(authID)
	if applied.Attributes["excluded_models"] != "new-model" {
		t.Fatalf("snapshot configuration did not apply: %+v", applied.Attributes)
	}
	if !applied.Quota.Exceeded || !applied.NextRetryAfter.After(time.Now()) {
		t.Fatalf("file snapshot cleared the live cooldown: quota=%+v retry=%v", applied.Quota, applied.NextRetryAfter)
	}
	if applied.cooldownRecordedSeq != cooled.cooldownRecordedSeq {
		t.Fatalf("file snapshot dropped the cooldown watermark: got %d want %d",
			applied.cooldownRecordedSeq, cooled.cooldownRecordedSeq)
	}
	if applied.Status != cooled.Status {
		t.Fatalf("file snapshot reset the credential status: got %q want %q", applied.Status, cooled.Status)
	}
}

// A snapshot applied between a 429 and a success that was already in flight
// keeps the cooldown's watermark, so the stale success still cannot clear it.
func TestFileSnapshotKeepsCooldownWatermark(t *testing.T) {
	const authID = "file-snapshot-watermark-auth"
	manager := issue6643SeqTestAuth(t, authID)
	ctx := context.Background()

	staleSeq := manager.nextExecutionSeq()
	manager.MarkResult(ctx, Result{
		AuthID: authID, Provider: "antigravity", Success: false, ExecutionSeq: manager.nextExecutionSeq(),
		Error: &Error{Code: "quota_exhausted", Message: "QUOTA_EXHAUSTED", Retryable: true, HTTPStatus: http.StatusTooManyRequests},
	})
	cooled, _ := manager.GetByID(authID)
	if !cooled.Quota.Exceeded || !cooled.NextRetryAfter.After(time.Now()) {
		t.Fatalf("precondition: expected a live cooldown, got quota=%+v retry=%v", cooled.Quota, cooled.NextRetryAfter)
	}

	snapshot := writeFileSnapshotAuth(t, authID, "antigravity", map[string]string{})
	if _, errUpdate := manager.Update(WithSkipPersist(ctx), snapshot); errUpdate != nil {
		t.Fatalf("snapshot update rejected: %v", errUpdate)
	}

	manager.MarkResult(ctx, Result{AuthID: authID, Provider: "antigravity", Success: true, ExecutionSeq: staleSeq})
	if after, _ := manager.GetByID(authID); !after.Quota.Exceeded || !after.NextRetryAfter.After(time.Now()) {
		t.Fatalf("a success dispatched before the cooldown cleared it after a snapshot: quota=%+v retry=%v",
			after.Quota, after.NextRetryAfter)
	}

	manager.MarkResult(ctx, Result{AuthID: authID, Provider: "antigravity", Success: true, ExecutionSeq: manager.nextExecutionSeq()})
	if final, _ := manager.GetByID(authID); final.Quota.Exceeded {
		t.Fatalf("a success dispatched after the cooldown did not clear it: quota=%+v", final.Quota)
	}
}

// The credential_quota carry keeps the cooldown across an ordinary update, so
// it must keep the watermark that guards it from stale successes too.
func TestCredentialQuotaCarryKeepsCooldownWatermark(t *testing.T) {
	const authID = "credential-quota-watermark-auth"
	manager := issue6643SeqTestAuth(t, authID)
	ctx := context.Background()

	retry := 3 * time.Hour
	manager.MarkResult(ctx, Result{
		AuthID: authID, Provider: "antigravity", Model: "m", ExecutionSeq: manager.nextExecutionSeq(),
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Code: "usage_limit_reached", Message: "quota exceeded"},
		RetryAfter: &retry, CredentialScope: true,
	})
	cooled, _ := manager.GetByID(authID)
	if !cooled.Quota.Exceeded || cooled.Quota.Reason != "credential_quota" || cooled.cooldownRecordedSeq == 0 {
		t.Fatalf("precondition: expected a credential_quota cooldown with a watermark, got quota=%+v seq=%d",
			cooled.Quota, cooled.cooldownRecordedSeq)
	}

	if _, errUpdate := manager.Update(WithSkipPersist(ctx), &Auth{ID: authID, Provider: "antigravity", Status: StatusActive}); errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}

	got, _ := manager.GetByID(authID)
	if got.Quota.Reason != "credential_quota" {
		t.Fatalf("credential_quota cooldown was not carried: quota=%+v", got.Quota)
	}
	if got.cooldownRecordedSeq != cooled.cooldownRecordedSeq {
		t.Fatalf("credential_quota carry dropped the cooldown watermark: got %d want %d",
			got.cooldownRecordedSeq, cooled.cooldownRecordedSeq)
	}
}

// writeCredentialFileSnapshot writes an auth file carrying accessToken and returns
// a snapshot bound to it, the way the watcher synthesizes one from disk.
func writeCredentialFileSnapshot(t *testing.T, authID string, content []byte, accessToken string) *Auth {
	t.Helper()
	path := filepath.Join(t.TempDir(), authID+".json")
	if errWrite := os.WriteFile(path, content, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	snapshot := &Auth{
		ID:         authID,
		Provider:   "claude",
		Status:     StatusActive,
		Metadata:   map[string]any{"access_token": accessToken},
		Attributes: map[string]string{AttributePath: path},
	}
	snapshot.BindFileSnapshot(path, content)
	return snapshot
}

// registerRejectedCredential registers an auth whose token was rejected with a
// terminal 401: unavailable, status error, no pending refresh.
func registerRejectedCredential(t *testing.T, manager *Manager, authID, accessToken string) {
	t.Helper()
	ctx := context.Background()
	if _, errRegister := manager.Register(WithSkipPersist(ctx), &Auth{
		ID:       authID,
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": accessToken},
	}); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	manager.MarkResult(ctx, Result{
		AuthID: authID, Provider: "claude", Success: false,
		Error: &Error{HTTPStatus: http.StatusUnauthorized, Code: "unauthorized", Message: "401 unauthorized"},
	})
	failed, _ := manager.GetByID(authID)
	if !hasUnauthorizedAuthFailure(failed) {
		t.Fatalf("precondition: expected a terminal unauthorized state, got status=%q unavailable=%t lastError=%v",
			failed.Status, failed.Unavailable, failed.LastError)
	}
}

// A fresh credential in the same auth file replaces the one that was rejected:
// the terminal unauthorized state belongs to the old token, so the reload must not
// keep the credential unusable.
func TestFileSnapshotWithChangedCredentialClearsTerminalUnauthorized(t *testing.T) {
	const authID = "file-snapshot-changed-credential-auth"
	manager := NewManager(nil, nil, nil)
	registerRejectedCredential(t, manager, authID, "rejected-token")

	content := []byte(`{"type":"claude","access_token":"fresh-token"}`)
	snapshot := writeCredentialFileSnapshot(t, authID, content, "fresh-token")
	if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), snapshot); errUpdate != nil {
		t.Fatalf("snapshot update rejected: %v", errUpdate)
	}

	applied, _ := manager.GetByID(authID)
	if got := authAccessToken(applied); got != "fresh-token" {
		t.Fatalf("fresh credential did not land: got token %q", got)
	}
	if hasUnauthorizedAuthFailure(applied) {
		t.Fatalf("fresh credential inherited the rejected one's 401 state: status=%q unavailable=%t lastError=%v",
			applied.Status, applied.Unavailable, applied.LastError)
	}
	if applied.Unavailable || !applied.NextRetryAfter.IsZero() || applied.LastError != nil {
		t.Fatalf("terminal unauthorized state was not cleared: unavailable=%t retry=%v lastError=%v",
			applied.Unavailable, applied.NextRetryAfter, applied.LastError)
	}
	if applied.Status != StatusActive {
		t.Fatalf("status after a fresh credential = %q, want %q", applied.Status, StatusActive)
	}
	if applied.cooldownRecordedSeq != 0 {
		t.Fatalf("cooldown watermark survived the credential change: %d", applied.cooldownRecordedSeq)
	}
}

// The counterweight: a snapshot of the same rejected credential must not resurrect
// it, or a reload would clear a genuine 401 every time the file is re-read.
func TestFileSnapshotWithUnchangedCredentialKeepsTerminalUnauthorized(t *testing.T) {
	const authID = "file-snapshot-unchanged-credential-auth"
	manager := NewManager(nil, nil, nil)
	registerRejectedCredential(t, manager, authID, "rejected-token")

	content := []byte(`{"type":"claude","access_token":"rejected-token"}`)
	snapshot := writeCredentialFileSnapshot(t, authID, content, "rejected-token")
	if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), snapshot); errUpdate != nil {
		t.Fatalf("snapshot update rejected: %v", errUpdate)
	}

	applied, _ := manager.GetByID(authID)
	if !hasUnauthorizedAuthFailure(applied) {
		t.Fatalf("an unchanged rejected credential lost its 401 state: status=%q unavailable=%t lastError=%v",
			applied.Status, applied.Unavailable, applied.LastError)
	}
}
