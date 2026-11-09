package executor

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	metaauth "github.com/therealtinhtute/llmhub/internal/auth/meta"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	"github.com/therealtinhtute/llmhub/internal/util"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

var (
	_ cliproxyauth.ProviderExecutor    = (*MetaExecutor)(nil)
	_ cliproxyauth.RequestAuthPreparer = (*MetaExecutor)(nil)
)

var metaRefreshGroup singleflight.Group

// Captured from Muse Code 1.4.14-R7309.1 (build dec7d7497fecdad135ec5835a8161dbc92e55b58)
// on linux-x86_64. Muse Code subscription keys return HTTP 403
// client_attestation_denied when the request still looks like muse-build/1.3.0.
const (
	metaClientVersion = "1.4.14"
	metaClientBuild   = "dec7d7497fecdad135ec5835a8161dbc92e55b58"
	metaClientSurface = "tbh:exec"
)

func metaUserAgent() string {
	goos := runtime.GOOS
	if goos == "darwin" {
		goos = "macos"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return fmt.Sprintf("muse-build/%s (non-interactive; %s-%s; build %s)", metaClientVersion, goos, arch, metaClientBuild)
}

// MetaExecutor implements the cliproxyauth.ProviderExecutor for Meta Muse models (api.meta.ai).
type MetaExecutor struct {
	cfg *config.Config
}

// NewMetaExecutor constructs a new Meta executor.
func NewMetaExecutor(cfg *config.Config) *MetaExecutor {
	return &MetaExecutor{cfg: cfg}
}

// Identifier returns the provider identifier "meta".
func (e *MetaExecutor) Identifier() string {
	return "meta"
}

// PrepareRequest injects Meta credentials into the outgoing HTTP request.
func (e *MetaExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, token := metaCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Del("Authorization")
	}
	applyMetaClientIdentity(req)

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Meta credentials into the request and executes it.
func (e *MetaExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("meta executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	enriched, err := e.ensureAuth(ctx, auth)
	if err != nil {
		return nil, err
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, enriched); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, enriched, 0)
	return httpClient.Do(httpReq)
}

// Refresh mints an API key from a DCA token if needed.
// Ported from upstream CLIProxyAPI commits 54d4f4c0193c, be7323f3bf66 and
// 4a0131c062cd: the executor produces a candidate only; the manager accepts and
// persists it (persistMetaMint inside updateInternal).
func (e *MetaExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("meta executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "meta executor: auth is nil"}
	}

	dcaToken := extractDCAToken(auth)
	if dcaToken == "" {
		if _, token := metaCreds(auth); token != "" {
			return auth, nil
		}
		return nil, statusErr{
			code: http.StatusUnauthorized,
			msg:  "meta executor: missing API key or DCA token",
		}
	}

	mintRes, err, _ := metaRefreshGroup.Do(dcaToken, func() (any, error) {
		authSvc := metaauth.NewMetaAuthWithProxyURL(e.cfg, auth.ProxyURL)
		return authSvc.MintAPIKey(ctx, dcaToken)
	})
	if err != nil {
		return nil, fmt.Errorf("meta executor: mint API key failed: %w", err)
	}

	minted, ok := mintRes.(*metaauth.MintedKeyResponse)
	if !ok || minted == nil || minted.APIKey == "" {
		return nil, fmt.Errorf("meta executor: mint API key returned empty key")
	}

	baseURL, _ := metaCreds(auth)
	if mintedURL := strings.TrimSpace(minted.BaseURL); mintedURL != "" {
		baseURL = mintedURL
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["base_url"] = baseURL
	auth.Metadata["api_key"] = minted.APIKey
	auth.Metadata["access_token"] = minted.APIKey
	auth.Metadata["dca_token"] = dcaToken
	delete(auth.Metadata, "expired")
	if minted.UserEmail != "" {
		auth.Metadata["email"] = minted.UserEmail
	}
	if minted.UserFullName != "" {
		auth.Metadata["name"] = minted.UserFullName
	}
	if minted.SubsTierName != "" {
		auth.Metadata["subs_tier_name"] = minted.SubsTierName
	} else {
		delete(auth.Metadata, "subs_tier_name")
	}
	if minted.SubsTierID != "" {
		auth.Metadata["subs_tier_id"] = minted.SubsTierID
	} else {
		delete(auth.Metadata, "subs_tier_id")
	}
	auth.Metadata["is_subs_active"] = minted.IsSubsActive
	auth.Metadata["has_payment_method"] = minted.HasPaymentMethod
	auth.Metadata["type"] = "meta"
	nowStr := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = nowStr

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["base_url"] = baseURL
	auth.Attributes["api_key"] = minted.APIKey
	auth.Attributes["access_token"] = minted.APIKey

	// Auth.Clone does not clone Storage. Keep the candidate isolated from live
	// login storage; file-loaded records already persist through Metadata.
	if existing, ok := auth.Storage.(*metaauth.MetaTokenStorage); ok && existing != nil {
		storage := *existing
		storage.APIKey = minted.APIKey
		storage.AccessToken = minted.APIKey
		storage.DCAToken = dcaToken
		storage.Expired = ""
		storage.BaseURL = baseURL
		storage.LastRefresh = nowStr
		storage.Metadata = auth.Metadata
		if minted.UserEmail != "" {
			storage.Email = minted.UserEmail
		}
		if minted.UserFullName != "" {
			storage.Name = minted.UserFullName
		}
		auth.Storage = &storage
	}
	// Only the manager may accept and persist the candidate.
	auth.LastRefreshedAt = time.Now()

	return auth, nil
}

// ShouldPrepareRequestAuth reports true when a Meta auth has a DCA token but no usable API key.
// Ported from upstream CLIProxyAPI commit d09042a54810.
func (e *MetaExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || metaIsConfigAPIKeyAuth(auth) {
		return false
	}
	_, token := metaCreds(auth)
	return token == "" && extractDCAToken(auth) != ""
}

// PrepareRequestAuth exchanges the DCA token for an API key before request execution,
// allowing the conductor to atomically install and persist the credential in the manager.
// Ported from upstream CLIProxyAPI commit d09042a54810.
func (e *MetaExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil || !e.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}
	return e.Refresh(ctx, auth)
}

func (e *MetaExecutor) ensureAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{
			code: http.StatusUnauthorized,
			msg:  "meta executor: missing auth",
		}
	}

	_, token := metaCreds(auth)
	if token == "" {
		if dcaToken := extractDCAToken(auth); dcaToken != "" {
			refreshed, err := e.Refresh(ctx, auth)
			if err != nil {
				return nil, err
			}
			auth = refreshed
			_, token = metaCreds(auth)
		}
	}

	if token == "" {
		if metaIsConfigAPIKeyAuth(auth) {
			return nil, statusErr{
				code: http.StatusUnauthorized,
				msg:  "meta executor: meta-api-key requires a valid API key (DCA tokens require OAuth storage)",
			}
		}
		return nil, statusErr{
			code: http.StatusUnauthorized,
			msg:  "meta executor: missing API key or access token",
		}
	}

	return e.enrichAuth(auth), nil
}

func (e *MetaExecutor) enrichAuth(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	baseURL, token := metaCreds(auth)
	var cloned *cliproxyauth.Auth
	if auth != nil {
		cloned = auth.Clone()
	} else {
		cloned = &cliproxyauth.Auth{
			Provider: "meta",
		}
	}
	if cloned.Attributes == nil {
		cloned.Attributes = make(map[string]string)
	}
	if strings.TrimSpace(cloned.Attributes["base_url"]) == "" {
		cloned.Attributes["base_url"] = baseURL
	}
	if strings.TrimSpace(cloned.Attributes["api_key"]) == "" {
		cloned.Attributes["api_key"] = token
	}
	if _, hasHeader := cloned.Attributes["header:User-Agent"]; !hasHeader {
		cloned.Attributes["header:User-Agent"] = metaUserAgent()
	}
	return cloned
}

// metaIsConfigAPIKeyAuth reports whether the auth entry was synthesized from a
// config meta-api-key list. Shim for upstream cliproxyauth.IsConfigAPIKeyAuth
// (sdk/cliproxy/auth/config_apikey.go): the fork encodes the same signal through
// the synthesizer-set auth_kind/api_key attributes and the source=config:*
// attribute, mirroring upstream AuthKind() precedence (explicit kind first,
// then the api_key attribute fallback).
func metaIsConfigAPIKeyAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	kind := metaNormalizeAuthKind(auth.Attributes["auth_kind"])
	if kind == "" && auth.Metadata != nil {
		if v, ok := auth.Metadata["auth_kind"].(string); ok {
			kind = metaNormalizeAuthKind(v)
		}
	}
	if kind == "" && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		kind = "apikey"
	}
	if kind != "apikey" {
		return false
	}
	// Mirrors upstream AuthSourceKind()==AuthSourceConfig: an explicit
	// source_backend=config marker, or the synthesizer's config:* source prefix.
	if strings.EqualFold(strings.TrimSpace(auth.Attributes["source_backend"]), "config") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth.Attributes["source"])), "config:")
}

func metaNormalizeAuthKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "apikey", "api_key", "api-key":
		return "apikey"
	case "oauth", "oauth2":
		return "oauth"
	default:
		return ""
	}
}

func extractDCAToken(a *cliproxyauth.Auth) string {
	if a == nil || metaIsConfigAPIKeyAuth(a) {
		return ""
	}
	if a.Attributes != nil {
		if d := strings.TrimSpace(a.Attributes["dca_token"]); d != "" {
			return d
		}
		if t := strings.TrimSpace(a.Attributes["access_token"]); strings.HasPrefix(t, "dca:") {
			return t
		}
	}
	if a.Metadata != nil {
		if d, ok := a.Metadata["dca_token"].(string); ok && strings.TrimSpace(d) != "" {
			return strings.TrimSpace(d)
		}
		if t, ok := a.Metadata["access_token"].(string); ok && strings.HasPrefix(strings.TrimSpace(t), "dca:") {
			return strings.TrimSpace(t)
		}
	}
	if a.Storage != nil {
		if ms, ok := a.Storage.(*metaauth.MetaTokenStorage); ok && ms != nil {
			if ms.DCAToken != "" {
				return ms.DCAToken
			}
			if strings.HasPrefix(ms.AccessToken, "dca:") {
				return ms.AccessToken
			}
		}
	}
	return ""
}

func metaCreds(a *cliproxyauth.Auth) (baseURL, token string) {
	baseURL = metaauth.DefaultAPIBaseURL
	if a == nil {
		return baseURL, ""
	}

	if a.Attributes != nil {
		if b := strings.TrimSpace(a.Attributes["base_url"]); b != "" {
			baseURL = b
		}
		if k := strings.TrimSpace(a.Attributes["api_key"]); k != "" && !strings.HasPrefix(k, "dca:") {
			token = k
		} else if t := strings.TrimSpace(a.Attributes["access_token"]); t != "" && !strings.HasPrefix(t, "dca:") {
			token = t
		}
	}
	if a.Metadata != nil {
		if baseURL == metaauth.DefaultAPIBaseURL {
			if b, ok := a.Metadata["base_url"].(string); ok && strings.TrimSpace(b) != "" {
				baseURL = strings.TrimSpace(b)
			} else if b, ok := a.Metadata["api_base_url"].(string); ok && strings.TrimSpace(b) != "" {
				baseURL = strings.TrimSpace(b)
			}
		}
		if token == "" {
			if k, ok := a.Metadata["api_key"].(string); ok && strings.TrimSpace(k) != "" && !strings.HasPrefix(strings.TrimSpace(k), "dca:") {
				token = strings.TrimSpace(k)
			} else if t, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(t) != "" && !strings.HasPrefix(strings.TrimSpace(t), "dca:") {
				token = strings.TrimSpace(t)
			}
		}
	}
	if token == "" && a.Storage != nil {
		if ms, ok := a.Storage.(*metaauth.MetaTokenStorage); ok && ms != nil {
			if ms.APIKey != "" {
				token = ms.APIKey
			} else if ms.AccessToken != "" && !strings.HasPrefix(ms.AccessToken, "dca:") {
				token = ms.AccessToken
			}
			if ms.BaseURL != "" && baseURL == metaauth.DefaultAPIBaseURL {
				baseURL = ms.BaseURL
			}
		}
	}
	return baseURL, token
}

// parseMetaRetryAfter recovers the subscription-quota reset timestamp Meta
// returns on 429 responses (upstream d09042a54810).
func parseMetaRetryAfter(statusCode int, errorBody []byte, now time.Time) *time.Duration {
	if (statusCode != http.StatusTooManyRequests && statusCode != http.StatusNotFound) || len(errorBody) == 0 {
		return nil
	}
	if resetsAt := gjson.GetBytes(errorBody, "error.resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			retryAfter := resetAtTime.Sub(now)
			return &retryAfter
		}
	}
	return nil
}

// isMetaSubscriptionQuota reports whether a 429 body is a credential-scoped
// subscription quota exhaustion rather than a transient rate limit
// (upstream d09042a54810). The conductor treats credential-scoped failures as
// applying to the whole credential across models (isCredentialScopedError).
func isMetaSubscriptionQuota(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests || len(body) == 0 {
		return false
	}
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
	if strings.Contains(msg, "subscription quota") || strings.Contains(msg, "quota exhausted") {
		return true
	}
	if (code == "rate_limit_exceeded" || strings.Contains(code, "quota")) && gjson.GetBytes(body, "error.resets_at").Exists() {
		return true
	}
	return false
}
