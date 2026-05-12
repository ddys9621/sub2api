// Package kiro — oidc.go implements the AWS SSO OIDC Device Authorization
// flow used by kiro-cli and other Builder ID / Enterprise SSO clients.
//
// Flow (RFC 8628 "Device Authorization Grant" tailored for AWS SSO OIDC):
//
//  1. RegisterClient  → clientId + clientSecret
//  2. DeviceAuthorize → userCode + verificationUri(+Complete) + deviceCode
//  3. Operator opens verificationUri in a browser, logs in (Google / GitHub /
//     Email...), and confirms the user code.
//  4. PollToken (every Interval seconds) → accessToken + refreshToken
//
// Refresh:
//   RefreshOIDC(refreshToken, clientId, clientSecret) → new access/refresh token.
//
// All endpoints live under https://oidc.{region}.amazonaws.com/. Region is
// independent from the eventual CodeWhisperer API region.
package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultOIDCRegion is the region used for the Builder ID SSO OIDC endpoints.
// AWS Builder ID itself is a global service but its token endpoints are
// regional — us-east-1 is the home region.
const DefaultOIDCRegion = "us-east-1"

// BuilderIDStartURL is the issuer / portal URL used when registering an OIDC
// client for AWS Builder ID. Enterprise SSO users pass their own start URL
// instead (e.g. https://d-abcdef0123.awsapps.com/start).
const BuilderIDStartURL = "https://view.awsapps.com/start"

// DefaultOIDCScopes are the CodeWhisperer / Q Developer scopes required for a
// Kiro-equivalent access token. They match what the official kiro-cli
// requests during `kiro-cli login`.
var DefaultOIDCScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
	"codewhisperer:transformations",
	"codewhisperer:taskassist",
}

// OIDCClientRegistration carries the public/secret pair returned by
// /client/register. Persist both alongside the account credentials: they are
// required to call /token (both on first exchange and every refresh).
type OIDCClientRegistration struct {
	ClientID     string    `json:"clientId"`
	ClientSecret string    `json:"clientSecret"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// OIDCDeviceAuthorization describes the user-facing half of the device grant.
// The admin UI should render VerificationURIComplete (or VerificationURI +
// UserCode) so the user can complete the login in a browser.
type OIDCDeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                int
	ExpiresIn               int
}

// OIDCTokenResponse is the successful /token response.
type OIDCTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int64  `json:"expiresIn"`
	ProfileArn   string `json:"profileArn"`
}

// OIDCPollStatus is the non-fatal status returned during polling. When Err is
// non-nil the device grant has been permanently denied or expired.
type OIDCPollStatus string

const (
	OIDCPollPending  OIDCPollStatus = "pending"
	OIDCPollSlowDown OIDCPollStatus = "slow_down"
	OIDCPollDone     OIDCPollStatus = "completed"
)

// RegisterOIDCClient performs POST /client/register. scopes defaults to the
// standard CodeWhisperer set when empty. issuerURL defaults to BuilderIDStartURL.
//
// Amazon bakes a 90-day expiry into the returned clientSecret; callers should
// re-register once it's close to expiry (or, more simply, when /token starts
// returning invalid_client).
func RegisterOIDCClient(ctx context.Context, httpClient *http.Client, region, clientName, issuerURL string, scopes []string) (*OIDCClientRegistration, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(region) == "" {
		region = DefaultOIDCRegion
	}
	if strings.TrimSpace(clientName) == "" {
		clientName = "sub2api-kiro"
	}
	if strings.TrimSpace(issuerURL) == "" {
		issuerURL = BuilderIDStartURL
	}
	if len(scopes) == 0 {
		scopes = DefaultOIDCScopes
	}

	payload := map[string]any{
		"clientName": clientName,
		"clientType": "public",
		"scopes":     scopes,
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"issuerUrl":  issuerURL,
	}
	raw, err := oidcPostJSON(ctx, httpClient, region, "/client/register", payload)
	if err != nil {
		return nil, fmt.Errorf("oidc register client: %w", err)
	}

	var resp struct {
		ClientID              string `json:"clientId"`
		ClientSecret          string `json:"clientSecret"`
		ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt"` // unix seconds
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("oidc register parse: %w", err)
	}
	if resp.ClientID == "" || resp.ClientSecret == "" {
		return nil, fmt.Errorf("oidc register returned empty client credentials")
	}
	expires := time.Time{}
	if resp.ClientSecretExpiresAt > 0 {
		expires = time.Unix(resp.ClientSecretExpiresAt, 0)
	}
	return &OIDCClientRegistration{ClientID: resp.ClientID, ClientSecret: resp.ClientSecret, ExpiresAt: expires}, nil
}

// StartOIDCDeviceAuthorization triggers POST /device_authorization.
func StartOIDCDeviceAuthorization(ctx context.Context, httpClient *http.Client, region string, reg *OIDCClientRegistration, startURL string) (*OIDCDeviceAuthorization, error) {
	if reg == nil {
		return nil, fmt.Errorf("client registration is nil")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(region) == "" {
		region = DefaultOIDCRegion
	}
	if strings.TrimSpace(startURL) == "" {
		startURL = BuilderIDStartURL
	}

	payload := map[string]string{
		"clientId":     reg.ClientID,
		"clientSecret": reg.ClientSecret,
		"startUrl":     startURL,
	}
	raw, err := oidcPostJSON(ctx, httpClient, region, "/device_authorization", payload)
	if err != nil {
		return nil, fmt.Errorf("oidc device authorization: %w", err)
	}

	var resp struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("oidc device authorization parse: %w", err)
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		return nil, fmt.Errorf("oidc device authorization returned empty deviceCode/userCode")
	}
	if resp.Interval <= 0 {
		resp.Interval = 5
	}
	if resp.ExpiresIn <= 0 {
		resp.ExpiresIn = 600
	}
	return &OIDCDeviceAuthorization{
		DeviceCode:              resp.DeviceCode,
		UserCode:                resp.UserCode,
		VerificationURI:         resp.VerificationURI,
		VerificationURIComplete: resp.VerificationURIComplete,
		Interval:                resp.Interval,
		ExpiresIn:               resp.ExpiresIn,
	}, nil
}

// PollOIDCDeviceToken polls /token once. Returns (token, pollStatus, error).
// Non-fatal statuses (pending / slow_down) come back with a nil token and a
// nil error; the caller should sleep Interval seconds (or more on slow_down)
// and try again. Fatal outcomes (expired_token / access_denied) return a
// non-nil error.
func PollOIDCDeviceToken(ctx context.Context, httpClient *http.Client, region string, reg *OIDCClientRegistration, deviceCode string) (*OIDCTokenResponse, OIDCPollStatus, error) {
	if reg == nil {
		return nil, "", fmt.Errorf("client registration is nil")
	}
	if strings.TrimSpace(deviceCode) == "" {
		return nil, "", fmt.Errorf("device code is empty")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(region) == "" {
		region = DefaultOIDCRegion
	}

	payload := map[string]string{
		"clientId":     reg.ClientID,
		"clientSecret": reg.ClientSecret,
		"grantType":    "urn:ietf:params:oauth:grant-type:device_code",
		"deviceCode":   deviceCode,
	}
	raw, status, err := oidcPostJSONRaw(ctx, httpClient, region, "/token", payload)
	if err != nil {
		return nil, "", fmt.Errorf("oidc poll token: %w", err)
	}

	if status >= 200 && status < 300 {
		var resp OIDCTokenResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, "", fmt.Errorf("oidc poll parse: %w", err)
		}
		if resp.AccessToken == "" {
			return nil, "", fmt.Errorf("oidc token response empty accessToken")
		}
		return &resp, OIDCPollDone, nil
	}

	// Authorization-pending / slow-down / expired-token / access-denied all
	// come back with HTTP 400 and an "error" string in the body.
	var errBody struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &errBody)
	switch errBody.Error {
	case "authorization_pending":
		return nil, OIDCPollPending, nil
	case "slow_down":
		return nil, OIDCPollSlowDown, nil
	case "expired_token":
		return nil, "", fmt.Errorf("device code expired (user did not finish login in time)")
	case "access_denied":
		return nil, "", fmt.Errorf("user denied authorization")
	default:
		return nil, "", fmt.Errorf("oidc poll unexpected response (HTTP %d): %s", status, truncateForError(raw, 512))
	}
}

// RefreshOIDCToken exchanges a refresh_token for a new access_token/refresh_token
// pair. This is the endpoint used by every background refresh for
// Builder-ID-based accounts; it never requires CSRF or cookies.
func RefreshOIDCToken(ctx context.Context, httpClient *http.Client, region string, reg *OIDCClientRegistration, refreshToken string) (*OIDCTokenResponse, error) {
	if reg == nil {
		return nil, fmt.Errorf("client registration is nil")
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("refresh token is empty")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(region) == "" {
		region = DefaultOIDCRegion
	}

	payload := map[string]string{
		"clientId":     reg.ClientID,
		"clientSecret": reg.ClientSecret,
		"grantType":    "refresh_token",
		"refreshToken": refreshToken,
	}
	raw, err := oidcPostJSON(ctx, httpClient, region, "/token", payload)
	if err != nil {
		return nil, fmt.Errorf("oidc refresh: %w", err)
	}
	var resp OIDCTokenResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("oidc refresh parse: %w", err)
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("oidc refresh empty accessToken (body=%s)", truncateForError(raw, 256))
	}
	if resp.RefreshToken == "" {
		// AWS sometimes omits refresh_token on refresh; keep the old one.
		resp.RefreshToken = refreshToken
	}
	return &resp, nil
}

// ---- internal helpers ----

func oidcEndpointURL(region, path string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com%s", region, path)
}

func oidcPostJSON(ctx context.Context, httpClient *http.Client, region, path string, payload any) ([]byte, error) {
	raw, status, err := oidcPostJSONRaw(ctx, httpClient, region, path, payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", status, truncateForError(raw, 512))
	}
	return raw, nil
}

func oidcPostJSONRaw(ctx context.Context, httpClient *http.Client, region, path string, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oidcEndpointURL(region, path), bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sub2api-kiro-oidc/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return raw, resp.StatusCode, nil
}
