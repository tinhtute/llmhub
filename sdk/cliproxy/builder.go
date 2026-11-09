// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"fmt"
	"strings"
	"time"

	configaccess "github.com/therealtinhtute/llmhub/internal/access/config_access"
	"github.com/therealtinhtute/llmhub/internal/api"
	"github.com/therealtinhtute/llmhub/internal/quotaalert"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/therealtinhtute/llmhub/internal/runtimecontrol"
	"github.com/therealtinhtute/llmhub/internal/runtimepolicy"
	sdkaccess "github.com/therealtinhtute/llmhub/sdk/access"
	sdkAuth "github.com/therealtinhtute/llmhub/sdk/auth"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/therealtinhtute/llmhub/sdk/config"
)

// Builder constructs a Service instance with customizable providers.
// It provides a fluent interface for configuring all aspects of the service
// including authentication, file watching, HTTP server options, and lifecycle hooks.
type Builder struct {
	// cfg holds the application configuration.
	cfg *config.Config

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

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// cooldownStateStore persists runtime cooldown snapshots.
	cooldownStateStore coreauth.CooldownStateStore

	// serverOptions contains additional server configuration options.
	serverOptions []api.ServerOption

	// managementConfigStore persists management config changes outside the filesystem.
	managementConfigStore api.ManagementConfigStore
	runtimeSettingsStore  runtimecontrol.SettingsStore

	runtimeStoragePolicy runtimepolicy.RuntimeStorage

	quotaAlertService *quotaalert.Service
	quotaAlertStore   quotaalert.Store
	quotaAlertCipher  *quotaalert.SecretCipher
}

// Hooks allows callers to plug into service lifecycle stages.
// These callbacks provide opportunities to perform custom initialization
// and cleanup operations during service startup and shutdown.
type Hooks struct {
	// OnBeforeStart is called before the service starts, allowing configuration
	// modifications or additional setup.
	OnBeforeStart func(*config.Config)

	// OnAfterStart is called after the service has started successfully,
	// providing access to the service instance for additional operations.
	OnAfterStart func(*Service)
}

// NewBuilder creates a Builder with default dependencies left unset.
// Use the fluent interface methods to configure the service before calling Build().
//
// Returns:
//   - *Builder: A new builder instance ready for configuration
func NewBuilder() *Builder {
	return &Builder{}
}

// WithConfig sets the configuration instance used by the service.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *Builder: The builder instance for method chaining
func (b *Builder) WithConfig(cfg *config.Config) *Builder {
	b.cfg = cfg
	return b
}

// WithConfigPath sets the absolute configuration file path used for reload watching.
//
// Parameters:
//   - path: The absolute path to the configuration file
//
// Returns:
//   - *Builder: The builder instance for method chaining
func (b *Builder) WithConfigPath(path string) *Builder {
	b.configPath = path
	return b
}

// WithTokenClientProvider overrides the provider responsible for token-backed clients.
func (b *Builder) WithTokenClientProvider(provider TokenClientProvider) *Builder {
	b.tokenProvider = provider
	return b
}

// WithAPIKeyClientProvider overrides the provider responsible for API key-backed clients.
func (b *Builder) WithAPIKeyClientProvider(provider APIKeyClientProvider) *Builder {
	b.apiKeyProvider = provider
	return b
}

// WithWatcherFactory allows customizing the watcher factory that handles reloads.
func (b *Builder) WithWatcherFactory(factory WatcherFactory) *Builder {
	b.watcherFactory = factory
	return b
}

// WithHooks registers lifecycle hooks executed around service startup.
func (b *Builder) WithHooks(h Hooks) *Builder {
	b.hooks = h
	return b
}

// WithAuthManager overrides the authentication manager used for token lifecycle operations.
func (b *Builder) WithAuthManager(mgr *sdkAuth.Manager) *Builder {
	b.authManager = mgr
	return b
}

// WithRequestAccessManager overrides the request authentication manager.
func (b *Builder) WithRequestAccessManager(mgr *sdkaccess.Manager) *Builder {
	b.accessManager = mgr
	return b
}

// WithCoreAuthManager overrides the runtime auth manager responsible for request execution.
func (b *Builder) WithCoreAuthManager(mgr *coreauth.Manager) *Builder {
	b.coreManager = mgr
	return b
}

// WithCooldownStateStore overrides the runtime cooldown snapshot store.
func (b *Builder) WithCooldownStateStore(store coreauth.CooldownStateStore) *Builder {
	b.cooldownStateStore = store
	return b
}

// WithServerOptions appends server configuration options used during construction.
func (b *Builder) WithServerOptions(opts ...api.ServerOption) *Builder {
	b.serverOptions = append(b.serverOptions, opts...)
	return b
}

// WithLocalManagementPassword configures a password that is only accepted from localhost management requests.
func (b *Builder) WithLocalManagementPassword(password string) *Builder {
	if password == "" {
		return b
	}
	b.serverOptions = append(b.serverOptions, api.WithLocalManagementPassword(password))
	return b
}

// WithPostAuthHook registers a hook to be called after an Auth record is created
// but before it is persisted to storage.
func (b *Builder) WithPostAuthHook(hook coreauth.PostAuthHook) *Builder {
	if hook == nil {
		return b
	}
	b.serverOptions = append(b.serverOptions, api.WithPostAuthHook(hook))
	return b
}

func (b *Builder) WithManagementConfigStore(store api.ManagementConfigStore) *Builder {
	b.managementConfigStore = store
	if runtimeStore, ok := store.(runtimecontrol.SettingsStore); ok {
		b.runtimeSettingsStore = runtimeStore
	}
	return b
}

func (b *Builder) WithRuntimeSettingsStore(store runtimecontrol.SettingsStore) *Builder {
	b.runtimeSettingsStore = store
	return b
}

func (b *Builder) WithRuntimeStoragePolicy(policy runtimepolicy.RuntimeStorage) *Builder {
	b.runtimeStoragePolicy = policy
	return b
}

func (b *Builder) WithQuotaAlertService(service *quotaalert.Service) *Builder {
	b.quotaAlertService = service
	return b
}

func (b *Builder) WithQuotaAlertStore(store quotaalert.Store) *Builder {
	b.quotaAlertStore = store
	return b
}

func (b *Builder) WithQuotaAlertSecretCipher(cipher *quotaalert.SecretCipher) *Builder {
	b.quotaAlertCipher = cipher
	return b
}

// Build validates inputs, applies defaults, and returns a ready-to-run service.
func (b *Builder) Build() (*Service, error) {
	if b.cfg == nil {
		return nil, fmt.Errorf("cliproxy: configuration is required")
	}
	if b.configPath == "" {
		return nil, fmt.Errorf("cliproxy: configuration path is required")
	}

	tokenProvider := b.tokenProvider
	if tokenProvider == nil {
		tokenProvider = NewFileTokenClientProvider()
	}

	apiKeyProvider := b.apiKeyProvider
	if apiKeyProvider == nil {
		apiKeyProvider = NewAPIKeyClientProvider()
	}

	watcherFactory := b.watcherFactory
	if watcherFactory == nil {
		watcherFactory = defaultWatcherFactory
	}

	authManager := b.authManager
	if authManager == nil {
		authManager = newDefaultAuthManager()
	}

	accessManager := b.accessManager
	if accessManager == nil {
		accessManager = sdkaccess.NewManager()
	}

	configaccess.Register(&b.cfg.SDKConfig)
	accessManager.SetProviders(sdkaccess.RegisteredProviders())

	quotaAlertHook := newQuotaAlertResultHook()
	coreManager := b.coreManager
	if coreManager != nil {
		// Custom manager: compose the wake hook with the embedder's own hook.
		coreManager.AddHook(quotaAlertHook)
	}
	if coreManager == nil {
		tokenStore := sdkAuth.GetTokenStore()
		if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && b.cfg != nil {
			dirSetter.SetBaseDir(b.cfg.AuthDir)
		}

		strategy := ""
		sessionAffinity := false
		sessionAffinityTTL := time.Hour
		if b.cfg != nil {
			strategy = strings.ToLower(strings.TrimSpace(b.cfg.Routing.Strategy))
			// Support both legacy ClaudeCodeSessionAffinity and new universal SessionAffinity
			sessionAffinity = b.cfg.Routing.SessionAffinity
			if ttlStr := strings.TrimSpace(b.cfg.Routing.SessionAffinityTTL); ttlStr != "" {
				if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
					sessionAffinityTTL = parsed
				}
			}
		}
		var selector coreauth.Selector
		switch strategy {
		case "fill-first", "fillfirst", "ff":
			selector = &coreauth.FillFirstSelector{}
		default:
			selector = &coreauth.RoundRobinSelector{}
		}

		// Wrap with session affinity if enabled (failover is always on)
		if sessionAffinity {
			selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
				Fallback: selector,
				TTL:      sessionAffinityTTL,
			})
		}

		coreManager = coreauth.NewManager(tokenStore, selector, quotaAlertHook)
	}
	cooldownStateStore := b.cooldownStateStore
	if cooldownStateStore == nil {
		if provider, ok := sdkAuth.GetTokenStore().(coreauth.CooldownStateStoreProvider); ok {
			cooldownStateStore = provider.CooldownStateStore()
		}
	}
	coreManager.SetCooldownStateStore(cooldownStateStore)
	// Attach a default RoundTripper provider so providers can opt-in per-auth transports.
	coreManager.SetRoundTripperProvider(newDefaultRoundTripperProvider())
	coreManager.SetConfig(b.cfg)
	coreManager.SetOAuthModelAlias(b.cfg.OAuthModelAlias)

	quotaService := b.quotaAlertService
	if quotaService == nil && b.quotaAlertStore != nil {
		collectorRegistry, err := quotaalert.NewDefaultCollectorRegistry()
		if err != nil {
			return nil, fmt.Errorf("cliproxy: create quota alert collectors: %w", err)
		}
		sender, err := quotaalert.NewTelegramStoreSender(quotaalert.TelegramStoreSenderConfig{
			Store:  b.quotaAlertStore,
			Cipher: b.quotaAlertCipher,
		})
		if err != nil {
			return nil, fmt.Errorf("cliproxy: create quota alert Telegram sender: %w", err)
		}
		quotaService, err = quotaalert.NewService(quotaalert.ServiceConfig{
			Store:             b.quotaAlertStore,
			AuthSource:        NewQuotaAlertAuthSource(coreManager),
			CollectorRegistry: collectorRegistry,
			Sender:            sender,
			CollectorDeps:     quotaalert.CollectorDependencies{Refresh: NewQuotaAlertAuthRefresher(coreManager)},
		})
		if err != nil {
			return nil, fmt.Errorf("cliproxy: create quota alert service: %w", err)
		}
	}
	if quotaAlertHook != nil {
		quotaAlertHook.SetTarget(quotaService)
	}

	// The service owns a runtime clone: two Services built from one config must
	// not share a pool publisher or generation, or the second reload retires the
	// first service's pooled transports and fences its snapshot.
	// Upstream CLIProxyAPI 638f5f55e0d9.
	service := &Service{
		cfg:                   b.cfg.CloneForRuntime(),
		configPath:            b.configPath,
		tokenProvider:         tokenProvider,
		apiKeyProvider:        apiKeyProvider,
		watcherFactory:        watcherFactory,
		hooks:                 b.hooks,
		authManager:           authManager,
		accessManager:         accessManager,
		coreManager:           coreManager,
		serverOptions:         append([]api.ServerOption(nil), b.serverOptions...),
		managementConfigStore: b.managementConfigStore,
		runtimeSettingsStore:  b.runtimeSettingsStore,
		runtimeStoragePolicy:  b.runtimeStoragePolicy,
		quotaAlertStore:       b.quotaAlertStore,
		quotaAlertCipher:      b.quotaAlertCipher,
		quotaAlertService:     quotaService,
	}
	// Reserve this runtime's pool publisher before the first config publication, so a
	// reload here never retires another runtime's pooled transports.
	// Upstream CLIProxyAPI 638f5f55e0d9.
	service.proxyPoolPublisher = helps.RegisterProxyPoolPublisher()
	helps.AdvanceProxyPoolGeneration(service.proxyPoolPublisher, service.cfg)
	return service, nil
}
