package watcher

import (
	"path/filepath"
	"testing"
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
