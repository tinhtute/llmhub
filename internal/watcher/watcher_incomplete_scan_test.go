package watcher

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

const (
	scanTestAuthJSON          = `{"type":"codex","prefix":"pfx"}`
	scanTestAuthJSONRewritten = `{"type":"codex","prefix":"pfx-rewritten"}`
)

// scanTestWatcher builds the smallest watcher that can run the full-scan entry
// point, so the tests observe exactly what the service consumes from it.
func scanTestWatcher(t *testing.T, authDir string) *Watcher {
	t.Helper()
	return &Watcher{
		authDir:         authDir,
		config:          &config.Config{AuthDir: authDir},
		authQueue:       make(chan AuthUpdate, 64),
		lastAuthHashes:  make(map[string]string),
		fileAuthsByPath: make(map[string]map[string]*coreauth.Auth),
	}
}

// drainScanUpdates takes everything the watcher tried to dispatch. Reading the
// pending batch directly keeps the assertions free of the dispatch goroutine.
func drainScanUpdates(w *Watcher) []AuthUpdate {
	w.dispatchMu.Lock()
	defer w.dispatchMu.Unlock()
	updates := make([]AuthUpdate, 0, len(w.pendingOrder))
	for _, key := range w.pendingOrder {
		updates = append(updates, w.pendingUpdates[key])
		delete(w.pendingUpdates, key)
	}
	w.pendingOrder = w.pendingOrder[:0]
	return updates
}

func writeScanAuthFile(t *testing.T, path, data string) {
	t.Helper()
	if errWrite := os.WriteFile(path, []byte(data), 0o600); errWrite != nil {
		t.Fatalf("write %s: %v", path, errWrite)
	}
}

// breakScanFileRead replaces path with a self-referential symlink. Reading it
// fails with ELOOP, which is not fs.ErrNotExist: the file is unreadable, not
// absent. A chmod fixture cannot express this because the tests may run as root.
func breakScanFileRead(t *testing.T, path string) {
	t.Helper()
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatalf("remove %s: %v", path, errRemove)
	}
	if errSymlink := os.Symlink(filepath.Base(path), path); errSymlink != nil {
		t.Fatalf("symlink %s: %v", path, errSymlink)
	}
}

// breakScanDirRead replaces dir with a self-referential symlink so that listing
// it fails with ELOOP instead of fs.ErrNotExist.
func breakScanDirRead(t *testing.T, dir string) {
	t.Helper()
	if errRename := os.Rename(dir, dir+".away"); errRename != nil {
		t.Fatalf("rename %s: %v", dir, errRename)
	}
	if errSymlink := os.Symlink(dir, dir); errSymlink != nil {
		t.Fatalf("symlink %s: %v", dir, errSymlink)
	}
}

func deletesFor(updates []AuthUpdate, id string) []AuthUpdate {
	var deletes []AuthUpdate
	for _, update := range updates {
		if update.Action == AuthUpdateActionDelete && update.ID == id {
			deletes = append(deletes, update)
		}
	}
	return deletes
}

// A read error during a full scan must not be read as "the file is gone": the
// credential that is still backed by a present file keeps its record and no
// deletion is dispatched for it.
func TestRefreshAuthStatePreservesCredentialOnReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	w.addOrUpdateClient(path)
	drainScanUpdates(w)

	breakScanFileRead(t, path)
	w.refreshAuthState(false)
	if deletes := deletesFor(drainScanUpdates(w), "account.json"); len(deletes) != 0 {
		t.Fatalf("read error during a full scan deleted the credential: %+v", deletes)
	}

	// The file is still unreadable: repeated scans must stay quiet instead of
	// deleting on the second pass.
	w.refreshAuthState(false)
	if deletes := deletesFor(drainScanUpdates(w), "account.json"); len(deletes) != 0 {
		t.Fatalf("second scan of an unreadable file deleted the credential: %+v", deletes)
	}

	// The file becomes readable again with unchanged content: no update churn.
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatalf("remove symlink: %v", errRemove)
	}
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w.refreshAuthState(false)
	if updates := drainScanUpdates(w); len(updates) != 0 {
		t.Fatalf("recovered file with unchanged content churned the auth state: %+v", updates)
	}
}

// An auth directory that cannot be listed must not erase credentials: the scan
// cannot tell "no auth files" from "could not look", so nothing is deleted.
func TestRefreshAuthStatePreservesCredentialOnUnreadableAuthDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	w.addOrUpdateClient(path)
	drainScanUpdates(w)

	breakScanDirRead(t, dir)
	w.refreshAuthState(false)
	if deletes := deletesFor(drainScanUpdates(w), "account.json"); len(deletes) != 0 {
		t.Fatalf("unreadable auth directory erased the credential: %+v", deletes)
	}
}

// The other direction: a file that is genuinely absent after a successful scan
// still deletes, including after an earlier scan that had to preserve it.
func TestRefreshAuthStateDeletesConfirmedRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	w.addOrUpdateClient(path)
	drainScanUpdates(w)

	// A readable scan that no longer lists the file is a confirmed removal.
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatalf("remove %s: %v", path, errRemove)
	}
	w.refreshAuthState(false)
	if deletes := deletesFor(drainScanUpdates(w), "account.json"); len(deletes) != 1 {
		t.Fatalf("confirmed removal did not delete the credential: %+v", deletes)
	}

	// Preservation is not sticky: an unreadable scan preserves, and the next
	// scan that can read the directory and cannot find the file deletes.
	writeScanAuthFile(t, path, scanTestAuthJSONRewritten)
	w.addOrUpdateClient(path)
	drainScanUpdates(w)
	breakScanFileRead(t, path)
	w.refreshAuthState(false)
	if deletes := deletesFor(drainScanUpdates(w), "account.json"); len(deletes) != 0 {
		t.Fatalf("unreadable file deleted the credential: %+v", deletes)
	}
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatalf("remove symlink: %v", errRemove)
	}
	w.refreshAuthState(false)
	if deletes := deletesFor(drainScanUpdates(w), "account.json"); len(deletes) != 1 {
		t.Fatalf("removal after a preserved scan did not delete the credential: %+v", deletes)
	}
}

// A record preserved by an unreadable scan keeps its file index, so the
// file-removal path can still delete it once the file really goes away.
func TestRefreshAuthStatePreservesFileIndexForUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	w.addOrUpdateClient(path)
	drainScanUpdates(w)

	breakScanFileRead(t, path)
	w.refreshAuthState(false)
	drainScanUpdates(w)

	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatalf("remove symlink: %v", errRemove)
	}
	w.removeClient(path)
	deletes := deletesFor(drainScanUpdates(w), "account.json")
	if len(deletes) != 1 || !deletes[0].IsFileRemoval() {
		t.Fatalf("file removal after a preserved scan did not delete the credential: %+v", deletes)
	}
}

// A partially written or empty auth file is present, not absent: in-place
// writes must not delete the credential that the file still carries.
func TestRefreshAuthStatePreservesCredentialOnPartialJSON(t *testing.T) {
	for _, partial := range []string{`{"type":"codex","pre`, ""} {
		dir := t.TempDir()
		path := filepath.Join(dir, "account.json")
		writeScanAuthFile(t, path, scanTestAuthJSON)
		w := scanTestWatcher(t, dir)
		w.addOrUpdateClient(path)
		drainScanUpdates(w)

		writeScanAuthFile(t, path, partial)
		w.refreshAuthState(false)
		if deletes := deletesFor(drainScanUpdates(w), "account.json"); len(deletes) != 0 {
			t.Fatalf("partial file %q deleted the credential: %+v", partial, deletes)
		}
	}
}

// A full scan running while a file event lands must never drop a credential
// whose file is still present. Run with -race.
func TestRefreshAuthStateConcurrentFileUpdateUnderUnreadableScan(t *testing.T) {
	dir := t.TempDir()
	unreadablePath := filepath.Join(dir, "account.json")
	rewrittenPath := filepath.Join(dir, "second.json")
	writeScanAuthFile(t, unreadablePath, scanTestAuthJSON)
	writeScanAuthFile(t, rewrittenPath, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	w.addOrUpdateClient(unreadablePath)
	w.addOrUpdateClient(rewrittenPath)
	drainScanUpdates(w)
	breakScanFileRead(t, unreadablePath)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			w.refreshAuthState(false)
		})
	}
	wg.Go(func() {
		for range 10 {
			if errWrite := os.WriteFile(rewrittenPath, []byte(scanTestAuthJSON), 0o600); errWrite != nil {
				t.Errorf("rewrite %s: %v", rewrittenPath, errWrite)
				return
			}
			w.addOrUpdateClient(rewrittenPath)
		}
	})
	wg.Wait()

	const preservedID = "account.json"
	const rewrittenID = "second.json"
	// Drain once: a drain per id consumes the whole batch on the first call and
	// leaves the later ids checked against nothing.
	assertNoDelete := func(updates []AuthUpdate, stage string) {
		t.Helper()
		for _, id := range []string{preservedID, rewrittenID} {
			if deletes := deletesFor(updates, id); len(deletes) != 0 {
				t.Fatalf("%s deleted the credential %s while its file was present: %+v", stage, id, deletes)
			}
		}
	}
	assertNoDelete(drainScanUpdates(w), "a concurrent scan")
	w.refreshAuthState(false)
	assertNoDelete(drainScanUpdates(w), "a scan after the concurrent run")
}

// The single-file path is the import and delete-check path: an unreadable file
// must keep failing there instead of reporting an empty (absent) result, which
// is what lets the caller treat the read as uncertain.
func TestSnapshotAuthFileFailsOnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	if auths, errReadable := w.SnapshotAuthFile(path); errReadable != nil || len(auths) != 1 {
		t.Fatalf("readable file snapshot: auths=%d err=%v", len(auths), errReadable)
	}

	breakScanFileRead(t, path)
	if _, errUnreadable := w.SnapshotAuthFile(path); errUnreadable == nil {
		t.Fatalf("unreadable file snapshot reported success instead of an error")
	}
}

// A dangling symlink is a listed entry whose target is gone: reading it fails with
// fs.ErrNotExist, which is positive evidence that no credential content is there.
// The credential must be deleted, exactly as for a file that was removed.
func TestRefreshAuthStateDeletesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	// The target lives outside the watched directory, so no event reaches removeClient.
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "target.json")
	writeScanAuthFile(t, target, scanTestAuthJSON)
	path := filepath.Join(dir, "account.json")
	if errSymlink := os.Symlink(target, path); errSymlink != nil {
		t.Fatalf("symlink auth file: %v", errSymlink)
	}
	w := scanTestWatcher(t, dir)
	w.addOrUpdateClient(path)
	drainScanUpdates(w)
	if w.currentAuths["account.json"] == nil {
		t.Fatalf("precondition: the symlinked auth file was not loaded")
	}

	if errRemove := os.Remove(target); errRemove != nil {
		t.Fatalf("remove symlink target: %v", errRemove)
	}
	w.refreshAuthState(false)
	if deletes := deletesFor(drainScanUpdates(w), "account.json"); len(deletes) != 1 {
		t.Fatalf("a dangling symlink did not delete the credential: %+v", deletes)
	}
}
