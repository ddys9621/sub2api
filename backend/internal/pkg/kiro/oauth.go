package kiro

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Kiro OAuth endpoints.
const (
	// Cognito domain hosts the /oauth2/authorize endpoint used by app.kiro.dev.
	CognitoDomain = "kiro-prod-us-east-1.auth.us-east-1.amazoncognito.com"

	// OAuth redirect URI registered by Kiro on Cognito's hosted UI client.
	// We CANNOT change this to our own host because it has to match the
	// configured Cognito callback list. Users complete the flow in a browser
	// and paste the resulting URL back into the admin console.
	DefaultRedirectURI = "https://app.kiro.dev/signin/oauth"

	// OAuth scope.
	Scopes = "email openid"

	// Kiro Web Portal service endpoint for RPC operations (CBOR Smithy protocol).
	KiroPortalBaseURL = "https://app.kiro.dev/service/KiroWebPortalService/operation"
)

// BuildAuthorizationURL builds the Cognito authorize URL that redirects to
// Google (identity_provider=Google). The caller is responsible for preserving
// state / codeVerifier server-side until the user returns with a code.
func BuildAuthorizationURL(state, codeChallenge string) string {
	params := url.Values{}
	params.Set("client_id", CognitoClientID)
	params.Set("response_type", "code")
	params.Set("scope", Scopes)
	params.Set("redirect_uri", DefaultRedirectURI)
	params.Set("state", state)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("identity_provider", "Google")
	return fmt.Sprintf("https://%s/oauth2/authorize?%s", CognitoDomain, params.Encode())
}

// GenerateRandomBytes reads n random bytes from crypto/rand.
func GenerateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

func base64URLEncode(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// GenerateState returns a random state token for CSRF protection.
func GenerateState() (string, error) {
	b, err := GenerateRandomBytes(32)
	if err != nil {
		return "", err
	}
	return base64URLEncode(b), nil
}

// GenerateSessionID returns a random opaque identifier used as map key for
// pending OAuth sessions.
func GenerateSessionID() (string, error) {
	b, err := GenerateRandomBytes(16)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenerateCodeVerifier returns an RFC 7636 PKCE code_verifier (43 chars).
func GenerateCodeVerifier() (string, error) {
	b, err := GenerateRandomBytes(32)
	if err != nil {
		return "", err
	}
	return base64URLEncode(b), nil
}

// GenerateCodeChallenge derives the S256 PKCE code_challenge from a verifier.
func GenerateCodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64URLEncode(sum[:])
}

// ---------------------------------------------------------------------------
// In-memory OAuth session store
// ---------------------------------------------------------------------------

// SessionStore keeps transient PKCE state keyed by sessionID.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*OAuthSession
	stopCh   chan struct{}
}

// NewSessionStore starts a background janitor to expire stale sessions.
func NewSessionStore() *SessionStore {
	s := &SessionStore{
		sessions: make(map[string]*OAuthSession),
		stopCh:   make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// Set stores a session keyed by id.
func (s *SessionStore) Set(id string, session *OAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = session
}

// Get returns a session if still valid (!expired).
func (s *SessionStore) Get(id string) (*OAuthSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	if time.Since(sess.CreatedAt) > SessionTTL {
		return nil, false
	}
	return sess, true
}

// Delete removes a session by id (no-op if absent).
func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// Stop terminates the background cleanup goroutine.
func (s *SessionStore) Stop() {
	select {
	case <-s.stopCh:
		return
	default:
		close(s.stopCh)
	}
}

func (s *SessionStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.mu.Lock()
			for id, sess := range s.sessions {
				if time.Since(sess.CreatedAt) > SessionTTL {
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// ExtractCodeAndState parses the callback URL (or any string containing
// ?code=XXX&state=YYY) that the user pastes back into the admin console.
// Accepts:
//   - full URL: https://app.kiro.dev/signin/oauth?code=xxx&state=yyy
//   - query-only string: ?code=xxx&state=yyy
//   - semicolon/whitespace-separated key=value pairs
func ExtractCodeAndState(raw string) (code, state string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("callback URL is empty")
	}

	// If it looks like a URL, parse the query string.
	if strings.Contains(raw, "?") {
		qIdx := strings.Index(raw, "?")
		qs := raw[qIdx+1:]
		if hashIdx := strings.Index(qs, "#"); hashIdx >= 0 {
			qs = qs[:hashIdx]
		}
		values, perr := url.ParseQuery(qs)
		if perr != nil {
			return "", "", fmt.Errorf("parse query: %w", perr)
		}
		code = values.Get("code")
		state = values.Get("state")
		if code == "" {
			return "", "", fmt.Errorf("no 'code' parameter in callback URL")
		}
		return code, state, nil
	}

	// Fallback: key=value pairs separated by & ; or whitespace
	norm := strings.NewReplacer(";", "&", "\n", "&", "\r", "&", "\t", "&", " ", "&").Replace(raw)
	values, perr := url.ParseQuery(norm)
	if perr != nil {
		return "", "", fmt.Errorf("parse key=value pairs: %w", perr)
	}
	code = values.Get("code")
	state = values.Get("state")
	if code == "" {
		return "", "", fmt.Errorf("no 'code' found in input")
	}
	return code, state, nil
}
