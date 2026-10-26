// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements the Antigravity executor that proxies requests to the antigravity
// upstream using OAuth credentials.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/cache"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/misc"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	antigravityclaude "github.com/therealtinhtute/llmhub/internal/translator/antigravity/claude"
	"github.com/therealtinhtute/llmhub/internal/util"
	sdkAuth "github.com/therealtinhtute/llmhub/sdk/auth"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/therealtinhtute/llmhub/sdk/proxyutil"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

const (
	antigravityBaseURLDaily                 = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandboxBaseURLDaily          = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityBaseURLProd                  = "https://cloudcode-pa.googleapis.com"
	antigravityCountTokensPath              = "/v1internal:countTokens"
	antigravityStreamPath                   = "/v1internal:streamGenerateContent"
	antigravityGeneratePath                 = "/v1internal:generateContent"
	antigravityClientID                     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityClientSecret                 = "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"
	defaultAntigravityAgent                 = "antigravity/1.21.9 darwin/arm64" // fallback only; overridden at runtime by misc.AntigravityUserAgent()
	antigravityAuthType                     = "antigravity"
	antigravityRequestTokenSafetyWindow     = 5 * time.Minute
	antigravityCredentialAcquisitionTimeout = 30 * time.Second
	antigravityCreditsHintRefreshInterval   = cliproxyauth.AntigravityCreditsHintRefreshInterval
	antigravityCreditsHintRefreshTimeout    = 5 * time.Second
	antigravityShortQuotaCooldownThreshold  = 5 * time.Minute
	antigravityInstantRetryThreshold        = 3 * time.Second
	// systemInstruction              = "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.**Absolute paths only****Proactiveness**"
)

type antigravity429Category string

type antigravityCreditsFailureState struct {
	PermanentlyDisabled      bool
	ExplicitBalanceExhausted bool
}

type antigravity429DecisionKind string

const (
	antigravity429Unknown                         antigravity429Category     = "unknown"
	antigravity429RateLimited                     antigravity429Category     = "rate_limited"
	antigravity429QuotaExhausted                  antigravity429Category     = "quota_exhausted"
	antigravity429SoftRateLimit                   antigravity429Category     = "soft_rate_limit"
	antigravity429DecisionSoftRetry               antigravity429DecisionKind = "soft_retry"
	antigravity429DecisionInstantRetrySameAuth    antigravity429DecisionKind = "instant_retry_same_auth"
	antigravity429DecisionShortCooldownSwitchAuth antigravity429DecisionKind = "short_cooldown_switch_auth"
	antigravity429DecisionFullQuotaExhausted      antigravity429DecisionKind = "full_quota_exhausted"
)

type antigravity429Decision struct {
	kind       antigravity429DecisionKind
	retryAfter *time.Duration
	reason     string
}

var (
	randSource                        = rand.New(rand.NewSource(time.Now().UnixNano()))
	randSourceMutex                   sync.Mutex
	antigravityCreditsFailureByAuth   sync.Map
	antigravityShortCooldownByAuth    sync.Map
	antigravityCreditsBalanceByAuth   sync.Map // auth.ID → antigravityCreditsBalance
	antigravityCreditsHintRefreshByID sync.Map // auth.ID → *antigravityCreditsHintRefreshState
	antigravityRefreshGroup           singleflight.Group
	antigravityQuotaExhaustedKeywords = []string{
		"quota_exhausted",
		"quota exhausted",
	}
)

type antigravityCreditsBalance struct {
	CreditAmount    float64
	MinCreditAmount float64
	PaidTierID      string
	Known           bool
}

type antigravityCreditsHintRefreshState struct {
	mu                sync.Mutex
	registrationEpoch uint64
	lastAttempt       time.Time
	task              *antigravityCreditsRefreshTask
}

type antigravityCreditsRefreshTask struct {
	state     *antigravityCreditsHintRefreshState
	ctx       context.Context
	lifecycle context.Context
	cancel    context.CancelFunc
	done      chan struct{}
}

func antigravityAuthHasCredits(auth *cliproxyauth.Auth) bool {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	if hint, ok := cliproxyauth.GetAntigravityCreditsHint(auth.ID); ok && hint.Known {
		return hint.Available
	}
	val, ok := antigravityCreditsBalanceByAuth.Load(strings.TrimSpace(auth.ID))
	if !ok {
		return true // optimistic: assume credits available when balance unknown
	}
	bal, valid := val.(antigravityCreditsBalance)
	if !valid {
		antigravityCreditsBalanceByAuth.Delete(strings.TrimSpace(auth.ID))
		return false
	}
	if !bal.Known {
		return false
	}
	available := bal.CreditAmount >= bal.MinCreditAmount
	cliproxyauth.SetAntigravityCreditsHint(strings.TrimSpace(auth.ID), cliproxyauth.AntigravityCreditsHint{
		Known:           true,
		Available:       available,
		CreditAmount:    bal.CreditAmount,
		MinCreditAmount: bal.MinCreditAmount,
		PaidTierID:      bal.PaidTierID,
		UpdatedAt:       time.Now(),
	})
	return available
}

// parseMetaFloat extracts a float64 from auth.Metadata (handles string and numeric types).
func parseMetaFloat(metadata map[string]any, key string) (float64, bool) {
	v, ok := metadata[key]
	if !ok {
		return 0, false
	}
	switch typed := v.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return f, true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// AntigravityExecutor proxies requests to the antigravity upstream.
type AntigravityExecutor struct {
	cfg *config.Config
}

// NewAntigravityExecutor creates a new Antigravity executor instance.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *AntigravityExecutor: A new Antigravity executor instance
func NewAntigravityExecutor(cfg *config.Config) *AntigravityExecutor {
	return &AntigravityExecutor{cfg: cfg}
}

// Each Antigravity credential gets its own HTTP/1.1 connection pool. Sessions routed
// to the same auth reuse that pool, while different OAuth identities never share a
// TCP/TLS connection, matching the native client's one-credential process model.
// The cache is bounded so pools cannot accumulate when keys churn.
var (
	antigravityBaseTransport = defaultAntigravityBaseTransport()
	antigravityTransports    = helps.NewTransportCache[antigravityTransportKey](antigravityTransportCacheCapacity)
)

const (
	// antigravityTransportCacheCapacity caps how many Antigravity connection pools stay
	// alive. The bound exists only to stop entries from accumulating when keys churn, for
	// example when a credential's proxy is rotated through the management API or when an
	// SDK embedder supplies a freshly built base transport per request.
	//
	// It is sized for large deployments on purpose. An unused cache entry costs under 1 KB
	// and no goroutines, so capacity is close to free, whereas evicting a pool that is
	// still in active use forces the next request on that credential to redo the TCP + TLS
	// handshake and defeats the point of caching. Credential counts in the low thousands
	// are expected once Home-managed pools are included.
	//
	// Capacity is therefore NOT the lever for bounding memory: an idle pooled connection
	// costs roughly 38 KB plus three goroutines, and that total is driven by live traffic
	// and reclaimed by IdleConnTimeout. Shrinking this number does not save that memory,
	// it only causes pool thrashing.
	antigravityTransportCacheCapacity = 8192

	// antigravityDefaultMaxIdleConnsPerHost sets the default number of idle connections
	// to retain per host per credential when connection pooling is enabled.
	// Matches Go's DefaultMaxIdleConnsPerHost (2) and the native Antigravity binary.
	antigravityDefaultMaxIdleConnsPerHost = 2

	// antigravityMaxAllowedMaxIdleConnsPerHost is the hard upper bound on MaxIdleConnsPerHost (100).
	// Prevents unbounded connection pool expansion in multi-credential environments.
	antigravityMaxAllowedMaxIdleConnsPerHost = 100

	// antigravityDefaultIdleConnTimeout is the default idle connection timeout (30 seconds).
	// Kept strictly far below Google Frontend (GFE / ESF) 240-second cutoff to prevent
	// client-side reuse of half-closed connections that cause connection resets.
	antigravityDefaultIdleConnTimeout = 30 * time.Second

	// antigravityMaxAllowedIdleConnTimeout is the hard upper bound on IdleConnTimeout (210 seconds).
	// Kept strictly below Google Frontend (GFE / ESF) 240.0-second HTTP/1.1 idle keep-alive cutoff
	// with a 30-second safety margin to eliminate timer race conditions.
	antigravityMaxAllowedIdleConnTimeout = 210 * time.Second

	// antigravityAnonymousTransportScope is the pool scope for auth objects that carry
	// no identity at all. Reaching it means the auth has no ID, no source path and no
	// token of any kind, so there is no credential to keep isolated and a single shared
	// pool is safe. Allocating a private pool per request instead would leak a
	// connection pool, and the goroutines managing it, on every call.
	antigravityAnonymousTransportScope = "anonymous"
)

// antigravityPoolSettings resolves the effective upstream connection pooling
// behavior. Ported from upstream commits d5397905f09e and 68dd99d56f68.
type antigravityPoolSettings struct {
	shortMode           bool
	idleConnTimeout     time.Duration
	maxIdleConnsPerHost int
}

func resolveAntigravityPoolSettings(cfg *config.Config) antigravityPoolSettings {
	// By default, upstream connection pooling is disabled (shortMode = true, maxIdleConnsPerHost = -1)
	// to prevent socket buildup and stale connection errors across rotating credentials.
	settings := antigravityPoolSettings{
		shortMode:           true,
		maxIdleConnsPerHost: -1,
		idleConnTimeout:     0,
	}
	if cfg == nil {
		return settings
	}

	// Pooling is active ONLY when explicitly enabled: true
	poolCfg := cfg.AntigravityConnectionPool
	if poolCfg.Enabled == nil || !*poolCfg.Enabled {
		return settings
	}

	// Enabled is true: initialize default pool settings
	settings.shortMode = false
	settings.idleConnTimeout = antigravityDefaultIdleConnTimeout
	settings.maxIdleConnsPerHost = antigravityDefaultMaxIdleConnsPerHost

	rawTimeout := strings.TrimSpace(poolCfg.IdleConnTimeout)
	if rawTimeout != "" {
		d, err := time.ParseDuration(rawTimeout)
		if err != nil {
			log.Warnf("antigravity executor: invalid idle-conn-timeout %q: %v, using default %v", rawTimeout, err, antigravityDefaultIdleConnTimeout)
		} else {
			if d <= 0 {
				settings.shortMode = true
				settings.maxIdleConnsPerHost = -1
				return settings
			}
			if d > antigravityMaxAllowedIdleConnTimeout {
				d = antigravityMaxAllowedIdleConnTimeout
			}
			settings.idleConnTimeout = d
		}
	}

	if poolCfg.MaxIdleConnsPerHost != nil {
		val := *poolCfg.MaxIdleConnsPerHost
		if val < 0 {
			settings.shortMode = true
			settings.maxIdleConnsPerHost = -1
			return settings
		}
		if val > antigravityMaxAllowedMaxIdleConnsPerHost {
			val = antigravityMaxAllowedMaxIdleConnsPerHost
		}
		settings.maxIdleConnsPerHost = val
	}

	if settings.maxIdleConnsPerHost < 0 {
		settings.shortMode = true
	}

	return settings
}

// ResetAntigravityTransports purges all cached Antigravity connection pools and closes their idle connections.
// Used during configuration hot-reloads to ensure updated pool parameters apply immediately.
func ResetAntigravityTransports() {
	antigravityTransports.Purge()
}

// AntigravityTransportsLen reports the current number of cached Antigravity transports.
func AntigravityTransportsLen() int {
	return antigravityTransports.Len()
}

// closeAntigravityAuthIdleTransports closes and removes all idle connections for the given auth.
func closeAntigravityAuthIdleTransports(auth *cliproxyauth.Auth) {
	if auth == nil {
		return
	}
	scope := antigravityTransportScope(auth)
	if scope == "" || scope == antigravityAnonymousTransportScope {
		return
	}
	antigravityTransports.CloseMatching(func(key antigravityTransportKey) bool {
		return key.credential == scope
	})
}

// antigravityTransportKey identifies one connection pool. At most one of proxy and
// base is set: proxy for a credential-scoped proxy pool, base for a transport handed
// in through the request context, and neither for a direct pool.
// Resolved pool settings (shortMode, idleConnTimeout, maxIdleConnsPerHost) are included
// in the key to prevent stale transport reuse across configuration hot-reloads under load.
type antigravityTransportKey struct {
	credential          string
	proxy               string
	base                *http.Transport
	shortMode           bool
	idleConnTimeout     time.Duration
	maxIdleConnsPerHost int
}

func defaultAntigravityBaseTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport
	}
	return &http.Transport{}
}

func cloneTransportWithHTTP11(base *http.Transport, cfgs ...*config.Config) *http.Transport {
	if base == nil {
		return nil
	}

	clone := base.Clone()
	clone.ForceAttemptHTTP2 = false
	// Wipe TLSNextProto to prevent implicit HTTP/2 upgrade.
	clone.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
	}
	// Native Antigravity sends no ALPN extension. With HTTP/2 disabled above,
	// an empty NextProtos keeps the wire shape aligned while using HTTP/1.1.
	clone.TLSClientConfig.NextProtos = nil
	applyAntigravityPoolLimits(clone, cfgs...)
	return clone
}

// applyAntigravityPoolLimits configures connection pool parameters for Antigravity.
// Default: IdleConnTimeout=30s, MaxIdleConnsPerHost=2.
// IdleConnTimeout is strictly capped at 210s (antigravityMaxAllowedIdleConnTimeout)
// matching the Google Frontend (GFE / ESF) HTTP/1.1 idle cutoff.
// If short-lived connection mode is configured, MaxIdleConnsPerHost is set to -1
// so idle connections are closed immediately upon request completion.
func applyAntigravityPoolLimits(transport *http.Transport, cfgs ...*config.Config) {
	if transport == nil {
		return
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	if settings.shortMode {
		transport.MaxIdleConnsPerHost = -1
		transport.DisableKeepAlives = false
		transport.IdleConnTimeout = 0
		return
	}

	// If the operator base transport already disabled pooling (< 0), honor it.
	if transport.MaxIdleConnsPerHost < 0 {
		return
	}

	// Go treats 0 as DefaultMaxIdleConnsPerHost (2). Ensure at least configured/default limit.
	if transport.MaxIdleConnsPerHost < settings.maxIdleConnsPerHost {
		transport.MaxIdleConnsPerHost = settings.maxIdleConnsPerHost
	}

	// MaxIdleConns caps the pool across all hosts.
	if transport.MaxIdleConns > 0 && transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		transport.MaxIdleConns = transport.MaxIdleConnsPerHost
	}

	// Apply IdleConnTimeout:
	// If the config explicitly specified a timeout, apply settings.idleConnTimeout.
	// If config did not specify a timeout:
	// - if base transport already has a timeout > 0, preserve it (capped at 210s).
	// - otherwise, apply default 30s.
	rawTimeout := ""
	if cfg != nil {
		rawTimeout = strings.TrimSpace(cfg.AntigravityConnectionPool.IdleConnTimeout)
	}
	if rawTimeout != "" {
		transport.IdleConnTimeout = settings.idleConnTimeout
	} else if transport.IdleConnTimeout == 0 || transport.IdleConnTimeout == 90*time.Second {
		transport.IdleConnTimeout = settings.idleConnTimeout
	} else if transport.IdleConnTimeout > antigravityMaxAllowedIdleConnTimeout {
		transport.IdleConnTimeout = antigravityMaxAllowedIdleConnTimeout
	}
}

// antigravityHTTP11Transport returns the HTTP/1.1 pool shared by every request that
// uses the same credential and the same base transport. The base is either the
// process default or a transport provided through the request context.
func antigravityHTTP11Transport(auth *cliproxyauth.Auth, base *http.Transport, cfgs ...*config.Config) *http.Transport {
	if base == nil {
		return nil
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	key := antigravityTransportKey{
		credential:          antigravityTransportScope(auth),
		base:                base,
		shortMode:           settings.shortMode,
		idleConnTimeout:     settings.idleConnTimeout,
		maxIdleConnsPerHost: settings.maxIdleConnsPerHost,
	}
	transport, errGet := antigravityTransports.Get(key, func() (*http.Transport, error) {
		return cloneTransportWithHTTP11(base, cfgs...), nil
	})
	if errGet != nil {
		// Defensive only: the builder above cannot fail. Never return nil here, because a
		// nil Transport makes http.Client fall back to http.DefaultTransport, which
		// advertises h2 over ALPN and would break the Antigravity wire fingerprint.
		log.Debugf("antigravity executor: cache HTTP/1.1 transport failed: %v", errGet)
		return cloneTransportWithHTTP11(base, cfgs...)
	}
	return transport
}

// antigravityProxiedHTTP11Transport returns the credential-scoped HTTP/1.1 pool for
// one proxy setting, or nil when the proxy setting cannot be turned into a
// transport. Keying on the normalized proxy string rather than on a prebuilt
// transport keeps one pool per credential and proxy instead of one per request.
func antigravityProxiedHTTP11Transport(auth *cliproxyauth.Auth, proxyURL string, cfgs ...*config.Config) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	key := antigravityTransportKey{
		credential:          antigravityTransportScope(auth),
		proxy:               proxyURL,
		shortMode:           settings.shortMode,
		idleConnTimeout:     settings.idleConnTimeout,
		maxIdleConnsPerHost: settings.maxIdleConnsPerHost,
	}
	transport, errGet := antigravityTransports.Get(key, func() (*http.Transport, error) {
		base, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
		if errBuild != nil {
			return nil, errBuild
		}
		if base == nil {
			return nil, fmt.Errorf("antigravity executor: proxy setting produced no transport")
		}
		return cloneTransportWithHTTP11(base, cfgs...), nil
	})
	if errGet != nil {
		// The caller falls back to NewProxyAwareHTTPClient, which reports the failure
		// and applies the context transport fallback.
		return nil
	}
	return transport
}

// antigravityTransportScope returns the connection-pool scope for one credential.
// Runtime auths always carry an ID. Incomplete auth objects, such as those built by
// tests, plugins or SDK embedders, fall back to another stable credential marker so
// they neither share a pool with an unrelated OAuth identity nor allocate a fresh
// pool, and with it a fresh set of pool goroutines, on every single request.
func antigravityTransportScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return antigravityAnonymousTransportScope
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return "id:" + id
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			return "path:" + path
		}
		if source := strings.TrimSpace(auth.Attributes["source"]); source != "" {
			return "source:" + source
		}
	}
	// Fall back to the credential material itself. Auth.Label is deliberately not used:
	// it is documented as an optional human readable label for logging and carries no
	// uniqueness guarantee, so two different OAuth identities sharing one label would
	// wrongly share a TCP/TLS pool.
	//
	// The refresh token is preferred over the access token because it stays stable
	// across token rotation. Keying on the access token would move a credential to a new
	// pool on every refresh, and would also strand refresh requests themselves, which
	// run before any access token exists.
	if refresh := strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token")); refresh != "" {
		return antigravityCredentialScope("refresh:", refresh)
	}
	if access := strings.TrimSpace(metaStringValue(auth.Metadata, "access_token")); access != "" {
		return antigravityCredentialScope("token:", access)
	}
	return antigravityAnonymousTransportScope
}

// antigravityCredentialScope derives a pool scope from secret credential material.
// Only a short digest is retained, and it is never logged, so a pool key cannot be
// used to recover the credential it came from.
func antigravityCredentialScope(prefix, secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return prefix + hex.EncodeToString(digest[:8])
}

// newAntigravityHTTPClient creates an HTTP client specifically for Antigravity,
// enforcing HTTP/1.1 by disabling HTTP/2 to match the native Antigravity client, which
// negotiates TLS 1.3 without advertising an ALPN protocol and therefore never uses h2.
// The underlying Transport is always shared so keep-alive connections survive across
// requests instead of forcing a fresh TCP + TLS handshake every time.
func newAntigravityHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	// Native Antigravity reuses one transport across requests. Opt into a
	// credential-scoped proxy transport only here so other providers keep their
	// existing lifecycle and different OAuth identities remain isolated.
	if proxyURL := antigravityProxyURL(cfg, auth); proxyURL != "" {
		if transport := antigravityProxiedHTTP11Transport(auth, proxyURL, cfg); transport != nil {
			return &http.Client{Transport: transport, Timeout: timeout}
		}
		// Fall through so NewProxyAwareHTTPClient reports the failure and applies the
		// context transport fallback, preserving the previous behavior.
	}

	client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, timeout)
	// Direct requests share an HTTP/1.1 pool only within the selected credential.
	if client.Transport == nil {
		client.Transport = antigravityHTTP11Transport(auth, antigravityBaseTransport, cfg)
		return client
	}

	// Preserve a context-provided transport while forcing HTTP/1.1. The cache key
	// includes credential identity, so sharing the base does not share TLS pools.
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		// A RoundTripper that is not an *http.Transport owns its own protocol behavior.
		return client
	}
	if transport == nil {
		// A typed-nil *http.Transport still satisfies the interface nil check in
		// NewProxyAwareHTTPClient. Leaving it in place would make http.Client fall back
		// to http.DefaultTransport, which advertises h2 over ALPN and breaks the
		// Antigravity fingerprint, so substitute the process base transport.
		transport = antigravityBaseTransport
	}
	client.Transport = antigravityHTTP11Transport(auth, transport, cfg)
	return client
}

func antigravityProxyURL(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func validateAntigravityRequestSignatures(from sdktranslator.Format, rawJSON []byte) ([]byte, error) {
	if from.String() != "claude" {
		return rawJSON, nil
	}
	// Always strip thinking blocks with invalid signatures (empty or non-Claude-format).
	rawJSON = antigravityclaude.StripEmptySignatureThinkingBlocks(rawJSON)
	if cache.SignatureCacheEnabled() {
		return rawJSON, nil
	}
	if !cache.SignatureBypassStrictMode() {
		// Non-strict bypass: let the translator handle invalid signatures
		// by dropping unsigned thinking blocks silently (no 400).
		return rawJSON, nil
	}
	if err := antigravityclaude.ValidateClaudeBypassSignatures(rawJSON); err != nil {
		return rawJSON, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	return rawJSON, nil
}

func ensureAntigravityGeminiLeadingUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	return helps.EnsureGeminiLeadingUserContent(payload, "request.contents")
}

// ensureAntigravityGeminiTrailingUserContent appends a synthetic empty user turn
// if the final turn is a model turn. Claude targets are left unchanged because
// the adapter rejects empty text parts. Ported from upstream 5dc428f39270.
func ensureAntigravityGeminiTrailingUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	payload = helps.DropGeminiTrailingThoughtOnlyContent(payload, "request.contents")
	return helps.EnsureGeminiTrailingUserContent(payload, "request.contents")
}

// ensureAntigravityGeminiBoundaryUserContent normalizes both leading and trailing
// turns for Gemini targets. Claude targets are left unchanged.
func ensureAntigravityGeminiBoundaryUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	payload = helps.DropGeminiTrailingThoughtOnlyContent(payload, "request.contents")
	return helps.EnsureGeminiBoundaryUserContent(payload, "request.contents")
}

// hasAntigravityClaudeTypedWebSearchTool reports whether a Claude-format request
// carries a typed web_search tool block.
// Ported from upstream CLIProxyAPI v7.3.6 (internal/runtime/executor/antigravity_executor.go).
func hasAntigravityClaudeTypedWebSearchTool(payload []byte) bool {
	tools := util.GetGJSONBytesNoCopy(payload, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "web_search_20250305", "web_search_20260209":
			return true
		}
	}
	return false
}

// hasAntigravityGoogleSearchTool reports whether a translated Antigravity request
// carries a native googleSearch tool block.
// Ported from upstream CLIProxyAPI v7.3.6.
func hasAntigravityGoogleSearchTool(payload []byte) bool {
	tools := util.GetGJSONBytesNoCopy(payload, "request.tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if tool.Get("googleSearch").Exists() {
			return true
		}
	}
	return false
}

// hasAntigravityResponsesWebSearchTool reports whether an OpenAI Responses request
// carries a web search tool type.
// Ported from upstream CLIProxyAPI v7.3.6.
func hasAntigravityResponsesWebSearchTool(rawJSON []byte) bool {
	tools := util.GetGJSONBytesNoCopy(rawJSON, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
			return true
		}
	}
	return false
}

// shouldResolveAntigravityWebSearchGroundingURLs gates Vertex grounding redirect
// resolution to requests that actually translated to a googleSearch Antigravity
// request from a Claude typed web-search tool or an OpenAI Responses web-search
// tool. Ported from upstream CLIProxyAPI v7.3.6.
func shouldResolveAntigravityWebSearchGroundingURLs(from sdktranslator.Format, originalRequestRawJSON, requestRawJSON []byte) bool {
	if !hasAntigravityGoogleSearchTool(requestRawJSON) {
		return false
	}
	switch from {
	case sdktranslator.FormatClaude:
		return hasAntigravityClaudeTypedWebSearchTool(originalRequestRawJSON)
	case sdktranslator.FormatOpenAIResponse:
		return hasAntigravityResponsesWebSearchTool(originalRequestRawJSON)
	default:
		return false
	}
}

// resolveWebSearchGroundingURLs replaces Vertex grounding redirect URLs in the
// upstream response when the request path enabled native web search.
// Ported from upstream CLIProxyAPI v7.3.6.
func (e *AntigravityExecutor) resolveWebSearchGroundingURLs(ctx context.Context, auth *cliproxyauth.Auth, from sdktranslator.Format, originalRequestRawJSON, requestRawJSON, responseRawJSON []byte) []byte {
	if !shouldResolveAntigravityWebSearchGroundingURLs(from, originalRequestRawJSON, requestRawJSON) {
		return responseRawJSON
	}
	return helps.ResolveAntigravityGroundingURLs(ctx, e.cfg, auth, responseRawJSON)
}

// Identifier returns the executor identifier.
func (e *AntigravityExecutor) Identifier() string { return antigravityAuthType }

// PrepareRequest injects Antigravity credentials into the outgoing HTTP request.
func (e *AntigravityExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _, errToken := e.ensureAccessToken(req.Context(), auth)
	if errToken != nil {
		return errToken
	}
	if strings.TrimSpace(token) == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// HttpRequest injects Antigravity credentials into the request and executes it.
// It uses a whitelist approach: all incoming headers are stripped and only
// the minimum set required by the Antigravity protocol is explicitly set.
func (e *AntigravityExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("antigravity executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)

	// --- Whitelist: save only the headers we need from the original request ---
	contentType := httpReq.Header.Get("Content-Type")

	// Wipe ALL incoming headers
	for k := range httpReq.Header {
		delete(httpReq.Header, k)
	}

	// --- Set only the headers Antigravity actually sends ---
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	// Content-Length is managed automatically by Go's http.Client from the Body
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	// Connection management is a Request field, not a header, so the header
	// whitelist below cannot strip it. An inbound "Connection: close" makes Go's
	// server set Request.Close, and WithContext copies that field verbatim, which
	// would both leak the downstream header upstream and drain the shared pool.
	httpReq.Close = false

	// Inject Authorization: Bearer <token>
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func injectEnabledCreditTypes(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	if !gjson.ValidBytes(payload) {
		return nil
	}
	updated, err := sjson.SetRawBytes(payload, "enabledCreditTypes", []byte(`["GOOGLE_ONE_AI"]`))
	if err != nil {
		return nil
	}
	return updated
}

func classifyAntigravity429(body []byte) antigravity429Category {
	switch decideAntigravity429(body).kind {
	case antigravity429DecisionInstantRetrySameAuth, antigravity429DecisionShortCooldownSwitchAuth:
		return antigravity429RateLimited
	case antigravity429DecisionFullQuotaExhausted:
		return antigravity429QuotaExhausted
	case antigravity429DecisionSoftRetry:
		return antigravity429SoftRateLimit
	default:
		return antigravity429Unknown
	}
}

func decideAntigravity429(body []byte) antigravity429Decision {
	decision := antigravity429Decision{kind: antigravity429DecisionSoftRetry}
	if len(body) == 0 {
		return decision
	}

	if retryAfter, parseErr := parseRetryDelay(body); parseErr == nil && retryAfter != nil {
		decision.retryAfter = retryAfter
	}

	status := strings.TrimSpace(gjson.GetBytes(body, "error.status").String())
	if !strings.EqualFold(status, "RESOURCE_EXHAUSTED") {
		return decision
	}

	details := gjson.GetBytes(body, "error.details")
	if details.Exists() && details.IsArray() {
		for _, detail := range details.Array() {
			if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
				continue
			}
			reason := strings.TrimSpace(detail.Get("reason").String())
			decision.reason = reason
			switch {
			case strings.EqualFold(reason, "QUOTA_EXHAUSTED"), strings.EqualFold(reason, "INSUFFICIENT_G1_CREDITS_BALANCE"):
				decision.kind = antigravity429DecisionFullQuotaExhausted
				return decision
			case strings.EqualFold(reason, "RATE_LIMIT_EXCEEDED"):
				if decision.retryAfter == nil {
					decision.kind = antigravity429DecisionSoftRetry
					return decision
				}
				switch {
				case *decision.retryAfter < antigravityInstantRetryThreshold:
					decision.kind = antigravity429DecisionInstantRetrySameAuth
				case *decision.retryAfter < antigravityShortQuotaCooldownThreshold:
					decision.kind = antigravity429DecisionShortCooldownSwitchAuth
				default:
					decision.kind = antigravity429DecisionFullQuotaExhausted
				}
				return decision
			}
		}
	}

	lowerBody := strings.ToLower(string(body))
	for _, keyword := range antigravityQuotaExhaustedKeywords {
		if strings.Contains(lowerBody, keyword) {
			decision.kind = antigravity429DecisionFullQuotaExhausted
			decision.reason = "quota_exhausted"
			return decision
		}
	}

	decision.kind = antigravity429DecisionSoftRetry
	return decision
}

func antigravityCreditsRetryEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.QuotaExceeded.AntigravityCredits
}

func clearAntigravityCreditsFailureState(auth *cliproxyauth.Auth) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	_, hadFailure := antigravityCreditsFailureByAuth.LoadAndDelete(authID)
	if hint, ok := cliproxyauth.GetAntigravityCreditsHint(authID); ok && hint.Known && !hint.Available {
		if hadFailure || !cliproxyauth.IsAntigravityCreditsHintFresh(hint, time.Now()) {
			cliproxyauth.SetAntigravityCreditsHint(authID, cliproxyauth.AntigravityCreditsHint{})
		}
	}
}
func markAntigravityCreditsPermanentlyDisabled(auth *cliproxyauth.Auth) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || antigravityCoolingDisabled(auth, nil) {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	state := antigravityCreditsFailureState{
		PermanentlyDisabled:      true,
		ExplicitBalanceExhausted: true,
	}
	antigravityCreditsFailureByAuth.Store(authID, state)
	antigravityCreditsBalanceByAuth.Store(authID, antigravityCreditsBalance{
		CreditAmount:    0,
		MinCreditAmount: 1,
		Known:           true,
	})
	cliproxyauth.SetAntigravityCreditsHint(authID, cliproxyauth.AntigravityCreditsHint{
		Known:           true,
		Available:       false,
		CreditAmount:    0,
		MinCreditAmount: 1,
		UpdatedAt:       time.Now(),
	})
}

func clearAntigravityCreditsPermanentlyDisabled(auth *cliproxyauth.Auth) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	antigravityCreditsFailureByAuth.Delete(strings.TrimSpace(auth.ID))
}

func antigravityHasExplicitCreditsBalanceExhaustedReason(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	details := gjson.GetBytes(body, "error.details")
	if !details.Exists() || !details.IsArray() {
		return false
	}
	for _, detail := range details.Array() {
		if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		reason := strings.TrimSpace(detail.Get("reason").String())
		if strings.EqualFold(reason, "INSUFFICIENT_G1_CREDITS_BALANCE") {
			return true
		}
	}
	return false
}

func newAntigravityStatusErr(statusCode int, body []byte) statusErr {
	err := statusErr{code: statusCode, msg: string(body)}
	if statusCode == http.StatusTooManyRequests {
		if retryAfter, parseErr := parseRetryDelay(body); parseErr == nil && retryAfter != nil {
			err.retryAfter = retryAfter
		}
	}
	return err
}

// Execute performs a non-streaming request to the Antigravity API.
func (e *AntigravityExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if helps.HasResponsesCompactionItem(req.Payload) {
		expanded, errExpand := helps.ExpandAntigravityCompactionCapsules(req.Payload)
		if errExpand != nil {
			return resp, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
		}
		req.Payload = expanded
		if len(opts.OriginalRequest) > 0 {
			expandedOrig, errOrig := helps.ExpandAntigravityCompactionCapsules(opts.OriginalRequest)
			if errOrig == nil {
				opts.OriginalRequest = expandedOrig
			} else {
				opts.OriginalRequest = expanded
			}
		}
	}
	if opts.Alt == "responses/compact" || helps.HasResponsesCompactionTrigger(req.Payload) || helps.HasResponsesCompactionTrigger(opts.OriginalRequest) {
		return e.executeCompaction(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if !antigravityCoolingDisabled(auth, e.cfg) {
		if inCooldown, remaining := antigravityIsInShortCooldown(auth, baseModel, time.Now()); inCooldown && !antigravityShouldBypassShortCooldown(ctx, e.cfg) {
			log.Debugf("antigravity executor: auth %s in short cooldown for model %s (%s remaining), returning 429 to switch auth", auth.ID, baseModel, remaining)
			d := remaining
			return resp, statusErr{code: http.StatusTooManyRequests, msg: fmt.Sprintf("auth in short cooldown, %s remaining", remaining), retryAfter: &d}
		}
	}

	isClaude := strings.Contains(strings.ToLower(baseModel), "claude")
	if isClaude || strings.Contains(baseModel, "gemini-3-pro") || strings.Contains(baseModel, "gemini-3.1-flash-image") {
		return e.executeClaudeNonStream(ctx, auth, req, opts)
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(from, originalPayload)
	if errValidate != nil {
		return resp, errValidate
	}
	req.Payload = originalPayload
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, ModelInfo: modelInfo}
	originalTranslated, translated := helps.TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq, originalPayload, req.Payload)

	// Use request-bound capabilities when Home attached them (upstream 6ff680e90ab5).
	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "antigravity", from.String(), "request", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	// Capture the effective tier after the last payload rule has run.
	reporter.SetEffectiveServiceTier(translated)
	translated, replayScope, errReplay := prepareAntigravityGeminiReasoningReplayPayload(ctx, baseModel, req, opts, translated)
	if errReplay != nil {
		return resp, errReplay
	}

	useCredits := cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(e.cfg)

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	attempts := antigravityRetryAttempts(auth, e.cfg)

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lastStatus int
		var lastBody []byte
		var lastErr error

		for idx, baseURL := range baseURLs {
			requestPayload := translated
			if useCredits {
				if cp := injectEnabledCreditTypes(translated); len(cp) > 0 {
					requestPayload = cp
					helps.MarkCreditsUsed(ctx)
				}
			}

			requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)
			httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, requestPayload, false, opts.Alt, baseURL, helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata))
			if errReq != nil {
				err = errReq
				return resp, err
			}

			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errDo)
				if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
					return resp, errDo
				}
				lastStatus = 0
				lastBody = nil
				lastErr = errDo
				if idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				err = errDo
				return resp, err
			}

			helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
			bodyBytes, errRead := io.ReadAll(httpResp.Body)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("antigravity executor: close response body error: %v", errClose)
			}
			if errRead != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errRead)
				err = errRead
				return resp, err
			}
			helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)

			if httpResp.StatusCode == http.StatusTooManyRequests {
				decision := decideAntigravity429(bodyBytes)
				switch decision.kind {
				case antigravity429DecisionInstantRetrySameAuth:
					if attempt+1 < attempts {
						if decision.retryAfter != nil && *decision.retryAfter > 0 {
							wait := antigravityInstantRetryDelay(*decision.retryAfter)
							log.Debugf("antigravity executor: instant retry for model %s, waiting %s", baseModel, wait)
							if errWait := antigravityWait(ctx, wait); errWait != nil {
								return resp, errWait
							}
						}
						continue attemptLoop
					}
				case antigravity429DecisionShortCooldownSwitchAuth:
					closeAntigravityAuthIdleTransports(auth)
					if decision.retryAfter != nil && *decision.retryAfter > 0 && !antigravityCoolingDisabled(auth, e.cfg) {
						markAntigravityShortCooldown(auth, baseModel, time.Now(), *decision.retryAfter)
						log.Debugf("antigravity executor: short quota cooldown (%s) for model %s, recorded cooldown", *decision.retryAfter, baseModel)
					}
				case antigravity429DecisionFullQuotaExhausted:
					closeAntigravityAuthIdleTransports(auth)
					if useCredits && antigravityHasExplicitCreditsBalanceExhaustedReason(bodyBytes) && !antigravityCoolingDisabled(auth, e.cfg) {
						markAntigravityCreditsPermanentlyDisabled(auth)
					}
					// No credits logic - just fall through to error return below
				}
			}

			if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
				log.Debugf("antigravity executor: upstream error status: %d, body: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), bodyBytes))
				lastStatus = httpResp.StatusCode
				lastBody = append([]byte(nil), bodyBytes...)
				lastErr = nil
				if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				if antigravityShouldRetryTransientResourceExhausted429(httpResp.StatusCode, bodyBytes) && attempt+1 < attempts {
					delay := antigravityTransient429RetryDelay(attempt)
					log.Debugf("antigravity executor: transient 429 resource exhausted for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
					if errWait := antigravityWait(ctx, delay); errWait != nil {
						return resp, errWait
					}
					continue attemptLoop
				}
				if antigravityShouldRetryNoCapacity(httpResp.StatusCode, bodyBytes) {
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					if attempt+1 < attempts {
						delay := antigravityNoCapacityRetryDelay(attempt)
						log.Debugf("antigravity executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return resp, errWait
						}
						continue attemptLoop
					}
				}
				if antigravityShouldRetrySoftRateLimit(httpResp.StatusCode, bodyBytes) {
					if attempt+1 < attempts {
						delay := antigravitySoftRateLimitDelay(attempt)
						log.Debugf("antigravity executor: soft rate limit for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return resp, errWait
						}
						continue attemptLoop
					}
				}
				err = newAntigravityStatusErr(httpResp.StatusCode, bodyBytes)
				return resp, err
			}

			// Success
			if useCredits {
				clearAntigravityCreditsFailureState(auth)
			}
			cacheAntigravityReasoningReplayFromResponse(ctx, replayScope, requestPayload, bodyBytes)
			bodyBytes = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, translated, bodyBytes)
			reporter.ObserveResponseModel(bodyBytes)
			reporter.Publish(ctx, helps.ParseAntigravityUsage(bodyBytes))
			var param any
			converted := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bodyBytes, &param)
			if from == sdktranslator.FormatOpenAIResponse {
				converted = helps.EnsureResponsesUsageDetails(converted)
			}
			resp = cliproxyexecutor.Response{Payload: converted, Headers: httpResp.Header.Clone()}
			reporter.EnsurePublished(ctx)
			return resp, nil
		}

		switch {
		case lastStatus != 0:
			err = newAntigravityStatusErr(lastStatus, lastBody)
		case lastErr != nil:
			err = lastErr
		default:
			err = statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
		}
		return resp, err
	}

	return resp, err
}

// executeCompaction runs a non-stream summary generation upstream and wraps the
// resulting summary in an encrypted compaction capsule response.
func (e *AntigravityExecutor) executeCompaction(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	payload := req.Payload
	if len(payload) == 0 && len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(payload, baseModel)

	summaryReq := cliproxyexecutor.Request{
		Model:    req.Model,
		Payload:  summaryPayload,
		Metadata: req.Metadata,
	}
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse

	summaryResp, errSummary := e.Execute(ctx, auth, summaryReq, summaryOpts)
	if errSummary != nil {
		return resp, errSummary
	}

	summaryText, errExtract := helps.ExtractAntigravitySummaryText(summaryResp.Payload)
	if errExtract != nil {
		return resp, fmt.Errorf("extract summary: %w", errExtract)
	}
	capsule, errSeal := helps.SealAntigravityCompaction(summaryText, baseModel)
	if errSeal != nil {
		return resp, fmt.Errorf("seal compaction capsule: %w", errSeal)
	}

	inputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.input_tokens").Int())
	outputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.output_tokens").Int())
	totalTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.total_tokens").Int())
	if totalTokens == 0 && inputTokens == 0 {
		usage := helps.ParseOpenAIUsage(summaryResp.Payload)
		inputTokens = int(usage.InputTokens)
		outputTokens = int(usage.OutputTokens)
		totalTokens = int(usage.TotalTokens)
	}

	respBytes := helps.BuildAntigravityCompactionResponse(baseModel, capsule, inputTokens, outputTokens, totalTokens)
	return cliproxyexecutor.Response{
		Payload: respBytes,
		Headers: summaryResp.Headers,
	}, nil
}

// executeClaudeNonStream performs a claude non-streaming request to the Antigravity API.
func (e *AntigravityExecutor) executeClaudeNonStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if !antigravityCoolingDisabled(auth, e.cfg) {
		if inCooldown, remaining := antigravityIsInShortCooldown(auth, baseModel, time.Now()); inCooldown && !antigravityShouldBypassShortCooldown(ctx, e.cfg) {
			log.Debugf("antigravity executor: auth %s in short cooldown for model %s (%s remaining), returning 429 to switch auth", auth.ID, baseModel, remaining)
			d := remaining
			return resp, statusErr{code: http.StatusTooManyRequests, msg: fmt.Sprintf("auth in short cooldown, %s remaining", remaining), retryAfter: &d}
		}
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(from, originalPayload)
	if errValidate != nil {
		return resp, errValidate
	}
	req.Payload = originalPayload
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Stream: true, ModelInfo: modelInfo}
	originalTranslated, translated := helps.TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq, originalPayload, req.Payload)

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "antigravity", from.String(), "request", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	// Capture the effective tier after the last payload rule has run.
	reporter.SetEffectiveServiceTier(translated)

	useCredits := cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(e.cfg)

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)

	attempts := antigravityRetryAttempts(auth, e.cfg)

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lastStatus int
		var lastBody []byte
		var lastErr error

		for idx, baseURL := range baseURLs {
			requestPayload := translated
			if useCredits {
				if cp := injectEnabledCreditTypes(translated); len(cp) > 0 {
					requestPayload = cp
					helps.MarkCreditsUsed(ctx)
				}
			}
			requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)
			httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, requestPayload, true, opts.Alt, baseURL, helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata))
			if errReq != nil {
				err = errReq
				return resp, err
			}

			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errDo)
				if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
					return resp, errDo
				}
				lastStatus = 0
				lastBody = nil
				lastErr = errDo
				if idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				err = errDo
				return resp, err
			}
			helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
			if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
				bodyBytes, errRead := io.ReadAll(httpResp.Body)
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("antigravity executor: close response body error: %v", errClose)
				}
				if errRead != nil {
					helps.RecordAPIResponseError(ctx, e.cfg, errRead)
					if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
						err = errRead
						return resp, err
					}
					if errCtx := ctx.Err(); errCtx != nil {
						err = errCtx
						return resp, err
					}
					lastStatus = 0
					lastBody = nil
					lastErr = errRead
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					err = errRead
					return resp, err
				}
				helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
				if httpResp.StatusCode == http.StatusTooManyRequests {
					decision := decideAntigravity429(bodyBytes)

					switch decision.kind {
					case antigravity429DecisionInstantRetrySameAuth:
						if attempt+1 < attempts {
							if decision.retryAfter != nil && *decision.retryAfter > 0 {
								wait := antigravityInstantRetryDelay(*decision.retryAfter)
								log.Debugf("antigravity executor: instant retry for model %s, waiting %s", baseModel, wait)
								if errWait := antigravityWait(ctx, wait); errWait != nil {
									return resp, errWait
								}
							}
							continue attemptLoop
						}
					case antigravity429DecisionShortCooldownSwitchAuth:
						closeAntigravityAuthIdleTransports(auth)
						if decision.retryAfter != nil && *decision.retryAfter > 0 && !antigravityCoolingDisabled(auth, e.cfg) {
							markAntigravityShortCooldown(auth, baseModel, time.Now(), *decision.retryAfter)
							log.Debugf("antigravity executor: short quota cooldown (%s) for model %s, recorded cooldown", *decision.retryAfter, baseModel)
						}
					case antigravity429DecisionFullQuotaExhausted:
						closeAntigravityAuthIdleTransports(auth)
						if useCredits && antigravityHasExplicitCreditsBalanceExhaustedReason(bodyBytes) && !antigravityCoolingDisabled(auth, e.cfg) {
							markAntigravityCreditsPermanentlyDisabled(auth)
						}
						// No credits logic - just fall through to error return below
					}
				}

				lastStatus = httpResp.StatusCode
				lastBody = append([]byte(nil), bodyBytes...)
				lastErr = nil
				if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				if antigravityShouldRetryTransientResourceExhausted429(httpResp.StatusCode, bodyBytes) && attempt+1 < attempts {
					delay := antigravityTransient429RetryDelay(attempt)
					log.Debugf("antigravity executor: transient 429 resource exhausted for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
					if errWait := antigravityWait(ctx, delay); errWait != nil {
						return resp, errWait
					}
					continue attemptLoop
				}
				if antigravityShouldRetryNoCapacity(httpResp.StatusCode, bodyBytes) {
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					if attempt+1 < attempts {
						delay := antigravityNoCapacityRetryDelay(attempt)
						log.Debugf("antigravity executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return resp, errWait
						}
						continue attemptLoop
					}
				}
				if antigravityShouldRetrySoftRateLimit(httpResp.StatusCode, bodyBytes) {
					if attempt+1 < attempts {
						delay := antigravitySoftRateLimitDelay(attempt)
						log.Debugf("antigravity executor: soft rate limit for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return resp, errWait
						}
						continue attemptLoop
					}
				}
				err = newAntigravityStatusErr(httpResp.StatusCode, bodyBytes)
				return resp, err
			}

			// Stream success
			if useCredits {
				clearAntigravityCreditsFailureState(auth)
			}
			out := make(chan cliproxyexecutor.StreamChunk)
			go func(resp *http.Response) {
				defer close(out)
				defer func() {
					if errClose := resp.Body.Close(); errClose != nil {
						log.Errorf("antigravity executor: close response body error: %v", errClose)
					}
				}()
				scanner := bufio.NewScanner(resp.Body)
				scanner.Buffer(nil, streamScannerBuffer)
				for scanner.Scan() {
					line := scanner.Bytes()
					helps.AppendAPIResponseChunk(ctx, e.cfg, line)

					// Filter usage metadata for all models
					// Only retain usage statistics in the terminal chunk
					line = helps.FilterSSEUsageMetadata(line)

					payload := helps.JSONPayload(line)
					if payload == nil {
						continue
					}
					reporter.ObserveResponseModel(payload)

					if detail, ok := helps.ParseAntigravityStreamUsage(payload); ok {
						reporter.Publish(ctx, detail)
					}

					out <- cliproxyexecutor.StreamChunk{Payload: payload}
				}
				if errScan := scanner.Err(); errScan != nil {
					helps.RecordAPIResponseError(ctx, e.cfg, errScan)
					reporter.PublishFailure(ctx, errScan)
					out <- cliproxyexecutor.StreamChunk{Err: errScan}
				} else {
					reporter.EnsurePublished(ctx)
				}
			}(httpResp)

			var buffer bytes.Buffer
			for chunk := range out {
				if chunk.Err != nil {
					return resp, chunk.Err
				}
				if len(chunk.Payload) > 0 {
					_, _ = buffer.Write(chunk.Payload)
					_, _ = buffer.Write([]byte("\n"))
				}
			}
			resp = cliproxyexecutor.Response{Payload: e.convertStreamToNonStream(buffer.Bytes())}

			resp.Payload = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, translated, resp.Payload)
			reporter.ObserveResponseModel(resp.Payload)
			reporter.Publish(ctx, helps.ParseAntigravityUsage(resp.Payload))
			var param any
			converted := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, resp.Payload, &param)
			if from == sdktranslator.FormatOpenAIResponse {
				converted = helps.EnsureResponsesUsageDetails(converted)
			}
			resp = cliproxyexecutor.Response{Payload: converted, Headers: httpResp.Header.Clone()}
			reporter.EnsurePublished(ctx)

			return resp, nil
		}

		switch {
		case lastStatus != 0:
			err = newAntigravityStatusErr(lastStatus, lastBody)
		case lastErr != nil:
			err = lastErr
		default:
			err = statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
		}
		return resp, err
	}

	return resp, err
}

func (e *AntigravityExecutor) convertStreamToNonStream(stream []byte) []byte {
	responseTemplate := ""
	var traceID string
	var finishReason string
	var modelVersion string
	var responseID string
	var role string
	var usageRaw string
	parts := make([]map[string]interface{}, 0)
	var pendingKind string
	var pendingText strings.Builder
	var pendingThoughtSig string

	flushPending := func() {
		if pendingKind == "" {
			return
		}
		text := pendingText.String()
		switch pendingKind {
		case "text":
			if strings.TrimSpace(text) == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			parts = append(parts, map[string]interface{}{"text": text})
		case "thought":
			if strings.TrimSpace(text) == "" && pendingThoughtSig == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			part := map[string]interface{}{"thought": true}
			part["text"] = text
			if pendingThoughtSig != "" {
				part["thoughtSignature"] = pendingThoughtSig
			}
			parts = append(parts, part)
		}
		pendingKind = ""
		pendingText.Reset()
		pendingThoughtSig = ""
	}

	normalizePart := func(partResult gjson.Result) map[string]interface{} {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(partResult.Raw), &m)
		if m == nil {
			m = map[string]interface{}{}
		}
		sig := partResult.Get("thoughtSignature").String()
		if sig == "" {
			sig = partResult.Get("thought_signature").String()
		}
		if sig != "" {
			m["thoughtSignature"] = sig
			delete(m, "thought_signature")
		}
		if inlineData, ok := m["inline_data"]; ok {
			m["inlineData"] = inlineData
			delete(m, "inline_data")
		}
		return m
	}

	for _, line := range bytes.Split(stream, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !gjson.ValidBytes(trimmed) {
			continue
		}

		root := gjson.ParseBytes(trimmed)
		responseNode := root.Get("response")
		if !responseNode.Exists() {
			if root.Get("candidates").Exists() {
				responseNode = root
			} else {
				continue
			}
		}
		responseTemplate = responseNode.Raw

		if traceResult := root.Get("traceId"); traceResult.Exists() && traceResult.String() != "" {
			traceID = traceResult.String()
		}

		if roleResult := responseNode.Get("candidates.0.content.role"); roleResult.Exists() {
			role = roleResult.String()
		}

		if finishResult := responseNode.Get("candidates.0.finishReason"); finishResult.Exists() && finishResult.String() != "" {
			finishReason = finishResult.String()
		}

		if modelResult := responseNode.Get("modelVersion"); modelResult.Exists() && modelResult.String() != "" {
			modelVersion = modelResult.String()
		}
		if responseIDResult := responseNode.Get("responseId"); responseIDResult.Exists() && responseIDResult.String() != "" {
			responseID = responseIDResult.String()
		}
		if usageResult := responseNode.Get("usageMetadata"); usageResult.Exists() {
			usageRaw = usageResult.Raw
		} else if usageMetadataResult := root.Get("usageMetadata"); usageMetadataResult.Exists() {
			usageRaw = usageMetadataResult.Raw
		}

		if partsResult := responseNode.Get("candidates.0.content.parts"); partsResult.IsArray() {
			for _, part := range partsResult.Array() {
				hasFunctionCall := part.Get("functionCall").Exists()
				hasInlineData := part.Get("inlineData").Exists() || part.Get("inline_data").Exists()
				sig := part.Get("thoughtSignature").String()
				if sig == "" {
					sig = part.Get("thought_signature").String()
				}
				text := part.Get("text").String()
				thought := part.Get("thought").Bool()

				if hasFunctionCall || hasInlineData {
					flushPending()
					parts = append(parts, normalizePart(part))
					continue
				}

				if thought || part.Get("text").Exists() {
					kind := "text"
					if thought {
						kind = "thought"
					}
					if pendingKind != "" && pendingKind != kind {
						flushPending()
					}
					pendingKind = kind
					pendingText.WriteString(text)
					if kind == "thought" && sig != "" {
						pendingThoughtSig = sig
					}
					continue
				}

				flushPending()
				parts = append(parts, normalizePart(part))
			}
		}
	}
	flushPending()

	if responseTemplate == "" {
		responseTemplate = `{"candidates":[{"content":{"role":"model","parts":[]}}]}`
	}

	partsJSON, _ := json.Marshal(parts)
	updatedTemplate, _ := sjson.SetRawBytes([]byte(responseTemplate), "candidates.0.content.parts", partsJSON)
	responseTemplate = string(updatedTemplate)
	if role != "" {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "candidates.0.content.role", role)
		responseTemplate = string(updatedTemplate)
	}
	if finishReason != "" {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "candidates.0.finishReason", finishReason)
		responseTemplate = string(updatedTemplate)
	}
	if modelVersion != "" {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "modelVersion", modelVersion)
		responseTemplate = string(updatedTemplate)
	}
	if responseID != "" {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "responseId", responseID)
		responseTemplate = string(updatedTemplate)
	}
	if usageRaw != "" {
		updatedTemplate, _ = sjson.SetRawBytes([]byte(responseTemplate), "usageMetadata", []byte(usageRaw))
		responseTemplate = string(updatedTemplate)
	} else if !gjson.Get(responseTemplate, "usageMetadata").Exists() {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "usageMetadata.promptTokenCount", 0)
		responseTemplate = string(updatedTemplate)
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "usageMetadata.candidatesTokenCount", 0)
		responseTemplate = string(updatedTemplate)
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "usageMetadata.totalTokenCount", 0)
		responseTemplate = string(updatedTemplate)
	}

	output := `{"response":{},"traceId":""}`
	updatedOutput, _ := sjson.SetRawBytes([]byte(output), "response", []byte(responseTemplate))
	output = string(updatedOutput)
	if traceID != "" {
		updatedOutput, _ = sjson.SetBytes([]byte(output), "traceId", traceID)
		output = string(updatedOutput)
	}
	return []byte(output)
}

// ExecuteStream performs a streaming request to the Antigravity API.
func (e *AntigravityExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if helps.HasResponsesCompactionItem(req.Payload) {
		expanded, errExpand := helps.ExpandAntigravityCompactionCapsules(req.Payload)
		if errExpand != nil {
			return nil, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
		}
		req.Payload = expanded
		if len(opts.OriginalRequest) > 0 {
			expandedOrig, errOrig := helps.ExpandAntigravityCompactionCapsules(opts.OriginalRequest)
			if errOrig == nil {
				opts.OriginalRequest = expandedOrig
			} else {
				opts.OriginalRequest = expanded
			}
		}
	}
	if helps.HasResponsesCompactionTrigger(req.Payload) || helps.HasResponsesCompactionTrigger(opts.OriginalRequest) {
		return e.executeCompactionStream(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	ctx = context.WithValue(ctx, "alt", "")
	if !antigravityCoolingDisabled(auth, e.cfg) {
		if inCooldown, remaining := antigravityIsInShortCooldown(auth, baseModel, time.Now()); inCooldown && !antigravityShouldBypassShortCooldown(ctx, e.cfg) {
			log.Debugf("antigravity executor: auth %s in short cooldown for model %s (%s remaining), returning 429 to switch auth", auth.ID, baseModel, remaining)
			d := remaining
			return nil, statusErr{code: http.StatusTooManyRequests, msg: fmt.Sprintf("auth in short cooldown, %s remaining", remaining), retryAfter: &d}
		}
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(from, originalPayload)
	if errValidate != nil {
		return nil, errValidate
	}
	req.Payload = originalPayload
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Stream: true, ModelInfo: modelInfo}
	originalTranslated, translated := helps.TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq, originalPayload, req.Payload)

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "antigravity", from.String(), "request", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	// Capture the effective tier after the last payload rule has run.
	reporter.SetEffectiveServiceTier(translated)
	translated, replayScope, errReplay := prepareAntigravityGeminiReasoningReplayPayload(ctx, baseModel, req, opts, translated)
	if errReplay != nil {
		return nil, errReplay
	}

	useCredits := cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(e.cfg)

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)

	attempts := antigravityRetryAttempts(auth, e.cfg)

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lastStatus int
		var lastBody []byte
		var lastErr error

		for idx, baseURL := range baseURLs {
			requestPayload := translated
			if useCredits {
				if cp := injectEnabledCreditTypes(translated); len(cp) > 0 {
					requestPayload = cp
					helps.MarkCreditsUsed(ctx)
				}
			}
			requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)
			httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, requestPayload, true, opts.Alt, baseURL, helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata))
			if errReq != nil {
				err = errReq
				return nil, err
			}
			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errDo)
				if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
					return nil, errDo
				}
				lastStatus = 0
				lastBody = nil
				lastErr = errDo
				if idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				err = errDo
				return nil, err
			}
			helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
			if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
				bodyBytes, errRead := io.ReadAll(httpResp.Body)
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("antigravity executor: close response body error: %v", errClose)
				}
				if errRead != nil {
					helps.RecordAPIResponseError(ctx, e.cfg, errRead)
					if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
						err = errRead
						return nil, err
					}
					if errCtx := ctx.Err(); errCtx != nil {
						err = errCtx
						return nil, err
					}
					lastStatus = 0
					lastBody = nil
					lastErr = errRead
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					err = errRead
					return nil, err
				}
				helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
				if httpResp.StatusCode == http.StatusTooManyRequests {
					decision := decideAntigravity429(bodyBytes)

					switch decision.kind {
					case antigravity429DecisionInstantRetrySameAuth:
						if attempt+1 < attempts {
							if decision.retryAfter != nil && *decision.retryAfter > 0 {
								wait := antigravityInstantRetryDelay(*decision.retryAfter)
								log.Debugf("antigravity executor: instant retry for model %s, waiting %s", baseModel, wait)
								if errWait := antigravityWait(ctx, wait); errWait != nil {
									return nil, errWait
								}
							}
							continue attemptLoop
						}
					case antigravity429DecisionShortCooldownSwitchAuth:
						closeAntigravityAuthIdleTransports(auth)
						if decision.retryAfter != nil && *decision.retryAfter > 0 && !antigravityCoolingDisabled(auth, e.cfg) {
							markAntigravityShortCooldown(auth, baseModel, time.Now(), *decision.retryAfter)
							log.Debugf("antigravity executor: short quota cooldown (%s) for model %s recorded", *decision.retryAfter, baseModel)
						}
					case antigravity429DecisionFullQuotaExhausted:
						closeAntigravityAuthIdleTransports(auth)
						if useCredits && antigravityHasExplicitCreditsBalanceExhaustedReason(bodyBytes) && !antigravityCoolingDisabled(auth, e.cfg) {
							markAntigravityCreditsPermanentlyDisabled(auth)
						}
						// No credits logic - just fall through to error return below
					}
				}

				lastStatus = httpResp.StatusCode
				lastBody = append([]byte(nil), bodyBytes...)
				lastErr = nil
				if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				if antigravityShouldRetryTransientResourceExhausted429(httpResp.StatusCode, bodyBytes) && attempt+1 < attempts {
					delay := antigravityTransient429RetryDelay(attempt)
					log.Debugf("antigravity executor: transient 429 resource exhausted for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
					if errWait := antigravityWait(ctx, delay); errWait != nil {
						return nil, errWait
					}
					continue attemptLoop
				}
				if antigravityShouldRetryNoCapacity(httpResp.StatusCode, bodyBytes) {
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					if attempt+1 < attempts {
						delay := antigravityNoCapacityRetryDelay(attempt)
						log.Debugf("antigravity executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return nil, errWait
						}
						continue attemptLoop
					}
				}
				if antigravityShouldRetrySoftRateLimit(httpResp.StatusCode, bodyBytes) {
					if attempt+1 < attempts {
						delay := antigravitySoftRateLimitDelay(attempt)
						log.Debugf("antigravity executor: soft rate limit for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return nil, errWait
						}
						continue attemptLoop
					}
				}
				err = newAntigravityStatusErr(httpResp.StatusCode, bodyBytes)
				return nil, err
			}

			// Stream success
			if useCredits {
				clearAntigravityCreditsFailureState(auth)
			}
			out := make(chan cliproxyexecutor.StreamChunk)
			replayAccumulator := newAntigravityReasoningReplayAccumulator(replayScope, requestPayload)
			go func(resp *http.Response) {
				defer close(out)
				defer func() {
					if errClose := resp.Body.Close(); errClose != nil {
						log.Errorf("antigravity executor: close response body error: %v", errClose)
					}
				}()
				scanner := bufio.NewScanner(resp.Body)
				scanner.Buffer(nil, streamScannerBuffer)
				claudeInputTokens := helps.NewClaudeInputTokenState(from, to, from, originalPayload)
				var param any
				for scanner.Scan() {
					line := scanner.Bytes()
					replayAccumulator.ObserveSSELine(line)
					helps.AppendAPIResponseChunk(ctx, e.cfg, line)

					// Filter usage metadata for all models
					// Only retain usage statistics in the terminal chunk
					line = helps.FilterSSEUsageMetadata(line)

					payload := helps.JSONPayload(line)
					if payload == nil {
						continue
					}
					reporter.ObserveResponseModel(payload)

					if detail, ok := helps.ParseAntigravityStreamUsage(payload); ok {
						reporter.Publish(ctx, detail)
					}

					payload = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, translated, payload)
					chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(payload), &param, claudeInputTokens)
					for i := range chunks {
						select {
						case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
						case <-ctx.Done():
							return
						}
					}
				}
				tail := helps.TranslateStreamWithClaudeInputTokens(ctx, to, from, req.Model, opts.OriginalRequest, translated, []byte("[DONE]"), &param, claudeInputTokens)
				for i := range tail {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: tail[i]}:
					case <-ctx.Done():
						return
					}
				}
				if errScan := scanner.Err(); errScan != nil {
					helps.RecordAPIResponseError(ctx, e.cfg, errScan)
					reporter.PublishFailure(ctx, errScan)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
					case <-ctx.Done():
					}
				} else {
					replayAccumulator.Commit(ctx)
					reporter.EnsurePublished(ctx)
				}
			}(httpResp)
			return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
		}

		switch {
		case lastStatus != 0:
			err = newAntigravityStatusErr(lastStatus, lastBody)
		case lastErr != nil:
			err = lastErr
		default:
			err = statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
		}
		return nil, err
	}

	return nil, err
}

// executeCompactionStream runs a non-stream summary generation upstream and
// replays the sealed compaction capsule as a Responses SSE stream.
func (e *AntigravityExecutor) executeCompactionStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	payload := req.Payload
	if len(payload) == 0 && len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(payload, baseModel)

	summaryReq := cliproxyexecutor.Request{
		Model:    req.Model,
		Payload:  summaryPayload,
		Metadata: req.Metadata,
	}
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse

	summaryResp, errSummary := e.Execute(ctx, auth, summaryReq, summaryOpts)
	if errSummary != nil {
		return nil, errSummary
	}

	summaryText, errExtract := helps.ExtractAntigravitySummaryText(summaryResp.Payload)
	if errExtract != nil {
		return nil, fmt.Errorf("extract summary: %w", errExtract)
	}
	capsule, errSeal := helps.SealAntigravityCompaction(summaryText, baseModel)
	if errSeal != nil {
		return nil, fmt.Errorf("seal compaction capsule: %w", errSeal)
	}

	inputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.input_tokens").Int())
	outputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.output_tokens").Int())
	totalTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.total_tokens").Int())
	if totalTokens == 0 && inputTokens == 0 {
		usage := helps.ParseOpenAIUsage(summaryResp.Payload)
		inputTokens = int(usage.InputTokens)
		outputTokens = int(usage.OutputTokens)
		totalTokens = int(usage.TotalTokens)
	}

	chunks := helps.BuildAntigravityCompactionStreamChunks(baseModel, capsule, inputTokens, outputTokens, totalTokens)
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)

	headers := summaryResp.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")

	return &cliproxyexecutor.StreamResult{
		Headers: headers,
		Chunks:  out,
	}, nil
}

// Refresh refreshes the authentication credentials using the refresh token.
func (e *AntigravityExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return auth, nil
	}
	updated, errRefresh := e.refreshToken(ctx, auth.Clone())
	if errRefresh != nil {
		return nil, errRefresh
	}
	return updated, nil
}

func (e *AntigravityExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	return antigravityProjectIDFromAuth(auth) == ""
}

func (e *AntigravityExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil || !e.ShouldPrepareRequestAuth(auth) {
		return nil, nil
	}

	updated := auth.Clone()
	token, refreshedAuth, errToken := e.ensureAccessToken(ctx, updated)
	if errToken != nil {
		return nil, errToken
	}
	if refreshedAuth != nil {
		updated = refreshedAuth
	}
	if antigravityProjectIDFromAuth(updated) != "" {
		return updated, nil
	}

	projectID, errProject := e.fetchAntigravityProjectID(ctx, updated, token)
	if errProject != nil {
		return nil, missingAntigravityProjectIDError(errProject)
	}
	if projectID == "" {
		return nil, missingAntigravityProjectIDError(nil)
	}
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["project_id"] = projectID
	return updated, nil
}

// CountTokens counts tokens for the given request using the Antigravity API.
func (e *AntigravityExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")
	respCtx := context.WithValue(ctx, "alt", opts.Alt)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayloadSource, errValidate := validateAntigravityRequestSignatures(from, originalPayloadSource)
	if errValidate != nil {
		return cliproxyexecutor.Response{}, errValidate
	}
	req.Payload = originalPayloadSource
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return cliproxyexecutor.Response{}, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	if strings.TrimSpace(token) == "" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	// Prepare payload once (doesn't depend on baseURL)
	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Body: req.Payload, ModelInfo: modelInfo}
	payload := helps.TranslateRequestEnvelopeWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq).Body

	payload, err := helps.ApplyRequestThinking(payload, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	payload = deleteJSONField(payload, "project")
	payload = deleteJSONField(payload, "model")
	payload = deleteJSONField(payload, "request.safetySettings")
	// Strip stateful session fields before dispatching countTokens (upstream
	// d0fb44ca95e8): the endpoint rejects toolConfig/labels/sessionId payloads.
	payload = deleteJSONField(payload, "request.toolConfig")
	payload = deleteJSONField(payload, "request.labels")
	payload = deleteJSONField(payload, "request.sessionId")
	payload = ensureAntigravityGeminiLeadingUserContent(baseModel, payload)

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	var lastStatus int
	var lastBody []byte
	var lastErr error

	for idx, baseURL := range baseURLs {
		base := strings.TrimSuffix(baseURL, "/")
		if base == "" {
			base = buildBaseURL(auth)
		}

		var requestURL strings.Builder
		requestURL.WriteString(base)
		requestURL.WriteString(antigravityCountTokensPath)
		if opts.Alt != "" {
			requestURL.WriteString("?$alt=")
			requestURL.WriteString(url.QueryEscape(opts.Alt))
		}

		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payload))
		if errReq != nil {
			return cliproxyexecutor.Response{}, errReq
		}
		httpReq.Close = false
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
		if host := resolveHost(base); host != "" {
			httpReq.Host = host
		}
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       requestURL.String(),
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      payload,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errDo)
			if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
				return cliproxyexecutor.Response{}, errDo
			}
			lastStatus = 0
			lastBody = nil
			lastErr = errDo
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return cliproxyexecutor.Response{}, errDo
		}

		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return cliproxyexecutor.Response{}, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)

		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			count := gjson.GetBytes(bodyBytes, "totalTokens").Int()
			translated := sdktranslator.TranslateTokenCount(respCtx, to, from, count, bodyBytes)
			return cliproxyexecutor.Response{Payload: translated, Headers: httpResp.Header.Clone()}, nil
		}

		lastStatus = httpResp.StatusCode
		lastBody = append([]byte(nil), bodyBytes...)
		lastErr = nil
		if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
			log.Debugf("antigravity executor: rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
			continue
		}
		sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		if httpResp.StatusCode == http.StatusTooManyRequests {
			closeAntigravityAuthIdleTransports(auth)
			if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return cliproxyexecutor.Response{}, sErr
	}

	switch {
	case lastStatus != 0:
		sErr := statusErr{code: lastStatus, msg: string(lastBody)}
		if lastStatus == http.StatusTooManyRequests {
			if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return cliproxyexecutor.Response{}, sErr
	case lastErr != nil:
		return cliproxyexecutor.Response{}, lastErr
	default:
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
	}
}

func (e *AntigravityExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, *cliproxyauth.Auth, error) {
	if auth == nil {
		return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	expiry := tokenExpiry(auth.Metadata)
	if accessToken != "" && expiry.After(time.Now().Add(antigravityRequestTokenSafetyWindow)) {
		e.maybeRefreshAntigravityCreditsHint(ctx, auth, accessToken)
		return accessToken, nil, nil
	}
	refreshCtx := context.Background()
	if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			refreshCtx = context.WithValue(refreshCtx, "cliproxy.roundtripper", rt)
		}
	}
	if refreshed, handled, err := helps.RefreshAuthViaHome(refreshCtx, e.cfg, auth); handled {
		if err != nil {
			return "", nil, err
		}
		token := metaStringValue(refreshed.Metadata, "access_token")
		if strings.TrimSpace(token) == "" {
			return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
		}
		e.maybeRefreshAntigravityCreditsHint(ctx, refreshed, token)
		return token, refreshed, nil
	}

	updated, errRefresh := e.refreshToken(refreshCtx, auth.Clone())
	if errRefresh != nil {
		return "", nil, errRefresh
	}
	return metaStringValue(updated.Metadata, "access_token"), updated, nil
}

func (e *AntigravityExecutor) maybeRefreshAntigravityCreditsHint(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) {
	if e == nil || auth == nil || !antigravityCreditsRetryEnabled(e.cfg) || antigravityCoolingDisabled(auth, e.cfg) {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	if hint, ok := cliproxyauth.GetAntigravityCreditsHint(authID); ok && cliproxyauth.IsAntigravityCreditsHintFresh(hint, time.Now()) {
		return
	}
	if strings.TrimSpace(accessToken) == "" {
		accessToken = metaStringValue(auth.Metadata, "access_token")
	}
	if strings.TrimSpace(accessToken) == "" {
		return
	}

	e.queueAntigravityCreditsRefresh(ctx, auth, accessToken, antigravityCreditsHintRefreshTimeout)
}

func (e *AntigravityExecutor) queueAntigravityCreditsRefresh(ctx context.Context, auth *cliproxyauth.Auth, accessToken string, timeout time.Duration) {
	if e == nil || auth == nil || (ctx != nil && ctx.Err() != nil) {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" || strings.TrimSpace(accessToken) == "" {
		return
	}
	state := &antigravityCreditsHintRefreshState{}
	if existing, loaded := antigravityCreditsHintRefreshByID.LoadOrStore(authID, state); loaded {
		if cast, ok := existing.(*antigravityCreditsHintRefreshState); ok && cast != nil {
			state = cast
		} else {
			antigravityCreditsHintRefreshByID.Delete(authID)
			antigravityCreditsHintRefreshByID.Store(authID, state)
		}
	}

	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	state.mu.Lock()
	if ctx.Err() != nil || auth.RegistrationEpoch < state.registrationEpoch {
		state.mu.Unlock()
		return
	}
	oldTask := state.task
	if auth.RegistrationEpoch != state.registrationEpoch {
		state.registrationEpoch = auth.RegistrationEpoch
		state.lastAttempt = time.Time{}
		state.task = nil
	} else if oldTask != nil {
		if oldTask.ctx.Err() == nil {
			state.mu.Unlock()
			return
		}
		if oldTask.lifecycle.Err() != nil {
			state.lastAttempt = time.Time{}
		}
		state.task = nil
	}
	if !state.lastAttempt.IsZero() && now.Sub(state.lastAttempt) < antigravityCreditsHintRefreshInterval {
		state.mu.Unlock()
		if oldTask != nil {
			oldTask.cancel()
		}
		return
	}

	lifecycle := ctx
	var refreshCtx context.Context
	var cancelRefresh context.CancelFunc
	if timeout > 0 {
		// Keep the existing independent warm-hint budget and transport selection.
		lifecycle = context.Background()
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			lifecycle = context.WithValue(lifecycle, "cliproxy.roundtripper", rt)
		}
		refreshCtx, cancelRefresh = context.WithTimeout(lifecycle, timeout)
	} else {
		// Optional post-refresh work follows its lifecycle without adding a timer.
		refreshCtx, cancelRefresh = context.WithCancel(lifecycle)
	}
	task := &antigravityCreditsRefreshTask{
		state:     state,
		ctx:       refreshCtx,
		lifecycle: lifecycle,
		cancel:    cancelRefresh,
		done:      make(chan struct{}),
	}
	state.task = task
	state.lastAttempt = now
	state.mu.Unlock()
	if oldTask != nil {
		oldTask.cancel()
	}
	authCopy := auth.Clone()

	go func() {
		defer close(task.done)
		defer task.cancel()
		defer func() {
			state.mu.Lock()
			if state.task == task {
				state.task = nil
				if task.lifecycle.Err() != nil {
					state.lastAttempt = time.Time{}
				}
			}
			state.mu.Unlock()
		}()
		e.updateAntigravityCreditsBalanceForTask(task.ctx, authCopy, accessToken, task)
	}()
}

func (e *AntigravityExecutor) refreshToken(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, statusErr{code: http.StatusUnauthorized, msg: "missing refresh token"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	refreshToken = strings.TrimSpace(refreshToken)

	result, errRefresh, _ := antigravityRefreshGroup.Do(refreshToken, func() (interface{}, error) {
		// A caller may leave, but shared credential acquisition must still be bounded.
		refreshCtx, cancelRefresh := context.WithTimeout(context.WithoutCancel(ctx), antigravityCredentialAcquisitionTimeout)
		defer cancelRefresh()
		return e.refreshTokenSingleFlight(refreshCtx, auth, refreshToken)
	})
	if errRefresh != nil {
		return auth, errRefresh
	}
	tokenResp, ok := result.(*antigravityTokenRefreshData)
	if !ok || tokenResp == nil {
		return auth, fmt.Errorf("antigravity token refresh failed: invalid single-flight result")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenResp.RefreshToken
	}
	auth.Metadata["expires_in"] = tokenResp.ExpiresIn
	now := time.Now()
	auth.Metadata["timestamp"] = now.UnixMilli()
	auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	auth.Metadata["type"] = antigravityAuthType
	if errProject := e.ensureAntigravityProjectID(ctx, auth, tokenResp.AccessToken); errProject != nil {
		log.Warnf("antigravity executor: ensure project id failed: %v", errProject)
	}
	e.queueAntigravityCreditsRefresh(ctx, auth, tokenResp.AccessToken, 0)
	return auth, nil
}

func (e *AntigravityExecutor) refreshTokenSingleFlight(ctx context.Context, auth *cliproxyauth.Auth, refreshToken string) (*antigravityTokenRefreshData, error) {
	form := url.Values{}
	form.Set("client_id", antigravityClientID)
	form.Set("client_secret", antigravityClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("Host", "oauth2.googleapis.com")
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Real Antigravity uses Go's default User-Agent for OAuth token refresh
	httpReq.Header.Set("User-Agent", "Go-http-client/2.0")

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return nil, errRead
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		if httpResp.StatusCode == http.StatusTooManyRequests {
			if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return nil, sErr
	}

	var tokenResp antigravityTokenRefreshData
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return &tokenResp, nil

}

type antigravityTokenRefreshData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (e *AntigravityExecutor) ensureAntigravityProjectID(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) error {
	if auth == nil {
		return nil
	}

	if antigravityProjectIDFromAuth(auth) != "" {
		return nil
	}

	projectID, errFetch := e.fetchAntigravityProjectID(ctx, auth, accessToken)
	if errFetch != nil {
		return errFetch
	}
	if projectID == "" {
		return nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["project_id"] = projectID

	return nil
}

func (e *AntigravityExecutor) fetchAntigravityProjectID(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) (string, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return "", nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	// Project discovery is required credential preparation, not a generation request.
	acquisitionCtx, cancelAcquisition := context.WithTimeout(ctx, antigravityCredentialAcquisitionTimeout)
	defer cancelAcquisition()
	httpClient := newAntigravityHTTPClient(acquisitionCtx, e.cfg, auth, 0)
	projectID, errFetch := sdkAuth.FetchAntigravityProjectID(acquisitionCtx, token, httpClient)
	if errFetch != nil {
		return "", errFetch
	}
	return strings.TrimSpace(projectID), nil
}

func (e *AntigravityExecutor) projectIDForRequest(_ context.Context, auth *cliproxyauth.Auth, _ string) (string, error) {
	if projectID := antigravityProjectIDFromAuth(auth); projectID != "" {
		return projectID, nil
	}
	return "", missingAntigravityProjectIDError(nil)
}

func antigravityProjectIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if pid, ok := auth.Metadata["project_id"].(string); ok {
		return strings.TrimSpace(pid)
	}
	return ""
}

func missingAntigravityProjectIDError(cause error) statusErr {
	msg := "antigravity auth missing project_id"
	statusCode := http.StatusBadRequest
	var retryAfter *time.Duration
	if cause != nil {
		msg = fmt.Sprintf("%s: %v", msg, cause)
		// Preserve the upstream status (e.g. 403 on project discovery denial) and
		// any retry hint instead of flattening every failure to a client 400.
		// Ported from upstream CLIProxyAPI v7.3.6.
		type statusCoder interface {
			StatusCode() int
		}
		var sc statusCoder
		if errors.As(cause, &sc) && sc != nil {
			if code := sc.StatusCode(); code > 0 {
				statusCode = code
			}
		}
		type retryAfterProvider interface {
			RetryAfter() *time.Duration
		}
		var rap retryAfterProvider
		if errors.As(cause, &rap) && rap != nil {
			retryAfter = rap.RetryAfter()
		}
	}
	return statusErr{code: statusCode, msg: msg, retryAfter: retryAfter}
}

func (e *AntigravityExecutor) updateAntigravityCreditsBalance(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) {
	e.updateAntigravityCreditsBalanceForTask(ctx, auth, accessToken, nil)
}

func (e *AntigravityExecutor) updateAntigravityCreditsBalanceForTask(ctx context.Context, auth *cliproxyauth.Auth, accessToken string, task *antigravityCreditsRefreshTask) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return
	}

	userAgent := resolveLoadCodeAssistUserAgent(auth)
	loadReqBody, errMarshal := json.Marshal(map[string]any{
		"metadata": map[string]string{
			"ideType": "ANTIGRAVITY",
		},
	})
	if errMarshal != nil {
		log.Debugf("antigravity executor: marshal loadCodeAssist request error: %v", errMarshal)
		return
	}
	baseURL := antigravityLoadCodeAssistBaseURL(auth)
	endpointURL := strings.TrimSuffix(baseURL, "/") + "/v1internal:loadCodeAssist"
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(loadReqBody))
	if errReq != nil {
		log.Debugf("antigravity executor: create loadCodeAssist request error: %v", errReq)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		log.Debugf("antigravity executor: loadCodeAssist request error: %v", errDo)
		return
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close loadCodeAssist response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil || httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("antigravity executor: loadCodeAssist returned status %d, err=%v", httpResp.StatusCode, errRead)
		return
	}

	authID := strings.TrimSpace(auth.ID)
	publish := func(balance *antigravityCreditsBalance, hint cliproxyauth.AntigravityCreditsHint) {
		if task != nil {
			state := task.state
			state.mu.Lock()
			defer state.mu.Unlock()
			current, _ := antigravityCreditsHintRefreshByID.Load(authID)
			if current != state || state.task != task || state.registrationEpoch != auth.RegistrationEpoch {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		if balance != nil {
			antigravityCreditsBalanceByAuth.Store(authID, *balance)
		}
		cliproxyauth.SetAntigravityCreditsHint(authID, hint)
		if balance != nil && hint.Available {
			clearAntigravityCreditsPermanentlyDisabled(auth)
		}
	}
	paidTierID := strings.TrimSpace(gjson.GetBytes(bodyBytes, "paidTier.id").String())

	credits := gjson.GetBytes(bodyBytes, "paidTier.availableCredits")
	if !credits.IsArray() {
		publish(nil, cliproxyauth.AntigravityCreditsHint{
			Known:      true,
			Available:  false,
			PaidTierID: paidTierID,
			UpdatedAt:  time.Now(),
		})
		return
	}
	for _, credit := range credits.Array() {
		if !strings.EqualFold(credit.Get("creditType").String(), "GOOGLE_ONE_AI") {
			continue
		}
		creditAmount, errCA := strconv.ParseFloat(strings.TrimSpace(credit.Get("creditAmount").String()), 64)
		if errCA != nil {
			continue
		}
		minAmount, errMA := strconv.ParseFloat(strings.TrimSpace(credit.Get("minimumCreditAmountForUsage").String()), 64)
		if errMA != nil {
			continue
		}
		bal := antigravityCreditsBalance{
			CreditAmount:    creditAmount,
			MinCreditAmount: minAmount,
			PaidTierID:      paidTierID,
			Known:           true,
		}
		publish(&bal, cliproxyauth.AntigravityCreditsHint{
			Known:           true,
			Available:       creditAmount >= minAmount,
			CreditAmount:    creditAmount,
			MinCreditAmount: minAmount,
			PaidTierID:      paidTierID,
			UpdatedAt:       time.Now(),
		})
		return
	}
}

func (e *AntigravityExecutor) buildRequest(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, payload []byte, stream bool, alt, baseURL string, derivedSessionIDs ...string) (*http.Request, error) {
	if token == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = buildBaseURL(auth)
	}
	path := antigravityGeneratePath
	if stream {
		path = antigravityStreamPath
	}
	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(path)
	if stream {
		if alt != "" {
			requestURL.WriteString("?$alt=")
			requestURL.WriteString(url.QueryEscape(alt))
		} else {
			requestURL.WriteString("?alt=sse")
		}
	} else if alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(alt))
	}

	projectID, errProject := e.projectIDForRequest(ctx, auth, token)
	if errProject != nil {
		return nil, errProject
	}
	payload = geminiToAntigravity(modelName, payload, projectID, derivedSessionIDs...)
	payload, _ = sjson.SetBytes(payload, "model", modelName)

	// Cap maxOutputTokens to model's max_completion_tokens from registry
	if maxOut := gjson.GetBytes(payload, "request.generationConfig.maxOutputTokens"); maxOut.Exists() && maxOut.Type == gjson.Number {
		if modelInfo := registry.LookupModelInfo(modelName, "antigravity"); modelInfo != nil && modelInfo.MaxCompletionTokens > 0 {
			if int(maxOut.Int()) > modelInfo.MaxCompletionTokens {
				payload, _ = sjson.SetBytes(payload, "request.generationConfig.maxOutputTokens", modelInfo.MaxCompletionTokens)
			}
		}
	}

	isClaudeModel := strings.Contains(modelName, "claude")
	useAntigravitySchema := isClaudeModel || strings.Contains(modelName, "gemini-3-pro") || strings.Contains(modelName, "gemini-3.1-pro")
	var (
		bodyReader io.Reader
		payloadLog []byte
	)
	if antigravityRequestNeedsSchemaSanitization(payload) {
		payloadStr := sanitizeAntigravityRequestSchemasForModel(string(payload), useAntigravitySchema, isClaudeModel)

		if isClaudeModel {
			updated, _ := sjson.SetBytes([]byte(payloadStr), "request.toolConfig.functionCallingConfig.mode", "VALIDATED")
			payloadStr = string(updated)
		} else {
			payloadStr, _ = sjson.Delete(payloadStr, "request.generationConfig.maxOutputTokens")
		}

		bodyReader = strings.NewReader(payloadStr)
		if e.cfg != nil && e.cfg.RequestLog {
			payloadLog = []byte(payloadStr)
		}
	} else {
		if strings.Contains(modelName, "claude") {
			payload, _ = sjson.SetBytes(payload, "request.toolConfig.functionCallingConfig.mode", "VALIDATED")
		} else {
			payload, _ = sjson.DeleteBytes(payload, "request.generationConfig.maxOutputTokens")
		}

		bodyReader = bytes.NewReader(payload)
		if e.cfg != nil && e.cfg.RequestLog {
			payloadLog = append([]byte(nil), payload...)
		}
	}

	// if useAntigravitySchema {
	// 	systemInstructionPartsResult := gjson.Get(payloadStr, "request.systemInstruction.parts")
	// 	payloadStr, _ = sjson.SetBytes([]byte(payloadStr), "request.systemInstruction.role", "user")
	// 	payloadStr, _ = sjson.SetBytes([]byte(payloadStr), "request.systemInstruction.parts.0.text", systemInstruction)
	// 	payloadStr, _ = sjson.SetBytes([]byte(payloadStr), "request.systemInstruction.parts.1.text", fmt.Sprintf("Please ignore following [ignore]%s[/ignore]", systemInstruction))

	// 	if systemInstructionPartsResult.Exists() && systemInstructionPartsResult.IsArray() {
	// 		for _, partResult := range systemInstructionPartsResult.Array() {
	// 			payloadStr, _ = sjson.SetRawBytes([]byte(payloadStr), "request.systemInstruction.parts.-1", []byte(partResult.Raw))
	// 		}
	// 	}
	// }

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bodyReader)
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Close = false
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       requestURL.String(),
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payloadLog,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	return httpReq, nil
}

func sanitizeAntigravityRequestSchemasForModel(payloadStr string, useAntigravitySchema, isClaudeModel bool) string {
	return util.SanitizeAntigravityRequestSchemasForModel(payloadStr, useAntigravitySchema, isClaudeModel)
}

var (
	antigravityGenerationConfigContainers = []string{
		"request.generationConfig", "request.generation_config",
	}
	antigravityGenerationSchemaKeys = []string{
		"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema",
	}
)

func antigravityRequestNeedsSchemaSanitization(payload []byte) bool {
	if gjson.GetBytes(payload, "request.tools.0").Exists() {
		return true
	}
	for _, container := range antigravityGenerationConfigContainers {
		for _, key := range antigravityGenerationSchemaKeys {
			if gjson.GetBytes(payload, container+"."+key).Exists() {
				return true
			}
		}
	}
	return false
}

func tokenExpiry(metadata map[string]any) time.Time {
	if metadata == nil {
		return time.Time{}
	}
	if expStr, ok := metadata["expired"].(string); ok {
		expStr = strings.TrimSpace(expStr)
		if expStr != "" {
			if parsed, errParse := time.Parse(time.RFC3339, expStr); errParse == nil {
				return parsed
			}
		}
	}
	expiresIn, hasExpires := int64Value(metadata["expires_in"])
	tsMs, hasTimestamp := int64Value(metadata["timestamp"])
	if hasExpires && hasTimestamp {
		return time.Unix(0, tsMs*int64(time.Millisecond)).Add(time.Duration(expiresIn) * time.Second)
	}
	return time.Time{}
}

func metaStringValue(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata[key]; ok {
		switch typed := v.(type) {
		case string:
			return strings.TrimSpace(typed)
		case []byte:
			return strings.TrimSpace(string(typed))
		}
	}
	return ""
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	case json.Number:
		if i, errParse := typed.Int64(); errParse == nil {
			return i, true
		}
	case string:
		if strings.TrimSpace(typed) == "" {
			return 0, false
		}
		if i, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); errParse == nil {
			return i, true
		}
	}
	return 0, false
}

func buildBaseURL(auth *cliproxyauth.Auth) string {
	if baseURLs := antigravityBaseURLFallbackOrder(auth); len(baseURLs) > 0 {
		return baseURLs[0]
	}
	return antigravityBaseURLDaily
}

func antigravityLoadCodeAssistBaseURL(auth *cliproxyauth.Auth) string {
	return buildBaseURL(auth)
}

func resolveHost(base string) string {
	parsed, errParse := url.Parse(base)
	if errParse != nil {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

func resolveUserAgent(auth *cliproxyauth.Auth) string {
	return misc.AntigravityRequestUserAgent(antigravityConfiguredUserAgent(auth))
}

func resolveLoadCodeAssistUserAgent(auth *cliproxyauth.Auth) string {
	return misc.AntigravityLoadCodeAssistUserAgent(antigravityConfiguredUserAgent(auth))
}

func antigravityConfiguredUserAgent(auth *cliproxyauth.Auth) string {
	raw := ""
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				raw = ua
			}
		}
		if raw == "" && auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
				raw = strings.TrimSpace(ua)
			}
		}
	}
	return raw
}

func antigravityRetryAttempts(auth *cliproxyauth.Auth, cfg *config.Config) int {
	retry := 0
	if cfg != nil {
		retry = cfg.RequestRetry
	}
	if auth != nil {
		if override, ok := auth.RequestRetryOverride(); ok {
			retry = override
		}
	}
	if retry < 0 {
		retry = 0
	}
	attempts := retry + 1
	if attempts < 1 {
		return 1
	}
	return attempts
}

func antigravityShouldRetryNoCapacity(statusCode int, body []byte) bool {
	if statusCode != http.StatusServiceUnavailable {
		return false
	}
	if len(body) == 0 {
		return false
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, cliproxyauth.KeywordNoCapacityAvailable)
}

func antigravityShouldRetryTransientResourceExhausted429(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	if len(body) == 0 {
		return false
	}
	if classifyAntigravity429(body) != antigravity429Unknown {
		return false
	}
	status := strings.TrimSpace(gjson.GetBytes(body, "error.status").String())
	if !strings.EqualFold(status, "RESOURCE_EXHAUSTED") {
		return false
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, cliproxyauth.KeywordResourceHasBeenExhausted)
}

func antigravityShouldRetrySoftRateLimit(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	return decideAntigravity429(body).kind == antigravity429DecisionSoftRetry
}

func antigravityShouldBypassShortCooldown(ctx context.Context, cfg *config.Config) bool {
	return cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(cfg)
}

func antigravitySoftRateLimitDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := time.Duration(attempt+1) * 500 * time.Millisecond
	if base > 3*time.Second {
		base = 3 * time.Second
	}
	return base
}

func antigravityShortCooldownKey(auth *cliproxyauth.Auth, modelName string) string {
	if auth == nil {
		return ""
	}
	authID := strings.TrimSpace(auth.ID)
	modelName = strings.TrimSpace(modelName)
	if authID == "" || modelName == "" {
		return ""
	}
	return authID + "|" + modelName + "|sc"
}

// antigravityCoolingDisabled reports whether quota cooling is disabled for the
// auth, honoring per-credential/provider overrides and the global disable-cooling flag.
func antigravityCoolingDisabled(auth *cliproxyauth.Auth, cfg *config.Config) bool {
	return cliproxyauth.QuotaCooldownDisabledForAuthWithConfig(auth, cfg)
}

func antigravityIsInShortCooldown(auth *cliproxyauth.Auth, modelName string, now time.Time) (bool, time.Duration) {
	if antigravityCoolingDisabled(auth, nil) {
		return false, 0
	}
	key := antigravityShortCooldownKey(auth, modelName)
	if key == "" {
		return false, 0
	}
	value, ok := antigravityShortCooldownByAuth.Load(key)
	if !ok {
		return false, 0
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		antigravityShortCooldownByAuth.Delete(key)
		return false, 0
	}
	remaining := until.Sub(now)
	if remaining <= 0 {
		antigravityShortCooldownByAuth.Delete(key)
		return false, 0
	}
	return true, remaining
}

func markAntigravityShortCooldown(auth *cliproxyauth.Auth, modelName string, now time.Time, duration time.Duration) {
	if antigravityCoolingDisabled(auth, nil) {
		return
	}
	key := antigravityShortCooldownKey(auth, modelName)
	if key == "" {
		return
	}
	antigravityShortCooldownByAuth.Store(key, now.Add(duration))
}

func antigravityNoCapacityRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Duration(attempt+1) * 250 * time.Millisecond
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	return delay
}

func antigravityTransient429RetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Duration(attempt+1) * 100 * time.Millisecond
	if delay > 500*time.Millisecond {
		delay = 500 * time.Millisecond
	}
	return delay
}

func antigravityInstantRetryDelay(wait time.Duration) time.Duration {
	if wait <= 0 {
		return 0
	}
	return wait + 800*time.Millisecond
}

func antigravityWait(ctx context.Context, wait time.Duration) error {
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

var antigravityBaseURLFallbackOrder = func(auth *cliproxyauth.Auth) []string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return []string{base}
	}
	return []string{
		antigravityBaseURLDaily,
		antigravityBaseURLProd,
		// antigravitySandboxBaseURLDaily,
	}
}

func resolveCustomAntigravityBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["base_url"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
	}
	return ""
}

func geminiToAntigravity(modelName string, payload []byte, projectID string, derivedSessionIDs ...string) []byte {
	template := payload
	template = helps.SetStringIfDifferent(template, "model", modelName)
	template = helps.SetStringIfDifferent(template, "userAgent", "antigravity")

	isImageModel := strings.Contains(modelName, "image")

	// Preserve a requestType already present on the translated payload (e.g.
	// "web_search" envelopes built by the Responses translator); only derive a
	// default when none was set. Ported from upstream CLIProxyAPI v7.3.6.
	reqType := strings.TrimSpace(gjson.GetBytes(template, "requestType").String())
	if reqType == "" {
		if isImageModel {
			reqType = "image_gen"
		} else {
			reqType = "agent"
		}
		template, _ = sjson.SetBytes(template, "requestType", reqType)
	}

	if projectID != "" {
		template = helps.SetStringIfDifferent(template, "project", projectID)
	} else {
		template, _ = sjson.DeleteBytes(template, "project")
	}

	if isImageModel {
		template, _ = sjson.SetBytes(template, "requestId", generateImageGenRequestID())
	} else if reqType != "web_search" {
		template, _ = sjson.SetBytes(template, "requestId", generateRequestID())
		sessionID := strings.TrimSpace(gjson.GetBytes(template, "request.sessionId").String())
		if sessionID == "" && len(derivedSessionIDs) > 0 {
			sessionID = strings.TrimSpace(derivedSessionIDs[0])
		}
		if sessionID == "" {
			sessionID = generateStableSessionID(payload)
		}
		template, _ = sjson.SetBytes(template, "request.sessionId", sessionID)
	}

	template, _ = sjson.DeleteBytes(template, "request.safetySettings")
	if toolConfig := gjson.GetBytes(template, "toolConfig"); toolConfig.Exists() && !gjson.GetBytes(template, "request.toolConfig").Exists() {
		template, _ = sjson.SetRawBytes(template, "request.toolConfig", []byte(toolConfig.Raw))
		template, _ = sjson.DeleteBytes(template, "toolConfig")
	}
	return template
}

func generateRequestID() string {
	return "agent-" + uuid.NewString()
}

func generateImageGenRequestID() string {
	return fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
}

func generateSessionID() string {
	randSourceMutex.Lock()
	n := randSource.Int63n(9_000_000_000_000_000_000)
	randSourceMutex.Unlock()
	return "-" + strconv.FormatInt(n, 10)
}

func generateStableSessionID(payload []byte) string {
	contents := gjson.GetBytes(payload, "request.contents")
	if contents.IsArray() {
		for _, content := range contents.Array() {
			if content.Get("role").String() == "user" {
				text := content.Get("parts.0.text").String()
				if text != "" {
					h := sha256.Sum256([]byte(text))
					n := int64(binary.BigEndian.Uint64(h[:8])) & 0x7FFFFFFFFFFFFFFF
					return "-" + strconv.FormatInt(n, 10)
				}
			}
		}
	}
	return generateSessionID()
}
