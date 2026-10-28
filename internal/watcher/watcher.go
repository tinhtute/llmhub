// Package watcher watches config/auth files and triggers hot reloads.
// It supports cross-platform fsnotify event handling.
package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/watcher/synthesizer"
	"gopkg.in/yaml.v3"

	log "github.com/sirupsen/logrus"
	sdkAuth "github.com/therealtinhtute/llmhub/sdk/auth"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// storePersister captures persistence-capable token store methods used by the watcher.
type storePersister interface {
	PersistConfig(ctx context.Context) error
	PersistAuthFiles(ctx context.Context, message string, paths ...string) error
}

type authDirProvider interface {
	AuthDir() string
}

// Watcher manages file watching for configuration and authentication files
type Watcher struct {
	configPath        string
	authDir           string
	config            *config.Config
	clientsMutex      sync.RWMutex
	authRescanMu      sync.Mutex
	configReloadMu    sync.Mutex
	configReloadTimer *time.Timer
	serverUpdateMu    sync.Mutex
	serverUpdateTimer *time.Timer
	serverUpdateLast  time.Time
	serverUpdatePend  bool
	stopped           atomic.Bool
	reloadCallback    func(*config.Config)
	watcher           *fsnotify.Watcher
	lastAuthHashes    map[string]string
	lastAuthContents  map[string]*coreauth.Auth
	fileAuthsByPath   map[string]map[string]*coreauth.Auth
	lastRemoveTimes   map[string]time.Time
	lastConfigHash    string
	authQueue         chan<- AuthUpdate
	currentAuths      map[string]*coreauth.Auth
	authRevisions     map[string]uint64 // Includes deletion tombstones; guarded by clientsMutex.
	// persistedAuthRevisions holds the revision stamped by the last runtime
	// auth update the watcher persisted, per auth ID. A full scan compares it
	// with authRevisions to tell "this file moved during the scan" from "the
	// record was persisted during the scan", which is what makes the scan
	// re-read the file instead of republishing the observation it started with.
	// Guarded by clientsMutex.
	persistedAuthRevisions map[string]uint64
	fileObservations       map[string]uint64 // Tracks file events even when content is unchanged.
	activeAuthScans        int               // Guarded by clientsMutex.
	runtimeAuths           map[string]*coreauth.Auth
	dispatchMu             sync.Mutex
	dispatchCond           *sync.Cond
	pendingUpdates         map[string]AuthUpdate
	pendingOrder           []string
	dispatchCancel         context.CancelFunc
	storePersister         storePersister
	mirroredAuthDir        string
	oldConfigYaml          []byte
}

// AuthUpdateAction represents the type of change detected in auth sources.
type AuthUpdateAction string

const (
	AuthUpdateActionAdd    AuthUpdateAction = "add"
	AuthUpdateActionModify AuthUpdateAction = "modify"
	AuthUpdateActionDelete AuthUpdateAction = "delete"
)

// AuthUpdate describes an incremental change to auth configuration.
type AuthUpdate struct {
	Action      AuthUpdateAction
	ID          string
	Auth        *coreauth.Auth
	revision    uint64 // Watcher-local ordering, independent of runtime auth generations.
	fileRemoval bool   // The source file was removed, not just an ID from its content.
}

// IsFileRemoval reports whether this delete was observed as a whole-file removal.
func (u AuthUpdate) IsFileRemoval() bool {
	return u.fileRemoval
}

// SetFileRemoval marks a delete as a whole-file removal rather than a per-ID diff.
func (u *AuthUpdate) SetFileRemoval(fileRemoval bool) {
	if u != nil {
		u.fileRemoval = fileRemoval
	}
}

// Revision returns the monotonic watcher revision assigned to this update.
func (u AuthUpdate) Revision() uint64 {
	return u.revision
}

const (
	// replaceCheckDelay is a short delay to allow atomic replace (rename) to settle
	// before deciding whether a Remove event indicates a real deletion.
	replaceCheckDelay        = 50 * time.Millisecond
	configReloadDebounce     = 150 * time.Millisecond
	authRemoveDebounceWindow = 1 * time.Second
	serverUpdateDebounce     = 1 * time.Second
)

// NewWatcher creates a new file watcher instance
func NewWatcher(configPath, authDir string, reloadCallback func(*config.Config)) (*Watcher, error) {
	watcher, errNewWatcher := fsnotify.NewWatcher()
	if errNewWatcher != nil {
		return nil, errNewWatcher
	}
	w := &Watcher{
		configPath:      configPath,
		authDir:         authDir,
		reloadCallback:  reloadCallback,
		watcher:         watcher,
		lastAuthHashes:  make(map[string]string),
		fileAuthsByPath: make(map[string]map[string]*coreauth.Auth),
	}
	w.dispatchCond = sync.NewCond(&w.dispatchMu)
	if store := sdkAuth.GetTokenStore(); store != nil {
		if persister, ok := store.(storePersister); ok {
			w.storePersister = persister
			log.Debug("persistence-capable token store detected; watcher will propagate persisted changes")
		}
		if provider, ok := store.(authDirProvider); ok {
			if fixed := strings.TrimSpace(provider.AuthDir()); fixed != "" {
				w.mirroredAuthDir = fixed
				log.Debugf("mirrored auth directory locked to %s", fixed)
			}
		}
	}
	return w, nil
}

// Start begins watching the configuration file and authentication directory
func (w *Watcher) Start(ctx context.Context) error {
	return w.start(ctx)
}

// Stop stops the file watcher
func (w *Watcher) Stop() error {
	w.stopped.Store(true)
	w.stopDispatch()
	w.stopConfigReloadTimer()
	w.stopServerUpdateTimer()
	return w.watcher.Close()
}

// SetConfig updates the current configuration
func (w *Watcher) SetConfig(cfg *config.Config) {
	// Own an isolated snapshot: producers reuse their Config pointer after
	// this call, and sharing it would race with reload-time readers.
	cloned := cfg.CloneForRuntime()
	w.clientsMutex.Lock()
	defer w.clientsMutex.Unlock()
	w.config = cloned
	w.oldConfigYaml, _ = yaml.Marshal(cloned)
}

// SetAuthUpdateQueue sets the queue used to emit auth updates.
func (w *Watcher) SetAuthUpdateQueue(queue chan<- AuthUpdate) {
	w.setAuthUpdateQueue(queue)
}

// DispatchRuntimeAuthUpdate allows external runtime providers (e.g., websocket-driven auths)
// to push auth updates through the same queue used by file/config watchers.
// Returns true if the update was enqueued; false if no queue is configured.
func (w *Watcher) DispatchRuntimeAuthUpdate(update AuthUpdate) bool {
	return w.dispatchRuntimeAuthUpdate(update)
}

// RereadDiscardedFileSnapshot invalidates watcher caches for a file snapshot the
// manager discarded and re-reads the backing file, so a change already on disk is
// not suppressed by the digest the watcher already published.
func (w *Watcher) RereadDiscardedFileSnapshot(auth *coreauth.Auth) {
	if w == nil {
		return
	}
	w.rereadDiscardedFileSnapshot(auth)
}

// SnapshotCoreAuths converts current clients snapshot into core auth entries.
func (w *Watcher) SnapshotCoreAuths() []*coreauth.Auth {
	w.clientsMutex.RLock()
	cfg := w.config
	w.clientsMutex.RUnlock()
	return snapshotCoreAuths(cfg, w.authDir)
}

// SnapshotAuthFile re-synthesizes one source file without consulting cached auth state.
// Parse errors are surfaced so deletion reconciliation never relies on fallback auths.
func (w *Watcher) SnapshotAuthFile(path string) ([]*coreauth.Auth, error) {
	if w == nil {
		return nil, fmt.Errorf("file auth snapshot is unavailable")
	}
	w.clientsMutex.RLock()
	cfg := w.config
	authDir := w.authDir
	w.clientsMutex.RUnlock()
	if cfg == nil || authDir == "" {
		return nil, fmt.Errorf("file auth snapshot is not configured")
	}
	// Management uploads use absolute paths even when authDir is configured
	// relatively. Keep both absolute so synthesis retains file-relative IDs.
	absoluteAuthDir, errAbsAuthDir := filepath.Abs(authDir)
	if errAbsAuthDir != nil {
		return nil, fmt.Errorf("resolve auth directory: %w", errAbsAuthDir)
	}
	absolutePath, errAbsPath := filepath.Abs(path)
	if errAbsPath != nil {
		return nil, fmt.Errorf("resolve auth file path: %w", errAbsPath)
	}
	authDir = filepath.Clean(absoluteAuthDir)
	path = filepath.Clean(absolutePath)
	data, errReadFile := os.ReadFile(path)
	if errReadFile != nil {
		return nil, fmt.Errorf("read auth file: %w", errReadFile)
	}
	return synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{
		Config:            cfg,
		AuthDir:           authDir,
		Now:               time.Now(),
		IDGenerator:       synthesizer.NewStableIDGenerator(),
		StrictFileParsing: true,
	}, path, data)
}
