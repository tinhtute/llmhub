// clients.go implements watcher client lifecycle logic and persistence helpers.
// It reloads clients, handles incremental auth file changes, and persists updates when supported.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/therealtinhtute/llmhub/internal/watcher/diff"
	"github.com/therealtinhtute/llmhub/internal/watcher/synthesizer"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

func (w *Watcher) reloadClients(rescanAuth bool, affectedOAuthProviders []string, forceAuthRefresh bool) {
	log.Debugf("starting full client load process")

	w.clientsMutex.RLock()
	cfg := w.config
	w.clientsMutex.RUnlock()

	if cfg == nil {
		log.Error("config is nil, cannot reload clients")
		return
	}

	if len(affectedOAuthProviders) > 0 {
		w.clientsMutex.Lock()
		if w.currentAuths != nil {
			filtered := make(map[string]*coreauth.Auth, len(w.currentAuths))
			for id, auth := range w.currentAuths {
				if auth == nil {
					continue
				}
				provider := strings.ToLower(strings.TrimSpace(auth.Provider))
				if _, match := matchProvider(provider, affectedOAuthProviders); match {
					continue
				}
				filtered[id] = auth
			}
			w.currentAuths = filtered
			log.Debugf("applying oauth-excluded-models to providers %v", affectedOAuthProviders)
		} else {
			w.currentAuths = nil
		}
		w.clientsMutex.Unlock()
	}

	geminiAPIKeyCount, vertexCompatAPIKeyCount, claudeAPIKeyCount, codexAPIKeyCount, metaAPIKeyCount, openAICompatCount := BuildAPIKeyClients(cfg)
	totalAPIKeyClients := geminiAPIKeyCount + vertexCompatAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + metaAPIKeyCount + openAICompatCount
	log.Debugf("loaded %d API key clients", totalAPIKeyClients)

	var authFileCount int
	if rescanAuth {
		authFileCount = w.loadFileClients(cfg)
		log.Debugf("loaded %d file-based clients", authFileCount)
	} else {
		w.clientsMutex.RLock()
		authFileCount = len(w.lastAuthHashes)
		w.clientsMutex.RUnlock()
		log.Debugf("skipping auth directory rescan; retaining %d existing auth files", authFileCount)
	}

	if rescanAuth {
		// Serialize the full rescan against incremental event handling so
		// per-file updates cannot slip between the directory read and the
		// hash-cache swap below.
		w.authRescanMu.Lock()
		w.clientsMutex.Lock()

		// A rescan can only describe the files it could read. Keep the previous
		// index and hashes for every path it could not, so an unreadable file or
		// directory does not look like a removal.
		previousByPath := make(map[string]map[string]*coreauth.Auth, len(w.fileAuthsByPath))
		for path, pathAuths := range w.fileAuthsByPath {
			previousByPath[path] = pathAuths
		}
		var incompleteRescanPaths map[string]struct{}
		markRescanIncomplete := func(path string) {
			normalized := w.normalizeAuthPath(path)
			if normalized == "" {
				return
			}
			if incompleteRescanPaths == nil {
				incompleteRescanPaths = make(map[string]struct{})
			}
			incompleteRescanPaths[normalized] = struct{}{}
		}

		w.lastAuthHashes = make(map[string]string)
		cacheAuthContents := log.IsLevelEnabled(log.DebugLevel)
		if cacheAuthContents {
			w.lastAuthContents = make(map[string]*coreauth.Auth)
		} else {
			w.lastAuthContents = nil
		}
		w.fileAuthsByPath = make(map[string]map[string]*coreauth.Auth)
		if resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir); errResolveAuthDir != nil {
			log.Errorf("failed to resolve auth directory for hash cache: %v", errResolveAuthDir)
		} else if resolvedAuthDir != "" {
			entries, errReadDir := os.ReadDir(resolvedAuthDir)
			if errReadDir != nil {
				log.Errorf("failed to read auth directory for hash cache: %v", errReadDir)
				// A missing directory holds no auths; any other failure means the
				// rescan could not look, so the paths it tracked stay known.
				if !errors.Is(errReadDir, fs.ErrNotExist) {
					for trackedPath := range previousByPath {
						if w.pathIsInDir(trackedPath, resolvedAuthDir) {
							markRescanIncomplete(trackedPath)
						}
					}
				}
			} else {
				for _, entry := range entries {
					if entry == nil || entry.IsDir() {
						continue
					}
					name := entry.Name()
					if !strings.HasSuffix(strings.ToLower(name), ".json") {
						continue
					}
					fullPath := filepath.Join(resolvedAuthDir, name)
					data, errReadFile := os.ReadFile(fullPath)
					if errReadFile != nil {
						// Unreadable keeps the path known; fs.ErrNotExist does not,
						// because it is positive evidence that the content is gone.
						if !errors.Is(errReadFile, fs.ErrNotExist) {
							markRescanIncomplete(fullPath)
						}
					} else if len(data) > 0 {
						sum := sha256.Sum256(data)
						normalizedPath := w.normalizeAuthPath(fullPath)
						w.lastAuthHashes[normalizedPath] = hex.EncodeToString(sum[:])
						// Parse and cache auth content for future diff comparisons (debug only).
						if cacheAuthContents {
							var auth coreauth.Auth
							if errParse := json.Unmarshal(data, &auth); errParse == nil {
								w.lastAuthContents[normalizedPath] = &auth
							}
						}
						ctx := &synthesizer.SynthesisContext{
							Config:      cfg,
							AuthDir:     resolvedAuthDir,
							Now:         time.Now(),
							IDGenerator: synthesizer.NewStableIDGenerator(),
						}
						if generated, errSynth := synthesizer.SynthesizeAuthFile(ctx, fullPath, data); errSynth == nil && len(generated) > 0 {
							if pathAuths := authSliceToMap(generated); len(pathAuths) > 0 {
								w.fileAuthsByPath[normalizedPath] = authIDSet(pathAuths)
							}
						}
					}
				}
			}
		}
		// Restore what the rescan could not see: the hash stays empty so the next
		// event re-reads the file, and the file -> ID index survives.
		for path := range incompleteRescanPaths {
			if _, known := w.lastAuthHashes[path]; !known {
				w.lastAuthHashes[path] = ""
			}
			if _, indexed := w.fileAuthsByPath[path]; !indexed {
				if previous := previousByPath[path]; previous != nil {
					w.fileAuthsByPath[path] = previous
				}
			}
		}
		w.clientsMutex.Unlock()
		w.authRescanMu.Unlock()
	}

	totalNewClients := authFileCount + geminiAPIKeyCount + vertexCompatAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + metaAPIKeyCount + openAICompatCount

	if w.reloadCallback != nil {
		log.Debugf("triggering server update callback before auth refresh")
		w.reloadCallback(cfg)
	}

	w.refreshAuthState(forceAuthRefresh)

	log.Infof("full client load complete - %d clients (%d auth files + %d Gemini API keys + %d Vertex API keys + %d Claude API keys + %d Codex keys + %d Meta API keys + %d OpenAI-compat)",
		totalNewClients,
		authFileCount,
		geminiAPIKeyCount,
		vertexCompatAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		metaAPIKeyCount,
		openAICompatCount,
	)
}

// pathIsInDir reports whether path's parent directory is dir, comparing both the
// raw and absolute forms because the watcher tracks auth paths in either.
func (w *Watcher) pathIsInDir(path, dir string) bool {
	parent := w.normalizeAuthPath(filepath.Dir(path))
	target := w.normalizeAuthPath(dir)
	if parent == "" || target == "" {
		return false
	}
	if parent == target {
		return true
	}
	absParent, errParent := filepath.Abs(parent)
	absTarget, errTarget := filepath.Abs(target)
	if errParent != nil || errTarget != nil {
		return false
	}
	return w.normalizeAuthPath(absParent) == w.normalizeAuthPath(absTarget)
}

// invalidateAuthFileHashLocked forgets the recorded hash for path so the next
// read is a real re-read instead of a hash skip. Callers hold clientsMutex.
func (w *Watcher) invalidateAuthFileHashLocked(path string) string {
	normalized := w.normalizeAuthPath(path)
	if normalized == "" {
		return ""
	}
	if w.lastAuthHashes == nil {
		w.lastAuthHashes = make(map[string]string)
	}
	w.lastAuthHashes[normalized] = ""
	abs, errAbs := filepath.Abs(normalized)
	if errAbs != nil {
		return normalized
	}
	absNormalized := w.normalizeAuthPath(abs)
	if absNormalized != "" && absNormalized != normalized {
		if _, known := w.lastAuthHashes[absNormalized]; known {
			w.lastAuthHashes[absNormalized] = ""
		}
	}
	return normalized
}

// rereadDiscardedFileSnapshot re-reads the file behind a snapshot the manager
// discarded, after invalidating what the watcher believes it already published.
// Invalidating the digest is what terminates a discard/re-read cycle: the same
// discarded observation can never match the stored record again, so it is read
// at most once.
func (w *Watcher) rereadDiscardedFileSnapshot(auth *coreauth.Auth) {
	if w == nil || auth == nil || !auth.HasFileSnapshot() {
		return
	}
	path := auth.FileSnapshotPath()
	if path == "" {
		return
	}
	w.authRescanMu.Lock()
	defer w.authRescanMu.Unlock()

	w.clientsMutex.Lock()
	existing := w.currentAuths[auth.ID]
	if existing == nil || !existing.SameFileSnapshot(auth) {
		w.clientsMutex.Unlock()
		return
	}
	normalized := w.invalidateAuthFileHashLocked(path)
	if normalized == "" {
		w.clientsMutex.Unlock()
		return
	}
	existing.InvalidateFileSnapshot()
	for id := range w.fileAuthsByPath[normalized] {
		if sibling := w.currentAuths[id]; sibling != nil && sibling.SameFileSnapshot(auth) {
			sibling.InvalidateFileSnapshot()
		}
	}
	w.clientsMutex.Unlock()

	w.addOrUpdateClient(path)
}

func (w *Watcher) addOrUpdateClient(path string) {
	// Record the observation before reading: a concurrent full scan must not
	// treat this file's state as settled while this update is in flight.
	// Callers from handleEvent already hold authRescanMu; direct callers are
	// serialized by the same mutex scope inside refresh paths.
	w.observeAuthFile(path)
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		log.Errorf("failed to read auth file %s: %v", filepath.Base(path), errRead)
		return
	}
	if len(data) == 0 {
		log.Debugf("ignoring empty auth file: %s", filepath.Base(path))
		return
	}

	sum := sha256.Sum256(data)
	curHash := hex.EncodeToString(sum[:])
	normalized := w.normalizeAuthPath(path)

	// Parse new auth content for diff comparison
	var newAuth coreauth.Auth
	if errParse := json.Unmarshal(data, &newAuth); errParse != nil {
		log.Errorf("failed to parse auth file %s: %v", filepath.Base(path), errParse)
		return
	}

	w.clientsMutex.Lock()
	if w.config == nil {
		log.Error("config is nil, cannot add or update client")
		w.clientsMutex.Unlock()
		return
	}
	if w.fileAuthsByPath == nil {
		w.fileAuthsByPath = make(map[string]map[string]*coreauth.Auth)
	}
	if w.lastAuthHashes == nil {
		w.lastAuthHashes = make(map[string]string)
	}
	if prev, ok := w.lastAuthHashes[normalized]; ok && prev == curHash {
		log.Debugf("auth file unchanged (hash match), skipping reload: %s", filepath.Base(path))
		w.clientsMutex.Unlock()
		return
	}

	// Get old auth for diff comparison
	cacheAuthContents := log.IsLevelEnabled(log.DebugLevel)
	var oldAuth *coreauth.Auth
	if cacheAuthContents && w.lastAuthContents != nil {
		oldAuth = w.lastAuthContents[normalized]
	}

	// Compute and log field changes
	if cacheAuthContents {
		if changes := diff.BuildAuthChangeDetails(oldAuth, &newAuth); len(changes) > 0 {
			log.Debugf("auth field changes for %s:", filepath.Base(path))
			for _, c := range changes {
				log.Debugf("  %s", c)
			}
		}
	}

	// Update caches
	w.lastAuthHashes[normalized] = curHash
	if cacheAuthContents {
		if w.lastAuthContents == nil {
			w.lastAuthContents = make(map[string]*coreauth.Auth)
		}
		w.lastAuthContents[normalized] = &newAuth
	}

	oldByID := make(map[string]*coreauth.Auth, len(w.fileAuthsByPath[normalized]))
	for id, a := range w.fileAuthsByPath[normalized] {
		oldByID[id] = a
	}

	// Build synthesized auth entries for this single file only.
	sctx := &synthesizer.SynthesisContext{
		Config:      w.config,
		AuthDir:     w.authDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}
	generated, errSynth := synthesizer.SynthesizeAuthFile(sctx, path, data)
	if errSynth != nil {
		log.WithError(errSynth).Warnf("failed to synthesize auth file %s", filepath.Base(path))
	}
	newByID := authSliceToMap(generated)
	if len(newByID) > 0 {
		w.fileAuthsByPath[normalized] = authIDSet(newByID)
	} else {
		delete(w.fileAuthsByPath, normalized)
	}
	updates := w.computePerPathUpdatesLocked(oldByID, newByID)
	w.clientsMutex.Unlock()

	w.persistAuthAsync(fmt.Sprintf("Sync auth %s", filepath.Base(path)), path)
	w.dispatchAuthUpdates(updates)
}

func (w *Watcher) removeClient(path string) {
	w.observeAuthFile(path)
	normalized := w.normalizeAuthPath(path)
	w.clientsMutex.Lock()
	oldByID := make(map[string]*coreauth.Auth, len(w.fileAuthsByPath[normalized]))
	for id, a := range w.fileAuthsByPath[normalized] {
		oldByID[id] = a
	}
	delete(w.lastAuthHashes, normalized)
	delete(w.lastAuthContents, normalized)
	delete(w.fileAuthsByPath, normalized)

	updates := w.computePerPathUpdatesLocked(oldByID, map[string]*coreauth.Auth{})
	// Only the whole-file removal path gets this mark. Content diffs may drop
	// individual plugin auth IDs while their source file still exists.
	for index := range updates {
		if updates[index].Action == AuthUpdateActionDelete {
			updates[index].SetFileRemoval(true)
		}
	}
	w.clientsMutex.Unlock()

	w.persistAuthAsync(fmt.Sprintf("Remove auth %s", filepath.Base(path)), path)
	w.dispatchAuthUpdates(updates)
}

func (w *Watcher) computePerPathUpdatesLocked(oldByID, newByID map[string]*coreauth.Auth) []AuthUpdate {
	if w.currentAuths == nil {
		w.currentAuths = make(map[string]*coreauth.Auth)
	}
	updates := make([]AuthUpdate, 0, len(oldByID)+len(newByID))
	for id, newAuth := range newByID {
		existing, ok := w.currentAuths[id]
		if !ok {
			w.currentAuths[id] = newAuth.Clone()
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionAdd, ID: id, Auth: newAuth.Clone()})
			continue
		}
		// Always refresh the stored snapshot so later comparisons see the
		// latest content even when no update is emitted for this pass.
		w.currentAuths[id] = newAuth.Clone()
		if !authEqual(existing, newAuth) {
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionModify, ID: id, Auth: newAuth.Clone()})
		}
	}
	for id := range oldByID {
		if _, stillExists := newByID[id]; stillExists {
			continue
		}
		delete(w.currentAuths, id)
		updates = append(updates, AuthUpdate{Action: AuthUpdateActionDelete, ID: id})
	}
	w.stampAuthUpdatesLocked(updates)
	return updates
}

func authSliceToMap(auths []*coreauth.Auth) map[string]*coreauth.Auth {
	byID := make(map[string]*coreauth.Auth, len(auths))
	for _, a := range auths {
		if a == nil || strings.TrimSpace(a.ID) == "" {
			continue
		}
		byID[a.ID] = a
	}
	return byID
}

func authIDSet(auths map[string]*coreauth.Auth) map[string]*coreauth.Auth {
	set := make(map[string]*coreauth.Auth, len(auths))
	for id := range auths {
		set[id] = nil
	}
	return set
}

func (w *Watcher) loadFileClients(cfg *config.Config) int {
	authFileCount := 0
	successfulAuthCount := 0

	authDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir)
	if errResolveAuthDir != nil {
		log.Errorf("failed to resolve auth directory: %v", errResolveAuthDir)
		return 0
	}
	if authDir == "" {
		return 0
	}

	entries, errReadDir := os.ReadDir(authDir)
	if errReadDir != nil {
		log.Errorf("error reading auth directory: %v", errReadDir)
		return 0
	}
	for _, entry := range entries {
		if entry == nil || entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		authFileCount++
		log.Debugf("processing auth file %d: %s", authFileCount, name)
		fullPath := filepath.Join(authDir, name)
		if data, errReadFile := os.ReadFile(fullPath); errReadFile == nil && len(data) > 0 {
			successfulAuthCount++
		}
	}
	log.Debugf("auth directory scan complete - found %d .json files, %d readable", authFileCount, successfulAuthCount)
	return authFileCount
}

// BuildAPIKeyClients returns per-family API key client counts in the order
// (gemini, vertexCompat, claude, codex, meta, openAICompat).
// Ported from upstream CLIProxyAPI commit 54d4f4c0 (meta-api-key count added).
func BuildAPIKeyClients(cfg *config.Config) (int, int, int, int, int, int) {
	geminiAPIKeyCount := 0
	vertexCompatAPIKeyCount := 0
	claudeAPIKeyCount := 0
	codexAPIKeyCount := 0
	metaAPIKeyCount := 0
	openAICompatCount := 0

	if len(cfg.GeminiKey) > 0 {
		geminiAPIKeyCount += len(cfg.GeminiKey)
	}
	if len(cfg.VertexCompatAPIKey) > 0 {
		vertexCompatAPIKeyCount += len(cfg.VertexCompatAPIKey)
	}
	if len(cfg.ClaudeKey) > 0 {
		claudeAPIKeyCount += len(cfg.ClaudeKey)
	}
	if len(cfg.CodexKey) > 0 {
		codexAPIKeyCount += len(cfg.CodexKey)
	}
	if len(cfg.MetaKey) > 0 {
		metaAPIKeyCount += len(cfg.MetaKey)
	}
	if len(cfg.OpenAICompatibility) > 0 {
		for _, compatConfig := range cfg.OpenAICompatibility {
			if compatConfig.Disabled {
				continue
			}
			openAICompatCount += len(compatConfig.APIKeyEntries)
		}
	}
	return geminiAPIKeyCount, vertexCompatAPIKeyCount, claudeAPIKeyCount, codexAPIKeyCount, metaAPIKeyCount, openAICompatCount
}

func (w *Watcher) persistConfigAsync() {
	if w == nil || w.storePersister == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.storePersister.PersistConfig(ctx); err != nil {
			log.Errorf("failed to persist config change: %v", err)
		}
	}()
}

func (w *Watcher) persistAuthAsync(message string, paths ...string) {
	if w == nil || w.storePersister == nil {
		return
	}
	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	if len(filtered) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.storePersister.PersistAuthFiles(ctx, message, filtered...); err != nil {
			log.Errorf("failed to persist auth changes: %v", err)
		}
	}()
}

func (w *Watcher) stopServerUpdateTimer() {
	w.serverUpdateMu.Lock()
	defer w.serverUpdateMu.Unlock()
	if w.serverUpdateTimer != nil {
		w.serverUpdateTimer.Stop()
		w.serverUpdateTimer = nil
	}
	w.serverUpdatePend = false
}

func (w *Watcher) triggerServerUpdate(cfg *config.Config) {
	if w == nil || w.reloadCallback == nil || cfg == nil {
		return
	}
	if w.stopped.Load() {
		return
	}

	now := time.Now()

	w.serverUpdateMu.Lock()
	if w.serverUpdateLast.IsZero() || now.Sub(w.serverUpdateLast) >= serverUpdateDebounce {
		w.serverUpdateLast = now
		if w.serverUpdateTimer != nil {
			w.serverUpdateTimer.Stop()
			w.serverUpdateTimer = nil
		}
		w.serverUpdatePend = false
		w.serverUpdateMu.Unlock()
		w.reloadCallback(cfg)
		return
	}

	if w.serverUpdatePend {
		w.serverUpdateMu.Unlock()
		return
	}

	delay := serverUpdateDebounce - now.Sub(w.serverUpdateLast)
	if delay < 10*time.Millisecond {
		delay = 10 * time.Millisecond
	}
	w.serverUpdatePend = true
	if w.serverUpdateTimer != nil {
		w.serverUpdateTimer.Stop()
		w.serverUpdateTimer = nil
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		if w.stopped.Load() {
			return
		}
		w.clientsMutex.RLock()
		latestCfg := w.config
		w.clientsMutex.RUnlock()

		w.serverUpdateMu.Lock()
		if w.serverUpdateTimer != timer || !w.serverUpdatePend {
			w.serverUpdateMu.Unlock()
			return
		}
		w.serverUpdateTimer = nil
		w.serverUpdatePend = false
		if latestCfg == nil || w.reloadCallback == nil || w.stopped.Load() {
			w.serverUpdateMu.Unlock()
			return
		}

		w.serverUpdateLast = time.Now()
		w.serverUpdateMu.Unlock()
		w.reloadCallback(latestCfg)
	})
	w.serverUpdateTimer = timer
	w.serverUpdateMu.Unlock()
}
