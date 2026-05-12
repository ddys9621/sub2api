package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

// kiroRefreshWindow is how long before upstream expiry we proactively refresh.
// Kiro tokens are issued with expiresIn = 3600s (1 hour) and we refresh with a
// 15-minute safety margin so that scheduled requests never see an expired
// credential.
const kiroRefreshWindow = 15 * time.Minute

// KiroTokenRefresher plugs KiroOAuthService into the shared background refresh
// pipeline (token_refresh_service.go).
type KiroTokenRefresher struct {
	kiroOAuthService *KiroOAuthService
}

// NewKiroTokenRefresher wires the refresher with its OAuth service.
func NewKiroTokenRefresher(kiroOAuthService *KiroOAuthService) *KiroTokenRefresher {
	return &KiroTokenRefresher{kiroOAuthService: kiroOAuthService}
}

// CacheKey returns the distributed lock key used by OAuthRefreshAPI.
func (r *KiroTokenRefresher) CacheKey(account *Account) string {
	return KiroTokenCacheKey(account)
}

// CanRefresh gates which accounts this refresher serves.
func (r *KiroTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.Platform == PlatformKiro && account.Type == AccountTypeOAuth
}

// NeedsRefresh uses a fixed 15-minute window, ignoring the global default so
// Kiro's short-lived tokens get renewed consistently even when the operator
// tweaks the generic scheduler.
func (r *KiroTokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return false
	}
	return time.Until(*expiresAt) < kiroRefreshWindow
}

// Refresh performs the token refresh and returns the new credentials map to be
// persisted on the Account row. It auto-selects the refresh path:
//
//  1. OIDC (Builder ID / Enterprise SSO): credentials contain client_id + client_secret
//     → POST https://oidc.{region}.amazonaws.com/token (standard OIDC, no CSRF)
//  2. Desktop Auth (Social login — Google/GitHub): credentials contain auth_method=social
//     or lack client_id → POST https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken
//     (plain JSON, no CSRF)
//  3. Legacy CBOR (fallback): uses app.kiro.dev CBOR RPC (requires CSRF token)
func (r *KiroTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	prevRefresh := account.GetCredential("refresh_token")
	clientID := account.GetCredential("client_id")
	clientSecret := account.GetCredential("client_secret")
	region := account.GetCredential("region")
	if region == "" {
		region = "us-east-1"
	}

	var tokenInfo *kiro.TokenInfo
	var err error

	switch {
	case clientID != "" && clientSecret != "":
		// Path 1: OIDC refresh (Builder ID / Enterprise SSO)
		reg := &kiro.OIDCClientRegistration{ClientID: clientID, ClientSecret: clientSecret}
		oidcResp, oidcErr := kiro.RefreshOIDCToken(ctx, nil, region, reg, prevRefresh)
		if oidcErr != nil {
			return nil, oidcErr
		}
		tokenInfo = &kiro.TokenInfo{
			AccessToken:  oidcResp.AccessToken,
			RefreshToken: oidcResp.RefreshToken,
			ExpiresIn:    oidcResp.ExpiresIn,
			ExpiresAt:    time.Now().Unix() + oidcResp.ExpiresIn,
			TokenType:    oidcResp.TokenType,
			ProfileArn:   oidcResp.ProfileArn,
		}

	default:
		// Path 2: Desktop Auth refresh (Social login — no CSRF needed)
		client, clientErr := kiro.NewClient(kiro.WithProxyURL(proxyURLForAccount(account)))
		if clientErr != nil {
			return nil, clientErr
		}
		defer client.Close()
		tokenInfo, err = client.RefreshDesktop(ctx, prevRefresh)
		if err != nil {
			// Path 3 fallback: Legacy CBOR (for very old accounts that somehow
			// only work with the portal RPC). This path requires CSRF.
			prevAccess := account.GetCredential("access_token")
			prevCSRF := account.GetCredential("csrf_token")
			tokenInfo, err = r.kiroOAuthService.RefreshAccountToken(ctx, prevAccess, prevRefresh, prevCSRF, proxyURLForAccount(account))
			if err != nil {
				return nil, err
			}
		}
	}

	next := r.kiroOAuthService.BuildAccountCredentials(tokenInfo)
	next = MergeCredentials(account.Credentials, next)

	// Preserve fields that occasionally come back empty (e.g. profile_arn is
	// only guaranteed to be present in the initial ExchangeToken response).
	preserveIfEmpty(next, "profile_arn", account.GetCredential("profile_arn"))
	preserveIfEmpty(next, "email", account.GetCredential("email"))
	preserveIfEmpty(next, "user_id", account.GetCredential("user_id"))
	// Preserve OIDC client credentials for future refreshes
	preserveIfEmpty(next, "client_id", clientID)
	preserveIfEmpty(next, "client_secret", clientSecret)
	preserveIfEmpty(next, "region", region)

	if tokenInfo.CSRFToken == "" {
		preserveIfEmpty(next, "csrf_token", account.GetCredential("csrf_token"))
	}

	return next, nil
}

func preserveIfEmpty(creds map[string]any, key, fallback string) {
	if fallback == "" {
		return
	}
	if v, ok := creds[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return
		}
	}
	creds[key] = fallback
}

// proxyURLForAccount returns the configured outbound proxy URL if any. Kept as
// its own helper so it can be extended when we introduce per-account proxies
// with richer resolution logic.
func proxyURLForAccount(account *Account) string {
	if account == nil || account.Proxy == nil {
		return ""
	}
	return account.Proxy.URL()
}
