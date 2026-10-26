package watcher

import (
	"sync"
	"testing"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// TestDispatchAuthUpdatesDropsStaleRevisions ensures an out-of-order delayed
// update cannot overwrite newer state in the pending queue.
func TestDispatchAuthUpdatesDropsStaleRevisions(t *testing.T) {
	w := &Watcher{}
	w.SetConfig(&config.Config{})
	w.authQueue = make(chan AuthUpdate, 4)

	w.clientsMutex.Lock()
	stale := AuthUpdate{Action: AuthUpdateActionModify, ID: "a", Auth: &coreauth.Auth{ID: "a"}}
	fresh := AuthUpdate{Action: AuthUpdateActionModify, ID: "a", Auth: &coreauth.Auth{ID: "a"}}
	batch := []AuthUpdate{stale, fresh}
	w.stampAuthUpdatesLocked(batch)
	stale, fresh = batch[0], batch[1]
	w.clientsMutex.Unlock()

	if stale.revision == 0 || fresh.revision == 0 || stale.revision >= fresh.revision {
		t.Fatalf("revisions not monotonic: stale=%d fresh=%d", stale.revision, fresh.revision)
	}

	w.dispatchAuthUpdates([]AuthUpdate{stale})
	if len(w.pendingUpdates) != 0 {
		t.Fatalf("stale update was enqueued: %v", w.pendingUpdates)
	}

	w.dispatchAuthUpdates([]AuthUpdate{fresh})
	if len(w.pendingUpdates) != 1 {
		t.Fatalf("fresh update was not enqueued: %v", w.pendingUpdates)
	}
	for _, update := range w.pendingUpdates {
		if update.revision != fresh.revision {
			t.Fatalf("enqueued revision = %d, want %d", update.revision, fresh.revision)
		}
	}
}

// TestRefreshAuthStateKeepsConcurrentDelete reproduces a full scan finishing
// after a file deletion: the stale scan must not resurrect the deleted auth.
func TestRefreshAuthStateKeepsConcurrentDelete(t *testing.T) {
	tmpDir := t.TempDir()
	w := &Watcher{authDir: tmpDir}
	w.SetConfig(&config.Config{AuthDir: tmpDir})

	const authPath = "/tmp/auth/keep.json"
	kept := &coreauth.Auth{ID: "keep", Attributes: map[string]string{"path": authPath}}
	w.clientsMutex.Lock()
	w.currentAuths = map[string]*coreauth.Auth{"keep": kept}
	w.clientsMutex.Unlock()

	release := make(chan struct{})
	origSnapshot := snapshotCoreAuthsFunc
	snapshotCoreAuthsFunc = func(cfg *config.Config, authDir string) ([]*coreauth.Auth, map[string]struct{}) {
		<-release
		// Stale view: still reports the deleted file.
		return []*coreauth.Auth{kept}, nil
	}
	defer func() { snapshotCoreAuthsFunc = origSnapshot }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.refreshAuthState(false)
	}()
	waitForCondition(t, func() bool {
		w.clientsMutex.RLock()
		defer w.clientsMutex.RUnlock()
		return w.activeAuthScans > 0
	}, "scan did not start")

	// Concurrent deletion lands while the scan is in flight.
	w.clientsMutex.Lock()
	w.fileAuthsByPath = map[string]map[string]*coreauth.Auth{
		w.normalizeAuthPath(authPath): {"keep": nil},
	}
	w.lastAuthHashes = map[string]string{w.normalizeAuthPath(authPath): "hash"}
	w.clientsMutex.Unlock()
	w.removeClient(authPath)
	close(release)
	wg.Wait()

	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	if _, ok := w.currentAuths["keep"]; ok {
		t.Fatal("stale scan resurrected concurrently deleted auth 'keep'")
	}
}

// TestRefreshAuthStateKeepsConcurrentAdd reproduces a full scan finishing
// after a runtime auth lands: the empty stale scan must not drop it.
func TestRefreshAuthStateKeepsConcurrentAdd(t *testing.T) {
	tmpDir := t.TempDir()
	w := &Watcher{authDir: tmpDir}
	w.SetConfig(&config.Config{AuthDir: tmpDir})

	release := make(chan struct{})
	origSnapshot := snapshotCoreAuthsFunc
	snapshotCoreAuthsFunc = func(cfg *config.Config, authDir string) ([]*coreauth.Auth, map[string]struct{}) {
		<-release
		return nil, nil
	}
	defer func() { snapshotCoreAuthsFunc = origSnapshot }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.refreshAuthState(false)
	}()
	waitForCondition(t, func() bool {
		w.clientsMutex.RLock()
		defer w.clientsMutex.RUnlock()
		return w.activeAuthScans > 0
	}, "scan did not start")

	added := &coreauth.Auth{ID: "new", Attributes: map[string]string{"path": "/tmp/auth/new.json"}}
	w.DispatchRuntimeAuthUpdate(AuthUpdate{Action: AuthUpdateActionAdd, Auth: added})
	close(release)
	wg.Wait()

	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	if _, ok := w.currentAuths["new"]; !ok {
		t.Fatal("stale empty scan dropped concurrently added auth 'new'")
	}
}

func waitForCondition(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", msg)
}
