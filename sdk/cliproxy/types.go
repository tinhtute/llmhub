// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"
	"fmt"

	"github.com/therealtinhtute/llmhub/internal/watcher"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/therealtinhtute/llmhub/sdk/config"
)

// TokenClientProvider loads clients backed by stored authentication tokens.
// It provides an interface for loading authentication tokens from various sources
// and creating clients for AI service providers.
type TokenClientProvider interface {
	// Load loads token-based clients from the configured source.
	//
	// Parameters:
	//   - ctx: The context for the loading operation
	//   - cfg: The application configuration
	//
	// Returns:
	//   - *TokenClientResult: The result containing loaded clients
	//   - error: An error if loading fails
	Load(ctx context.Context, cfg *config.Config) (*TokenClientResult, error)
}

// TokenClientResult represents clients generated from persisted tokens.
// It contains metadata about the loading operation and the number of successful authentications.
type TokenClientResult struct {
	// SuccessfulAuthed is the number of successfully authenticated clients.
	SuccessfulAuthed int
}

// APIKeyClientProvider loads clients backed directly by configured API keys.
// It provides an interface for loading API key-based clients for various AI service providers.
type APIKeyClientProvider interface {
	// Load loads API key-based clients from the configuration.
	//
	// Parameters:
	//   - ctx: The context for the loading operation
	//   - cfg: The application configuration
	//
	// Returns:
	//   - *APIKeyClientResult: The result containing loaded clients
	//   - error: An error if loading fails
	Load(ctx context.Context, cfg *config.Config) (*APIKeyClientResult, error)
}

// APIKeyClientResult is returned by APIKeyClientProvider.Load()
type APIKeyClientResult struct {
	// GeminiKeyCount is the number of Gemini API keys loaded
	GeminiKeyCount int

	// VertexCompatKeyCount is the number of Vertex-compatible API keys loaded
	VertexCompatKeyCount int

	// ClaudeKeyCount is the number of Claude API keys loaded
	ClaudeKeyCount int

	// CodexKeyCount is the number of Codex API keys loaded
	CodexKeyCount int

	// MetaKeyCount is the number of Meta API keys loaded
	MetaKeyCount int

	// OpenAICompatCount is the number of OpenAI compatibility API keys loaded
	OpenAICompatCount int
}

// WatcherFactory creates a watcher for configuration and token changes.
// The reload callback receives the updated configuration when changes are detected.
//
// Parameters:
//   - configPath: The path to the configuration file to watch
//   - authDir: The directory containing authentication tokens to watch
//   - reload: The callback function to call when changes are detected
//
// Returns:
//   - *WatcherWrapper: A watcher wrapper instance
//   - error: An error if watcher creation fails
type WatcherFactory func(configPath, authDir string, reload func(*config.Config)) (*WatcherWrapper, error)

// WatcherWrapper exposes the subset of watcher methods required by the SDK.
type WatcherWrapper struct {
	start func(ctx context.Context) error
	stop  func() error

	setConfig                   func(cfg *config.Config)
	snapshotAuths               func() []*coreauth.Auth
	snapshotAuthFile            func(path string) ([]*coreauth.Auth, error)
	rereadDiscardedFileSnapshot func(auth *coreauth.Auth)
	setUpdateQueue              func(queue chan<- watcher.AuthUpdate)
	dispatchRuntimeUpdate       func(update watcher.AuthUpdate) bool
}

// Start proxies to the underlying watcher Start implementation.
func (w *WatcherWrapper) Start(ctx context.Context) error {
	if w == nil || w.start == nil {
		return nil
	}
	return w.start(ctx)
}

// Stop proxies to the underlying watcher Stop implementation.
func (w *WatcherWrapper) Stop() error {
	if w == nil || w.stop == nil {
		return nil
	}
	return w.stop()
}

// SetConfig updates the watcher configuration cache.
func (w *WatcherWrapper) SetConfig(cfg *config.Config) {
	if w == nil || w.setConfig == nil {
		return
	}
	w.setConfig(cfg)
}

// DispatchRuntimeAuthUpdate forwards runtime auth updates (e.g., websocket providers)
// into the watcher-managed auth update queue when available.
// Returns true if the update was enqueued successfully.
func (w *WatcherWrapper) DispatchRuntimeAuthUpdate(update watcher.AuthUpdate) bool {
	if w == nil || w.dispatchRuntimeUpdate == nil {
		return false
	}
	return w.dispatchRuntimeUpdate(update)
}

// SetClients updates the watcher file-backed clients registry.
// SetClients and SetAPIKeyClients removed; watcher manages its own caches

// SnapshotClients returns the current combined clients snapshot from the underlying watcher.
// SnapshotClients removed; use SnapshotAuths

// SnapshotAuths returns the current auth entries derived from legacy clients.
func (w *WatcherWrapper) SnapshotAuths() []*coreauth.Auth {
	if w == nil || w.snapshotAuths == nil {
		return nil
	}
	return w.snapshotAuths()
}

// SnapshotAuthFile re-synthesizes a single file and preserves parsing errors.
func (w *WatcherWrapper) SnapshotAuthFile(path string) ([]*coreauth.Auth, error) {
	if w == nil || w.snapshotAuthFile == nil {
		return nil, fmt.Errorf("file auth snapshot is unavailable")
	}
	return w.snapshotAuthFile(path)
}

// RereadDiscardedFileSnapshot asks the underlying watcher to invalidate and
// re-read a file snapshot the manager discarded as stale.
func (w *WatcherWrapper) RereadDiscardedFileSnapshot(auth *coreauth.Auth) {
	if w == nil || w.rereadDiscardedFileSnapshot == nil {
		return
	}
	w.rereadDiscardedFileSnapshot(auth)
}

// SetAuthUpdateQueue registers the channel used to propagate auth updates.
func (w *WatcherWrapper) SetAuthUpdateQueue(queue chan<- watcher.AuthUpdate) {
	if w == nil || w.setUpdateQueue == nil {
		return
	}
	w.setUpdateQueue(queue)
}
