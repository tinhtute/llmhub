package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/clienterror"
	internalconfig "github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/home"
	"github.com/therealtinhtute/llmhub/internal/logging"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/therealtinhtute/llmhub/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	cliproxysession "github.com/therealtinhtute/llmhub/sdk/cliproxy/session"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// RequestAuthPreparer lets an executor update missing auth metadata immediately
// before a request. Manager serializes and persists returned updates.
type RequestAuthPreparer interface {
	ShouldPrepareRequestAuth(auth *Auth) bool
	PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error)
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

const (
	homeAuthCountMetadataKey = "__cliproxy_home_auth_count"
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval    = 5 * time.Second
	refreshMaxConcurrency   = 16
	refreshPendingBackoff   = time.Minute
	refreshFailureBackoff   = 5 * time.Minute
	invalidGrantBackoffBase = time.Minute
	invalidGrantBackoffMax  = 30 * time.Minute
	// refreshIneffectiveBackoff throttles refresh attempts when an executor returns
	// success but the auth still evaluates as needing refresh (e.g. token expiry
	// wasn't updated). Without this guard, the auto-refresh loop can tight-loop and
	// burn CPU at idle.
	refreshIneffectiveBackoff = 30 * time.Second
	quotaBackoffBase          = time.Second
	quotaBackoffMax           = 30 * time.Minute
	// minQuotaCooldownFloor bounds quota-error cooldowns derived from provider
	// Retry-After hints so sub-second values cannot trigger zero-wait retry storms.
	minQuotaCooldownFloor = 10 * time.Second
	// transientErrorCooldown is the default cooldown for recoverable upstream
	// failures (408/500/502/503/504 and Cloudflare 520-526 origin errors).
	transientErrorCooldown = time.Minute
)

var quotaCooldownDisabled atomic.Bool

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

var transientErrorCooldownSeconds atomic.Int64

// SetTransientErrorCooldownSeconds configures cooldowns for transient upstream
// errors (408/500/502/503/504/520-526). 0 keeps the legacy transientErrorCooldown
// default; negative values disable these cooldowns.
func SetTransientErrorCooldownSeconds(seconds int) {
	transientErrorCooldownSeconds.Store(int64(seconds))
}

// QuotaCooldownDisabledForAuth returns whether cooling is disabled for the auth under global settings.
func QuotaCooldownDisabledForAuth(auth *Auth) bool {
	return quotaCooldownDisabledForAuth(auth)
}

// QuotaCooldownDisabledForAuthWithConfig returns whether cooling is disabled for the auth with the given config.
func QuotaCooldownDisabledForAuthWithConfig(auth *Auth, cfg *internalconfig.Config) bool {
	return quotaCooldownDisabledForAuthWithConfig(auth, cfg)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	return quotaCooldownDisabledForAuthWithConfig(auth, nil)
}

// cooldownDisabledForAuth resolves the cooling policy for auth against the
// manager's latest runtime config, so provider-level disable_cooling overrides
// (e.g. openai-compatibility entries) are honored during request-time decisions.
func (m *Manager) cooldownDisabledForAuth(auth *Auth) bool {
	if m == nil {
		return quotaCooldownDisabledForAuth(auth)
	}
	return quotaCooldownDisabledForAuthWithConfig(auth, m.runtimeConfigSnapshot())
}

// recoverableFailureRetryAfterWithHint computes the retry deadline for
// recoverable upstream failures (408/500/502/503/504 and Cloudflare 520-526
// origin errors). A positive provider RetryAfter hint takes precedence over the
// configured transientErrorCooldownSeconds override and the default
// transientErrorCooldown. Negative transientErrorCooldownSeconds or
// disableCooling return a zero time (no cooldown).
func recoverableFailureRetryAfterWithHint(now time.Time, retryAfter *time.Duration, disableCooling bool) time.Time {
	if disableCooling {
		return time.Time{}
	}
	seconds := transientErrorCooldownSeconds.Load()
	if seconds < 0 {
		return time.Time{}
	}
	if retryAfter != nil && *retryAfter > 0 {
		return now.Add(*retryAfter)
	}
	if seconds == 0 {
		return now.Add(transientErrorCooldown)
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

func quotaCooldownDisabledForAuthWithConfig(auth *Auth, cfg *internalconfig.Config) bool {
	// Home owns cooldown state, so downstream instances must not schedule local cooldowns.
	if cfg != nil && cfg.Home.Enabled {
		return true
	}
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
		if override, ok := providerCoolingOverrideForAuth(auth, cfg); ok {
			return override
		}
	}
	if cfg != nil && cfg.DisableCooling {
		return true
	}
	return quotaCooldownDisabled.Load()
}

func providerCoolingOverrideForAuth(auth *Auth, cfg *internalconfig.Config) (bool, bool) {
	if auth == nil || cfg == nil {
		return false, false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		return false, false
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if providerKey == "" && compatName == "" && provider != "openai-compatibility" {
		return false, false
	}
	if providerKey == "" {
		providerKey = provider
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, provider)
	if entry == nil || entry.DisableCooling == nil {
		return false, false
	}
	return *entry.DisableCooling, true
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// RouteModel is the requested logical route model before alias resolution.
	RouteModel string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// CredentialScope indicates that the failure affects the whole credential
	// across models (e.g. Codex usage_limit_reached quota exhaustion when
	// model-level cooling is disabled, upstream b064b832e242).
	CredentialScope bool
	// Error describes the failure when Success is false.
	Error *Error
	// RequestScoped marks failures caused by the request rather than the credential.
	RequestScoped bool
	// Options carries execution request options (headers, metadata, etc.) for result tracking.
	Options cliproxyexecutor.Options
	// SkipQuotaObservation reports that this result must not replace the last
	// observed watermark. Count-tokens requests reuse the credential but are not
	// generation traffic; their response headers are not a generation snapshot.
	SkipQuotaObservation bool
	// UsageDetail carries provider token usage for runtime account accounting.
	UsageDetail coreusage.Detail
	// UsageEstimated marks usage values estimated from provider runtime signals.
	UsageEstimated bool
	// ExecutionSeq is the manager-local monotonic sequence assigned when the
	// execution was dispatched upstream. It orders in-flight executions so a
	// success dispatched before a cooldown was recorded cannot clear that
	// cooldown. Zero means unknown (direct MarkResult callers) and is exempt
	// from the ordering guard.
	ExecutionSeq uint64
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

// StoppableSelector is an optional interface for selectors that hold resources.
// Selectors that implement this interface will have Stop called during shutdown.
type StoppableSelector interface {
	Selector
	Stop()
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

// hookChain forwards lifecycle callbacks to two hooks in order.
type hookChain struct {
	first  Hook
	second Hook
}

func (h hookChain) OnAuthRegistered(ctx context.Context, auth *Auth) {
	h.first.OnAuthRegistered(ctx, auth)
	h.second.OnAuthRegistered(ctx, auth)
}

func (h hookChain) OnAuthUpdated(ctx context.Context, auth *Auth) {
	h.first.OnAuthUpdated(ctx, auth)
	h.second.OnAuthUpdated(ctx, auth)
}

func (h hookChain) OnResult(ctx context.Context, result Result) {
	h.first.OnResult(ctx, result)
	h.second.OnResult(ctx, result)
}

// hookBox boxes a Hook so it can be published atomically after construction.
type hookBox struct {
	hook Hook
}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store     Store
	executors map[string]ProviderExecutor
	selector  Selector
	hook      Hook
	// lateHook carries hooks added via AddHook after construction; read under
	// currentHook so the immutable construction hook always runs first.
	lateHook atomic.Pointer[hookBox]
	mu       sync.RWMutex
	auths    map[string]*Auth
	// executionSeq stamps upstream dispatches with a monotonic sequence used
	// to reject cooldown clears from executions dispatched before the cooldown.
	executionSeq atomic.Uint64
	scheduler    *authScheduler
	// homeRuntimeAuths caches auths returned by Home so websocket sessions can
	// reuse an established upstream credential without dispatching every turn.
	homeRuntimeAuths      map[string]map[string]*Auth
	homeRuntimeAuthOwners map[string]map[string]*HomeDispatchSelection
	homeSessionSelections map[string]map[homeSessionSelectionKey]*HomeDispatchSelection
	homeSessionLocks      sync.Map
	homeSessionAliases    homeSessionAliasCache
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets             map[string]int
	homeDispatchBundle          atomic.Pointer[HomeDispatchBundle]
	homeInFlightPublisherConfig atomic.Pointer[HomeInFlightPublisherConfig]

	// Retry controls request retry behavior.
	requestRetry        atomic.Int32
	maxRetryCredentials atomic.Int32
	maxRetryInterval    atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// apiKeyModelCapabilities caches resolved model capability snapshots for
	// configured-model auths (upstream apiKeyModelRoutingSnapshot.capabilities).
	apiKeyModelCapabilities atomic.Value

	// modelPoolOffsets tracks per-auth alias pool rotation state.
	modelPoolOffsets map[string]int

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// comboResolver resolves combo names to candidate lists; combos and rotation
	// cursor state are (re)built from the runtime config.
	comboResolver *ComboResolver

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// Auto refresh state
	refreshCancel context.CancelFunc
	refreshLoop   *authAutoRefreshLoop
	// refreshJobs retains queued and running jobs across loop restarts under m.mu.
	refreshJobs map[string]*authRefreshJob

	cooldownStateStore CooldownStateStore

	requestPrepareLocks sync.Map
	// refreshLocks serializes credential refresh per auth ID so concurrent
	// 401 recoveries and auto-refresh workers do not race the same refresh_token.
	// Ported from upstream CLIProxyAPI (conductor.go).
	refreshLocks sync.Map
	// persistLocks serializes disk persistence per auth ID and guards against out-of-order writes.
	// Ported from upstream CLIProxyAPI commit 4c1bebe837a6.
	persistLocks sync.Map
	// authMutationLocks serializes mutations per auth ID (upstream lockAuthMutationContext).
	authMutationLocks sync.Map
	// from a previous registration cycle are rejected (upstream 4c1bebe837a6).
	authEpochs map[string]uint64
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:                 store,
		executors:             make(map[string]ProviderExecutor),
		selector:              selector,
		hook:                  hook,
		auths:                 make(map[string]*Auth),
		homeRuntimeAuths:      make(map[string]map[string]*Auth),
		homeRuntimeAuthOwners: make(map[string]map[string]*HomeDispatchSelection),
		homeSessionSelections: make(map[string]map[homeSessionSelectionKey]*HomeDispatchSelection),
		providerOffsets:       make(map[string]int),
		modelPoolOffsets:      make(map[string]int),
		authEpochs:            make(map[string]uint64),
	}
	manager.comboResolver = NewComboResolver()
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.apiKeyModelCapabilities.Store(&apiKeyModelCapabilitySnapshot{config: &internalconfig.Config{}})
	defaultInFlightConfig, errInFlightConfig := HomeInFlightPublisherConfigFromConfig(internalconfig.DefaultCredentialInFlightConfig())
	if errInFlightConfig == nil {
		manager.ApplyHomeInFlightPublisherConfig(defaultInFlightConfig)
	}
	manager.scheduler = newAuthScheduler(selector)
	return manager
}

// AddHook appends a lifecycle hook after construction; it runs after the
// construction-time hook and any previously added hooks.
func (m *Manager) AddHook(hook Hook) {
	if m == nil || hook == nil {
		return
	}
	for {
		existing := m.lateHook.Load()
		var combined hookBox
		if existing != nil {
			combined = hookBox{hook: hookChain{first: existing.hook, second: hook}}
		} else {
			combined = hookBox{hook: hook}
		}
		if m.lateHook.CompareAndSwap(existing, &combined) {
			return
		}
	}
}

// currentHook resolves the effective hook: construction hook, or its chain with
// any post-construction hooks. Never nil.
func (m *Manager) currentHook() Hook {
	if m == nil {
		return NoopHook{}
	}
	hook := m.hook
	if hook == nil {
		hook = NoopHook{}
	}
	if late := m.lateHook.Load(); late != nil && late.hook != nil {
		hook = hookChain{first: hook, second: late.hook}
	}
	return hook
}

// HomeDispatchBundle is the immutable client and registry pair for one Home lifetime.
type HomeDispatchBundle struct {
	client     homeAuthDispatcher
	registry   *executionregistry.Registry
	generation uint64
}

func (m *Manager) PublishHomeDispatch(client homeAuthDispatcher, registry *executionregistry.Registry, generation uint64) *HomeDispatchBundle {
	if m == nil || client == nil || registry == nil {
		return nil
	}
	bundle := &HomeDispatchBundle{client: client, registry: registry, generation: generation}
	m.homeDispatchBundle.Store(bundle)
	return bundle
}

func (m *Manager) ClearHomeDispatchBundle(bundle *HomeDispatchBundle) bool {
	return m != nil && bundle != nil && m.homeDispatchBundle.CompareAndSwap(bundle, nil)
}

func (m *Manager) HomeDispatchBundle() *HomeDispatchBundle {
	if m == nil {
		return nil
	}
	return m.homeDispatchBundle.Load()
}

func (m *Manager) SetHomeExecutionRegistry(registry *executionregistry.Registry) {
	if m != nil {
		m.PublishHomeDispatch(currentHomeDispatcher(), registry, 0)
	}
}

func (m *Manager) ClearHomeExecutionRegistry(registry *executionregistry.Registry) bool {
	bundle := m.HomeDispatchBundle()
	return bundle != nil && bundle.registry == registry && m.ClearHomeDispatchBundle(bundle)
}

func (m *Manager) HomeExecutionRegistry() *executionregistry.Registry {
	bundle := m.HomeDispatchBundle()
	if bundle == nil {
		return nil
	}
	return bundle.registry
}

func isBuiltInSelector(selector Selector) bool {
	switch selector.(type) {
	case *RoundRobinSelector, *FillFirstSelector:
		return true
	default:
		return false
	}
}

func (m *Manager) syncSchedulerFromSnapshot(auths []*Auth) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.rebuild(auths)
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

// RefreshSchedulerEntry re-upserts a single auth into the scheduler so that its
// supportedModelSet is rebuilt from the current global model registry state.
// This must be called after models have been registered for a newly added auth,
// because the initial scheduler.upsertAuth during Register/Update runs before
// registerModelsForAuth and therefore snapshots an empty model set.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()
	m.scheduler.upsertAuth(snapshot)
}

// ReconcileRegistryModelStates aligns per-model runtime state with the current
// registry snapshot for one auth.
//
// Supported models are reset to a clean state because re-registration already
// cleared the registry-side cooldown/suspension snapshot. ModelStates for
// models that are no longer present in the registry are pruned entirely so
// renamed/removed models cannot keep auth-level status stale.
func (m *Manager) ReconcileRegistryModelStates(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}

	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	supported := make(map[string]struct{}, len(supportedModels))
	for _, model := range supportedModels {
		if model == nil {
			continue
		}
		modelKey := canonicalModelKey(model.ID)
		if modelKey == "" {
			continue
		}
		supported[modelKey] = struct{}{}
	}

	var snapshot *Auth
	now := time.Now()

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if ok && auth != nil && len(auth.ModelStates) > 0 {
		changed := false
		for modelKey, state := range auth.ModelStates {
			baseModel := canonicalModelKey(modelKey)
			if baseModel == "" {
				baseModel = strings.TrimSpace(modelKey)
			}
			if _, supportedModel := supported[baseModel]; !supportedModel {
				// Drop state for models that disappeared from the current registry
				// snapshot. Keeping them around leaks stale errors into auth-level
				// status, management output, and websocket fallback checks.
				delete(auth.ModelStates, modelKey)
				changed = true
				continue
			}
			if state == nil {
				continue
			}
			if modelStateIsClean(state) {
				continue
			}
			resetModelState(state, now)
			changed = true
		}
		if len(auth.ModelStates) == 0 {
			auth.ModelStates = nil
		}
		if changed {
			updateAggregatedAvailability(auth, now)
			if !hasModelError(auth, now) {
				auth.LastError = nil
				auth.StatusMessage = ""
				auth.Status = StatusActive
			}
			auth.UpdatedAt = now
			if errPersist := m.persist(ctx, auth); errPersist != nil {
				logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth changes during model state reconciliation: %v", errPersist)
			}
			snapshot = auth.Clone()
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

// RestoreCooldownState reapplies durable runtime cooldowns after auth/model registration.
func (m *Manager) RestoreCooldownState(ctx context.Context) (int, error) {
	store := m.currentCooldownStateStore()
	if store == nil {
		return 0, nil
	}
	records, err := store.Load(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	applied, snapshots, registryRecords := m.applyCooldownStateRecords(records, now)
	if m.scheduler != nil {
		for _, snapshot := range snapshots {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	for _, record := range registryRecords {
		if record.Model == "" {
			continue
		}
		if record.Quota.Exceeded {
			registry.GetGlobalRegistry().SetModelQuotaExceeded(record.AuthID, record.Model)
		}
		registry.GetGlobalRegistry().SuspendClientModel(record.AuthID, record.Model, cooldownRecordReason(record))
	}
	if err := m.saveCooldownStateSnapshot(ctx, store, now); err != nil {
		return applied, err
	}
	return applied, nil
}

// nextExecutionSeq stamps a new upstream dispatch with the next monotonic
// execution sequence. The stamp travels on the execution's Result so that
// MarkResult can order the execution against cooldowns recorded meanwhile.
func (m *Manager) nextExecutionSeq() uint64 {
	if m == nil {
		return 0
	}
	return m.executionSeq.Add(1)
}

// currentExecutionSeq returns the latest execution sequence issued so far.
// Capturing it as the cooldown watermark when a cooldown is recorded marks
// every execution dispatched up to that moment as predating the cooldown.
func (m *Manager) currentExecutionSeq() uint64 {
	if m == nil {
		return 0
	}
	return m.executionSeq.Load()
}

func (m *Manager) applyCooldownStateRecords(records []CooldownStateRecord, now time.Time) (int, []*Auth, []CooldownStateRecord) {
	if m == nil || len(records) == 0 {
		return 0, nil, nil
	}
	modified := make(map[string]*Auth)
	registryRecords := make([]CooldownStateRecord, 0, len(records))
	applied := 0

	m.mu.Lock()
	for _, raw := range records {
		record, err := raw.Normalize(now)
		if err != nil || record.Expired(now) {
			continue
		}
		auth, ok := m.auths[record.AuthID]
		if !ok || auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if record.Model == "" {
			applyAuthCooldownRecord(auth, record)
			modified[auth.ID] = auth
			applied++
			continue
		}
		if !cooldownRecordModelSupported(record.AuthID, record.Model) {
			continue
		}
		state := ensureModelState(auth, record.Model)
		applyModelCooldownRecord(state, record)
		auth.Status = StatusError
		if record.LastError != nil {
			auth.LastError = cloneError(record.LastError)
		}
		if reason := cooldownRecordReason(record); reason != "" {
			auth.StatusMessage = reason
		}
		updateAggregatedAvailability(auth, now)
		auth.UpdatedAt = record.UpdatedAt
		modified[auth.ID] = auth
		registryRecords = append(registryRecords, record)
		applied++
	}
	snapshots := make([]*Auth, 0, len(modified))
	for _, auth := range modified {
		snapshots = append(snapshots, auth.Clone())
	}
	m.mu.Unlock()

	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	return applied, snapshots, registryRecords
}

func applyAuthCooldownRecord(auth *Auth, record CooldownStateRecord) {
	if auth == nil {
		return
	}
	auth.Unavailable = true
	auth.Status = cooldownRecordStatus(record.Status)
	auth.StatusMessage = cooldownRecordReason(record)
	auth.NextRetryAfter = record.NextRetryAfter
	auth.Quota = record.Quota
	auth.LastError = cloneError(record.LastError)
	auth.UpdatedAt = record.UpdatedAt
}

func applyModelCooldownRecord(state *ModelState, record CooldownStateRecord) {
	if state == nil {
		return
	}
	state.Unavailable = true
	state.Status = cooldownRecordStatus(record.Status)
	state.StatusMessage = cooldownRecordReason(record)
	state.NextRetryAfter = record.NextRetryAfter
	state.Quota = record.Quota
	state.LastError = cloneError(record.LastError)
	state.UpdatedAt = record.UpdatedAt
}

func cooldownRecordStatus(status string) Status {
	switch Status(strings.TrimSpace(status)) {
	case StatusActive, StatusPending, StatusRefreshing, StatusError, StatusDisabled:
		return Status(strings.TrimSpace(status))
	default:
		return StatusError
	}
}

func cooldownRecordReason(record CooldownStateRecord) string {
	if reason := strings.TrimSpace(record.Quota.Reason); reason != "" {
		return reason
	}
	return strings.TrimSpace(record.Reason)
}

func cooldownRecordModelSupported(authID, model string) bool {
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return false
	}
	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(supportedModels) == 0 {
		return true
	}
	target := canonicalModelKey(model)
	for _, supportedModel := range supportedModels {
		if supportedModel == nil {
			continue
		}
		registered := strings.TrimSpace(supportedModel.ID)
		if registered == "" {
			continue
		}
		if registered == model || canonicalModelKey(registered) == target {
			return true
		}
	}
	return false
}

func (m *Manager) currentCooldownStateStore() CooldownStateStore {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	store := m.cooldownStateStore
	m.mu.RUnlock()
	return store
}

func (m *Manager) persistCooldownStateSnapshot(ctx context.Context) {
	store := m.currentCooldownStateStore()
	if store == nil {
		return
	}
	if err := m.saveCooldownStateSnapshot(ctx, store, time.Now()); err != nil {
		logEntryWithRequestID(ctx).Warnf("failed to persist cooldown state snapshot: %v", err)
	}
}

func (m *Manager) saveCooldownStateSnapshot(ctx context.Context, store CooldownStateStore, now time.Time) error {
	if m == nil || store == nil {
		return nil
	}
	records := m.cooldownStateSnapshot(now)
	return store.Save(ctx, records)
}

func (m *Manager) cooldownStateSnapshot(now time.Time) []CooldownStateRecord {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	records := make([]CooldownStateRecord, 0)
	for _, auth := range m.auths {
		if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if cooldownAuthRestorable(auth, now) {
			records = append(records, cooldownRecordFromAuth(auth))
		}
		for model, state := range auth.ModelStates {
			if !cooldownModelRestorable(state, now) {
				continue
			}
			records = append(records, cooldownRecordFromModel(auth, model, state))
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].AuthID != records[j].AuthID {
			return records[i].AuthID < records[j].AuthID
		}
		return records[i].Model < records[j].Model
	})
	return records
}

// EarliestComboReset returns the earliest NextRetryAfter among current cooldown
// records matching any of the candidates' provider/model pairs, when that reset
// is after now.
func (m *Manager) EarliestComboReset(candidates []ComboCandidate, now time.Time) (time.Time, bool) {
	if m == nil || len(candidates) == 0 {
		return time.Time{}, false
	}
	var earliest time.Time
	found := false
	for _, record := range m.cooldownStateSnapshot(now) {
		matches := false
		for _, candidate := range candidates {
			if strings.EqualFold(record.Provider, candidate.Provider) && (record.Model == "" || record.Model == candidate.Model) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if !found || record.NextRetryAfter.Before(earliest) {
			earliest = record.NextRetryAfter
			found = true
		}
	}
	return earliest, found
}

func cooldownAuthRestorable(auth *Auth, now time.Time) bool {
	// An auth-level record is saved even when per-model states exist, so
	// credential-scoped cooldowns (e.g. credential_quota) survive restarts
	// alongside their model records (upstream ed70aeaa1627 parity).
	return auth != nil && auth.Unavailable && auth.NextRetryAfter.After(now)
}

func cooldownModelRestorable(state *ModelState, now time.Time) bool {
	return state != nil && state.Unavailable && state.NextRetryAfter.After(now)
}

func cooldownRecordFromAuth(auth *Auth) CooldownStateRecord {
	return CooldownStateRecord{
		Provider:       strings.ToLower(strings.TrimSpace(auth.Provider)),
		AuthID:         auth.ID,
		AuthFile:       auth.FileName,
		Status:         string(auth.Status),
		NextRetryAfter: auth.NextRetryAfter,
		Reason:         strings.TrimSpace(auth.StatusMessage),
		Quota:          auth.Quota,
		LastError:      cloneError(auth.LastError),
		UpdatedAt:      auth.UpdatedAt,
	}
}

func cooldownRecordFromModel(auth *Auth, model string, state *ModelState) CooldownStateRecord {
	return CooldownStateRecord{
		Provider:       strings.ToLower(strings.TrimSpace(auth.Provider)),
		AuthID:         auth.ID,
		AuthFile:       auth.FileName,
		Model:          strings.TrimSpace(model),
		Status:         string(state.Status),
		NextRetryAfter: state.NextRetryAfter,
		Reason:         strings.TrimSpace(state.StatusMessage),
		Quota:          state.Quota,
		LastError:      cloneError(state.LastError),
		UpdatedAt:      state.UpdatedAt,
	}
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.mu.Lock()
	m.selector = selector
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// SetCooldownStateStore configures optional runtime cooldown snapshot persistence.
func (m *Manager) SetCooldownStateStore(store CooldownStateStore) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.cooldownStateStore = store
	m.mu.Unlock()
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	previous, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	previousAliasTTL := homeSessionAliasTTL(previous)
	m.runtimeConfig.Store(cfg)
	if homeSessionAliasTTL(cfg) != previousAliasTTL {
		m.homeSessionAliases.clear()
	}
	if !cfg.Home.Enabled {
		m.clearHomeRuntimeAuths()
		m.homeSessionAliases.clear()
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	// Rebuild combo definitions and clear rotation cursors on every config
	// change, including the watcher's debounced reload.
	if m.comboResolver != nil {
		m.comboResolver.SetCombos(cfg.Combos)
	}
}

// ComboResolver returns the manager's combo resolver.
func (m *Manager) ComboResolver() *ComboResolver {
	if m == nil {
		return nil
	}
	return m.comboResolver
}

// HomeEnabled reports whether the home control plane integration is enabled in the runtime config.
func (m *Manager) HomeEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.Home.Enabled
}

func (m *Manager) lookupAPIKeyUpstreamModel(authID, requestedModel string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if table == nil {
		return ""
	}
	byAlias := table[authID]
	if len(byAlias) == 0 {
		return ""
	}
	key := strings.ToLower(thinking.ParseSuffix(requestedModel).ModelName)
	if key == "" {
		key = strings.ToLower(requestedModel)
	}
	resolved := strings.TrimSpace(byAlias[key])
	if resolved == "" {
		return ""
	}
	return preserveRequestedModelSuffix(requestedModel, resolved)
}

func isAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	kind, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(kind), "api_key")
}

func isOpenAICompatAPIKeyAuth(auth *Auth) bool {
	if !isAPIKeyAuth(auth) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

func openAICompatProviderKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if providerKey := strings.TrimSpace(auth.Attributes["provider_key"]); providerKey != "" {
			return strings.ToLower(providerKey)
		}
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			return strings.ToLower(compatName)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func openAICompatModelPoolKey(auth *Auth, requestedModel string) string {
	base := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if base == "" {
		base = strings.TrimSpace(requestedModel)
	}
	return strings.ToLower(strings.TrimSpace(auth.ID)) + "|" + openAICompatProviderKey(auth) + "|" + strings.ToLower(base)
}

func (m *Manager) nextModelPoolOffset(key string, size int) int {
	if m == nil || size <= 1 {
		return 0
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelPoolOffsets == nil {
		m.modelPoolOffsets = make(map[string]int)
	}
	offset := m.modelPoolOffsets[key]
	if offset >= 2_147_483_640 {
		offset = 0
	}
	m.modelPoolOffsets[key] = offset + 1
	if size <= 0 {
		return 0
	}
	return offset % size
}

func rotateStrings(values []string, offset int) []string {
	if len(values) <= 1 {
		return values
	}
	if offset <= 0 {
		out := make([]string, len(values))
		copy(out, values)
		return out
	}
	offset = offset % len(values)
	out := make([]string, 0, len(values))
	out = append(out, values[offset:]...)
	out = append(out, values[:offset]...)
	return out
}

func (m *Manager) resolveOpenAICompatUpstreamModelPool(auth *Auth, requestedModel string) []string {
	if m == nil || !isOpenAICompatAPIKeyAuth(auth) {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func preserveRequestedModelSuffix(requestedModel, resolved string) string {
	return preserveResolvedModelSuffix(resolved, thinking.ParseSuffix(requestedModel))
}

func (m *Manager) executionModelCandidates(auth *Auth, routeModel string) []string {
	if auth != nil && auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			return []string{homeModel}
		}
	}
	requestedModel := rewriteModelForAuth(routeModel, auth)
	requestedModel = m.applyOAuthModelAlias(auth, requestedModel)
	if pool := m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		if len(pool) == 1 {
			return pool
		}
		offset := m.nextModelPoolOffset(openAICompatModelPoolKey(auth, requestedModel), len(pool))
		return rotateStrings(pool, offset)
	}
	resolved := m.applyAPIKeyModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolved) == "" {
		resolved = requestedModel
	}
	return []string{resolved}
}

func (m *Manager) selectionModelForAuth(auth *Auth, routeModel string) string {
	requestedModel := rewriteModelForAuth(routeModel, auth)
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = strings.TrimSpace(routeModel)
	}
	resolvedModel := m.applyOAuthModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolvedModel) == "" {
		resolvedModel = requestedModel
	}
	return resolvedModel
}

func (m *Manager) selectionModelKeyForAuth(auth *Auth, routeModel string) string {
	return canonicalModelKey(m.selectionModelForAuth(auth, routeModel))
}

func (m *Manager) stateModelForExecution(auth *Auth, routeModel, upstreamModel string, pooled bool) string {
	if auth != nil && auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			if resolved := strings.TrimSpace(upstreamModel); resolved != "" {
				return resolved
			}
			return homeModel
		}
	}
	stateModel := executionResultModel(routeModel, upstreamModel, pooled)
	selectionModel := m.selectionModelForAuth(auth, routeModel)
	if canonicalModelKey(selectionModel) == canonicalModelKey(upstreamModel) && strings.TrimSpace(selectionModel) != "" {
		return strings.TrimSpace(upstreamModel)
	}
	return stateModel
}

func executionResultModel(routeModel, upstreamModel string, pooled bool) string {
	if pooled {
		if resolved := strings.TrimSpace(upstreamModel); resolved != "" {
			return resolved
		}
	}
	if requested := strings.TrimSpace(routeModel); requested != "" {
		return requested
	}
	return strings.TrimSpace(upstreamModel)
}

func (m *Manager) filterExecutionModels(auth *Auth, routeModel string, candidates []string, pooled bool) []string {
	if len(candidates) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]string, 0, len(candidates))
	for _, upstreamModel := range candidates {
		stateModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
		blocked, _, _ := isAuthBlockedForModel(auth, stateModel, now)
		if blocked {
			continue
		}
		out = append(out, upstreamModel)
	}
	return out
}

func (m *Manager) preparedExecutionModels(auth *Auth, routeModel string) ([]string, bool) {
	candidates := m.executionModelCandidates(auth, routeModel)
	pooled := len(candidates) > 1
	return m.filterExecutionModels(auth, routeModel, candidates, pooled), pooled
}

func (m *Manager) prepareExecutionModels(auth *Auth, routeModel string) []string {
	models, _ := m.preparedExecutionModels(auth, routeModel)
	return models
}

func (m *Manager) availableAuthsForRouteModel(auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	return m.availableAuthsForRouteModelWithPriorityMode(auths, provider, routeModel, now, false)
}

func (m *Manager) availableAuthsForRouteModelAcrossPriorities(auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	return m.availableAuthsForRouteModelWithPriorityMode(auths, provider, routeModel, now, true)
}

func (m *Manager) availableAuthsForRouteModelWithPriorityMode(auths []*Auth, provider, routeModel string, now time.Time, allPriorities bool) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority := make(map[int][]*Auth)
	cooldownCount := 0
	unauthorizedCount := 0
	var earliest time.Time
	for _, candidate := range auths {
		checkModel := m.selectionModelForAuth(candidate, routeModel)
		blocked, reason, next := isAuthBlockedForModel(candidate, checkModel, now)
		if !blocked {
			priority := authPriority(candidate)
			availableByPriority[priority] = append(availableByPriority[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
		}
		// Ported from upstream CLIProxyAPI commit 6724a95851f9: track the earliest
		// recovery deadline for every non-disabled block (cooldown or transient)
		// so unavailable errors can advertise a locally computed Retry-After hint.
		if reason != blockReasonDisabled && next.After(now) && (earliest.IsZero() || next.Before(earliest)) {
			earliest = next
		}
		if hasUnauthorizedAuthFailure(candidate) {
			unauthorizedCount++
		}
	}

	if len(availableByPriority) == 0 {
		lastCandidateErr := latestCandidateErrorForModel(auths, func(candidate *Auth) string {
			return m.selectionModelForAuth(candidate, routeModel)
		})
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownErrorWithCause(routeModel, providerForError, resetIn, lastCandidateErr)
		}
		if unauthorizedCount == len(auths) && len(auths) > 0 {
			terminalCause := latestUnauthorizedCandidateError(auths)
			if terminalCause == nil {
				terminalCause = lastCandidateErr
			}
			return nil, NewTerminalAuthError(&Error{
				Code:       "auth_unavailable",
				Message:    "no auth available",
				Retryable:  false,
				HTTPStatus: http.StatusServiceUnavailable,
			}, terminalCause)
		}
		return nil, newAuthUnavailableErrorWithCause(earliest, now, lastCandidateErr)
	}

	return availableAuthsFromPriorityBuckets(availableByPriority, allPriorities), nil
}

func latestCandidateErrorForModel(auths []*Auth, selectionModelFunc func(*Auth) string) error {
	var latestModelTime time.Time
	var latestModelAuthID string
	var latestModelErr error

	var latestAuthTime time.Time
	var latestAuthID string
	var latestAuthErr error

	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		var curModelErr error
		var curModelTime time.Time
		if len(candidate.ModelStates) > 0 {
			checkModel := ""
			if selectionModelFunc != nil {
				checkModel = selectionModelFunc(candidate)
			}
			modelKey := canonicalModelKey(checkModel)
			if checkModel != "" {
				if state, ok := candidate.ModelStates[checkModel]; ok && state != nil {
					if state.LastError != nil {
						curModelErr = state.LastError
						curModelTime = state.UpdatedAt
					} else if strings.TrimSpace(state.StatusMessage) != "" {
						curModelErr = errors.New(state.StatusMessage)
						curModelTime = state.UpdatedAt
					}
				}
			}
			if curModelErr == nil && modelKey != "" {
				if state, ok := candidate.ModelStates[modelKey]; ok && state != nil {
					if state.LastError != nil {
						curModelErr = state.LastError
						curModelTime = state.UpdatedAt
					} else if strings.TrimSpace(state.StatusMessage) != "" {
						curModelErr = errors.New(state.StatusMessage)
						curModelTime = state.UpdatedAt
					}
				}
			}
		}

		if curModelErr != nil {
			if curModelTime.IsZero() {
				curModelTime = candidate.UpdatedAt
			}
			if latestModelErr == nil || curModelTime.After(latestModelTime) || (curModelTime.Equal(latestModelTime) && candidate.ID > latestModelAuthID) {
				latestModelTime = curModelTime
				latestModelAuthID = candidate.ID
				latestModelErr = curModelErr
			}
		}

		var curAuthErr error
		var curAuthTime time.Time
		if candidate.LastError != nil {
			curAuthErr = candidate.LastError
			curAuthTime = candidate.UpdatedAt
		} else if strings.TrimSpace(candidate.StatusMessage) != "" {
			curAuthErr = errors.New(candidate.StatusMessage)
			curAuthTime = candidate.UpdatedAt
		}
		if curAuthErr != nil {
			if curAuthTime.IsZero() {
				curAuthTime = candidate.UpdatedAt
			}
			if latestAuthErr == nil || curAuthTime.After(latestAuthTime) || (curAuthTime.Equal(latestAuthTime) && candidate.ID > latestAuthID) {
				latestAuthTime = curAuthTime
				latestAuthID = candidate.ID
				latestAuthErr = curAuthErr
			}
		}
	}

	if latestModelErr != nil {
		return latestModelErr
	}
	return latestAuthErr
}

// latestUnauthorizedCandidateError returns the most recent auth-level error among
// candidates carrying an unauthorized (terminal) upstream authentication failure.
func latestUnauthorizedCandidateError(auths []*Auth) error {
	var latestTime time.Time
	var latestAuthID string
	var latestErr error

	for _, candidate := range auths {
		if candidate == nil || !hasUnauthorizedAuthFailure(candidate) {
			continue
		}
		curTime := candidate.UpdatedAt
		if candidate.LastError != nil {
			if latestErr == nil || curTime.After(latestTime) || (curTime.Equal(latestTime) && candidate.ID > latestAuthID) {
				latestTime = curTime
				latestAuthID = candidate.ID
				latestErr = candidate.LastError
			}
		}
	}
	return latestErr
}

func restoreModelCooldownErrorModel(err error, requestedModel string) error {
	var cooldownErr *modelCooldownError
	if !errors.As(err, &cooldownErr) || cooldownErr == nil {
		return err
	}
	if requestedModel == "" || requestedModel == cooldownErr.model {
		return err
	}
	return newModelCooldownErrorWithCause(requestedModel, cooldownErr.provider, cooldownErr.resetIn, cooldownErr.cause)
}

// availableAuthsForSelector reports the candidates handed to the configured selector.
// For a session-affinity selector the full across-priority set is returned so an
// established binding can be validated instead of being preempted by a recovered
// higher-priority credential; other selectors keep seeing only the highest tier.
func (m *Manager) availableAuthsForSelector(selector Selector, auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	if _, sessionAffinity := selector.(*SessionAffinitySelector); !sessionAffinity {
		return m.availableAuthsForRouteModel(auths, provider, routeModel, now)
	}
	return m.availableAuthsForRouteModelAcrossPriorities(auths, provider, routeModel, now)
}

func selectionArgForSelector(selector Selector, routeModel string) string {
	if isBuiltInSelector(selector) {
		return ""
	}
	return routeModel
}

func (m *Manager) authSupportsRouteModel(registryRef *registry.ModelRegistry, auth *Auth, routeModel string) bool {
	if registryRef == nil || auth == nil {
		return true
	}
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return true
	}
	if registryRef.ClientSupportsModel(auth.ID, routeKey) {
		return true
	}
	selectionKey := m.selectionModelKeyForAuth(auth, routeModel)
	if selectionKey != "" && selectionKey != routeKey && registryRef.ClientSupportsModel(auth.ID, selectionKey) {
		return true
	}
	return m.authAllowsPassthroughModel(auth)
}

// authAllowsPassthroughModel reports whether auth's resolved openai-compatibility
// config entry has Passthrough enabled, letting a model id absent from the
// registry (because Models is empty) still pass eligibility instead of the
// candidate being filtered out as unsupported.
func (m *Manager) authAllowsPassthroughModel(auth *Auth) bool {
	if m == nil || !isOpenAICompatAPIKeyAuth(auth) {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return false
	}
	providerKey, compatName := "", ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	return entry != nil && entry.Passthrough
}

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, opts cliproxyexecutor.Options, executionSeq uint64) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var failed bool
		var usageDetail coreusage.Detail
		var usageEstimated bool
		var hasUsage bool
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := &Error{Message: chunk.Err.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](chunk.Err); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestScoped: isRequestScopedError(chunk.Err), CredentialScope: isCredentialScopedError(chunk.Err), Options: opts, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(chunk.Err)
				action, okAction := matchRequestScopedErrorAction(auth, chunk.Err, m.runtimeConfigSnapshot())
				applyRequestScopedActionToResult(action, okAction, &result)
				m.MarkResult(ctx, result)
			}
			if detail, estimated, ok := kiroUsageResultFromMetadata(provider, chunk.Metadata); ok {
				usageDetail = detail
				usageEstimated = estimated
				hasUsage = true
			}
			if !forward {
				return false
			}
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if !failed {
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: true, Options: opts, ExecutionSeq: executionSeq}
			if hasUsage {
				result.UsageDetail = usageDetail
				result.UsageEstimated = usageEstimated
			}
			m.MarkResult(ctx, result)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel, executionModel string, execModels []string, pooled bool) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	ctx = syncMetadataSessionToContext(ctx, opts.Metadata)
	var lastErr error
	didRefreshOnUnauthorized := false
	for idx, execModel := range execModels {
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		if executionModel != "" {
			execReq.Model = executionModel
		}
		execReq = m.attachResolvedExecutionModelInfo(execReq, auth, routeModel, execModel, executionModel != "")
		executionSeq := m.nextExecutionSeq()
		streamResult, errStream := executor.ExecuteStream(ctx, auth, execReq, opts)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			// Refresh a stale credential once after a 401 and retry the same
			// auth before fallback (upstream conductor_stream.go
			// tryRefreshAfterUnauthorized call site; required by be7323f3bf66).
			if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(ctx, auth, errStream, didRefreshOnUnauthorized); okRefresh {
				auth = refreshed
				didRefreshOnUnauthorized = true
				executionSeq = m.nextExecutionSeq()
				streamResult, errStream = executor.ExecuteStream(ctx, auth, execReq, opts)
				if errStream != nil {
					if errCtx := ctx.Err(); errCtx != nil {
						return nil, errCtx
					}
				}
			}
		}
		if errStream != nil {
			rerr := &Error{Message: errStream.Error()}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errStream); ok && se != nil {
				rerr.HTTPStatus = se.StatusCode()
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestScoped: isRequestScopedError(errStream), CredentialScope: isCredentialScopedError(errStream), Options: opts, ExecutionSeq: executionSeq}
			result.RetryAfter = retryAfterFromError(errStream)
			action, okAction := matchRequestScopedErrorAction(auth, errStream, m.runtimeConfigSnapshot())
			applyRequestScopedActionToResult(action, okAction, &result)
			m.MarkResult(ctx, result)
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(errStream)
				}
				if result.CredentialScope {
					return nil, errStream
				}
				lastErr = errStream
				continue
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			if result.CredentialScope {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCtx
			}
			// A 401 surfacing inside the stream bootstrap means the credential
			// went stale before first payload; refresh once and retry (upstream
			// conductor_stream.go second tryRefreshAfterUnauthorized call site,
			// required by be7323f3bf66).
			if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(ctx, auth, bootstrapErr, didRefreshOnUnauthorized); okRefresh {
				discardStreamChunks(streamResult.Chunks)
				auth = refreshed
				didRefreshOnUnauthorized = true
				executionSeq = m.nextExecutionSeq()
				retryStream, retryErr := executor.ExecuteStream(ctx, auth, execReq, opts)
				if retryErr != nil {
					if errCtx := ctx.Err(); errCtx != nil {
						return nil, errCtx
					}
					bootstrapErr = retryErr
					streamResult = &cliproxyexecutor.StreamResult{}
				} else {
					streamResult = retryStream
					buffered, closed, bootstrapErr = readStreamBootstrap(ctx, streamResult.Chunks)
					if bootstrapErr != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							discardStreamChunks(streamResult.Chunks)
							return nil, errCtx
						}
					}
				}
			}
		}
		if bootstrapErr != nil {
			credentialScope := isCredentialScopedError(bootstrapErr)
			action, okAction := matchRequestScopedErrorAction(auth, bootstrapErr, m.runtimeConfigSnapshot())
			if okAction {
				rerr := &Error{Message: bootstrapErr.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestScoped: isRequestScopedError(bootstrapErr), CredentialScope: credentialScope, Options: opts, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				applyRequestScopedActionToResult(action, okAction, &result)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(bootstrapErr)
				}
				if credentialScope {
					return nil, bootstrapErr
				}
				lastErr = bootstrapErr
				continue
			}
			if isRequestInvalidError(bootstrapErr) {
				rerr := &Error{Message: bootstrapErr.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestScoped: isRequestScopedError(bootstrapErr), CredentialScope: credentialScope, Options: opts, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 && !credentialScope {
				rerr := &Error{Message: bootstrapErr.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestScoped: isRequestScopedError(bootstrapErr), CredentialScope: credentialScope, Options: opts, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			rerr := &Error{Message: bootstrapErr.Error()}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
				rerr.HTTPStatus = se.StatusCode()
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestScoped: isRequestScopedError(bootstrapErr), CredentialScope: credentialScope, ExecutionSeq: executionSeq}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			m.MarkResult(ctx, result)
			discardStreamChunks(streamResult.Chunks)
			return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr, Options: opts, ExecutionSeq: executionSeq}
			m.MarkResult(ctx, result)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, newStreamBootstrapError(emptyErr, streamResult.Headers)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, streamResult.Headers, buffered, remaining, opts, executionSeq), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, lastErr
}

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}

		byAlias := make(map[string]string)
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "gemini":
			if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "claude":
			if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "codex":
			if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "vertex":
			if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "meta":
			// Ported from upstream CLIProxyAPI commit 47cc31ae (conductor_models.go).
			if entry := resolveMetaAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		default:
			// OpenAI-compat uses config selection from auth.Attributes.
			providerKey := ""
			compatName := ""
			if auth.Attributes != nil {
				providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			}
			if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
				if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
					compileAPIKeyModelAliasForModels(byAlias, entry.Models)
				}
			}
		}

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	capabilities := make(apiKeyModelCapabilityTable)
	for _, auth := range m.auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if byRoute := compileAPIKeyModelCapabilitiesForAuth(cfg, auth); len(byRoute) > 0 {
			capabilities[auth.ID] = byRoute
		}
	}

	m.apiKeyModelAlias.Store(out)
	m.apiKeyModelCapabilities.Store(&apiKeyModelCapabilitySnapshot{config: cfg, capabilities: capabilities})
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	var toReschedule []string
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	// Auths whose effective executor key matches the newly registered provider
	// get a pending refresh reschedule so they can run through the new executor
	// (upstream 60e5b8bd432e).
	for id, auth := range m.auths {
		if auth != nil && strings.EqualFold(executorKeyFromAuth(auth), provider) {
			toReschedule = append(toReschedule, id)
		}
	}
	m.mu.Unlock()

	for _, id := range toReschedule {
		m.queueRefreshReschedule(id)
	}

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	releaseMutation, errMutation := m.lockAuthMutationContext(ctx, auth.ID)
	if errMutation != nil {
		return nil, errMutation
	}
	defer releaseMutation()
	now := time.Now()
	if auth.Generation == 0 {
		auth.Generation = 1
	}
	if auth.CreatedAt.IsZero() {
		auth.CreatedAt = now
	}
	auth.UpdatedAt = now
	normalizeModelStates(auth)
	auth.EnsureIndex()
	m.mu.Lock()
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64)
	}
	if existing, exists := m.auths[auth.ID]; exists && existing != nil && existing.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = existing.RegistrationEpoch
	}
	if auth.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = auth.RegistrationEpoch
	}
	m.authEpochs[auth.ID]++
	auth.RegistrationEpoch = m.authEpochs[auth.ID]
	auth.Generation = 1
	auth.ClearFileSnapshot()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	m.queueRefreshReschedule(auth.ID)
	if err := m.persist(ctx, auth); err != nil {
		return auth.Clone(), err
	}
	m.currentHook().OnAuthRegistered(ctx, auth.Clone())
	return auth.Clone(), nil
}

// updateAuthMode selects how updateInternal reconciles an incoming auth with
// the currently registered runtime state.
// Ported from upstream CLIProxyAPI commit 4c1bebe837a6.
type updateAuthMode int

const (
	updateModeReplace updateAuthMode = iota
	updateModeRefresh
	updateModePrepare
)

// UpdatePreparedAuth atomically merges request preparation results into the latest runtime auth
// under the manager lock, preserving concurrent modifications without modifying refresh lifecycle fields.
// Ported from upstream CLIProxyAPI commit 4c1bebe837a6.
func (m *Manager) UpdatePreparedAuth(ctx context.Context, base, updated *Auth) (*Auth, error) {
	return m.updateInternal(ctx, base, updated, updateModePrepare)
}

// UpdateRefreshedAuth atomically merges refresh results into the latest runtime auth
// under the manager lock, preserving concurrent modifications (proxy_url, notes, weights, etc.).
// Ported from upstream CLIProxyAPI commit 4c1bebe837a6.
func (m *Manager) UpdateRefreshedAuth(ctx context.Context, base, updated *Auth) (*Auth, error) {
	return m.updateInternal(ctx, base, updated, updateModeRefresh)
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	return m.updateInternal(ctx, nil, auth, updateModeReplace)
}

func (m *Manager) lockAuthMutationContext(ctx context.Context, id string) (func(), error) {
	if m == nil || id == "" {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	val, _ := m.authMutationLocks.LoadOrStore(id, make(chan struct{}, 1))
	gate := val.(chan struct{})
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() {
			<-gate
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) lockAuthMutation(id string) func() {
	release, _ := m.lockAuthMutationContext(context.Background(), id)
	return release
}

// Modify applies edit to the latest form of id while holding the per-auth
// mutation gate, and commits the result only when the store revision still
// matches expectedRevision. Use expectedRevision == 0 for a mutex-serialized
// read-modify-write with no optimistic check.
//
// It returns (nil, false, nil) when the auth is not present, (auth, false,
// ErrRevisionConflict) when another writer won the race, and (auth, true, nil)
// when the edit committed.
func (m *Manager) Modify(ctx context.Context, id string, expectedRevision int64, edit func(*Auth) error) (*Auth, bool, error) {
	id = strings.TrimSpace(id)
	if m == nil || id == "" || edit == nil {
		return nil, false, nil
	}
	releaseMutation, errMutation := m.lockAuthMutationContext(ctx, id)
	if errMutation != nil {
		return nil, false, errMutation
	}
	defer releaseMutation()

	m.mu.RLock()
	existing, ok := m.auths[id]
	var current *Auth
	if ok && existing != nil {
		current = existing.Clone()
	}
	m.mu.RUnlock()
	if current == nil {
		return nil, false, nil
	}
	if expectedRevision > 0 {
		// Fail before editing when the record already moved, so the loser of a
		// race never publishes its edit into the runtime snapshot.
		storeRevision, errRevision := m.storeRevision(ctx, id)
		switch {
		case errors.Is(errRevision, errStoreNotRevisioned):
			log.WithField("auth", id).Debug("auth store does not expose revisions; serializing without optimistic concurrency")
			expectedRevision = 0
		case errors.Is(errRevision, os.ErrNotExist):
			// The row is gone: drop the stale runtime record so a retry converges
			// on not-found instead of writing the credential back.
			m.Forget(id)
			return nil, false, errRevision
		case errRevision != nil:
			return current, false, errRevision
		case storeRevision != expectedRevision:
			return current, false, fmt.Errorf("modify auth %s: stored revision %d != expected %d: %w", id, storeRevision, expectedRevision, ErrRevisionConflict)
		}
	}
	if errEdit := edit(current); errEdit != nil {
		return nil, false, errEdit
	}
	committedAuth, errUpdate := m.updateInternalLocked(ctx, nil, current, updateModeReplace, expectedRevision)
	if errUpdate != nil {
		return committedAuth, false, errUpdate
	}
	if committedAuth == nil {
		return nil, false, nil
	}
	return committedAuth, true, nil
}

// storeRevision reads the store revision for id, reporting whether the store
// supports optimistic concurrency at all.
func (m *Manager) storeRevision(ctx context.Context, id string) (int64, error) {
	revisioned, ok := m.store.(RevisionedStore)
	if !ok || revisioned == nil {
		return 0, errStoreNotRevisioned
	}
	return revisioned.AuthRevision(ctx, id)
}

var errStoreNotRevisioned = errors.New("auth store does not expose revisions")

func (m *Manager) updateInternal(ctx context.Context, base, auth *Auth, mode updateAuthMode) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	releaseMutation, errMutation := m.lockAuthMutationContext(ctx, auth.ID)
	if errMutation != nil {
		return nil, errMutation
	}
	defer releaseMutation()
	return m.updateInternalLocked(ctx, base, auth, mode, 0)
}

// updateInternalLocked performs the update. Callers must hold the per-auth
// mutation gate for auth.ID.
// credentialsChanged reports whether the authentication credentials (tokens or API
// keys) differ between two Auth records. Ported from upstream CLIProxyAPI
// (conductor_refresh.go, d5816d5) without the outer-token field the local tree
// does not have.
func credentialsChanged(existing, incoming *Auth) bool {
	if existing == nil || incoming == nil {
		return false
	}
	if authAccessToken(existing) != authAccessToken(incoming) || authRefreshToken(existing) != authRefreshToken(incoming) {
		return true
	}
	existingIDToken := authMetadataString(existing, "id_token")
	if existingIDToken == "" {
		existingIDToken = authMetadataString(existing, "idToken")
	}
	incomingIDToken := authMetadataString(incoming, "id_token")
	if incomingIDToken == "" {
		incomingIDToken = authMetadataString(incoming, "idToken")
	}
	if existingIDToken != incomingIDToken {
		return true
	}
	existingKey := ""
	if existing.Attributes != nil {
		existingKey = existing.Attributes[AttributeAPIKey]
	}
	if existingKey == "" {
		existingKey = authMetadataString(existing, "api_key")
	}
	incomingKey := ""
	if incoming.Attributes != nil {
		incomingKey = incoming.Attributes[AttributeAPIKey]
	}
	if incomingKey == "" {
		incomingKey = authMetadataString(incoming, "api_key")
	}
	return existingKey != incomingKey
}

func (m *Manager) updateInternalLocked(ctx context.Context, base, auth *Auth, mode updateAuthMode, expectedRevision int64) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	NormalizeCredentialMetadata(auth.Metadata)
	isFileSnapshot := mode == updateModeReplace && auth.HasFileSnapshot()
	if isFileSnapshot {
		m.mu.RLock()
		existing, ok := m.auths[auth.ID]
		m.mu.RUnlock()
		if !ok || existing == nil {
			return nil, nil
		}
		// Validate file snapshot freshness without holding m.mu across filesystem I/O.
		if !auth.FileSnapshotMatchesDisk() {
			m.mu.RLock()
			current := m.auths[auth.ID].Clone()
			m.mu.RUnlock()
			return current, nil
		}
	}
	m.mu.Lock()
	existing, hasExisting := m.auths[auth.ID]
	if hasExisting && existing == nil {
		hasExisting = false
	}
	// Merge modes reconcile an executor-produced snapshot against the current
	// runtime state; without a registered auth there is nothing to merge into.
	if !hasExisting && mode != updateModeReplace {
		m.mu.Unlock()
		return nil, nil
	}
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64)
	}
	if hasExisting && existing.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = existing.RegistrationEpoch
	}
	if (mode == updateModeRefresh || mode == updateModePrepare) && base != nil && existing.RegistrationEpoch != base.RegistrationEpoch {
		m.mu.Unlock()
		return nil, fmt.Errorf("update auth %s: stale registration epoch %d != %d", auth.ID, base.RegistrationEpoch, existing.RegistrationEpoch)
	}
	if hasExisting && mode == updateModeRefresh {
		if merged := MergeRefreshedAuth(base, existing, auth); merged != nil {
			auth = merged
			NormalizeCredentialMetadata(auth.Metadata)
		}
	} else if hasExisting && mode == updateModePrepare {
		if merged := MergePreparedAuth(base, existing, auth); merged != nil {
			auth = merged
			NormalizeCredentialMetadata(auth.Metadata)
		}
	}
	if auth.RegistrationEpoch != 0 && auth.RegistrationEpoch < m.authEpochs[auth.ID] {
		m.mu.Unlock()
		return nil, fmt.Errorf("update auth %s: stale registration epoch %d < %d", auth.ID, auth.RegistrationEpoch, m.authEpochs[auth.ID])
	}
	if auth.RegistrationEpoch >= m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = auth.RegistrationEpoch
	} else if auth.RegistrationEpoch == 0 {
		auth.RegistrationEpoch = m.authEpochs[auth.ID]
	}
	if hasExisting {
		if !auth.indexAssigned && auth.Index == "" {
			auth.Index = existing.Index
			auth.indexAssigned = existing.indexAssigned
		}
		if isFileSnapshot && auth.CreatedAt.IsZero() {
			auth.CreatedAt = existing.CreatedAt
		}
		auth.Success = existing.Success
		auth.Failed = existing.Failed
		auth.recentRequests = existing.recentRequests
		if auth.Generation <= existing.Generation {
			auth.Generation = existing.Generation + 1
		} else {
			auth.Generation++
		}
		if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
			if isFileSnapshot {
				// A credential file carries configuration, not runtime availability.
				// Keep the live record's runtime state so a reload does not restart a
				// cooldown that requests are already serving out.
				auth.ModelStates = existing.ModelStates
				auth.LastRefreshedAt = existing.LastRefreshedAt
				auth.NextRefreshAfter = existing.NextRefreshAfter
				auth.RefreshFailures = existing.RefreshFailures
				if (auth.Status == "" || auth.Status == StatusActive) && existing.Status != "" {
					auth.Status = existing.Status
				}
				if auth.StatusMessage == "" {
					auth.StatusMessage = existing.StatusMessage
				}
				auth.Unavailable = existing.Unavailable
				auth.NextRetryAfter = existing.NextRetryAfter
				if auth.LastError == nil && existing.LastError != nil {
					auth.LastError = cloneError(existing.LastError)
				}
				observation := mergeQuotaObservation(auth.Quota, existing.Quota)
				auth.Quota = existing.Quota.Clone()
				auth.Quota.ObservedAt = observation.ObservedAt
				auth.Quota.Signals = observation.Signals
				auth.cooldownRecordedSeq = existing.cooldownRecordedSeq
			} else if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
			if existing.Quota.Exceeded && existing.Quota.Reason == "credential_quota" && existing.Quota.NextRecoverAt.After(time.Now()) {
				auth.Unavailable = existing.Unavailable
				auth.NextRetryAfter = existing.NextRetryAfter
				auth.Quota = existing.Quota
				auth.cooldownRecordedSeq = existing.cooldownRecordedSeq
				if auth.Status == StatusActive {
					auth.Status = existing.Status
				}
			}
		}
		// A file snapshot that carries a different credential replaces the one that
		// was rejected: the terminal unauthorized state belonged to the old token, so
		// keeping it would leave a freshly supplied credential unusable. A snapshot of
		// the same rejected credential does not match here and keeps its state.
		if isFileSnapshot && credentialsChanged(existing, auth) &&
			(hasUnauthorizedAuthFailure(existing) || isUnauthorizedError(auth.LastError)) {
			auth.Unavailable = false
			auth.NextRetryAfter = time.Time{}
			auth.cooldownRecordedSeq = 0
			auth.LastError = nil
			auth.StatusMessage = ""
			auth.Status = StatusActive
		}
	}
	auth.UpdatedAt = time.Now()
	normalizeModelStates(auth)
	auth.EnsureIndex()
	// A minted Meta key must reach the configured store before requests can use
	// it. Keep the epoch check, save and installation together so a concurrent
	// reload or removal cannot let an obsolete mint overwrite the credential on
	// disk (upstream 4a0131c062cd, conductor_lifecycle.go).
	persistMetaMint := (mode == updateModePrepare || mode == updateModeRefresh) && strings.EqualFold(strings.TrimSpace(auth.Provider), "meta")
	if persistMetaMint {
		if errPersist := m.persist(ctx, auth); errPersist != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("persist meta auth: %w", errPersist)
		}
	}
	auth.ClearFileSnapshot()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	m.queueRefreshReschedule(auth.ID)
	if !persistMetaMint {
		if err := m.persistWithRevision(ctx, auth, expectedRevision); err != nil {
			if expectedRevision > 0 {
				switch {
				case errors.Is(err, ErrRevisionConflict):
					// The record still exists but moved: keep the pre-edit state.
					m.rollbackAuthUpdate(auth.ID, existing)
				case errors.Is(err, os.ErrNotExist):
					// Another instance deleted the row. The store is authoritative,
					// so drop the record instead of advertising a credential that no
					// longer exists.
					m.Forget(auth.ID)
				}
			}
			return auth.Clone(), err
		}
	}
	m.currentHook().OnAuthUpdated(ctx, auth.Clone())
	return auth.Clone(), nil
}

// rollbackAuthUpdate restores the pre-edit runtime record after a rejected
// optimistic write, so memory never advertises an edit the store refused.
// Callers hold the per-auth mutation gate for id.
func (m *Manager) rollbackAuthUpdate(id string, previous *Auth) {
	m.mu.Lock()
	if previous == nil {
		delete(m.auths, id)
	} else {
		m.auths[id] = previous
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		if previous == nil {
			m.scheduler.removeAuth(id)
		} else {
			m.scheduler.upsertAuth(previous.Clone())
		}
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
}

// Forget removes an auth entry from the in-memory manager without persisting a
// disabled replacement. Storage-backed delete notifications already removed the
// source record before calling this.
func (m *Manager) Forget(id string) bool {
	if m == nil {
		return false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	m.mu.Lock()
	_, ok := m.auths[id]
	if ok {
		delete(m.auths, id)
		delete(m.homeRuntimeAuths, id)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	return true
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		auth.EnsureIndex()
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	req, opts = cliproxysession.Enrich(req, opts)
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if m.HomeEnabled() {
		resp, errHome := m.executeHome(ctx, req, opts, false)
		return resp, unwrapRequestStopError(errHome)
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	roundExcluded := make(map[string]struct{})
	for attempt := 0; ; attempt++ {
		roundAttempted := make(map[string]struct{})
		roundOpts := withAttemptedAuthTracker(opts, roundAttempted)
		resp, errExec, progressed := m.executeMixedOnce(ctx, normalized, req, roundOpts, maxRetryCredentials, roundExcluded)
		if errExec == nil {
			return resp, nil
		}
		if !progressed && attempt > 0 {
			// The pool is exhausted for this request: no fresh credential could be
			// dispatched this round, so further rounds cannot improve the outcome.
			break
		}
		if isRequestStopError(errExec) {
			return cliproxyexecutor.Response{}, unwrapRequestStopError(errExec)
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterErrorWithAttempted(errExec, attempt, normalized, req.Model, maxWait, roundAttempted)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		lastErr = unwrapRequestStopError(lastErr)
		if hasAntigravityProvider(normalized) && shouldAttemptAntigravityCreditsFallback(m, lastErr, normalized) {
			if resp, ok := m.tryAntigravityCreditsExecute(ctx, req, opts); ok {
				return resp, nil
			}
		}
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	req, opts = cliproxysession.Enrich(req, opts)
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if m.HomeEnabled() {
		resp, errHome := m.executeHome(ctx, req, opts, true)
		return resp, unwrapRequestStopError(errHome)
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	roundExcluded := make(map[string]struct{})
	for attempt := 0; ; attempt++ {
		roundAttempted := make(map[string]struct{})
		roundOpts := withAttemptedAuthTracker(opts, roundAttempted)
		resp, errExec, progressed := m.executeCountMixedOnce(ctx, normalized, req, roundOpts, maxRetryCredentials, roundExcluded)
		if errExec == nil {
			return resp, nil
		}
		if !progressed && attempt > 0 {
			break
		}
		if isRequestStopError(errExec) {
			return cliproxyexecutor.Response{}, unwrapRequestStopError(errExec)
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterErrorWithAttempted(errExec, attempt, normalized, req.Model, maxWait, roundAttempted)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, unwrapRequestStopError(lastErr)
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	req, opts = cliproxysession.Enrich(req, opts)
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if m.HomeEnabled() {
		return m.executeHomeStream(ctx, req, opts)
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	roundExcluded := make(map[string]struct{})
	for attempt := 0; ; attempt++ {
		roundAttempted := make(map[string]struct{})
		roundOpts := withAttemptedAuthTracker(opts, roundAttempted)
		result, errStream, progressed := m.executeStreamMixedOnce(ctx, normalized, req, roundOpts, maxRetryCredentials, roundExcluded)
		if errStream == nil {
			return result, nil
		}
		if !progressed && attempt > 0 {
			break
		}
		if isRequestStopError(errStream) {
			return nil, unwrapRequestStopError(errStream)
		}
		lastErr = errStream
		wait, shouldRetry := m.shouldRetryAfterErrorWithAttempted(errStream, attempt, normalized, req.Model, maxWait, roundAttempted)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return nil, errWait
		}
	}
	if lastErr != nil {
		lastErr = unwrapRequestStopError(lastErr)
		if hasAntigravityProvider(normalized) && shouldAttemptAntigravityCreditsFallback(m, lastErr, normalized) {
			if result, ok := m.tryAntigravityCreditsExecuteStream(ctx, req, opts); ok {
				return result, nil
			}
		}
		var bootstrapErr *streamBootstrapError
		if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
			return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
		}
		return nil, lastErr
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
}

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, roundExcluded map[string]struct{}) (cliproxyexecutor.Response, error, bool) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}, false
	}
	routeModel := authSelectionModelFromOptions(opts, req.Model)
	executionModel, restoreExecutionModel := executionModelForAuthSelection(opts, req.Model)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{}, len(roundExcluded))
	for authID := range roundExcluded {
		tried[authID] = struct{}{}
	}
	attempted := make(map[string]struct{})
	progressed := false
	var lastErr error
	for {
		if !homeMode && maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			// Round exhausted its credential budget: carry the tried set into the
			// next round's exclusion metadata so subsequent rounds pick fresh
			// credentials instead of replaying failed ones (R14).
			if roundExcluded != nil {
				for authID := range attempted {
					roundExcluded[authID] = struct{}{}
				}
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr, progressed
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}, progressed
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if maxRetryCredentials > 0 && roundExcluded != nil {
				for authID := range attempted {
					roundExcluded[authID] = struct{}{}
				}
			}
			progressedHere := len(attempted) > 0
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) && lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr, progressedHere
			}
			return cliproxyexecutor.Response{}, errPick, progressedHere
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)

		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		progressed = true
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: &Error{Message: errPrepare.Error()}, Options: opts}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errPrepare); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			m.MarkResult(execCtx, result)
			lastErr = errPrepare
			continue
		}
		execOpts := opts
		if pickOpts.Metadata != nil {
			if canonicalID, ok := pickOpts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey]; ok {
				meta := make(map[string]any, len(execOpts.Metadata)+2)
				for k, v := range execOpts.Metadata {
					meta[k] = v
				}
				meta[cliproxyexecutor.CanonicalSessionIDMetadataKey] = canonicalID
				if parentID, okParent := pickOpts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey]; okParent && parentID != canonicalID {
					meta[cliproxyexecutor.ParentSessionIDMetadataKey] = parentID
				} else {
					delete(meta, cliproxyexecutor.ParentSessionIDMetadataKey)
				}
				execOpts.Metadata = meta
			}
		}
		execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
		var authErr error
		didRefreshOnUnauthorized := false
		for _, upstreamModel := range models {
			resultModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			if restoreExecutionModel {
				execReq.Model = executionModel
			}
			execReq = m.attachResolvedExecutionModelInfo(execReq, auth, routeModel, upstreamModel, restoreExecutionModel)
			executionSeq := m.nextExecutionSeq()
			resp, errExec := executor.Execute(execCtx, auth, execReq, execOpts)
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					return cliproxyexecutor.Response{}, errCtx, progressed
				}
				// A 401 can be a stale credential rather than a dead account —
				// Meta mints a fresh API key from its DCA token on this path.
				// Refresh once and retry the same auth before fallback
				// (upstream conductor_execution.go tryRefreshAfterUnauthorized
				// call site; required by be7323f3bf66).
				if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(execCtx, auth, errExec, didRefreshOnUnauthorized); okRefresh {
					auth = refreshed
					didRefreshOnUnauthorized = true
					executionSeq = m.nextExecutionSeq()
					resp, errExec = executor.Execute(execCtx, auth, execReq, execOpts)
					if errExec != nil {
						if errCtx := execCtx.Err(); errCtx != nil {
							return cliproxyexecutor.Response{}, errCtx, progressed
						}
					}
				}
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil, Options: execOpts, ExecutionSeq: executionSeq}
			if errExec != nil {
				result.Error = resultErrorFromError(errExec)
				result.RequestScoped = isRequestScopedError(errExec)
				result.CredentialScope = isCredentialScopedError(errExec)
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](errExec); ok && se != nil {
					result.Error.HTTPStatus = se.StatusCode()
				}
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				action, okAction := matchRequestScopedErrorAction(auth, errExec, m.runtimeConfigSnapshot())
				applyRequestScopedActionToResult(action, okAction, &result)
				m.MarkResult(execCtx, result)
				if okAction {
					if isRequestScopedStop(action, okAction) {
						return cliproxyexecutor.Response{}, wrapRequestStopError(errExec), progressed
					}
					if result.CredentialScope {
						return cliproxyexecutor.Response{}, errExec, progressed
					}
					authErr = errExec
					continue
				}
				if isRequestInvalidError(errExec) {
					return cliproxyexecutor.Response{}, errExec, progressed
				}
				if result.CredentialScope {
					return cliproxyexecutor.Response{}, errExec, progressed
				}
				authErr = errExec
				continue
			}
			applyKiroUsageResultFromResponse(&result, resp)
			m.MarkResult(execCtx, result)
			return resp, nil, true
		}
		if authErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, authErr, m.runtimeConfigSnapshot())
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return cliproxyexecutor.Response{}, wrapRequestStopError(authErr), progressed
				}
				lastErr = authErr
				if homeMode {
					homeAuthCount++
				}
				continue
			}
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr, progressed
			}
			lastErr = authErr
			if homeMode {
				homeAuthCount++
			}
			continue
		}
	}
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, roundExcluded map[string]struct{}) (cliproxyexecutor.Response, error, bool) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}, false
	}
	routeModel := authSelectionModelFromOptions(opts, req.Model)
	executionModel, restoreExecutionModel := executionModelForAuthSelection(opts, req.Model)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{}, len(roundExcluded))
	for authID := range roundExcluded {
		tried[authID] = struct{}{}
	}
	attempted := make(map[string]struct{})
	progressed := false
	var lastErr error
	for {
		if !homeMode && maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			// Round exhausted its credential budget: carry the tried set into the
			// next round's exclusion metadata so subsequent rounds pick fresh
			// credentials instead of replaying failed ones (R14).
			if roundExcluded != nil {
				for authID := range attempted {
					roundExcluded[authID] = struct{}{}
				}
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr, progressed
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}, progressed
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if maxRetryCredentials > 0 && roundExcluded != nil {
				for authID := range attempted {
					roundExcluded[authID] = struct{}{}
				}
			}
			progressedHere := len(attempted) > 0
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) && lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr, progressedHere
			}
			return cliproxyexecutor.Response{}, errPick, progressedHere
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)

		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		progressed = true
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: &Error{Message: errPrepare.Error()}, Options: opts}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errPrepare); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			m.MarkResult(execCtx, result)
			lastErr = errPrepare
			continue
		}
		execOpts := opts
		if pickOpts.Metadata != nil {
			if canonicalID, ok := pickOpts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey]; ok {
				meta := make(map[string]any, len(execOpts.Metadata)+2)
				for k, v := range execOpts.Metadata {
					meta[k] = v
				}
				meta[cliproxyexecutor.CanonicalSessionIDMetadataKey] = canonicalID
				if parentID, okParent := pickOpts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey]; okParent && parentID != canonicalID {
					meta[cliproxyexecutor.ParentSessionIDMetadataKey] = parentID
				} else {
					delete(meta, cliproxyexecutor.ParentSessionIDMetadataKey)
				}
				execOpts.Metadata = meta
			}
		}
		execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
		var authErr error
		didRefreshOnUnauthorized := false
		for _, upstreamModel := range models {
			resultModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			if restoreExecutionModel {
				execReq.Model = executionModel
			}
			execReq = m.attachResolvedExecutionModelInfo(execReq, auth, routeModel, upstreamModel, restoreExecutionModel)
			executionSeq := m.nextExecutionSeq()
			resp, errExec := executor.CountTokens(execCtx, auth, execReq, execOpts)
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					return cliproxyexecutor.Response{}, errCtx, progressed
				}
				// Refresh a stale credential once after a 401 and retry the
				// same auth before fallback (upstream conductor_execution.go
				// count-path tryRefreshAfterUnauthorized call site).
				if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(execCtx, auth, errExec, didRefreshOnUnauthorized); okRefresh {
					auth = refreshed
					didRefreshOnUnauthorized = true
					executionSeq = m.nextExecutionSeq()
					resp, errExec = executor.CountTokens(execCtx, auth, execReq, execOpts)
					if errExec != nil {
						if errCtx := execCtx.Err(); errCtx != nil {
							return cliproxyexecutor.Response{}, errCtx, progressed
						}
					}
				}
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil, Options: execOpts, ExecutionSeq: executionSeq}
			if errExec != nil {
				result.Error = resultErrorFromError(errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				action, okAction := matchRequestScopedErrorAction(auth, errExec, m.runtimeConfigSnapshot())
				applyRequestScopedActionToResult(action, okAction, &result)
				// Some Anthropic-compatible upstreams do not implement count_tokens
				// and return a generic endpoint 404. Keep failure accounting and hooks
				// without suspending a model that still works for messages requests.
				if isCountTokensEndpointNotFoundError(errExec, execReq.Model) && (result.Error == nil || result.Error.Code != ErrorCodeForceCooldown) {
					m.recordAvailabilityNeutralResult(execCtx, result)
				} else {
					m.MarkResult(execCtx, result)
				}
				if okAction {
					if isRequestScopedStop(action, okAction) {
						return cliproxyexecutor.Response{}, wrapRequestStopError(errExec), progressed
					}
					authErr = errExec
					continue
				}
				if isRequestInvalidError(errExec) {
					return cliproxyexecutor.Response{}, errExec, progressed
				}
				authErr = errExec
				continue
			}
			applyKiroUsageResultFromResponse(&result, resp)
			m.MarkResult(execCtx, result)
			return resp, nil, true
		}
		if authErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, authErr, m.runtimeConfigSnapshot())
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return cliproxyexecutor.Response{}, wrapRequestStopError(authErr), progressed
				}
				lastErr = authErr
				if homeMode {
					homeAuthCount++
				}
				continue
			}
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr, progressed
			}
			lastErr = authErr
			if homeMode {
				homeAuthCount++
			}
			continue
		}
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, roundExcluded map[string]struct{}) (*cliproxyexecutor.StreamResult, error, bool) {
	if len(providers) == 0 {
		return nil, nil, false
	}
	routeModel := authSelectionModelFromOptions(opts, req.Model)
	executionModel, _ := executionModelForAuthSelection(opts, req.Model)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{}, len(roundExcluded))
	for authID := range roundExcluded {
		tried[authID] = struct{}{}
	}
	attempted := make(map[string]struct{})
	progressed := false
	var lastErr error
	for {
		if !homeMode && maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			// Round exhausted its credential budget: carry the tried set into the
			// next round's exclusion metadata so subsequent rounds pick fresh
			// credentials instead of replaying failed ones (R14).
			if roundExcluded != nil {
				for authID := range attempted {
					roundExcluded[authID] = struct{}{}
				}
			}
			if lastErr != nil {
				return nil, lastErr, progressed
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}, progressed
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if maxRetryCredentials > 0 && roundExcluded != nil {
				for authID := range attempted {
					roundExcluded[authID] = struct{}{}
				}
			}
			progressedHere := len(attempted) > 0
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) && lastErr != nil {
				return nil, lastErr, progressedHere
			}
			return nil, errPick, progressedHere
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		progressed = true
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: &Error{Message: errPrepare.Error()}, Options: opts}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errPrepare); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			m.MarkResult(execCtx, result)
			lastErr = errPrepare
			continue
		}
		execOpts := opts
		if pickOpts.Metadata != nil {
			if canonicalID, ok := pickOpts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey]; ok {
				meta := make(map[string]any, len(execOpts.Metadata)+2)
				for k, v := range execOpts.Metadata {
					meta[k] = v
				}
				meta[cliproxyexecutor.CanonicalSessionIDMetadataKey] = canonicalID
				if parentID, okParent := pickOpts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey]; okParent && parentID != canonicalID {
					meta[cliproxyexecutor.ParentSessionIDMetadataKey] = parentID
				} else {
					delete(meta, cliproxyexecutor.ParentSessionIDMetadataKey)
				}
				execOpts.Metadata = meta
			}
		}
		execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
		streamResult, errStream := m.executeStreamWithModelPool(execCtx, executor, auth, provider, req, execOpts, routeModel, executionModel, models, pooled)
		if errStream != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return nil, errCtx, progressed
			}
			action, okAction := matchRequestScopedErrorAction(auth, errStream, m.runtimeConfigSnapshot())
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(errStream), progressed
				}
				lastErr = errStream
				if homeMode {
					homeAuthCount++
				}
				continue
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream, progressed
			}
			lastErr = errStream
			if homeMode {
				homeAuthCount++
			}
			continue
		}
		return streamResult, nil, true
	}
}

// withAttemptedAuthTracker records every credential ID selected during a retry
// round into attempted, chaining any previously installed selection callback.
func withAttemptedAuthTracker(opts cliproxyexecutor.Options, attempted map[string]struct{}) cliproxyexecutor.Options {
	if attempted == nil {
		return opts
	}
	meta := cloneRequestMetadata(opts.Metadata)
	prevCallback, _ := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string))
	meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey] = func(authID string) {
		if strings.TrimSpace(authID) != "" {
			attempted[authID] = struct{}{}
		}
		if prevCallback != nil {
			prevCallback(authID)
		}
	}
	opts.Metadata = meta
	return opts
}

func cloneRequestMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return make(map[string]any, 4)
	}
	dst := make(map[string]any, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	opts.Metadata = cloneRequestMetadata(opts.Metadata)
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	return opts
}

func withHomeAuthCount(opts cliproxyexecutor.Options, count int) cliproxyexecutor.Options {
	if count <= 0 {
		count = 1
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[homeAuthCountMetadataKey] = count
	opts.Metadata = meta
	return opts
}

func homeAuthCountFromMetadata(meta map[string]any) int {
	if len(meta) == 0 {
		return 1
	}
	switch value := meta[homeAuthCountMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	}
	return 1
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

type requestAuthPrepareLock struct {
	mu sync.Mutex
}

func (m *Manager) prepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	preparer, ok := executor.(RequestAuthPreparer)
	if !ok {
		return auth, nil
	}

	return m.PrepareRequestAuth(ctx, preparer, auth)
}

// PrepareRequestAuth prepares a registered credential using the same serialization
// and lifecycle checks as normal request execution. Management tools use this path too.
// Ported from upstream CLIProxyAPI commit 4a0131c062cd (conductor_execution.go).
func (m *Manager) PrepareRequestAuth(ctx context.Context, preparer RequestAuthPreparer, auth *Auth) (*Auth, error) {
	if m == nil || preparer == nil || auth == nil || !preparer.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}

	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	isMeta := strings.EqualFold(strings.TrimSpace(auth.Provider), "meta")
	var prepareMu *sync.Mutex
	if isMeta {
		// Meta also mints on 401 recovery; serialize both paths per credential
		// (upstream 4a0131c0 uses refreshLocks for meta instead of
		// requestPrepareLocks). refreshLocks only ever stores *authRefreshLock.
		lockValue, _ := m.refreshLocks.LoadOrStore(id, &authRefreshLock{})
		prepareMu = &lockValue.(*authRefreshLock).mu
	} else {
		lockValue, _ := m.requestPrepareLocks.LoadOrStore(id, &requestAuthPrepareLock{})
		lock, ok := lockValue.(*requestAuthPrepareLock)
		if !ok || lock == nil {
			return preparer.PrepareRequestAuth(ctx, auth.Clone())
		}
		prepareMu = &lock.mu
	}

	prepareMu.Lock()
	defer prepareMu.Unlock()

	target := auth.Clone()
	m.mu.RLock()
	current := m.auths[id]
	if current != nil {
		target = current.Clone()
	}
	m.mu.RUnlock()
	if current == nil && isMeta {
		return nil, fmt.Errorf("prepare meta auth: credential no longer registered")
	}

	if !preparer.ShouldPrepareRequestAuth(target) {
		return target, nil
	}

	// The preparer mutates a clone of the base snapshot; the result is merged
	// back into the latest runtime state so concurrent user/runtime changes are
	// preserved (upstream 4c1bebe837a6).
	base := target.Clone()
	updated, errPrepare := preparer.PrepareRequestAuth(ctx, base.Clone())
	if errPrepare != nil {
		return auth, errPrepare
	}
	if updated == nil {
		return target, nil
	}

	saved, errUpdate := m.UpdatePreparedAuth(ctx, base, updated)
	if errUpdate != nil {
		return nil, errUpdate
	}
	if saved != nil {
		return saved, nil
	}
	if isMeta {
		return nil, fmt.Errorf("prepare meta auth: credential removed during mint")
	}
	return target, nil
}

func contextWithRequestedModelAlias(ctx context.Context, opts cliproxyexecutor.Options, fallback string) context.Context {
	alias := requestedModelAliasFromOptions(opts, fallback)
	ctx = coreusage.WithRequestedModelAlias(ctx, alias)
	if effort := reasoningEffortFromOptions(opts); effort != "" {
		ctx = coreusage.WithReasoningEffort(ctx, effort)
	}
	return ctx
}

func requestedModelAliasFromOptions(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		return strings.TrimSpace(value)
	case []byte:
		if len(value) == 0 {
			return fallback
		}
		return strings.TrimSpace(string(value))
	default:
		return fallback
	}
}

func reasoningEffortFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ReasoningEffortMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func disallowFreeAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.DisallowFreeAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(val))
		return err == nil && parsed
	case []byte:
		parsed, err := strconv.ParseBool(strings.TrimSpace(string(val)))
		return err == nil && parsed
	default:
		return false
	}
}

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return resolved
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	case "meta":
		// Ported from upstream CLIProxyAPI commit 47cc31ae (conductor_models.go).
		upstreamModel = resolveUpstreamModelForMetaAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return upstreamModel
	}
	return requestedModel
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
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
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

// resolveMetaAPIKeyConfig returns the meta-api-key config entry matching the
// auth credential. Ported from upstream CLIProxyAPI commit 47cc31ae.
func resolveMetaAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.MetaKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.MetaKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveUpstreamModelForMetaAPIKey resolves the configured upstream model for a
// meta-api-key credential. Ported from upstream CLIProxyAPI commit 47cc31ae.
func resolveUpstreamModelForMetaAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveMetaAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(cfg *internalconfig.Config, providerKey, compatName, authProvider string) *internalconfig.OpenAICompatibility {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

func (m *Manager) retrySettings() (int, int, time.Duration) {
	if m == nil {
		return 0, 0, 0
	}
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load())
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	return m.closestCooldownWaitWithAttempted(providers, model, attempt, 0, nil)
}

// closestCooldownWaitWithAttempted returns the smallest positive cooldown wait
// among retry-eligible credentials. Credentials already attempted during a round
// that failed with 429 must not drive an immediate zero-wait retry round: while
// cooling is enabled they contribute at least minQuotaCooldownFloor even when
// their recorded cooldown has already expired or was never set.
func (m *Manager) closestCooldownWaitWithAttempted(providers []string, model string, attempt int, status int, attempted map[string]struct{}) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := canonicalSchedulingProvider(providers[i])
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := canonicalSchedulingProvider(auth.Provider)
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		blocked, reason, next := isAuthBlockedForModel(auth, checkModel, now)

		wasAttempted := false
		if len(attempted) > 0 {
			_, wasAttempted = attempted[auth.ID]
		}
		if wasAttempted && status == http.StatusTooManyRequests && !m.cooldownDisabledForAuth(auth) {
			// Skip only hard blocks without a retry deadline (disabled or
			// terminal); every other attempted credential enforces the floor.
			if blocked && (reason == blockReasonDisabled || next.IsZero()) {
				continue
			}
			wait := minQuotaCooldownFloor
			if blocked {
				if remaining := next.Sub(now); remaining > wait {
					wait = remaining
				}
			}
			if !found || wait < minWait {
				minWait = wait
				found = true
			}
			continue
		}
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) retryAllowed(attempt int, providers []string) bool {
	if m == nil || attempt < 0 || len(providers) == 0 {
		return false
	}
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := canonicalSchedulingProvider(providers[i])
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := canonicalSchedulingProvider(auth.Provider)
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt < effectiveRetry {
			return true
		}
	}
	return false
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	return m.shouldRetryAfterErrorWithAttempted(err, attempt, providers, model, maxWait, nil)
}

// shouldRetryAfterErrorWithAttempted decides whether another retry round should
// run after err, and how long to wait first. attempted carries the credential
// IDs selected during the round that just failed so a 429 cannot bounce straight
// back onto them with a zero-wait retry.
func (m *Manager) shouldRetryAfterErrorWithAttempted(err error, attempt int, providers []string, model string, maxWait time.Duration, attempted map[string]struct{}) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	status := statusCodeFromError(err)
	if status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) || isRequestStopError(err) {
		return 0, false
	}
	wait, found := m.closestCooldownWaitWithAttempted(providers, model, attempt, status, attempted)
	if found {
		if wait > maxWait {
			return 0, false
		}
		return wait, true
	}
	if isTransientTransportError(err) {
		// Pre-HTTP transport failures retry without cooling the credential
		// (upstream bef1f65c6c1d). Honor a provider retry hint when present;
		// otherwise retry immediately since nothing is cooling.
		if !m.retryAllowed(attempt, providers) {
			return 0, false
		}
		if retryAfter := retryAfterFromError(err); retryAfter != nil {
			if *retryAfter < 0 || *retryAfter > maxWait {
				return 0, false
			}
			if *retryAfter > 0 {
				return *retryAfter, true
			}
		}
		return 0, true
	}
	if disp, cooldown, ok := Classify(status, strings.ToLower(err.Error())); ok {
		switch disp {
		case DispositionReturn:
			return 0, false
		case DispositionCooldown:
			if !m.retryAllowed(attempt, providers) {
				return 0, false
			}
			if cooldown > maxWait {
				return 0, false
			}
			return cooldown, true
		case DispositionRetryBackoff:
			if !m.retryAllowed(attempt, providers) {
				return 0, false
			}
			backoffWait := retryBackoffWait(err, attempt)
			if backoffWait <= 0 || backoffWait > maxWait {
				return 0, false
			}
			return backoffWait, true
		}
	}
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if !m.retryAllowed(attempt, providers) {
		return 0, false
	}
	retryAfter := retryAfterFromError(err)
	if retryAfter == nil || *retryAfter <= 0 || *retryAfter > maxWait {
		return 0, false
	}
	return *retryAfter, true
}

// retryBackoffWait computes the wait for a DispositionRetryBackoff error. It
// prefers a provider-reported retry-after duration, clamped at quotaBackoffMax
// so a far-future provider reset (Codex reports 5-6h) never parks a credential
// for the day. Without a provider-reported duration it falls back to the
// existing exponential backoff formula, keyed by the request's retry attempt
// since shouldRetryAfterError has no auth in scope to carry a persisted level.
func retryBackoffWait(err error, attempt int) time.Duration {
	if provided := retryAfterFromError(err); provided != nil && *provided > 0 {
		wait := *provided
		if wait > quotaBackoffMax {
			wait = quotaBackoffMax
		}
		return wait
	}
	level := attempt
	if level < 0 {
		level = 0
	}
	wait, _ := nextQuotaCooldown(level, false)
	return wait
}

// cooldownWaitJitterCap bounds the random jitter added to cooldown waits so a
// long wait is never extended by more than this amount.
const cooldownWaitJitterCap = 2 * time.Second

func jitteredCooldownWait(base, maxRetryInterval time.Duration) time.Duration {
	if maxRetryInterval <= 0 {
		return 0
	}
	clamped := base
	if clamped < 0 {
		clamped = 0
	}
	if clamped > maxRetryInterval {
		clamped = maxRetryInterval
	}
	jitterCap := clamped / 4
	if jitterCap > cooldownWaitJitterCap {
		jitterCap = cooldownWaitJitterCap
	}
	if remaining := maxRetryInterval - clamped; jitterCap > remaining {
		jitterCap = remaining
	}
	if jitterCap <= 0 {
		return clamped
	}
	return clamped + rand.N(jitterCap+1)
}

func waitForCooldown(ctx context.Context, wait, maxRetryInterval time.Duration) error {
	wait = jitteredCooldownWait(wait, maxRetryInterval)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	modelKey := canonicalModelKey(result.Model)
	if !result.Success && !shouldSkipCredentialCooldown(result.Error) {
		// Warn-level credential diagnostic identifying the failing credential
		// without leaking secret material; cancellations are filtered inside.
		m.mu.RLock()
		failedAuth := m.auths[result.AuthID]
		m.mu.RUnlock()
		warnLogUpstreamFailure(ctx, nil, result.Provider, result.Model, failedAuth, 0, result.Error)
	}
	if !result.Success && (result.RequestScoped || shouldSkipCredentialCooldown(result.Error)) {
		m.currentHook().OnResult(ctx, result)
		return
	}

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	var authSnapshot *Auth

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}
		// retainModelCooldown pins a still-live model cooldown against a success
		// from an execution dispatched before that cooldown was recorded. Such a
		// success predates the failure and proves nothing about recovery, so the
		// state transition it normally causes is skipped entirely.
		retainModelCooldown := modelKey != "" && result.ExecutionSeq > 0
		if retainModelCooldown {
			if existing := existingModelState(auth, modelKey); existing != nil && isModelStateActiveCooldown(existing, now) &&
				existing.cooldownRecordedSeq > 0 && result.ExecutionSeq <= existing.cooldownRecordedSeq {
				// Retain active model cooldown
			} else {
				retainModelCooldown = false
			}
		}
		retainAuthCooldown := modelKey == "" && result.ExecutionSeq > 0 &&
			isAuthActiveCooldown(auth, now) && auth.cooldownRecordedSeq > 0 && result.ExecutionSeq <= auth.cooldownRecordedSeq

		if result.Success {
			if auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
				// Retain active credential-scoped cooldown
			} else if retainModelCooldown || retainAuthCooldown {
				// Retain a cooldown recorded after this execution was dispatched.
			} else if modelKey != "" {
				state := ensureModelState(auth, modelKey)
				resetModelState(state, now)
				updateAggregatedAvailability(auth, now)
				if !hasModelError(auth, now) {
					auth.LastError = nil
					auth.StatusMessage = ""
					auth.Status = StatusActive
				}
				auth.UpdatedAt = now
				shouldResumeModel = true
				clearModelQuota = true
			} else {
				clearAuthStateOnSuccess(auth, now)
			}
		} else {
			if modelKey != "" {
				if !result.RequestScoped && !shouldSkipCredentialCooldown(result.Error) {
					disableCooling := m.cooldownDisabledForAuth(auth)
					if result.Error != nil && result.Error.Code == ErrorCodeForceCooldown {
						disableCooling = false
					}
					state := ensureModelState(auth, modelKey)
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					prevModelRetryAfter := state.NextRetryAfter
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
						auth.LastError = cloneError(result.Error)
						auth.StatusMessage = result.Error.Message
					}

					statusCode := statusCodeFromResult(result.Error)
					if isModelSupportResultError(result.Error) {
						if disableCooling {
							state.NextRetryAfter = time.Time{}
						} else if result.RetryAfter != nil && *result.RetryAfter > 0 {
							state.NextRetryAfter = now.Add(*result.RetryAfter)
						} else {
							next := now.Add(12 * time.Hour)
							state.NextRetryAfter = next
							suspendReason = "model_not_supported"
							shouldSuspendModel = true
						}
					} else if isInvalidGrantResultError(result.Error) {
						if disableCooling {
							state.NextRetryAfter = time.Time{}
						} else {
							state.NextRetryAfter = now.Add(30 * time.Minute)
							suspendReason = "invalid_grant"
							shouldSuspendModel = true
						}
					} else {
						switch statusCode {
						case 401:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "unauthorized"
								shouldSuspendModel = true
							}
						case 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "payment_required"
								shouldSuspendModel = true
							}
						case 404:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else if result.RetryAfter != nil && *result.RetryAfter > 0 {
								state.NextRetryAfter = now.Add(*result.RetryAfter)
							} else {
								next := now.Add(12 * time.Hour)
								state.NextRetryAfter = next
								suspendReason = "not_found"
								shouldSuspendModel = true
							}
						case 429:
							var next time.Time
							var credentialNext time.Time
							backoffLevel := state.Quota.BackoffLevel
							if result.CredentialScope {
								backoffLevel = 0
								if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" {
									backoffLevel = auth.Quota.BackoffLevel
								}
							}
							if !disableCooling {
								if result.RetryAfter != nil {
									cooldown := *result.RetryAfter
									if cooldown < minQuotaCooldownFloor {
										cooldown = minQuotaCooldownFloor
									}
									next = now.Add(cooldown).Round(0)
								} else {
									quotaForFailure := state.Quota
									if result.CredentialScope {
										if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" {
											quotaForFailure = auth.Quota
										} else {
											quotaForFailure.NextRecoverAt = time.Time{}
											quotaForFailure.BackoffLevel = 0
										}
									}
									next, backoffLevel = quotaCooldownAfterFailure(quotaForFailure, disableCooling, now)
								}
								credentialNext = next
								if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(next) {
									next = state.Quota.NextRecoverAt
								}
							}
							state.NextRetryAfter = next
							applyCooldownFields(&state.Quota, QuotaState{
								Exceeded:      true,
								Reason:        "quota",
								NextRecoverAt: next,
								BackoffLevel:  backoffLevel,
							})
							if result.CredentialScope && !disableCooling {
								// A credential-scoped quota failure (e.g. Codex
								// usage_limit_reached with model-level cooling off)
								// propagates a credential_quota cooldown to every
								// sibling model state and the auth aggregate
								// (upstream b064b832e242 + 1c22598d0b5a).
								for _, otherState := range auth.ModelStates {
									if otherState != nil && otherState != state {
										otherState.Unavailable = true
										otherState.Status = StatusError
										otherQuotaNext := credentialNext
										if otherState.Quota.Exceeded && otherState.Quota.NextRecoverAt.After(otherQuotaNext) {
											otherQuotaNext = otherState.Quota.NextRecoverAt
										}
										otherRetryAfter := otherQuotaNext
										// Propagation only extends a sibling's still-live
										// per-model deadline; it never shortens one, and a
										// longer non-quota deadline is not promoted into a
										// quota recovery time.
										if !otherState.NextRetryAfter.IsZero() && otherState.NextRetryAfter.After(otherRetryAfter) {
											otherRetryAfter = otherState.NextRetryAfter
										}
										otherState.NextRetryAfter = otherRetryAfter
										applyCooldownFields(&otherState.Quota, QuotaState{
											Exceeded:      true,
											Reason:        "credential_quota",
											NextRecoverAt: otherQuotaNext,
											BackoffLevel:  backoffLevel,
										})
									}
								}
								auth.Unavailable = true
								authNext := credentialNext
								if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" &&
									auth.Quota.NextRecoverAt.After(authNext) {
									authNext = auth.Quota.NextRecoverAt
								}
								auth.Quota.Exceeded = true
								auth.Quota.Reason = "credential_quota"
								auth.Quota.NextRecoverAt = authNext
								auth.Quota.BackoffLevel = backoffLevel
								auth.NextRetryAfter = authNext
							}
							if !disableCooling {
								suspendReason = "quota"
								shouldSuspendModel = true
								setModelQuota = true
							}
						case 408, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526:
							state.NextRetryAfter = recoverableFailureRetryAfterWithHint(now, result.RetryAfter, disableCooling)
						default:
							state.NextRetryAfter = time.Time{}
						}
					}

					if result.Error != nil && result.Error.Code == ErrorCodeForceCooldown && state.NextRetryAfter.IsZero() {
						state.NextRetryAfter = now.Add(time.Minute)
					}

					// A later failure only extends a still-live model cooldown; it
					// never shortens one. A deliberate zero write (disableCooling)
					// still clears the deadline (upstream 1c22598d0b5a).
					if !state.NextRetryAfter.IsZero() && prevModelRetryAfter.After(state.NextRetryAfter) && prevModelRetryAfter.After(now) {
						state.NextRetryAfter = prevModelRetryAfter
					}
					// Capture the dispatch watermark of the cooldown just recorded.
					// Credential-scoped cooldowns were propagated to the auth and
					// every sibling state inside the failure branch; their watermarks
					// are stamped here so a sibling's later, longer model-scoped
					// cooldown can re-stamp only its own state.
					cooldownSeq := m.currentExecutionSeq()
					state.cooldownRecordedSeq = cooldownSeq
					if result.CredentialScope && !disableCooling && statusCodeFromResult(result.Error) == 429 {
						auth.cooldownRecordedSeq = cooldownSeq
						for _, otherState := range auth.ModelStates {
							if otherState != nil && otherState != state {
								otherState.cooldownRecordedSeq = cooldownSeq
							}
						}
					}
					auth.Status = StatusError
					auth.UpdatedAt = now
					updateAggregatedAvailability(auth, now)
				}
			} else {
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now, m.cooldownDisabledForAuth(auth))
				auth.cooldownRecordedSeq = m.currentExecutionSeq()
			}
		}

		applyKiroRuntimeUsageStats(auth, result, now)
		if !result.SkipQuotaObservation {
			responseHeaders := logging.GetResponseHeaders(ctx)
			auth.Quota.ObserveResponseHeadersForProvider(result.Provider, responseHeaders, now)
			if modelKey != "" {
				if ms, ok := auth.ModelStates[modelKey]; ok && ms != nil {
					ms.Quota.ObserveResponseHeadersForProvider(result.Provider, responseHeaders, now)
				}
			}
		}
		_ = m.persist(ctx, auth)
		authSnapshot = auth.Clone()
	}
	m.mu.Unlock()
	if m.scheduler != nil && authSnapshot != nil {
		// upsertAuth re-evaluates the whole auth into every provider shard,
		// which already satisfies upstream upsertAuthResult's credential-scope
		// re-evaluation (the targetModels arg there is only an optimization).
		m.scheduler.upsertAuth(authSnapshot)
	}

	if clearModelQuota && modelKey != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, modelKey)
	}
	if setModelQuota && modelKey != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, modelKey)
	}
	if shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, modelKey)
	} else if shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, modelKey, suspendReason)
	}

	m.persistCooldownStateSnapshot(ctx)
	m.currentHook().OnResult(ctx, result)
	m.updateSessionAffinity(result)
}

func (m *Manager) updateSessionAffinity(result Result) {
	if m == nil || m.selector == nil {
		return
	}
	if affinity, ok := m.selector.(interface {
		OnResult(Result)
	}); ok && affinity != nil {
		affinity.OnResult(result)
	}
}

func (m *Manager) recordAvailabilityNeutralResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}
		_ = m.persist(ctx, auth)
	}
	m.mu.Unlock()

	m.currentHook().OnResult(ctx, result)
}

func applyKiroUsageResultFromResponse(result *Result, resp cliproxyexecutor.Response) {
	if result == nil {
		return
	}
	detail, estimated, ok := kiroUsageResultFromMetadata(result.Provider, resp.Metadata)
	if !ok {
		return
	}
	result.UsageDetail = detail
	result.UsageEstimated = estimated
}

func kiroUsageResultFromMetadata(provider string, metadata map[string]any) (coreusage.Detail, bool, bool) {
	if !strings.EqualFold(strings.TrimSpace(provider), "kiro") || len(metadata) == 0 {
		return coreusage.Detail{}, false, false
	}
	detailRaw, ok := metadata["kiro_usage_detail"]
	if !ok || detailRaw == nil {
		return coreusage.Detail{}, false, false
	}
	detailMap, ok := detailRaw.(map[string]any)
	if !ok {
		raw, err := json.Marshal(detailRaw)
		if err != nil {
			return coreusage.Detail{}, false, false
		}
		if err := json.Unmarshal(raw, &detailMap); err != nil {
			return coreusage.Detail{}, false, false
		}
	}
	detail := coreusage.Detail{
		InputTokens:     int64FromMetadata(firstAuthQuotaValue(detailMap, "prompt_tokens", "input_tokens")),
		OutputTokens:    int64FromMetadata(firstAuthQuotaValue(detailMap, "completion_tokens", "output_tokens")),
		ReasoningTokens: int64FromMetadata(firstAuthQuotaValue(detailMap, "completion_tokens_details", "output_tokens_details")),
		TotalTokens:     int64FromMetadata(firstAuthQuotaValue(detailMap, "total_tokens")),
	}
	if nested, ok := firstAuthQuotaValue(detailMap, "completion_tokens_details", "output_tokens_details").(map[string]any); ok {
		detail.ReasoningTokens = int64FromMetadata(firstAuthQuotaValue(nested, "reasoning_tokens"))
	}
	if nested, ok := firstAuthQuotaValue(detailMap, "prompt_tokens_details", "input_tokens_details").(map[string]any); ok {
		detail.CachedTokens = int64FromMetadata(firstAuthQuotaValue(nested, "cached_tokens"))
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	if !usageDetailNonZero(detail) {
		return coreusage.Detail{}, false, false
	}
	estimated := boolFromMetadata(metadata["kiro_usage_estimated"])
	return detail, estimated, true
}

func applyKiroRuntimeUsageStats(auth *Auth, result Result, now time.Time) {
	if auth == nil || !result.Success || !strings.EqualFold(strings.TrimSpace(result.Provider), "kiro") {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	stats := map[string]any{}
	if existing, ok := auth.Metadata["kiro_usage_stats"]; ok && existing != nil {
		if existingMap, ok := existing.(map[string]any); ok {
			for key, value := range existingMap {
				stats[key] = value
			}
		} else if raw, err := json.Marshal(existing); err == nil {
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err == nil {
				for key, value := range decoded {
					stats[key] = value
				}
			}
		}
	}
	detail := result.UsageDetail
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	stats["requests"] = int64FromMetadata(stats["requests"]) + 1
	stats["prompt_tokens"] = int64FromMetadata(stats["prompt_tokens"]) + detail.InputTokens
	stats["completion_tokens"] = int64FromMetadata(stats["completion_tokens"]) + detail.OutputTokens
	stats["total_tokens"] = int64FromMetadata(stats["total_tokens"]) + detail.TotalTokens
	if result.UsageEstimated && detail.TotalTokens > 0 {
		stats["estimated_tokens"] = int64FromMetadata(stats["estimated_tokens"]) + detail.TotalTokens
	}
	if model := strings.TrimSpace(result.Model); model != "" {
		stats["last_model"] = model
	}
	if now.IsZero() {
		now = time.Now()
	}
	stats["last_used_at"] = now.UTC()
	stats["updated_at"] = now.UTC()
	auth.Metadata["kiro_usage_stats"] = stats
}

func int64FromMetadata(value any) int64 {
	if value == nil {
		return 0
	}
	if number, ok := authQuotaNumber(value); ok {
		return int64(number)
	}
	return 0
}

func boolFromMetadata(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "on":
			return true
		default:
			return false
		}
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	default:
		return false
	}
}

func usageDetailNonZero(detail coreusage.Detail) bool {
	return detail.InputTokens != 0 ||
		detail.OutputTokens != 0 ||
		detail.ReasoningTokens != 0 ||
		detail.CachedTokens != 0 ||
		detail.TotalTokens != 0
}

func ensureModelState(auth *Auth, model string) *ModelState {
	model = canonicalModelKey(model)
	if auth == nil || model == "" {
		return nil
	}
	normalizeModelStates(auth)
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

// existingModelState returns the state recorded for the canonical model key,
// or nil when the auth tracks no state for it.
// Ported from upstream CLIProxyAPI (conductor_cooldown.go).
func existingModelState(auth *Auth, model string) *ModelState {
	model = canonicalModelKey(model)
	if auth == nil || model == "" {
		return nil
	}
	return auth.ModelStates[model]
}

// isModelStateActiveCooldown reports whether the model state carries a still-live cooldown.
func isModelStateActiveCooldown(state *ModelState, now time.Time) bool {
	if state == nil {
		return false
	}
	if state.Status == StatusDisabled {
		return true
	}
	if !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
		return true
	}
	if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
		return true
	}
	if state.Quota.Exceeded && state.Quota.NextRecoverAt.IsZero() {
		return true
	}
	return false
}

// isAuthActiveCooldown reports whether the auth carries a still-live
// auth-level cooldown recorded without a model key.
func isAuthActiveCooldown(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return true
	}
	if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
		return true
	}
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.IsZero() {
		return true
	}
	return false
}

// clearUnauthorizedModelStates resets per-model states whose last failure was
// unauthorized after a successful credential refresh, so refreshed credentials
// regain access to models suspended only by the stale token.
// Ported from upstream CLIProxyAPI (conductor_refresh.go).
func clearUnauthorizedModelStates(auth *Auth, now time.Time) []string {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	var resumed []string
	for model, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		isUnauth := false
		if state.LastError != nil {
			if state.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(state.LastError.Code, "unauthorized") || isUnauthorizedError(state.LastError) {
				isUnauth = true
			}
		}
		if !isUnauth && strings.Contains(strings.ToLower(state.StatusMessage), "unauthorized") {
			isUnauth = true
		}
		if !isUnauth {
			continue
		}
		resetModelState(state, now)
		resumed = append(resumed, model)
	}
	if len(resumed) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	return resumed
}

// clientModelProjectionForAuth computes the registry-side availability projection
// for one route model from the auth's runtime state.
// Ported from upstream CLIProxyAPI (conductor_models.go).
func (m *Manager) clientModelProjectionForAuth(auth *Auth, routeModel string, now time.Time) registry.ClientModelProjection {
	targetModel := strings.TrimSpace(routeModel)
	if targetModel == "" {
		return registry.ClientModelProjection{}
	}
	if auth == nil {
		return registry.ClientModelProjection{ModelID: targetModel}
	}

	targetKey := ""
	if m != nil {
		targetKey = m.selectionModelKeyForAuth(auth, targetModel)
	}
	if targetKey == "" {
		targetKey = canonicalModelKey(targetModel)
	}

	state := existingModelState(auth, targetKey)
	isSuspended := auth.Disabled || auth.Status == StatusDisabled
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		isSuspended = true
	}
	isQuotaExceeded := false
	var suspendReason string
	if state != nil {
		if state.Status == StatusDisabled || state.Unavailable || (!state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now)) {
			isSuspended = true
		}
		if state.Quota.Exceeded && (state.Quota.NextRecoverAt.IsZero() || state.Quota.NextRecoverAt.After(now)) {
			isQuotaExceeded = true
		}
		if isSuspended {
			suspendReason = cooldownReason(state.StatusMessage, state.Quota, state.LastError)
		}
	}
	if len(auth.ModelStates) == 0 && auth.Unavailable && auth.NextRetryAfter.After(now) {
		// With no per-model states, scheduling falls back to the credential-wide
		// cooldown (isAuthBlockedForModel); the projection must agree with it.
		// When states exist, unmatched models stay schedulable by design, so the
		// credential-wide fields must not suspend them here either.
		isSuspended = true
	}
	if isSuspended && suspendReason == "" {
		suspendReason = cooldownReason(auth.StatusMessage, auth.Quota, auth.LastError)
	}

	return registry.ClientModelProjection{
		ModelID:       targetModel,
		Suspended:     isSuspended,
		SuspendReason: suspendReason,
		QuotaExceeded: isQuotaExceeded,
	}
}

// normalizeModelStates folds any non-canonical (thinking-suffix) model state keys into
// their canonical form so cooldown and scheduler sharing treat all variant suffixes of
// the same model as one state. Returns true when the map was rewritten.
func normalizeModelStates(auth *Auth) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	normalized := make(map[string]*ModelState, len(auth.ModelStates))
	changed := false
	for model, state := range auth.ModelStates {
		modelKey := canonicalModelKey(model)
		if modelKey == "" {
			modelKey = strings.TrimSpace(model)
		}
		if modelKey != model {
			changed = true
		}
		if existing, ok := normalized[modelKey]; ok {
			normalized[modelKey] = mergeModelState(existing, state)
			changed = true
			continue
		}
		normalized[modelKey] = state
	}
	if changed {
		auth.ModelStates = normalized
	}
	return changed
}

// mergeModelState combines two model states for the same canonical model key, preferring
// the most recently updated state while keeping the stricter failure signals.
func mergeModelState(target, source *ModelState) *ModelState {
	if target == nil {
		return source
	}
	if source == nil {
		return target
	}

	preferred := target
	fallback := source
	if source.UpdatedAt.After(target.UpdatedAt) {
		preferred = source
		fallback = target
	}
	merged := ModelState{
		Status:         preferred.Status,
		StatusMessage:  preferred.StatusMessage,
		Unavailable:    target.Unavailable || source.Unavailable,
		NextRetryAfter: target.NextRetryAfter,
		LastError:      cloneError(preferred.LastError),
		Quota: QuotaState{
			Exceeded:      target.Quota.Exceeded || source.Quota.Exceeded,
			Reason:        preferred.Quota.Reason,
			NextRecoverAt: target.Quota.NextRecoverAt,
			BackoffLevel:  preferred.Quota.BackoffLevel,
		},
		UpdatedAt: preferred.UpdatedAt,
	}
	merged.Quota = mergeQuotaObservation(merged.Quota, fallback.Quota)
	merged.Quota = mergeQuotaObservation(merged.Quota, preferred.Quota)
	if fallback.NextRetryAfter.After(merged.NextRetryAfter) {
		merged.NextRetryAfter = fallback.NextRetryAfter
	}
	if fallback.Quota.NextRecoverAt.After(merged.Quota.NextRecoverAt) {
		merged.Quota.NextRecoverAt = fallback.Quota.NextRecoverAt
	}
	// Keep the strongest cooldown watermark from either side so merging a
	// restored or alias-migrated state into a live auth cannot reopen the
	// stale-success race. Both sides stay zero across a process restart,
	// where restored cooldowns must remain clearable by any success.
	merged.cooldownRecordedSeq = target.cooldownRecordedSeq
	if source.cooldownRecordedSeq > merged.cooldownRecordedSeq {
		merged.cooldownRecordedSeq = source.cooldownRecordedSeq
	}
	return &merged
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	applyCooldownFields(&state.Quota, QuotaState{})
	state.cooldownRecordedSeq = 0
	state.UpdatedAt = now
}

func modelStateIsClean(state *ModelState) bool {
	if state == nil {
		return true
	}
	if state.Status != StatusActive {
		return false
	}
	if state.Unavailable || state.StatusMessage != "" || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		return false
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		return false
	}
	return true
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	// A live credential-scoped quota cooldown keeps the whole credential
	// unavailable regardless of per-model aggregates (upstream b064b832e242).
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		auth.Unavailable = true
		return
	}
	if len(auth.ModelStates) == 0 {
		clearAggregatedAvailability(auth)
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		hasState = true
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		clearAggregatedAvailability(auth)
		return
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		// Aggregation never shortens a still-live auth-level recovery deadline
		// (upstream 1c22598d0b5a).
		if auth.Quota.NextRecoverAt.After(quotaRecover) {
			quotaRecover = auth.Quota.NextRecoverAt
		}
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		// Retain a still-live auth-level quota cooldown even when no model
		// state currently reports quota (upstream 1c22598d0b5a).
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func clearAggregatedAvailability(auth *Auth) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	applyCooldownFields(&auth.Quota, QuotaState{})
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.cooldownRecordedSeq = 0
	auth.UpdatedAt = now
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func resultErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	var sourceErr *Error
	var resultErr *Error
	if errors.As(err, &sourceErr) && sourceErr != nil {
		resultErr = cloneError(sourceErr)
	} else {
		resultErr = &Error{Message: err.Error()}
	}
	if resultErr.HTTPStatus == 0 {
		resultErr.HTTPStatus = statusCodeFromError(err)
	}
	switch {
	case isRequestScopedError(err):
		// Prefer true request-scoped faults (including Claude OAuth cancellation)
		// over the broader connection-lifecycle classification.
		resultErr.Code = requestScopedErrorCode
	case isConnectionLifecycleError(err):
		// Preserve lifecycle classification for MarkResult without making the error
		// request-scoped (which would also stop credential fallback).
		if resultErr.Code == "" || resultErr.Code == connectionLifecycleErrorCode {
			resultErr.Code = connectionLifecycleErrorCode
		}
	case isTransientTransportError(err):
		// Pre-HTTP dial/TLS/DNS/reset failures retry under request-retry without
		// cooling the credential (upstream bef1f65c6c1d).
		if resultErr.Code == "" || resultErr.Code == transientTransportErrorCode {
			resultErr.Code = transientTransportErrorCode
		}
	}
	return resultErr
}

func isRequestScopedError(err error) bool {
	if err == nil {
		return false
	}
	type requestScopedError interface {
		IsRequestScoped() bool
	}
	var requestErr requestScopedError
	return errors.As(err, &requestErr) && requestErr != nil && requestErr.IsRequestScoped()
}

// isCredentialScopedError reports whether an executor error applies to the
// whole credential across models rather than only the requested model
// (upstream conductor_cooldown.go).
func isCredentialScopedError(err error) bool {
	if err == nil {
		return false
	}
	type credentialScopedProvider interface {
		IsCredentialScoped() bool
	}
	var csp credentialScopedProvider
	return errors.As(err, &csp) && csp != nil && csp.IsCredentialScoped()
}

func isUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromError(err) == http.StatusUnauthorized {
		return true
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "status 401") || strings.Contains(raw, "401 unauthorized")
}

func hasUnauthorizedAuthFailure(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	if auth.Unavailable && auth.Status == StatusError && auth.NextRefreshAfter.IsZero() &&
		(auth.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(auth.LastError.Code, "unauthorized")) {
		return true
	}
	return false
}

// HasUnauthorizedAuthFailure reports whether the auth has a terminal unauthorized error
// with no pending refresh scheduled.
// Ported from upstream CLIProxyAPI commit b4ff581dafa4 (conductor_cooldown.go).
func HasUnauthorizedAuthFailure(auth *Auth) bool {
	return hasUnauthorizedAuthFailure(auth)
}

func hasDisabledInvalidGrantFailure(auth *Auth) bool {
	if auth == nil {
		return false
	}
	isDisabled := auth.Disabled || auth.Status == StatusDisabled
	if !isDisabled {
		return false
	}
	if auth.LastError != nil && (isInvalidGrantResultError(auth.LastError) || isInvalidGrantErrorMessage(auth.LastError.Message) || isInvalidGrantErrorMessage(auth.LastError.Code)) {
		return true
	}
	return false
}

// HasDisabledInvalidGrantFailure reports whether the auth is disabled and has encountered an invalid_grant error.
func HasDisabledInvalidGrantFailure(auth *Auth) bool {
	return hasDisabledInvalidGrantFailure(auth)
}

func refreshErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 && isUnauthorizedError(err) {
		statusCode = http.StatusUnauthorized
	}
	authErr := &Error{Message: err.Error(), HTTPStatus: statusCode}
	if statusCode == http.StatusUnauthorized {
		authErr.Code = "unauthorized"
		authErr.Retryable = false
	}
	return authErr
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	value := *retryAfter
	return &value
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isModelSupportError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Message)
}

func isInvalidGrantIdentifier(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "invalid_grant")
}

func isErrorIdentifierByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func containsBoundedErrorIdentifier(message, identifier string) bool {
	message = strings.ToLower(message)
	identifier = strings.ToLower(identifier)
	for offset := 0; offset < len(message); {
		index := strings.Index(message[offset:], identifier)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(identifier)
		if (index == 0 || !isErrorIdentifierByte(message[index-1])) && (end == len(message) || !isErrorIdentifierByte(message[end])) {
			return true
		}
		offset = index + 1
	}
	return false
}

func containsStructuredInvalidGrant(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if text, ok := item.(string); ok {
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "error", "code", "type":
					if isInvalidGrantIdentifier(text) {
						return true
					}
				case "message", "error_description", "description":
					if containsBoundedErrorIdentifier(text, "invalid_grant") {
						return true
					}
				}
			}
			switch item.(type) {
			case map[string]any, []any:
				if containsStructuredInvalidGrant(item) {
					return true
				}
			}
		}
	case []any:
		for _, item := range typed {
			if containsStructuredInvalidGrant(item) {
				return true
			}
		}
	}
	return false
}

func isInvalidGrantErrorMessage(message string) bool {
	var payload any
	if errJSON := json.Unmarshal([]byte(strings.TrimSpace(message)), &payload); errJSON == nil {
		return containsStructuredInvalidGrant(payload)
	}
	return containsBoundedErrorIdentifier(message, "invalid_grant")
}

func isInvalidGrantError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnauthorized && status != 0 {
		return false
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return isInvalidGrantIdentifier(authErr.Code) || isInvalidGrantErrorMessage(authErr.Message)
	}
	return isInvalidGrantErrorMessage(err.Error())
}

func isInvalidGrantResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnauthorized && status != 0 {
		return false
	}
	return isInvalidGrantIdentifier(err.Code) || isInvalidGrantErrorMessage(err.Message)
}

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return clienterror.IsItemNotPersisted(err.Message)
}

func isRequestScopedResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return err.IsRequestScoped() || isRequestScopedNotFoundResultError(err)
}

// shouldSkipCredentialCooldown reports failures that must not mark auth/model cooling.
// Connection lifecycle is intentionally separate from request_scoped so transport
// drops do not also stop credential rotation via isRequestInvalidError.
func shouldSkipCredentialCooldown(err *Error) bool {
	if err != nil && err.Code == ErrorCodeForceCooldown {
		return false
	}
	return isRequestScopedResultError(err) || isConnectionLifecycleResultError(err) || isTransientTransportResultError(err)
}

// isConnectionLifecycleError reports transport/session lifecycle failures that must
// not cool credentials: client cancellation and WebSocket close/EOF disconnects.
func isConnectionLifecycleError(err error) bool {
	if err == nil {
		return false
	}
	// Typed WebSocket close codes are an unambiguous connection lifecycle signal.
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr != nil {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure:
			return true
		}
	}
	// Credential/auth/quota statuses must never be reclassified from response text.
	if statusCodeFromError(err) != 0 {
		return false
	}
	// Client abort and request-scoped timeouts are not credential faults.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return isConnectionLifecycleMessage(err.Error())
}

func isConnectionLifecycleResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if err.Code == connectionLifecycleErrorCode {
		return true
	}
	// Message fallback only when no HTTP status is attached, so 401/429/5xx
	// response bodies cannot suppress credential cooldown.
	if statusCodeFromResult(err) != 0 {
		return false
	}
	return isConnectionLifecycleMessage(err.Message)
}

func isConnectionLifecycleMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	switch lower {
	case "context canceled", "context deadline exceeded", "eof", "unexpected eof":
		return true
	}
	// gorilla/websocket CloseError.Error() and common wrappers.
	if strings.Contains(lower, "websocket: close 1000") ||
		strings.Contains(lower, "websocket: close 1001") ||
		strings.Contains(lower, "websocket: close 1006") {
		return true
	}
	// Wrapped transport EOF phrasing (e.g. "read tcp ...: unexpected EOF").
	if strings.Contains(lower, "unexpected eof") {
		return true
	}
	return false
}

// isTransientTransportError reports pre-HTTP dial/TLS/DNS/reset failures that
// should retry under request-retry without cooling the selected credential.
// Ported from upstream CLIProxyAPI commit bef1f65c6c1d.
func isTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	// HTTP-status failures stay on the credential/status retry path.
	if statusCodeFromError(err) != 0 {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr != nil && (dnsErr.IsTimeout || dnsErr.IsTemporary || dnsErr.Timeout()) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr != nil && netErr.Timeout() {
		return true
	}
	if isTransientSyscallError(err) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr != nil {
		return true
	}
	return isTransientTransportMessage(err.Error())
}

func isTransientTransportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if err.Code == transientTransportErrorCode {
		return true
	}
	if statusCodeFromResult(err) != 0 {
		return false
	}
	return isTransientTransportMessage(err.Message)
}

func isTransientSyscallError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED,
		syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EPIPE:
		return true
	default:
		return false
	}
}

func isTransientTransportMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	switch {
	case strings.Contains(lower, "tls: tls handshake"),
		strings.Contains(lower, "tls handshake timeout"),
		strings.Contains(lower, "wsarecv"),
		strings.Contains(lower, "wsasend"),
		strings.Contains(lower, "a connection attempt failed"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "i/o timeout"),
		strings.Contains(lower, "no such host"),
		strings.Contains(lower, "server misbehaving"),
		strings.Contains(lower, "network is unreachable"),
		strings.Contains(lower, "no route to host"),
		strings.Contains(lower, "broken pipe"),
		strings.Contains(lower, "connection aborted"),
		strings.Contains(lower, "use of closed network connection"),
		strings.Contains(lower, "unexpected eof"):
		return true
	default:
		return false
	}
}

// isRequestRetryRoundError reports whether err participates in request-retry
// rounds: either a credential retry-round status or a transient transport failure.
// Ported from upstream CLIProxyAPI commit bef1f65c6c1d.
func isRequestRetryRoundError(err error) bool {
	if err == nil {
		return false
	}
	return isCredentialRetryRoundStatus(statusCodeFromError(err)) || isTransientTransportError(err)
}

// isCredentialRetryRoundStatus reports statuses that participate in credential
// retry rounds (upstream conductor_selection.go).
func isCredentialRetryRoundStatus(status int) bool {
	switch status {
	case http.StatusForbidden,
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isCountTokensEndpointNotFoundError(err error, _ string) bool {
	if err == nil || statusCodeFromError(err) != http.StatusNotFound {
		return false
	}
	return !isExplicitModelNotFoundError(err)
}

func isExplicitModelNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		if isModelNotFoundIdentifier(authErr.Code) || isStructuredModelNotFoundError(authErr.Message) {
			return true
		}
	}
	if isStructuredModelNotFoundError(err.Error()) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, nested := range wrapped.Unwrap() {
			if isExplicitModelNotFoundError(nested) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return isExplicitModelNotFoundError(wrapped.Unwrap())
	}
	return false
}

func isStructuredModelNotFoundError(message string) bool {
	var payload any
	if errJSON := json.Unmarshal([]byte(strings.TrimSpace(message)), &payload); errJSON != nil {
		return false
	}
	return containsStructuredModelNotFound(payload)
}

func containsStructuredModelNotFound(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if text, ok := item.(string); ok {
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "code", "type", "error":
					if isModelNotFoundIdentifier(text) {
						return true
					}
				}
			}
			switch item.(type) {
			case map[string]any, []any:
				if containsStructuredModelNotFound(item) {
					return true
				}
			}
		}
	case []any:
		for _, item := range typed {
			if containsStructuredModelNotFound(item) {
				return true
			}
		}
	}
	return false
}

func isModelNotFoundIdentifier(value string) bool {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	return normalized == "model_not_found"
}

// isRequestInvalidError returns true if the error represents a client request
// error that should neither rotate nor penalize credentials. Model-support
// errors remain eligible for alternate routing and keep their model-level state.
// Untyped 413 payload rejections (message_too_big) are deliberately excluded so
// they retain credential fallback; only explicitly typed request-scoped errors
// stop rotation for oversized payloads.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	if isRequestScopedError(err) {
		return true
	}
	if isInvalidGrantError(err) {
		return false
	}
	if isModelSupportError(err) {
		return false
	}
	status := statusCodeFromError(err)
	if clienterror.IsRequestFault(status, err) && status != http.StatusRequestEntityTooLarge {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Message != "" {
		// When authErr.Code is non-empty, Error() formats as "Code: Message" which
		// breaks JSON parsing in clienterror. Re-evaluate against the raw Message body.
		if clienterror.IsRequestFault(status, errors.New(authErr.Message)) && status != http.StatusRequestEntityTooLarge {
			return true
		}
	}
	return false
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time, disableCooling bool) {
	if auth == nil {
		return
	}
	prevAuthRetryAfter := auth.NextRetryAfter
	if shouldSkipCredentialCooldown(resultErr) {
		return
	}
	if resultErr != nil && resultErr.Code == ErrorCodeForceCooldown {
		disableCooling = false
	}
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 402, 403:
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 404:
		auth.StatusMessage = "not_found"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else if retryAfter != nil && *retryAfter > 0 {
			auth.NextRetryAfter = now.Add(*retryAfter)
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		var next time.Time
		if !disableCooling {
			if retryAfter != nil {
				cooldown := *retryAfter
				if cooldown < minQuotaCooldownFloor {
					cooldown = minQuotaCooldownFloor
				}
				next = now.Add(cooldown)
			} else {
				next, auth.Quota.BackoffLevel = quotaCooldownAfterFailure(auth.Quota, disableCooling, now)
			}
			if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(next) {
				next = auth.Quota.NextRecoverAt
			}
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526:
		auth.StatusMessage = "transient upstream error"
		auth.NextRetryAfter = recoverableFailureRetryAfterWithHint(now, retryAfter, disableCooling)
		auth.Unavailable = !auth.NextRetryAfter.IsZero()
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
	// A later failure only extends a still-live credential cooldown; a
	// deliberate zero write (disableCooling) still clears it
	// (upstream 1c22598d0b5a).
	if !auth.NextRetryAfter.IsZero() && prevAuthRetryAfter.After(auth.NextRetryAfter) && prevAuthRetryAfter.After(now) {
		auth.NextRetryAfter = prevAuthRetryAfter
	}
	if resultErr != nil && resultErr.Code == ErrorCodeForceCooldown && auth.NextRetryAfter.IsZero() {
		auth.NextRetryAfter = now.Add(time.Minute)
	}
}

// quotaCooldownAfterFailure returns the recovery deadline and backoff level for
// a quota failure observed at now. Failures inside an active recovery window
// reuse that window instead of escalating the backoff level again.
func quotaCooldownAfterFailure(quota QuotaState, disableCooling bool, now time.Time) (time.Time, int) {
	if disableCooling {
		_, nextLevel := nextQuotaCooldown(quota.BackoffLevel, disableCooling)
		return time.Time{}, nextLevel
	}
	if quota.NextRecoverAt.After(now) {
		return quota.NextRecoverAt, quota.BackoffLevel
	}
	cooldown, nextLevel := nextQuotaCooldown(quota.BackoffLevel, disableCooling)
	var next time.Time
	if cooldown > 0 {
		next = now.Add(cooldown)
	}
	return next, nextLevel
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// ResetQuota clears quota/cooldown state for an auth and resumes registry routing.
func (m *Manager) ResetQuota(ctx context.Context, authID string) (*Auth, []string, error) {
	if m == nil {
		return nil, nil, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil, errors.New("auth id is required")
	}

	now := time.Now()
	models := make([]string, 0)
	registeredModels := modelsForRegisteredAuth(authID)

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return nil, nil, nil
	}

	for modelKey, state := range auth.ModelStates {
		if strings.TrimSpace(modelKey) == "" {
			continue
		}
		models = append(models, modelKey)
		if state != nil {
			resetModelState(state, now)
		}
	}
	if len(models) == 0 {
		models = append(models, registeredModels...)
	}
	models = dedupeStrings(models)

	// ResetQuota is an explicit administrative clear: drop the auth-level
	// cooldown so aggregation cannot retain a still-live quota deadline.
	applyCooldownFields(&auth.Quota, QuotaState{})

	updateAggregatedAvailability(auth, now)
	if len(auth.ModelStates) == 0 {
		clearAggregatedAvailability(auth)
	}
	if !auth.Disabled && auth.Status != StatusDisabled && !hasModelError(auth, now) {
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	auth.UpdatedAt = now
	if errPersist := m.persist(ctx, auth); errPersist != nil {
		m.mu.Unlock()
		return nil, nil, errPersist
	}
	snapshot := auth.Clone()
	m.mu.Unlock()

	for _, modelKey := range models {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(authID, modelKey)
		registry.GetGlobalRegistry().ResumeClientModel(authID, modelKey)
	}
	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	m.persistCooldownStateSnapshot(ctx)
	return snapshot, models, nil
}

// ResetUsage clears the success/failure counters and the recent-request ring for
// an auth. Those counters are in-memory only, so nothing is persisted and routing
// state is left untouched. Calling it repeatedly yields the same result.
func (m *Manager) ResetUsage(authID string) (*Auth, error) {
	if m == nil {
		return nil, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, errors.New("auth id is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		return nil, nil
	}
	auth.resetUsageCounters()
	return auth.Clone(), nil
}

func modelsForRegisteredAuth(authID string) []string {
	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	models := make([]string, 0, len(supportedModels))
	for _, supportedModel := range supportedModels {
		if supportedModel == nil || strings.TrimSpace(supportedModel.ID) == "" {
			continue
		}
		models = append(models, canonicalModelKey(supportedModel.ID))
	}
	return models
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// PublishIfRegistrationCurrent serializes the final model-registration
// publication against identity mutations for this auth, then calls publish
// only if the manager still holds auth with the same model-registration
// inputs. It does not persist or clone auth state. publish must be fast and
// must not re-enter the manager.
func (m *Manager) PublishIfRegistrationCurrent(id string, registrationEpoch uint64, prefix, provider string, disabled bool, authKind string, attributes map[string]string, publish func() bool) bool {
	if m == nil || id == "" || publish == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.auths[id]
	if !ok || current == nil || !modelRegistrationInputsMatch(current, registrationEpoch, prefix, provider, disabled, authKind, attributes) {
		return false
	}
	return publish()
}

func modelRegistrationInputsMatch(current *Auth, registrationEpoch uint64, prefix, provider string, disabled bool, authKind string, attributes map[string]string) bool {
	if current == nil {
		return false
	}
	currentDisabled := current.Disabled || current.Status == StatusDisabled
	return current.RegistrationEpoch == registrationEpoch &&
		current.Prefix == prefix &&
		current.Provider == provider &&
		currentDisabled == disabled &&
		current.AuthKind() == authKind &&
		maps.Equal(current.Attributes, attributes)
}

// GetExecutionSessionAuthByID retrieves a Home runtime auth scoped to an execution session.
func (m *Manager) GetExecutionSessionAuthByID(sessionID string, authID string) (*Auth, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	if auth == nil {
		return nil, false
	}
	return auth.Clone(), true
}

// canonicalSchedulingProvider normalizes provider spellings to their canonical
// scheduling key. kimi.com credentials schedule under "kimi" and kimi.ai
// credentials under "kimi-ai" so the two OAuth domains stay isolated while
// dotted aliases resolve to their canonical forms.
func canonicalSchedulingProvider(key string) string {
	lower := strings.ToLower(strings.TrimSpace(key))
	switch lower {
	case "kimi.com":
		return "kimi"
	case "kimi.ai":
		return "kimi-ai"
	default:
		return lower
	}
}

// executorLocked resolves the executor for a provider key while the caller holds
// m.mu (read or write). It tolerates case differences and maps the kimi domain
// aliases onto the shared "kimi" executor registration.
func (m *Manager) executorLocked(provider string) (ProviderExecutor, bool) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	if !okExecutor {
		switch strings.ToLower(provider) {
		case "kimi-ai", "kimi.ai", "kimi.com":
			executor, okExecutor = m.executors["kimi"]
		}
	}
	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executorLocked(provider)
}

// AvailableProviders returns the set of provider keys that currently have at least one
// registered auth record that is not disabled. It is a best-effort snapshot for routing
// decisions and does not account for per-model cooldowns or transient runtime availability.
// Disabled auths (Disabled flag or StatusDisabled) are excluded so routing does not target
// providers that auth selection would refuse to use, which would otherwise cause execution
// failures instead of falling back to lower-priority routers.
func (m *Manager) AvailableProviders() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[string]struct{}, len(m.auths))
	out := make([]string, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		provider := canonicalSchedulingProvider(auth.Provider)
		if provider == "" {
			continue
		}
		if _, ok := seen[provider]; !ok {
			seen[provider] = struct{}{}
			out = append(out, provider)
		}
	}
	sort.Strings(out)
	return out
}

// HasProviderAuth reports whether at least one non-disabled auth record is registered for
// the provider. Disabled auths (Disabled flag or StatusDisabled) are excluded to match the
// behavior of auth selection, which refuses to pick disabled credentials.
func (m *Manager) HasProviderAuth(provider string) bool {
	if m == nil {
		return false
	}
	targetKey := canonicalSchedulingProvider(provider)
	if targetKey == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if canonicalSchedulingProvider(auth.Provider) == targetKey {
			return true
		}
	}
	return false
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.Lock()
	var selections []*HomeDispatchSelection
	if sessionID == CloseAllExecutionSessionsID {
		m.clearHomeRuntimeAuthsLocked()
		selections = m.takeAllHomeSessionSelectionsLocked()
		m.clearHomeSessionLocks()
	} else {
		m.clearHomeRuntimeAuthsForSessionLocked(sessionID)
		selections = m.takeHomeSessionSelectionsLocked(sessionID)
		m.homeSessionLocks.Delete(sessionID)
	}
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.Unlock()

	for _, selection := range selections {
		selection.End("session_closed")
	}
	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

func (m *Manager) useSchedulerFastPath() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	return isBuiltInSelector(m.selector)
}

func shouldRetrySchedulerPick(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable"
}

func (m *Manager) routeAwareSelectionRequired(auth *Auth, routeModel string) bool {
	if auth == nil || strings.TrimSpace(routeModel) == "" {
		return false
	}
	return m.selectionModelKeyForAuth(auth, routeModel) != canonicalModelKey(routeModel)
}

func codexAlphaSearchRequired(opts cliproxyexecutor.Options) bool {
	return strings.EqualFold(strings.TrimSpace(opts.SourceFormat.String()), "codex-alpha-search")
}

func authSupportsCodexAlphaSearch(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if auth.Attributes != nil && strings.EqualFold(strings.TrimSpace(auth.Attributes["alpha_search"]), "true") {
		return true
	}
	if auth.Metadata != nil {
		if enabled, ok := auth.Metadata["alpha_search"].(bool); ok {
			return enabled
		}
	}
	return false
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if m.HomeEnabled() {
		auth, exec, _, err := m.pickNextViaHome(ctx, model, opts, tried)
		return auth, exec, err
	}

	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = provider
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = selectionArgForSelector(m.selector, model)

	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)

	m.mu.RLock()
	executor, okExecutor := m.executorLocked(provider)
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	targetKey := canonicalSchedulingProvider(provider)
	for _, candidate := range m.auths {
		if candidate == nil || canonicalSchedulingProvider(candidate.Provider) != targetKey || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		if codexAlphaSearchRequired(opts) && !authSupportsCodexAlphaSearch(candidate) {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, errAvailable := m.availableAuthsForSelector(m.selector, candidates, provider, model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		m.warnLogAuthUnavailable(ctx, []string{provider}, model, opts, nil, errAvailable)
		return nil, nil, errAvailable
	}
	selected, errPick := m.selector.Pick(ctx, provider, selectionArgForSelector(m.selector, model), opts, available)
	if errPick != nil {
		m.mu.RUnlock()
		m.warnLogAuthUnavailable(ctx, []string{provider}, model, opts, nil, errPick)
		return nil, nil, errPick
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if m.HomeEnabled() {
		auth, exec, _, err := m.pickNextViaHome(ctx, model, opts, tried)
		return auth, exec, err
	}

	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = provider
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = model

	if !m.useSchedulerFastPath() {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	if strings.TrimSpace(model) != "" {
		m.mu.RLock()
		targetKey := canonicalSchedulingProvider(provider)
		for _, candidate := range m.auths {
			if candidate == nil || canonicalSchedulingProvider(candidate.Provider) != targetKey || candidate.Disabled {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if m.routeAwareSelectionRequired(candidate, model) {
				m.mu.RUnlock()
				return m.pickNextLegacy(ctx, provider, model, opts, tried)
			}
		}
		m.mu.RUnlock()
	}
	executor, okExecutor := m.Executor(provider)
	if !okExecutor {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, errPick := m.scheduler.pickSingle(ctx, provider, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, errPick = m.scheduler.pickSingle(ctx, provider, model, opts, tried)
		}
		if errPick != nil {
			m.warnLogAuthUnavailable(ctx, []string{provider}, model, opts, tried, errPick)
			return nil, nil, errPick
		}
		if selected == nil {
			return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if (disallowFreeAuth && isFreeCodexAuth(selected)) || (codexAlphaSearchRequired(opts) && !authSupportsCodexAlphaSearch(selected)) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		authCopy := selected.Clone()
		if !selected.indexAssigned {
			m.mu.Lock()
			if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
				current.EnsureIndex()
				authCopy = current.Clone()
			}
			m.mu.Unlock()
		}
		return authCopy, executor, nil
	}
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m.HomeEnabled() {
		return m.pickNextViaHome(ctx, model, opts, tried)
	}

	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = "mixed"
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = selectionArgForSelector(m.selector, model)

	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := canonicalSchedulingProvider(provider)
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		if codexAlphaSearchRequired(opts) && !authSupportsCodexAlphaSearch(candidate) {
			continue
		}
		providerKey := canonicalSchedulingProvider(candidate.Provider)
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if _, ok := m.executorLocked(providerKey); !ok {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, errAvailable := m.availableAuthsForSelector(m.selector, candidates, "mixed", model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		m.warnLogAuthUnavailable(ctx, providers, model, opts, nil, errAvailable)
		return nil, nil, "", errAvailable
	}
	selected, errPick := m.selector.Pick(ctx, "mixed", selectionArgForSelector(m.selector, model), opts, available)
	if errPick != nil {
		m.mu.RUnlock()
		m.warnLogAuthUnavailable(ctx, providers, model, opts, nil, errPick)
		return nil, nil, "", errPick
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	providerKey := canonicalSchedulingProvider(selected.Provider)
	executor, okExecutor := m.executorLocked(providerKey)
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m.HomeEnabled() {
		return m.pickNextViaHome(ctx, model, opts, tried)
	}

	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = "mixed"
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = model

	if !m.useSchedulerFastPath() {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}

	eligibleProviders := make([]string, 0, len(providers))
	seenProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := canonicalSchedulingProvider(provider)
		if providerKey == "" {
			continue
		}
		if _, seen := seenProviders[providerKey]; seen {
			continue
		}
		if _, okExecutor := m.Executor(providerKey); !okExecutor {
			continue
		}
		seenProviders[providerKey] = struct{}{}
		eligibleProviders = append(eligibleProviders, providerKey)
	}
	if len(eligibleProviders) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if strings.TrimSpace(model) != "" {
		providerSet := make(map[string]struct{}, len(eligibleProviders))
		for _, providerKey := range eligibleProviders {
			providerSet[providerKey] = struct{}{}
		}
		m.mu.RLock()
		for _, candidate := range m.auths {
			if candidate == nil || candidate.Disabled {
				continue
			}
			if _, ok := providerSet[canonicalSchedulingProvider(candidate.Provider)]; !ok {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if codexAlphaSearchRequired(opts) && !authSupportsCodexAlphaSearch(candidate) {
				continue
			}
			if m.routeAwareSelectionRequired(candidate, model) {
				m.mu.RUnlock()
				return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
			}
		}
		m.mu.RUnlock()
	}

	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, providerKey, errPick := m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, providerKey, errPick = m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
		}
		if errPick != nil {
			return nil, nil, "", errPick
		}
		if selected == nil {
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if (disallowFreeAuth && isFreeCodexAuth(selected)) || (codexAlphaSearchRequired(opts) && !authSupportsCodexAlphaSearch(selected)) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		executor, okExecutor := m.Executor(providerKey)
		if !okExecutor {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		authCopy := selected.Clone()
		if !selected.indexAssigned {
			m.mu.Lock()
			if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
				current.EnsureIndex()
				authCopy = current.Clone()
			}
			m.mu.Unlock()
		}
		return authCopy, executor, providerKey, nil
	}
}

type homeErrorEnvelope struct {
	Error *homeErrorDetail `json:"error"`
}

type homeErrorDetail struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	Code         string `json:"code,omitempty"`
	Retryable    bool   `json:"retryable,omitempty"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
}

const (
	homeUpstreamModelAttributeKey     = "home_upstream_model"
	homeForceMappingAttributeKey      = "home_force_mapping"
	homeOriginalAliasAttributeKey     = "home_original_alias"
	homeRequestRetryExceededErrorCode = "request_retry_exceeded"
)

func isHomeRequestRetryExceededError(err error) bool {
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(authErr.Code), homeRequestRetryExceededErrorCode)
}

func shouldReturnLastErrorOnPickFailure(homeMode bool, lastErr error, errPick error) bool {
	if lastErr == nil {
		return false
	}
	if !homeMode {
		return true
	}
	return isHomeRequestRetryExceededError(errPick)
}

type homeAuthDispatchResponse struct {
	Model         string                 `json:"model"`
	Provider      string                 `json:"provider"`
	AuthIndex     string                 `json:"auth_index"`
	UserAPIKey    string                 `json:"user_api_key"`
	ForceMapping  bool                   `json:"force_mapping"`
	OriginalAlias string                 `json:"original_alias"`
	ModelInfo     *homeDispatchModelInfo `json:"model_info,omitempty"`
	Auth          Auth                   `json:"auth"`
}

// homeDispatchModelInfo carries the capability metadata Home resolved for the
// dispatched model so thinking/reasoning handling can use the authoritative
// definition instead of the local registry fallback.
//
// Ported from upstream CLIProxyAPI commits 6ff680e90ab5 and a9e92b81453f
// (sdk/cliproxy/auth/conductor_home.go); the latter adds NativeCapabilities.
type homeDispatchModelInfo struct {
	ID                         string                       `json:"id"`
	Type                       string                       `json:"type,omitempty"`
	InputTokenLimit            int                          `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit           int                          `json:"outputTokenLimit,omitempty"`
	ContextLength              int                          `json:"context_length,omitempty"`
	MaxCompletionTokens        int                          `json:"max_completion_tokens,omitempty"`
	Thinking                   *registry.ThinkingSupport    `json:"thinking,omitempty"`
	NativeCapabilities         *registry.NativeCapabilities `json:"native_capabilities,omitempty"`
	SupportConfigurationUpdate *bool                        `json:"support_configuration_update,omitempty"`
	UserDefined                bool                         `json:"user_defined"`
}

func (m *homeDispatchModelInfo) registryModelInfo() *registry.ModelInfo {
	if m == nil || strings.TrimSpace(m.ID) == "" {
		return nil
	}
	return &registry.ModelInfo{
		ID:                         strings.TrimSpace(m.ID),
		Type:                       strings.TrimSpace(m.Type),
		InputTokenLimit:            m.InputTokenLimit,
		OutputTokenLimit:           m.OutputTokenLimit,
		ContextLength:              m.ContextLength,
		MaxCompletionTokens:        m.MaxCompletionTokens,
		Thinking:                   m.Thinking,
		NativeCapabilities:         m.NativeCapabilities,
		SupportConfigurationUpdate: m.SupportConfigurationUpdate != nil && *m.SupportConfigurationUpdate,
		UserDefined:                m.UserDefined,
	}
}

type homeAuthDispatcher interface {
	HeartbeatOK() bool
	RPopAuth(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int) ([]byte, error)
	AbortAmbiguousDispatch()
}

var currentHomeDispatcher = func() homeAuthDispatcher {
	return home.Current()
}

func setHomeUserAPIKeyOnGinContext(ctx context.Context, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || ctx == nil {
		return
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Set(string, any) })
	if !ok || ginCtx == nil {
		return
	}
	ginCtx.Set("userApiKey", apiKey)
}

func homeDispatchHeaders(ctx context.Context, headers http.Header) http.Header {
	apiKey, ok := homeQueryCredentialFromContext(ctx)
	if !ok {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = http.Header{}
	}
	if out.Get("Authorization") != "" || out.Get("X-Goog-Api-Key") != "" || out.Get("X-Api-Key") != "" {
		return out
	}
	out.Set("X-Goog-Api-Key", apiKey)
	return out
}

func homeQueryCredentialFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if queryCtx, ok := ctx.Value("gin").(interface{ Query(string) string }); ok && queryCtx != nil {
		if apiKey := strings.TrimSpace(queryCtx.Query("key")); apiKey != "" {
			return apiKey, true
		}
		if apiKey := strings.TrimSpace(queryCtx.Query("auth_token")); apiKey != "" {
			return apiKey, true
		}
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return "", false
	}
	rawMetadata, ok := ginCtx.Get("accessMetadata")
	if !ok {
		return "", false
	}
	source := accessMetadataSource(rawMetadata)
	if source != "query-key" && source != "query-auth-token" {
		return "", false
	}
	rawAPIKey, ok := ginCtx.Get("userApiKey")
	if !ok {
		return "", false
	}
	apiKey := contextStringValue(rawAPIKey)
	if apiKey == "" {
		return "", false
	}
	return apiKey, true
}

func accessMetadataSource(raw any) string {
	switch v := raw.(type) {
	case map[string]string:
		return strings.TrimSpace(v["source"])
	case map[string]any:
		return contextStringValue(v["source"])
	default:
		return ""
	}
}

func contextStringValue(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func homeExecutionSessionIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func (m *Manager) clearHomeRuntimeAuths() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.clearHomeRuntimeAuthsLocked()
	selections := m.takeAllHomeSessionSelectionsLocked()
	m.mu.Unlock()
	for _, selection := range selections {
		selection.End("home_disabled")
	}
}

func (m *Manager) clearHomeRuntimeAuthsLocked() {
	if m == nil {
		return
	}
	m.homeRuntimeAuths = make(map[string]map[string]*Auth)
	m.homeRuntimeAuthOwners = make(map[string]map[string]*HomeDispatchSelection)
}

func (m *Manager) clearHomeRuntimeAuthsForSessionLocked(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}
	delete(m.homeRuntimeAuths, sessionID)
	delete(m.homeRuntimeAuthOwners, sessionID)
}

func (m *Manager) rememberHomeRuntimeAuth(sessionID string, auth *Auth) {
	sessionID = strings.TrimSpace(sessionID)
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	if m == nil || auth == nil || sessionID == "" || authID == "" || !authWebsocketsEnabled(auth) {
		return
	}
	m.mu.Lock()
	if m.homeRuntimeAuths == nil {
		m.homeRuntimeAuths = make(map[string]map[string]*Auth)
	}
	sessionAuths := m.homeRuntimeAuths[sessionID]
	if sessionAuths == nil {
		sessionAuths = make(map[string]*Auth)
		m.homeRuntimeAuths[sessionID] = sessionAuths
	}
	sessionAuths[authID] = auth.Clone()
	if owners := m.homeRuntimeAuthOwners[sessionID]; owners != nil {
		delete(owners, authID)
		if len(owners) == 0 {
			delete(m.homeRuntimeAuthOwners, sessionID)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) homeRuntimeAuthByID(sessionID string, authID string) (*Auth, ProviderExecutor, string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, nil, "", false
	}
	m.mu.RLock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	m.mu.RUnlock()
	if auth == nil || !authWebsocketsEnabled(auth) {
		return nil, nil, "", false
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" {
		return nil, nil, "", false
	}
	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", false
	}
	return auth.Clone(), executor, providerKey, true
}

func (m *Manager) pickNextViaHome(ctx context.Context, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	executionSessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	count := homeAuthCountFromMetadata(opts.Metadata)
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && count <= 1 {
		if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" {
			_, alreadyTried := tried[pinnedAuthID]
			if !alreadyTried {
				if auth, executor, providerKey, ok := m.homeRuntimeAuthByID(executionSessionID, pinnedAuthID); ok {
					return auth, executor, providerKey, nil
				}
			}
		}
	}

	client := home.Current()
	if client == nil || !client.HeartbeatOK() {
		return nil, nil, "", &Error{Code: "home_unavailable", Message: "home control center unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}

	requestedModel := requestedModelFromMetadata(opts.Metadata, model)
	sessionID := ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	dispatchHeaders := homeDispatchHeaders(ctx, opts.Headers)

	raw, err := client.RPopAuth(ctx, requestedModel, sessionID, dispatchHeaders, count)
	if err != nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: err.Error(), HTTPStatus: http.StatusServiceUnavailable}
	}

	var env homeErrorEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal == nil && env.Error != nil {
		code := strings.TrimSpace(env.Error.Type)
		if code == "" {
			code = strings.TrimSpace(env.Error.Code)
		}
		msg := strings.TrimSpace(env.Error.Message)
		if msg == "" {
			msg = "home returned error"
		}
		status := http.StatusBadGateway
		switch strings.ToLower(code) {
		case "model_not_found":
			status = http.StatusNotFound
		case "authentication_error", "unauthorized":
			status = http.StatusUnauthorized
		}
		return nil, nil, "", &Error{Code: code, Message: msg, HTTPStatus: status}
	}

	var dispatch homeAuthDispatchResponse
	if errUnmarshal := json.Unmarshal(raw, &dispatch); errUnmarshal != nil {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
	}
	setHomeUserAPIKeyOnGinContext(ctx, dispatch.UserAPIKey)
	auth := dispatch.Auth
	if strings.TrimSpace(auth.ID) == "" {
		// Backward compatibility: older home instances returned the auth directly.
		if errUnmarshal := json.Unmarshal(raw, &auth); errUnmarshal != nil {
			return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
		}
	}
	if upstreamModel := strings.TrimSpace(dispatch.Model); upstreamModel != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 1)
		}
		auth.Attributes[homeUpstreamModelAttributeKey] = upstreamModel
	}
	if strings.TrimSpace(auth.ID) == "" {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned auth without id", HTTPStatus: http.StatusBadGateway}
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned auth without provider", HTTPStatus: http.StatusBadGateway}
	}

	homeAuthIndex := strings.TrimSpace(dispatch.AuthIndex)
	if homeAuthIndex != "" {
		auth.Index = homeAuthIndex
		auth.indexAssigned = true
	} else {
		auth.EnsureIndex()
	}

	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered", HTTPStatus: http.StatusBadGateway}
	}

	authCopy := auth.Clone()
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && authWebsocketsEnabled(authCopy) {
		m.rememberHomeRuntimeAuth(executionSessionID, authCopy)
	}
	return authCopy, executor, providerKey, nil
}

func requestedModelFromMetadata(metadata map[string]any, fallback string) string {
	if metadata != nil {
		if v, ok := metadata[cliproxyexecutor.RequestedModelMetadataKey]; ok {
			switch typed := v.(type) {
			case string:
				if trimmed := strings.TrimSpace(typed); trimmed != "" {
					return trimmed
				}
			case []byte:
				if trimmed := strings.TrimSpace(string(typed)); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return "unknown"
	}
	return fallback
}

func (m *Manager) findAllAntigravityCreditsCandidateAuths(routeModel string, opts cliproxyexecutor.Options) []creditsCandidateEntry {
	if m == nil {
		return nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var known []creditsCandidateEntry
	var unknown []creditsCandidateEntry
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if pinnedAuthID != "" && auth.ID != pinnedAuthID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			continue
		}
		if !strings.Contains(strings.ToLower(strings.TrimSpace(routeModel)), "claude") {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		executor, ok := m.executorLocked(providerKey)
		if !ok {
			continue
		}

		hint, okHint := GetAntigravityCreditsHint(auth.ID)
		if okHint && hint.Known {
			if !hint.Available {
				continue
			}
			known = append(known, creditsCandidateEntry{
				auth:     auth.Clone(),
				executor: executor,
				provider: providerKey,
			})
			continue
		}
		unknown = append(unknown, creditsCandidateEntry{
			auth:     auth.Clone(),
			executor: executor,
			provider: providerKey,
		})
	}
	sort.Slice(known, func(i, j int) bool {
		return known[i].auth.ID < known[j].auth.ID
	})
	sort.Slice(unknown, func(i, j int) bool {
		return unknown[i].auth.ID < unknown[j].auth.ID
	})
	return append(known, unknown...)
}

type creditsCandidateEntry struct {
	auth     *Auth
	executor ProviderExecutor
	provider string
}

func hasAntigravityProvider(providers []string) bool {
	for _, p := range providers {
		if strings.EqualFold(strings.TrimSpace(p), "antigravity") {
			return true
		}
	}
	return false
}

func shouldAttemptAntigravityCreditsFallback(m *Manager, lastErr error, providers []string) bool {
	status := statusCodeFromError(lastErr)
	log.WithFields(log.Fields{
		"lastErr":   errorString(lastErr),
		"status":    status,
		"providers": providers,
	}).Debug("shouldAttemptAntigravityCreditsFallback")
	if m == nil || lastErr == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.QuotaExceeded.AntigravityCredits {
		return false
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case 0:
		var authErr *Error
		if errors.As(lastErr, &authErr) && authErr != nil {
			return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable" || authErr.Code == "model_cooldown"
		}
		var cooldownErr *modelCooldownError
		if errors.As(lastErr, &cooldownErr) {
			return true
		}
		return false
	default:
		return false
	}
}

func (m *Manager) tryAntigravityCreditsExecute(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, bool) {
	routeModel := req.Model
	candidates := m.findAllAntigravityCreditsCandidateAuths(routeModel, opts)
	for _, c := range candidates {
		if ctx.Err() != nil {
			return cliproxyexecutor.Response{}, false
		}
		creditsCtx := WithAntigravityCredits(ctx)
		if rt := m.roundTripperFor(c.auth); rt != nil {
			creditsCtx = context.WithValue(creditsCtx, roundTripperContextKey{}, rt)
			creditsCtx = context.WithValue(creditsCtx, "cliproxy.roundtripper", rt)
		}
		creditsOpts := ensureRequestedModelMetadata(opts, routeModel)
		creditsCtx = contextWithRequestedModelAlias(creditsCtx, creditsOpts, routeModel)
		preparedAuth, errPrepare := m.prepareRequestAuth(creditsCtx, c.executor, c.auth)
		if errPrepare != nil {
			continue
		}
		c.auth = preparedAuth
		publishSelectedAuthMetadata(creditsOpts.Metadata, c.auth.ID)
		models := m.executionModelCandidates(c.auth, routeModel)
		if len(models) == 0 {
			continue
		}
		for _, upstreamModel := range models {
			resultModel := m.stateModelForExecution(c.auth, routeModel, upstreamModel, len(models) > 1)
			execReq := req
			execReq.Model = upstreamModel
			execReq = m.attachResolvedExecutionModelInfo(execReq, c.auth, routeModel, upstreamModel, false)
			creditsCtx = syncMetadataSessionToContext(creditsCtx, creditsOpts.Metadata)
			executionSeq := m.nextExecutionSeq()
			resp, errExec := c.executor.Execute(creditsCtx, c.auth, execReq, creditsOpts)
			result := Result{AuthID: c.auth.ID, Provider: c.provider, Model: resultModel, Success: errExec == nil, Options: creditsOpts, ExecutionSeq: executionSeq}
			if errExec != nil {
				result.Error = &Error{Message: errExec.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](errExec); ok && se != nil {
					result.Error.HTTPStatus = se.StatusCode()
				}
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.MarkResult(creditsCtx, result)
				continue
			}
			applyKiroUsageResultFromResponse(&result, resp)
			m.MarkResult(creditsCtx, result)
			return resp, true
		}
	}
	return cliproxyexecutor.Response{}, false
}

func (m *Manager) tryAntigravityCreditsExecuteStream(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, bool) {
	routeModel := req.Model
	candidates := m.findAllAntigravityCreditsCandidateAuths(routeModel, opts)
	for _, c := range candidates {
		if ctx.Err() != nil {
			return nil, false
		}
		creditsCtx := WithAntigravityCredits(ctx)
		if rt := m.roundTripperFor(c.auth); rt != nil {
			creditsCtx = context.WithValue(creditsCtx, roundTripperContextKey{}, rt)
			creditsCtx = context.WithValue(creditsCtx, "cliproxy.roundtripper", rt)
		}
		creditsOpts := ensureRequestedModelMetadata(opts, routeModel)
		preparedAuth, errPrepare := m.prepareRequestAuth(creditsCtx, c.executor, c.auth)
		if errPrepare != nil {
			continue
		}
		c.auth = preparedAuth
		publishSelectedAuthMetadata(creditsOpts.Metadata, c.auth.ID)
		models := m.executionModelCandidates(c.auth, routeModel)
		if len(models) == 0 {
			continue
		}
		creditsCtx = syncMetadataSessionToContext(creditsCtx, creditsOpts.Metadata)
		result, errStream := m.executeStreamWithModelPool(creditsCtx, c.executor, c.auth, c.provider, req, creditsOpts, routeModel, "", models, len(models) > 1)
		if errStream != nil {
			continue
		}
		return result, true
	}
	return nil, false
}

// authPersistLock serializes persistence per auth ID and records the newest
// (epoch, generation) pair already sent to the store so out-of-order writes are
// dropped. Ported from upstream CLIProxyAPI commit 4c1bebe837a6.
type authPersistLock struct {
	mu             sync.Mutex
	lastEpoch      uint64
	lastGeneration uint64
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	return m.persistWithRevision(ctx, auth, 0)
}

// persistWithRevision saves auth, using the store's compare-and-swap when
// expectedRevision > 0 and the store exposes revisions. A stale expectation
// surfaces as ErrRevisionConflict.
func (m *Manager) persistWithRevision(ctx context.Context, auth *Auth, expectedRevision int64) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if !PersistsToStore(auth) {
		return nil
	}
	lockVal, _ := m.persistLocks.LoadOrStore(auth.ID, &authPersistLock{})
	pLock, _ := lockVal.(*authPersistLock)
	if pLock == nil {
		pLock = &authPersistLock{}
		m.persistLocks.Store(auth.ID, pLock)
	}
	pLock.mu.Lock()
	defer pLock.mu.Unlock()
	if auth.RegistrationEpoch < pLock.lastEpoch ||
		(auth.RegistrationEpoch == pLock.lastEpoch && auth.Generation < pLock.lastGeneration) {
		// Stale write from an older registration cycle or an older snapshot of the
		// same cycle: never overwrite newer persisted state (upstream 4c1bebe837a6).
		return nil
	}
	pLock.lastEpoch = auth.RegistrationEpoch
	pLock.lastGeneration = auth.Generation
	if shouldSkipPersist(ctx) {
		return nil
	}
	if expectedRevision > 0 {
		if revisioned, ok := m.store.(RevisionedStore); ok && revisioned != nil {
			_, err := revisioned.SaveIfRevision(ctx, auth, expectedRevision)
			return err
		}
		log.WithField("auth", auth.ID).Debug("auth store does not expose revisions; persisting without optimistic concurrency")
	}
	_, err := m.store.Save(ctx, auth)
	return err
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = refreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancelCtx := context.WithCancel(parent)
	workers := m.refreshWorkers()
	loop := newAuthAutoRefreshLoop(m, interval, workers)

	m.mu.Lock()
	m.refreshCancel = cancelCtx
	m.refreshLoop = loop
	m.mu.Unlock()

	loop.rebuild(time.Now())
	go loop.run(ctx)
}

// StopAutoRefresh cancels the background refresh loop, if running.
// It also stops the selector if it implements StoppableSelector.
func (m *Manager) StopAutoRefresh() {
	m.mu.Lock()
	cancel := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Stop selector if it implements StoppableSelector (e.g., SessionAffinitySelector)
	if stoppable, ok := m.selector.(StoppableSelector); ok {
		stoppable.Stop()
	}
}

func (m *Manager) queueRefreshReschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.queueReschedule(authID)
}

func (m *Manager) queueRefreshUnschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.remove(authID)
}

func invalidGrantBackoffDuration(failures int) time.Duration {
	if failures <= 1 {
		return invalidGrantBackoffBase
	}
	shift := failures - 1
	if shift > 10 {
		shift = 10
	}
	backoff := invalidGrantBackoffBase * time.Duration(1<<shift)
	if backoff > invalidGrantBackoffMax {
		return invalidGrantBackoffMax
	}
	return backoff
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil {
		return false
	}
	if hasUnauthorizedAuthFailure(a) || hasDisabledInvalidGrantFailure(a) {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(loop *authAutoRefreshLoop, id string, registrationEpoch uint64, now time.Time) *authRefreshJob {
	m.mu.Lock()
	auth := m.auths[id]
	if auth == nil || auth.RegistrationEpoch != registrationEpoch || hasUnauthorizedAuthFailure(auth) || hasDisabledInvalidGrantFailure(auth) {
		m.mu.Unlock()
		return nil
	}
	if m.refreshJobs[id] != nil || (!auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter)) {
		m.mu.Unlock()
		return nil
	}
	job := &authRefreshJob{
		id:                id,
		registrationEpoch: registrationEpoch,
		pendingUntil:      now.Add(refreshPendingBackoff),
		loop:              loop,
	}
	if m.refreshJobs == nil {
		m.refreshJobs = make(map[string]*authRefreshJob)
	}
	m.refreshJobs[id] = job
	auth.NextRefreshAfter = job.pendingUntil
	auth.Generation++
	auth.UpdatedAt = now
	m.mu.Unlock()

	m.queueRefreshReschedule(id)
	return job
}

func (m *Manager) beginRefreshJob(ctx context.Context, job *authRefreshJob) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[job.id]
	if ctx.Err() != nil || m.refreshJobs[job.id] != job || auth == nil || auth.RegistrationEpoch != job.registrationEpoch {
		return false
	}
	job.running = true
	return true
}

func (m *Manager) finishRefreshJob(job *authRefreshJob, retryAt time.Time, queuedOnly bool) {
	m.mu.Lock()
	if m.refreshJobs[job.id] != job || (queuedOnly && job.running) {
		m.mu.Unlock()
		return
	}
	delete(m.refreshJobs, job.id)
	if auth := m.auths[job.id]; auth != nil && auth.RegistrationEpoch == job.registrationEpoch && auth.NextRefreshAfter.Equal(job.pendingUntil) {
		// Clear only this job's pending marker, preserving newer refresh outcomes.
		auth.NextRefreshAfter = retryAt
		auth.Generation++
		auth.UpdatedAt = time.Now()
	}
	m.mu.Unlock()
	// Notify the current loop, which may differ from the job's original loop.
	m.queueRefreshReschedule(job.id)
}

// authRefreshLock serializes refresh operations for a single credential.
// Ported from upstream CLIProxyAPI (conductor_refresh.go).
type authRefreshLock struct {
	mu sync.Mutex
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	_, _ = m.refreshAuthForRequest(ctx, id, "")
}

// tryRefreshAfterUnauthorized refreshes local OAuth credentials once after a
// 401 so the current auth can be retried before fallback/suspend.
// Ported from upstream CLIProxyAPI sdk/cliproxy/auth/conductor_refresh.go;
// required by the Meta DCA mint-on-401 recovery path (upstream be7323f3bf66).
func (m *Manager) tryRefreshAfterUnauthorized(ctx context.Context, auth *Auth, execErr error, alreadyTried bool) (*Auth, bool) {
	if m == nil || auth == nil || alreadyTried || execErr == nil {
		return auth, false
	}
	// Request-scoped failures describe this request, not stale credentials.
	// Refreshing would turn a direct error response into an implicit retry.
	if isRequestScopedError(execErr) {
		return auth, false
	}
	if !isUnauthorizedError(execErr) || !authHasRefreshCredential(auth) {
		return auth, false
	}
	log.Debugf("unauthorized response for %s (%s), refreshing credentials before fallback", auth.Provider, auth.ID)
	refreshed, errRefresh := m.refreshAuthForRequest(ctx, auth.ID, authAccessToken(auth))
	if errRefresh != nil || refreshed == nil {
		log.Debugf("credential refresh before fallback failed for %s (%s): %v", auth.Provider, auth.ID, errRefresh)
		return auth, false
	}
	return refreshed, true
}

// refreshAuthForRequest performs a synchronous credential refresh for the given auth.
// failedAccessToken lets concurrent callers reuse a refresh that already replaced the
// access token that produced the unauthorized response.
// Ported from upstream CLIProxyAPI commits 9812b1e76872 and 4c1bebe837a6.
func (m *Manager) refreshAuthForRequest(ctx context.Context, id, failedAccessToken string) (*Auth, error) {
	return m.refreshAuthForRequestAtEpoch(ctx, id, failedAccessToken, 0)
}

func (m *Manager) refreshAuthForRequestAtEpoch(ctx context.Context, id, failedAccessToken string, registrationEpoch uint64) (*Auth, error) {
	if m == nil {
		return nil, errors.New("auth manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("auth id is empty")
	}

	lockValue, _ := m.refreshLocks.LoadOrStore(id, &authRefreshLock{})
	lock, _ := lockValue.(*authRefreshLock)
	if lock == nil {
		lock = &authRefreshLock{}
		m.refreshLocks.Store(id, lock)
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()

	m.mu.RLock()
	auth := m.auths[id].Clone()
	var exec ProviderExecutor
	if auth != nil {
		// Use the same effective provider key as request execution so OpenAI-compat
		// auths registered under namespaced keys still resolve for refresh.
		exec, _ = m.executorLocked(executorKeyFromAuth(auth))
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return nil, errors.New("auth or executor not found")
	}
	if registrationEpoch != 0 && auth.RegistrationEpoch != registrationEpoch {
		return nil, errors.New("auth registration changed before refresh")
	}
	if hasDisabledInvalidGrantFailure(auth) {
		return nil, errors.New("auth is disabled with invalid grant")
	}

	// Another request may already have refreshed this credential.
	if failedAccessToken != "" {
		if currentToken := authAccessToken(auth); currentToken != "" && currentToken != failedAccessToken {
			return auth.Clone(), nil
		}
	}

	base := auth.Clone()
	updated, err := exec.Refresh(ctx, base.Clone())
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return nil, err
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		unauthorized := isUnauthorizedError(err)
		invalidGrant := isInvalidGrantError(err)
		shouldReschedule := false
		isPermanentlyDisabled := false
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			if base != nil && current.RegistrationEpoch != base.RegistrationEpoch {
				m.mu.Unlock()
				return nil, err
			}
			current.Generation++
			current.UpdatedAt = now
			current.LastError = refreshErrorFromError(err)

			isDisabled := current.Disabled || current.Status == StatusDisabled
			hasValidAccessToken := current.HasValidAccessToken(now)
			if isDisabled && invalidGrant {
				current.Unavailable = true
				current.Status = StatusDisabled
				current.NextRefreshAfter = time.Time{}
				current.RefreshFailures = 0
				current.StatusMessage = "disabled (invalid grant)"
				isPermanentlyDisabled = true
			} else if isDisabled {
				current.Unavailable = true
				current.Status = StatusDisabled
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
				if current.StatusMessage == "" {
					current.StatusMessage = "disabled"
				}
				shouldReschedule = true
			} else if !hasValidAccessToken {
				current.Unavailable = true
				current.Status = StatusError
				if unauthorized {
					current.NextRefreshAfter = time.Time{}
					current.RefreshFailures = 0
					current.StatusMessage = "unauthorized"
				} else if invalidGrant {
					current.RefreshFailures++
					current.NextRefreshAfter = now.Add(invalidGrantBackoffDuration(current.RefreshFailures))
					current.StatusMessage = "invalid grant (retrying)"
					shouldReschedule = true
				} else {
					current.RefreshFailures = 0
					current.NextRefreshAfter = now.Add(refreshFailureBackoff)
					current.StatusMessage = "token expired"
					shouldReschedule = true
				}
			} else {
				// Access token remains valid. Preserve current in-flight/cooldown
				// status without overwrite (upstream 9812b1e76872).
				nextRetry := now.Add(refreshFailureBackoff)
				if invalidGrant {
					current.RefreshFailures++
					nextRetry = now.Add(invalidGrantBackoffDuration(current.RefreshFailures))
				} else {
					current.RefreshFailures = 0
				}
				if exp, ok := current.AccessTokenExpirationTime(); ok && !exp.IsZero() && nextRetry.After(exp) {
					nextRetry = exp
				}
				current.NextRefreshAfter = nextRetry
				shouldReschedule = true

				if !current.Unavailable {
					log.Warnf("credential refresh failed for %s (%s): %s; retaining active credential as access token is unexpired", current.Provider, current.ID, SanitizeUpstreamErrorSummary(err.Error()))
				}
			}
			m.auths[id] = current
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		if shouldReschedule {
			m.queueRefreshReschedule(id)
		} else if isPermanentlyDisabled {
			m.queueRefreshUnschedule(id)
		}
		return nil, err
	}
	if updated == nil {
		updated = base.Clone()
	}
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.StatusMessage = ""
	updated.Unavailable = false
	updated.RefreshFailures = 0
	if updated.Status == StatusError || updated.Status == "" {
		updated.Status = StatusActive
	}
	updated.UpdatedAt = now
	_ = clearUnauthorizedModelStates(updated, now)
	if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	saved, errUpdate := m.UpdateRefreshedAuth(ctx, base, updated)
	if errUpdate != nil {
		// Warn, not debug: a restart after a lost persist fails with invalid_grant.
		log.WithFields(log.Fields{"auth_id": auth.ID, "credential": auth.ID, "provider": auth.Provider}).Warnf("persist refreshed auth %s (%s) failed: %v", auth.Provider, auth.ID, errUpdate)
		return nil, errUpdate
	}
	if saved == nil {
		return nil, fmt.Errorf("auth %s not found", id)
	}
	targetAuth := saved
	supportedModels, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(id)
	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(targetAuth, sm.ID, now))
	}
	if len(projections) > 0 {
		registry.GetGlobalRegistry().ApplyClientModelProjections(id, regEpoch, targetAuth.Generation, projections)
	}
	return saved.Clone(), nil
}

// ForceRefreshAuth triggers an immediate synchronous refresh for the credential.
// Ported from upstream CLIProxyAPI (conductor_refresh.go).
func (m *Manager) ForceRefreshAuth(ctx context.Context, id string) (*Auth, error) {
	if m == nil {
		return nil, errors.New("auth manager is nil")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("auth id is empty")
	}
	return m.refreshAuthForRequest(ctx, id, "")
}

// refreshWorkers resolves the refresh worker pool size from runtime config,
// falling back to refreshMaxConcurrency.
// Ported from upstream CLIProxyAPI commit 6dce78673fbc.
func (m *Manager) refreshWorkers() int {
	workers := refreshMaxConcurrency
	if m != nil {
		if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
			workers = cfg.AuthAutoRefreshWorkers
		}
	}
	return workers
}

// ForceRefreshResult records the outcome of a forced refresh for one credential.
// Ported from upstream CLIProxyAPI (conductor_refresh.go).
type ForceRefreshResult struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// ForceRefreshAll triggers an immediate refresh for all credentials that have refresh tokens or custom refresh evaluators.
// Worker-pool bounding and cancellation checks ported from upstream CLIProxyAPI commit 6dce78673fbc.
func (m *Manager) ForceRefreshAll(ctx context.Context) []ForceRefreshResult {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.auths))
	for id, auth := range m.auths {
		if auth != nil && !auth.Disabled && (authHasRefreshCredential(auth) || auth.Runtime != nil) {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()

	results := make([]ForceRefreshResult, len(ids))
	if len(ids) == 0 {
		return results
	}

	workers := m.refreshWorkers()
	if workers <= 0 {
		workers = 1
	}
	if workers > len(ids) {
		workers = len(ids)
	}

	type refreshJob struct {
		index  int
		authID string
	}

	jobCh := make(chan refreshJob, len(ids))
	for i, id := range ids {
		jobCh <- refreshJob{index: i, authID: id}
	}
	close(jobCh)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				if errCtx := ctx.Err(); errCtx != nil {
					results[job.index] = ForceRefreshResult{
						ID:      job.authID,
						Success: false,
						Error:   errCtx.Error(),
					}
					continue
				}

				_, err := m.ForceRefreshAuth(ctx, job.authID)
				res := ForceRefreshResult{ID: job.authID, Success: err == nil}
				if err != nil {
					res.Error = err.Error()
				}
				results[job.index] = res
			}
		}()
	}
	wg.Wait()
	return results
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	exec, _ := m.executorLocked(provider)
	return exec
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey)
		}
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "kimi.com":
		return "kimi"
	case "kimi.ai":
		return "kimi-ai"
	default:
		return provider
	}
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec, _ = m.executorLocked(executorKeyFromAuth(a))
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(ctx context.Context, auth *Auth, method, targetURL string, body []byte, headers http.Header) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	return exec.HttpRequest(ctx, auth, req)
}

// syncMetadataSessionToContext mirrors the authoritative session hierarchy from
// execution metadata into the immutable ClientRequestMetadata carried by ctx so
// asynchronous usage consumers (reporters, queue plugins) observe the same
// canonical session and parent lineage as the routing path. When metadata has
// no session identity, stale context values are cleared.
func syncMetadataSessionToContext(ctx context.Context, metadata map[string]any) context.Context {
	if ctx == nil {
		return nil
	}
	canonicalID := ""
	if len(metadata) > 0 {
		canonicalID, _ = metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
		if canonicalID == "" {
			canonicalID, _ = metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey].(string)
		}
		if canonicalID == "" {
			if execID, _ := metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string); execID != "" {
				execID = strings.TrimSpace(execID)
				if !strings.HasPrefix(execID, "execution:") {
					canonicalID = "execution:" + execID
				} else {
					canonicalID = execID
				}
			}
		}
		if canonicalID == "" {
			if derivedID, _ := metadata[cliproxyexecutor.DerivedSessionIDMetadataKey].(string); derivedID != "" {
				derivedID = strings.TrimSpace(derivedID)
				if !strings.HasPrefix(derivedID, "derived:") {
					canonicalID = "derived:" + derivedID
				} else {
					canonicalID = derivedID
				}
			}
		}
	}
	canonicalID = strings.TrimSpace(canonicalID)
	if canonicalID == "" {
		clientMeta := logging.GetClientRequestMetadata(ctx)
		if clientMeta.SessionID != "" || clientMeta.ParentSessionID != "" || clientMeta.NodeKind != "" || clientMeta.IsFork || clientMeta.IsCompaction {
			clientMeta.SessionID = ""
			clientMeta.ParentSessionID = ""
			clientMeta.NodeKind = ""
			clientMeta.IsFork = false
			clientMeta.IsCompaction = false
			ctx = logging.WithClientRequestMetadata(ctx, clientMeta)
		}
		return util.WithSessionID(ctx, "")
	}
	clientMeta := logging.GetClientRequestMetadata(ctx)
	clientMeta.SessionID = cliproxysession.BoundSessionIdentity(canonicalID)
	if parentID, ok := metadata[cliproxyexecutor.ParentSessionIDMetadataKey].(string); ok && strings.TrimSpace(parentID) != "" {
		clientMeta.ParentSessionID = cliproxysession.BoundSessionIdentity(strings.TrimSpace(parentID))
	} else {
		clientMeta.ParentSessionID = ""
	}
	if clientMeta.SessionID == clientMeta.ParentSessionID {
		clientMeta.ParentSessionID = ""
	}
	if nodeKind, ok := metadata[cliproxyexecutor.NodeKindMetadataKey].(string); ok && strings.TrimSpace(nodeKind) != "" {
		clientMeta.NodeKind = strings.TrimSpace(nodeKind)
	} else {
		clientMeta.NodeKind = ""
	}
	if isFork, ok := metadata[cliproxyexecutor.IsForkMetadataKey].(bool); ok {
		clientMeta.IsFork = isFork
	} else {
		clientMeta.IsFork = false
	}
	if isCompaction, ok := metadata[cliproxyexecutor.IsCompactionMetadataKey].(bool); ok {
		clientMeta.IsCompaction = isCompaction
	} else {
		clientMeta.IsCompaction = false
	}
	ctx = logging.WithClientRequestMetadata(ctx, clientMeta)
	return util.WithSessionID(ctx, clientMeta.SessionID)
}

// authSelectionModelFromOptions returns the model used for auth selection. A
// restored execution request keeps its upstream model in Request.Model while
// this key preserves the original selection model (upstream conductor_execution.go).
func authSelectionModelFromOptions(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.AuthSelectionModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	case []byte:
		if strings.TrimSpace(string(value)) != "" {
			return strings.TrimSpace(string(value))
		}
	}
	return fallback
}

// executionModelForAuthSelection reports whether Request.Model is already an
// execution (upstream) model rather than the selection model.
func executionModelForAuthSelection(opts cliproxyexecutor.Options, model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}
	selectionModel := authSelectionModelFromOptions(opts, model)
	if selectionModel == model {
		return "", false
	}
	return model, true
}
