package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

// KiroOAuthService orchestrates the Kiro (Cognito + Google IdP) OAuth flow.
//
// Flow:
//  1. Admin clicks "Add Kiro account" -> GenerateAuthURL returns a Cognito
//     authorize URL + sessionID (used later as a map key for PKCE verifier).
//  2. Admin completes Google login in a browser. Cognito redirects back to
//     https://app.kiro.dev/signin/oauth?code=XXX&state=YYY. The browser tab
//     shows a "Sign in..." page.
//  3. Admin copies the full URL (or just ?code=...&state=...) and pastes it
//     back into the dialog. ExchangeCode looks up the PKCE verifier by
//     sessionID, validates state, and performs the CBOR ExchangeToken RPC.
//  4. On success we return tokens + user info; the handler layer persists a
//     new Account record.
type KiroOAuthService struct {
	sessionStore       *kiro.SessionStore
	proxyRepo          ProxyRepository
	builderIDSessions  sync.Map // map[string]*KiroBuilderIDSession
}

// KiroBuilderIDSession holds transient state for an in-progress Builder ID
// device authorization flow.
type KiroBuilderIDSession struct {
	ClientID     string
	ClientSecret string
	DeviceCode   string
	Interval     int
	Region       string
	ExpiresAt    time.Time
}

// StoreBuilderIDSession saves a Builder ID session for later polling.
func (s *KiroOAuthService) StoreBuilderIDSession(id string, sess *KiroBuilderIDSession) {
	s.builderIDSessions.Store(id, sess)
}

// GetBuilderIDSession retrieves a Builder ID session by ID.
func (s *KiroOAuthService) GetBuilderIDSession(id string) *KiroBuilderIDSession {
	v, ok := s.builderIDSessions.Load(id)
	if !ok {
		return nil
	}
	return v.(*KiroBuilderIDSession)
}

// DeleteBuilderIDSession removes a Builder ID session.
func (s *KiroOAuthService) DeleteBuilderIDSession(id string) {
	s.builderIDSessions.Delete(id)
}

// NewKiroOAuthService constructs the service. The sessionStore has its own
// janitor goroutine; call Stop() during graceful shutdown.
func NewKiroOAuthService(proxyRepo ProxyRepository) *KiroOAuthService {
	return &KiroOAuthService{
		sessionStore: kiro.NewSessionStore(),
		proxyRepo:    proxyRepo,
	}
}

// Stop releases background resources.
func (s *KiroOAuthService) Stop() { s.sessionStore.Stop() }

// KiroAuthURLResult is the response returned to the admin console when
// initiating the OAuth flow.
type KiroAuthURLResult struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
}

// GenerateAuthURL builds a Cognito authorize URL bound to a fresh PKCE pair
// and stores the verifier server-side keyed by SessionID.
func (s *KiroOAuthService) GenerateAuthURL(ctx context.Context, proxyID *int64) (*KiroAuthURLResult, error) {
	state, err := kiro.GenerateState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	verifier, err := kiro.GenerateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("generate code verifier: %w", err)
	}
	sessionID, err := kiro.GenerateSessionID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}

	var proxyURL string
	if proxyID != nil && s.proxyRepo != nil {
		proxy, perr := s.proxyRepo.GetByID(ctx, *proxyID)
		if perr == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	s.sessionStore.Set(sessionID, &kiro.OAuthSession{
		State:        state,
		CodeVerifier: verifier,
		ProxyURL:     proxyURL,
		CreatedAt:    time.Now(),
	})

	challenge := kiro.GenerateCodeChallenge(verifier)
	return &KiroAuthURLResult{
		AuthURL:   kiro.BuildAuthorizationURL(state, challenge),
		SessionID: sessionID,
		State:     state,
	}, nil
}

// KiroExchangeCodeInput is the payload for completing the flow.
type KiroExchangeCodeInput struct {
	SessionID   string
	CallbackURL string // raw URL or "code=..&state=.." string pasted by the user
	ProxyID     *int64
}

// KiroExchangeResult combines the tokens and user info captured during login.
type KiroExchangeResult struct {
	Tokens   *kiro.TokenInfo
	UserInfo *kiro.UserInfo
	Usage    *kiro.UsageAndLimits
	ProxyURL string
}

// ExchangeCode redeems the authorization code produced by the browser flow.
func (s *KiroOAuthService) ExchangeCode(ctx context.Context, in *KiroExchangeCodeInput) (*KiroExchangeResult, error) {
	if in == nil || in.SessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	sess, ok := s.sessionStore.Get(in.SessionID)
	if !ok {
		return nil, fmt.Errorf("session %q not found or expired", in.SessionID)
	}

	code, state, err := kiro.ExtractCodeAndState(in.CallbackURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback URL: %w", err)
	}
	if sess.State != "" && state != "" && sess.State != state {
		return nil, fmt.Errorf("state mismatch (possible CSRF)")
	}

	proxyURL := sess.ProxyURL
	if in.ProxyID != nil && s.proxyRepo != nil {
		proxy, perr := s.proxyRepo.GetByID(ctx, *in.ProxyID)
		if perr == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	client, err := kiro.NewClient(kiro.WithProxyURL(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("build kiro client: %w", err)
	}
	defer client.Close()

	tokens, err := client.ExchangeToken(ctx, kiro.ExchangeCodeInput{
		Code:         code,
		CodeVerifier: sess.CodeVerifier,
		IdP:          "Google",
		RedirectURI:  kiro.DefaultRedirectURI,
	})
	if err != nil {
		return nil, fmt.Errorf("exchange token: %w", err)
	}

	// Session no longer needed (PKCE verifier is single-use).
	s.sessionStore.Delete(in.SessionID)

	result := &KiroExchangeResult{
		Tokens:   tokens,
		ProxyURL: proxyURL,
	}

	// Best-effort enrichment: pull user info + quota snapshot so the admin
	// UI can show useful context. Failures are non-fatal.
	if info, err := client.GetUserInfo(ctx, tokens.AccessToken); err == nil {
		result.UserInfo = info
		if tokens.Email == "" {
			tokens.Email = info.Email
		}
		if tokens.UserID == "" {
			tokens.UserID = info.UserID
		}
	}
	if usage, err := client.GetUserUsageAndLimits(ctx, tokens.AccessToken); err == nil {
		result.Usage = usage
	}

	// After any authenticated RPC above, the client has scraped (or been
	// handed) a valid CSRF token. Persist it so subsequent RefreshToken
	// calls can skip the portal-HTML scrape entirely — which is the only
	// source of random 401 "Invalid CSRF token" failures once access_token
	// is expired and the portal no longer yields the meta tag.
	if tokens.CSRFToken == "" {
		if csrf := strings.TrimSpace(client.CSRFToken()); csrf != "" {
			tokens.CSRFToken = csrf
		}
	}

	// Last-resort fallback: enrichment calls may have been skipped or
	// silently failed. Explicitly seed the AccessToken cookie and scrape
	// the portal HTML one more time so csrf gets persisted no matter what.
	if tokens.CSRFToken == "" && strings.TrimSpace(tokens.AccessToken) != "" {
		client.SeedAccessToken(tokens.AccessToken)
		if err := client.EnsureCSRFToken(ctx); err == nil {
			if csrf := strings.TrimSpace(client.CSRFToken()); csrf != "" {
				tokens.CSRFToken = csrf
			}
		}
	}

	return result, nil
}

// RefreshAccountToken refreshes the Kiro session tied to an account and
// returns the new TokenInfo (access / refresh / expires_at). Caller persists.
//
// Both accessToken and refreshToken are required: Kiro's CSRF protection
// requires a valid AccessToken cookie to fetch the portal HTML (which carries
// the CSRF token) before /RefreshToken is accepted. When the AccessToken has
// already expired, the admin flow should capture a fresh one via ExchangeCode;
// callers that encounter this should surface a re-login prompt.
//
// If a previously rotated csrfToken is available (returned by the last
// RefreshToken RPC and stored alongside the account credentials), passing it
// via csrfToken skips the HTML scrape entirely.
func (s *KiroOAuthService) RefreshAccountToken(ctx context.Context, accessToken, refreshToken, csrfToken, proxyURL string) (*kiro.TokenInfo, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("refresh token is empty")
	}
	client, err := kiro.NewClient(kiro.WithProxyURL(proxyURL))
	if err != nil {
		return nil, err
	}
	defer client.Close()
	if strings.TrimSpace(accessToken) != "" {
		client.SeedAccessToken(accessToken)
	}
	if strings.TrimSpace(csrfToken) != "" {
		client.SetCSRFToken(csrfToken)
	}
	return client.RefreshSession(ctx, refreshToken)
}

// BuildAccountCredentials returns the credentials map persisted on the
// Account row (accounts.credentials JSONB). Kept here so DB layout can evolve.
func (s *KiroOAuthService) BuildAccountCredentials(t *kiro.TokenInfo) map[string]any {
	if t == nil {
		return map[string]any{}
	}
	return map[string]any{
		"access_token":  t.AccessToken,
		"refresh_token": t.RefreshToken,
		"expires_at":    t.ExpiresAt,
		"token_type":    firstNonEmptyOrDefault(t.TokenType, "Bearer"),
		"profile_arn":   t.ProfileArn,
		"email":         t.Email,
		"user_id":       t.UserID,
		"csrf_token":    t.CSRFToken,
	}
}

func firstNonEmptyOrDefault(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
