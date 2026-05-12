package kiro

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
)

// ForbiddenError represents an upstream 403 (token expired / invalid).
type ForbiddenError struct {
	StatusCode int
	Body       string
}

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("kiro upstream 403: %s", strings.TrimSpace(e.Body))
}

// UpstreamError wraps any non-2xx response from the Kiro portal.
type UpstreamError struct {
	StatusCode int
	Body       string
	Operation  string
}

func (e *UpstreamError) Error() string {
	snippet := e.Body
	if len(snippet) > 300 {
		snippet = snippet[:300] + "..."
	}
	return fmt.Sprintf("kiro %s failed (HTTP %d): %s", e.Operation, e.StatusCode, snippet)
}

// Client is a thin HTTP client for the Kiro Web Portal RPC endpoints and
// the CodeWhisperer streaming API. It supports an optional outbound proxy
// and maintains a cookie jar so that HttpOnly cookies set on ExchangeToken
// (AccessToken / RefreshToken / UserId / Idp) are automatically attached on
// subsequent RPC calls.
type Client struct {
	httpClient *http.Client
	jar        http.CookieJar
	proxyURL   string
	baseURL    string // Portal base URL (without trailing slash)
	userAgent  string
	visitorID  string
	// lastCookies holds the cookies returned by the most recent RPC call,
	// keyed by name (HttpOnly & non-HttpOnly alike). Kiro sets AccessToken /
	// RefreshToken / UserId / Idp this way instead of returning them in the
	// response body.
	lastCookies map[string]*http.Cookie
	// csrfToken is fetched lazily from https://app.kiro.dev/ once the jar
	// carries an AccessToken cookie. Kiro's authenticated RPCs (RefreshToken,
	// GetUserInfo, GetUserUsageAndLimits, ...) reject requests without it.
	csrfToken string
}

// ClientOption customises Client construction.
type ClientOption func(*Client)

// WithProxyURL sets an outbound HTTP/HTTPS/SOCKS5 proxy.
func WithProxyURL(u string) ClientOption { return func(c *Client) { c.proxyURL = u } }

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) ClientOption { return func(c *Client) { c.userAgent = ua } }

// WithVisitorID overrides the generated x-kiro-visitorid header value.
func WithVisitorID(id string) ClientOption { return func(c *Client) { c.visitorID = id } }

// NewClient builds a Client with an optional outbound proxy.
func NewClient(opts ...ClientOption) (*Client, error) {
	c := &Client{
		baseURL:     KiroPortalBaseURL,
		userAgent:   defaultUserAgent(),
		visitorID:   generateVisitorID(),
		lastCookies: make(map[string]*http.Cookie),
	}
	for _, opt := range opts {
		opt(c)
	}

	jar, jerr := cookiejar.New(nil)
	if jerr != nil {
		return nil, fmt.Errorf("create cookie jar: %w", jerr)
	}
	c.jar = jar

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}

	if c.proxyURL != "" {
		proxyFn, err := proxyFnFromURL(c.proxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = proxyFn
	}

	c.httpClient = &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
		Jar:       jar,
	}
	return c, nil
}

// HTTPClient exposes the underlying http.Client (useful for the EventStream
// forwarder which needs to disable the global timeout).
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// UserAgent returns the configured User-Agent string.
func (c *Client) UserAgent() string { return c.userAgent }

// VisitorID returns the configured visitor identifier.
func (c *Client) VisitorID() string { return c.visitorID }

// Close releases idle connections held by the underlying transport.
func (c *Client) Close() {
	if tr, ok := c.httpClient.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// buildHeaders returns the default header set for Kiro RPC calls.
func (c *Client) buildHeaders(accessToken string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/cbor")
	h.Set("Accept", "application/cbor")
	h.Set("smithy-protocol", "rpc-v2-cbor")
	h.Set("User-Agent", c.userAgent)
	h.Set("x-amz-user-agent", "aws-sdk-js/1.0.0")
	h.Set("x-kiro-visitorid", c.visitorID)
	h.Set("Origin", "https://app.kiro.dev")
	h.Set("Referer", "https://app.kiro.dev/")
	if accessToken != "" {
		h.Set("Authorization", "Bearer "+accessToken)
	}
	if c.csrfToken != "" {
		h.Set("x-csrf-token", c.csrfToken)
	}
	return h
}

// ensureCSRFToken fetches the Kiro web portal HTML with the current cookie jar
// and extracts the <meta name="csrf-token" content="..."> value. Called lazily
// before any authenticated RPC. Idempotent.
func (c *Client) ensureCSRFToken(ctx context.Context) error {
	if c.csrfToken != "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://app.kiro.dev/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch kiro portal html: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kiro portal html returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return fmt.Errorf("read kiro portal html: %w", err)
	}
	token := extractMetaContent(string(body), "csrf-token")
	if token == "" {
		return fmt.Errorf("csrf-token meta tag not found; is the AccessToken cookie valid?")
	}
	c.csrfToken = token
	return nil
}

// SetCSRFToken overrides the cached CSRF token (for tests).
func (c *Client) SetCSRFToken(token string) { c.csrfToken = token }

// CSRFToken exposes the CSRF token currently held by the client. Populated
// after the first authenticated RPC (GetUserInfo / RefreshToken) or after
// a successful ensureCSRFToken portal scrape. Empty when no authenticated
// call has happened yet.
func (c *Client) CSRFToken() string { return c.csrfToken }

// EnsureCSRFToken is the exported variant of ensureCSRFToken. Callers that
// want to pre-populate the CSRF cache (e.g. right after ExchangeToken so
// it can be persisted) can invoke this directly.
func (c *Client) EnsureCSRFToken(ctx context.Context) error {
	return c.ensureCSRFToken(ctx)
}

// SeedAccessToken primes the cookie jar with an AccessToken cookie. This is
// required for operations that need CSRF: the portal HTML only embeds the
// csrf-token meta tag when the request carries a valid AccessToken cookie.
func (c *Client) SeedAccessToken(token string) {
	if token == "" || c.jar == nil {
		return
	}
	portalURL, err := url.Parse("https://app.kiro.dev")
	if err != nil {
		return
	}
	c.jar.SetCookies(portalURL, []*http.Cookie{{
		Name:  "AccessToken",
		Value: token,
		Path:  "/",
	}})
}

// extractMetaContent returns the content of <meta name="X" content="Y">.
// Minimal regex-style parse to avoid dragging in html/atom dependencies.
func extractMetaContent(html, metaName string) string {
	needle := `name="` + metaName + `"`
	for i := 0; i < len(html); {
		idx := indexOf(html[i:], needle)
		if idx < 0 {
			return ""
		}
		j := i + idx
		// Find the enclosing <meta ... > tag.
		tagStart := lastIndexBefore(html, "<meta", j)
		tagEnd := indexOf(html[j:], ">")
		if tagStart < 0 || tagEnd < 0 {
			i = j + len(needle)
			continue
		}
		tag := html[tagStart : j+tagEnd+1]
		content := extractAttr(tag, "content")
		if content != "" {
			return content
		}
		i = j + tagEnd + 1
	}
	return ""
}

func indexOf(s, sub string) int      { return strings.Index(s, sub) }
func lastIndexBefore(s, sub string, before int) int {
	if before > len(s) {
		before = len(s)
	}
	return strings.LastIndex(s[:before], sub)
}

func extractAttr(tag, attr string) string {
	keys := []string{attr + `="`, attr + `='`}
	for _, key := range keys {
		start := strings.Index(tag, key)
		if start < 0 {
			continue
		}
		start += len(key)
		quote := tag[start-1]
		end := strings.IndexByte(tag[start:], quote)
		if end < 0 {
			continue
		}
		return tag[start : start+end]
	}
	return ""
}

// rpc executes a Kiro Web Portal RPC call with a CBOR payload and decodes the
// response. Any non-200 response is returned as an UpstreamError (or
// ForbiddenError on HTTP 403).
//
// When needsCSRF is true, the client lazily fetches the CSRF token from the
// Kiro portal HTML before issuing the call. ExchangeToken does not require
// CSRF; all other operations (RefreshToken, GetUserInfo, ...) do.
func (c *Client) rpc(ctx context.Context, operation, accessToken string, needsCSRF bool, body map[string]any) (map[string]any, error) {
	if needsCSRF {
		if err := c.ensureCSRFToken(ctx); err != nil {
			return nil, fmt.Errorf("%s: %w", operation, err)
		}
	}

	payload, err := EncodeCBOR(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s payload: %w", operation, err)
	}

	u := fmt.Sprintf("%s/%s", c.baseURL, operation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = c.buildHeaders(accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s http request: %w", operation, err)
	}
	defer resp.Body.Close()

	// Capture Set-Cookie headers. Kiro uses HttpOnly cookies to deliver
	// AccessToken / RefreshToken / UserId / Idp after ExchangeToken.
	for _, ck := range resp.Cookies() {
		c.lastCookies[ck.Name] = ck
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("%s read body: %w", operation, err)
	}

	if resp.StatusCode == http.StatusForbidden {
		return nil, &ForbiddenError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Body may be CBOR error; attempt to decode for a human message.
		msg := string(respBody)
		if decoded, decErr := DecodeCBOR(respBody); decErr == nil {
			if m, ok := decoded.(map[string]any); ok {
				if errMsg := AsString(m, "message"); errMsg != "" {
					msg = errMsg
				}
			}
		}
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: msg, Operation: operation}
	}

	decoded, err := DecodeCBOR(respBody)
	if err != nil {
		return nil, fmt.Errorf("%s decode cbor: %w (head: %x)", operation, err, headBytes(respBody))
	}
	m, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected cbor map, got %T", operation, decoded)
	}
	return m, nil
}

func headBytes(b []byte) []byte {
	if len(b) > 32 {
		return b[:32]
	}
	return b
}

// ---------------------------------------------------------------------------
// OAuth RPC methods
// ---------------------------------------------------------------------------

// ExchangeToken exchanges an authorization code for session tokens.
func (c *Client) ExchangeToken(ctx context.Context, in ExchangeCodeInput) (*TokenInfo, error) {
	idp := in.IdP
	if idp == "" {
		idp = "Google"
	}
	redirect := in.RedirectURI
	if redirect == "" {
		redirect = DefaultRedirectURI
	}
	payload := map[string]any{
		"code":         in.Code,
		"codeVerifier": in.CodeVerifier,
		"idp":          idp,
		"redirectUri":  redirect,
	}
	resp, err := c.rpc(ctx, "ExchangeToken", "", false, payload)
	if err != nil {
		return nil, err
	}
	ti := buildTokenInfo(resp)
	c.mergeCookiesIntoToken(ti)
	return ti, nil
}

// RefreshSession renews access/refresh tokens.
//
// Observed RPC shape: POST /operation/RefreshToken with body
// { refreshToken, idp }. Auth header is NOT required; the refresh token
// itself is the credential. The CSRF token meta tag must however be
// present, so the caller must seed a valid AccessToken via SeedAccessToken
// before invoking RefreshSession.
func (c *Client) RefreshSession(ctx context.Context, refreshToken string) (*TokenInfo, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is empty")
	}
	// Seed the cookie jar so the RPC call is indistinguishable from a
	// same-session refresh initiated by the Kiro web portal.
	portalURL, _ := url.Parse("https://app.kiro.dev")
	if c.jar != nil && portalURL != nil {
		c.jar.SetCookies(portalURL, []*http.Cookie{
			{Name: "RefreshToken", Value: refreshToken, Path: "/"},
			{Name: "Idp", Value: "Google", Path: "/"},
		})
	}
	payload := map[string]any{
		"refreshToken": refreshToken,
		"idp":          "Google",
	}
	resp, err := c.rpc(ctx, "RefreshToken", "", true, payload)
	if err != nil {
		return nil, err
	}
	ti := buildTokenInfo(resp)
	c.mergeCookiesIntoToken(ti)
	// If upstream didn't return a fresh refresh token, keep the old one.
	if ti.RefreshToken == "" {
		ti.RefreshToken = refreshToken
	}
	// Rotate the CSRF token: later RPC calls within this Client reuse the
	// newly issued token instead of re-scraping the portal HTML.
	if ti.CSRFToken != "" {
		c.csrfToken = ti.CSRFToken
	}
	return ti, nil
}

// mergeCookiesIntoToken fills in token fields from cookies captured during the
// most recent RPC. Kiro ships AccessToken / RefreshToken / UserId / Idp as
// HttpOnly cookies rather than response-body fields on ExchangeToken.
func (c *Client) mergeCookiesIntoToken(ti *TokenInfo) {
	if ti == nil {
		return
	}
	if ck := c.lastCookies["AccessToken"]; ck != nil && ti.AccessToken == "" {
		ti.AccessToken = ck.Value
	}
	if ck := c.lastCookies["RefreshToken"]; ck != nil && ti.RefreshToken == "" {
		ti.RefreshToken = ck.Value
	}
	if ck := c.lastCookies["UserId"]; ck != nil && ti.UserID == "" {
		ti.UserID = ck.Value
	}
	// Some variants of the response expose the expiry via an access-token
	// cookie Max-Age. Prefer the explicit expiresIn field from the body.
	if ti.ExpiresAt == 0 && ti.ExpiresIn > 0 {
		ti.ExpiresAt = time.Now().Unix() + ti.ExpiresIn - 60
	}
}

// GetUserInfo fetches the authenticated user's profile.
func (c *Client) GetUserInfo(ctx context.Context, accessToken string) (*UserInfo, error) {
	resp, err := c.rpc(ctx, "GetUserInfo", accessToken, true, map[string]any{})
	if err != nil {
		return nil, err
	}
	return &UserInfo{
		Email:    AsString(resp, "email"),
		UserID:   AsString(resp, "userId"),
		FullName: AsString(resp, "fullName"),
	}, nil
}

// GetUserUsageAndLimits fetches the authenticated user's quota snapshot.
func (c *Client) GetUserUsageAndLimits(ctx context.Context, accessToken string) (*UsageAndLimits, error) {
	resp, err := c.rpc(ctx, "GetUserUsageAndLimits", accessToken, true, map[string]any{})
	if err != nil {
		return nil, err
	}
	return &UsageAndLimits{
		SubscriptionType:  AsString(resp, "subscriptionType"),
		VibeRequestsUsed:  AsInt64(resp, "vibeRequestsUsed"),
		VibeRequestsLimit: AsInt64(resp, "vibeRequestsLimit"),
		SpecRequestsUsed:  AsInt64(resp, "specRequestsUsed"),
		SpecRequestsLimit: AsInt64(resp, "specRequestsLimit"),
		Raw:               resp,
	}, nil
}

// ListAvailableModels returns the list of models surfaced to the logged-in
// account (free / pro / enterprise). Returns raw CBOR-decoded map for
// forward compatibility.
func (c *Client) ListAvailableModels(ctx context.Context, accessToken string) (map[string]any, error) {
	return c.rpc(ctx, "ListAvailableModels", accessToken, true, map[string]any{})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildTokenInfo(resp map[string]any) *TokenInfo {
	now := time.Now().Unix()
	expiresIn := AsInt64(resp, "expiresIn")
	// Safety margin: refresh 60s before upstream expiry.
	expiresAt := now + expiresIn - 60
	if expiresIn <= 0 {
		expiresAt = now + 3300 // ~55min default
	}
	return &TokenInfo{
		AccessToken:  AsString(resp, "accessToken"),
		RefreshToken: AsString(resp, "refreshToken"),
		ExpiresIn:    expiresIn,
		ExpiresAt:    expiresAt,
		TokenType:    AsString(resp, "tokenType"),
		ProfileArn:   AsString(resp, "profileArn"),
		Email:        AsString(resp, "email"),
		UserID:       AsString(resp, "userId"),
		CSRFToken:    AsString(resp, "csrfToken"),
		Extra:        resp,
	}
}

func defaultUserAgent() string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
}

func generateVisitorID() string {
	ts := time.Now().UnixMilli()
	rnd, _ := GenerateRandomBytes(6)
	return fmt.Sprintf("%d-%x", ts, rnd)
}

func proxyFnFromURL(raw string) (func(*http.Request) (*url.URL, error), error) {
	_, parsed, err := proxyurl.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}
	if parsed == nil {
		return http.ProxyFromEnvironment, nil
	}
	return http.ProxyURL(parsed), nil
}
