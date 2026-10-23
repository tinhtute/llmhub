// Package management provides the management API handlers and middleware
// for configuring the server and managing auth files.
package management

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/buildinfo"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/nativeproviders"
	"github.com/therealtinhtute/llmhub/internal/quotaalert"
	"github.com/therealtinhtute/llmhub/internal/runtimecontrol"
	"github.com/therealtinhtute/llmhub/internal/runtimepolicy"
	sdkAuth "github.com/therealtinhtute/llmhub/sdk/auth"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

type attemptInfo struct {
	count        int
	blockedUntil time.Time
	lastActivity time.Time // track last activity for cleanup
}

// attemptCleanupInterval controls how often stale IP entries are purged
const attemptCleanupInterval = 1 * time.Hour

// attemptMaxIdleTime controls how long an IP can be idle before cleanup
const attemptMaxIdleTime = 2 * time.Hour

// Handler aggregates config reference, persistence path and helpers.
type Handler struct {
	cfg                  *config.Config
	configFilePath       string
	mu                   sync.Mutex
	attemptsMu           sync.Mutex
	failedAttempts       map[string]*attemptInfo // keyed by client IP
	authManager          *coreauth.Manager
	tokenStore           coreauth.Store
	localPassword        string
	allowRemoteOverride  bool
	envSecret            string
	logDir               string
	runtimeStoragePolicy runtimepolicy.RuntimeStorage
	postAuthHook         coreauth.PostAuthHook
	configStore          ManagementConfigStore
	configVersion        int64
	nativeProviderStore  nativeproviders.Store
	configChangeHook     func(*config.Config)
	quotaAlertStore      quotaalert.Store
	quotaAlertCipher     *quotaalert.SecretCipher
	runtimeSettingsStore runtimecontrol.SettingsStore
	selfUpdateEngine     SelfUpdateEngine
	selfUpdateRunner     func(context.Context) error
	selfUpdateMu         sync.Mutex
}

type ManagementConfigStore interface {
	LoadConfigBytes(ctx context.Context) ([]byte, error)
	SaveConfig(ctx context.Context, data []byte) (int64, error)
}

// ConfigVersionProvider optionally provides the current monotonic version of the persisted config.
type ConfigVersionProvider interface {
	CurrentVersion(ctx context.Context) (int64, error)
}

// NewHandler creates a new management handler instance.
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager) *Handler {
	envSecret, _ := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)

	h := &Handler{
		cfg:                 cfg.CloneForRuntime(),
		configFilePath:      configFilePath,
		failedAttempts:      make(map[string]*attemptInfo),
		authManager:         manager,
		tokenStore:          sdkAuth.GetTokenStore(),
		allowRemoteOverride: envSecret != "",
		envSecret:           envSecret,
	}
	h.startAttemptCleanup()
	return h
}

// startAttemptCleanup launches a background goroutine that periodically
// removes stale IP entries from failedAttempts to prevent memory leaks.
func (h *Handler) startAttemptCleanup() {
	go func() {
		ticker := time.NewTicker(attemptCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			h.purgeStaleAttempts()
		}
	}()
}

// purgeStaleAttempts removes IP entries that have been idle beyond attemptMaxIdleTime
// and whose ban (if any) has expired.
func (h *Handler) purgeStaleAttempts() {
	now := time.Now()
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	for ip, ai := range h.failedAttempts {
		// Skip if still banned
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			continue
		}
		// Remove if idle too long
		if now.Sub(ai.lastActivity) > attemptMaxIdleTime {
			delete(h.failedAttempts, ip)
		}
	}
}

// NewHandler creates a new management handler instance.
func NewHandlerWithoutConfigFilePath(cfg *config.Config, manager *coreauth.Manager) *Handler {
	return NewHandler(cfg, "", manager)
}

// SetConfig updates the in-memory config reference when the server hot-reloads.
// The config is cloned so later management edits to h.cfg cannot race with
// the server snapshot handed to this call.
func (h *Handler) SetConfig(cfg *config.Config) {
	if h == nil {
		return
	}
	cloned := cfg.CloneForRuntime()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.isStaleConfigSnapshotLocked(cloned) {
		log.Debug("management: ignoring config reload parsed from an older config store version")
		return
	}
	if cloned.ConfigVersion > h.configVersion {
		h.configVersion = cloned.ConfigVersion
	}
	h.cfg = cloned
}

// isStaleConfigSnapshotLocked reports whether cfg was parsed from an older
// store version than the current store version. Management saves hold h.mu,
// so none can land during the check. Callers must hold h.mu.
func (h *Handler) isStaleConfigSnapshotLocked(cfg *config.Config) bool {
	if cfg == nil || h.configStore == nil {
		return false
	}
	currentVersion, err := h.currentConfigVersionLocked(context.Background())
	if err != nil || currentVersion == 0 {
		return false
	}
	if cfg.ConfigVersion > 0 {
		return cfg.ConfigVersion < currentVersion
	}
	// Fallback for reload snapshots that omit ConfigVersion (e.g. unversioned
	// ParseConfigBytes callers): compare against the store's current content.
	// If the store already holds newer content, this reload is stale.
	data, errLoad := h.configStore.LoadConfigBytes(context.Background())
	if errLoad != nil || len(data) == 0 {
		return false
	}
	if cfg.SourceHash != "" && cfg.SourceHash == config.ComputeSourceHash(data) {
		return false
	}
	storeCfg, errParse := config.ParseConfigBytes(data)
	if errParse != nil {
		return false
	}
	if h.cfg != nil && !configsEqual(cfg, storeCfg) {
		return true
	}
	return false
}

func configsEqual(a, b *config.Config) bool {
	if a == nil || b == nil {
		return a == b
	}
	normA := *a
	normB := *b
	normA.SourceHash, normA.ConfigVersion = "", 0
	normB.SourceHash, normB.ConfigVersion = "", 0
	normA.OpenAICompatibility = nativeproviders.StripProjectedEntries(normA.OpenAICompatibility)
	normB.OpenAICompatibility = nativeproviders.StripProjectedEntries(normB.OpenAICompatibility)
	bytesA, errA := yaml.Marshal(&normA)
	bytesB, errB := yaml.Marshal(&normB)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(bytesA, bytesB)
}

// configSnapshot returns a clone of the current config taken under h.mu, for
// code that runs outside the request holding the lock (OAuth login goroutines,
// token save helpers). It clones because management handlers mutate h.cfg in
// place under h.mu, so handing out the pointer would still race with them.
func (h *Handler) configSnapshot() *config.Config {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return nil
	}
	return h.cfg.CloneForRuntime()
}

func (h *Handler) currentConfigVersionLocked(ctx context.Context) (int64, error) {
	if h.configStore != nil {
		if vp, ok := h.configStore.(ConfigVersionProvider); ok {
			v, err := vp.CurrentVersion(ctx)
			if err == nil {
				if v > h.configVersion {
					h.configVersion = v
				}
				return v, nil
			}
			return h.configVersion, err
		}
	}
	if h.cfg != nil && h.cfg.ConfigVersion > h.configVersion {
		h.configVersion = h.cfg.ConfigVersion
	}
	return h.configVersion, nil
}

// validateIfMatchLocked validates the optional If-Match header against the current
// store version. When an If-Match header is present, comparison is strictly fail-closed:
// if current version cannot be read, is zero, or does not match, a 412 status is set on c and
// false is returned.
func (h *Handler) validateIfMatchLocked(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return true
	}
	values := c.Request.Header.Values("If-Match")
	if len(values) == 0 {
		return true
	}
	if h.configStore == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "management config store is not configured"})
		return false
	}
	currentVersion, errVer := h.currentConfigVersionLocked(c.Request.Context())
	if errVer != nil || currentVersion == 0 {
		if currentVersion > 0 {
			c.Header("ETag", formatConfigETag(currentVersion))
		}
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "precondition_failed"})
		return false
	}
	if !ifMatchSatisfied(values, currentVersion) {
		c.Header("ETag", formatConfigETag(currentVersion))
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "precondition_failed"})
		return false
	}
	return true
}

// formatConfigETag renders a config version as a strong ETag.
func formatConfigETag(version int64) string {
	return fmt.Sprintf(`"%d"`, version)
}

// ifMatchSatisfied reports whether the optional If-Match header lines pass
// the RFC 9110 strong comparison against currentVersion. Repeated lines form
// one list. An absent header always passes; a present but blank header
// carries no tag and fails; the whole field "*" matches any existing
// representation (version > 0); weak and malformed entity tags never match.
func ifMatchSatisfied(values []string, currentVersion int64) bool {
	if len(values) == 0 {
		return true
	}
	field := strings.TrimSpace(strings.Join(values, ","))
	if field == "*" {
		return currentVersion > 0
	}
	expected := strconv.FormatInt(currentVersion, 10)
	for _, tag := range splitEntityTagList(field) {
		tag = strings.TrimSpace(tag)
		if len(tag) >= 2 && tag[0] == '"' && tag[len(tag)-1] == '"' && !strings.Contains(tag[1:len(tag)-1], `"`) {
			val := tag[1 : len(tag)-1]
			if val == expected {
				return true
			}
		}
	}
	return false
}

// splitEntityTagList splits an If-Match field value on list commas. A comma
// inside a quoted entity tag is tag content, not a separator.
func splitEntityTagList(field string) []string {
	parts := []string{}
	start := 0
	quoted := false
	for i := range len(field) {
		switch field[i] {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				parts = append(parts, field[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, field[start:])
}

// SetAuthManager updates the auth manager reference used by management endpoints.
func (h *Handler) SetAuthManager(manager *coreauth.Manager) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.authManager = manager
	h.mu.Unlock()
}

// SetLocalPassword configures the runtime-local password accepted for localhost requests.
func (h *Handler) SetLocalPassword(password string) { h.localPassword = password }

// SetLogDirectory updates the directory where main.log should be looked up.
func (h *Handler) SetLogDirectory(dir string) {
	if dir == "" {
		return
	}
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	h.logDir = dir
}

func (h *Handler) SetRuntimeStoragePolicy(policy runtimepolicy.RuntimeStorage) {
	if h == nil {
		return
	}
	h.runtimeStoragePolicy = policy
}

// SetPostAuthHook registers a hook to be called after auth record creation but before persistence.
func (h *Handler) SetPostAuthHook(hook coreauth.PostAuthHook) {
	h.postAuthHook = hook
}

func (h *Handler) SetConfigStore(store ManagementConfigStore) {
	if h == nil {
		return
	}
	h.configStore = store
	if nativeStore, ok := store.(nativeproviders.Store); ok {
		h.nativeProviderStore = nativeStore
	}
}

func (h *Handler) SetConfigChangeHook(hook func(*config.Config)) {
	if h == nil {
		return
	}
	h.configChangeHook = hook
}

// SetSelfUpdateEngine wires the staging engine used by
// POST /v0/management/self-update (R13).
func (h *Handler) SetSelfUpdateEngine(engine SelfUpdateEngine) {
	if h == nil {
		return
	}
	h.selfUpdateEngine = engine
}

// SetSelfUpdateRestartRunner overrides the restart command runner used by
// POST /v0/management/self-update/apply. Tests inject it to observe exactly
// one invocation of the fixed command.
func (h *Handler) SetSelfUpdateRestartRunner(runner func(context.Context) error) {
	if h == nil {
		return
	}
	h.selfUpdateRunner = runner
}

// Middleware enforces access control for management endpoints.
// All requests (local and remote) require a valid management key.
// Additionally, remote access requires allow-remote-management=true.
func (h *Handler) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-CPA-VERSION", buildinfo.Version)
		c.Header("X-CPA-COMMIT", buildinfo.Commit)
		c.Header("X-CPA-BUILD-DATE", buildinfo.BuildDate)

		clientIP := c.ClientIP()
		localClient := clientIP == "127.0.0.1" || clientIP == "::1"

		// Accept either Authorization: Bearer <key> or X-Management-Key
		var provided string
		if ah := c.GetHeader("Authorization"); ah != "" {
			parts := strings.SplitN(ah, " ", 2)
			if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
				provided = parts[1]
			} else {
				provided = ah
			}
		}
		if provided == "" {
			provided = c.GetHeader("X-Management-Key")
		}

		allowed, statusCode, errMsg := h.AuthenticateManagementKey(clientIP, localClient, provided)
		if !allowed {
			c.AbortWithStatusJSON(statusCode, gin.H{"error": errMsg})
			return
		}
		c.Next()
	}
}

// AuthenticateManagementKey verifies the provided management key for the given client.
// It mirrors the behaviour of Middleware() so non-HTTP callers can reuse the same logic.
func (h *Handler) AuthenticateManagementKey(clientIP string, localClient bool, provided string) (bool, int, string) {
	const maxFailures = 5
	const banDuration = 30 * time.Minute

	if h == nil {
		return false, http.StatusForbidden, "remote management disabled"
	}

	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	var (
		allowRemote bool
		secretHash  string
	)
	if cfg != nil {
		allowRemote = cfg.RemoteManagement.AllowRemote
		secretHash = cfg.RemoteManagement.SecretKey
	}
	if h.allowRemoteOverride {
		allowRemote = true
	}
	envSecret := h.envSecret

	now := time.Now()
	h.attemptsMu.Lock()
	ai := h.failedAttempts[clientIP]
	if ai != nil && !ai.blockedUntil.IsZero() {
		if now.Before(ai.blockedUntil) {
			remaining := ai.blockedUntil.Sub(now).Round(time.Second)
			h.attemptsMu.Unlock()
			return false, http.StatusForbidden, fmt.Sprintf("IP banned due to too many failed attempts. Try again in %s", remaining)
		}
		// Ban expired, reset state
		ai.blockedUntil = time.Time{}
		ai.count = 0
	}
	h.attemptsMu.Unlock()

	if !localClient && !allowRemote {
		return false, http.StatusForbidden, "remote management disabled"
	}

	fail := func() {
		h.attemptsMu.Lock()
		aip := h.failedAttempts[clientIP]
		if aip == nil {
			aip = &attemptInfo{}
			h.failedAttempts[clientIP] = aip
		}
		aip.count++
		aip.lastActivity = time.Now()
		if aip.count >= maxFailures {
			aip.blockedUntil = time.Now().Add(banDuration)
			aip.count = 0
		}
		h.attemptsMu.Unlock()
	}

	reset := func() {
		h.attemptsMu.Lock()
		if ai := h.failedAttempts[clientIP]; ai != nil {
			ai.count = 0
			ai.blockedUntil = time.Time{}
		}
		h.attemptsMu.Unlock()
	}

	if secretHash == "" && envSecret == "" {
		return false, http.StatusForbidden, "remote management key not set"
	}

	if provided == "" {
		fail()
		return false, http.StatusUnauthorized, "missing management key"
	}

	if localClient {
		if lp := h.localPassword; lp != "" {
			if subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
				reset()
				return true, 0, ""
			}
		}
	}

	if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
		reset()
		return true, 0, ""
	}

	if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
		fail()
		return false, http.StatusUnauthorized, "invalid management key"
	}

	reset()

	return true, 0, ""
}

// persistLocked saves the current in-memory config to the configured store.
// It expects the caller to hold h.mu. The returned hook must be invoked only
// after h.mu has been released.
func (h *Handler) persistLocked(c *gin.Context) (*config.Config, func(*config.Config), bool) {
	if h.configStore == nil {
		if c != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "management config store is not configured"})
		}
		return nil, nil, false
	}
	if !h.validateIfMatchLocked(c) {
		return nil, nil, false
	}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	persistedCfg := *h.cfg
	persistedCfg.OpenAICompatibility = nativeproviders.StripProjectedEntries(h.cfg.OpenAICompatibility)
	data, errMarshal := yaml.Marshal(&persistedCfg)
	if errMarshal != nil {
		if c != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to encode config: %v", errMarshal)})
		}
		return nil, nil, false
	}
	version, errSave := h.configStore.SaveConfig(ctx, data)
	if errSave != nil {
		if c != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", errSave)})
		}
		return nil, nil, false
	}
	h.configVersion = version
	if h.cfg != nil {
		h.cfg.ConfigVersion = version
	}
	if c != nil {
		c.Header("ETag", formatConfigETag(version))
	}
	return h.cfg, h.configChangeHook, true
}

// persistLockedAndRespond is for callers that already hold h.mu and need the
// config change hook to run outside the critical section.
func (h *Handler) persistLockedAndRespond(c *gin.Context) bool {
	cfgForHook, hook, ok := h.persistLocked(c)
	h.mu.Unlock()
	if !ok {
		h.mu.Lock()
		return false
	}
	if hook != nil {
		hook(cfgForHook)
	}
	h.mu.Lock()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	return true
}

// Helper methods for simple types. The setter and the save run under h.mu, so a
// reload publishing h.cfg cannot replace it between the change and the save.
func (h *Handler) updateBoolField(c *gin.Context, set func(bool)) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.validateIfMatchLocked(c) {
		return
	}
	set(*body.Value)
	h.persistLockedAndRespond(c)
}

func (h *Handler) updateIntField(c *gin.Context, set func(int)) {
	var body struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.validateIfMatchLocked(c) {
		return
	}
	set(*body.Value)
	h.persistLockedAndRespond(c)
}

func (h *Handler) updateStringField(c *gin.Context, set func(string)) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.validateIfMatchLocked(c) {
		return
	}
	set(*body.Value)
	h.persistLockedAndRespond(c)
}
