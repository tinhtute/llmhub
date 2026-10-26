package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

// A config reload rescans the auth directory. When that rescan cannot read a
// file it must keep the path known: otherwise a later removal event for that
// file is ignored as "unknown auth file" and the credential is never removed.
func TestReloadClientsRetainsKnownFilesForUnreadableAuthDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	writeScanAuthFile(t, path, scanTestAuthJSON)
	w := scanTestWatcher(t, dir)
	w.reloadClients(true, nil, false)
	drainScanUpdates(w)

	breakScanDirRead(t, dir)
	w.reloadClients(true, nil, false)
	drainScanUpdates(w)

	// The directory is readable again and the file is genuinely gone: the
	// removal must still reach the consumer.
	restoreScanDir(t, dir)
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatalf("remove %s: %v", path, errRemove)
	}
	w.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Remove})
	deletes := deletesFor(drainScanUpdates(w), "account.json")
	if len(deletes) != 1 || !deletes[0].IsFileRemoval() {
		t.Fatalf("removal after an unreadable rescan was not reconciled: %+v", deletes)
	}
}

// restoreScanDir puts the real directory back where breakScanDirRead hid it.
func restoreScanDir(t *testing.T, dir string) {
	t.Helper()
	if errRemove := os.Remove(dir); errRemove != nil {
		t.Fatalf("remove directory symlink: %v", errRemove)
	}
	if errRename := os.Rename(dir+".away", dir); errRename != nil {
		t.Fatalf("restore %s: %v", dir, errRename)
	}
}
