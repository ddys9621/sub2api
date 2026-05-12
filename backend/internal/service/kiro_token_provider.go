package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kiro token lifetime in production is 1 hour. We refresh 3 minutes before
// expiry on the hot path, and the background scheduler uses 30 minutes.
const (
	kiroTokenRefreshSkew = 3 * time.Minute
	kiroTokenCacheSkew   = 5 * time.Minute
	// kiroRequestRefreshTimeout bounds how long the request path is willing
	// to block on a token refresh. Past this point we surface the error,
	// park the account temporarily, and let the background refresher retry.
	kiroRequestRefreshTimeout = 10 * time.Second
)

// KiroTokenCache reuses the shared Gemini-style cache interface.
type KiroTokenCache = GeminiTokenCache

// KiroTokenProvider fetches a valid Kiro accessToken for a given account,
// refreshing through the CBOR RefreshToken RPC when necessary. It plugs into
// the project's shared OAuthRefreshAPI / RefreshExecutor infrastructure so
// that background refreshes and hot-path refreshes do not race.
type KiroTokenProvider struct {
	accountRepo      AccountRepository
	tokenCache       KiroTokenCache
	kiroOAuthService *KiroOAuthService
	refreshAPI       *OAuthRefreshAPI
	executor         OAuthRefreshExecutor
	refreshPolicy    ProviderRefreshPolicy
	tempUnschedCache TempUnschedCache
	// accountProbe lets the provider confirm an account is actually dead
	// before stalling it. Refreshes fail for transient reasons all the
	// time (network blip, AWS 5xx, context deadline) — parking the
	// account on every such blip produces the user-reported "no
	// available accounts" even though a manual test still works. When
	// set, markTempUnschedulable runs a live probe first and only
	// parks the account if the probe also fails.
	accountProbe     KiroAccountProbe
	backfillCooldown sync.Map
}

// KiroAccountProbe verifies an account is actually usable at the moment.
// nil error means the account can serve requests right now.
type KiroAccountProbe interface {
	ProbeAccount(ctx context.Context, account *Account) error
}

// NewKiroTokenProvider constructs the provider with sensible defaults.
func NewKiroTokenProvider(
	accountRepo AccountRepository,
	tokenCache KiroTokenCache,
	kiroOAuthService *KiroOAuthService,
) *KiroTokenProvider {
	return &KiroTokenProvider{
		accountRepo:      accountRepo,
		tokenCache:       tokenCache,
		kiroOAuthService: kiroOAuthService,
		refreshPolicy:    KiroProviderRefreshPolicy(),
	}
}

// SetRefreshAPI wires the unified refresh coordinator + executor.
func (p *KiroTokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

// SetRefreshPolicy overrides the caller-side policy (for tests).
func (p *KiroTokenProvider) SetRefreshPolicy(policy ProviderRefreshPolicy) {
	p.refreshPolicy = policy
}

// SetTempUnschedCache lets hot-path failures immediately remove the account
// from scheduling consideration, without waiting for the next DB sweep.
func (p *KiroTokenProvider) SetTempUnschedCache(cache TempUnschedCache) {
	p.tempUnschedCache = cache
}

// SetAccountProbe wires a live-probe implementation. When set, the
// provider will verify a seemingly-dead account is actually dead before
// flipping it to temp-unschedulable — refreshes fail for transient
// reasons (network blip, AWS 5xx) all the time, and parking the
// account on every blip leads to "no available accounts" errors even
// when the account is usable.
func (p *KiroTokenProvider) SetAccountProbe(probe KiroAccountProbe) {
	p.accountProbe = probe
}

// GetAccessToken returns a Kiro accessToken that is valid for at least
// kiroTokenRefreshSkew more seconds. Blocks if a refresh is required.
func (p *KiroTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformKiro {
		return "", errors.New("not a kiro account")
	}
	if account.Type != AccountTypeOAuth {
		return "", errors.New("not a kiro oauth account")
	}

	cacheKey := KiroTokenCacheKey(account)

	// Fast path: cache.
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
	}

	// Refresh path: trigger only when about to expire.
	expiresAt := account.GetCredentialAsTime("expires_at")
	needsRefresh := expiresAt == nil || time.Until(*expiresAt) <= kiroTokenRefreshSkew
	if needsRefresh && p.refreshAPI != nil && p.executor != nil {
		refreshCtx, cancel := context.WithTimeout(ctx, kiroRequestRefreshTimeout)
		defer cancel()
		result, err := p.refreshAPI.RefreshIfNeeded(refreshCtx, account, p.executor, kiroTokenRefreshSkew)
		if err != nil {
			p.markTempUnschedulable(account, err)
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
		} else if result.LockHeld {
			if p.refreshPolicy.OnLockHeld == ProviderLockHeldWaitForCache && p.tokenCache != nil {
				if token, cacheErr := p.tokenCache.GetAccessToken(ctx, cacheKey); cacheErr == nil && strings.TrimSpace(token) != "" {
					return token, nil
				}
			}
		} else {
			account = result.Account
			expiresAt = account.GetCredentialAsTime("expires_at")
		}
	} else if needsRefresh && p.tokenCache != nil {
		locked, err := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, 30*time.Second)
		if err == nil && locked {
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		}
	}

	accessToken := account.GetCredential("access_token")
	if strings.TrimSpace(accessToken) == "" {
		return "", errors.New("access_token not found in credentials")
	}

	// Cache for most of the remaining lifetime (leaving a safety skew).
	if p.tokenCache != nil {
		latestAccount, isStale := CheckTokenVersion(ctx, account, p.accountRepo)
		if isStale && latestAccount != nil {
			slog.Debug("kiro_token_version_stale_use_latest", "account_id", account.ID)
			accessToken = latestAccount.GetCredential("access_token")
			if strings.TrimSpace(accessToken) == "" {
				return "", errors.New("access_token not found after version check")
			}
		} else {
			ttl := 30 * time.Minute
			if expiresAt != nil {
				until := time.Until(*expiresAt)
				switch {
				case until > kiroTokenCacheSkew:
					ttl = until - kiroTokenCacheSkew
				case until > 0:
					ttl = until
				default:
					ttl = time.Minute
				}
			}
			_ = p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl)
		}
	}

	return accessToken, nil
}

// Public Kiro IDE profile ARNs. Kiro IDE itself ships these constants
// hard-coded into the desktop binary (see Kiro-account-manager's
// resolveProfileArn() in src/main/proxy/kiroApi.ts) and uses them whenever
// the operator's account does not have a per-tenant profileArn of its own.
//
// They are NOT secrets — they identify Amazon's own Q Developer profiles
// to which every Builder ID / social-login user is implicitly enrolled.
// Without one of these, /generateAssistantResponse returns
//
//	HTTP 400 {"message":"profileArn is required for this request."}
//
// because the upstream switched to mandatory-profileArn semantics on the
// current AWS build.
const (
	// kiroBuilderIDPublicProfileArn is the catch-all profile for AWS Builder
	// ID accounts (the default identity provider used by every fresh Kiro
	// signup that does not go through Identity Center / Enterprise SSO).
	kiroBuilderIDPublicProfileArn = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"

	// kiroSocialPublicProfileArn covers Google / GitHub social-login
	// accounts which Kiro routes through a separate AWS-owned tenant.
	kiroSocialPublicProfileArn = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"
)

// ProfileArn returns the profileArn that should be sent on
// /generateAssistantResponse for this account.
//
// Resolution order, mirroring Kiro IDE's own resolveProfileArn():
//
//  1. credentials.profile_arn — present only for IDC / enterprise SSO
//     accounts where each tenant has its own profile.
//  2. Public social-login fallback — when credentials.provider is one of
//     Google / GitHub or auth_method == social.
//  3. Public Builder ID fallback — for everything else (Builder ID is the
//     dominant case).
//
// Returning a non-empty string for every reasonable account means
// BuildKiroPayload always emits the field, eliminating the historical
// 400 "profileArn is required" failures for accounts whose credentials
// row was captured before AWS started enforcing the field.
func (p *KiroTokenProvider) ProfileArn(account *Account) string {
	if account == nil {
		return ""
	}
	if explicit := strings.TrimSpace(account.GetCredential("profile_arn")); explicit != "" {
		return explicit
	}

	provider := strings.ToLower(strings.TrimSpace(account.GetCredential("provider")))
	authMethod := strings.ToLower(strings.TrimSpace(account.GetCredential("auth_method")))
	switch {
	case provider == "google", provider == "github", authMethod == "social":
		return kiroSocialPublicProfileArn
	default:
		// Builder ID is by far the most common path; fall back to the
		// public catch-all so even accounts imported with an empty
		// profile_arn keep working.
		return kiroBuilderIDPublicProfileArn
	}
}

// markTempUnschedulable parks the account when hot-path refresh fails so the
// scheduler skips it until the background service recovers.
//
// Before actually parking, we run a live probe (if configured): lots of
// refresh errors are transient (network blip, AWS 5xx bursts, context
// deadline under load) and the account is still perfectly usable. If
// the probe succeeds, we log and skip the park entirely — the scheduler
// keeps the account in the pool and the next request just works.
func (p *KiroTokenProvider) markTempUnschedulable(account *Account, refreshErr error) {
	if p.accountRepo == nil || account == nil {
		return
	}
	if p.accountProbe != nil {
		probeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := p.accountProbe.ProbeAccount(probeCtx, account); err == nil {
			slog.Info("kiro_token_provider.probe_ok_skip_temp_unsched",
				"account_id", account.ID,
				"refresh_error", refreshErr.Error(),
			)
			return
		} else {
			slog.Warn("kiro_token_provider.probe_failed_proceed_temp_unsched",
				"account_id", account.ID,
				"refresh_error", refreshErr.Error(),
				"probe_error", err.Error(),
			)
		}
	}
	now := time.Now()
	until := now.Add(tokenRefreshTempUnschedDuration)
	reason := "kiro token refresh failed on request path: " + refreshErr.Error()
	bgCtx := context.Background()
	if err := p.accountRepo.SetTempUnschedulable(bgCtx, account.ID, until, reason); err != nil {
		slog.Warn("kiro_token_provider.set_temp_unschedulable_failed",
			"account_id", account.ID,
			"error", err,
		)
		return
	}
	slog.Warn("kiro_token_provider.temp_unschedulable_set",
		"account_id", account.ID,
		"until", until.Format(time.RFC3339),
		"reason", reason,
	)
	if p.tempUnschedCache != nil {
		state := &TempUnschedState{
			UntilUnix:       until.Unix(),
			TriggeredAtUnix: now.Unix(),
			ErrorMessage:    reason,
		}
		if err := p.tempUnschedCache.SetTempUnsched(bgCtx, account.ID, state); err != nil {
			slog.Debug("kiro_token_provider.temp_unsched_cache_set_failed", "account_id", account.ID, "error", err)
		}
	}
}

// KiroTokenCacheKey builds the Redis cache key for the account token.
func KiroTokenCacheKey(account *Account) string {
	if account == nil {
		return "kiro:token:0"
	}
	return "kiro:token:" + strconv.FormatInt(account.ID, 10)
}
