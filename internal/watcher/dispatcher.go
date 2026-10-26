// dispatcher.go implements auth update dispatching and queue management.
// It batches, deduplicates, and delivers auth updates to registered consumers.
package watcher

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/watcher/synthesizer"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

var snapshotCoreAuthsFunc = snapshotCoreAuthsWithIncomplete

func (w *Watcher) setAuthUpdateQueue(queue chan<- AuthUpdate) {
	w.clientsMutex.Lock()
	defer w.clientsMutex.Unlock()
	w.authQueue = queue
	if w.dispatchCond == nil {
		w.dispatchCond = sync.NewCond(&w.dispatchMu)
	}
	if w.dispatchCancel != nil {
		w.dispatchCancel()
		if w.dispatchCond != nil {
			w.dispatchMu.Lock()
			w.dispatchCond.Broadcast()
			w.dispatchMu.Unlock()
		}
		w.dispatchCancel = nil
	}
	if queue != nil {
		ctx, cancel := context.WithCancel(context.Background())
		w.dispatchCancel = cancel
		go w.dispatchLoop(ctx)
	}
}

func (w *Watcher) dispatchRuntimeAuthUpdate(update AuthUpdate) bool {
	if w == nil {
		return false
	}
	w.clientsMutex.Lock()
	if w.runtimeAuths == nil {
		w.runtimeAuths = make(map[string]*coreauth.Auth)
	}
	switch update.Action {
	case AuthUpdateActionAdd, AuthUpdateActionModify:
		if update.Auth != nil && update.Auth.ID != "" {
			clone := update.Auth.Clone()
			w.runtimeAuths[clone.ID] = clone
			if w.currentAuths == nil {
				w.currentAuths = make(map[string]*coreauth.Auth)
			}
			w.currentAuths[clone.ID] = clone.Clone()
		}
	case AuthUpdateActionDelete:
		id := update.ID
		if id == "" && update.Auth != nil {
			id = update.Auth.ID
		}
		if id != "" {
			delete(w.runtimeAuths, id)
			if w.currentAuths != nil {
				delete(w.currentAuths, id)
			}
		}
	}
	updates := []AuthUpdate{update}
	w.stampAuthUpdatesLocked(updates)
	w.clientsMutex.Unlock()
	if w.getAuthQueue() == nil {
		return false
	}
	w.dispatchAuthUpdates(updates)
	return true
}

func (w *Watcher) refreshAuthState(force bool) {
	w.clientsMutex.Lock()
	w.activeAuthScans++
	cfg := w.config
	authDir := w.authDir
	previous := maps.Clone(w.authRevisions)
	previousFiles := maps.Clone(w.fileObservations)
	w.clientsMutex.Unlock()
	auths, incompleteFiles := snapshotCoreAuthsFunc(cfg, authDir)
	w.clientsMutex.Lock()
	w.activeAuthScans--
	incompletePaths := w.incompleteScanPathsLocked(incompleteFiles, previousFiles)
	authRawPath := func(auth *coreauth.Auth) string {
		if auth == nil || auth.Attributes == nil {
			return ""
		}
		path := auth.Attributes[coreauth.AttributePath]
		if path == "" {
			path = auth.Attributes[coreauth.AttributeSource]
		}
		return path
	}
	authPath := func(auth *coreauth.Auth) string {
		return w.normalizeAuthPath(authRawPath(auth))
	}
	incompletePathOrDir := func(rawPath string, checkDir bool) bool {
		if len(incompletePaths) == 0 || strings.TrimSpace(rawPath) == "" {
			return false
		}
		target := rawPath
		if checkDir {
			target = filepath.Dir(rawPath)
		}
		normalized := w.normalizeAuthPath(target)
		if normalized == "" {
			return false
		}
		if _, ok := incompletePaths[normalized]; ok {
			return true
		}
		abs, errAbs := filepath.Abs(normalized)
		if errAbs != nil {
			return false
		}
		_, ok := incompletePaths[w.normalizeAuthPath(abs)]
		return ok
	}
	// A full scan may finish after a newer file update landed. Keep those
	// concurrent changes, including deletions, instead of publishing stale
	// scan data over them.
	changedDuringScan := func(auth *coreauth.Auth) bool {
		if auth == nil {
			return false
		}
		normalized := authPath(auth)
		return previous[auth.ID] != w.authRevisions[auth.ID] ||
			previousFiles[normalized] != w.fileObservations[normalized]
	}
	// A file the scan could not read, or an auth directory it could not list,
	// cannot prove that the credentials backed by it are gone.
	incompleteDuringScan := func(auth *coreauth.Auth) bool {
		if auth == nil {
			return false
		}
		rawPath := authRawPath(auth)
		if incompletePathOrDir(rawPath, false) {
			return true
		}
		// An unreadable auth directory covers every file inside it.
		return incompletePathOrDir(rawPath, true)
	}
	for index, auth := range auths {
		if auth != nil && (changedDuringScan(auth) || incompleteDuringScan(auth)) {
			auths[index] = nil
		}
	}
	for _, auth := range w.currentAuths {
		if auth == nil || (!changedDuringScan(auth) && !incompleteDuringScan(auth)) {
			continue
		}
		// Keep the record exactly as it was last understood: no delete, and no
		// modify either, because the reload produced no new content for it.
		auths = append(auths, auth.Clone())
	}
	if len(w.runtimeAuths) > 0 {
		for _, a := range w.runtimeAuths {
			if a != nil {
				auths = append(auths, a.Clone())
			}
		}
	}
	updates := w.prepareAuthUpdatesLocked(auths, force)
	w.clientsMutex.Unlock()
	w.dispatchAuthUpdates(updates)
}

// incompleteScanPathsLocked normalises the paths a scan could not read and drops
// their cached hashes so the next event re-reads them instead of hash-skipping.
// Callers hold clientsMutex.
func (w *Watcher) incompleteScanPathsLocked(files map[string]struct{}, previousFiles map[string]uint64) map[string]struct{} {
	if len(files) == 0 {
		return nil
	}
	paths := make(map[string]struct{}, len(files))
	for path := range files {
		normalized := w.normalizeAuthPath(path)
		if normalized == "" {
			continue
		}
		paths[normalized] = struct{}{}
		abs := ""
		if absolutePath, errAbs := filepath.Abs(normalized); errAbs == nil {
			abs = w.normalizeAuthPath(absolutePath)
			if abs != "" {
				paths[abs] = struct{}{}
			}
		}
		// Only when no file event landed during the scan: an event already forced
		// its own re-read and must keep the hash it recorded.
		quiet := previousFiles[normalized] == w.fileObservations[normalized] &&
			(abs == "" || previousFiles[abs] == w.fileObservations[abs])
		if !quiet {
			continue
		}
		if _, known := w.lastAuthHashes[normalized]; known {
			w.lastAuthHashes[normalized] = ""
		}
		if abs != "" {
			if _, known := w.lastAuthHashes[abs]; known {
				w.lastAuthHashes[abs] = ""
			}
		}
	}
	return paths
}

// indexFileBackedAuthLocked keeps the file -> auth ID index current for a record
// the reconciler retains, so the file-removal path can still find and delete it.
// Callers hold clientsMutex.
func (w *Watcher) indexFileBackedAuthLocked(auth *coreauth.Auth) {
	if auth == nil || auth.ID == "" || len(auth.Attributes) == 0 {
		return
	}
	path := auth.Attributes[coreauth.AttributePath]
	if path == "" {
		path = auth.Attributes[coreauth.AttributeSource]
	}
	normalized := w.normalizeAuthPath(path)
	if normalized == "" || strings.HasPrefix(normalized, "config:") {
		return
	}
	if w.fileAuthsByPath == nil {
		w.fileAuthsByPath = make(map[string]map[string]*coreauth.Auth)
	}
	pathAuths := w.fileAuthsByPath[normalized]
	if pathAuths == nil {
		pathAuths = make(map[string]*coreauth.Auth)
		w.fileAuthsByPath[normalized] = pathAuths
	}
	pathAuths[auth.ID] = nil
	if abs, errAbs := filepath.Abs(normalized); errAbs == nil {
		if absNormalized := w.normalizeAuthPath(abs); absNormalized != "" && absNormalized != normalized {
			if aliasAuths := w.fileAuthsByPath[absNormalized]; aliasAuths != nil {
				aliasAuths[auth.ID] = nil
			}
		}
	}
}

func (w *Watcher) prepareAuthUpdatesLocked(auths []*coreauth.Auth, force bool) []AuthUpdate {
	newState := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		cloned := auth.Clone()
		newState[auth.ID] = cloned
		w.indexFileBackedAuthLocked(cloned)
	}
	updates := make([]AuthUpdate, 0, len(newState)+len(w.currentAuths))
	for id, auth := range newState {
		if existing, ok := w.currentAuths[id]; !ok {
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionAdd, ID: id, Auth: auth.Clone()})
		} else if force || !authEqual(existing, auth) {
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionModify, ID: id, Auth: auth.Clone()})
		}
	}
	// A missing ID in a full scan does not imply its source file was removed.
	// Leave these deletes unmarked so content/config changes still remove auths.
	for id := range w.currentAuths {
		if _, ok := newState[id]; !ok {
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionDelete, ID: id})
		}
	}
	// A path that no longer backs any retained auth must not keep an index entry,
	// or a later file-removal event would delete a record under a stale path.
	for path, pathAuths := range w.fileAuthsByPath {
		for id := range pathAuths {
			if _, ok := newState[id]; !ok {
				delete(pathAuths, id)
			}
		}
		if len(pathAuths) == 0 {
			delete(w.fileAuthsByPath, path)
		}
	}
	w.currentAuths = newState
	w.stampAuthUpdatesLocked(updates)
	if w.authQueue == nil {
		return nil
	}
	return updates
}

// stampAuthUpdatesLocked records observations even without a queue and retains
// deletion revisions so a scan cannot resurrect an auth that appeared then vanished.
func (w *Watcher) stampAuthUpdatesLocked(updates []AuthUpdate) {
	if len(updates) == 0 {
		return
	}
	if w.authRevisions == nil {
		w.authRevisions = make(map[string]uint64)
	}
	for index := range updates {
		update := &updates[index]
		if update.ID == "" && update.Auth != nil {
			update.ID = update.Auth.ID
		}
		if update.ID == "" {
			continue
		}
		w.authRevisions[update.ID]++
		update.revision = w.authRevisions[update.ID]
	}
}

func (w *Watcher) dispatchAuthUpdates(updates []AuthUpdate) {
	if len(updates) == 0 {
		return
	}
	// Filter stale revisions first, then enqueue. The two steps deliberately
	// use separate critical sections: holding clientsMutex across the
	// dispatchMu enqueue would invert the lock order used by
	// setAuthUpdateQueue/stopDispatch (clientsMutex -> dispatchMu) and risk a
	// shutdown-time deadlock. Every state change stamps and enqueues its own
	// newer update immediately, so a newer revision always follows and wins
	// the per-ID coalescing below even in the residual interleaving window.
	w.clientsMutex.RLock()
	queue := w.authQueue
	filtered := make([]AuthUpdate, 0, len(updates))
	for _, update := range updates {
		if update.revision != 0 && update.revision < w.authRevisions[update.ID] {
			continue
		}
		filtered = append(filtered, update)
	}
	w.clientsMutex.RUnlock()
	if queue == nil || len(filtered) == 0 {
		return
	}
	baseTS := time.Now().UnixNano()
	w.dispatchMu.Lock()
	if w.pendingUpdates == nil {
		w.pendingUpdates = make(map[string]AuthUpdate)
	}
	for idx, update := range filtered {
		key := w.authUpdateKey(update, baseTS+int64(idx))
		if _, exists := w.pendingUpdates[key]; !exists {
			w.pendingOrder = append(w.pendingOrder, key)
		}
		w.pendingUpdates[key] = update
	}
	if w.dispatchCond != nil {
		w.dispatchCond.Signal()
	}
	w.dispatchMu.Unlock()
}

func (w *Watcher) authUpdateKey(update AuthUpdate, ts int64) string {
	if update.ID != "" {
		return update.ID
	}
	return fmt.Sprintf("%s:%d", update.Action, ts)
}

func (w *Watcher) dispatchLoop(ctx context.Context) {
	for {
		batch, ok := w.nextPendingBatch(ctx)
		if !ok {
			return
		}
		queue := w.getAuthQueue()
		if queue == nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for _, update := range batch {
			select {
			case queue <- update:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (w *Watcher) nextPendingBatch(ctx context.Context) ([]AuthUpdate, bool) {
	w.dispatchMu.Lock()
	defer w.dispatchMu.Unlock()
	for len(w.pendingOrder) == 0 {
		if ctx.Err() != nil {
			return nil, false
		}
		w.dispatchCond.Wait()
		if ctx.Err() != nil {
			return nil, false
		}
	}
	batch := make([]AuthUpdate, 0, len(w.pendingOrder))
	for _, key := range w.pendingOrder {
		batch = append(batch, w.pendingUpdates[key])
		delete(w.pendingUpdates, key)
	}
	w.pendingOrder = w.pendingOrder[:0]
	return batch, true
}

func (w *Watcher) getAuthQueue() chan<- AuthUpdate {
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	return w.authQueue
}

func (w *Watcher) stopDispatch() {
	if w.dispatchCancel != nil {
		w.dispatchCancel()
		w.dispatchCancel = nil
	}
	w.dispatchMu.Lock()
	w.pendingOrder = nil
	w.pendingUpdates = nil
	if w.dispatchCond != nil {
		w.dispatchCond.Broadcast()
	}
	w.dispatchMu.Unlock()
	w.clientsMutex.Lock()
	w.authQueue = nil
	w.clientsMutex.Unlock()
}

func authEqual(a, b *coreauth.Auth) bool {
	if a != nil && b != nil && a.HasFileSnapshot() && b.HasFileSnapshot() && !a.SameFileSnapshot(b) {
		return false
	}
	return reflect.DeepEqual(normalizeAuth(a), normalizeAuth(b))
}

func normalizeAuth(a *coreauth.Auth) *coreauth.Auth {
	if a == nil {
		return nil
	}
	clone := a.Clone()
	clone.ClearFileSnapshot()
	clone.CreatedAt = time.Time{}
	clone.UpdatedAt = time.Time{}
	clone.LastRefreshedAt = time.Time{}
	clone.NextRefreshAfter = time.Time{}
	clone.Runtime = nil
	clone.Quota.NextRecoverAt = time.Time{}
	return clone
}

func snapshotCoreAuths(cfg *config.Config, authDir string) []*coreauth.Auth {
	auths, _ := snapshotCoreAuthsWithIncomplete(cfg, authDir)
	return auths
}

// snapshotCoreAuthsWithIncomplete also returns what the scan could not read, so
// the reconciler can tell "no auth file here" from "could not look".
func snapshotCoreAuthsWithIncomplete(cfg *config.Config, authDir string) ([]*coreauth.Auth, map[string]struct{}) {
	ctx := &synthesizer.SynthesisContext{
		Config:      cfg,
		AuthDir:     authDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}

	var out []*coreauth.Auth

	configSynth := synthesizer.NewConfigSynthesizer()
	if auths, err := configSynth.Synthesize(ctx); err == nil {
		out = append(out, auths...)
	}

	fileSynth := synthesizer.NewFileSynthesizer()
	if auths, err := fileSynth.Synthesize(ctx); err == nil {
		out = append(out, auths...)
	}

	return out, ctx.IncompleteFiles
}
