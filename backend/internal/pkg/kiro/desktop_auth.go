// Package kiro — desktop_auth.go implements the lightweight refresh endpoint
// used by Kiro IDE for social-login accounts (Google / GitHub / Amazon). This
// endpoint accepts a plain JSON body {"refreshToken":"..."} and returns a new
// {accessToken, refreshToken, expiresIn} without requiring a CSRF token or the
// CBOR rpc-v2 wire format used by app.kiro.dev. It is the same endpoint the
// official Kiro Desktop IDE calls during its background refresh loop.
//
// Advantages over app.kiro.dev/service/.../RefreshToken:
//   - No CSRF cookie/meta-tag scrape (eliminates HTTP 401 "Invalid CSRF token").
//   - Plain JSON, no CBOR encoding/decoding.
//   - Stable across client rotations.
//
// We keep the CBOR path (RefreshSession in client.go) as a fallback for legacy
// accounts that were minted before we started persisting clean credentials.
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

// DesktopRefreshEndpoint is the public Kiro IDE refresh URL for social-login
// accounts. Region is fixed to us-east-1 because the Cognito user pool
// backing social logins lives there.
const DesktopRefreshEndpoint = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"

// RefreshDesktop exchanges a long-lived refreshToken for a fresh accessToken
// via the Kiro IDE auth service. Returns a minimal TokenInfo that the caller
// merges into the account credentials.
//
// The new TokenInfo keeps ProfileArn / Email / UserID / CSRFToken empty since
// this endpoint only rotates access/refresh; the caller must preserve the
// previous values (preserveIfEmpty in KiroTokenRefresher already does).
func (c *Client) RefreshDesktop(ctx context.Context, refreshToken string) (*TokenInfo, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("refresh token is empty")
	}

	payload := map[string]string{"refreshToken": refreshToken}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal refresh payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DesktopRefreshEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro desktop refresh: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kiro desktop refresh failed (HTTP %d): %s", resp.StatusCode, truncateForError(raw, 512))
	}

	var parsed struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		TokenType    string `json:"tokenType"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w (body=%s)", err, truncateForError(raw, 256))
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("kiro desktop refresh returned no accessToken (body=%s)", truncateForError(raw, 256))
	}

	expiresAt := int64(0)
	if parsed.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + parsed.ExpiresIn
	}

	ti := &TokenInfo{
		AccessToken:  parsed.AccessToken,
		RefreshToken: firstNonEmptyString(parsed.RefreshToken, refreshToken),
		ExpiresIn:    parsed.ExpiresIn,
		ExpiresAt:    expiresAt,
		TokenType:    firstNonEmptyString(parsed.TokenType, "Bearer"),
	}
	return ti, nil
}

func firstNonEmptyString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func truncateForError(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
