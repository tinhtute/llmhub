// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/api"
	"github.com/therealtinhtute/llmhub/internal/home"
	"github.com/therealtinhtute/llmhub/internal/nativeproviders"
	"github.com/therealtinhtute/llmhub/internal/quotaalert"
	"github.com/therealtinhtute/llmhub/internal/redisqueue"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor"
	"github.com/therealtinhtute/llmhub/internal/runtimecontrol"
	"github.com/therealtinhtute/llmhub/internal/runtimepolicy"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/therealtinhtute/llmhub/internal/watcher"
	"github.com/therealtinhtute/llmhub/internal/watcher/diff"
	"github.com/therealtinhtute/llmhub/internal/wsrelay"
	sdkaccess "github.com/therealtinhtute/llmhub/sdk/access"
	sdkAuth "github.com/therealtinhtute/llmhub/sdk/auth"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/executionregistry"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	"github.com/therealtinhtute/llmhub/sdk/config"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Service wraps the proxy server lifecycle so external programs can embed the CLI proxy.
// It manages the complete lifecycle including authentication, file watching, HTTP server,
// and integration with various AI service providers.
type Service struct {
	// cfg holds the current application configuration.
	cfg *config.Config

	// cfgMu protects concurrent access to the configuration.
	cfgMu sync.RWMutex

	// configUpdateMu serializes config updates across watcher + home.
	configUpdateMu sync.Mutex

	// configPath is the path to the configuration file.
	configPath string

	// tokenProvider handles loading token-based clients.
	tokenProvider TokenClientProvider

	// apiKeyProvider handles loading API key-based clients.
	apiKeyProvider APIKeyClientProvider

	// watcherFactory creates file watcher instances.
	watcherFactory WatcherFactory

	// hooks provides lifecycle callbacks.
	hooks Hooks

	// serverOptions contains additional server configuration options.
	serverOptions []api.ServerOption

	// managementConfigStore persists management config changes outside local files.
	managementConfigStore api.ManagementConfigStore
	runtimeSettingsStore  runtimecontrol.SettingsStore

	quotaAlertStore  quotaalert.Store
	quotaAlertCipher *quotaalert.SecretCipher

	runtimeStoragePolicy runtimepolicy.RuntimeStorage

	// server is the HTTP API server instance.
	server *api.Server

	// pprofServer manages the optional pprof HTTP debug server.
	pprofServer *pprofServer

	// serverErr channel for server startup/shutdown errors.
	serverErr chan error

	// watcher handles file system monitoring.
	watcher *WatcherWrapper

	// watcherCancel cancels the watcher context.
	watcherCancel context.CancelFunc

	// authUpdates channel for authentication updates.
	authUpdates chan watcher.AuthUpdate

	// authQueueStop cancels the auth update queue processing.
	authQueueStop context.CancelFunc

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// shutdownOnce ensures shutdown is called only once.
	shutdownOnce sync.Once

	// wsGateway manages websocket Gemini providers.
	wsGateway *wsrelay.Manager

	homeClient         *home.Client
	homeCancel         context.CancelFunc
	homeReleaseCancel  context.CancelFunc
	homeRegistry       *executionregistry.Registry
	homeDispatchBundle *coreauth.HomeDispatchBundle
	homeGeneration     uint64
	homeLifetimeMu     sync.Mutex
	homeReleaseFlusher interface {
		Flush(context.Context) error
	}

	quotaAlertService *quotaalert.Service
}

// RegisterUsagePlugin registers a usage plugin on the global usage manager.
// This allows external code to monitor API usage and token consumption.
//
// Parameters:
//   - plugin: The usage plugin to register
func (s *Service) RegisterUsagePlugin(plugin usage.Plugin) {
	usage.RegisterPlugin(plugin)
}

// newDefaultAuthManager creates a default authentication manager with all supported providers.
func newDefaultAuthManager() *sdkAuth.Manager {
	return sdkAuth.NewManager(
		sdkAuth.GetTokenStore(),
		sdkAuth.NewGeminiAuthenticator(),
		sdkAuth.NewCodexAuthenticator(),
		sdkAuth.NewClaudeAuthenticator(),
		sdkAuth.NewAntigravityAuthenticator(),
		sdkAuth.NewKimiAuthenticator(),
		sdkAuth.NewKimiAIAuthenticator(),
		sdkAuth.NewKimiAIDotAuthenticator(),
		sdkAuth.NewXAIAuthenticator(),
		// Ported from upstream CLIProxyAPI commit 54d4f4c0.
		sdkAuth.NewMetaAuthenticator(),
	)
}

func (s *Service) ensureAuthUpdateQueue(ctx context.Context) {
	if s == nil {
		return
	}
	if s.authUpdates == nil {
		s.authUpdates = make(chan watcher.AuthUpdate, 256)
	}
	if s.authQueueStop != nil {
		return
	}
	queueCtx, cancel := context.WithCancel(ctx)
	s.authQueueStop = cancel
	go s.consumeAuthUpdates(queueCtx)
}

func (s *Service) consumeAuthUpdates(ctx context.Context) {
	ctx = coreauth.WithSkipPersist(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-s.authUpdates:
			if !ok {
				return
			}
			s.handleAuthUpdate(ctx, update)
		labelDrain:
			for {
				select {
				case nextUpdate := <-s.authUpdates:
					s.handleAuthUpdate(ctx, nextUpdate)
				default:
					break labelDrain
				}
			}
		}
	}
}

func (s *Service) emitAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.watcher != nil && s.watcher.DispatchRuntimeAuthUpdate(update) {
		return
	}
	if s.authUpdates != nil {
		select {
		case s.authUpdates <- update:
			return
		default:
			log.Debugf("auth update queue saturated, applying inline action=%v id=%s", update.Action, update.ID)
		}
	}
	s.handleAuthUpdate(ctx, update)
}

func (s *Service) handleAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || s.coreManager == nil {
		return
	}
	switch update.Action {
	case watcher.AuthUpdateActionAdd, watcher.AuthUpdateActionModify:
		if update.Auth == nil || update.Auth.ID == "" {
			return
		}
		s.applyCoreAuthAddOrUpdate(ctx, update.Auth)
	case watcher.AuthUpdateActionDelete:
		id := update.ID
		if id == "" && update.Auth != nil {
			id = update.Auth.ID
		}
		if id == "" {
			return
		}
		if existing, ok := s.coreManager.GetByID(id); ok && existing != nil {
			if update.Auth != nil && isStaleCoreAuth(existing, update.Auth) {
				log.Debugf("skipping stale auth delete for %s: incoming gen=%d, existing gen=%d", id, update.Auth.Generation, existing.Generation)
				return
			}
			// Whole-file deletes can outlive a persisted re-create whose inline
			// sync did not carry a revision. Recheck local files, including remote
			// store mirrors, but do not suppress per-ID content/config deletes.
			if update.Auth == nil && update.IsFileRemoval() {
				path := existing.Attributes[coreauth.AttributePath]
				switch existing.AuthSourceKind() {
				case coreauth.AuthSourceFile:
					if path == "" {
						path = existing.Attributes[coreauth.AttributeSource]
					}
				case coreauth.AuthSourceGit, coreauth.AuthSourcePostgres, coreauth.AuthSourceObjectStore:
					// Remote stores are file-backed only when they provide a local path.
				default:
					path = ""
				}
				if path != "" && s.shouldSkipFileAuthDelete(path, id) {
					return
				}
			}
		}
		s.applyCoreAuthRemoval(ctx, id)
	default:
		log.Debugf("received unknown auth update action: %v", update.Action)
	}
}

func (s *Service) ensureWebsocketGateway() {
	if s == nil {
		return
	}
	if s.wsGateway != nil {
		return
	}
	opts := wsrelay.Options{
		Path:           "/v1/ws",
		OnConnected:    s.wsOnConnected,
		OnDisconnected: s.wsOnDisconnected,
		LogDebugf:      log.Debugf,
		LogInfof:       log.Infof,
		LogWarnf:       log.Warnf,
	}
	s.wsGateway = wsrelay.NewManager(opts)
}

func (s *Service) wsOnConnected(channelID string) {
	if s == nil || channelID == "" {
		return
	}
	if !strings.HasPrefix(strings.ToLower(channelID), "aistudio-") {
		return
	}
	if s.coreManager != nil {
		if existing, ok := s.coreManager.GetByID(channelID); ok && existing != nil {
			if !existing.Disabled && existing.Status == coreauth.StatusActive {
				return
			}
		}
	}
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:         channelID,  // keep channel identifier as ID
		Provider:   "aistudio", // logical provider for switch routing
		Label:      channelID,  // display original channel id
		Status:     coreauth.StatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"email": channelID}, // metadata drives logging and usage tracking
	}
	log.Infof("websocket provider connected: %s", channelID)
	s.emitAuthUpdate(context.Background(), watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionAdd,
		ID:     auth.ID,
		Auth:   auth,
	})
}

func (s *Service) wsOnDisconnected(channelID string, reason error) {
	if s == nil || channelID == "" {
		return
	}
	if reason != nil {
		if strings.Contains(reason.Error(), "replaced by new connection") {
			log.Infof("websocket provider replaced: %s", channelID)
			return
		}
		log.Warnf("websocket provider disconnected: %s (%v)", channelID, reason)
	} else {
		log.Infof("websocket provider disconnected: %s", channelID)
	}
	ctx := context.Background()
	s.emitAuthUpdate(ctx, watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionDelete,
		ID:     channelID,
	})
}

func (s *Service) applyCoreAuthAddOrUpdate(ctx context.Context, auth *coreauth.Auth) {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" {
		return
	}
	auth = auth.Clone()
	s.ensureExecutorsForAuth(auth)

	// IMPORTANT: Update coreManager FIRST, before model registration.
	// This ensures that configuration changes (proxy_url, prefix, etc.) take effect
	// immediately for API calls, rather than waiting for model registration to complete.
	op := "register"
	var err error
	if existing, ok := s.coreManager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		// A file snapshot's runtime state is merged by the manager, which owns the
		// cooldown and generation fences. Copying it here as well would let the two
		// rules drift apart.
		if !auth.HasFileSnapshot() && !existing.Disabled && existing.Status != coreauth.StatusDisabled && !auth.Disabled && auth.Status != coreauth.StatusDisabled {
			auth.LastRefreshedAt = existing.LastRefreshedAt
			auth.NextRefreshAfter = existing.NextRefreshAfter
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
		}
		op = "update"
		var updated *coreauth.Auth
		updated, err = s.coreManager.Update(ctx, auth)
		if err == nil && updated != nil {
			if updated.Generation == existing.Generation {
				log.Debugf("skipping stale auth update for %s: incoming snapshot did not advance generation (gen=%d)", auth.ID, existing.Generation)
			}
			auth = updated
		}
	} else {
		var registered *coreauth.Auth
		registered, err = s.coreManager.Register(ctx, auth)
		if err == nil && registered != nil {
			auth = registered
		}
	}
	if err != nil {
		log.Errorf("failed to %s auth %s: %v", op, auth.ID, err)
		current, ok := s.coreManager.GetByID(auth.ID)
		if !ok || current.Disabled {
			GlobalModelRegistry().UnregisterClient(auth.ID)
			return
		}
		auth = current
	}

	// Register models after auth is updated in coreManager.
	// This operation may block on network calls, but the auth configuration
	// is already effective at this point.
	s.completeModelRegistrationForAuth(ctx, auth)
}

func (s *Service) applyCoreAuthRemoval(ctx context.Context, id string) {
	if s == nil || id == "" {
		return
	}
	if s.coreManager == nil {
		return
	}
	GlobalModelRegistry().UnregisterClient(id)
	if existing, ok := s.coreManager.GetByID(id); ok && existing != nil {
		s.coreManager.Forget(id)
		if strings.EqualFold(strings.TrimSpace(existing.Provider), "codex") {
			executor.CloseCodexWebsocketSessionsForAuthID(existing.ID, "auth_removed")
		}
	}
}

// shouldSkipFileAuthDelete retains re-created IDs and preserves auths on uncertain reads.
func (s *Service) shouldSkipFileAuthDelete(path, id string) bool {
	_, errStat := os.Stat(path)
	if os.IsNotExist(errStat) {
		return false
	}
	if errStat != nil {
		log.Debugf("skipping auth delete for %s: backing file is not confirmed missing", id)
		return true
	}
	// A file can return with only a subset of its previous plugin auth IDs.
	// Read just this file so another file with the same ID cannot retain the auth.
	auths, errSnapshotAuthFile := s.currentWatcher().SnapshotAuthFile(path)
	if errSnapshotAuthFile != nil || len(auths) == 0 {
		log.Debugf("skipping auth delete for %s: file auth snapshot is uncertain (err=%v)", id, errSnapshotAuthFile)
		return true
	}
	hasAuth := false
	for _, auth := range auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		hasAuth = true
		// Store IDs retain case, but the file synthesizer lowercases IDs on Windows.
		// Other platforms keep exact matching for case-sensitive plugin auth IDs.
		if authIDMatches(auth.ID, id, runtime.GOOS == "windows") {
			log.Debugf("skipping auth delete for %s: auth ID is present in the re-created file", id)
			return true
		}
	}
	if !hasAuth {
		log.Debugf("skipping auth delete for %s: file auth snapshot has no valid IDs", id)
	}
	return !hasAuth
}

func (s *Service) currentWatcher() *WatcherWrapper {
	if s == nil {
		return nil
	}
	return s.watcher
}

func authIDMatches(snapshotID, id string, foldCase bool) bool {
	if foldCase {
		return strings.EqualFold(snapshotID, id)
	}
	return snapshotID == id
}

func isStaleCoreAuth(existing, incoming *coreauth.Auth) bool {
	if existing == nil || incoming == nil {
		return false
	}
	if incoming.RegistrationEpoch > 0 && incoming.RegistrationEpoch < existing.RegistrationEpoch {
		return true
	}
	if incoming.Generation > 0 && incoming.Generation < existing.Generation {
		return true
	}
	return false
}

func (s *Service) applyRetryConfig(cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	maxInterval := time.Duration(cfg.MaxRetryInterval) * time.Second
	s.coreManager.SetRetryConfig(cfg.RequestRetry, maxInterval, cfg.MaxRetryCredentials)
	coreauth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)
}

func openAICompatInfoFromAuth(a *coreauth.Auth) (providerKey string, compatName string, ok bool) {
	if a == nil {
		return "", "", false
	}
	if len(a.Attributes) > 0 {
		providerKey = strings.TrimSpace(a.Attributes["provider_key"])
		compatName = strings.TrimSpace(a.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey), compatName, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "openai-compatibility") {
		return "openai-compatibility", strings.TrimSpace(a.Label), true
	}
	return "", "", false
}

func (s *Service) ensureExecutorsForAuth(a *coreauth.Auth) {
	s.ensureExecutorsForAuthWithMode(a, false)
}

func (s *Service) ensureExecutorsForAuthWithMode(a *coreauth.Auth, forceReplace bool) {
	if s == nil || s.coreManager == nil || a == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		if !forceReplace {
			existingExecutor, hasExecutor := s.coreManager.Executor("codex")
			if hasExecutor {
				_, isCodexAutoExecutor := existingExecutor.(*executor.CodexAutoExecutor)
				if isCodexAutoExecutor {
					return
				}
			}
		}
		s.coreManager.RegisterExecutor(executor.NewCodexAutoExecutor(s.cfg))
		return
	}
	// Skip disabled auth entries when (re)binding executors.
	// Disabled auths can linger during config reloads (e.g., removed OpenAI-compat entries)
	// and must not override active provider executors.
	if a.Disabled {
		return
	}
	if compatProviderKey, _, isCompat := openAICompatInfoFromAuth(a); isCompat {
		if compatProviderKey == "" {
			compatProviderKey = strings.ToLower(strings.TrimSpace(a.Provider))
		}
		if compatProviderKey == "" {
			compatProviderKey = "openai-compatibility"
		}
		s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(compatProviderKey, s.cfg))
		return
	}
	switch strings.ToLower(a.Provider) {
	case "gemini":
		s.coreManager.RegisterExecutor(executor.NewGeminiExecutor(s.cfg))
	case "vertex":
		s.coreManager.RegisterExecutor(executor.NewGeminiVertexExecutor(s.cfg))
	case "gemini-cli":
		s.coreManager.RegisterExecutor(executor.NewGeminiCLIExecutor(s.cfg))
	case "aistudio":
		if s.wsGateway != nil {
			s.coreManager.RegisterExecutor(executor.NewAIStudioExecutor(s.cfg, a.ID, s.wsGateway))
		}
		return
	case "antigravity":
		s.coreManager.RegisterExecutor(executor.NewAntigravityExecutor(s.cfg))
	case "claude":
		s.coreManager.RegisterExecutor(executor.NewClaudeExecutor(s.cfg))
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		s.coreManager.RegisterExecutor(executor.NewKimiExecutor(s.cfg))
	case "xai":
		s.coreManager.RegisterExecutor(executor.NewXAIExecutor(s.cfg))
	case "kiro":
		s.coreManager.RegisterExecutor(executor.NewKiroExecutor(s.cfg))
	case "devin":
		s.coreManager.RegisterExecutor(executor.NewDevinExecutor(s.cfg))
	case "meta":
		// Ported from upstream CLIProxyAPI commit 54d4f4c0.
		s.coreManager.RegisterExecutor(executor.NewMetaExecutor(s.cfg))
	default:
		providerKey := strings.ToLower(strings.TrimSpace(a.Provider))
		if providerKey == "" {
			providerKey = "openai-compatibility"
		}
		s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(providerKey, s.cfg))
	}
}

// comboModelsClientID is the registry client id under which combo virtual models are registered.
const comboModelsClientID = "llmhub-combos"

// registerComboModels (re)registers combo names as client-visible models so /v1/models lists
// them and clients can select them. Combo models are virtual: they are never routed as ordinary
// models themselves (config validation rejects candidates that are combo names).
func (s *Service) registerComboModels(cfg *config.Config) {
	if s == nil {
		return
	}
	GlobalModelRegistry().UnregisterClient(comboModelsClientID)
	if cfg == nil || len(cfg.Combos) == 0 {
		return
	}
	models := make([]*ModelInfo, 0, len(cfg.Combos))
	for _, combo := range cfg.Combos {
		models = append(models, &ModelInfo{
			ID:          combo.Name,
			Object:      "model",
			OwnedBy:     "combo",
			Type:        "combo",
			DisplayName: combo.Name,
		})
	}
	GlobalModelRegistry().RegisterClient(comboModelsClientID, "combo", models)
}

func (s *Service) registerResolvedModelsForAuth(a *coreauth.Auth, fence modelRegistrationFence, providerKey string, models []*ModelInfo) bool {
	return s.registerResolvedModelsForAuthWithContext(context.Background(), a, fence, providerKey, models)
}

func (s *Service) registerResolvedModelsForAuthWithContext(ctx context.Context, a *coreauth.Auth, fence modelRegistrationFence, providerKey string, models []*ModelInfo) bool {
	if a == nil || a.ID == "" {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	published := s.registerResolvedModelsForAuthFenced(a, fence, providerKey, models)
	if !published && ctx.Err() == nil {
		s.reconcileSupersededAuthRegistration(ctx, a.ID)
	}
	return published
}

// rebindExecutors refreshes provider executors so they observe the latest configuration.
func (s *Service) rebindExecutors() {
	if s == nil || s.coreManager == nil {
		return
	}
	auths := s.coreManager.List()
	reboundCodex := false
	for _, auth := range auths {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			if reboundCodex {
				continue
			}
			reboundCodex = true
		}
		s.ensureExecutorsForAuthWithMode(auth, true)
	}
}

func (s *Service) applyConfigUpdate(newCfg *config.Config) {
	if s == nil {
		return
	}

	s.configUpdateMu.Lock()
	defer s.configUpdateMu.Unlock()

	previousStrategy := ""
	var previousSessionAffinity bool
	var previousSessionAffinityTTL string
	var previousHome config.HomeConfig
	var previousHomeSet bool
	s.cfgMu.RLock()
	if s.cfg != nil {
		previousStrategy = strings.ToLower(strings.TrimSpace(s.cfg.Routing.Strategy))
		previousSessionAffinity = s.cfg.Routing.SessionAffinity
		previousSessionAffinityTTL = s.cfg.Routing.SessionAffinityTTL
		previousHome = s.cfg.Home
		previousHomeSet = true
	}
	s.cfgMu.RUnlock()

	if newCfg == nil {
		s.cfgMu.RLock()
		newCfg = s.cfg
		s.cfgMu.RUnlock()
	}
	if newCfg == nil {
		return
	}
	if nativeStore, ok := s.managementConfigStore.(nativeproviders.Store); ok {
		if err := nativeproviders.HydrateConfig(context.Background(), newCfg, nativeStore); err != nil {
			log.Warnf("failed to hydrate native provider resources: %v", err)
		}
	}

	nextStrategy := strings.ToLower(strings.TrimSpace(newCfg.Routing.Strategy))
	normalizeStrategy := func(strategy string) string {
		switch strategy {
		case "fill-first", "fillfirst", "ff":
			return "fill-first"
		default:
			return "round-robin"
		}
	}
	previousStrategy = normalizeStrategy(previousStrategy)
	nextStrategy = normalizeStrategy(nextStrategy)

	nextSessionAffinity := newCfg.Routing.SessionAffinity
	nextSessionAffinityTTL := newCfg.Routing.SessionAffinityTTL

	selectorChanged := previousStrategy != nextStrategy ||
		previousSessionAffinity != nextSessionAffinity ||
		previousSessionAffinityTTL != nextSessionAffinityTTL

	if s.coreManager != nil && selectorChanged {
		var selector coreauth.Selector
		switch nextStrategy {
		case "fill-first":
			selector = &coreauth.FillFirstSelector{}
		default:
			selector = &coreauth.RoundRobinSelector{}
		}

		if nextSessionAffinity {
			ttl := time.Hour
			if ttlStr := strings.TrimSpace(nextSessionAffinityTTL); ttlStr != "" {
				if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
					ttl = parsed
				}
			}
			selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
				Fallback: selector,
				TTL:      ttl,
			})
		}

		s.coreManager.SetSelector(selector)
	}

	s.applyRetryConfig(newCfg)
	s.applyPprofConfig(newCfg)
	if s.server != nil {
		s.server.UpdateClients(newCfg)
	}
	s.cfgMu.Lock()
	s.cfg = newCfg
	s.cfgMu.Unlock()
	if s.coreManager != nil {
		s.coreManager.SetConfig(newCfg)
		s.coreManager.SetOAuthModelAlias(newCfg.OAuthModelAlias)
	}
	if previousHomeSet && !reflect.DeepEqual(previousHome, newCfg.Home) {
		s.reconcileHomeLifetime(context.Background(), newCfg)
	}
	if newCfg.Home.Enabled {
		s.registerHomeExecutors()
	}
	s.registerComboModels(newCfg)
	s.rebindExecutors()
}

func forceHomeRuntimeConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	cfg.APIKeys = nil
	cfg.UsageStatisticsEnabled = true
	cfg.DisableCooling = true
	cfg.WebsocketAuth = false
	cfg.EnableGeminiCLIEndpoint = false
	cfg.RemoteManagement.AllowRemote = false
	cfg.RemoteManagement.DisableControlPanel = true
}

func (s *Service) registerHomeExecutors() {
	if s == nil || s.coreManager == nil || s.cfg == nil {
		return
	}

	// Register baseline executors so home-dispatched auth entries can execute without
	// requiring any local auth-dir credentials.
	s.coreManager.RegisterExecutor(executor.NewCodexAutoExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewClaudeExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewGeminiExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewGeminiVertexExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewGeminiCLIExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewAIStudioExecutor(s.cfg, "", s.wsGateway))
	s.coreManager.RegisterExecutor(executor.NewAntigravityExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewKimiExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewKiroExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewDevinExecutor(s.cfg))
	// Ported from upstream CLIProxyAPI commit 54d4f4c0 (baselineExecutorAuths "meta" entry).
	s.coreManager.RegisterExecutor(executor.NewMetaExecutor(s.cfg))
	s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor("openai-compatibility", s.cfg))
}

func (s *Service) applyHomeOverlay(remoteCfg *config.Config) {
	if s == nil || remoteCfg == nil {
		return
	}

	s.cfgMu.RLock()
	baseCfg := s.cfg
	s.cfgMu.RUnlock()
	if baseCfg == nil {
		return
	}

	merged := *remoteCfg
	merged.Host = baseCfg.Host
	merged.Port = baseCfg.Port
	merged.TLS = baseCfg.TLS
	merged.Home = baseCfg.Home
	forceHomeRuntimeConfig(&merged)

	logHomeConfigChanges(baseCfg, &merged)
	s.applyConfigUpdate(&merged)
}

func logHomeConfigChanges(oldCfg, newCfg *config.Config) {
	if oldCfg == nil || newCfg == nil || !newCfg.Home.Enabled || (!oldCfg.Debug && !newCfg.Debug) {
		return
	}

	details := diff.BuildConfigChangeDetails(oldCfg, newCfg)
	if len(details) == 0 {
		return
	}

	if newCfg.Debug && !log.IsLevelEnabled(log.DebugLevel) {
		util.SetLogLevel(newCfg)
	}

	log.Debugf("home config changes detected:")
	for _, detail := range details {
		log.Debugf("  %s", detail)
	}
}

func (s *Service) startHomeUsageForwarder(ctx context.Context, client *home.Client) {
	if s == nil || client == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	sleep := func(d time.Duration) bool {
		if d <= 0 {
			return true
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if !client.HeartbeatOK() {
				if !sleep(time.Second) {
					return
				}
				continue
			}

			items := redisqueue.PopOldest(64)
			if len(items) == 0 {
				if !sleep(500 * time.Millisecond) {
					return
				}
				continue
			}

			for i := range items {
				if errPush := client.LPushUsage(ctx, items[i]); errPush != nil {
					for j := i; j < len(items); j++ {
						redisqueue.Enqueue(items[j])
					}
					if !sleep(time.Second) {
						return
					}
					break
				}
			}
		}
	}()
}

func (s *Service) startHomeSubscriber(ctx context.Context) {
	if s == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	s.startHomeSubscriberWithConfig(ctx, cfg)
}

func (s *Service) startHomeSubscriberWithConfig(ctx context.Context, cfg *config.Config) {
	if s == nil || cfg == nil || !cfg.Home.Enabled {
		return
	}

	s.homeLifetimeMu.Lock()
	defer s.homeLifetimeMu.Unlock()
	s.stopHomeLifetimeLocked(context.Background())

	homeCtx := ctx
	if homeCtx == nil {
		homeCtx = context.Background()
	}
	homeCtx, cancel := context.WithCancel(homeCtx)
	s.homeCancel = cancel

	client := home.New(cfg.Home)
	if errLifecycle := client.SetLifecycleConfig(cfg.CredentialConcurrency); errLifecycle != nil {
		log.Warnf("failed to apply Home credential concurrency config: %v", errLifecycle)
	}
	registry := executionregistry.New()
	releaseCtx, releaseCancel := context.WithCancel(context.Background())
	releaseFlusher := home.NewReleaseFlusher(client.LimiterConfig, client.PushConcurrencyRelease)
	registry.SetReleaseSink(releaseFlusher.MarkDirty)
	go releaseFlusher.Run(releaseCtx)

	s.homeClient = client
	s.homeRegistry = registry
	s.homeReleaseCancel = releaseCancel
	s.homeReleaseFlusher = releaseFlusher
	home.SetCurrent(client)
	if s.coreManager != nil {
		s.homeGeneration++
		s.homeDispatchBundle = s.coreManager.PublishHomeDispatch(client, registry, s.homeGeneration)
		if publisherConfig, errPublisher := coreauth.HomeInFlightPublisherConfigFromConfig(cfg.CredentialInFlight); errPublisher == nil {
			s.coreManager.ApplyHomeInFlightPublisherConfig(publisherConfig)
		}
		go s.coreManager.StartHomeInFlightPublisher(homeCtx, client, registry)
	}

	go client.StartConfigSubscriber(homeCtx, func(raw []byte) error {
		parsed, err := config.ParseConfigBytes(raw)
		if err != nil {
			log.Warnf("failed to parse home config payload: %v", err)
			return err
		}
		if errLifecycle := client.SetLifecycleConfig(parsed.CredentialConcurrency); errLifecycle != nil {
			return errLifecycle
		}
		s.homeLifetimeMu.Lock()
		defer s.homeLifetimeMu.Unlock()
		if s.homeClient != client {
			return context.Canceled
		}
		if s.coreManager != nil {
			if publisherConfig, errPublisher := coreauth.HomeInFlightPublisherConfigFromConfig(parsed.CredentialInFlight); errPublisher == nil {
				s.coreManager.ApplyHomeInFlightPublisherConfig(publisherConfig)
			}
		}
		registry.ObserveBarrier(parsed.CredentialConcurrency.ObservationBarrierRevision)
		s.applyHomeOverlay(parsed)
		return nil
	})
	s.startHomeUsageForwarder(homeCtx, client)
}

func (s *Service) reconcileHomeLifetime(ctx context.Context, cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	if cfg.Home.Enabled {
		s.startHomeSubscriberWithConfig(ctx, cfg)
		return
	}
	s.stopHomeLifetime(ctx)
}

func (s *Service) stopHomeLifetime(ctx context.Context) {
	if s == nil {
		return
	}
	s.homeLifetimeMu.Lock()
	defer s.homeLifetimeMu.Unlock()
	s.stopHomeLifetimeLocked(ctx)
}

func (s *Service) stopHomeLifetimeLocked(ctx context.Context) {
	if s.homeCancel != nil {
		s.homeCancel()
		s.homeCancel = nil
	}
	if s.coreManager != nil && s.homeDispatchBundle != nil {
		s.coreManager.ClearHomeDispatchBundle(s.homeDispatchBundle)
		s.homeDispatchBundle = nil
	}
	if s.homeRegistry != nil {
		drainCtx := ctx
		if drainCtx == nil {
			drainCtx = context.Background()
		}
		bound := time.Duration(0)
		s.cfgMu.RLock()
		if s.cfg != nil {
			bound = s.cfg.CredentialConcurrency.WithDefaults().CPACancelBound
		}
		s.cfgMu.RUnlock()
		if bound <= 0 {
			bound = 5 * time.Second
		}
		drainCtx, cancelDrain := context.WithTimeout(drainCtx, bound)
		if errDrain := s.homeRegistry.Drain(drainCtx); errDrain != nil {
			log.Warnf("failed to drain Home execution registry: %v", errDrain)
		} else if s.homeReleaseFlusher != nil {
			if errFlush := s.homeReleaseFlusher.Flush(drainCtx); errFlush != nil {
				log.Warnf("failed to flush Home credential releases: %v", errFlush)
			}
		}
		cancelDrain()
		s.homeRegistry = nil
	}
	if s.homeReleaseCancel != nil {
		s.homeReleaseCancel()
		s.homeReleaseCancel = nil
	}
	s.homeReleaseFlusher = nil
	if s.homeClient != nil {
		s.homeClient.Close()
		s.homeClient = nil
	}
	home.ClearCurrent()
}

// Run starts the service and blocks until the context is cancelled or the server stops.
// It initializes all components including authentication, file watching, HTTP server,
// and starts processing requests. The method blocks until the context is cancelled.
//
// Parameters:
//   - ctx: The context for controlling the service lifecycle
//
// Returns:
//   - error: An error if the service fails to start or run
func (s *Service) Run(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("cliproxy: service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	usage.StartDefault(ctx)
	homeEnabled := s.cfg != nil && s.cfg.Home.Enabled
	if homeEnabled {
		forceHomeRuntimeConfig(s.cfg)
		redisqueue.SetUsageStatisticsEnabled(true)
	}

	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if errShutdown := s.Shutdown(shutdownCtx); errShutdown != nil {
			log.Errorf("service shutdown returned error: %v", errShutdown)
		}
	}()

	if !homeEnabled && s.requiresLocalAuthDir() {
		if errEnsureAuthDir := s.ensureAuthDir(); errEnsureAuthDir != nil {
			return errEnsureAuthDir
		}
	}

	if nativeStore, ok := s.managementConfigStore.(nativeproviders.Store); ok {
		if err := nativeproviders.HydrateConfig(ctx, s.cfg, nativeStore); err != nil {
			log.Warnf("failed to hydrate native provider resources at startup: %v", err)
		}
	}

	s.applyRetryConfig(s.cfg)

	if s.coreManager != nil && !homeEnabled {
		if errLoad := s.coreManager.Load(ctx); errLoad != nil {
			log.Warnf("failed to load auth store: %v", errLoad)
		}
		s.registerLoadedCoreAuths(ctx)
		if restored, errRestore := s.coreManager.RestoreCooldownState(ctx); errRestore != nil {
			log.Warnf("failed to restore cooldown state: %v", errRestore)
		} else if restored > 0 {
			log.Debugf("restored %d cooldown state records", restored)
		}
	}

	if !homeEnabled {
		tokenResult, err := s.tokenProvider.Load(ctx, s.cfg)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if tokenResult == nil {
			tokenResult = &TokenClientResult{}
		}

		apiKeyResult, err := s.apiKeyProvider.Load(ctx, s.cfg)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if apiKeyResult == nil {
			apiKeyResult = &APIKeyClientResult{}
		}
	}

	// legacy clients removed; no caches to refresh

	// handlers no longer depend on legacy clients; pass nil slice initially
	serverOptions := append([]api.ServerOption(nil), s.serverOptions...)
	serverOptions = append(serverOptions, api.WithRuntimeStoragePolicy(s.runtimeStoragePolicy))
	if s.managementConfigStore != nil {
		serverOptions = append(serverOptions,
			api.WithManagementConfigStore(s.managementConfigStore),
			api.WithManagementConfigChangeHook(func(newCfg *config.Config) {
				s.applyConfigUpdate(newCfg)
			}),
		)
	}
	if s.runtimeSettingsStore != nil {
		serverOptions = append(serverOptions, api.WithRuntimeSettingsStore(s.runtimeSettingsStore))
	}
	if s.quotaAlertStore != nil {
		serverOptions = append(serverOptions, api.WithQuotaAlertStore(s.quotaAlertStore))
	}
	if s.quotaAlertCipher != nil {
		serverOptions = append(serverOptions, api.WithQuotaAlertSecretCipher(s.quotaAlertCipher))
	}
	s.server = api.NewServer(s.cfg, s.coreManager, s.accessManager, s.configPath, serverOptions...)

	if s.authManager == nil {
		s.authManager = newDefaultAuthManager()
	}

	if homeEnabled {
		s.startHomeSubscriber(ctx)
	}

	s.ensureWebsocketGateway()
	if s.server != nil && s.wsGateway != nil {
		s.server.AttachWebsocketRoute(s.wsGateway.Path(), s.wsGateway.Handler())
		s.server.SetWebsocketAuthChangeHandler(func(oldEnabled, newEnabled bool) {
			if oldEnabled == newEnabled {
				return
			}
			if !oldEnabled && newEnabled {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if errStop := s.wsGateway.Stop(ctx); errStop != nil {
					log.Warnf("failed to reset websocket connections after ws-auth change %t -> %t: %v", oldEnabled, newEnabled, errStop)
					return
				}
				log.Debugf("ws-auth enabled; existing websocket sessions terminated to enforce authentication")
				return
			}
			log.Debugf("ws-auth disabled; existing websocket sessions remain connected")
		})
	}

	if homeEnabled {
		s.registerHomeExecutors()
		// Home mode does not expose in-process Redis RESP usage output; usage is forwarded to home instead.
		redisqueue.SetEnabled(true)
	}

	if s.hooks.OnBeforeStart != nil {
		s.hooks.OnBeforeStart(s.cfg)
	}

	// Register callback for startup and periodic model catalog refresh.
	// When remote model definitions change, re-register models for affected providers.
	// This intentionally rebuilds per-auth model availability from the latest catalog
	// snapshot instead of preserving prior registry suppression state.
	registry.SetModelRefreshCallback(func(changedProviders []string) {
		if s == nil || s.coreManager == nil || len(changedProviders) == 0 {
			return
		}

		providerSet := make(map[string]bool, len(changedProviders))
		for _, p := range changedProviders {
			norm := strings.ToLower(strings.TrimSpace(p))
			if norm != "" {
				providerSet[norm] = true
				switch norm {
				case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
					providerSet["kimi"] = true
					providerSet["kimi-ai"] = true
					providerSet["kimi.ai"] = true
					providerSet["kimi.com"] = true
				}
			}
		}

		auths := s.coreManager.List()
		refreshed := 0
		for _, item := range auths {
			if item == nil || item.ID == "" {
				continue
			}
			auth, ok := s.coreManager.GetByID(item.ID)
			if !ok || auth == nil || auth.Disabled {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(auth.Provider))
			if !providerSet[provider] {
				continue
			}

			if s.refreshModelRegistrationForAuth(auth) {
				refreshed++
			}
		}

		if refreshed > 0 {
			log.Infof("re-registered models for %d auth(s) due to model catalog changes: %v", refreshed, changedProviders)
		}
	})

	s.serverErr = make(chan error, 1)
	go func() {
		if errStart := s.server.Start(); errStart != nil {
			s.serverErr <- errStart
		} else {
			s.serverErr <- nil
		}
	}()

	time.Sleep(100 * time.Millisecond)
	fmt.Printf("API server started successfully on: %s:%d\n", s.cfg.Host, s.cfg.Port)

	s.applyPprofConfig(s.cfg)

	if s.hooks.OnAfterStart != nil {
		s.hooks.OnAfterStart(s)
	}

	if !homeEnabled {
		var watcherWrapper *WatcherWrapper
		reloadCallback := func(newCfg *config.Config) { s.applyConfigUpdate(newCfg) }

		watcherWrapper, errCreate := s.watcherFactory(s.configPath, s.cfg.AuthDir, reloadCallback)
		if errCreate != nil {
			return fmt.Errorf("cliproxy: failed to create watcher: %w", errCreate)
		}
		s.watcher = watcherWrapper
		s.ensureAuthUpdateQueue(ctx)
		if s.authUpdates != nil {
			watcherWrapper.SetAuthUpdateQueue(s.authUpdates)
		}
		watcherWrapper.SetConfig(s.cfg)

		watcherCtx, watcherCancel := context.WithCancel(context.Background())
		s.watcherCancel = watcherCancel
		if errStart := watcherWrapper.Start(watcherCtx); errStart != nil {
			return fmt.Errorf("cliproxy: failed to start watcher: %w", errStart)
		}
		log.Info("runtime storage watcher started for config and auth updates")
	}

	// Prefer core auth manager auto refresh if available.
	if s.coreManager != nil && !homeEnabled {
		interval := 15 * time.Minute
		s.coreManager.StartAutoRefresh(context.Background(), interval)
		log.Infof("core auth auto-refresh started (interval=%s)", interval)
	}
	if s.quotaAlertService != nil && !homeEnabled {
		s.quotaAlertService.Start(context.Background())
		log.Info("quota alert monitor started")
	}

	select {
	case <-ctx.Done():
		log.Debug("service context cancelled, shutting down...")
		return ctx.Err()
	case errServer := <-s.serverErr:
		return errServer
	}
}

// Shutdown gracefully stops background workers and the HTTP server.
// It ensures all resources are properly cleaned up and connections are closed.
// The shutdown is idempotent and can be called multiple times safely.
//
// Parameters:
//   - ctx: The context for controlling the shutdown timeout
//
// Returns:
//   - error: An error if shutdown fails
func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var shutdownErr error
	s.shutdownOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}

		s.stopHomeLifetime(ctx)
		if s.quotaAlertService != nil {
			s.quotaAlertService.Stop()
		}
		redisqueue.FlushUsage(ctx)

		// legacy refresh loop removed; only stopping core auth manager below

		if s.watcherCancel != nil {
			s.watcherCancel()
		}
		if s.coreManager != nil {
			s.coreManager.StopAutoRefresh()
		}
		if s.watcher != nil {
			if err := s.watcher.Stop(); err != nil {
				log.Errorf("failed to stop file watcher: %v", err)
				shutdownErr = err
			}
		}
		if s.wsGateway != nil {
			if err := s.wsGateway.Stop(ctx); err != nil {
				log.Errorf("failed to stop websocket gateway: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}
		if s.authQueueStop != nil {
			s.authQueueStop()
			s.authQueueStop = nil
		}

		if errShutdownPprof := s.shutdownPprof(ctx); errShutdownPprof != nil {
			log.Errorf("failed to stop pprof server: %v", errShutdownPprof)
			if shutdownErr == nil {
				shutdownErr = errShutdownPprof
			}
		}

		// no legacy clients to persist

		if s.server != nil {
			if errStop := s.server.Stop(ctx); errStop != nil {
				log.Errorf("error stopping API server: %v", errStop)
				if shutdownErr == nil {
					shutdownErr = errStop
				}
			}
		}

		usage.StopDefault()
	})
	return shutdownErr
}

func (s *Service) ensureAuthDir() error {
	info, err := os.Stat(s.cfg.AuthDir)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(s.cfg.AuthDir, 0o755); mkErr != nil {
				return fmt.Errorf("cliproxy: failed to create auth directory %s: %w", s.cfg.AuthDir, mkErr)
			}
			log.Infof("created missing auth directory: %s", s.cfg.AuthDir)
			return nil
		}
		return fmt.Errorf("cliproxy: error checking auth directory %s: %w", s.cfg.AuthDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cliproxy: auth path exists but is not a directory: %s", s.cfg.AuthDir)
	}
	return nil
}

func (s *Service) requiresLocalAuthDir() bool {
	if s == nil {
		return false
	}
	return !s.runtimeStoragePolicy.PostgresDurableMode()
}

// registerModelsForAuth (re)binds provider models in the global registry using the core auth ID as client identifier.
func (s *Service) registerModelsForAuth(a *coreauth.Auth) bool {
	return s.completeModelRegistrationForAuth(context.Background(), a)
}

func (s *Service) completeModelRegistrationForAuth(ctx context.Context, auth *coreauth.Auth) bool {
	if s == nil || auth == nil || auth.ID == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	targetAuth := auth
	if s.coreManager != nil {
		if latest, ok := s.latestAuthForModelRegistration(auth.ID); ok && latest != nil {
			targetAuth = latest
		}
	}
	published := s.registerModelsForAuthDirect(ctx, targetAuth)
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if !published && s.coreManager != nil {
		// The registration was fenced out because its registration inputs
		// or the registry epoch changed mid-computation. Reconcile the latest
		// auth once instead of risking an empty model list. The retry
		// recomputes from the latest snapshot and never republishes the superseded computation's models.
		if latest, ok := s.latestAuthForModelRegistration(auth.ID); ok && latest != nil {
			targetAuth = latest
			if latest.Disabled {
				accepted := s.unregisterModelsForAuthFenced(latest, newModelRegistrationFence(latest, s.cfg))
				log.WithFields(log.Fields{"auth_id": latest.ID, "provider": latest.Provider, "superseded": !accepted}).Debugf("model registration reconciliation attempted for disabled auth %s", latest.ID)
			} else {
				accepted := s.registerModelsForAuthDirect(ctx, targetAuth)
				if ctx != nil && ctx.Err() != nil {
					return false
				}
				log.WithFields(log.Fields{"auth_id": latest.ID, "provider": latest.Provider, "superseded": !accepted}).Debugf("model registration reconciliation attempted for auth %s", latest.ID)
			}
		}
	}
	if s.coreManager != nil {
		s.coreManager.ReconcileRegistryModelStates(ctx, targetAuth.ID)
		s.coreManager.RefreshSchedulerEntry(targetAuth.ID)
	}
	return true
}

func (s *Service) registerModelsForAuthDirect(ctx context.Context, a *coreauth.Auth) bool {
	if a == nil || a.ID == "" {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	// Capture the publication fence once before any runtime read or model resolution.
	fence := newModelRegistrationFence(a, s.cfg)
	if a.Disabled {
		if s != nil && s.coreManager != nil {
			if current, ok := s.coreManager.GetByID(a.ID); ok && current != nil && !current.Disabled {
				return false
			}
		}
		return s.unregisterModelsForAuthFenced(a, fence)
	}
	if s != nil && s.coreManager != nil {
		if current, ok := s.coreManager.GetByID(a.ID); !ok || current == nil || current.Disabled {
			return true
		}
	}
	authKind := strings.ToLower(strings.TrimSpace(a.Attributes["auth_kind"]))
	if authKind == "" {
		if kind, _ := a.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = "apikey"
		}
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["gemini_virtual_primary"]); strings.EqualFold(v, "true") {
			return s.unregisterModelsForAuthFenced(a, fence)
		}
	}
	// Unregister legacy client ID (if present) to avoid double counting
	if a.Runtime != nil {
		if idGetter, ok := a.Runtime.(interface{ GetClientID() string }); ok {
			if rid := idGetter.GetClientID(); rid != "" && rid != a.ID {
				GlobalModelRegistry().UnregisterClient(rid)
			}
		}
	}
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	compatProviderKey, compatDisplayName, compatDetected := openAICompatInfoFromAuth(a)
	if compatDetected {
		provider = "openai-compatibility"
	}
	excluded := s.oauthExcludedModels(provider, authKind)
	// Credential-specific exclusions take priority and override the global config.
	// If this attribute is present, it represents the effective list of exclusions for this credential.
	if a.Attributes != nil {
		if val, ok := a.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	var models []*ModelInfo
	switch provider {
	case "gemini":
		models = registry.GetGeminiModels()
		if entry := s.resolveConfigGeminiKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildGeminiConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "vertex":
		// Vertex AI Gemini supports the same model identifiers as Gemini.
		models = registry.GetGeminiVertexModels()
		if entry := s.resolveConfigVertexCompatKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildVertexCompatConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "gemini-cli":
		models = registry.GetGeminiCLIModels()
		models = applyExcludedModels(models, excluded)
	case "aistudio":
		models = registry.GetAIStudioModels()
		models = applyExcludedModels(models, excluded)
	case "antigravity":
		models = registry.GetAntigravityModels()
		models = applyExcludedModels(models, excluded)
	case "claude":
		models = registry.GetClaudeModels()
		if entry := s.resolveConfigClaudeKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildClaudeConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "codex":
		if authKind == "apikey" {
			if entry := s.resolveConfigCodexKey(a); entry != nil {
				models = buildCodexConfigModels(entry)
				excluded = entry.ExcludedModels
			}
			models = applyExcludedModels(models, excluded)
			break
		}
		codexPlanType := ""
		if a.Attributes != nil {
			codexPlanType = strings.TrimSpace(a.Attributes["plan_type"])
		}
		switch strings.ToLower(codexPlanType) {
		case "pro":
			models = registry.GetCodexProModels()
		case "plus":
			models = registry.GetCodexPlusModels()
		case "team", "business", "go":
			models = registry.GetCodexTeamModels()
		case "free":
			models = registry.GetCodexFreeModels()
		default:
			models = registry.GetCodexProModels()
		}
		models = applyExcludedModels(models, excluded)
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		models = registry.GetKimiModels()
		models = applyExcludedModels(models, excluded)
	case "xai":
		models = registry.GetXAIModels()
		models = applyExcludedModels(models, excluded)
	case "kiro":
		models = registry.GetKiroModels()
		if liveModels, refreshedAuth, err := s.resolveKiroModelsForAuth(a); err != nil {
			log.Warnf("kiro models: live model load failed for %s, using static fallback: %v", a.ID, err)
		} else if len(liveModels) > 0 {
			models = liveModels
			if refreshedAuth != nil && s.coreManager != nil {
				if _, errUpdate := s.coreManager.Update(context.Background(), refreshedAuth); errUpdate != nil {
					log.Warnf("kiro models: failed to persist refreshed auth %s: %v", refreshedAuth.ID, errUpdate)
				}
			}
		}
		models = applyExcludedModels(models, excluded)
	case "meta":
		models = registry.GetMetaModels()
		if entry := s.resolveConfigMetaKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildMetaConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	default:
		// Handle OpenAI-compatibility providers by name using config
		if s.cfg != nil {
			providerKey := provider
			compatName := strings.TrimSpace(a.Provider)
			isCompatAuth := false
			if compatDetected {
				if compatProviderKey != "" {
					providerKey = compatProviderKey
				}
				if compatDisplayName != "" {
					compatName = compatDisplayName
				}
				isCompatAuth = true
			}
			if strings.EqualFold(providerKey, "openai-compatibility") {
				isCompatAuth = true
				if a.Attributes != nil {
					if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
						compatName = v
					}
					if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
						providerKey = strings.ToLower(v)
						isCompatAuth = true
					}
				}
				if providerKey == "openai-compatibility" && compatName != "" {
					providerKey = strings.ToLower(compatName)
				}
			} else if a.Attributes != nil {
				if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
					compatName = v
					isCompatAuth = true
				}
				if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
					providerKey = strings.ToLower(v)
					isCompatAuth = true
				}
			}
			for i := range s.cfg.OpenAICompatibility {
				compat := &s.cfg.OpenAICompatibility[i]
				if compat.Disabled {
					continue
				}
				if strings.EqualFold(compat.Name, compatName) {
					isCompatAuth = true
					ms := buildOpenAICompatibilityConfigModels(compat)
					// Register and return
					if len(ms) > 0 {
						if providerKey == "" {
							providerKey = "openai-compatibility"
						}
						return s.registerResolvedModelsForAuthFenced(a, fence, providerKey, applyModelPrefixes(ms, a.Prefix, s.cfg.ForceModelPrefix))
					}
					// Ensure stale registrations are cleared when model list becomes empty.
					return s.unregisterModelsForAuthFenced(a, fence)
				}
			}
			if isCompatAuth {
				// No matching provider found or models removed entirely; drop any prior registration.
				return s.unregisterModelsForAuthFenced(a, fence)
			}
		}
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	models = applyOAuthModelAlias(s.cfg, provider, authKind, models)
	if len(models) > 0 {
		key := provider
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(a.Provider))
		}
		return s.registerResolvedModelsForAuthFenced(a, fence, key, applyModelPrefixes(models, a.Prefix, s.cfg != nil && s.cfg.ForceModelPrefix))
	}

	return s.unregisterModelsForAuthFenced(a, fence)
}

func (s *Service) resolveKiroModelsForAuth(a *coreauth.Auth) ([]*ModelInfo, *coreauth.Auth, error) {
	if a == nil || a.Disabled {
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	return executor.NewKiroExecutor(s.cfg).ResolveModels(ctx, a)
}

func (s *Service) registerLoadedCoreAuths(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	loaded := s.coreManager.List()
	for _, auth := range loaded {
		if auth == nil || auth.ID == "" {
			continue
		}
		if !auth.Disabled {
			s.ensureExecutorsForAuth(auth)
		}
		s.completeModelRegistrationForAuth(ctx, auth)
	}
}

// refreshModelRegistrationForAuth re-applies the latest model registration for
// one auth and reconciles any concurrent auth changes that race with the
// refresh. Callers are expected to pre-filter provider membership.
//
// Re-registration is deliberate: registry cooldown/suspension state is treated
// as part of the previous registration snapshot and is cleared when the auth is
// rebound to the refreshed model catalog.
func (s *Service) refreshModelRegistrationForAuth(current *coreauth.Auth) bool {
	if s == nil || s.coreManager == nil || current == nil || current.ID == "" {
		return false
	}

	if !current.Disabled {
		s.ensureExecutorsForAuth(current)
	}
	s.registerModelsForAuthDirect(context.Background(), current)
	s.coreManager.ReconcileRegistryModelStates(context.Background(), current.ID)

	latest, ok := s.latestAuthForModelRegistration(current.ID)
	if !ok || latest == nil {
		// If the auth is absent, applyCoreAuthRemoval owns unregistration.
		return false
	}
	if latest.Disabled {
		s.unregisterModelsForAuthFenced(latest, newModelRegistrationFence(latest, s.cfg))
		s.coreManager.RefreshSchedulerEntry(current.ID)
		return false
	}

	// Re-apply the latest auth snapshot so concurrent auth updates cannot leave
	// stale model registrations behind. This may duplicate registration work when
	// no auth fields changed, but keeps the refresh path simple and correct.
	s.ensureExecutorsForAuth(latest)
	s.registerModelsForAuthDirect(context.Background(), latest)
	s.coreManager.ReconcileRegistryModelStates(context.Background(), latest.ID)
	s.coreManager.RefreshSchedulerEntry(current.ID)
	return true
}

// latestAuthForModelRegistration returns the latest auth snapshot regardless of
// provider membership. Callers use this after a registration attempt to restore
// whichever state currently owns the client ID in the global registry.
func (s *Service) latestAuthForModelRegistration(authID string) (*coreauth.Auth, bool) {
	if s == nil || s.coreManager == nil || authID == "" {
		return nil, false
	}
	auth, ok := s.coreManager.GetByID(authID)
	if !ok || auth == nil || auth.ID == "" {
		return nil, false
	}
	return auth, true
}

func (s *Service) resolveConfigClaudeKey(auth *coreauth.Auth) *config.ClaudeKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.ClaudeKey {
		entry := &s.cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range s.cfg.ClaudeKey {
			entry := &s.cfg.ClaudeKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func (s *Service) resolveConfigGeminiKey(auth *coreauth.Auth) *config.GeminiKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.GeminiKey {
		entry := &s.cfg.GeminiKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) resolveConfigVertexCompatKey(auth *coreauth.Auth) *config.VertexCompatKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.VertexCompatAPIKey {
		entry := &s.cfg.VertexCompatAPIKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range s.cfg.VertexCompatAPIKey {
			entry := &s.cfg.VertexCompatAPIKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func (s *Service) resolveConfigCodexKey(auth *coreauth.Auth) *config.CodexKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.CodexKey {
		entry := &s.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

// resolveConfigMetaKey returns the config meta-api-key entry matching the auth's
// api_key/base_url attributes. Mirrors resolveConfigCodexKey; upstream's
// resolveConfigCodexStyleKey (service_models.go, commit 54d4f4c0) shares the
// same credential-matching semantics.
func (s *Service) resolveConfigMetaKey(auth *coreauth.Auth) *config.MetaKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	// Fast path: upstream resolveConfigCodexStyleKey trusts the synthesized
	// config_index for meta (validateIndexCredentials=false).
	if auth.Attributes != nil {
		if idxStr := strings.TrimSpace(auth.Attributes["config_index"]); idxStr != "" {
			if parsed, errIndex := strconv.Atoi(idxStr); errIndex == nil && parsed >= 0 && parsed < len(s.cfg.MetaKey) {
				return &s.cfg.MetaKey[parsed]
			}
		}
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.MetaKey {
		entry := &s.cfg.MetaKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) oauthExcludedModels(provider, authKind string) []string {
	cfg := s.cfg
	if cfg == nil {
		return nil
	}
	authKindKey := strings.ToLower(strings.TrimSpace(authKind))
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if authKindKey == "apikey" {
		return nil
	}
	return cfg.OAuthExcludedModels[providerKey]
}

func applyExcludedModels(models []*ModelInfo, excluded []string) []*ModelInfo {
	if len(models) == 0 || len(excluded) == 0 {
		return models
	}

	patterns := make([]string, 0, len(excluded))
	for _, item := range excluded {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			patterns = append(patterns, strings.ToLower(trimmed))
		}
	}
	if len(patterns) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.ToLower(strings.TrimSpace(model.ID))
		blocked := false
		for _, pattern := range patterns {
			if matchWildcard(pattern, modelID) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// cloneModelInfoForCatalogRoute copies model into a catalog-route entry while
// deep-copying NativeCapabilities so prefixed/aliased catalog entries cannot
// mutate the source model's capability metadata. Ported from upstream
// CLIProxyAPI sdk/cliproxy/service_models.go (4311ae874774).
func cloneModelInfoForCatalogRoute(model *ModelInfo) ModelInfo {
	clone := *model
	if model.NativeCapabilities != nil {
		capabilities := *model.NativeCapabilities
		if model.NativeCapabilities.WebSearch != nil {
			webSearch := *model.NativeCapabilities.WebSearch
			capabilities.WebSearch = &webSearch
		}
		clone.NativeCapabilities = &capabilities
	}
	return clone
}

func applyModelPrefixes(models []*ModelInfo, prefix string, forceModelPrefix bool) []*ModelInfo {
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPrefix == "" || len(models) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)

	addModel := func(model *ModelInfo) {
		if model == nil {
			return
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		baseID := strings.TrimSpace(model.ID)
		if baseID == "" {
			continue
		}
		if !forceModelPrefix || trimmedPrefix == baseID {
			addModel(model)
		}
		clone := cloneModelInfoForCatalogRoute(model)
		clone.ID = trimmedPrefix + "/" + baseID
		// Ported from upstream CLIProxyAPI commit 8f23ad029144: prefixed catalog
		// entries resolve client metadata through the canonical model ID.
		if clone.MetadataModelID == "" {
			clone.MetadataModelID = baseID
		}
		clone.ExplicitThinking = model.ExplicitThinking
		clone.ExplicitInputModalities = model.ExplicitInputModalities
		addModel(&clone)
	}
	return out
}

// matchWildcard performs case-insensitive wildcard matching where '*' matches any substring.
func matchWildcard(pattern, value string) bool {
	if pattern == "" {
		return false
	}

	// Fast path for exact match (no wildcard present).
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	// Handle prefix.
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}

	// Handle suffix.
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}

	// Handle middle segments in order.
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}

	return true
}

type modelEntry interface {
	GetName() string
	GetAlias() string
	GetDisplayName() string
}

type modelMaxContextLengthEntry interface {
	GetMaxContextLength() int
}

// modelCompatEntry exposes the is-compat flag on configured API-key model
// entries. Ported from upstream CLIProxyAPI commit 81d6ba774621
// (service_models.go modelCompatEntry).
type modelCompatEntry interface {
	GetIsCompat() bool
}

// modelThinkingEntry exposes explicit thinking/reasoning configuration on model
// entries that support it. Ported from upstream CLIProxyAPI commit 8f23ad029144
// (service_models.go modelEntry.GetThinking usage).
type modelThinkingEntry interface {
	GetThinking() *registry.ThinkingSupport
}

func buildConfiguredModelInfo(model modelEntry, ownedBy, modelType string, created int64, fallbackDisplayName string, userDefined bool) *ModelInfo {
	name := strings.TrimSpace(model.GetName())
	alias := strings.TrimSpace(model.GetAlias())
	if alias == "" {
		alias = name
	}
	if alias == "" {
		return nil
	}
	displayName := strings.TrimSpace(model.GetDisplayName())
	if displayName == "" {
		displayName = fallbackDisplayName
	}
	if displayName == "" {
		displayName = alias
	}
	// Ported from upstream CLIProxyAPI commit 8f23ad029144: configured aliases
	// resolve client metadata through their upstream model name.
	metadataModelID := name
	if metadataModelID == "" {
		metadataModelID = alias
	}
	info := &ModelInfo{
		ID:              alias,
		MetadataModelID: metadataModelID,
		Object:          "model",
		Created:         created,
		OwnedBy:         ownedBy,
		Type:            modelType,
		DisplayName:     displayName,
		UserDefined:     userDefined,
	}
	if maxContextModel, okMaxContext := any(model).(modelMaxContextLengthEntry); okMaxContext {
		if maxContextLength := maxContextModel.GetMaxContextLength(); maxContextLength > 0 {
			info.ContextLength = maxContextLength
			info.MaxContextLength = maxContextLength
		}
	}
	// Ported from upstream CLIProxyAPI commit 81d6ba774621.
	if compatModel, okCompat := any(model).(modelCompatEntry); okCompat {
		info.IsCompat = compatModel.GetIsCompat()
	}
	return info
}

func buildOpenAICompatibilityConfigModels(compat *config.OpenAICompatibility) []*ModelInfo {
	if compat == nil || len(compat.Models) == 0 {
		return nil
	}
	now := time.Now().Unix()
	models := make([]*ModelInfo, 0, len(compat.Models))
	for i := range compat.Models {
		model := compat.Models[i]
		modelType := "openai-compatibility"
		if model.Image {
			modelType = registry.OpenAIImageModelType
		}
		info := buildConfiguredModelInfo(model, compat.Name, modelType, now, strings.TrimSpace(model.Alias), false)
		if info == nil {
			continue
		}
		thinking := model.Thinking
		if thinking == nil && !model.Image {
			thinking = &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
		}
		// Ported from upstream CLIProxyAPI commit 8f23ad029144: explicit config
		// flags gate provider-capability constraints in Codex client metadata.
		if model.Thinking != nil {
			info.ExplicitThinking = true
		}
		if len(model.InputModalities) > 0 {
			info.ExplicitInputModalities = true
		}
		info.Thinking = thinking
		info.SupportedInputModalities = normalizeCompatConfigModalities(model.InputModalities)
		models = append(models, info)
	}
	return models
}

// normalizeCompatConfigModalities lowercases, trims, and deduplicates configured
// modality names. Ported from upstream CLIProxyAPI (sdk/cliproxy/service_models.go).
func normalizeCompatConfigModalities(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		modality := strings.ToLower(strings.TrimSpace(item))
		if modality == "" {
			continue
		}
		if _, exists := seen[modality]; exists {
			continue
		}
		seen[modality] = struct{}{}
		out = append(out, modality)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildConfigModels[T modelEntry](models []T, ownedBy, modelType, metadataChannel string) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for i := range models {
		model := models[i]
		name := strings.TrimSpace(model.GetName())
		info := buildConfiguredModelInfo(model, ownedBy, modelType, now, name, true)
		if info == nil {
			continue
		}
		key := strings.ToLower(info.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if name != "" {
			if upstream := registry.LookupStaticModelInfo(name); upstream != nil && upstream.Thinking != nil {
				info.Thinking = upstream.Thinking
			}
			if staticInfo := registry.LookupStaticModelInfoByChannel(name, metadataChannel); staticInfo != nil && staticInfo.NativeCapabilities != nil {
				info.NativeCapabilities = cloneModelInfoForCatalogRoute(staticInfo).NativeCapabilities
			}
		}
		// Ported from upstream CLIProxyAPI commit 8f23ad029144: an explicit
		// thinking configuration on the model entry marks the capability as
		// user-defined and takes precedence over static registry metadata.
		if thinkingModel, okThinking := any(model).(modelThinkingEntry); okThinking {
			if configured := thinkingModel.GetThinking(); configured != nil {
				info.ExplicitThinking = true
				info.Thinking = configured
			}
		}
		out = append(out, info)
	}
	return out
}

func buildVertexCompatConfigModels(entry *config.VertexCompatKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "vertex", "vertex")
}

func buildGeminiConfigModels(entry *config.GeminiKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "gemini", "gemini")
}

func buildClaudeConfigModels(entry *config.ClaudeKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "anthropic", "claude", "claude")
}

// buildMetaConfigModels builds ModelInfo entries for a meta-api-key config
// credential. Ported from upstream CLIProxyAPI commit 54d4f4c0
// (sdk/cliproxy/service_models.go).
func buildMetaConfigModels(entry *config.MetaKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "meta", "meta", "meta")
}

func buildCodexConfigModels(entry *config.CodexKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	if len(entry.Models) == 0 {
		models := registry.GetCodexProModels()
		for _, model := range models {
			if model != nil {
				model.SupportConfigurationUpdate = false
			}
		}
		return models
	}
	models := buildConfigModels(entry.Models, "openai", "openai", "codex")
	configuredDisplayNames := make(map[string]string, len(entry.Models))
	configuredConfigurationUpdates := make(map[string]bool, len(entry.Models))
	seenConfiguredModels := make(map[string]struct{}, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		alias := strings.TrimSpace(model.Alias)
		if alias == "" {
			alias = strings.TrimSpace(model.Name)
		}
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if _, exists := seenConfiguredModels[key]; exists {
			continue
		}
		seenConfiguredModels[key] = struct{}{}
		configuredConfigurationUpdates[key] = model.SupportConfigurationUpdate
		if displayName := strings.TrimSpace(model.DisplayName); displayName != "" {
			configuredDisplayNames[key] = displayName
		}
	}
	for _, model := range models {
		if model == nil {
			continue
		}
		key := strings.ToLower(model.ID)
		if displayName, ok := configuredDisplayNames[key]; ok {
			model.DisplayName = displayName
		}
		model.SupportConfigurationUpdate = configuredConfigurationUpdates[key]
	}
	return models
}

func rewriteModelInfoName(name, oldID, newID string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return name
	}
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	if oldID == "" || newID == "" {
		return name
	}
	if strings.EqualFold(oldID, newID) {
		return name
	}
	if strings.EqualFold(trimmed, oldID) {
		return newID
	}
	if strings.HasSuffix(trimmed, "/"+oldID) {
		prefix := strings.TrimSuffix(trimmed, oldID)
		return prefix + newID
	}
	if trimmed == "models/"+oldID {
		return "models/" + newID
	}
	return name
}

func applyOAuthModelAlias(cfg *config.Config, provider, authKind string, models []*ModelInfo) []*ModelInfo {
	if cfg == nil || len(models) == 0 {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, authKind)
	if channel == "" || len(cfg.OAuthModelAlias) == 0 {
		return models
	}
	aliases := cfg.OAuthModelAlias[channel]
	if len(aliases) == 0 {
		return models
	}

	type aliasEntry struct {
		alias       string
		displayName string
		fork        bool
	}

	forward := make(map[string][]aliasEntry, len(aliases))
	for i := range aliases {
		name := strings.TrimSpace(aliases[i].Name)
		alias := strings.TrimSpace(aliases[i].Alias)
		if name == "" || alias == "" {
			continue
		}
		if strings.EqualFold(name, alias) {
			continue
		}
		key := strings.ToLower(name)
		forward[key] = append(forward[key], aliasEntry{
			alias:       alias,
			displayName: strings.TrimSpace(aliases[i].DisplayName),
			fork:        aliases[i].Fork,
		})
	}
	if len(forward) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		entries := forward[key]
		if len(entries) == 0 {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
			continue
		}

		keepOriginal := false
		for _, entry := range entries {
			if entry.fork {
				keepOriginal = true
				break
			}
		}
		if keepOriginal {
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				out = append(out, model)
			}
		}

		addedAlias := false
		for _, entry := range entries {
			mappedID := strings.TrimSpace(entry.alias)
			if mappedID == "" {
				continue
			}
			if strings.EqualFold(mappedID, id) {
				continue
			}
			aliasKey := strings.ToLower(mappedID)
			if _, exists := seen[aliasKey]; exists {
				continue
			}
			seen[aliasKey] = struct{}{}
			clone := cloneModelInfoForCatalogRoute(model)
			clone.ID = mappedID
			// Ported from upstream CLIProxyAPI commit 8f23ad029144: aliased
			// models keep (or gain) the canonical model ID used to resolve
			// client-facing template metadata.
			if model.MetadataModelID != "" {
				clone.MetadataModelID = model.MetadataModelID
			} else {
				clone.MetadataModelID = id
			}
			clone.ExplicitThinking = model.ExplicitThinking
			clone.ExplicitInputModalities = model.ExplicitInputModalities
			if entry.displayName != "" {
				clone.DisplayName = entry.displayName
			}
			if clone.Name != "" {
				clone.Name = rewriteModelInfoName(clone.Name, id, mappedID)
			}
			out = append(out, &clone)
			addedAlias = true
		}

		if !keepOriginal && !addedAlias {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
		}
	}
	return out
}
