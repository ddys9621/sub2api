// Package kiro provides a client for interacting with Amazon Kiro (Q Developer / CodeWhisperer) services.
//
// Kiro reverse-proxy support follows the same scheduling/billing infrastructure as other AI accounts
// in sub2api. This package contains:
//   - OAuth (Cognito + Google IdP) login helpers with PKCE
//   - Smithy rpc-v2-cbor protocol encoding/decoding (for ExchangeToken / RefreshToken)
//   - AWS EventStream binary frame parser (for /generateAssistantResponse streaming response)
//   - Request/response transformers: Anthropic Messages <-> Kiro generateAssistantResponse payload
package kiro

import "time"

// Default AWS region for Kiro endpoints.
const DefaultRegion = "us-east-1"

// Cognito (public) client ID for Kiro Web Portal OAuth flow.
// This value is publicly visible in the authorize URL on https://app.kiro.dev.
const CognitoClientID = "59bd15eh40ee7pc20h0bkcu7id"

// OAuthSession represents the transient state kept server-side while the user
// completes the OAuth code grant in the browser.
type OAuthSession struct {
	State        string    `json:"state"`
	CodeVerifier string    `json:"code_verifier"`
	ProxyURL     string    `json:"proxy_url,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// SessionTTL is the maximum lifetime of a pending OAuth session.
const SessionTTL = 30 * time.Minute

// TokenInfo is the decoded result of the ExchangeToken / RefreshToken CBOR RPC.
// Field names follow the Kiro service contract (camelCase). Additional fields
// observed on the wire are kept in Extra for forward compatibility.
type TokenInfo struct {
	AccessToken  string         `json:"accessToken"`
	RefreshToken string         `json:"refreshToken"`
	ExpiresIn    int64          `json:"expiresIn"`       // seconds
	ExpiresAt    int64          `json:"expiresAt"`       // unix seconds, filled by client
	TokenType    string         `json:"tokenType,omitempty"`
	ProfileArn   string         `json:"profileArn,omitempty"`
	Email        string         `json:"email,omitempty"`
	UserID       string         `json:"userId,omitempty"`
	// CSRFToken is rotated by RefreshToken RPCs and returned inline in the
	// response body. Persisting it alongside the account credentials lets us
	// skip the HTML-scrape step on subsequent refreshes.
	CSRFToken string         `json:"csrfToken,omitempty"`
	Extra     map[string]any `json:"-"`
}

// ExchangeCodeInput holds parameters for the ExchangeToken CBOR RPC.
type ExchangeCodeInput struct {
	Code         string
	CodeVerifier string
	IdP          string // e.g. "Google"
	RedirectURI  string
}

// UserInfo is a subset of Kiro GetUserInfo response used for account display.
type UserInfo struct {
	Email    string `json:"email,omitempty"`
	UserID   string `json:"userId,omitempty"`
	FullName string `json:"fullName,omitempty"`
}

// UsageAndLimits is a subset of Kiro GetUserUsageAndLimits used for quota display.
type UsageAndLimits struct {
	SubscriptionType     string `json:"subscriptionType,omitempty"`
	VibeRequestsUsed     int64  `json:"vibeRequestsUsed,omitempty"`
	VibeRequestsLimit    int64  `json:"vibeRequestsLimit,omitempty"`
	SpecRequestsUsed     int64  `json:"specRequestsUsed,omitempty"`
	SpecRequestsLimit    int64  `json:"specRequestsLimit,omitempty"`
	// Raw keeps the full response for display/debug.
	Raw map[string]any `json:"-"`
}
