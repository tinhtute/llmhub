package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/therealtinhtute/llmhub/sdk/config"
)

// A file snapshot the manager discards because the file already moved on must be
// re-read, not dropped: otherwise the credential keeps the content the file no
// longer holds until some later event happens to change that file again.
func TestServiceRereadsDiscardedFileSnapshot(t *testing.T) {
	const authID = "service-reread-discarded-snapshot"
	ctx := context.Background()

	var rereadIDs []string
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		watcher: &WatcherWrapper{
			rereadDiscardedFileSnapshot: func(auth *coreauth.Auth) {
				if auth != nil {
					rereadIDs = append(rereadIDs, auth.ID)
				}
			},
		},
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	path := filepath.Join(t.TempDir(), authID+".json")
	firstContent := []byte(`{"type":"claude","prefix":"first"}`)
	if errWrite := os.WriteFile(path, firstContent, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	fileSnapshot := func(content []byte, prefix string) *coreauth.Auth {
		snapshot := &coreauth.Auth{
			ID:         authID,
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Prefix:     prefix,
			Attributes: map[string]string{coreauth.AttributePath: path},
		}
		snapshot.BindFileSnapshot(path, content)
		return snapshot
	}

	service.applyCoreAuthAddOrUpdate(ctx, fileSnapshot(firstContent, "first"))
	if applied, ok := service.coreManager.GetByID(authID); !ok || applied.Prefix != "first" {
		t.Fatalf("precondition: snapshot was not applied, got %+v", applied)
	}
	if len(rereadIDs) != 0 {
		t.Fatalf("a matching snapshot triggered a re-read: %v", rereadIDs)
	}

	// The file moves on but the queued snapshot still describes the old content:
	// the manager discards it, and that discard must be answered with a re-read.
	secondContent := []byte(`{"type":"claude","prefix":"second"}`)
	if errWrite := os.WriteFile(path, secondContent, 0o600); errWrite != nil {
		t.Fatalf("rewrite auth file: %v", errWrite)
	}
	service.applyCoreAuthAddOrUpdate(ctx, fileSnapshot(firstContent, "first"))
	if len(rereadIDs) != 1 || rereadIDs[0] != authID {
		t.Fatalf("discarded snapshot was not re-read, re-reads=%v", rereadIDs)
	}

	// A snapshot of the current file content still applies without a re-read.
	service.applyCoreAuthAddOrUpdate(ctx, fileSnapshot(secondContent, "second"))
	if applied, ok := service.coreManager.GetByID(authID); !ok || applied.Prefix != "second" {
		t.Fatalf("fresh snapshot was not applied, got %+v", applied)
	}
	if len(rereadIDs) != 1 {
		t.Fatalf("a snapshot matching disk triggered another re-read: %v", rereadIDs)
	}
}
