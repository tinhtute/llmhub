package watcher

import (
	"path/filepath"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// A snapshot the manager discarded as stale must be re-read from disk instead of
// being dropped, and the re-read must happen once: the invalidation it performs is
// what makes a second discard of the same observation a no-op.
func TestDiscardedFileSnapshotRereadsOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	w.addOrUpdateClient(path)
	published := drainScanUpdates(w)
	if len(published) != 1 || published[0].Auth == nil || !published[0].Auth.HasFileSnapshot() {
		t.Fatalf("precondition: expected one file snapshot update, got %+v", published)
	}
	stale := published[0].Auth

	// The file moves on without an event, so the manager still holds the snapshot
	// it is about to discard as stale.
	writeScanAuthFile(t, path, scanTestAuthJSONRewritten)
	w.rereadDiscardedFileSnapshot(stale)

	updates := drainScanUpdates(w)
	if len(updates) != 1 {
		t.Fatalf("discarded snapshot was not re-read: %+v", updates)
	}
	if updates[0].Action != AuthUpdateActionModify || updates[0].Auth == nil {
		t.Fatalf("expected a modify carrying the re-read content, got %+v", updates[0])
	}
	if stale.SameFileSnapshot(updates[0].Auth) {
		t.Fatalf("re-read produced the same observation the manager discarded")
	}

	// The same discarded observation can never be re-read twice.
	w.rereadDiscardedFileSnapshot(stale)
	if extra := drainScanUpdates(w); len(extra) != 0 {
		t.Fatalf("re-read of the same discarded snapshot repeated: %+v", extra)
	}

	// The invalidation is what breaks the match, and it must not turn the record
	// into a non-file snapshot: the watcher still owns re-reading it.
	invalidated := updates[0].Auth.Clone()
	if !invalidated.HasFileSnapshot() {
		t.Fatalf("a file snapshot stopped being one when invalidated")
	}
	invalidated.InvalidateFileSnapshot()
	if !invalidated.HasFileSnapshot() {
		t.Fatalf("an invalidated file snapshot stopped being one")
	}
	if invalidated.SameFileSnapshot(updates[0].Auth) {
		t.Fatalf("an invalidated file snapshot still matched its original observation")
	}
}

// A login hook that persists its file-bound snapshot while a full scan is running
// keeps that binding, and the scan itself re-reads the file once it finishes: the
// hook observed the file after the scan read it, so republishing the scan's own
// observation would drop the content the file holds now. Nothing here calls the
// re-read helper: the scan loop has to do it.
func TestRefreshAuthStateRereadsSnapshotPersistedDuringScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	w.addOrUpdateClient(path)
	published := drainScanUpdates(w)
	if len(published) != 1 || published[0].Auth == nil || !published[0].Auth.HasFileSnapshot() {
		t.Fatalf("precondition: expected one file snapshot update, got %+v", published)
	}
	persisted := published[0].Auth
	authID := persisted.ID

	original := snapshotCoreAuthsFunc
	t.Cleanup(func() { snapshotCoreAuthsFunc = original })
	snapshotCoreAuthsFunc = func(cfg *config.Config, authDir string) ([]*coreauth.Auth, map[string]struct{}) {
		// The hook persists the snapshot it read before the file moved on.
		w.DispatchRuntimeAuthUpdate(AuthUpdate{Action: AuthUpdateActionModify, ID: authID, Auth: persisted.Clone()})
		writeScanAuthFile(t, path, scanTestAuthJSONRewritten)
		return original(cfg, authDir)
	}
	w.refreshAuthState(false)
	snapshotCoreAuthsFunc = original

	// The scan republished its own observation first, but its re-read of the file
	// lands last and carries the content the file holds now.
	updates := drainScanUpdates(w)
	if len(updates) != 1 || updates[0].Auth == nil {
		t.Fatalf("a scan finishing over a persisted snapshot dispatched %d updates, want 1: %+v", len(updates), updates)
	}
	if !updates[0].Auth.HasFileSnapshot() {
		t.Fatalf("the re-read dropped the file snapshot binding: %+v", updates[0].Auth)
	}
	if updates[0].Auth.SameFileSnapshot(persisted) {
		t.Fatalf("the scan republished the observation it started with instead of re-reading the file")
	}
	if updates[0].Auth.Prefix != "pfx-rewritten" {
		t.Fatalf("the scan did not re-read the file's current content: prefix=%q", updates[0].Auth.Prefix)
	}
	if current := w.currentAuths[authID]; current == nil || current.Prefix != "pfx-rewritten" {
		t.Fatalf("the watcher still caches the pre-persist content: %+v", current)
	}

	// The re-read settles the scan: a following scan has nothing new to publish.
	w.refreshAuthState(false)
	if settled := drainScanUpdates(w); len(settled) != 0 {
		t.Fatalf("a scan after the re-read churned %d updates: %+v", len(settled), settled)
	}
}
