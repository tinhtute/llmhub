package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/auth/devin"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/misc"
	"github.com/therealtinhtute/llmhub/internal/util"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// devinOAuthService abstracts the Devin auth service so tests can stub the
// network-dependent exchange and record creation steps.
// Ported from upstream CLIProxyAPI
// internal/api/handlers/management/auth_files_devin_oauth.go (44e62bc8acc2);
// the callback wait uses the local in-memory oauth session store
// (WaitOAuthCallbackForPendingSession) instead of upstream's file polling.
type devinOAuthService interface {
	BuildAuthorizationURL(redirectURI, codeChallenge, state string) string
	ExchangeCodeForToken(ctx context.Context, code, codeVerifier string) (string, error)
	CreateAuthRecord(ctx context.Context, token string) (*coreauth.Auth, error)
}

var newDevinOAuthService = func(cfg *config.Config) devinOAuthService {
	client := util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second})
	return devin.NewDevinAuthService(client)
}

// RequestDevinToken starts the same callback/status flow used by the other WebUI providers.
// The Devin authorization server redirects straight back to this server, so the
// redirect URI always points at the management callback endpoint.
func (h *Handler) RequestDevinToken(c *gin.Context) {
	redirectURI, errRedirect := h.managementCallbackURL("/devin/callback")
	if errRedirect != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
		return
	}
	pkceCodes, errPKCE := devin.GeneratePKCECodes()
	if errPKCE != nil {
		log.Errorf("Failed to generate Devin PKCE codes: %v", errPKCE)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}
	state, errState := misc.GenerateRandomState()
	if errState != nil {
		log.Errorf("Failed to generate state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	authSvc := newDevinOAuthService(h.configSnapshot())
	authURL := authSvc.BuildAuthorizationURL(redirectURI, pkceCodes.CodeChallenge, state)
	RegisterOAuthSession(state, "devin")
	// Do not inherit the HTTP request cancellation: login continues after returning the URL.
	ctx := PopulateAuthContext(context.Background(), c)
	go h.completeDevinOAuth(ctx, state, pkceCodes.CodeVerifier, authSvc)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) completeDevinOAuth(ctx context.Context, state, codeVerifier string, authSvc devinOAuthService) {
	// The watcher cancels ctx once the session stops being pending (cancelled
	// via DELETE /oauth-session, completed, or errored), so an in-flight token
	// exchange or record fetch aborts instead of persisting a cancelled flow.
	// Ported from upstream CLIProxyAPI auth_files_devin_oauth.go (44e62bc8acc2,
	// cancel watch introduced by 6e819ab62257).
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	go watchOAuthSessionCancel(ctx, cancel, state, "devin")

	payload, errWait := WaitOAuthCallbackForPendingSession("devin", state, 5*time.Minute)
	if errWait != nil {
		if errors.Is(errWait, errOAuthSessionNotPending) {
			return
		}
		if IsOAuthSessionPending(state, "devin") {
			SetOAuthSessionError(state, errWait.Error())
		}
		return
	}
	if !IsOAuthSessionPending(state, "devin") {
		return
	}
	if payloadState := strings.TrimSpace(payload["state"]); payloadState != "" && payloadState != state {
		SetOAuthSessionError(state, "State code error")
		return
	}
	if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
		SetOAuthSessionError(state, "Devin authorization denied")
		return
	}
	authCode := strings.TrimSpace(payload["code"])
	if authCode == "" {
		SetOAuthSessionError(state, "Missing authorization code")
		return
	}

	token, errExchange := authSvc.ExchangeCodeForToken(ctx, authCode, codeVerifier)
	if errExchange != nil || strings.TrimSpace(token) == "" {
		// Upstream errors can contain tokens or authorization codes; never expose them.
		if IsOAuthSessionPending(state, "devin") {
			SetOAuthSessionError(state, "Failed to exchange authorization code for tokens")
		}
		return
	}
	if !IsOAuthSessionPending(state, "devin") {
		return
	}
	record, errRecord := authSvc.CreateAuthRecord(ctx, token)
	if errRecord != nil {
		if IsOAuthSessionPending(state, "devin") {
			SetOAuthSessionError(state, "Failed to create Devin authentication record")
		}
		return
	}
	if errGuard := guardOAuthSessionPendingForSave(state, "devin"); errGuard != nil {
		return
	}
	savedPath, errSave := h.saveTokenRecord(ctx, record)
	if errSave != nil {
		SetOAuthSessionError(state, "Failed to save authentication tokens")
		return
	}
	// Unlike sibling providers, Devin deliberately does not complete other
	// sessions for the provider: concurrent Devin logins may coexist
	// (upstream 44e62bc8acc2 test TestDevinRemoteOAuthFlow).
	CompleteOAuthSession(state)
	log.Info("Devin authentication successful")
	fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
	fmt.Println("You can now use Devin services through this CLI")
}
