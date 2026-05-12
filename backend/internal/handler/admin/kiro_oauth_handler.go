package admin

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// KiroOAuthHandler exposes the Cognito + Google IdP login flow for Kiro
// accounts. The browser completes OAuth against app.kiro.dev and Cognito,
// then the admin pastes the resulting URL back here so the server can redeem
// the code with its server-side PKCE verifier.
type KiroOAuthHandler struct {
	kiroOAuthService *service.KiroOAuthService
	adminService     service.AdminService
	quotaFetcher     *service.KiroQuotaFetcher
}

// NewKiroOAuthHandler wires the handler with its service.
func NewKiroOAuthHandler(kiroOAuthService *service.KiroOAuthService, adminService service.AdminService, quotaFetcher *service.KiroQuotaFetcher) *KiroOAuthHandler {
	return &KiroOAuthHandler{
		kiroOAuthService: kiroOAuthService,
		adminService:     adminService,
		quotaFetcher:     quotaFetcher,
	}
}

// KiroGenerateAuthURLRequest carries optional egress proxy hints.
type KiroGenerateAuthURLRequest struct {
	ProxyID *int64 `json:"proxy_id"`
}

// GenerateAuthURL returns the Cognito authorize URL + session identifier.
// POST /api/v1/admin/kiro/oauth/auth-url
func (h *KiroOAuthHandler) GenerateAuthURL(c *gin.Context) {
	var req KiroGenerateAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Body is optional; ignore bind failures.
		req = KiroGenerateAuthURLRequest{}
	}

	result, err := h.kiroOAuthService.GenerateAuthURL(c.Request.Context(), req.ProxyID)
	if err != nil {
		response.InternalError(c, "生成 Kiro 授权链接失败: "+err.Error())
		return
	}
	response.Success(c, result)
}

// KiroExchangeCodeRequest is the payload posted when the admin returns from
// the Cognito-hosted Google login. Either the full callback URL or a bare
// `code` value (+ optional `state`) is accepted.
type KiroExchangeCodeRequest struct {
	SessionID   string `json:"session_id" binding:"required"`
	CallbackURL string `json:"callback_url"`
	Code        string `json:"code"`
	State       string `json:"state"`
	ProxyID     *int64 `json:"proxy_id"`
}

// ExchangeCode redeems the authorization code server-side.
// POST /api/v1/admin/kiro/oauth/exchange-code
func (h *KiroOAuthHandler) ExchangeCode(c *gin.Context) {
	var req KiroExchangeCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	// Support two input styles:
	//   1) Paste the entire callback URL -> callback_url field
	//   2) Paste just the `code` parameter -> code (+state) fields
	callback := req.CallbackURL
	if callback == "" && req.Code != "" {
		callback = "?code=" + req.Code
		if req.State != "" {
			callback += "&state=" + req.State
		}
	}
	if callback == "" {
		response.BadRequest(c, "必须提供 callback_url 或 code 字段")
		return
	}

	result, err := h.kiroOAuthService.ExchangeCode(c.Request.Context(), &service.KiroExchangeCodeInput{
		SessionID:   req.SessionID,
		CallbackURL: callback,
		ProxyID:     req.ProxyID,
	})
	if err != nil {
		response.BadRequest(c, "Token 交换失败: "+err.Error())
		return
	}

	// Strip the raw CBOR map from the response to keep the payload compact.
	result.Tokens.Extra = nil
	response.Success(c, gin.H{
		"tokens":    result.Tokens,
		"user_info": result.UserInfo,
		"usage":     result.Usage,
		"proxy_url": result.ProxyURL,
	})
}

// KiroRefreshTokenRequest validates an existing refresh token.
//
// Kiro requires BOTH an access token (to fetch the CSRF meta tag from the
// portal HTML) AND a refresh token (the actual credential used by the
// RefreshToken RPC). If the admin only has a refresh token, they must first
// perform a fresh ExchangeCode to obtain a new access token.
type KiroRefreshTokenRequest struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token" binding:"required"`
	CSRFToken    string `json:"csrf_token"`
	ProxyID      *int64 `json:"proxy_id"`
}

// RefreshToken exchanges a refresh token for a fresh access token (admin-side
// "test" operation; token refresh in the hot path is handled by the scheduler).
// POST /api/v1/admin/kiro/oauth/refresh-token
func (h *KiroOAuthHandler) RefreshToken(c *gin.Context) {
	var req KiroRefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	var proxyURL string
	// In the admin flow the proxy id is resolved server-side via AccountHandler;
	// for this endpoint we stay minimal and forward with no proxy.
	tokens, err := h.kiroOAuthService.RefreshAccountToken(c.Request.Context(), req.AccessToken, req.RefreshToken, req.CSRFToken, proxyURL)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	tokens.Extra = nil
	response.Success(c, tokens)
}

// KiroCreateFromOAuthRequest is the payload for one-shot account creation
// from a completed OAuth callback.
type KiroCreateFromOAuthRequest struct {
	SessionID   string  `json:"session_id" binding:"required"`
	CallbackURL string  `json:"callback_url"`
	Code        string  `json:"code"`
	State       string  `json:"state"`
	ProxyID     *int64  `json:"proxy_id"`
	Name        string  `json:"name"`
	Notes       *string `json:"notes"`
	Concurrency int     `json:"concurrency"`
	Priority    int     `json:"priority"`
	GroupIDs    []int64 `json:"group_ids"`
}

// CreateAccountFromOAuth exchanges the Cognito callback for tokens and
// persists a brand-new Kiro account in a single call. This is the happy path
// triggered from the admin UI after the user completes Google login.
// POST /api/v1/admin/kiro/create-from-oauth
func (h *KiroOAuthHandler) CreateAccountFromOAuth(c *gin.Context) {
	var req KiroCreateFromOAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	// Accept either the full callback URL or a bare `code` (+ optional state).
	callback := req.CallbackURL
	if callback == "" && req.Code != "" {
		callback = "?code=" + req.Code
		if req.State != "" {
			callback += "&state=" + req.State
		}
	}
	if callback == "" {
		response.BadRequest(c, "必须提供 callback_url 或 code")
		return
	}

	result, err := h.kiroOAuthService.ExchangeCode(c.Request.Context(), &service.KiroExchangeCodeInput{
		SessionID:   req.SessionID,
		CallbackURL: callback,
		ProxyID:     req.ProxyID,
	})
	if err != nil {
		response.BadRequest(c, "Token 交换失败: "+err.Error())
		return
	}

	name := req.Name
	if name == "" {
		if result.Tokens != nil && result.Tokens.Email != "" {
			name = result.Tokens.Email
		} else {
			name = "Kiro OAuth Account"
		}
	}

	credentials := h.kiroOAuthService.BuildAccountCredentials(result.Tokens)

	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:        name,
		Notes:       req.Notes,
		Platform:    service.PlatformKiro,
		Type:        service.AccountTypeOAuth,
		Credentials: credentials,
		ProxyID:     req.ProxyID,
		Concurrency: req.Concurrency,
		Priority:    req.Priority,
		GroupIDs:    req.GroupIDs,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"account":   account,
		"user_info": result.UserInfo,
		"usage":     result.Usage,
	})
}

// KiroCreateFromTokensRequest lets an admin paste raw tokens (accessToken +
// refreshToken + profileArn) directly. Useful when the OAuth flow is driven
// externally (Kiro IDE / kiro-cli / manual cURL) and we just want to adopt
// the resulting session.
type KiroCreateFromTokensRequest struct {
	AccessToken       string          `json:"access_token"`
	AccessTokenCamel  string          `json:"accessToken"`
	RefreshToken      string          `json:"refresh_token"`
	RefreshTokenCamel string          `json:"refreshToken"`
	ProfileArn        string          `json:"profile_arn"`
	ProfileArnCamel   string          `json:"profileArn"`
	CSRFToken         string          `json:"csrf_token"`
	CSRFTokenCamel    string          `json:"csrfToken"`
	Email             string          `json:"email"`
	UserID            string          `json:"user_id"`
	UserIDCamel       string          `json:"userId"`
	ExpiresIn         int64           `json:"expires_in"`
	ExpiresInCamel    int64           `json:"expiresIn"`
	ExpiresAt         json.RawMessage `json:"expires_at"`
	ExpiresAtCamel    json.RawMessage `json:"expiresAt"`
	TokenType         string          `json:"token_type"`
	TokenTypeCamel    string          `json:"tokenType"`
	ClientID          string          `json:"client_id"`
	ClientIDCamel     string          `json:"clientId"`
	ClientSecret      string          `json:"client_secret"`
	ClientSecretCamel string          `json:"clientSecret"`
	Region            string          `json:"region"`
	Provider          string          `json:"provider"`
	AuthMethod        string          `json:"auth_method"`
	AuthMethodCamel   string          `json:"authMethod"`
	// MachineID is the per-device fingerprint embedded into Kiro IDE's
	// User-Agent (KiroIDE-<ver>-<machineId>). Pasting the original GUID
	// captured from Kiro-account-manager (64-char hex) is what makes
	// sub2api traffic indistinguishable from the real desktop client —
	// without it, AWS sees one shared device behind every account.
	MachineID        string  `json:"machine_id"`
	MachineIDCamel   string  `json:"machineId"`
	ProxyID          *int64  `json:"proxy_id"`
	Name             string  `json:"name"`
	Label            string  `json:"label"`
	Notes            *string `json:"notes"`
	Concurrency      int     `json:"concurrency"`
	Priority         int     `json:"priority"`
	GroupIDs         []int64 `json:"group_ids"`
}

// CreateAccountFromTokens persists a Kiro account from an externally obtained
// token pair. No CBOR RPC is performed; the caller vouches for the values.
// POST /api/v1/admin/kiro/create-from-tokens
func (h *KiroOAuthHandler) CreateAccountFromTokens(c *gin.Context) {
	var req KiroCreateFromTokensRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	accessToken := firstKiroTokenString(req.AccessToken, req.AccessTokenCamel)
	if accessToken == "" {
		response.BadRequest(c, "access_token is required")
		return
	}
	refreshToken := firstKiroTokenString(req.RefreshToken, req.RefreshTokenCamel)
	profileArn := firstKiroTokenString(req.ProfileArn, req.ProfileArnCamel)
	csrfToken := firstKiroTokenString(req.CSRFToken, req.CSRFTokenCamel)
	userID := firstKiroTokenString(req.UserID, req.UserIDCamel)
	tokenType := firstKiroTokenString(req.TokenType, req.TokenTypeCamel)
	clientID := firstKiroTokenString(req.ClientID, req.ClientIDCamel)
	clientSecret := firstKiroTokenString(req.ClientSecret, req.ClientSecretCamel)
	region := strings.TrimSpace(req.Region)
	if region == "" {
		region = "us-east-1"
	}
	provider := strings.TrimSpace(req.Provider)
	authMethod := firstKiroTokenString(req.AuthMethod, req.AuthMethodCamel)
	machineID := firstKiroTokenString(req.MachineID, req.MachineIDCamel)

	name := req.Name
	if name == "" {
		if req.Label != "" {
			name = req.Label
		} else if req.Email != "" {
			name = req.Email
		} else if clientID != "" && clientSecret != "" {
			name = "Kiro Builder ID Account"
		} else {
			name = "Kiro OAuth Account"
		}
	}

	if tokenType == "" {
		tokenType = "Bearer"
	}
	expiresAt := parseKiroTokenExpiresAt(req.ExpiresAt, req.ExpiresAtCamel)
	expiresIn := req.ExpiresIn
	if expiresIn == 0 {
		expiresIn = req.ExpiresInCamel
	}
	if expiresAt == 0 && expiresIn > 0 {
		// Safety skew mirrors buildTokenInfo: refresh 60s before upstream expiry.
		expiresAt = time.Now().Unix() + expiresIn - 60
	}
	credentials := map[string]any{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"profile_arn":   profileArn,
		"csrf_token":    csrfToken,
		"email":         req.Email,
		"user_id":       userID,
		"token_type":    tokenType,
		"expires_at":    expiresAt,
	}
	if clientID != "" {
		credentials["client_id"] = clientID
	}
	if clientSecret != "" {
		credentials["client_secret"] = clientSecret
	}
	if region != "" {
		credentials["region"] = region
	}
	if isKiroBuilderIDPayload(provider, authMethod, clientID, clientSecret) {
		credentials["auth_method"] = "builderid"
	} else if authMethod != "" {
		credentials["auth_method"] = strings.ToLower(authMethod)
	}
	if provider != "" {
		credentials["provider"] = provider
	}
	// Persist the operator-supplied device fingerprint so the streaming
	// gateway can emit it in every User-Agent. When absent, the gateway
	// falls back to a sha256-derived value — stable but distinguishable
	// from a real Kiro IDE machineId.
	if machineID != "" {
		credentials["machine_id"] = machineID
	}

	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:        name,
		Notes:       req.Notes,
		Platform:    service.PlatformKiro,
		Type:        service.AccountTypeOAuth,
		Credentials: credentials,
		ProxyID:     req.ProxyID,
		Concurrency: req.Concurrency,
		Priority:    req.Priority,
		GroupIDs:    req.GroupIDs,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, account)
}

func firstKiroTokenString(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func isKiroBuilderIDPayload(provider, authMethod, clientID, clientSecret string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	a := strings.ToLower(strings.TrimSpace(authMethod))
	return strings.Contains(p, "builder") || a == "idc" || a == "builderid" || (clientID != "" && clientSecret != "")
}

func parseKiroTokenExpiresAt(values ...json.RawMessage) int64 {
	for _, raw := range values {
		raw = bytesTrimSpace(raw)
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
			return normalizeKiroEpoch(n)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		if ts := parseKiroExpiresAtString(s); ts > 0 {
			return ts
		}
	}
	return 0
}

func parseKiroExpiresAtString(raw string) int64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		return normalizeKiroEpoch(n)
	}
	layouts := []string{
		time.RFC3339,
		"2006/01/02 15:04:05",
		"2006-01-02 15:04:05",
		"2006/1/2 15:04:05",
		"2006-1-2 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func normalizeKiroEpoch(n float64) int64 {
	if n > 1_000_000_000_000 {
		n = n / 1000
	}
	return int64(n)
}

func bytesTrimSpace(in []byte) []byte {
	start := 0
	for start < len(in) && (in[start] == ' ' || in[start] == '\n' || in[start] == '\r' || in[start] == '\t') {
		start++
	}
	end := len(in)
	for end > start && (in[end-1] == ' ' || in[end-1] == '\n' || in[end-1] == '\r' || in[end-1] == '\t') {
		end--
	}
	return in[start:end]
}

// FetchAccountQuota resolves a Kiro account by id and returns a fresh quota
// snapshot (subscription tier + vibe/spec request counters). Invoked from the
// account detail panel in the admin UI.
// POST /api/v1/admin/kiro/accounts/:id/quota
func (h *KiroOAuthHandler) FetchAccountQuota(c *gin.Context) {
	idParam := c.Param("id")
	if idParam == "" {
		response.BadRequest(c, "account id is required")
		return
	}
	id, err := parseInt64(idParam)
	if err != nil {
		response.BadRequest(c, "invalid account id: "+err.Error())
		return
	}

	account, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if account.Platform != service.PlatformKiro {
		response.BadRequest(c, "account is not a kiro account")
		return
	}
	if h.quotaFetcher == nil {
		response.InternalError(c, "kiro quota fetcher not configured")
		return
	}

	snapshot, err := h.quotaFetcher.FetchQuota(c.Request.Context(), account)
	if err != nil {
		response.BadRequest(c, "获取 Kiro 用量失败: "+err.Error())
		return
	}

	response.Success(c, gin.H{
		"subscription_type": snapshot.SubscriptionType,
		"vibe_used":         snapshot.VibeUsed,
		"vibe_limit":        snapshot.VibeLimit,
		"spec_used":         snapshot.SpecUsed,
		"spec_limit":        snapshot.SpecLimit,
		"raw":               snapshot.Raw,
	})
}

// parseInt64 is a local helper to avoid importing strconv at the top of the
// file when only a single call site needs it.
func parseInt64(s string) (int64, error) {
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid digit %q", c)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// ============================================================================
// AWS Builder ID — OIDC Device Authorization Grant
// ============================================================================

// StartBuilderIDLogin initiates the OIDC device authorization flow.
// POST /api/v1/admin/kiro/builderid/start
func (h *KiroOAuthHandler) StartBuilderIDLogin(c *gin.Context) {
	var req struct {
		Region string `json:"region"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.Region == "" {
		req.Region = "us-east-1"
	}

	ctx := c.Request.Context()

	// Step 1: Register a fresh OIDC client (public, device_code + refresh_token grants).
	reg, err := kiro.RegisterOIDCClient(ctx, nil, req.Region, "sub2api-kiro", "", nil)
	if err != nil {
		response.InternalError(c, "OIDC client registration failed: "+err.Error())
		return
	}

	// Step 2: Start device authorization.
	auth, err := kiro.StartOIDCDeviceAuthorization(ctx, nil, req.Region, reg, "")
	if err != nil {
		response.InternalError(c, "OIDC device authorization failed: "+err.Error())
		return
	}

	// Store session server-side so PollBuilderIDLogin can retrieve it.
	sessionID := fmt.Sprintf("bid_%d", time.Now().UnixNano())
	h.kiroOAuthService.StoreBuilderIDSession(sessionID, &service.KiroBuilderIDSession{
		ClientID:     reg.ClientID,
		ClientSecret: reg.ClientSecret,
		DeviceCode:   auth.DeviceCode,
		Interval:     auth.Interval,
		Region:       req.Region,
		ExpiresAt:    time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second),
	})

	verificationURI := auth.VerificationURIComplete
	if verificationURI == "" {
		verificationURI = auth.VerificationURI
	}

	response.Success(c, gin.H{
		"session_id":       sessionID,
		"user_code":        auth.UserCode,
		"verification_uri": verificationURI,
		"interval":         auth.Interval,
		"expires_in":       auth.ExpiresIn,
	})
}

// PollBuilderIDLogin polls the OIDC /token endpoint for the device grant.
// POST /api/v1/admin/kiro/builderid/poll
func (h *KiroOAuthHandler) PollBuilderIDLogin(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "session_id is required")
		return
	}

	sess := h.kiroOAuthService.GetBuilderIDSession(req.SessionID)
	if sess == nil {
		response.BadRequest(c, "session not found or expired")
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		h.kiroOAuthService.DeleteBuilderIDSession(req.SessionID)
		response.BadRequest(c, "device authorization expired")
		return
	}

	reg := &kiro.OIDCClientRegistration{ClientID: sess.ClientID, ClientSecret: sess.ClientSecret}
	tokenResp, status, err := kiro.PollOIDCDeviceToken(c.Request.Context(), nil, sess.Region, reg, sess.DeviceCode)
	if err != nil {
		h.kiroOAuthService.DeleteBuilderIDSession(req.SessionID)
		response.BadRequest(c, err.Error())
		return
	}

	if status == kiro.OIDCPollPending || status == kiro.OIDCPollSlowDown {
		interval := sess.Interval
		if status == kiro.OIDCPollSlowDown {
			interval += 5
			sess.Interval = interval
		}
		response.Success(c, gin.H{"status": string(status), "interval": interval})
		return
	}

	// Completed — clean up session and return tokens.
	h.kiroOAuthService.DeleteBuilderIDSession(req.SessionID)
	response.Success(c, gin.H{
		"status":        "completed",
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"expires_in":    tokenResp.ExpiresIn,
		"client_id":     sess.ClientID,
		"client_secret": sess.ClientSecret,
		"region":        sess.Region,
		"profile_arn":   tokenResp.ProfileArn,
	})
}

// CreateAccountFromBuilderID creates a Kiro account from a completed Builder ID
// device grant. The frontend calls this after PollBuilderIDLogin returns "completed".
// POST /api/v1/admin/kiro/builderid/create-account
func (h *KiroOAuthHandler) CreateAccountFromBuilderID(c *gin.Context) {
	var req struct {
		AccessToken  string  `json:"access_token" binding:"required"`
		RefreshToken string  `json:"refresh_token" binding:"required"`
		ClientID     string  `json:"client_id" binding:"required"`
		ClientSecret string  `json:"client_secret" binding:"required"`
		Region       string  `json:"region"`
		ExpiresIn    int64   `json:"expires_in"`
		Name         string  `json:"name"`
		Notes        *string `json:"notes"`
		Concurrency  int     `json:"concurrency"`
		Priority     int     `json:"priority"`
		GroupIDs     []int64 `json:"group_ids"`
		ProfileArn   string  `json:"profile_arn"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}
	if req.Region == "" {
		req.Region = "us-east-1"
	}

	name := req.Name
	if name == "" {
		name = "Kiro Builder ID Account"
	}

	var expiresAt int64
	if req.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + req.ExpiresIn
	}

	// profileArn: if not provided by the OIDC response, leave empty.
	// Builder ID (AWS SSO OIDC) accounts do NOT need profileArn — Kiro
	// accepts the request without it. Only Desktop Auth (Google social
	// login) accounts require it, and those get it from the initial
	// ExchangeToken response.
	profileArn := req.ProfileArn

	credentials := map[string]any{
		"access_token":  req.AccessToken,
		"refresh_token": req.RefreshToken,
		"client_id":     req.ClientID,
		"client_secret": req.ClientSecret,
		"region":        req.Region,
		"expires_at":    expiresAt,
		"token_type":    "Bearer",
		"profile_arn":   profileArn,
		"auth_method":   "builderid",
	}

	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:        name,
		Notes:       req.Notes,
		Platform:    service.PlatformKiro,
		Type:        service.AccountTypeOAuth,
		Credentials: credentials,
		Concurrency: req.Concurrency,
		Priority:    req.Priority,
		GroupIDs:    req.GroupIDs,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, account)
}
