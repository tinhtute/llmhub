package cliproxy

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/watcher"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/therealtinhtute/llmhub/sdk/config"
)

// A credential file snapshot reaches the manager through applyCoreAuthAddOrUpdate.
// It carries configuration only, so a live runtime cooldown must survive it.
func TestServiceFileSnapshotPreservesCooldown(t *testing.T) {
	const authID = "service-file-snapshot-cooldown-auth"
	ctx := context.Background()
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	service.applyCoreAuthAddOrUpdate(ctx, &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
	})
	service.coreManager.MarkResult(ctx, coreauth.Result{
		AuthID: authID, Provider: "claude", Success: false,
		Error: &coreauth.Error{Code: "quota_exhausted", Message: "QUOTA_EXHAUSTED", Retryable: true, HTTPStatus: http.StatusTooManyRequests},
	})
	cooled, ok := service.coreManager.GetByID(authID)
	if !ok || !cooled.Quota.Exceeded || !cooled.NextRetryAfter.After(time.Now()) {
		t.Fatalf("precondition: expected a live cooldown, got %+v", cooled)
	}

	path := filepath.Join(t.TempDir(), authID+".json")
	data := []byte(`{"type":"claude"}`)
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	snapshot := &coreauth.Auth{
		ID:         authID,
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributePath: path, "excluded_models": "new-model"},
	}
	snapshot.BindFileSnapshot(path, data)
	service.applyCoreAuthAddOrUpdate(ctx, snapshot)

	applied, ok := service.coreManager.GetByID(authID)
	if !ok {
		t.Fatalf("auth disappeared after the file snapshot")
	}
	if applied.Attributes["excluded_models"] != "new-model" {
		t.Fatalf("snapshot configuration did not apply: %+v", applied.Attributes)
	}
	if !applied.Quota.Exceeded || !applied.NextRetryAfter.After(time.Now()) {
		t.Fatalf("file snapshot cleared the live cooldown: quota=%+v retry=%v", applied.Quota, applied.NextRetryAfter)
	}
}

// A file event the watcher enqueued before an operator disabled the credential
// reaches the manager after that decision. The disable is fenced to the file
// observation it was taken against, so the delayed event cannot undo it while an
// event carrying content written afterwards re-enables the credential.
func TestServiceDelayedSnapshotCannotUndoOperatorDisable(t *testing.T) {
	const authID = "service-delayed-snapshot-disable-auth"
	ctx := coreauth.WithSkipPersist(context.Background())
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	path := filepath.Join(t.TempDir(), authID+".json")
	login := func(token string) *coreauth.Auth {
		t.Helper()
		data := []byte(`{"type":"claude","access_token":"` + token + `"}`)
		if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
			t.Fatalf("write auth file: %v", errWrite)
		}
		snapshot := &coreauth.Auth{
			ID:         authID,
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{coreauth.AttributePath: path},
			Metadata:   map[string]any{"type": "claude", "access_token": token},
		}
		snapshot.BindFileSnapshot(path, data)
		return snapshot
	}
	deliver := func(observed *coreauth.Auth) *coreauth.Auth {
		t.Helper()
		service.handleAuthUpdate(ctx, watcher.AuthUpdate{
			Action: watcher.AuthUpdateActionModify, ID: authID, Auth: observed,
		})
		applied, ok := service.coreManager.GetByID(authID)
		if !ok || applied == nil {
			t.Fatalf("the file event left no record behind")
		}
		return applied
	}

	service.handleAuthUpdate(ctx, watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionAdd, ID: authID, Auth: login("first-token"),
	})
	if registered, ok := service.coreManager.GetByID(authID); !ok || registered == nil {
		t.Fatal("precondition: the login snapshot did not register")
	}

	if _, committed, errModify := service.coreManager.Modify(ctx, authID, 0, func(current *coreauth.Auth) error {
		current.Disabled = true
		current.Status = coreauth.StatusDisabled
		current.StatusMessage = "disabled via management API"
		return nil
	}); errModify != nil || !committed {
		t.Fatalf("operator disable: committed=%v err=%v", committed, errModify)
	}

	// The delayed event carries the bytes the disable was taken against.
	stale := deliver(login("first-token"))
	if !stale.Disabled || stale.Status != coreauth.StatusDisabled {
		t.Fatalf("the delayed file event undid the operator disable: disabled=%v status=%q",
			stale.Disabled, stale.Status)
	}

	// An event carrying content written after the disable is newer than it.
	newer := deliver(login("second-token"))
	if newer.Disabled || newer.Status != coreauth.StatusActive {
		t.Fatalf("a file written after the disable did not re-enable the credential: disabled=%v status=%q",
			newer.Disabled, newer.Status)
	}
	if got := newer.Metadata["access_token"]; got != "second-token" {
		t.Fatalf("re-enabled record lost the newer credential: token=%v", got)
	}

	if replayed := deliver(login("second-token")); replayed.Disabled {
		t.Fatalf("a replayed newer file event flipped the record back to disabled")
	}
}
