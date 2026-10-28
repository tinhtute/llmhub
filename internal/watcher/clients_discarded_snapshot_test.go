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
// keeps that binding: the scan's own synthesis is withdrawn in favour of it, and
// when the file moved on in the meantime the retained binding is what lets the
// manager discard the observation and re-read the file instead of keeping stale
// content forever.
func TestRefreshAuthStateKeepsBoundSnapshotPersistedDuringScan(t *testing.T) {
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
		// The hook's snapshot was read before the operator's file moved on.
		w.DispatchRuntimeAuthUpdate(AuthUpdate{Action: AuthUpdateActionModify, ID: authID, Auth: persisted.Clone()})
		writeScanAuthFile(t, path, scanTestAuthJSONRewritten)
		return original(cfg, authDir)
	}
	w.refreshAuthState(false)
	snapshotCoreAuthsFunc = original

	updates := drainScanUpdates(w)
	if len(updates) != 1 {
		t.Fatalf("a scan finishing over a persisted snapshot dispatched %d updates, want 1: %+v", len(updates), updates)
	}
	republished := updates[0]
	if republished.Action != AuthUpdateActionModify || republished.Auth == nil {
		t.Fatalf("expected the persisted snapshot to win the scan, got %+v", republished)
	}
	if !republished.Auth.HasFileSnapshot() || !republished.Auth.SameFileSnapshot(persisted) {
		t.Fatalf("the scan dropped the persisted snapshot's file binding: snapshot=%v same=%v",
			republished.Auth.HasFileSnapshot(), republished.Auth.SameFileSnapshot(persisted))
	}

	// The manager refuses that observation because the file already moved on, and
	// asks for a re-read; the re-read carries the current content.
	w.rereadDiscardedFileSnapshot(republished.Auth)
	reread := drainScanUpdates(w)
	if len(reread) != 1 || reread[0].Auth == nil {
		t.Fatalf("discarded persisted snapshot was not re-read: %+v", reread)
	}
	if reread[0].Auth.SameFileSnapshot(persisted) {
		t.Fatalf("re-read returned the observation the manager discarded")
	}
	if reread[0].Auth.Prefix != "pfx-rewritten" {
		t.Fatalf("re-read did not carry the file's current content: prefix=%q", reread[0].Auth.Prefix)
	}
}
