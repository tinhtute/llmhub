// Package api provides the HTTP API server implementation for the CLI Proxy API.
// It includes the main server struct, routing setup, middleware for CORS and authentication,
// and integration with various AI API handlers (OpenAI, Claude, Gemini).
// The server supports hot-reloading of clients and configuration.
package api

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/access"
	codexLiveHandlers "github.com/therealtinhtute/llmhub/internal/api/handlers/codexlive"
	managementHandlers "github.com/therealtinhtute/llmhub/internal/api/handlers/management"
	"github.com/therealtinhtute/llmhub/internal/api/middleware"
	"github.com/therealtinhtute/llmhub/internal/cache"
	codexmodels "github.com/therealtinhtute/llmhub/internal/client/codex/models"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/home"
	"github.com/therealtinhtute/llmhub/internal/logging"
	"github.com/therealtinhtute/llmhub/internal/managementasset"
	"github.com/therealtinhtute/llmhub/internal/quotaalert"
	"github.com/therealtinhtute/llmhub/internal/redisqueue"
	"github.com/therealtinhtute/llmhub/internal/registry"
	antigravityexecutor "github.com/therealtinhtute/llmhub/internal/runtime/executor"
	"github.com/therealtinhtute/llmhub/internal/runtimecontrol"
	"github.com/therealtinhtute/llmhub/internal/runtimepolicy"
	"github.com/therealtinhtute/llmhub/internal/util"
	sdkaccess "github.com/therealtinhtute/llmhub/sdk/access"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers/claude"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers/gemini"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers/openai"
	sdkAuth "github.com/therealtinhtute/llmhub/sdk/auth"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	coreexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"golang.org/x/net/http2"
	"gopkg.in/yaml.v3"
)

const oauthCallbackSuccessHTML = `<html><head><meta charset="utf-8"><title>Authentication successful</title><script>setTimeout(function(){window.close();},5000);</script></head><body><h1>Authentication successful!</h1><p>You can close this window.</p><p>This window will close automatically in 5 seconds.</p></body></html>`

type ManagementConfigStore = managementHandlers.ManagementConfigStore

type serverOptionConfig struct {
	extraMiddleware            []gin.HandlerFunc
	engineConfigurator         func(*gin.Engine)
	routerConfigurator         func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)
	requestLoggerFactory       func(*config.Config, string) logging.RequestLogger
	localPassword              string
	keepAliveEnabled           bool
	keepAliveTimeout           time.Duration
	keepAliveOnTimeout         func()
	postAuthHook               auth.PostAuthHook
	managementConfigStore      managementHandlers.ManagementConfigStore
	managementConfigChangeHook func(*config.Config)
	quotaAlertStore            quotaalert.Store
	quotaAlertCipher           *quotaalert.SecretCipher
	runtimeSettingsStore       runtimecontrol.SettingsStore
	runtimeStoragePolicy       runtimepolicy.RuntimeStorage
	selfUpdateEngine           managementHandlers.SelfUpdateEngine
	selfUpdateRestartRunner    func(context.Context) error
	homeModelsReader           func(context.Context) ([]byte, error)
}

// ServerOption customises HTTP server construction.
type ServerOption func(*serverOptionConfig)

func defaultRequestLoggerFactory(cfg *config.Config, configPath string) logging.RequestLogger {
	configDir := filepath.Dir(configPath)
	logsDir := logging.ResolveLogDirectory(cfg)
	logger := logging.NewFileRequestLogger(cfg.RequestLog, logsDir, configDir, cfg.ErrorLogsMaxFiles)
	logger.SetHomeEnabled(cfg != nil && cfg.Home.Enabled)
	return logger
}

// WithMiddleware appends additional Gin middleware during server construction.
func WithMiddleware(mw ...gin.HandlerFunc) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.extraMiddleware = append(cfg.extraMiddleware, mw...)
	}
}

// WithEngineConfigurator allows callers to mutate the Gin engine prior to middleware setup.
func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.engineConfigurator = fn
	}
}

// WithRouterConfigurator appends a callback after default routes are registered.
func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.routerConfigurator = fn
	}
}

// WithLocalManagementPassword stores a runtime-only management password accepted for localhost requests.
func WithLocalManagementPassword(password string) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.localPassword = password
	}
}

// WithKeepAliveEndpoint enables a keep-alive endpoint with the provided timeout and callback.
func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return func(cfg *serverOptionConfig) {
		if timeout <= 0 || onTimeout == nil {
			return
		}
		cfg.keepAliveEnabled = true
		cfg.keepAliveTimeout = timeout
		cfg.keepAliveOnTimeout = onTimeout
	}
}

// WithRequestLoggerFactory customises request logger creation.
func WithRequestLoggerFactory(factory func(*config.Config, string) logging.RequestLogger) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.requestLoggerFactory = factory
	}
}

// WithPostAuthHook registers a hook to be called after auth record creation.
func WithPostAuthHook(hook auth.PostAuthHook) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.postAuthHook = hook
	}
}

func WithManagementConfigStore(store managementHandlers.ManagementConfigStore) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.managementConfigStore = store
	}
}

func WithManagementConfigChangeHook(hook func(*config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.managementConfigChangeHook = hook
	}
}

func WithQuotaAlertStore(store quotaalert.Store) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.quotaAlertStore = store
	}
}

func WithQuotaAlertSecretCipher(cipher *quotaalert.SecretCipher) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.quotaAlertCipher = cipher
	}
}

func WithRuntimeSettingsStore(store runtimecontrol.SettingsStore) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.runtimeSettingsStore = store
	}
}

func (s *Server) runtimeControlRequestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil {
			return
		}
		if s == nil || s.runtimeSettingsStore == nil || c.Request == nil {
			c.Next()
			return
		}
		settings, err := s.runtimeSettingsStore.LoadRuntimeSettings(c.Request.Context())
		if err != nil {
			log.Debugf("runtime controls unavailable for request context: %v", err)
			c.Next()
			return
		}
		settings, err = settings.Normalize()
		if err != nil {
			log.Debugf("runtime controls invalid for request context: %v", err)
			c.Next()
			return
		}
		if settings.Cloaking.DisableCodex {
			c.Request = c.Request.WithContext(coreexecutor.WithCodexCloakingDisabled(c.Request.Context()))
		}
		c.Next()
	}
}

// WithSelfUpdateEngine wires the staging engine behind
// POST /v0/management/self-update (R13). The engine resolves ${DATA_DIR} the
// same way the update CLI does.
func WithSelfUpdateEngine(engine managementHandlers.SelfUpdateEngine) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.selfUpdateEngine = engine
	}
}

// WithSelfUpdateRestartRunner overrides the fixed restart command runner used
// by POST /v0/management/self-update/apply. When unset, the handler runs
// `sudo -n /usr/bin/systemctl restart llmhub.service` itself (R14).
func WithSelfUpdateRestartRunner(runner func(context.Context) error) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.selfUpdateRestartRunner = runner
	}
}

func WithRuntimeStoragePolicy(policy runtimepolicy.RuntimeStorage) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.runtimeStoragePolicy = policy
	}
}

// WithHomeModelsReader replaces the Home control-center read behind the home
// model catalog handlers (GET /v1/models, /v1beta/models and their variants).
//
// Test-only: no production construction path sets it, so an unset reader keeps
// the handlers reading the live Home client unchanged. Tests use it to drive
// the real handlers without a Home/Redis connection.
func WithHomeModelsReader(reader func(context.Context) ([]byte, error)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.homeModelsReader = reader
	}
}

// Server represents the main API server.
// It encapsulates the Gin engine, HTTP server, handlers, and configuration.
type Server struct {
	// engine is the Gin web framework engine instance.
	engine *gin.Engine

	// server is the underlying HTTP server.
	server *http.Server

	// muxBaseListener is the shared TCP listener used to serve both HTTP and Redis protocol traffic.
	listenerMu      sync.Mutex
	muxBaseListener net.Listener

	// muxHTTPListener receives HTTP connections selected by the multiplexer.
	muxHTTPListener *muxListener

	// handlers contains the API handlers for processing requests.
	handlers *handlers.BaseAPIHandler

	// cfg holds the current server configuration.
	cfgMu sync.RWMutex
	cfg   *config.Config

	// oldConfigYaml stores a YAML snapshot of the previous configuration for change detection.
	// This prevents issues when the config object is modified in place by Management API.
	oldConfigYaml []byte

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// requestLogger is the request logger instance for dynamic configuration updates.
	requestLogger        logging.RequestLogger
	loggerToggle         func(bool)
	runtimePolicy        runtimepolicy.RuntimeStorage
	runtimeSettingsStore runtimecontrol.SettingsStore
	codexLiveHandler     *codexLiveHandlers.Handler

	// configFilePath is the absolute path to the YAML config file for persistence.
	configFilePath string

	// currentPath is the absolute path to the current working directory.
	currentPath string

	// wsRoutes tracks registered websocket upgrade paths.
	wsRouteMu     sync.Mutex
	wsRoutes      map[string]struct{}
	wsAuthChanged func(bool, bool)
	wsAuthEnabled atomic.Bool

	// management handler
	mgmt *managementHandlers.Handler

	// managementRoutesRegistered tracks whether the management routes have been attached to the engine.
	managementRoutesRegistered atomic.Bool
	// managementRoutesEnabled controls whether management endpoints serve real handlers.
	managementRoutesEnabled atomic.Bool

	// envManagementSecret indicates whether MANAGEMENT_PASSWORD is configured.
	envManagementSecret bool

	localPassword string

	keepAliveEnabled   bool
	keepAliveTimeout   time.Duration
	keepAliveOnTimeout func()
	keepAliveHeartbeat chan struct{}
	keepAliveStop      chan struct{}

	// homeModelsReader, when non-nil, replaces the Home client read for the
	// home model catalog handlers. Only WithHomeModelsReader (tests) sets it.
	homeModelsReader func(context.Context) ([]byte, error)
}

// NewServer creates and initializes a new API server instance.
// It sets up the Gin engine, middleware, routes, and handlers.
//
// Parameters:
//   - cfg: The server configuration
//   - authManager: core runtime auth manager
//   - accessManager: request authentication manager
//
// Returns:
//   - *Server: A new server instance
func NewServer(cfg *config.Config, authManager *auth.Manager, accessManager *sdkaccess.Manager, configFilePath string, opts ...ServerOption) *Server {
	optionState := &serverOptionConfig{
		requestLoggerFactory: defaultRequestLoggerFactory,
	}
	for i := range opts {
		opts[i](optionState)
	}
	// Set gin mode
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create gin engine
	engine := gin.New()
	if errSetTrustedProxies := engine.SetTrustedProxies(cfg.TrustedProxies); errSetTrustedProxies != nil {
		log.WithError(errSetTrustedProxies).Error("invalid trusted-proxies configuration; forwarded client IP headers will be ignored")
		if errDisableTrustedProxies := engine.SetTrustedProxies(nil); errDisableTrustedProxies != nil {
			log.WithError(errDisableTrustedProxies).Error("failed to disable trusted proxy handling")
		}
	}
	if optionState.engineConfigurator != nil {
		optionState.engineConfigurator(engine)
	}

	// Add middleware
	engine.Use(logging.GinLogrusLogger())
	engine.Use(logging.GinLogrusRecovery())
	for _, mw := range optionState.extraMiddleware {
		engine.Use(mw)
	}

	// Add request logging middleware (positioned after recovery, before auth)
	// Resolve logs directory relative to the configuration file directory.
	var requestLogger logging.RequestLogger
	var toggle func(bool)
	if !cfg.CommercialMode {
		if optionState.runtimeStoragePolicy.PostgresDurableMode() {
			requestLogger = logging.NewDisabledRequestLogger()
		} else if optionState.requestLoggerFactory != nil {
			requestLogger = optionState.requestLoggerFactory(cfg, configFilePath)
		}
		if requestLogger != nil {
			engine.Use(middleware.RequestLoggingMiddleware(requestLogger))
			if setter, ok := requestLogger.(interface{ SetEnabled(bool) }); ok {
				toggle = setter.SetEnabled
			}
		}
	}

	engine.Use(corsMiddleware())
	wd, err := os.Getwd()
	if err != nil {
		wd = configFilePath
	}

	envAdminPassword, envAdminPasswordSet := os.LookupEnv("MANAGEMENT_PASSWORD")
	envAdminPassword = strings.TrimSpace(envAdminPassword)
	envManagementSecret := envAdminPasswordSet && envAdminPassword != ""

	// Create server instance
	s := &Server{
		engine:               engine,
		handlers:             handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager),
		cfg:                  cfg,
		accessManager:        accessManager,
		requestLogger:        requestLogger,
		loggerToggle:         toggle,
		runtimePolicy:        optionState.runtimeStoragePolicy,
		runtimeSettingsStore: optionState.runtimeSettingsStore,
		configFilePath:       configFilePath,
		currentPath:          wd,
		envManagementSecret:  envManagementSecret,
		wsRoutes:             make(map[string]struct{}),
		homeModelsReader:     optionState.homeModelsReader,
	}
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	// Save initial YAML snapshot
	s.oldConfigYaml, _ = yaml.Marshal(cfg)
	s.applyAccessConfig(nil, cfg)
	if authManager != nil {
		authManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials)
	}

	auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	applySignatureCacheConfig(nil, cfg)
	// Initialize management handler
	s.mgmt = managementHandlers.NewHandler(cfg, configFilePath, authManager)
	if optionState.localPassword != "" {
		s.mgmt.SetLocalPassword(optionState.localPassword)
	}
	s.mgmt.SetRuntimeStoragePolicy(optionState.runtimeStoragePolicy)
	if !optionState.runtimeStoragePolicy.PostgresDurableMode() {
		logDir := logging.ResolveLogDirectory(cfg)
		s.mgmt.SetLogDirectory(logDir)
	}
	if optionState.postAuthHook != nil {
		s.mgmt.SetPostAuthHook(optionState.postAuthHook)
	}
	if optionState.managementConfigStore != nil {
		s.mgmt.SetConfigStore(optionState.managementConfigStore)
	}
	if optionState.managementConfigChangeHook != nil {
		s.mgmt.SetConfigChangeHook(optionState.managementConfigChangeHook)
	}
	if optionState.quotaAlertStore != nil {
		s.mgmt.SetQuotaAlertStore(optionState.quotaAlertStore)
	}
	if optionState.quotaAlertCipher != nil {
		s.mgmt.SetQuotaAlertSecretCipher(optionState.quotaAlertCipher)
	}
	if optionState.runtimeSettingsStore != nil {
		s.mgmt.SetRuntimeSettingsStore(optionState.runtimeSettingsStore)
	}
	if optionState.selfUpdateEngine != nil {
		s.mgmt.SetSelfUpdateEngine(optionState.selfUpdateEngine)
	}
	if optionState.selfUpdateRestartRunner != nil {
		s.mgmt.SetSelfUpdateRestartRunner(optionState.selfUpdateRestartRunner)
	}
	s.localPassword = optionState.localPassword

	// Home heartbeat gate: when home is enabled, block all endpoints with 503 until the
	// subscribe-config heartbeat connection is healthy.
	engine.Use(s.homeHeartbeatMiddleware())

	// Setup routes
	s.setupRoutes()

	// Apply additional router configurators from options
	if optionState.routerConfigurator != nil {
		optionState.routerConfigurator(engine, s.handlers, cfg)
	}

	// Register management routes when configuration or environment secrets are available,
	// or when a local management password is provided (e.g. TUI mode).
	hasManagementSecret := cfg.RemoteManagement.SecretKey != "" || envManagementSecret || s.localPassword != ""
	s.managementRoutesEnabled.Store(hasManagementSecret)
	redisqueue.SetEnabled(hasManagementSecret || (cfg != nil && cfg.Home.Enabled))
	if hasManagementSecret {
		s.registerManagementRoutes()
	}

	if optionState.keepAliveEnabled {
		s.enableKeepAlive(optionState.keepAliveTimeout, optionState.keepAliveOnTimeout)
	}

	// Create HTTP server
	s.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler: engine,
	}

	return s
}

func (s *Server) getConfig() *config.Config {
	if s == nil {
		return nil
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) homeHeartbeatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.cfg == nil || !s.cfg.Home.Enabled {
			c.Next()
			return
		}
		if c != nil && c.Request != nil {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/v0/management/") || path == "/v0/management" || path == "/management.html" {
				c.Next()
				return
			}
		}
		client := home.Current()
		if client == nil || !client.HeartbeatOK() {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.Next()
	}
}

// setupRoutes configures the API routes for the server.
// It defines the endpoints and associates them with their respective handlers.
func (s *Server) setupRoutes() {
	healthzHandler := func(c *gin.Context) {
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusOK)
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	s.engine.GET("/healthz", healthzHandler)
	s.engine.HEAD("/healthz", healthzHandler)

	s.engine.GET("/management.html", s.serveManagementControlPanel)
	openaiHandlers := openai.NewOpenAIAPIHandler(s.handlers)
	geminiHandlers := gemini.NewGeminiAPIHandler(s.handlers)
	geminiCLIHandlers := gemini.NewGeminiCLIAPIHandler(s.handlers)
	claudeCodeHandlers := claude.NewClaudeCodeAPIHandler(s.handlers)
	openaiResponsesHandlers := openai.NewOpenAIResponsesAPIHandler(s.handlers)

	// OpenAI compatible API routes
	v1 := s.engine.Group("/v1")
	v1.Use(AuthMiddleware(s.accessManager), s.runtimeControlRequestContext())
	{
		v1.GET("/models", s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers))
		v1.POST("/chat/completions", openaiHandlers.ChatCompletions)
		v1.POST("/completions", openaiHandlers.Completions)
		v1.POST("/images/generations", openaiHandlers.ImagesGenerations)
		v1.POST("/images/edits", openaiHandlers.ImagesEdits)
		v1.POST("/videos", openaiHandlers.VideosCreate)
		v1.POST("/videos/generations", openaiHandlers.XAIVideosGenerations)
		v1.POST("/videos/edits", openaiHandlers.XAIVideosEdits)
		v1.POST("/videos/extensions", openaiHandlers.XAIVideosExtensions)
		v1.GET("/videos/:request_id", openaiHandlers.XAIVideosRetrieve)
		v1.POST("/messages", claudeCodeHandlers.ClaudeMessages)
		v1.POST("/messages/count_tokens", claudeCodeHandlers.ClaudeCountTokens)
		v1.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		v1.POST("/responses", openaiResponsesHandlers.Responses)
		v1.POST("/responses/compact", openaiResponsesHandlers.Compact)
	}

	openAIV1 := s.engine.Group("/openai/v1")
	openAIV1.Use(AuthMiddleware(s.accessManager))
	{
		openAIV1.POST("/videos", openaiHandlers.VideosCreate)
		openAIV1.GET("/videos/:video_id", openaiHandlers.VideosRetrieve)
		openAIV1.GET("/videos/:video_id/content", openaiHandlers.VideosContent)
	}

	// Codex CLI direct route aliases (chatgpt_base_url compatible)
	codexDirect := s.engine.Group("/backend-api/codex")
	codexDirect.Use(AuthMiddleware(s.accessManager), s.runtimeControlRequestContext())
	{
		codexDirect.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		codexDirect.POST("/responses", openaiResponsesHandlers.Responses)
		codexDirect.POST("/responses/compact", openaiResponsesHandlers.Compact)
		codexDirect.POST("/alpha/search", openaiResponsesHandlers.AlphaSearch)
		if s.runtimeSettingsStore != nil {
			s.codexLiveHandler = codexLiveHandlers.New(s.handlers.AuthManager, s.runtimeSettingsStore)
			codexDirect.POST("/realtime/calls", s.codexLiveHandler.CreateCall)
			codexDirect.GET("/realtime/calls/:call_id", s.codexLiveHandler.Sideband)
			codexDirect.POST("/realtime/calls/:call_id/hangup", s.codexLiveHandler.HandleHangup)
			codexDirect.POST("/realtime/calls/:call_id/accept", s.codexLiveHandler.HandleSIPControl)
			codexDirect.POST("/realtime/calls/:call_id/reject", s.codexLiveHandler.HandleSIPControl)
			codexDirect.POST("/realtime/calls/:call_id/refer", s.codexLiveHandler.HandleSIPControl)
			codexDirect.GET("/realtime/translations", s.codexLiveHandler.HandleTranslation)
			codexDirect.POST("/realtime/translations", s.codexLiveHandler.HandleTranslation)
			codexDirect.POST("/realtime/transcription_sessions", s.codexLiveHandler.HandleTranscriptionSession)
			codexDirect.GET("/realtime", s.codexLiveHandler.Sideband)
			codexDirect.GET("/live/:call_id", s.codexLiveHandler.Sideband)
		}
	}

	// Gemini compatible API routes
	v1beta := s.engine.Group("/v1beta")
	v1beta.Use(AuthMiddleware(s.accessManager))
	{
		v1beta.GET("/models", s.geminiModelsHandler(geminiHandlers))
		v1beta.POST("/models/*action", geminiHandlers.GeminiHandler)
		v1beta.GET("/models/*action", s.geminiGetHandler(geminiHandlers))
	}

	// Root endpoint
	s.engine.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "LLMHub Server",
			"endpoints": []string{
				"POST /v1/chat/completions",
				"POST /v1/completions",
				"GET /v1/models",
			},
		})
	})
	s.engine.POST("/v1internal:method", geminiCLIHandlers.CLIHandler)

	// OAuth callback endpoints (reuse main server port)
	// These endpoints receive provider redirects and persist
	// the short-lived code/state for the waiting goroutine.
	s.engine.GET("/anthropic/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_ = managementHandlers.SubmitOAuthCallbackForPendingSession("anthropic", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/codex/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_ = managementHandlers.SubmitOAuthCallbackForPendingSession("codex", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/google/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_ = managementHandlers.SubmitOAuthCallbackForPendingSession("gemini", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/antigravity/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_ = managementHandlers.SubmitOAuthCallbackForPendingSession("antigravity", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/xai/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_ = managementHandlers.SubmitOAuthCallbackForPendingSession("xai", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	// Devin's authorization server redirects straight back to this endpoint;
	// invalid or expired callbacks are rejected (upstream 44e62bc8acc2).
	s.engine.GET("/devin/callback", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		code := strings.TrimSpace(c.Query("code"))
		state := strings.TrimSpace(c.Query("state"))
		errStr := strings.TrimSpace(c.Query("error"))
		if errStr == "" {
			errStr = strings.TrimSpace(c.Query("error_description"))
		}
		if code == "" && errStr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "code or error is required"})
			return
		}
		if errSubmit := managementHandlers.SubmitOAuthCallbackForPendingSession("devin", state, code, errStr); errSubmit != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired OAuth callback"})
			return
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	// Management routes are registered lazily by registerManagementRoutes when a secret is configured.
}

// AttachWebsocketRoute registers a websocket upgrade handler on the primary Gin engine.
// The handler is served as-is without additional middleware beyond the standard stack already configured.
func (s *Server) AttachWebsocketRoute(path string, handler http.Handler) {
	if s == nil || s.engine == nil || handler == nil {
		return
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = "/v1/ws"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	s.wsRouteMu.Lock()
	if _, exists := s.wsRoutes[trimmed]; exists {
		s.wsRouteMu.Unlock()
		return
	}
	s.wsRoutes[trimmed] = struct{}{}
	s.wsRouteMu.Unlock()

	authMiddleware := AuthMiddleware(s.accessManager)
	conditionalAuth := func(c *gin.Context) {
		if !s.wsAuthEnabled.Load() {
			c.Next()
			return
		}
		authMiddleware(c)
	}
	finalHandler := func(c *gin.Context) {
		handler.ServeHTTP(c.Writer, c.Request)
		c.Abort()
	}

	s.engine.GET(trimmed, conditionalAuth, finalHandler)
}

func (s *Server) registerManagementRoutes() {
	if s == nil || s.engine == nil || s.mgmt == nil {
		return
	}
	if !s.managementRoutesRegistered.CompareAndSwap(false, true) {
		return
	}

	log.Info("management routes registered after secret key configuration")

	mgmt := s.engine.Group("/v0/management")
	mgmt.Use(s.managementAvailabilityMiddleware(), s.mgmt.Middleware())
	{
		mgmt.GET("/config", s.mgmt.GetConfig)
		mgmt.GET("/config.yaml", s.mgmt.GetConfigYAML)
		mgmt.PUT("/config.yaml", s.mgmt.PutConfigYAML)
		mgmt.GET("/latest-version", s.mgmt.GetLatestVersion)

		mgmt.GET("/debug", s.mgmt.GetDebug)
		mgmt.PUT("/debug", s.mgmt.PutDebug)
		mgmt.PATCH("/debug", s.mgmt.PutDebug)

		mgmt.GET("/logging-to-file", s.mgmt.GetLoggingToFile)
		mgmt.PUT("/logging-to-file", s.mgmt.PutLoggingToFile)
		mgmt.PATCH("/logging-to-file", s.mgmt.PutLoggingToFile)

		mgmt.GET("/logs-max-total-size-mb", s.mgmt.GetLogsMaxTotalSizeMB)
		mgmt.PUT("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)
		mgmt.PATCH("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)

		mgmt.GET("/error-logs-max-files", s.mgmt.GetErrorLogsMaxFiles)
		mgmt.PUT("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)
		mgmt.PATCH("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)

		mgmt.GET("/usage-statistics-enabled", s.mgmt.GetUsageStatisticsEnabled)
		mgmt.PUT("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)
		mgmt.PATCH("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)

		mgmt.GET("/proxy-url", s.mgmt.GetProxyURL)
		mgmt.PUT("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.PATCH("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.DELETE("/proxy-url", s.mgmt.DeleteProxyURL)

		mgmt.POST("/api-call", s.mgmt.APICall)

		mgmt.POST("/self-update", s.mgmt.SelfUpdateStage)
		mgmt.POST("/self-update/apply", s.mgmt.SelfUpdateApply)

		mgmt.GET("/quota-exceeded/switch-project", s.mgmt.GetSwitchProject)
		mgmt.PUT("/quota-exceeded/switch-project", s.mgmt.PutSwitchProject)
		mgmt.PATCH("/quota-exceeded/switch-project", s.mgmt.PutSwitchProject)

		mgmt.GET("/quota-exceeded/switch-preview-model", s.mgmt.GetSwitchPreviewModel)
		mgmt.PUT("/quota-exceeded/switch-preview-model", s.mgmt.PutSwitchPreviewModel)
		mgmt.PATCH("/quota-exceeded/switch-preview-model", s.mgmt.PutSwitchPreviewModel)
		mgmt.POST("/reset-quota", s.mgmt.ResetQuota)

		mgmt.GET("/provider-presets", s.mgmt.GetProviderPresets)

		mgmt.GET("/runtime-controls", s.mgmt.GetRuntimeControls)
		mgmt.PUT("/runtime-controls", s.mgmt.PutRuntimeControls)
		mgmt.PATCH("/runtime-controls", s.mgmt.PutRuntimeControls)

		mgmt.GET("/quota-alerts/settings", s.mgmt.GetQuotaAlertSettings)
		mgmt.PUT("/quota-alerts/settings", s.mgmt.PutQuotaAlertSettings)
		mgmt.PUT("/quota-alerts/telegram", s.mgmt.PutQuotaAlertTelegram)
		mgmt.GET("/quota-alerts/state", s.mgmt.GetQuotaAlertState)
		mgmt.GET("/quota-alerts/events", s.mgmt.GetQuotaAlertEvents)
		mgmt.POST("/quota-alerts/events/:id/ack", s.mgmt.AckQuotaAlertEvent)
		mgmt.POST("/quota-alerts/telegram/test", s.mgmt.TestQuotaAlertTelegram)

		mgmt.GET("/api-keys", s.mgmt.GetAPIKeys)
		mgmt.PUT("/api-keys", s.mgmt.PutAPIKeys)
		mgmt.PATCH("/api-keys", s.mgmt.PatchAPIKeys)
		mgmt.DELETE("/api-keys", s.mgmt.DeleteAPIKeys)
		mgmt.GET("/api-key-usage", s.mgmt.GetAPIKeyUsage)
		mgmt.GET("/usage-queue", s.mgmt.GetUsageQueue)
		mgmt.POST("/reset-usage", s.mgmt.ResetUsage)

		mgmt.GET("/gemini-api-key", s.mgmt.GetGeminiKeys)
		mgmt.PUT("/gemini-api-key", s.mgmt.PutGeminiKeys)
		mgmt.PATCH("/gemini-api-key", s.mgmt.PatchGeminiKey)
		mgmt.DELETE("/gemini-api-key", s.mgmt.DeleteGeminiKey)

		mgmt.GET("/logs", s.mgmt.GetLogs)
		mgmt.DELETE("/logs", s.mgmt.DeleteLogs)
		mgmt.GET("/request-error-logs", s.mgmt.GetRequestErrorLogs)
		mgmt.GET("/request-error-logs/:name", s.mgmt.DownloadRequestErrorLog)
		mgmt.GET("/request-log-by-id/:id", s.mgmt.GetRequestLogByID)
		mgmt.GET("/request-log", s.mgmt.GetRequestLog)
		mgmt.PUT("/request-log", s.mgmt.PutRequestLog)
		mgmt.PATCH("/request-log", s.mgmt.PutRequestLog)
		mgmt.GET("/ws-auth", s.mgmt.GetWebsocketAuth)
		mgmt.PUT("/ws-auth", s.mgmt.PutWebsocketAuth)
		mgmt.PATCH("/ws-auth", s.mgmt.PutWebsocketAuth)

		mgmt.GET("/request-retry", s.mgmt.GetRequestRetry)
		mgmt.PUT("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.PATCH("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.GET("/max-retry-interval", s.mgmt.GetMaxRetryInterval)
		mgmt.PUT("/max-retry-interval", s.mgmt.PutMaxRetryInterval)
		mgmt.PATCH("/max-retry-interval", s.mgmt.PutMaxRetryInterval)

		mgmt.GET("/force-model-prefix", s.mgmt.GetForceModelPrefix)
		mgmt.PUT("/force-model-prefix", s.mgmt.PutForceModelPrefix)
		mgmt.PATCH("/force-model-prefix", s.mgmt.PutForceModelPrefix)

		mgmt.GET("/routing/strategy", s.mgmt.GetRoutingStrategy)
		mgmt.PUT("/routing/strategy", s.mgmt.PutRoutingStrategy)
		mgmt.PATCH("/routing/strategy", s.mgmt.PutRoutingStrategy)

		mgmt.GET("/claude-api-key", s.mgmt.GetClaudeKeys)
		mgmt.PUT("/claude-api-key", s.mgmt.PutClaudeKeys)
		mgmt.PATCH("/claude-api-key", s.mgmt.PatchClaudeKey)
		mgmt.DELETE("/claude-api-key", s.mgmt.DeleteClaudeKey)

		mgmt.GET("/codex-api-key", s.mgmt.GetCodexKeys)
		mgmt.PUT("/codex-api-key", s.mgmt.PutCodexKeys)
		mgmt.PATCH("/codex-api-key", s.mgmt.PatchCodexKey)
		mgmt.DELETE("/codex-api-key", s.mgmt.DeleteCodexKey)

		mgmt.GET("/native-providers/:provider", s.mgmt.GetNativeProviderResources)
		mgmt.PUT("/native-providers/:provider", s.mgmt.PutNativeProviderResource)
		mgmt.GET("/native-providers/:provider/:id/models", s.mgmt.GetNativeProviderModels)
		mgmt.PATCH("/native-providers/:provider/:id", s.mgmt.PatchNativeProviderResource)
		mgmt.PUT("/native-providers/:provider/:id", s.mgmt.PatchNativeProviderResource)
		mgmt.DELETE("/native-providers/:provider/:id", s.mgmt.DeleteNativeProviderResource)

		mgmt.GET("/openai-compatibility", s.mgmt.GetOpenAICompat)
		mgmt.PUT("/openai-compatibility", s.mgmt.PutOpenAICompat)
		mgmt.PATCH("/openai-compatibility", s.mgmt.PatchOpenAICompat)
		mgmt.DELETE("/openai-compatibility", s.mgmt.DeleteOpenAICompat)

		mgmt.GET("/vertex-api-key", s.mgmt.GetVertexCompatKeys)
		mgmt.PUT("/vertex-api-key", s.mgmt.PutVertexCompatKeys)
		mgmt.PATCH("/vertex-api-key", s.mgmt.PatchVertexCompatKey)
		mgmt.DELETE("/vertex-api-key", s.mgmt.DeleteVertexCompatKey)

		mgmt.GET("/meta-api-key", s.mgmt.GetMetaKeys)
		mgmt.PUT("/meta-api-key", s.mgmt.PutMetaKeys)
		mgmt.PATCH("/meta-api-key", s.mgmt.PatchMetaKey)
		mgmt.DELETE("/meta-api-key", s.mgmt.DeleteMetaKey)

		mgmt.GET("/oauth-excluded-models", s.mgmt.GetOAuthExcludedModels)
		mgmt.PUT("/oauth-excluded-models", s.mgmt.PutOAuthExcludedModels)
		mgmt.PATCH("/oauth-excluded-models", s.mgmt.PatchOAuthExcludedModels)
		mgmt.DELETE("/oauth-excluded-models", s.mgmt.DeleteOAuthExcludedModels)

		mgmt.GET("/oauth-model-alias", s.mgmt.GetOAuthModelAlias)
		mgmt.PUT("/oauth-model-alias", s.mgmt.PutOAuthModelAlias)
		mgmt.PATCH("/oauth-model-alias", s.mgmt.PatchOAuthModelAlias)
		mgmt.DELETE("/oauth-model-alias", s.mgmt.DeleteOAuthModelAlias)
		mgmt.GET("/oauth-request-scoped-errors", s.mgmt.GetOAuthRequestScopedErrors)
		mgmt.PUT("/oauth-request-scoped-errors", s.mgmt.PutOAuthRequestScopedErrors)
		mgmt.PATCH("/oauth-request-scoped-errors", s.mgmt.PatchOAuthRequestScopedErrors)
		mgmt.DELETE("/oauth-request-scoped-errors", s.mgmt.DeleteOAuthRequestScopedErrors)

		mgmt.GET("/auth-files", s.mgmt.ListAuthFiles)
		mgmt.GET("/auth-files/models", s.mgmt.GetAuthFileModels)
		mgmt.GET("/model-definitions/:channel", s.mgmt.GetStaticModelDefinitions)
		mgmt.GET("/auth-files/download", s.mgmt.DownloadAuthFile)
		mgmt.POST("/auth-files", s.mgmt.UploadAuthFile)
		mgmt.POST("/auth-files/kiro/quota", s.mgmt.RefreshKiroQuota)
		mgmt.POST("/auth-files/kiro/overage", s.mgmt.SetKiroOverage)
		mgmt.DELETE("/auth-files", s.mgmt.DeleteAuthFile)
		mgmt.PATCH("/auth-files/status", s.mgmt.PatchAuthFileStatus)
		mgmt.PATCH("/auth-files/fields", s.mgmt.PatchAuthFileFields)
		mgmt.POST("/auth-files/refresh", s.mgmt.RefreshAuthFiles)
		mgmt.POST("/vertex/import", s.mgmt.ImportVertexCredential)

		mgmt.GET("/anthropic-auth-url", s.mgmt.RequestAnthropicToken)
		mgmt.GET("/codex-auth-url", s.mgmt.RequestCodexToken)
		mgmt.GET("/gemini-cli-auth-url", s.mgmt.RequestGeminiCLIToken)
		mgmt.GET("/antigravity-auth-url", s.mgmt.RequestAntigravityToken)
		mgmt.GET("/kimi-auth-url", s.mgmt.RequestKimiToken)
		mgmt.GET("/kimi-ai-auth-url", s.mgmt.RequestKimiAIToken)
		mgmt.GET("/xai-auth-url", s.mgmt.RequestXAIToken)
		mgmt.GET("/devin-auth-url", s.mgmt.RequestDevinToken)
		mgmt.GET("/meta-auth-url", s.mgmt.RequestMetaToken)
		mgmt.POST("/oauth-callback", s.mgmt.PostOAuthCallback)
		mgmt.GET("/get-auth-status", s.mgmt.GetAuthStatus)
		mgmt.DELETE("/oauth-session", s.mgmt.CancelAuthSession)
	}
}

func (s *Server) managementAvailabilityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.cfg == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if s.cfg.Home.Enabled {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if !s.managementRoutesEnabled.Load() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

func (s *Server) serveManagementControlPanel(c *gin.Context) {
	cfg := s.cfg
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	data := managementasset.EmbeddedManagementHTML()
	if len(data) == 0 {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	// Ported from upstream CLIProxyAPI commit 78c14b80 (disable caching for
	// management.html to prevent stale SPA views).
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

func (s *Server) enableKeepAlive(timeout time.Duration, onTimeout func()) {
	if timeout <= 0 || onTimeout == nil {
		return
	}

	s.keepAliveEnabled = true
	s.keepAliveTimeout = timeout
	s.keepAliveOnTimeout = onTimeout
	s.keepAliveHeartbeat = make(chan struct{}, 1)
	s.keepAliveStop = make(chan struct{}, 1)

	s.engine.GET("/keep-alive", s.handleKeepAlive)

	go s.watchKeepAlive()
}

func (s *Server) handleKeepAlive(c *gin.Context) {
	if s.localPassword != "" {
		provided := strings.TrimSpace(c.GetHeader("Authorization"))
		if provided != "" {
			parts := strings.SplitN(provided, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
				provided = parts[1]
			}
		}
		if provided == "" {
			provided = strings.TrimSpace(c.GetHeader("X-Local-Password"))
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.localPassword)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
	}

	s.signalKeepAlive()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) signalKeepAlive() {
	if !s.keepAliveEnabled {
		return
	}
	select {
	case s.keepAliveHeartbeat <- struct{}{}:
	default:
	}
}

func (s *Server) watchKeepAlive() {
	if !s.keepAliveEnabled {
		return
	}

	timer := time.NewTimer(s.keepAliveTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			log.Warnf("keep-alive endpoint idle for %s, shutting down", s.keepAliveTimeout)
			if s.keepAliveOnTimeout != nil {
				s.keepAliveOnTimeout()
			}
			return
		case <-s.keepAliveHeartbeat:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.keepAliveTimeout)
		case <-s.keepAliveStop:
			return
		}
	}
}

// unifiedModelsHandler creates a unified handler for the /v1/models endpoint
// that routes to different handlers based on the User-Agent header.
// If User-Agent starts with "claude-cli", it routes to Claude handler,
// otherwise it routes to OpenAI handler.
func (s *Server) unifiedModelsHandler(openaiHandler *openai.OpenAIAPIHandler, claudeHandler *claude.ClaudeCodeAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Request.URL.Query()["client_version"]; ok {
			clientVersion := c.Query("client_version")
			if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
				s.handleHomeCodexClientModels(c, clientVersion)
				return
			}
			openaiHandler.OpenAIModels(c)
			return
		}

		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeModels(c)
			return
		}

		userAgent := c.GetHeader("User-Agent")

		// Route to Claude handler if User-Agent starts with "claude-cli"
		if strings.HasPrefix(userAgent, "claude-cli") {
			// log.Debugf("Routing /v1/models to Claude handler for User-Agent: %s", userAgent)
			claudeHandler.ClaudeModels(c)
		} else {
			// log.Debugf("Routing /v1/models to OpenAI handler for User-Agent: %s", userAgent)
			openaiHandler.OpenAIModels(c)
		}
	}
}

// handleHomeCodexClientModels builds the Codex client catalog from Home model IDs.
// Template metadata still comes from the local/remote codex_client_models catalog.
func (s *Server) handleHomeCodexClientModels(c *gin.Context, clientVersion string) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	models := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		models = append(models, formatHomeCodexModel(entry))
	}

	var webSearchCapabilityForModel codexmodels.WebSearchCapabilityForModelFunc
	if clientVersion == "cpa" {
		webSearchCapabilityForModel = homeWebSearchCapabilityForModel(entries)
	}
	optimizeMultiAgentV2 := s != nil && s.cfg != nil && s.cfg.CodexOptimizeMultiAgentV2
	payload := codexmodels.BuildResponseForClientWithCPACapabilities(models, nil, webSearchCapabilityForModel, optimizeMultiAgentV2, clientVersion)
	body, errMarshal := codexmodels.MarshalCompact(payload)
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMarshal.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func homeWebSearchCapabilityForModel(entries []homeModelEntry) codexmodels.WebSearchCapabilityForModelFunc {
	routesByID := make(map[string][]registry.NativeCapabilityRoute, len(entries))
	for _, entry := range entries {
		routesByID[entry.id] = append([]registry.NativeCapabilityRoute(nil), entry.nativeCapabilityRoutes...)
	}
	return func(id string) *bool {
		return registry.ResolveResponsesWebSearchCapability(routesByID[strings.TrimSpace(id)])
	}
}

func formatHomeCodexModel(entry homeModelEntry) map[string]any {
	model := map[string]any{
		"id":     entry.id,
		"object": "model",
	}
	if entry.created > 0 {
		model["created"] = entry.created
	}
	if entry.ownedBy != "" {
		model["owned_by"] = entry.ownedBy
	}
	// Ported from upstream CLIProxyAPI commit 28743473c11a (server_routes.go):
	// mark models served by the Devin provider so downstream Codex client
	// formatting can append the (Devin) display-name suffix.
	for _, p := range entry.providers {
		if strings.EqualFold(p, "devin") {
			model["type"] = "devin"
			break
		}
	}
	if entry.displayName != "" {
		model["display_name"] = entry.displayName
		model["description"] = entry.displayName
	}
	return model
}

func (s *Server) geminiModelsHandler(geminiHandler *gemini.GeminiAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeGeminiModels(c)
			return
		}

		geminiHandler.GeminiModels(c)
	}
}

func (s *Server) geminiGetHandler(geminiHandler *gemini.GeminiAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeGeminiModel(c)
			return
		}

		geminiHandler.GeminiGetHandler(c)
	}
}

type homeModelEntry struct {
	id                     string
	created                int64
	ownedBy                string
	displayName            string
	providers              []string
	nativeCapabilityRoutes []registry.NativeCapabilityRoute
}

func (s *Server) handleHomeModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	userAgent := c.GetHeader("User-Agent")
	isClaude := strings.HasPrefix(userAgent, "claude-cli")

	if isClaude {
		out := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			model := map[string]any{
				"id":       entry.id,
				"object":   "model",
				"owned_by": entry.ownedBy,
			}
			if entry.created > 0 {
				model["created_at"] = entry.created
			}
			if entry.displayName != "" {
				model["display_name"] = entry.displayName
			}
			out = append(out, model)
		}
		firstID := ""
		lastID := ""
		if len(out) > 0 {
			if id, okID := out[0]["id"].(string); okID {
				firstID = id
			}
			if id, okID := out[len(out)-1]["id"].(string); okID {
				lastID = id
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"data":     out,
			"has_more": false,
			"first_id": firstID,
			"last_id":  lastID,
		})
		return
	}

	filtered := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		model := map[string]any{
			"id":     entry.id,
			"object": "model",
		}
		if entry.created > 0 {
			model["created"] = entry.created
		}
		if entry.ownedBy != "" {
			model["owned_by"] = entry.ownedBy
		}
		filtered = append(filtered, model)
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   filtered,
	})
}

func (s *Server) handleHomeGeminiModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"models": formatHomeGeminiModels(entries),
	})
}

func (s *Server) handleHomeGeminiModel(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	action := strings.TrimPrefix(c.Param("action"), "/")
	action = strings.TrimSpace(action)
	for _, entry := range entries {
		if homeGeminiModelMatches(entry, action) {
			c.JSON(http.StatusOK, formatHomeGeminiModel(entry))
			return
		}
	}

	c.JSON(http.StatusNotFound, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: "Not Found",
			Type:    "not_found",
		},
	})
}

func (s *Server) loadHomeModelEntries(c *gin.Context) ([]homeModelEntry, bool) {
	if s == nil || c == nil || c.Request == nil {
		return nil, false
	}
	// The reader seam is test-only (WithHomeModelsReader); production leaves it
	// nil and reads the live Home client exactly as before.
	var raw []byte
	var errGet error
	if s.homeModelsReader != nil {
		raw, errGet = s.homeModelsReader(c.Request.Context())
	} else {
		client := home.Current()
		if client == nil {
			c.JSON(http.StatusServiceUnavailable, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "home control center unavailable",
					Type:    "server_error",
				},
			})
			return nil, false
		}
		raw, errGet = client.GetModels(c.Request.Context())
	}

	if errGet != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: errGet.Error(),
				Type:    "server_error",
			},
		})
		return nil, false
	}

	entries, errDecode := decodeHomeModels(raw)
	if errDecode != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: errDecode.Error(),
				Type:    "server_error",
			},
		})
		return nil, false
	}

	return entries, true
}

func formatHomeGeminiModels(entries []homeModelEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, formatHomeGeminiModel(entry))
	}
	return out
}

func formatHomeGeminiModel(entry homeModelEntry) map[string]any {
	name := entry.id
	if !strings.HasPrefix(name, "models/") {
		name = "models/" + name
	}
	displayName := entry.displayName
	if displayName == "" {
		displayName = entry.id
	}
	return map[string]any{
		"name":                       name,
		"displayName":                displayName,
		"description":                displayName,
		"supportedGenerationMethods": []string{"generateContent"},
	}
}

func homeGeminiModelMatches(entry homeModelEntry, action string) bool {
	id := strings.TrimSpace(entry.id)
	if id == "" || action == "" {
		return false
	}
	normalizedAction := strings.TrimPrefix(action, "models/")
	normalizedID := strings.TrimPrefix(id, "models/")
	return action == id || action == "models/"+id || normalizedAction == normalizedID
}

func decodeHomeModels(raw []byte) ([]homeModelEntry, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("home models payload is empty")
	}

	var bySection map[string][]map[string]any
	if err := json.Unmarshal(raw, &bySection); err != nil {
		return nil, fmt.Errorf("parse home models payload: %w", err)
	}
	if bySection == nil {
		return nil, fmt.Errorf("home models payload is not an object")
	}
	// Ported from CLIProxyAPI 4d34502f: an error envelope must never be read as
	// a provider section, otherwise an array-shaped error is relayed as a
	// catalog (or masked as an empty one).
	if _, hasError := bySection["error"]; hasError {
		return nil, fmt.Errorf("home models payload contains an error envelope")
	}

	indexByID := make(map[string]int)
	out := make([]homeModelEntry, 0, 256)
	hasModelRecords := false
	for section, models := range bySection {
		if models == nil {
			return nil, fmt.Errorf("home models section %q is null", section)
		}
		hasModelRecords = hasModelRecords || len(models) > 0
		provider := strings.ToLower(strings.TrimSpace(section))
		for _, model := range models {
			id, _ := model["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				name, _ := model["name"].(string)
				name = strings.TrimSpace(name)
				id = strings.TrimPrefix(name, "models/")
			}
			if id == "" {
				continue
			}
			route := registry.NativeCapabilityRoute{
				Provider:           provider,
				NativeCapabilities: homeModelNativeCapabilities(model),
			}
			if index, ok := indexByID[id]; ok {
				out[index].providers = appendUniqueHomeProvider(out[index].providers, provider)
				out[index].nativeCapabilityRoutes = append(out[index].nativeCapabilityRoutes, route)
				continue
			}

			created := int64(0)
			switch v := model["created"].(type) {
			case float64:
				created = int64(v)
			case int64:
				created = v
			case int:
				created = int64(v)
			case json.Number:
				if n, err := v.Int64(); err == nil {
					created = n
				}
			}

			ownedBy, _ := model["owned_by"].(string)
			ownedBy = strings.TrimSpace(ownedBy)
			displayName, _ := model["display_name"].(string)
			displayName = strings.TrimSpace(displayName)
			if displayName == "" {
				displayName, _ = model["displayName"].(string)
				displayName = strings.TrimSpace(displayName)
			}

			indexByID[id] = len(out)
			out = append(out, homeModelEntry{
				id:                     id,
				created:                created,
				ownedBy:                ownedBy,
				displayName:            displayName,
				providers:              appendUniqueHomeProvider(nil, provider),
				nativeCapabilityRoutes: []registry.NativeCapabilityRoute{route},
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	// Ported from CLIProxyAPI 07feedf7: an empty catalog is valid (a client whose
	// access scopes expose no routes). Sections that carried records but no
	// usable model ids are still malformed.
	if len(out) == 0 && hasModelRecords {
		return nil, fmt.Errorf("home models payload contains no models")
	}
	return out, nil
}

func homeModelNativeCapabilities(model map[string]any) *registry.NativeCapabilities {
	raw, ok := model["native_capabilities"].(map[string]any)
	if !ok {
		return nil
	}
	webSearch, ok := raw["web_search"].(bool)
	if !ok {
		return &registry.NativeCapabilities{}
	}
	return &registry.NativeCapabilities{WebSearch: &webSearch}
}

func appendUniqueHomeProvider(providers []string, provider string) []string {
	if provider == "" {
		return providers
	}
	for _, existing := range providers {
		if existing == provider {
			return providers
		}
	}
	return append(providers, provider)
}

// Start begins listening for and serving HTTP or HTTPS requests.
// It's a blocking call and will only return on an unrecoverable error.
//
// Returns:
//   - error: An error if the server fails to start
func (s *Server) Start() error {
	if s == nil || s.server == nil {
		return fmt.Errorf("failed to start HTTP server: server not initialized")
	}

	addr := s.server.Addr
	listener, errListen := net.Listen("tcp", addr)
	if errListen != nil {
		return fmt.Errorf("failed to start HTTP server: %v", errListen)
	}

	cfg := s.getConfig()
	useTLS := cfg != nil && cfg.TLS.Enable
	if useTLS {
		certPath := strings.TrimSpace(cfg.TLS.Cert)
		keyPath := strings.TrimSpace(cfg.TLS.Key)
		if certPath == "" || keyPath == "" {
			if errClose := listener.Close(); errClose != nil {
				log.Errorf("failed to close listener after TLS validation failure: %v", errClose)
			}
			return fmt.Errorf("failed to start HTTPS server: tls.cert or tls.key is empty")
		}
		certPair, errLoad := tls.LoadX509KeyPair(certPath, keyPath)
		if errLoad != nil {
			if errClose := listener.Close(); errClose != nil {
				log.Errorf("failed to close listener after TLS key pair load failure: %v", errClose)
			}
			return fmt.Errorf("failed to start HTTPS server: %v", errLoad)
		}

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{certPair},
			NextProtos:   []string{"h2", "http/1.1"},
		}
		s.server.TLSConfig = tlsConfig
		if errHTTP2 := http2.ConfigureServer(s.server, &http2.Server{}); errHTTP2 != nil {
			log.Warnf("failed to configure HTTP/2: %v", errHTTP2)
		}
		listener = tls.NewListener(listener, tlsConfig)
		log.Debugf("Starting API server on %s with TLS", addr)
	} else {
		log.Debugf("Starting API server on %s", addr)
	}

	httpListener := newMuxListener(listener.Addr(), 1024)
	s.listenerMu.Lock()
	s.muxBaseListener = listener
	s.muxHTTPListener = httpListener
	s.listenerMu.Unlock()

	httpErrCh := make(chan error, 1)
	acceptErrCh := make(chan error, 1)

	go func() {
		httpErrCh <- s.server.Serve(httpListener)
	}()
	go func() {
		acceptErrCh <- s.acceptMuxConnections(listener, httpListener)
	}()

	select {
	case errServe := <-httpErrCh:
		s.listenerMu.Lock()
		muxBase := s.muxBaseListener
		muxHTTP := s.muxHTTPListener
		s.muxBaseListener = nil
		s.muxHTTPListener = nil
		s.listenerMu.Unlock()
		if muxBase != nil {
			if errClose := muxBase.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				log.Debugf("failed to close shared listener after HTTP serve exit: %v", errClose)
			}
		}
		if muxHTTP != nil {
			_ = muxHTTP.Close()
		}
		errAccept := <-acceptErrCh
		errServe = normalizeHTTPServeError(errServe)
		errAccept = normalizeListenerError(errAccept)
		if errServe != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errServe)
		}
		if errAccept != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errAccept)
		}
		return nil
	case errAccept := <-acceptErrCh:
		s.listenerMu.Lock()
		muxHTTP := s.muxHTTPListener
		muxBase := s.muxBaseListener
		s.muxHTTPListener = nil
		s.muxBaseListener = nil
		s.listenerMu.Unlock()
		if muxHTTP != nil {
			_ = muxHTTP.Close()
		}
		if muxBase != nil {
			if errClose := muxBase.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				log.Debugf("failed to close shared listener after accept loop exit: %v", errClose)
			}
		}
		errServe := <-httpErrCh
		errServe = normalizeHTTPServeError(errServe)
		errAccept = normalizeListenerError(errAccept)
		if errAccept != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errAccept)
		}
		if errServe != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errServe)
		}
		return nil
	}
}

// Stop closes listeners and immediately shuts down the API server without waiting for active connections.
//
// Parameters:
//   - ctx: Context passed for compatibility.
//
// Returns:
//   - error: An error if the server fails to stop
func (s *Server) Stop(ctx context.Context) error {
	log.Debug("Stopping API server...")

	if s.keepAliveEnabled {
		select {
		case s.keepAliveStop <- struct{}{}:
		default:
		}
	}

	s.listenerMu.Lock()
	muxHTTP := s.muxHTTPListener
	s.muxHTTPListener = nil
	muxBase := s.muxBaseListener
	s.muxBaseListener = nil
	s.listenerMu.Unlock()

	if muxHTTP != nil {
		_ = muxHTTP.Close()
	}
	if muxBase != nil {
		if errCloseBase := muxBase.Close(); errCloseBase != nil && !errors.Is(errCloseBase, net.ErrClosed) {
			log.Debugf("failed to close shared listener: %v", errCloseBase)
		}
	}

	// Close the HTTP server immediately without graceful draining.
	var errCloseServer error
	if s.server != nil {
		errCloseServer = s.server.Close()
		if errors.Is(errCloseServer, http.ErrServerClosed) || errors.Is(errCloseServer, net.ErrClosed) {
			errCloseServer = nil
		}
	}
	if s.codexLiveHandler != nil {
		s.codexLiveHandler.Close()
	}
	if errCloseServer != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %v", errCloseServer)
	}

	log.Debug("API server stopped")
	return nil
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers
// to every response, allowing cross-origin requests.
//
// Returns:
//   - gin.HandlerFunc: The CORS middleware handler
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func (s *Server) applyAccessConfig(oldCfg, newCfg *config.Config) {
	if s == nil || s.accessManager == nil || newCfg == nil {
		return
	}
	if _, err := access.ApplyAccessProviders(s.accessManager, oldCfg, newCfg); err != nil {
		return
	}
}

// UpdateClients updates the server's client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (s *Server) UpdateClients(cfg *config.Config) {
	if cfg == nil {
		return
	}
	// Own an isolated snapshot: the caller (service bootstrap, tests) may
	// keep mutating or replacing its reference while requests are served.
	cfg = cfg.CloneForRuntime()
	// Reconstruct old config from YAML snapshot to avoid reference sharing issues
	var oldCfg *config.Config
	if len(s.oldConfigYaml) > 0 {
		_ = yaml.Unmarshal(s.oldConfigYaml, &oldCfg)
	}

	// Update request logger enabled state if it has changed
	previousRequestLog := false
	if oldCfg != nil {
		previousRequestLog = oldCfg.RequestLog
	}
	if s.requestLogger != nil && (oldCfg == nil || previousRequestLog != cfg.RequestLog) {
		if s.loggerToggle != nil {
			s.loggerToggle(cfg.RequestLog)
		} else if toggler, ok := s.requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggler.SetEnabled(cfg.RequestLog)
		}
	}

	if oldCfg == nil || oldCfg.Home.Enabled != cfg.Home.Enabled {
		if setter, ok := s.requestLogger.(interface{ SetHomeEnabled(bool) }); ok {
			setter.SetHomeEnabled(cfg.Home.Enabled)
		}
	}

	if oldCfg == nil || oldCfg.LoggingToFile != cfg.LoggingToFile || oldCfg.LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB {
		if err := logging.ConfigureLogOutputWithPolicy(cfg, s.runtimePolicy); err != nil {
			log.Errorf("failed to reconfigure log output: %v", err)
		}
	}

	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled {
		redisqueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	}

	if oldCfg == nil || oldCfg.RedisUsageQueueRetentionSeconds != cfg.RedisUsageQueueRetentionSeconds {
		redisqueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	}

	if s.requestLogger != nil && (oldCfg == nil || oldCfg.ErrorLogsMaxFiles != cfg.ErrorLogsMaxFiles) {
		if setter, ok := s.requestLogger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
			setter.SetErrorLogsMaxFiles(cfg.ErrorLogsMaxFiles)
		}
	}

	if oldCfg == nil || oldCfg.DisableCooling != cfg.DisableCooling {
		auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	}

	if oldCfg != nil && oldCfg.DisableImageGeneration != cfg.DisableImageGeneration {
		log.Infof("disable-image-generation updated: %v -> %v", oldCfg.DisableImageGeneration, cfg.DisableImageGeneration)
	}

	applySignatureCacheConfig(oldCfg, cfg)

	// Upstream commit d5397905f09e: purge cached Antigravity connection pools
	// when pool settings change so hot-reloads take effect immediately.
	if oldCfg != nil && !reflect.DeepEqual(oldCfg.AntigravityConnectionPool, cfg.AntigravityConnectionPool) {
		antigravityexecutor.ResetAntigravityTransports()
	}

	if s.handlers != nil && s.handlers.AuthManager != nil {
		s.handlers.AuthManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials)
	}

	// Update log level dynamically when debug flag changes
	if oldCfg == nil || oldCfg.Debug != cfg.Debug {
		util.SetLogLevel(cfg)
	}

	prevSecretEmpty := true
	if oldCfg != nil {
		prevSecretEmpty = oldCfg.RemoteManagement.SecretKey == ""
	}
	newSecretEmpty := cfg.RemoteManagement.SecretKey == ""
	if s.envManagementSecret {
		s.registerManagementRoutes()
		if s.managementRoutesEnabled.CompareAndSwap(false, true) {
			log.Info("management routes enabled via MANAGEMENT_PASSWORD")
		} else {
			s.managementRoutesEnabled.Store(true)
		}
	} else {
		switch {
		case prevSecretEmpty && !newSecretEmpty:
			s.registerManagementRoutes()
			if s.managementRoutesEnabled.CompareAndSwap(false, true) {
				log.Info("management routes enabled after secret key update")
			} else {
				s.managementRoutesEnabled.Store(true)
			}
		case !prevSecretEmpty && newSecretEmpty:
			if s.managementRoutesEnabled.CompareAndSwap(true, false) {
				log.Info("management routes disabled after secret key removal")
			} else {
				s.managementRoutesEnabled.Store(false)
			}
		default:
			s.managementRoutesEnabled.Store(!newSecretEmpty)
		}
	}
	redisqueue.SetEnabled(s.managementRoutesEnabled.Load() || (cfg != nil && cfg.Home.Enabled))

	s.applyAccessConfig(oldCfg, cfg)
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	if oldCfg != nil && s.wsAuthChanged != nil && oldCfg.WebsocketAuth != cfg.WebsocketAuth {
		s.wsAuthChanged(oldCfg.WebsocketAuth, cfg.WebsocketAuth)
	}

	// Save YAML snapshot for next comparison
	s.oldConfigYaml, _ = yaml.Marshal(cfg)

	s.handlers.UpdateClients(&cfg.SDKConfig)

	if s.mgmt != nil {
		s.mgmt.SetConfig(cfg)
		s.mgmt.SetAuthManager(s.handlers.AuthManager)
	}

	// Count client sources from configuration and auth store.
	authEntries := 0
	if cfg != nil && !cfg.Home.Enabled {
		tokenStore := sdkAuth.GetTokenStore()
		if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(cfg.AuthDir)
		}
		authEntries = util.CountAuthFiles(context.Background(), tokenStore)
	}
	geminiAPIKeyCount := len(cfg.GeminiKey)
	claudeAPIKeyCount := len(cfg.ClaudeKey)
	codexAPIKeyCount := len(cfg.CodexKey)
	vertexAICompatCount := len(cfg.VertexCompatAPIKey)
	openAICompatCount := 0
	for i := range cfg.OpenAICompatibility {
		entry := cfg.OpenAICompatibility[i]
		if entry.Disabled {
			continue
		}
		openAICompatCount += len(entry.APIKeyEntries)
	}

	total := authEntries + geminiAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + vertexAICompatCount + openAICompatCount
	fmt.Printf("server clients and configuration updated: %d clients (%d auth entries + %d Gemini API keys + %d Claude API keys + %d Codex keys + %d Vertex-compat + %d OpenAI-compat)\n",
		total,
		authEntries,
		geminiAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		vertexAICompatCount,
		openAICompatCount,
	)
}

func (s *Server) SetWebsocketAuthChangeHandler(fn func(bool, bool)) {
	if s == nil {
		return
	}
	s.wsAuthChanged = fn
}

// (management handlers moved to internal/api/handlers/management)

// AuthMiddleware returns a Gin middleware handler that authenticates requests
// using the configured authentication providers. When no providers are available,
// it allows all requests (legacy behaviour).
func AuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			c.Next()
			return
		}

		result, err := manager.Authenticate(c.Request.Context(), c.Request)
		if err == nil {
			if result != nil {
				c.Set("userApiKey", result.Principal)
				c.Set("accessProvider", result.Provider)
				if len(result.Metadata) > 0 {
					c.Set("accessMetadata", result.Metadata)
				}
			}
			c.Next()
			return
		}

		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
		}
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
	}
}

func configuredSignatureCacheEnabled(cfg *config.Config) bool {
	if cfg != nil && cfg.AntigravitySignatureCacheEnabled != nil {
		return *cfg.AntigravitySignatureCacheEnabled
	}
	return true
}

func applySignatureCacheConfig(oldCfg, cfg *config.Config) {
	newVal := configuredSignatureCacheEnabled(cfg)
	newStrict := configuredSignatureBypassStrict(cfg)
	if oldCfg == nil {
		cache.SetSignatureCacheEnabled(newVal)
		cache.SetSignatureBypassStrictMode(newStrict)
		return
	}

	oldVal := configuredSignatureCacheEnabled(oldCfg)
	if oldVal != newVal {
		cache.SetSignatureCacheEnabled(newVal)
	}

	oldStrict := configuredSignatureBypassStrict(oldCfg)
	if oldStrict != newStrict {
		cache.SetSignatureBypassStrictMode(newStrict)
	}
}

func configuredSignatureBypassStrict(cfg *config.Config) bool {
	if cfg != nil && cfg.AntigravitySignatureBypassStrict != nil {
		return *cfg.AntigravitySignatureBypassStrict
	}
	return false
}
