package cliproxy

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

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
