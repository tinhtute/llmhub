package synthesizer

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/watcher/diff"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// ConfigSynthesizer generates Auth entries from configuration API keys.
// It handles Gemini, Claude, Codex, OpenAI-compat, and Vertex-compat providers.
type ConfigSynthesizer struct{}

// NewConfigSynthesizer creates a new ConfigSynthesizer instance.
func NewConfigSynthesizer() *ConfigSynthesizer {
	return &ConfigSynthesizer{}
}

func addConfigWeightToAttrs(weight int64, attrs map[string]string) {
	if attrs == nil || weight <= 0 || weight == coreauth.DefaultCredentialWeight {
		return
	}
	attrs["weight"] = strconv.FormatInt(weight, 10)
}

// authProxyConnectionPool converts a per-credential pool override from config into
// the SDK representation carried on the auth record. Values are copied so the auth
// snapshot never aliases a live config field.
// Ported from upstream CLIProxyAPI internal/watcher/synthesizer/config.go (bf834311e6d6).
func authProxyConnectionPool(pool *config.ProxyConnectionPoolConfig) *coreauth.ProxyConnectionPoolOverride {
	if pool == nil {
		return nil
	}
	out := &coreauth.ProxyConnectionPoolOverride{IdleConnTimeout: pool.IdleConnTimeout}
	if pool.Enabled != nil {
		enabled := *pool.Enabled
		out.Enabled = &enabled
	}
	if pool.MaxIdleConnsPerHost != nil {
		maxIdle := *pool.MaxIdleConnsPerHost
		out.MaxIdleConnsPerHost = &maxIdle
	}
	// A block without fields is still the opt-in: it asks for the pool defaults. It
	// must therefore stay non-nil, or an operator writing `connection-pool: {}` (or
	// inheriting an empty group block) would silently get no pooling at all.
	// Upstream CLIProxyAPI 638f5f55e0d9.
	return out
}

// OpenAICompatConnectionPool resolves the effective connection-pool config for an
// OpenAI-compatible entry (exported for management auth-index mapping).
// Upstream CLIProxyAPI 8643ea56d520.
func OpenAICompatConnectionPool(compat *config.OpenAICompatibility, entry *config.OpenAICompatibilityAPIKey) *config.ProxyConnectionPoolConfig {
	return openAICompatConnectionPool(compat, entry)
}

// openAICompatConnectionPool resolves the pool override for one OpenAI-compatible
// key: the per-key block wins field by field, so a key that overrides only one value
// keeps inheriting the provider-group default for the rest.
func openAICompatConnectionPool(compat *config.OpenAICompatibility, entry *config.OpenAICompatibilityAPIKey) *config.ProxyConnectionPoolConfig {
	var group *config.ProxyConnectionPoolConfig
	if compat != nil {
		group = compat.ConnectionPool
	}
	if entry == nil || entry.ConnectionPool == nil {
		return group
	}
	merged := *entry.ConnectionPool
	if group != nil {
		if merged.Enabled == nil {
			merged.Enabled = group.Enabled
		}
		if strings.TrimSpace(merged.IdleConnTimeout) == "" {
			merged.IdleConnTimeout = group.IdleConnTimeout
		}
		if merged.MaxIdleConnsPerHost == nil {
			merged.MaxIdleConnsPerHost = group.MaxIdleConnsPerHost
		}
	}
	return &merged
}

// Synthesize generates Auth entries from config API keys.
func (s *ConfigSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 32)
	if ctx == nil || ctx.Config == nil {
		return out, nil
	}

	// Gemini API Keys
	out = append(out, s.synthesizeGeminiKeys(ctx)...)
	// Claude API Keys
	out = append(out, s.synthesizeClaudeKeys(ctx)...)
	// Codex API Keys
	out = append(out, s.synthesizeCodexKeys(ctx)...)
	// Meta API Keys
	out = append(out, s.synthesizeMetaKeys(ctx)...)
	// OpenAI-compat
	out = append(out, s.synthesizeOpenAICompat(ctx)...)
	// Vertex-compat
	out = append(out, s.synthesizeVertexCompat(ctx)...)

	return out, nil
}

// synthesizeGeminiKeys creates Auth entries for Gemini API keys.
func (s *ConfigSynthesizer) synthesizeGeminiKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.GeminiKey))
	for i := range cfg.GeminiKey {
		entry := cfg.GeminiKey[i]
		key := strings.TrimSpace(entry.APIKey)
		prefix := strings.TrimSpace(entry.Prefix)
		base := strings.TrimSpace(entry.BaseURL)
		if key == "" && base == "" {
			continue
		}
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		idParts := []string{key, base}
		if poolID := config.FormatConnectionPoolDedupID(entry.ConnectionPool); poolID != "" {
			idParts = append(idParts, poolID)
		}
		id, token := idGen.Next("gemini:apikey", idParts...)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:gemini[%s]", token),
			"config_index": strconv.Itoa(i),
		}
		if key != "" {
			attrs["api_key"] = key
		}
		metadata := map[string]any{}
		if entry.DisableCooling != nil {
			metadata["disable_cooling"] = *entry.DisableCooling
		}
		addRequestRetryToMetadata(entry.RequestRetry, metadata)
		addRequestScopedErrorsToMetadata(entry.RequestScopedErrors, metadata)
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		addConfigWeightToAttrs(entry.Weight, attrs)
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeGeminiModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		a := &coreauth.Auth{
			ID:                  id,
			Provider:            "gemini",
			Label:               "gemini-apikey",
			Prefix:              prefix,
			Status:              coreauth.StatusActive,
			ProxyURL:            proxyURL,
			Attributes:          attrs,
			Metadata:            metadata,
			CreatedAt:           now,
			UpdatedAt:           now,
			ProxyConnectionPool: authProxyConnectionPool(entry.ConnectionPool),
		}
		ApplyAuthExcludedModelsMeta(a, entry.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

// synthesizeClaudeKeys creates Auth entries for Claude API keys.
func (s *ConfigSynthesizer) synthesizeClaudeKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		ck := cfg.ClaudeKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		base := strings.TrimSpace(ck.BaseURL)
		idParts := []string{key, base}
		if poolID := config.FormatConnectionPoolDedupID(ck.ConnectionPool); poolID != "" {
			idParts = append(idParts, poolID)
		}
		id, token := idGen.Next("claude:apikey", idParts...)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:claude[%s]", token),
			"api_key":      key,
			"config_index": strconv.Itoa(i),
		}
		metadata := map[string]any{}
		if ck.DisableCooling != nil {
			metadata["disable_cooling"] = *ck.DisableCooling
		}
		addRequestRetryToMetadata(ck.RequestRetry, metadata)
		addRequestScopedErrorsToMetadata(ck.RequestScopedErrors, metadata)
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		addConfigWeightToAttrs(ck.Weight, attrs)
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeClaudeModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		a := &coreauth.Auth{
			ID:                  id,
			Provider:            "claude",
			Label:               "claude-apikey",
			Prefix:              prefix,
			Status:              coreauth.StatusActive,
			ProxyURL:            proxyURL,
			Attributes:          attrs,
			Metadata:            metadata,
			CreatedAt:           now,
			UpdatedAt:           now,
			ProxyConnectionPool: authProxyConnectionPool(ck.ConnectionPool),
		}
		ApplyAuthExcludedModelsMeta(a, ck.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

// synthesizeCodexKeys creates Auth entries for Codex API keys.
func (s *ConfigSynthesizer) synthesizeCodexKeys(ctx *SynthesisContext) []*coreauth.Auth {
	return s.synthesizeCodexStyleKeys(ctx, ctx.Config.CodexKey, "codex")
}

// synthesizeMetaKeys creates Auth entries for Meta API keys.
// Ported from upstream CLIProxyAPI commit 54d4f4c0 (the meta-api-key family
// shares the codex-style synthesis template; MetaKey = CodexKey).
func (s *ConfigSynthesizer) synthesizeMetaKeys(ctx *SynthesisContext) []*coreauth.Auth {
	return s.synthesizeCodexStyleKeys(ctx, ctx.Config.MetaKey, "meta")
}

func (s *ConfigSynthesizer) synthesizeCodexStyleKeys(ctx *SynthesisContext, entries []config.CodexKey, provider string) []*coreauth.Auth {
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(entries))
	for i := range entries {
		ck := entries[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		idParts := []string{key, ck.BaseURL}
		if poolID := config.FormatConnectionPoolDedupID(ck.ConnectionPool); poolID != "" {
			idParts = append(idParts, poolID)
		}
		id, token := idGen.Next(provider+":apikey", idParts...)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:%s[%s]", provider, token),
			"api_key":      key,
			"config_index": strconv.Itoa(i),
		}
		metadata := map[string]any{}
		if ck.DisableCooling != nil {
			metadata["disable_cooling"] = *ck.DisableCooling
		}
		addRequestRetryToMetadata(ck.RequestRetry, metadata)
		addRequestScopedErrorsToMetadata(ck.RequestScopedErrors, metadata)
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		addConfigWeightToAttrs(ck.Weight, attrs)
		if ck.BaseURL != "" {
			attrs["base_url"] = ck.BaseURL
		}
		if ck.Websockets {
			attrs["websockets"] = "true"
		}
		if provider == "codex" && ck.AlphaSearch {
			attrs["alpha_search"] = "true"
		}
		if provider == "codex" && ck.DisableCodexCloaking != nil {
			attrs[coreauth.AttributeCodexDisableCloaking] = strconv.FormatBool(*ck.DisableCodexCloaking)
		}
		if hash := diff.ComputeCodexModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		a := &coreauth.Auth{
			ID:                  id,
			Provider:            provider,
			Label:               provider + "-apikey",
			Prefix:              prefix,
			Status:              coreauth.StatusActive,
			ProxyURL:            proxyURL,
			Attributes:          attrs,
			Metadata:            metadata,
			CreatedAt:           now,
			UpdatedAt:           now,
			ProxyConnectionPool: authProxyConnectionPool(ck.ConnectionPool),
		}
		ApplyAuthExcludedModelsMeta(a, ck.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

// synthesizeOpenAICompat creates Auth entries for OpenAI-compatible providers.
func (s *ConfigSynthesizer) synthesizeOpenAICompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0)
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		prefix := strings.TrimSpace(compat.Prefix)
		providerName := strings.ToLower(strings.TrimSpace(compat.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		base := strings.TrimSpace(compat.BaseURL)
		disableCooling := compat.DisableCooling

		// Handle new APIKeyEntries format (preferred)
		createdEntries := 0
		for j := range compat.APIKeyEntries {
			entry := &compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			proxyURL := strings.TrimSpace(entry.ProxyURL)
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			idParts := []string{key, base, proxyURL}
			if poolID := config.FormatConnectionPoolDedupID(openAICompatConnectionPool(compat, entry)); poolID != "" {
				idParts = append(idParts, poolID)
			}
			id, token := idGen.Next(idKind, idParts...)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
				"config_index": strconv.Itoa(i),
			}
			metadata := map[string]any{}
			if disableCooling != nil {
				metadata["disable_cooling"] = *disableCooling
			}
			addRequestRetryToMetadata(compat.RequestRetry, metadata)
			addRequestScopedErrorsToMetadata(compat.RequestScopedErrors, metadata)
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			addConfigWeightToAttrs(compat.Weight, attrs)
			if key != "" {
				attrs["api_key"] = key
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			a := &coreauth.Auth{
				ID:                  id,
				Provider:            providerName,
				Label:               compat.Name,
				Prefix:              prefix,
				Status:              coreauth.StatusActive,
				ProxyURL:            proxyURL,
				Attributes:          attrs,
				Metadata:            metadata,
				CreatedAt:           now,
				UpdatedAt:           now,
				ProxyConnectionPool: authProxyConnectionPool(openAICompatConnectionPool(compat, entry)),
			}
			if len(a.Metadata) == 0 {
				a.Metadata = nil
			}
			out = append(out, a)
			createdEntries++
		}
		// Fallback: create entry without API key if no APIKeyEntries
		if createdEntries == 0 {
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			idParts := []string{base}
			if poolID := config.FormatConnectionPoolDedupID(openAICompatConnectionPool(compat, nil)); poolID != "" {
				idParts = append(idParts, poolID)
			}
			id, token := idGen.Next(idKind, idParts...)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
				"config_index": strconv.Itoa(i),
			}
			metadata := map[string]any{}
			if disableCooling != nil {
				metadata["disable_cooling"] = *disableCooling
			}
			addRequestRetryToMetadata(compat.RequestRetry, metadata)
			addRequestScopedErrorsToMetadata(compat.RequestScopedErrors, metadata)
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			addConfigWeightToAttrs(compat.Weight, attrs)
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			a := &coreauth.Auth{
				ID:                  id,
				Provider:            providerName,
				Label:               compat.Name,
				Prefix:              prefix,
				Status:              coreauth.StatusActive,
				Attributes:          attrs,
				Metadata:            metadata,
				CreatedAt:           now,
				UpdatedAt:           now,
				ProxyConnectionPool: authProxyConnectionPool(openAICompatConnectionPool(compat, nil)),
			}
			if len(a.Metadata) == 0 {
				a.Metadata = nil
			}
			out = append(out, a)
		}
	}
	return out
}

// synthesizeVertexCompat creates Auth entries for Vertex-compatible providers.
func (s *ConfigSynthesizer) synthesizeVertexCompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.VertexCompatAPIKey))
	for i := range cfg.VertexCompatAPIKey {
		compat := &cfg.VertexCompatAPIKey[i]
		providerName := "vertex"
		base := strings.TrimSpace(compat.BaseURL)

		key := strings.TrimSpace(compat.APIKey)
		prefix := strings.TrimSpace(compat.Prefix)
		proxyURL := strings.TrimSpace(compat.ProxyURL)
		idKind := "vertex:apikey"
		idParts := []string{key, base, proxyURL}
		if poolID := config.FormatConnectionPoolDedupID(compat.ConnectionPool); poolID != "" {
			idParts = append(idParts, poolID)
		}
		id, token := idGen.Next(idKind, idParts...)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:vertex-apikey[%s]", token),
			"base_url":     base,
			"provider_key": providerName,
			"config_index": strconv.Itoa(i),
		}
		if compat.Priority != 0 {
			attrs["priority"] = strconv.Itoa(compat.Priority)
		}
		addConfigWeightToAttrs(compat.Weight, attrs)
		if key != "" {
			attrs["api_key"] = key
		}
		if hash := diff.ComputeVertexCompatModelsHash(compat.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(compat.Headers, attrs)
		metadata := map[string]any{}
		if compat.DisableCooling != nil {
			metadata["disable_cooling"] = *compat.DisableCooling
		}
		addRequestRetryToMetadata(compat.RequestRetry, metadata)
		a := &coreauth.Auth{
			ID:                  id,
			Provider:            providerName,
			Label:               "vertex-apikey",
			Prefix:              prefix,
			Status:              coreauth.StatusActive,
			ProxyURL:            proxyURL,
			Attributes:          attrs,
			Metadata:            metadata,
			CreatedAt:           now,
			UpdatedAt:           now,
			ProxyConnectionPool: authProxyConnectionPool(compat.ConnectionPool),
		}
		ApplyAuthExcludedModelsMeta(a, compat.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}
