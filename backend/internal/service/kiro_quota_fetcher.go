package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

// KiroQuotaSnapshot captures the fields surfaced to the admin UI and the
// account_stats pipeline after a successful GetUserUsageAndLimits call.
type KiroQuotaSnapshot struct {
	// SubscriptionType is free / pro / enterprise, as reported by Kiro.
	SubscriptionType string
	// VibeUsed/Limit and SpecUsed/Limit are the two request-counter pairs
	// that Kiro's portal UI shows ("Vibe requests" and "Spec requests").
	VibeUsed  int64
	VibeLimit int64
	SpecUsed  int64
	SpecLimit int64
	// Raw is the complete decoded CBOR map for forward-compatibility. The
	// admin handler serialises this so the UI can render any new fields
	// Kiro introduces without a backend change.
	Raw map[string]any
}

// KiroQuotaFetcher fetches live usage+limits for a Kiro account via the
// /GetUserUsageAndLimits CBOR RPC. It reuses the KiroOAuthService client
// construction path, and expects the caller to pass a fresh access token
// (resolved via KiroTokenProvider).
type KiroQuotaFetcher struct {
	proxyRepo     ProxyRepository
	tokenProvider *KiroTokenProvider
}

// NewKiroQuotaFetcher builds the fetcher.
func NewKiroQuotaFetcher(proxyRepo ProxyRepository, tokenProvider *KiroTokenProvider) *KiroQuotaFetcher {
	return &KiroQuotaFetcher{proxyRepo: proxyRepo, tokenProvider: tokenProvider}
}

// CanFetch gates the fetcher: only OAuth Kiro accounts with a persisted
// access_token are eligible.
func (f *KiroQuotaFetcher) CanFetch(account *Account) bool {
	if account == nil {
		return false
	}
	if account.Platform != PlatformKiro || account.Type != AccountTypeOAuth {
		return false
	}
	return account.GetCredential("access_token") != ""
}

// FetchQuota calls GetUserUsageAndLimits for the given account and returns a
// snapshot suitable for storage or UI rendering. Any HTTP/CBOR failure is
// returned verbatim; the caller decides whether to retry or degrade.
func (f *KiroQuotaFetcher) FetchQuota(ctx context.Context, account *Account) (*KiroQuotaSnapshot, error) {
	if !f.CanFetch(account) {
		return nil, errors.New("kiro quota: account not eligible")
	}

	// Resolve a fresh token via the provider so an expired access_token
	// triggers a refresh instead of a 401.
	token := account.GetCredential("access_token")
	if f.tokenProvider != nil {
		if t, err := f.tokenProvider.GetAccessToken(ctx, account); err == nil && t != "" {
			token = t
		}
	}

	var proxyURL string
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	client, err := kiro.NewClient(kiro.WithProxyURL(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("kiro quota: build client: %w", err)
	}
	defer client.Close()

	// GetUserUsageAndLimits requires CSRF; ensure the portal HTML can be
	// scraped by seeding the jar with the current access token.
	if seedToken := account.GetCredential("access_token"); seedToken != "" {
		client.SeedAccessToken(seedToken)
	}
	if csrf := account.GetCredential("csrf_token"); csrf != "" {
		client.SetCSRFToken(csrf)
	}

	usage, err := client.GetUserUsageAndLimits(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("kiro quota: rpc: %w", err)
	}

	return &KiroQuotaSnapshot{
		SubscriptionType: usage.SubscriptionType,
		VibeUsed:         usage.VibeRequestsUsed,
		VibeLimit:        usage.VibeRequestsLimit,
		SpecUsed:         usage.SpecRequestsUsed,
		SpecLimit:        usage.SpecRequestsLimit,
		Raw:              usage.Raw,
	}, nil
}
