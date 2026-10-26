// Package registry provides centralized model management for all AI service providers.
// It implements a dynamic model registry with reference counting to track active clients
// and automatically hide models when no clients are available or when quota is exceeded.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	misc "github.com/therealtinhtute/llmhub/internal/misc"
)

// OpenAIImageModelType marks models that are callable through OpenAI-compatible image endpoints.
const OpenAIImageModelType = "openai-image"

// NativeCapabilities contains tri-state native capability metadata from the catalog.
type NativeCapabilities struct {
	// WebSearch reports explicit per-model support for native web search.
	// nil means the catalog has not established support either way.
	WebSearch *bool `json:"web_search,omitempty"`
}

// ModelInfo represents information about an available model
type ModelInfo struct {
	// ID is the unique identifier for the model
	ID string `json:"id"`
	// MetadataModelID identifies the canonical model used to resolve client metadata.
	// It is internal and must not be exposed in model-list responses.
	MetadataModelID string `json:"-"`
	// ExplicitThinking indicates thinking/reasoning configuration was explicitly configured for this model.
	ExplicitThinking bool `json:"-"`
	// ExplicitInputModalities indicates input modalities were explicitly configured for this model.
	ExplicitInputModalities bool `json:"-"`
	// Object type for the model (typically "model")
	Object string `json:"object"`
	// Created timestamp when the model was created
	Created int64 `json:"created"`
	// OwnedBy indicates the organization that owns the model
	OwnedBy string `json:"owned_by"`
	// Type indicates the model type (e.g., "claude", "gemini", "openai")
	Type string `json:"type"`
	// DisplayName is the human-readable name for the model
	DisplayName string `json:"display_name,omitempty"`
	// Name is used for Gemini-style model names
	Name string `json:"name,omitempty"`
	// Version is the model version
	Version string `json:"version,omitempty"`
	// Description provides detailed information about the model
	Description string `json:"description,omitempty"`
	// InputTokenLimit is the maximum input token limit
	InputTokenLimit int `json:"inputTokenLimit,omitempty"`
	// OutputTokenLimit is the maximum output token limit
	OutputTokenLimit int `json:"outputTokenLimit,omitempty"`
	// SupportedGenerationMethods lists supported generation methods
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
	// ContextLength is the context window size
	ContextLength int `json:"context_length,omitempty"`
	// MaxContextLength is an explicit per-model context window override from configuration.
	// It is carried internally for Codex client model catalog generation.
	MaxContextLength int `json:"-"`
	// MaxCompletionTokens is the maximum completion tokens
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	// SupportedParameters lists supported parameters
	SupportedParameters []string `json:"supported_parameters,omitempty"`
	// SupportedInputModalities lists supported input modalities (e.g., TEXT, IMAGE, VIDEO, AUDIO)
	SupportedInputModalities []string `json:"supportedInputModalities,omitempty"`
	// SupportedOutputModalities lists supported output modalities (e.g., TEXT, IMAGE)
	SupportedOutputModalities []string `json:"supportedOutputModalities,omitempty"`
	// SupportsWebSearch indicates this model can execute native web search
	// (currently populated for Antigravity models listed with web search support).
	SupportsWebSearch bool `json:"supports_web_search,omitempty"`

	// NativeCapabilities contains internal, static per-model capability metadata
	// decoded from the catalog's `native_capabilities` object. It is kept out of
	// normal JSON serialization so model listings stay unchanged.
	NativeCapabilities *NativeCapabilities `json:"-"`

	// SupportConfigurationUpdate reports internal support for configuration_update.
	SupportConfigurationUpdate bool `json:"-"`

	// Thinking holds provider-specific reasoning/thinking budget capabilities.
	// This is optional and currently used for Gemini thinking budget normalization.
	Thinking *ThinkingSupport `json:"thinking,omitempty"`

	// UserDefined indicates this model was defined through config file's models[]
	// array (e.g., openai-compatibility.*.models[], *-api-key.models[]).
	// UserDefined models have thinking configuration passed through without validation.
	UserDefined bool `json:"-"`

	// IsCompat enables compatibility handling for this configured API-key model.
	// When true, Codex/Responses executors preserve reasoning content and ids for
	// third-party Responses-compatible endpoints instead of sanitizing them.
	// Ported from upstream CLIProxyAPI commit 81d6ba774621.
	IsCompat bool `json:"-"`
}

// UnmarshalJSON loads internal capability metadata without exposing it
// through ModelInfo's normal JSON serialization.
func (m *ModelInfo) UnmarshalJSON(data []byte) error {
	type modelInfoAlias ModelInfo
	aux := struct {
		*modelInfoAlias
		NativeCapabilities         *NativeCapabilities `json:"native_capabilities"`
		SupportConfigurationUpdate bool                `json:"support_configuration_update"`
	}{modelInfoAlias: (*modelInfoAlias)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	m.NativeCapabilities = aux.NativeCapabilities
	m.SupportConfigurationUpdate = aux.SupportConfigurationUpdate
	return nil
}

// HasNativeWebSearch reports whether the model explicitly supports native web
// search, either via the static `supports_web_search` flag or via the
// `native_capabilities.web_search` catalog metadata.
func (m *ModelInfo) HasNativeWebSearch() bool {
	if m == nil {
		return false
	}
	if m.SupportsWebSearch {
		return true
	}
	return m.NativeCapabilities != nil && m.NativeCapabilities.WebSearch != nil && *m.NativeCapabilities.WebSearch
}

// LookupModelWebSearchSupport returns tri-state native web search support for a
// model: true/false when the catalog states it explicitly, nil when unknown.
// It resolves through the same dynamic-then-static path as LookupModelInfo so
// model-specific max-token and capability metadata stay consistent.
func LookupModelWebSearchSupport(modelID string, provider ...string) *bool {
	info := LookupModelInfo(modelID, provider...)
	if info == nil {
		return nil
	}
	if info.NativeCapabilities != nil && info.NativeCapabilities.WebSearch != nil {
		supported := *info.NativeCapabilities.WebSearch
		return &supported
	}
	if info.SupportsWebSearch {
		supported := true
		return &supported
	}
	return nil
}

type availableModelsCacheEntry struct {
	models    []map[string]any
	expiresAt time.Time
}

// ThinkingSupport describes a model family's supported internal reasoning budget range.
// Values are interpreted in provider-native token units.
type ThinkingSupport struct {
	// Min is the minimum allowed thinking budget (inclusive).
	Min int `json:"min,omitempty" yaml:"min,omitempty"`
	// Max is the maximum allowed thinking budget (inclusive).
	Max int `json:"max,omitempty" yaml:"max,omitempty"`
	// ZeroAllowed indicates whether 0 is a valid value (to disable thinking).
	ZeroAllowed bool `json:"zero_allowed,omitempty" yaml:"zero-allowed,omitempty"`
	// DynamicAllowed indicates whether -1 is a valid value (dynamic thinking budget).
	DynamicAllowed bool `json:"dynamic_allowed,omitempty" yaml:"dynamic-allowed,omitempty"`
	// Levels defines discrete reasoning effort levels (e.g., "low", "medium", "high").
	// When set, the model uses level-based reasoning instead of token budgets.
	Levels []string `json:"levels,omitempty" yaml:"levels,omitempty"`
	// DisableType is the Claude thinking.type sent when thinking is turned off.
	// Empty means "disabled"; claude-sonnet-5-5 needs "between_tools".
	DisableType string `json:"disable_type,omitempty" yaml:"disable-type,omitempty"`
}

// ModelRegistration tracks a model's availability
type ModelRegistration struct {
	// Info contains the model metadata
	Info *ModelInfo
	// InfoByProvider maps provider identifiers to specific ModelInfo to support differing capabilities.
	InfoByProvider map[string]*ModelInfo
	// Count is the number of active clients that can provide this model
	Count int
	// LastUpdated tracks when this registration was last modified
	LastUpdated time.Time
	// QuotaExceededClients tracks which clients have exceeded quota for this model
	QuotaExceededClients map[string]*time.Time
	// Providers tracks available clients grouped by provider identifier
	Providers map[string]int
	// SuspendedClients tracks temporarily disabled clients keyed by client ID
	SuspendedClients map[string]string
}

// ModelRegistryHook provides optional callbacks for external integrations to track model list changes.
// Hook implementations must be non-blocking and resilient; calls are executed asynchronously and panics are recovered.
type ModelRegistryHook interface {
	OnModelsRegistered(ctx context.Context, provider, clientID string, models []*ModelInfo)
	OnModelsUnregistered(ctx context.Context, provider, clientID string)
}

// ModelRegistry manages the global registry of available models
type ModelRegistry struct {
	// models maps model ID to registration information
	models map[string]*ModelRegistration
	// clientModels maps client ID to the models it provides
	clientModels map[string][]string
	// clientModelInfos maps client ID to a map of model ID -> ModelInfo
	// This preserves the original model info provided by each client
	clientModelInfos map[string]map[string]*ModelInfo
	// clientProviders maps client ID to its provider identifier
	clientProviders map[string]string
	// clientEpochs tracks monotonic registration epochs for each client ID
	clientEpochs map[string]uint64
	// clientGenerations tracks the latest generation applied for each client ID
	clientGenerations map[string]uint64
	// mutex ensures thread-safe access to the registry
	mutex *sync.RWMutex
	// availableModelsCache stores per-handler snapshots for GetAvailableModels.
	availableModelsCache map[string]availableModelsCacheEntry
	// generation tracks changes to model registrations and availability.
	generation uint64
	// hook is an optional callback sink for model registration changes
	hook ModelRegistryHook
}

// Global model registry instance
var globalRegistry *ModelRegistry
var registryOnce sync.Once

// GetGlobalRegistry returns the global model registry instance
func GetGlobalRegistry() *ModelRegistry {
	registryOnce.Do(func() {
		globalRegistry = &ModelRegistry{
			models:               make(map[string]*ModelRegistration),
			clientModels:         make(map[string][]string),
			clientModelInfos:     make(map[string]map[string]*ModelInfo),
			clientProviders:      make(map[string]string),
			clientEpochs:         make(map[string]uint64),
			clientGenerations:    make(map[string]uint64),
			availableModelsCache: make(map[string]availableModelsCacheEntry),
			mutex:                &sync.RWMutex{},
		}
	})
	return globalRegistry
}
func (r *ModelRegistry) ensureAvailableModelsCacheLocked() {
	if r.availableModelsCache == nil {
		r.availableModelsCache = make(map[string]availableModelsCacheEntry)
	}
}

func (r *ModelRegistry) invalidateAvailableModelsCacheLocked() {
	r.generation++
	if len(r.availableModelsCache) == 0 {
		return
	}
	clear(r.availableModelsCache)
}

// GetGeneration returns the current generation counter of model registrations.
func (r *ModelRegistry) GetGeneration() uint64 {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return r.generation
}

// LookupModelInfo searches dynamic registry (provider-specific > global) then static definitions.
func LookupModelInfo(modelID string, provider ...string) *ModelInfo {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}

	p := ""
	if len(provider) > 0 {
		p = strings.ToLower(strings.TrimSpace(provider[0]))
	}

	if info := GetGlobalRegistry().GetModelInfo(modelID, p); info != nil {
		return postProcessClaudeModelInfo(cloneModelInfo(info))
	}
	info := LookupStaticModelInfo(modelID)
	if info == nil {
		// The raw ID was already probed above; only its family candidates remain.
		for _, familyID := range claudeRegistryFamilyIDs(modelID) {
			if familyID != modelID {
				if info = LookupStaticModelInfo(familyID); info != nil {
					break
				}
			}
		}
	}
	return postProcessClaudeModelInfo(cloneModelInfo(info))
}

// claudeRegistryFamilyIDs is the candidate generator for LookupModelInfo, which
// probes each candidate through LookupStaticModelInfo — a whole-catalog scan, not
// a Claude-section scan — so a non-Claude ID can resolve here too. It keeps the
// shape this site has always used, which differs from claudeFamilyIDs:
//   - the bracket strip fires at the first '[' whether or not a ']' follows, so a
//     truncated "gpt-5.2[1m" still reaches "gpt-5.2";
//   - the date strip is computed on the full ID, independently of the bracket
//     strip, so "gpt-5.2[1m]-20261005" reaches "gpt-5.2" and then "gpt-5.2[1m]".
//
// Candidates, in resolution order: the normalized ID, its bracket-stripped base,
// then the full ID minus a trailing "-YYYYMMDD".
func claudeRegistryFamilyIDs(modelID string) []string {
	clean := normalizeFamilyID(modelID)
	if clean == "" {
		return nil
	}
	ids := []string{clean}
	seen := map[string]bool{clean: true}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if bracket := strings.IndexByte(clean, '['); bracket > 0 {
		add(clean[:bracket])
	}
	if dated, ok := stripTrailingDate(clean); ok {
		add(dated)
	}
	return ids
}

func postProcessClaudeModelInfo(info *ModelInfo) *ModelInfo {
	if info == nil || info.Thinking == nil {
		return info
	}
	id := strings.ToLower(strings.TrimSpace(info.ID))
	if id == "claude-sonnet-5-5" || strings.HasPrefix(id, "claude-sonnet-5-5-") || strings.HasPrefix(id, "claude-sonnet-5-5[") {
		if info.Thinking.DisableType == "" {
			info.Thinking.DisableType = "between_tools"
		}
	}
	if id == "claude-opus-5-5" || strings.HasPrefix(id, "claude-opus-5-5[") || strings.HasPrefix(id, "claude-opus-5-5-") {
		info.Thinking.ZeroAllowed = false
	}
	if id == "claude-fable-5" || strings.HasPrefix(id, "claude-fable-5-") || strings.HasPrefix(id, "claude-fable-5[") ||
		id == "claude-fable-5-1" || strings.HasPrefix(id, "claude-fable-5-1-") || strings.HasPrefix(id, "claude-fable-5-1[") {
		info.Thinking.ZeroAllowed = false
		info.Thinking.Min = 0
		info.Thinking.Max = 0
		info.Thinking.DynamicAllowed = true
	}
	if id == "claude-opus-4-7" || strings.HasPrefix(id, "claude-opus-4-7-") || strings.HasPrefix(id, "claude-opus-4-7[") ||
		id == "claude-opus-4-8" || strings.HasPrefix(id, "claude-opus-4-8-") || strings.HasPrefix(id, "claude-opus-4-8[") {
		info.Thinking.Min = 0
		info.Thinking.Max = 0
		info.Thinking.DynamicAllowed = true
	}
	return info
}

// NativeCapabilityRoute describes one registered route to a public model.
type NativeCapabilityRoute struct {
	Provider           string
	NativeCapabilities *NativeCapabilities
}

// ResolveResponsesWebSearchCapability conservatively resolves native web search
// across every route that can serve a public model. A known unsupported route or
// explicit model-level false wins; missing/unknown data produces unknown.
func ResolveResponsesWebSearchCapability(routes []NativeCapabilityRoute) *bool {
	if len(routes) == 0 {
		return nil
	}

	hasUnknown := false
	for _, route := range routes {
		if route.NativeCapabilities != nil && route.NativeCapabilities.WebSearch != nil && !*route.NativeCapabilities.WebSearch {
			return boolPointer(false)
		}
		pathSupport := responsesWebSearchProviderPathSupport(route.Provider)
		if pathSupport == nil {
			hasUnknown = true
			continue
		}
		if !*pathSupport {
			return boolPointer(false)
		}
		if route.NativeCapabilities == nil || route.NativeCapabilities.WebSearch == nil {
			hasUnknown = true
		}
	}
	if hasUnknown {
		return nil
	}
	return boolPointer(true)
}

func responsesWebSearchProviderPathSupport(provider string) *bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "codex", "xai", "claude", "antigravity":
		return boolPointer(true)
	case "openai", "openai-compatibility", "gemini", "aistudio", "vertex", "kimi", "kimi-ai", "kimi.ai", "kimi.com", "interactions", "gemini-interactions":
		return boolPointer(false)
	default:
		if strings.HasPrefix(provider, "openai-compatible-") {
			return boolPointer(false)
		}
		return nil
	}
}

func boolPointer(value bool) *bool {
	return &value
}

// GetResponsesWebSearchCapability resolves capability metadata across every
// registered client route for the exact public model ID.
func (r *ModelRegistry) GetResponsesWebSearchCapability(modelID string) *bool {
	modelID = strings.TrimSpace(modelID)
	if r == nil || modelID == "" {
		return nil
	}

	r.mutex.RLock()
	defer r.mutex.RUnlock()

	routes := make([]NativeCapabilityRoute, 0)
	for clientID, modelIDs := range r.clientModels {
		for _, registeredID := range modelIDs {
			if strings.TrimSpace(registeredID) != modelID {
				continue
			}
			var nativeCapabilities *NativeCapabilities
			if info := r.clientModelInfos[clientID][registeredID]; info != nil {
				nativeCapabilities = info.NativeCapabilities
			}
			routes = append(routes, NativeCapabilityRoute{
				Provider:           r.clientProviders[clientID],
				NativeCapabilities: nativeCapabilities,
			})
		}
	}
	return ResolveResponsesWebSearchCapability(routes)
}

// SetHook sets an optional hook for observing model registration changes.
func (r *ModelRegistry) SetHook(hook ModelRegistryHook) {
	if r == nil {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.hook = hook
}

const defaultModelRegistryHookTimeout = 5 * time.Second
const modelQuotaExceededWindow = 5 * time.Minute

func (r *ModelRegistry) triggerModelsRegistered(provider, clientID string, models []*ModelInfo) {
	hook := r.hook
	if hook == nil {
		return
	}
	modelsCopy := cloneModelInfosUnique(models)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Errorf("model registry hook OnModelsRegistered panic: %v", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), defaultModelRegistryHookTimeout)
		defer cancel()
		hook.OnModelsRegistered(ctx, provider, clientID, modelsCopy)
	}()
}

func (r *ModelRegistry) triggerModelsUnregistered(provider, clientID string) {
	hook := r.hook
	if hook == nil {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Errorf("model registry hook OnModelsUnregistered panic: %v", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), defaultModelRegistryHookTimeout)
		defer cancel()
		hook.OnModelsUnregistered(ctx, provider, clientID)
	}()
}

// RegisterClient registers a client and its supported models
// Parameters:
//   - clientID: Unique identifier for the client
//   - clientProvider: Provider name (e.g., "gemini", "claude", "openai")
//   - models: List of models that this client can provide
func (r *ModelRegistry) RegisterClient(clientID, clientProvider string, models []*ModelInfo) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.registerClientLocked(clientID, clientProvider, models)
}

// RegisterClientIfEpochCurrent registers a client's models only if its
// registration epoch still equals expectedEpoch and publish is non-nil. The
// fence evaluation and the registration runs atomically under the registry
// mutex. publish must be fast and must not call back into the registry or
// block. The returned value reports whether the registration was applied.
func (r *ModelRegistry) RegisterClientIfEpochCurrent(clientID, clientProvider string, expectedEpoch uint64, models []*ModelInfo, publish func() bool) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.clientEpochs[clientID] != expectedEpoch || publish == nil || !publish() {
		return false
	}
	r.registerClientLocked(clientID, clientProvider, models)
	return true
}

// UnregisterClientIfEpochCurrent removes a client only if its registration
// epoch still equals expectedEpoch and publish is non-nil. The fence
// evaluation and the removal runs atomically under the registry mutex.
// publish must be fast and must not call back into the registry or block.
// The returned value reports whether the removal was applied.
func (r *ModelRegistry) UnregisterClientIfEpochCurrent(clientID string, expectedEpoch uint64, publish func() bool) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.clientEpochs[clientID] != expectedEpoch || publish == nil || !publish() {
		return false
	}
	r.unregisterClientInternal(clientID)
	r.invalidateAvailableModelsCacheLocked()
	return true
}

// registerClientLocked reconciles a client's models while the registry mutex is held.
func (r *ModelRegistry) registerClientLocked(clientID, clientProvider string, models []*ModelInfo) {
	r.ensureAvailableModelsCacheLocked()
	provider := strings.ToLower(clientProvider)
	uniqueModelIDs := make([]string, 0, len(models))
	rawModelIDs := make([]string, 0, len(models))
	newModels := make(map[string]*ModelInfo, len(models))
	newCounts := make(map[string]int, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			continue
		}
		rawModelIDs = append(rawModelIDs, model.ID)
		newCounts[model.ID]++
		if _, exists := newModels[model.ID]; exists {
			continue
		}
		newModels[model.ID] = model
		uniqueModelIDs = append(uniqueModelIDs, model.ID)
	}

	if len(uniqueModelIDs) == 0 {
		// No models supplied; unregister existing client state if present.
		r.unregisterClientInternal(clientID)
		delete(r.clientModels, clientID)
		delete(r.clientModelInfos, clientID)
		delete(r.clientProviders, clientID)
		r.invalidateAvailableModelsCacheLocked()
		misc.LogCredentialSeparator()
		return
	}

	if r.clientGenerations == nil {
		r.clientGenerations = make(map[string]uint64)
	}
	if r.clientEpochs == nil {
		r.clientEpochs = make(map[string]uint64)
	}
	r.clientEpochs[clientID]++
	r.clientGenerations[clientID] = uint64(0)

	now := time.Now()

	oldModels, hadExisting := r.clientModels[clientID]
	oldProvider := r.clientProviders[clientID]
	providerChanged := oldProvider != provider
	if !hadExisting {
		// Pure addition path.
		for _, modelID := range rawModelIDs {
			model := newModels[modelID]
			r.addModelRegistration(modelID, provider, model, now, clientID)
		}
		r.clientModels[clientID] = append([]string(nil), rawModelIDs...)
		// Store client's own model infos
		clientInfos := make(map[string]*ModelInfo, len(newModels))
		for id, m := range newModels {
			clientInfos[id] = cloneModelInfo(m)
		}
		r.clientModelInfos[clientID] = clientInfos
		if provider != "" {
			r.clientProviders[clientID] = provider
		} else {
			delete(r.clientProviders, clientID)
		}
		r.invalidateAvailableModelsCacheLocked()
		r.triggerModelsRegistered(provider, clientID, models)
		log.Debugf("Registered client %s from provider %s with %d models", clientID, clientProvider, len(rawModelIDs))
		misc.LogCredentialSeparator()
		return
	}

	oldCounts := make(map[string]int, len(oldModels))
	for _, id := range oldModels {
		oldCounts[id]++
	}

	added := make([]string, 0)
	for _, id := range uniqueModelIDs {
		if oldCounts[id] == 0 {
			added = append(added, id)
		}
	}

	removed := make([]string, 0)
	for id := range oldCounts {
		if newCounts[id] == 0 {
			removed = append(removed, id)
		}
	}

	// Handle provider change for overlapping models before modifications.
	if providerChanged && oldProvider != "" {
		for id, newCount := range newCounts {
			if newCount == 0 {
				continue
			}
			oldCount := oldCounts[id]
			if oldCount == 0 {
				continue
			}
			toRemove := newCount
			if oldCount < toRemove {
				toRemove = oldCount
			}
			if reg, ok := r.models[id]; ok && reg.Providers != nil {
				if count, okProv := reg.Providers[oldProvider]; okProv {
					if count <= toRemove {
						delete(reg.Providers, oldProvider)
						if reg.InfoByProvider != nil {
							delete(reg.InfoByProvider, oldProvider)
						}
					} else {
						reg.Providers[oldProvider] = count - toRemove
						// Recompute the aggregate capability for the provider the
						// client moved away from (upstream 60e5b8bd432e).
						if reg.InfoByProvider != nil && reg.InfoByProvider[oldProvider] != nil {
							setModelInfoWebSearch(reg.InfoByProvider[oldProvider], r.mergedWebSearchCapabilityLocked(id, oldProvider, clientID, nil))
						}
					}
				}
			}
		}
	}

	// Apply removals first to keep counters accurate.
	for _, id := range removed {
		oldCount := oldCounts[id]
		for i := 0; i < oldCount; i++ {
			r.removeModelRegistration(clientID, id, oldProvider, now)
		}
	}

	for id, oldCount := range oldCounts {
		newCount := newCounts[id]
		if newCount == 0 || oldCount <= newCount {
			continue
		}
		overage := oldCount - newCount
		for i := 0; i < overage; i++ {
			r.removeModelRegistration(clientID, id, oldProvider, now)
		}
	}

	// Apply additions.
	for id, newCount := range newCounts {
		oldCount := oldCounts[id]
		if newCount <= oldCount {
			continue
		}
		model := newModels[id]
		diff := newCount - oldCount
		for i := 0; i < diff; i++ {
			r.addModelRegistration(id, provider, model, now, clientID)
		}
	}

	// Update metadata for models that remain associated with the client.
	addedSet := make(map[string]struct{}, len(added))
	for _, id := range added {
		addedSet[id] = struct{}{}
	}
	for _, id := range uniqueModelIDs {
		model := newModels[id]
		if reg, ok := r.models[id]; ok {
			// Capability propagation (upstream 60e5b8bd432e): re-registering a
			// client must not clobber another client's declared capability in the
			// aggregate views. Upstream ORs a dynamic SupportsWebSearch flag; the
			// fork merges the static NativeCapabilities.WebSearch tri-state
			// across the remaining clients' stored model infos.
			reg.Info = cloneModelInfo(model)
			setModelInfoWebSearch(reg.Info, r.mergedWebSearchCapabilityLocked(id, "", clientID, webSearchCapabilityOf(model)))
			if provider != "" {
				if reg.InfoByProvider == nil {
					reg.InfoByProvider = make(map[string]*ModelInfo)
				}
				reg.InfoByProvider[provider] = cloneModelInfo(model)
				setModelInfoWebSearch(reg.InfoByProvider[provider], r.mergedWebSearchCapabilityLocked(id, provider, clientID, webSearchCapabilityOf(model)))
			}
			if providerChanged && oldProvider != "" && reg.InfoByProvider != nil && reg.InfoByProvider[oldProvider] != nil {
				setModelInfoWebSearch(reg.InfoByProvider[oldProvider], r.mergedWebSearchCapabilityLocked(id, oldProvider, clientID, nil))
			}
			reg.LastUpdated = now
			// Re-registering an existing client/model binding starts a fresh registry
			// snapshot for that binding. Cooldown and suspension are transient
			// scheduling state and must not survive this reconciliation step.
			if reg.QuotaExceededClients != nil {
				delete(reg.QuotaExceededClients, clientID)
			}
			if reg.SuspendedClients != nil {
				delete(reg.SuspendedClients, clientID)
			}
			if providerChanged && provider != "" {
				if _, newlyAdded := addedSet[id]; newlyAdded {
					continue
				}
				overlapCount := newCounts[id]
				if oldCount := oldCounts[id]; oldCount < overlapCount {
					overlapCount = oldCount
				}
				if overlapCount <= 0 {
					continue
				}
				if reg.Providers == nil {
					reg.Providers = make(map[string]int)
				}
				reg.Providers[provider] += overlapCount
			}
		}
	}

	// Update client bookkeeping.
	if len(rawModelIDs) > 0 {
		r.clientModels[clientID] = append([]string(nil), rawModelIDs...)
	}
	// Update client's own model infos
	clientInfos := make(map[string]*ModelInfo, len(newModels))
	for id, m := range newModels {
		clientInfos[id] = cloneModelInfo(m)
	}
	r.clientModelInfos[clientID] = clientInfos
	if provider != "" {
		r.clientProviders[clientID] = provider
	} else {
		delete(r.clientProviders, clientID)
	}

	r.invalidateAvailableModelsCacheLocked()
	r.triggerModelsRegistered(provider, clientID, models)
	if len(added) == 0 && len(removed) == 0 && !providerChanged {
		// Only metadata (e.g., display name) changed; skip separator when no log output.
		return
	}

	log.Debugf("Reconciled client %s (provider %s) models: +%d, -%d", clientID, provider, len(added), len(removed))
	misc.LogCredentialSeparator()
}

func (r *ModelRegistry) addModelRegistration(modelID, provider string, model *ModelInfo, now time.Time, excludeClientID string) {
	if model == nil || modelID == "" {
		return
	}
	if existing, exists := r.models[modelID]; exists {
		existing.Count++
		existing.LastUpdated = now
		// Capability propagation (upstream 60e5b8bd432e): the aggregate view folds
		// the registering client's own declared capability together with every
		// other client's stored capability for this model.
		existing.Info = cloneModelInfo(model)
		setModelInfoWebSearch(existing.Info, r.mergedWebSearchCapabilityLocked(modelID, "", excludeClientID, webSearchCapabilityOf(model)))
		if existing.SuspendedClients == nil {
			existing.SuspendedClients = make(map[string]string)
		}
		if existing.InfoByProvider == nil {
			existing.InfoByProvider = make(map[string]*ModelInfo)
		}
		if provider != "" {
			if existing.Providers == nil {
				existing.Providers = make(map[string]int)
			}
			existing.Providers[provider]++
			existing.InfoByProvider[provider] = cloneModelInfo(model)
			setModelInfoWebSearch(existing.InfoByProvider[provider], r.mergedWebSearchCapabilityLocked(modelID, provider, excludeClientID, webSearchCapabilityOf(model)))
		}
		log.Debugf("Incremented count for model %s, now %d clients", modelID, existing.Count)
		return
	}

	registration := &ModelRegistration{
		Info:                 cloneModelInfo(model),
		InfoByProvider:       make(map[string]*ModelInfo),
		Count:                1,
		LastUpdated:          now,
		QuotaExceededClients: make(map[string]*time.Time),
		SuspendedClients:     make(map[string]string),
	}
	if provider != "" {
		registration.Providers = map[string]int{provider: 1}
		registration.InfoByProvider[provider] = cloneModelInfo(model)
	}
	r.models[modelID] = registration
	log.Debugf("Registered new model %s from provider %s", modelID, provider)
}

func (r *ModelRegistry) removeModelRegistration(clientID, modelID, provider string, now time.Time) {
	registration, exists := r.models[modelID]
	if !exists {
		return
	}
	registration.Count--
	registration.LastUpdated = now
	if registration.QuotaExceededClients != nil {
		delete(registration.QuotaExceededClients, clientID)
	}
	if registration.SuspendedClients != nil {
		delete(registration.SuspendedClients, clientID)
	}
	if registration.Count < 0 {
		registration.Count = 0
	}
	if provider != "" && registration.Providers != nil {
		if count, ok := registration.Providers[provider]; ok {
			if count <= 1 {
				delete(registration.Providers, provider)
				if registration.InfoByProvider != nil {
					delete(registration.InfoByProvider, provider)
				}
			} else {
				registration.Providers[provider] = count - 1
			}
		}
	}
	log.Debugf("Decremented count for model %s, now %d clients", modelID, registration.Count)
	if registration.Count <= 0 {
		delete(r.models, modelID)
		log.Debugf("Removed model %s as no clients remain", modelID)
	} else {
		// Capability propagation (upstream 60e5b8bd432e): the departing client's
		// stored infos are still present here, so exclude it explicitly and
		// recompute the aggregate capability over the remaining clients.
		if registration.Info != nil {
			setModelInfoWebSearch(registration.Info, r.mergedWebSearchCapabilityLocked(modelID, "", clientID, nil))
		}
		if provider != "" && registration.InfoByProvider != nil && registration.InfoByProvider[provider] != nil {
			setModelInfoWebSearch(registration.InfoByProvider[provider], r.mergedWebSearchCapabilityLocked(modelID, provider, clientID, nil))
		}
	}
}

func cloneModelInfo(model *ModelInfo) *ModelInfo {
	if model == nil {
		return nil
	}
	copyModel := *model
	if model.NativeCapabilities != nil {
		copyCapabilities := *model.NativeCapabilities
		if model.NativeCapabilities.WebSearch != nil {
			webSearch := *model.NativeCapabilities.WebSearch
			copyCapabilities.WebSearch = &webSearch
		}
		copyModel.NativeCapabilities = &copyCapabilities
	}
	if len(model.SupportedGenerationMethods) > 0 {
		copyModel.SupportedGenerationMethods = append([]string(nil), model.SupportedGenerationMethods...)
	}
	if len(model.SupportedParameters) > 0 {
		copyModel.SupportedParameters = append([]string(nil), model.SupportedParameters...)
	}
	if len(model.SupportedInputModalities) > 0 {
		copyModel.SupportedInputModalities = append([]string(nil), model.SupportedInputModalities...)
	}
	if len(model.SupportedOutputModalities) > 0 {
		copyModel.SupportedOutputModalities = append([]string(nil), model.SupportedOutputModalities...)
	}
	if model.Thinking != nil {
		copyThinking := *model.Thinking
		if len(model.Thinking.Levels) > 0 {
			copyThinking.Levels = append([]string(nil), model.Thinking.Levels...)
		}
		copyModel.Thinking = &copyThinking
	}
	if model.NativeCapabilities != nil {
		copyCapabilities := *model.NativeCapabilities
		if model.NativeCapabilities.WebSearch != nil {
			webSearch := *model.NativeCapabilities.WebSearch
			copyCapabilities.WebSearch = &webSearch
		}
		copyModel.NativeCapabilities = &copyCapabilities
	}
	return &copyModel
}

func cloneModelInfosUnique(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	cloned := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		cloned = append(cloned, cloneModelInfo(model))
	}
	return cloned
}

// UnregisterClient removes a client and decrements counts for its models
// Parameters:
//   - clientID: Unique identifier for the client to remove
func (r *ModelRegistry) UnregisterClient(clientID string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.unregisterClientInternal(clientID)
	r.invalidateAvailableModelsCacheLocked()
}

// unregisterClientInternal performs the actual client unregistration (internal, no locking)
func (r *ModelRegistry) unregisterClientInternal(clientID string) {
	models, exists := r.clientModels[clientID]
	provider, hasProvider := r.clientProviders[clientID]
	if !exists {
		if hasProvider {
			delete(r.clientProviders, clientID)
		}
		return
	}

	now := time.Now()
	for _, modelID := range models {
		if registration, isExists := r.models[modelID]; isExists {
			registration.Count--
			registration.LastUpdated = now

			// Remove quota tracking for this client
			delete(registration.QuotaExceededClients, clientID)
			if registration.SuspendedClients != nil {
				delete(registration.SuspendedClients, clientID)
			}

			if hasProvider && registration.Providers != nil {
				if count, ok := registration.Providers[provider]; ok {
					if count <= 1 {
						delete(registration.Providers, provider)
						if registration.InfoByProvider != nil {
							delete(registration.InfoByProvider, provider)
						}
					} else {
						registration.Providers[provider] = count - 1
					}
				}
			}

			log.Debugf("Decremented count for model %s, now %d clients", modelID, registration.Count)

			// Remove model if no clients remain
			if registration.Count <= 0 {
				delete(r.models, modelID)
				log.Debugf("Removed model %s as no clients remain", modelID)
			} else {
				// Capability propagation (upstream 60e5b8bd432e): drop the
				// unregistered client's contribution from the aggregate views.
				if registration.Info != nil {
					setModelInfoWebSearch(registration.Info, r.mergedWebSearchCapabilityLocked(modelID, "", clientID, nil))
				}
				if hasProvider && registration.InfoByProvider != nil && registration.InfoByProvider[provider] != nil {
					setModelInfoWebSearch(registration.InfoByProvider[provider], r.mergedWebSearchCapabilityLocked(modelID, provider, clientID, nil))
				}
			}
		}
	}

	if r.clientGenerations == nil {
		r.clientGenerations = make(map[string]uint64)
	}
	if r.clientEpochs == nil {
		r.clientEpochs = make(map[string]uint64)
	}
	r.clientEpochs[clientID]++
	r.clientGenerations[clientID] = uint64(0)

	delete(r.clientModels, clientID)
	delete(r.clientModelInfos, clientID)
	if hasProvider {
		delete(r.clientProviders, clientID)
	}
	log.Debugf("Unregistered client %s", clientID)
	// Separator line after completing client unregistration (after the summary line)
	misc.LogCredentialSeparator()
	r.triggerModelsUnregistered(provider, clientID)
}

// ClientModelProjection describes the desired availability and quota state for a client's model.
type ClientModelProjection struct {
	ModelID       string
	Suspended     bool
	SuspendReason string
	QuotaExceeded bool
}

// ApplyClientModelProjections applies model state projections for clientID atomically.
func (r *ModelRegistry) ApplyClientModelProjections(clientID string, epoch uint64, generation uint64, projections []ClientModelProjection) bool {
	if r == nil {
		return false
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return false
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.clientEpochs == nil {
		r.clientEpochs = make(map[string]uint64)
	}
	if r.clientGenerations == nil {
		r.clientGenerations = make(map[string]uint64)
	}

	clientRegisteredModels, exists := r.clientModels[clientID]
	if !exists || len(clientRegisteredModels) == 0 {
		return false
	}

	currentEpoch := r.clientEpochs[clientID]
	if epoch != currentEpoch {
		return false
	}

	currentGen := r.clientGenerations[clientID]
	if generation < currentGen {
		return false
	}

	registeredSet := make(map[string]struct{}, len(clientRegisteredModels))
	for _, m := range clientRegisteredModels {
		trimmedModel := strings.TrimSpace(m)
		if trimmedModel != "" {
			registeredSet[trimmedModel] = struct{}{}
		}
	}

	hasValidModel := false
	for _, proj := range projections {
		modelID := strings.TrimSpace(proj.ModelID)
		if modelID == "" {
			continue
		}
		if _, isOwned := registeredSet[modelID]; isOwned {
			if registration, existsModel := r.models[modelID]; existsModel && registration != nil {
				hasValidModel = true
				break
			}
		}
	}
	if !hasValidModel {
		return false
	}

	r.clientGenerations[clientID] = generation
	r.ensureAvailableModelsCacheLocked()

	now := time.Now()
	changed := false
	for _, proj := range projections {
		modelID := strings.TrimSpace(proj.ModelID)
		if modelID == "" {
			continue
		}
		if _, isOwned := registeredSet[modelID]; !isOwned {
			continue
		}
		registration, existsModel := r.models[modelID]
		if !existsModel || registration == nil {
			continue
		}

		// Update suspended state
		if proj.Suspended {
			if registration.SuspendedClients == nil {
				registration.SuspendedClients = make(map[string]string)
			}
			if currentReason, ok := registration.SuspendedClients[clientID]; !ok || currentReason != proj.SuspendReason {
				registration.SuspendedClients[clientID] = proj.SuspendReason
				registration.LastUpdated = now
				changed = true
			}
		} else {
			if registration.SuspendedClients != nil {
				if _, ok := registration.SuspendedClients[clientID]; ok {
					delete(registration.SuspendedClients, clientID)
					registration.LastUpdated = now
					changed = true
				}
			}
		}

		// Update quota exceeded state
		if proj.QuotaExceeded {
			if registration.QuotaExceededClients == nil {
				registration.QuotaExceededClients = make(map[string]*time.Time)
			}
			if _, ok := registration.QuotaExceededClients[clientID]; !ok {
				registration.QuotaExceededClients[clientID] = &now
				registration.LastUpdated = now
				changed = true
			}
		} else {
			if registration.QuotaExceededClients != nil {
				if _, ok := registration.QuotaExceededClients[clientID]; ok {
					delete(registration.QuotaExceededClients, clientID)
					registration.LastUpdated = now
					changed = true
				}
			}
		}
	}

	if changed {
		r.invalidateAvailableModelsCacheLocked()
	}
	return true
}

// ApplyClientModelCapabilities applies capability mutations to matching models of clientID
// if the client is currently registered and its registration epoch matches expectedEpoch.
// Returns true if applied, false if client is unregistered or epoch changed.
//
// Ported from upstream CLIProxyAPI commit 60e5b8bd432e
// (internal/registry/model_registry.go). Upstream's dynamic SupportsWebSearch flag
// maps onto the fork's static NativeCapabilities.WebSearch tri-state; the caller
// mutates the client's stored model infos and the aggregate registration views are
// recomputed across every remaining client.
func (r *ModelRegistry) ApplyClientModelCapabilities(clientID string, expectedEpoch uint64, mutate func(modelID string, info *ModelInfo)) bool {
	if r == nil || mutate == nil {
		return false
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return false
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.clientEpochs == nil || r.clientEpochs[clientID] != expectedEpoch {
		return false
	}
	clientInfos, exists := r.clientModelInfos[clientID]
	if !exists || len(clientInfos) == 0 {
		return false
	}

	provider := r.clientProviders[clientID]
	for id, info := range clientInfos {
		if info != nil {
			mutate(id, info)
			if reg, okReg := r.models[id]; okReg && reg != nil {
				if reg.Info != nil {
					setModelInfoWebSearch(reg.Info, r.mergedWebSearchCapabilityLocked(id, "", "", nil))
				}
				if provider != "" && reg.InfoByProvider != nil && reg.InfoByProvider[provider] != nil {
					setModelInfoWebSearch(reg.InfoByProvider[provider], r.mergedWebSearchCapabilityLocked(id, provider, "", nil))
				}
			}
		}
	}
	r.invalidateAvailableModelsCacheLocked()
	return true
}

// webSearchCapabilityOf returns the declared WebSearch tri-state of a model info.
func webSearchCapabilityOf(model *ModelInfo) *bool {
	if model == nil || model.NativeCapabilities == nil {
		return nil
	}
	return model.NativeCapabilities.WebSearch
}

// setModelInfoWebSearch writes the aggregate WebSearch tri-state onto info's
// NativeCapabilities, allocating the capabilities holder when needed.
func setModelInfoWebSearch(info *ModelInfo, webSearch *bool) {
	if info == nil {
		return
	}
	if info.NativeCapabilities == nil {
		if webSearch == nil {
			return
		}
		info.NativeCapabilities = &NativeCapabilities{}
	}
	info.NativeCapabilities.WebSearch = webSearch
}

// mergedWebSearchCapabilityLocked recomputes the aggregate WebSearch tri-state
// for modelID (optionally scoped to provider) across every registered client's
// stored model infos, excluding excludeClientID and folding in base, the
// registering client's own declared value which is not yet stored.
// Any explicit WebSearch=true wins (matching upstream's OR semantics); otherwise
// an explicit false is preserved; otherwise the capability stays unknown (nil).
// Callers must hold r.mutex.
// Ported from upstream CLIProxyAPI commit 60e5b8bd432e
// (internal/registry/model_registry.go hasClientSupportingWebSearchLocked).
func (r *ModelRegistry) mergedWebSearchCapabilityLocked(modelID, provider, excludeClientID string, base *bool) *bool {
	sawFalse := false
	if base != nil {
		if *base {
			return boolPointer(true)
		}
		sawFalse = true
	}
	for cID, infos := range r.clientModelInfos {
		if cID == excludeClientID {
			continue
		}
		if provider != "" && r.clientProviders[cID] != provider {
			continue
		}
		if info, ok := infos[modelID]; ok && info != nil {
			if declared := webSearchCapabilityOf(info); declared != nil {
				if *declared {
					return boolPointer(true)
				}
				sawFalse = true
			}
		}
	}
	if sawFalse {
		return boolPointer(false)
	}
	return nil
}

// IsModelSuspendedForClient reports whether a model is currently suspended for a specific client.
func (r *ModelRegistry) IsModelSuspendedForClient(clientID, modelID string) bool {
	clientID = strings.TrimSpace(clientID)
	modelID = strings.TrimSpace(modelID)
	if clientID == "" || modelID == "" {
		return false
	}

	r.mutex.RLock()
	defer r.mutex.RUnlock()

	registration, exists := r.models[modelID]
	if !exists || registration == nil || registration.SuspendedClients == nil {
		return false
	}
	_, suspended := registration.SuspendedClients[clientID]
	return suspended
}

// GetModelsAndEpochForClient returns the models registered for a specific client along with its registration epoch.
func (r *ModelRegistry) GetModelsAndEpochForClient(clientID string) ([]*ModelInfo, uint64) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	var epoch uint64
	if r.clientEpochs != nil {
		epoch = r.clientEpochs[clientID]
	}

	modelIDs, exists := r.clientModels[clientID]
	if !exists || len(modelIDs) == 0 {
		return nil, epoch
	}

	clientInfos := r.clientModelInfos[clientID]
	seen := make(map[string]struct{})
	result := make([]*ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		if _, dup := seen[modelID]; dup {
			continue
		}
		seen[modelID] = struct{}{}

		if clientInfos != nil {
			if info, okInfo := clientInfos[modelID]; okInfo && info != nil {
				result = append(result, cloneModelInfo(info))
				continue
			}
		}
		if reg, okReg := r.models[modelID]; okReg && reg.Info != nil {
			result = append(result, cloneModelInfo(reg.Info))
		}
	}
	return result, epoch
}

// ClientRegistrationEpoch returns the current registration epoch for a client.
func (r *ModelRegistry) ClientRegistrationEpoch(clientID string) uint64 {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	if r.clientEpochs == nil {
		return 0
	}
	return r.clientEpochs[clientID]
}

// SetModelQuotaExceeded marks a model as quota exceeded for a specific client
// Parameters:
//   - clientID: The client that exceeded quota
//   - modelID: The model that exceeded quota
func (r *ModelRegistry) SetModelQuotaExceeded(clientID, modelID string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.ensureAvailableModelsCacheLocked()

	if registration, exists := r.models[modelID]; exists {
		now := time.Now()
		registration.QuotaExceededClients[clientID] = &now
		r.invalidateAvailableModelsCacheLocked()
		log.Debugf("Marked model %s as quota exceeded for client %s", modelID, clientID)
	}
}

// ClearModelQuotaExceeded removes quota exceeded status for a model and client
// Parameters:
//   - clientID: The client to clear quota status for
//   - modelID: The model to clear quota status for
func (r *ModelRegistry) ClearModelQuotaExceeded(clientID, modelID string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.ensureAvailableModelsCacheLocked()

	if registration, exists := r.models[modelID]; exists {
		delete(registration.QuotaExceededClients, clientID)
		r.invalidateAvailableModelsCacheLocked()
		// log.Debugf("Cleared quota exceeded status for model %s and client %s", modelID, clientID)
	}
}

// SuspendClientModel marks a client's model as temporarily unavailable until explicitly resumed.
// Parameters:
//   - clientID: The client to suspend
//   - modelID: The model affected by the suspension
//   - reason: Optional description for observability
func (r *ModelRegistry) SuspendClientModel(clientID, modelID, reason string) {
	if clientID == "" || modelID == "" {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.ensureAvailableModelsCacheLocked()

	registration, exists := r.models[modelID]
	if !exists || registration == nil {
		return
	}
	if registration.SuspendedClients == nil {
		registration.SuspendedClients = make(map[string]string)
	}
	if _, already := registration.SuspendedClients[clientID]; already {
		return
	}
	registration.SuspendedClients[clientID] = reason
	registration.LastUpdated = time.Now()
	r.invalidateAvailableModelsCacheLocked()
	if reason != "" {
		log.Debugf("Suspended client %s for model %s: %s", clientID, modelID, reason)
	} else {
		log.Debugf("Suspended client %s for model %s", clientID, modelID)
	}
}

// ResumeClientModel clears a previous suspension so the client counts toward availability again.
// Parameters:
//   - clientID: The client to resume
//   - modelID: The model being resumed
func (r *ModelRegistry) ResumeClientModel(clientID, modelID string) {
	if clientID == "" || modelID == "" {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.ensureAvailableModelsCacheLocked()

	registration, exists := r.models[modelID]
	if !exists || registration == nil || registration.SuspendedClients == nil {
		return
	}
	if _, ok := registration.SuspendedClients[clientID]; !ok {
		return
	}
	delete(registration.SuspendedClients, clientID)
	registration.LastUpdated = time.Now()
	r.invalidateAvailableModelsCacheLocked()
	log.Debugf("Resumed client %s for model %s", clientID, modelID)
}

// ClientSupportsModel reports whether the client registered support for modelID.
func (r *ModelRegistry) ClientSupportsModel(clientID, modelID string) bool {
	clientID = strings.TrimSpace(clientID)
	modelID = strings.TrimSpace(modelID)
	if clientID == "" || modelID == "" {
		return false
	}

	r.mutex.RLock()
	defer r.mutex.RUnlock()

	models, exists := r.clientModels[clientID]
	if !exists || len(models) == 0 {
		return false
	}

	for _, id := range models {
		if strings.EqualFold(strings.TrimSpace(id), modelID) {
			return true
		}
	}

	return false
}

// GetAvailableModels returns all models that have at least one available client
// Parameters:
//   - handlerType: The handler type to filter models for (e.g., "openai", "claude", "gemini")
//
// Returns:
//   - []map[string]any: List of available models in the requested format
func (r *ModelRegistry) GetAvailableModels(handlerType string) []map[string]any {
	now := time.Now()

	r.mutex.RLock()
	if cache, ok := r.availableModelsCache[handlerType]; ok && (cache.expiresAt.IsZero() || now.Before(cache.expiresAt)) {
		models := cloneModelMaps(cache.models)
		r.mutex.RUnlock()
		return models
	}
	r.mutex.RUnlock()

	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.ensureAvailableModelsCacheLocked()

	if cache, ok := r.availableModelsCache[handlerType]; ok && (cache.expiresAt.IsZero() || now.Before(cache.expiresAt)) {
		return cloneModelMaps(cache.models)
	}

	models, expiresAt := r.buildAvailableModelsLocked(handlerType, now)
	r.availableModelsCache[handlerType] = availableModelsCacheEntry{
		models:    cloneModelMaps(models),
		expiresAt: expiresAt,
	}

	return models
}

func (r *ModelRegistry) buildAvailableModelsLocked(handlerType string, now time.Time) ([]map[string]any, time.Time) {
	models := make([]map[string]any, 0, len(r.models))
	var expiresAt time.Time

	for _, registration := range r.models {
		availableClients := registration.Count

		expiredClients := 0
		for _, quotaTime := range registration.QuotaExceededClients {
			if quotaTime == nil {
				continue
			}
			recoveryAt := quotaTime.Add(modelQuotaExceededWindow)
			if now.Before(recoveryAt) {
				expiredClients++
				if expiresAt.IsZero() || recoveryAt.Before(expiresAt) {
					expiresAt = recoveryAt
				}
			}
		}

		cooldownSuspended := 0
		otherSuspended := 0
		quotaAndOtherSuspended := 0
		if registration.SuspendedClients != nil {
			for clientID, reason := range registration.SuspendedClients {
				if strings.EqualFold(reason, "quota") {
					cooldownSuspended++
					continue
				}
				otherSuspended++
				if quotaTime := registration.QuotaExceededClients[clientID]; quotaTime != nil && now.Before(quotaTime.Add(modelQuotaExceededWindow)) {
					quotaAndOtherSuspended++
				}
			}
		}

		// A credential-wide quota can mark the same client both quota-exceeded and
		// suspended. Count that unavailable client only once.
		effectiveClients := availableClients - expiredClients - otherSuspended + quotaAndOtherSuspended
		if effectiveClients < 0 {
			effectiveClients = 0
		}

		if effectiveClients > 0 || (availableClients > 0 && (expiredClients > 0 || cooldownSuspended > 0) && otherSuspended == 0) {
			model := r.convertModelToMap(registration.Info, handlerType)
			if model != nil {
				models = append(models, model)
			}
		}
	}

	return models, expiresAt
}

func cloneModelMaps(models []map[string]any) []map[string]any {
	cloned := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if model == nil {
			cloned = append(cloned, nil)
			continue
		}
		copyModel := make(map[string]any, len(model))
		for key, value := range model {
			copyModel[key] = cloneModelMapValue(value)
		}
		cloned = append(cloned, copyModel)
	}
	return cloned
}

func cloneModelMapValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copyMap := make(map[string]any, len(typed))
		for key, entry := range typed {
			copyMap[key] = cloneModelMapValue(entry)
		}
		return copyMap
	case []any:
		copySlice := make([]any, len(typed))
		for i, entry := range typed {
			copySlice[i] = cloneModelMapValue(entry)
		}
		return copySlice
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

// GetAvailableModelsByProvider returns models available for the given provider identifier.
// Parameters:
//   - provider: Provider identifier (e.g., "codex", "gemini", "antigravity")
//
// Returns:
//   - []*ModelInfo: List of available models for the provider
func (r *ModelRegistry) GetAvailableModelsByProvider(provider string) []*ModelInfo {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil
	}

	r.mutex.RLock()
	defer r.mutex.RUnlock()

	type providerModel struct {
		count int
		info  *ModelInfo
	}

	providerModels := make(map[string]*providerModel)

	for clientID, clientProvider := range r.clientProviders {
		if clientProvider != provider {
			continue
		}
		modelIDs := r.clientModels[clientID]
		if len(modelIDs) == 0 {
			continue
		}
		clientInfos := r.clientModelInfos[clientID]
		for _, modelID := range modelIDs {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			entry := providerModels[modelID]
			if entry == nil {
				entry = &providerModel{}
				providerModels[modelID] = entry
			}
			entry.count++
			if entry.info == nil {
				if clientInfos != nil {
					if info := clientInfos[modelID]; info != nil {
						entry.info = info
					}
				}
				if entry.info == nil {
					if reg, ok := r.models[modelID]; ok && reg != nil && reg.Info != nil {
						entry.info = reg.Info
					}
				}
			}
		}
	}

	if len(providerModels) == 0 {
		return nil
	}

	now := time.Now()
	result := make([]*ModelInfo, 0, len(providerModels))

	for modelID, entry := range providerModels {
		if entry == nil || entry.count <= 0 {
			continue
		}
		registration, ok := r.models[modelID]

		expiredClients := 0
		cooldownSuspended := 0
		otherSuspended := 0
		quotaAndOtherSuspended := 0
		if ok && registration != nil {
			if registration.QuotaExceededClients != nil {
				for clientID, quotaTime := range registration.QuotaExceededClients {
					if clientID == "" {
						continue
					}
					if p, okProvider := r.clientProviders[clientID]; !okProvider || p != provider {
						continue
					}
					if quotaTime != nil && now.Sub(*quotaTime) < modelQuotaExceededWindow {
						expiredClients++
					}
				}
			}
			if registration.SuspendedClients != nil {
				for clientID, reason := range registration.SuspendedClients {
					if clientID == "" {
						continue
					}
					if p, okProvider := r.clientProviders[clientID]; !okProvider || p != provider {
						continue
					}
					if strings.EqualFold(reason, "quota") {
						cooldownSuspended++
						continue
					}
					otherSuspended++
					if quotaTime := registration.QuotaExceededClients[clientID]; quotaTime != nil && now.Before(quotaTime.Add(modelQuotaExceededWindow)) {
						quotaAndOtherSuspended++
					}
				}
			}
		}

		availableClients := entry.count
		effectiveClients := availableClients - expiredClients - otherSuspended + quotaAndOtherSuspended
		if effectiveClients < 0 {
			effectiveClients = 0
		}

		if effectiveClients > 0 || (availableClients > 0 && (expiredClients > 0 || cooldownSuspended > 0) && otherSuspended == 0) {
			if entry.info != nil {
				result = append(result, cloneModelInfo(entry.info))
				continue
			}
			if ok && registration != nil && registration.Info != nil {
				result = append(result, cloneModelInfo(registration.Info))
			}
		}
	}

	return result
}

// GetModelCount returns the number of available clients for a specific model
// Parameters:
//   - modelID: The model ID to check
//
// Returns:
//   - int: Number of available clients for the model
func (r *ModelRegistry) GetModelCount(modelID string) int {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	if registration, exists := r.models[modelID]; exists {
		now := time.Now()

		// Count clients that have exceeded quota but haven't recovered yet
		expiredClients := 0
		for _, quotaTime := range registration.QuotaExceededClients {
			if quotaTime != nil && now.Sub(*quotaTime) < modelQuotaExceededWindow {
				expiredClients++
			}
		}
		suspendedClients := 0
		for clientID := range registration.SuspendedClients {
			if quotaTime := registration.QuotaExceededClients[clientID]; quotaTime != nil && now.Before(quotaTime.Add(modelQuotaExceededWindow)) {
				continue
			}
			suspendedClients++
		}
		result := registration.Count - expiredClients - suspendedClients
		if result < 0 {
			return 0
		}
		return result
	}
	return 0
}

// GetModelProviders returns provider identifiers that currently supply the given model
// Parameters:
//   - modelID: The model ID to check
//
// Returns:
//   - []string: Provider identifiers ordered by availability count (descending)
func (r *ModelRegistry) GetModelProviders(modelID string) []string {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	registration, exists := r.models[modelID]
	if !exists || registration == nil || len(registration.Providers) == 0 {
		return nil
	}

	type providerCount struct {
		name  string
		count int
	}
	providers := make([]providerCount, 0, len(registration.Providers))
	// suspendedByProvider := make(map[string]int)
	// if registration.SuspendedClients != nil {
	// 	for clientID := range registration.SuspendedClients {
	// 		if provider, ok := r.clientProviders[clientID]; ok && provider != "" {
	// 			suspendedByProvider[provider]++
	// 		}
	// 	}
	// }
	for name, count := range registration.Providers {
		if count <= 0 {
			continue
		}
		// adjusted := count - suspendedByProvider[name]
		// if adjusted <= 0 {
		// 	continue
		// }
		// providers = append(providers, providerCount{name: name, count: adjusted})
		providers = append(providers, providerCount{name: name, count: count})
	}
	if len(providers) == 0 {
		return nil
	}

	sort.Slice(providers, func(i, j int) bool {
		if providers[i].count == providers[j].count {
			return providers[i].name < providers[j].name
		}
		return providers[i].count > providers[j].count
	})

	result := make([]string, 0, len(providers))
	for _, item := range providers {
		result = append(result, item.name)
	}
	return result
}

// GetModelInfo returns ModelInfo, prioritizing provider-specific definition if available.
func (r *ModelRegistry) GetModelInfo(modelID, provider string) *ModelInfo {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	if reg, ok := r.models[modelID]; ok && reg != nil {
		// Try provider specific definition first
		if provider != "" && reg.InfoByProvider != nil {
			if reg.Providers != nil {
				if count, ok := reg.Providers[provider]; ok && count > 0 {
					if info, ok := reg.InfoByProvider[provider]; ok && info != nil {
						return cloneModelInfo(info)
					}
				}
			}
		}
		// Fallback to global info (last registered)
		return cloneModelInfo(reg.Info)
	}
	return nil
}

// convertModelToMap converts ModelInfo to the appropriate format for different handler types
func (r *ModelRegistry) convertModelToMap(model *ModelInfo, handlerType string) map[string]any {
	if model == nil {
		return nil
	}

	switch handlerType {
	case "openai":
		result := map[string]any{
			"id":       model.ID,
			"object":   "model",
			"owned_by": model.OwnedBy,
		}
		if model.Created > 0 {
			result["created"] = model.Created
		}
		if model.Type != "" {
			result["type"] = model.Type
		}
		if model.DisplayName != "" {
			result["display_name"] = model.DisplayName
		}
		if model.Version != "" {
			result["version"] = model.Version
		}
		if model.Description != "" {
			result["description"] = model.Description
		}
		if model.ContextLength > 0 {
			result["context_length"] = model.ContextLength
		}
		if model.MaxContextLength > 0 {
			result["max_context_length"] = model.MaxContextLength
		}
		if model.MaxCompletionTokens > 0 {
			result["max_completion_tokens"] = model.MaxCompletionTokens
		}
		if len(model.SupportedParameters) > 0 {
			result["supported_parameters"] = append([]string(nil), model.SupportedParameters...)
		}
		return result

	case "claude":
		result := map[string]any{
			"id":       model.ID,
			"object":   "model",
			"owned_by": model.OwnedBy,
		}
		if model.Created > 0 {
			result["created_at"] = model.Created
		}
		if model.Type != "" {
			result["type"] = "model"
		}
		if model.DisplayName != "" {
			result["display_name"] = model.DisplayName
		} else {
			result["display_name"] = model.ID
		}
		return result

	case "gemini", "gemini-cli":
		result := map[string]any{}
		if model.Name != "" {
			result["name"] = model.Name
		} else {
			result["name"] = model.ID
		}
		if model.Version != "" {
			result["version"] = model.Version
		}
		if model.DisplayName != "" {
			result["displayName"] = model.DisplayName
		}
		if model.Description != "" {
			result["description"] = model.Description
		}
		if model.InputTokenLimit > 0 {
			result["inputTokenLimit"] = model.InputTokenLimit
		}
		if model.OutputTokenLimit > 0 {
			result["outputTokenLimit"] = model.OutputTokenLimit
		}
		if len(model.SupportedGenerationMethods) > 0 {
			result["supportedGenerationMethods"] = append([]string(nil), model.SupportedGenerationMethods...)
		}
		if len(model.SupportedInputModalities) > 0 {
			result["supportedInputModalities"] = append([]string(nil), model.SupportedInputModalities...)
		}
		if len(model.SupportedOutputModalities) > 0 {
			result["supportedOutputModalities"] = append([]string(nil), model.SupportedOutputModalities...)
		}
		return result

	default:
		// Generic format
		result := map[string]any{
			"id":     model.ID,
			"object": "model",
		}
		if model.OwnedBy != "" {
			result["owned_by"] = model.OwnedBy
		}
		if model.Type != "" {
			result["type"] = model.Type
		}
		if model.Created != 0 {
			result["created"] = model.Created
		}
		return result
	}
}

// CleanupExpiredQuotas removes expired quota tracking entries
func (r *ModelRegistry) CleanupExpiredQuotas() {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	now := time.Now()
	invalidated := false

	for modelID, registration := range r.models {
		for clientID, quotaTime := range registration.QuotaExceededClients {
			if quotaTime != nil && now.Sub(*quotaTime) >= modelQuotaExceededWindow {
				delete(registration.QuotaExceededClients, clientID)
				invalidated = true
				log.Debugf("Cleaned up expired quota tracking for model %s, client %s", modelID, clientID)
			}
		}
	}
	if invalidated {
		r.invalidateAvailableModelsCacheLocked()
	}
}

// GetFirstAvailableModel returns the first available model for the given handler type.
// It prioritizes models by their creation timestamp (newest first) and checks if they have
// available clients that are not suspended or over quota.
//
// Parameters:
//   - handlerType: The API handler type (e.g., "openai", "claude", "gemini")
//
// Returns:
//   - string: The model ID of the first available model, or empty string if none available
//   - error: An error if no models are available
func (r *ModelRegistry) GetFirstAvailableModel(handlerType string) (string, error) {

	// Get all available models for this handler type
	models := r.GetAvailableModels(handlerType)
	if len(models) == 0 {
		return "", fmt.Errorf("no models available for handler type: %s", handlerType)
	}

	// Sort models by creation timestamp (newest first)
	sort.Slice(models, func(i, j int) bool {
		// Extract created timestamps from map
		createdI, okI := models[i]["created"].(int64)
		createdJ, okJ := models[j]["created"].(int64)
		if !okI || !okJ {
			return false
		}
		return createdI > createdJ
	})

	// Find the first model with available clients
	for _, model := range models {
		if modelID, ok := model["id"].(string); ok {
			if count := r.GetModelCount(modelID); count > 0 {
				return modelID, nil
			}
		}
	}

	return "", fmt.Errorf("no available clients for any model in handler type: %s", handlerType)
}

// GetModelsForClient returns the models registered for a specific client.
// Parameters:
//   - clientID: The client identifier (typically auth file name or auth ID)
//
// Returns:
//   - []*ModelInfo: List of models registered for this client, nil if client not found
func (r *ModelRegistry) GetModelsForClient(clientID string) []*ModelInfo {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	modelIDs, exists := r.clientModels[clientID]
	if !exists || len(modelIDs) == 0 {
		return nil
	}

	// Try to use client-specific model infos first
	clientInfos := r.clientModelInfos[clientID]

	seen := make(map[string]struct{})
	result := make([]*ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		if _, dup := seen[modelID]; dup {
			continue
		}
		seen[modelID] = struct{}{}

		// Prefer client's own model info to preserve original type/owned_by
		if clientInfos != nil {
			if info, ok := clientInfos[modelID]; ok && info != nil {
				result = append(result, cloneModelInfo(info))
				continue
			}
		}
		// Fallback to global registry (for backwards compatibility)
		if reg, ok := r.models[modelID]; ok && reg.Info != nil {
			result = append(result, cloneModelInfo(reg.Info))
		}
	}
	return result
}
