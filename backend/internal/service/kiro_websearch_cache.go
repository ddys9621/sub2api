package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/redis/go-redis/v9"
)

// kiroWebSearchCacheTTL bounds how long cached MCP results stay fresh.
// 15 minutes is a good balance between "the web has moved on" and
// "don't re-spend credits for the same query in the same session".
const kiroWebSearchCacheTTL = 15 * time.Minute

// kiroWebSearchCacheKeyPrefix namespaces our keys alongside the existing
// kiro:* keys already used by the token cache.
const kiroWebSearchCacheKeyPrefix = "kiro:mcp:ws:"

// kiroMCPResultCache is a small Redis-backed cache for Kiro /mcp
// web_search responses. It is a thin wrapper rather than a new repository
// type because the cached value is simple JSON and every call-site is
// this one interceptor.
type kiroMCPResultCache struct {
	rdb *redis.Client
}

// newKiroMCPResultCache returns a cache backed by the given Redis client.
// When rdb is nil the cache is a no-op so calling code doesn't need to
// special-case the "no Redis" test environment.
func newKiroMCPResultCache(rdb *redis.Client) *kiroMCPResultCache {
	return &kiroMCPResultCache{rdb: rdb}
}

// Key derives a Redis key from an account identifier + the normalised
// query. Account scoping means accounts can't leak search results across
// tenants; query normalisation (lowercase + trim) deduplicates trivially
// different spellings.
func kiroMCPResultCacheKey(accountID int64, query string) string {
	norm := strings.ToLower(strings.TrimSpace(query))
	h := sha256.Sum256([]byte(norm))
	return fmt.Sprintf("%s%d:%s", kiroWebSearchCacheKeyPrefix, accountID, hex.EncodeToString(h[:16]))
}

// Get returns the cached MCP response for the given (account, query), or
// nil if the cache is empty / disabled / unavailable. Errors are not
// propagated — a cache miss should never fail the user-visible request.
func (c *kiroMCPResultCache) Get(ctx context.Context, accountID int64, query string) *kiro.MCPWebSearchResponse {
	if c == nil || c.rdb == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	key := kiroMCPResultCacheKey(accountID, query)
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil || len(raw) == 0 {
		return nil
	}
	var resp kiro.MCPWebSearchResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return nil
	}
	return &resp
}

// Set stores the response for the standard TTL. Failures are silent.
func (c *kiroMCPResultCache) Set(ctx context.Context, accountID int64, query string, resp *kiro.MCPWebSearchResponse) {
	if c == nil || c.rdb == nil || resp == nil || strings.TrimSpace(query) == "" {
		return
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	key := kiroMCPResultCacheKey(accountID, query)
	_ = c.rdb.Set(ctx, key, raw, kiroWebSearchCacheTTL).Err()
}
