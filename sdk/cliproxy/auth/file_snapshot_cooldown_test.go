package auth

import (
	"context"
	"errors"
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

// A file snapshot carries credentials and configuration; whether the credential
// is disabled belongs to the store, and that decision is fenced to the file
// observation it was taken against. A snapshot of that same observation was read
// before the decision and cannot undo it; a snapshot of content written after it
// is genuinely newer and re-enables the credential.
func TestFileSnapshotFenceRefusesOlderAndLandsNewer(t *testing.T) {
	const authID = "file-snapshot-operator-disable-auth"
	ctx := WithSkipPersist(context.Background())
	manager := NewManager(nil, nil, nil)

	path := filepath.Join(t.TempDir(), authID+".json")
	// snapshot writes token to the auth file and returns the observation the
	// watcher synthesizes from those bytes.
	snapshot := func(token string) *Auth {
		t.Helper()
		content := []byte(`{"type":"claude","access_token":"` + token + `"}`)
		if errWrite := os.WriteFile(path, content, 0o600); errWrite != nil {
			t.Fatalf("write auth file: %v", errWrite)
		}
		observed := &Auth{
			ID:         authID,
			Provider:   "claude",
			Status:     StatusActive,
			Metadata:   map[string]any{"access_token": token},
			Attributes: map[string]string{AttributePath: path},
		}
		observed.BindFileSnapshot(path, content)
		return observed
	}
	setDisabled := func(disabled bool) {
		t.Helper()
		statusMessage := ""
		if disabled {
			statusMessage = "disabled via management API"
		}
		if _, committed, errModify := manager.Modify(ctx, authID, 0, func(current *Auth) error {
			current.Disabled = disabled
			current.Status = StatusActive
			if disabled {
				current.Status = StatusDisabled
			}
			current.StatusMessage = statusMessage
			return nil
		}); errModify != nil || !committed {
			t.Fatalf("operator toggle: committed=%v err=%v", committed, errModify)
		}
	}
	apply := func(observed *Auth) *Auth {
		t.Helper()
		if _, errUpdate := manager.Update(ctx, observed); errUpdate != nil {
			t.Fatalf("snapshot update rejected: %v", errUpdate)
		}
		applied, _ := manager.GetByID(authID)
		return applied
	}

	if _, errRegister := manager.Register(ctx, snapshot("first-token")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	setDisabled(true)

	// A delayed login hook delivers the observation the record had before the
	// disable: the bytes are the ones the decision was taken against, so the
	// snapshot is older than the decision.
	stale := apply(snapshot("first-token"))
	if !stale.Disabled || stale.Status != StatusDisabled {
		t.Fatalf("a snapshot of the pre-disable observation undid the operator disable: disabled=%v status=%q",
			stale.Disabled, stale.Status)
	}

	// The file is written afterwards: that observation is newer than the disable,
	// so the credential returns to rotation with the new token.
	newer := apply(snapshot("second-token"))
	if newer.Disabled || newer.Status != StatusActive {
		t.Fatalf("a file written after the disable did not re-enable the credential: disabled=%v status=%q",
			newer.Disabled, newer.Status)
	}
	if got := authAccessToken(newer); got != "second-token" {
		t.Fatalf("re-enabled record lost the newer credential: token=%q", got)
	}

	// Replaying the same observation converges instead of flipping the record.
	replayed := apply(snapshot("second-token"))
	if replayed.Disabled || replayed.Status != StatusActive {
		t.Fatalf("a replayed newer observation flipped the record: disabled=%v status=%q",
			replayed.Disabled, replayed.Status)
	}

	// An operator disable after that land fences the newer observation, so
	// replaying it stays disabled.
	setDisabled(true)
	if fenced := apply(snapshot("second-token")); !fenced.Disabled || fenced.Status != StatusDisabled {
		t.Fatalf("a snapshot of the observation the new disable was taken against re-enabled the record: disabled=%v status=%q",
			fenced.Disabled, fenced.Status)
	}
}

// A credential that was re-logged after refresh failures replaces the tokens the
// failures were about, so it returns to rotation: the carried invalid_grant or
// unavailable state and the stale refresh backoff are dropped. A live retry or
// quota cooldown was recorded for a different cause and keeps holding it back.
func TestFileSnapshotReloginRestoresRotation(t *testing.T) {
	invalidGrant := errors.New(`token refresh failed with status 400: {"error": "invalid_grant", "error_description": "Refresh token not found or invalid"}`)
	outage := errors.New("token refresh failed with status 503: upstream connect error")
	for _, tc := range []struct {
		name        string
		refreshErr  error
		rateLimited bool
	}{
		{name: "invalid grant", refreshErr: invalidGrant},
		{name: "token endpoint outage", refreshErr: outage},
		{name: "active 429 cooldown", refreshErr: outage, rateLimited: true},
		{name: "active 429 cooldown with invalid grant", refreshErr: invalidGrant, rateLimited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const authID = "relogin-claude.json"
			ctx := WithSkipPersist(context.Background())
			manager := NewManager(nil, nil, nil)
			manager.RegisterExecutor(failingRefreshExecutor{
				schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"},
				err:                           tc.refreshErr,
			})
			path := filepath.Join(t.TempDir(), authID)
			// reloginSnapshot writes the credential file and returns it the way the
			// watcher reads it.
			reloginSnapshot := func(token string, expiry time.Time) *Auth {
				t.Helper()
				expired := expiry.Format(time.RFC3339)
				content := []byte(`{"type":"claude","access_token":"` + token + `-at","refresh_token":"` + token + `-rt","expired":"` + expired + `"}`)
				if errWrite := os.WriteFile(path, content, 0o600); errWrite != nil {
					t.Fatalf("write auth file: %v", errWrite)
				}
				snapshot := &Auth{
					ID:         authID,
					Provider:   "claude",
					FileName:   authID,
					Status:     StatusActive,
					Attributes: map[string]string{AttributePath: path},
					Metadata:   map[string]any{"type": "claude", "access_token": token + "-at", "refresh_token": token + "-rt", "expired": expired},
				}
				snapshot.BindFileSnapshot(path, content)
				return snapshot
			}

			if _, errRegister := manager.Register(ctx, reloginSnapshot("old", time.Now().Add(-time.Hour))); errRegister != nil {
				t.Fatalf("register: %v", errRegister)
			}
			if tc.rateLimited {
				retryAfter := time.Hour
				manager.MarkResult(ctx, Result{
					AuthID: authID, Provider: "claude", RetryAfter: &retryAfter,
					Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
				})
			}
			manager.refreshAuth(ctx, authID) // expired token, refresh fails

			if !tc.rateLimited {
				// The counterweight: a snapshot of the credential that failed keeps
				// its failure state, or every reload would resume a dead token.
				if _, errUpdate := manager.Update(ctx, reloginSnapshot("old", time.Now().Add(-time.Hour))); errUpdate != nil {
					t.Fatalf("unchanged snapshot update rejected: %v", errUpdate)
				}
				carried, _ := manager.GetByID(authID)
				if !carried.Unavailable || carried.Status != StatusError || carried.LastError == nil {
					t.Fatalf("unchanged-credential snapshot dropped the refresh failure: status=%q unavailable=%v last_error=%v",
						carried.Status, carried.Unavailable, carried.LastError)
				}
			}

			if _, errUpdate := manager.Update(ctx, reloginSnapshot("new", time.Now().Add(8*time.Hour))); errUpdate != nil {
				t.Fatalf("re-login snapshot rejected: %v", errUpdate)
			}
			after, _ := manager.GetByID(authID)
			if tc.rateLimited {
				if !after.Unavailable || !after.NextRetryAfter.After(time.Now()) {
					t.Fatalf("re-login released a live 429 cooldown: unavailable=%v retry=%v",
						after.Unavailable, after.NextRetryAfter)
				}
				if after.RefreshFailures != 0 || !after.NextRefreshAfter.IsZero() {
					t.Fatalf("re-login kept the stale refresh backoff: refresh_failures=%d next_refresh_after=%v",
						after.RefreshFailures, after.NextRefreshAfter)
				}
				return
			}
			if after.Unavailable || after.Status != StatusActive || after.LastError != nil ||
				after.RefreshFailures != 0 || !after.NextRefreshAfter.IsZero() {
				t.Fatalf("re-logged credential still out of rotation: status=%q msg=%q unavailable=%v refresh_failures=%d next_refresh_after=%v",
					after.Status, after.StatusMessage, after.Unavailable, after.RefreshFailures, after.NextRefreshAfter)
			}
		})
	}
}

// A record disabled before this process recorded any file observation — after a
// restart, say — has nothing a snapshot can be proven newer than, so the first
// snapshot keeps the disable. It also records the observation, so the fence is a
// fence and not a latch: the file written after it re-enables the credential.
func TestFileSnapshotDisableFenceConvergesWithoutRecordedObservation(t *testing.T) {
	const authID = "file-snapshot-disable-no-observation-auth"
	ctx := WithSkipPersist(context.Background())
	manager := NewManager(nil, nil, nil)

	path := filepath.Join(t.TempDir(), authID+".json")
	observe := func(token string) *Auth {
		t.Helper()
		content := []byte(`{"type":"claude","access_token":"` + token + `"}`)
		if errWrite := os.WriteFile(path, content, 0o600); errWrite != nil {
			t.Fatalf("write auth file: %v", errWrite)
		}
		observed := &Auth{
			ID:         authID,
			Provider:   "claude",
			Status:     StatusActive,
			Metadata:   map[string]any{"access_token": token},
			Attributes: map[string]string{AttributePath: path},
		}
		observed.BindFileSnapshot(path, content)
		return observed
	}
	// The disabled record arrives with the file it was disabled against.
	if _, errRegister := manager.Register(ctx, &Auth{
		ID:         authID,
		Provider:   "claude",
		Status:     StatusDisabled,
		Disabled:   true,
		Metadata:   map[string]any{"access_token": "first-token"},
		Attributes: map[string]string{AttributePath: path},
	}); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	if errWrite := os.WriteFile(path, []byte(`{"type":"claude","access_token":"first-token"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	if _, errUpdate := manager.Update(ctx, observe("first-token")); errUpdate != nil {
		t.Fatalf("snapshot update rejected: %v", errUpdate)
	}
	if first, _ := manager.GetByID(authID); !first.Disabled || first.Status != StatusDisabled {
		t.Fatalf("a snapshot with no recorded observation to be newer than enabled the record: disabled=%v status=%q",
			first.Disabled, first.Status)
	}

	if _, errUpdate := manager.Update(ctx, observe("second-token")); errUpdate != nil {
		t.Fatalf("snapshot update rejected: %v", errUpdate)
	}
	newer, _ := manager.GetByID(authID)
	if newer.Disabled || newer.Status != StatusActive {
		t.Fatalf("the fence latched: content written after the first observation did not re-enable the record: disabled=%v status=%q",
			newer.Disabled, newer.Status)
	}
	if got := authAccessToken(newer); got != "second-token" {
		t.Fatalf("re-enabled record lost the newer credential: token=%q", got)
	}
}
