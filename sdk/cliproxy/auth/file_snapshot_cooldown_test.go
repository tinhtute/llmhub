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
