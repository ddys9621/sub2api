package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func TestKiroMCPResultCache_NilSafe(t *testing.T) {
	var c *kiroMCPResultCache
	require.Nil(t, c.Get(context.Background(), 1, "q"))
	c.Set(context.Background(), 1, "q", &kiro.MCPWebSearchResponse{}) // must not panic
}

func TestKiroMCPResultCache_NilRedisClient(t *testing.T) {
	c := newKiroMCPResultCache(nil)
	require.Nil(t, c.Get(context.Background(), 1, "query"))
	c.Set(context.Background(), 1, "query", &kiro.MCPWebSearchResponse{}) // must not panic
}

func TestKiroMCPResultCacheKey_StableAndAccountScoped(t *testing.T) {
	k1 := kiroMCPResultCacheKey(42, "today news")
	k2 := kiroMCPResultCacheKey(42, "Today News")
	k3 := kiroMCPResultCacheKey(43, "today news")
	require.Equal(t, k1, k2, "key must normalise case + whitespace")
	require.NotEqual(t, k1, k3, "different accounts must get different keys")
	require.Contains(t, k1, "kiro:mcp:ws:")
}

func TestKiroMCPResultCacheKey_EmptyQueryStillDeterministic(t *testing.T) {
	// Empty query never reaches the cache in practice (Get/Set guard on
	// it) but the key derivation should still be deterministic.
	require.Equal(t, kiroMCPResultCacheKey(1, ""), kiroMCPResultCacheKey(1, "   "))
}
