package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	metaauth "github.com/therealtinhtute/llmhub/internal/auth/meta"
	"github.com/therealtinhtute/llmhub/internal/config"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// metaOAuthService abstracts the Meta auth service so tests can stub the
// network-dependent device-flow start, authorization poll, and storage
// construction steps (local testability shim mirroring devinOAuthService;
// upstream constructs *metaauth.MetaAuth directly).
// Ported from upstream CLIProxyAPI
// internal/api/handlers/management/auth_files_provider_oauth.go RequestMetaToken
// (23c16e2985bb, end-state at v7.3.4 checkpoint 8335eac73194).
type metaOAuthService interface {
	StartDeviceFlow(ctx context.Context) (*metaauth.DeviceCodeResponse, error)
	WaitForAuthorization(ctx context.Context, dcr *metaauth.DeviceCodeResponse) (*metaauth.MetaAuthBundle, error)
	CreateTokenStorage(bundle *metaauth.MetaAuthBundle) *metaauth.MetaTokenStorage
}

var newMetaOAuthService = func(cfg *config.Config) metaOAuthService {
	return metaauth.NewMetaAuth(cfg)
}

// RequestMetaToken starts the Meta (Muse Code) RFC 8628 device authorization
// flow: it requests a device/user code pair from auth.meta.com, registers the
// oauth session, and returns the verification URL plus user_code so the WebUI
// and TUI can render the device-code prompt. Completion happens in
// completeMetaOAuth, which polls the token endpoint until the user approves.
// Ported from upstream CLIProxyAPI
// internal/api/handlers/management/auth_files_provider_oauth.go (23c16e2985bb).
func (h *Handler) RequestMetaToken(c *gin.Context) {
	// Do not inherit the HTTP request cancellation: the device poll continues
	// after the start response is returned (same convention as RequestDevinToken).
	ctx := PopulateAuthContext(context.Background(), c)

	fmt.Println("Initializing Meta authentication...")

	state := fmt.Sprintf("meta-%d", time.Now().UnixNano())
	authSvc := newMetaOAuthService(h.configSnapshot())

	deviceFlow, errStartDeviceFlow := authSvc.StartDeviceFlow(ctx)
	if errStartDeviceFlow != nil {
		log.Errorf("Failed to start Meta device flow: %v", errStartDeviceFlow)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start device authorization flow"})
		return
	}
	authURL := strings.TrimSpace(deviceFlow.VerificationURIComplete)
	if authURL == "" {
		authURL = strings.TrimSpace(deviceFlow.VerificationURI)
	}

	RegisterOAuthSession(state, "meta")

	go h.completeMetaOAuth(ctx, state, deviceFlow, authSvc)

	response := gin.H{"status": "ok", "url": authURL, "state": state, "flow": "device"}
	if userCode := strings.TrimSpace(deviceFlow.UserCode); userCode != "" {
		response["user_code"] = userCode
	}
	if deviceFlow.ExpiresIn > 0 {
		response["expires_in"] = deviceFlow.ExpiresIn
	} else {
		response["expires_in"] = int(metaauth.MaxPollDuration / time.Second)
	}
	c.JSON(http.StatusOK, response)
}

// completeMetaOAuth polls Meta's token endpoint until the user approves the
// device code, then builds the token storage record and persists it. The
// watcher cancels pollCtx once the session stops being pending (cancelled via
// DELETE /oauth-session, completed, or errored), so the device poll aborts
// instead of persisting a cancelled flow — the same wiring completeDevinOAuth
// and RequestKimiToken use (upstream 6e819ab62257).
func (h *Handler) completeMetaOAuth(ctx context.Context, state string, deviceFlow *metaauth.DeviceCodeResponse, authSvc metaOAuthService) {
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	go watchOAuthSessionCancel(pollCtx, cancelPoll, state, "meta")

	fmt.Println("Waiting for Meta authentication...")
	bundle, errWaitForAuthorization := authSvc.WaitForAuthorization(pollCtx, deviceFlow)
	if errWaitForAuthorization != nil {
		if !IsOAuthSessionPending(state, "meta") {
			return
		}
		log.Errorf("Meta authentication failed: %v", errWaitForAuthorization)
		SetOAuthSessionError(state, oauthSessionErrorWithCause("Authentication failed", errWaitForAuthorization))
		return
	}
	if !IsOAuthSessionPending(state, "meta") {
		return
	}

	tokenStorage := authSvc.CreateTokenStorage(bundle)
	if tokenStorage == nil || strings.TrimSpace(tokenStorage.AccessToken) == "" {
		log.Error("Meta token exchange returned empty access token")
		SetOAuthSessionError(state, "Failed to exchange token")
		return
	}

	record := buildMetaAuthRecord(bundle, tokenStorage)
	if errGuard := guardOAuthSessionPendingForSave(state, "meta"); errGuard != nil {
		return
	}
	savedPath, errSave := h.saveTokenRecord(ctx, record)
	if errSave != nil {
		log.Errorf("Failed to save Meta token to file: %v", errSave)
		SetOAuthSessionError(state, "Failed to save token to file")
		return
	}

	CompleteOAuthSession(state)
	fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
	fmt.Println("You can now use Meta services through this CLI")
}

func buildMetaAuthRecord(bundle *metaauth.MetaAuthBundle, tokenStorage *metaauth.MetaTokenStorage) *coreauth.Auth {
	fileName := metaauth.CredentialFileName(tokenStorage.Email, tokenStorage.DCAToken)
	label := strings.TrimSpace(tokenStorage.Email)
	if label == "" {
		label = "Meta"
	}

	metadata := map[string]any{
		"type":         "meta",
		"access_token": tokenStorage.AccessToken,
		"token_type":   tokenStorage.TokenType,
		"expires_in":   tokenStorage.ExpiresIn,
		"expired":      tokenStorage.Expired,
		"last_refresh": tokenStorage.LastRefresh,
		"base_url":     tokenStorage.BaseURL,
		"auth_kind":    "oauth",
	}
	if tokenStorage.DCAExpired != "" {
		metadata["dca_expired"] = tokenStorage.DCAExpired
	}
	if tokenStorage.DCAExpiresAt > 0 {
		metadata["dca_expires_at"] = tokenStorage.DCAExpiresAt
	}
	if tokenStorage.APIKey != "" {
		metadata["api_key"] = tokenStorage.APIKey
	}
	if tokenStorage.DCAToken != "" {
		metadata["dca_token"] = tokenStorage.DCAToken
	}
	if tokenStorage.Email != "" {
		metadata["email"] = tokenStorage.Email
	}
	if tokenStorage.Name != "" {
		metadata["name"] = tokenStorage.Name
	}
	if bundle != nil && bundle.MintedKey != nil {
		metadata["subs_tier_name"] = bundle.MintedKey.SubsTierName
		metadata["subs_tier_id"] = bundle.MintedKey.SubsTierID
		metadata["is_subs_active"] = bundle.MintedKey.IsSubsActive
		metadata["has_payment_method"] = bundle.MintedKey.HasPaymentMethod
	}

	attrs := map[string]string{
		"auth_kind": "oauth",
		"base_url":  tokenStorage.BaseURL,
	}
	if tokenStorage.APIKey != "" {
		attrs["api_key"] = tokenStorage.APIKey
	}
	if tokenStorage.DCAToken != "" {
		attrs["dca_token"] = tokenStorage.DCAToken
	}
	if tokenStorage.Email != "" {
		attrs["email"] = tokenStorage.Email
	}

	return &coreauth.Auth{
		ID:         fileName,
		Provider:   "meta",
		FileName:   fileName,
		Label:      label,
		Storage:    tokenStorage,
		Metadata:   metadata,
		Attributes: attrs,
	}
}
