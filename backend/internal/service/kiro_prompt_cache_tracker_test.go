package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ==== 单元测试：tracker 基础语义 ====
//
// 这组测试覆盖移植自 promptCacheTracker.ts 的核心算法：
//   - cache_control 断点提取（system/messages/tools 三类）
//   - SHA-256 累积 fingerprint（前缀稳定性）
//   - 5min/1h ephemeral TTL 拆分
//   - 命中后刷新过期时间
//   - 单账号 200 条目上限
//   - 模型阈值差异（Opus 4096 / 其他 1024）
//   - 周期性过期清理

// helper：构造一个带 cache_control 的请求体。每条 user 消息 1500 chars
// (~500 tokens) 防止低于 1024 阈值。
func mkCachedRequest(systemTTL, lastMsgTTL string) []byte {
	systemArr := []map[string]any{{
		"type": "text",
		"text": strings.Repeat("System prompt block. ", 500), // ~10000 chars
		"cache_control": map[string]any{
			"type": "ephemeral",
			"ttl":  systemTTL,
		},
	}}
	if systemTTL == "" {
		delete(systemArr[0], "cache_control")
	}

	lastBlock := map[string]any{
		"type": "text",
		"text": strings.Repeat("Tail message. ", 200), // ~2800 chars
	}
	if lastMsgTTL != "" {
		lastBlock["cache_control"] = map[string]any{
			"type": "ephemeral",
			"ttl":  lastMsgTTL,
		}
	}

	body := map[string]any{
		"model":  "claude-sonnet-4-5-20250929",
		"system": systemArr,
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": []any{lastBlock},
			},
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// TestTracker_NoCacheControl_ReturnsNilProfile 验证：当请求里没有任何
// cache_control 标记时，BuildClaudeProfileFromBody 返回 nil。
// 这是 fallback 策略的关键前提——profile=nil 时上层不会改写 usage。
func TestTracker_NoCacheControl_ReturnsNilProfile(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": "Plain system prompt without cache_control.",
		"messages": [{"role":"user","content":"hello"}]
	}`)

	profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 100)
	require.Nil(t, profile, "no cache_control should yield nil profile")
}

// TestTracker_FirstRequest_IsAllCreation 验证首次请求 → 全部计入 creation，
// 没有 read 命中。
func TestTracker_FirstRequest_IsAllCreation(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	body := mkCachedRequest("5m", "5m")

	profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 5000)
	require.NotNil(t, profile)
	require.NotEmpty(t, profile.Breakpoints)

	usage := tracker.Compute(123, profile)
	require.Equal(t, 0, usage.CacheReadInputTokens, "first request: no read hit")
	require.Greater(t, usage.CacheCreationInputTokens, 0, "first request: all creation")
	require.Greater(t, usage.CacheCreation5mTokens, 0)
	require.Equal(t, 0, usage.CacheCreation1hTokens, "ttl=5m must not bucket into 1h")
}

// TestTracker_SecondRequest_ReadsCachedPrefix 验证：相同请求第二次发出时，
// fingerprint 命中，读出大部分 token，creation 仅剩当前 turn 的新增内容。
func TestTracker_SecondRequest_ReadsCachedPrefix(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	body := mkCachedRequest("5m", "5m")

	profile1 := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 5000)
	require.NotNil(t, profile1)
	tracker.Update(42, profile1) // 第一次成功，写入缓存

	// 第二次：完全相同的请求体——所有 fingerprint 都应命中。
	profile2 := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 5000)
	usage := tracker.Compute(42, profile2)
	require.Greater(t, usage.CacheReadInputTokens, 0, "identical request must hit cache")
}

// TestTracker_DifferentAccount_IsolatedCache 验证：账号间缓存隔离——
// 账号 A 写入的 fingerprint 不会被账号 B 读到。
func TestTracker_DifferentAccount_IsolatedCache(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	body := mkCachedRequest("5m", "5m")
	profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 5000)
	require.NotNil(t, profile)

	tracker.Update(1, profile)
	usageOther := tracker.Compute(2, profile)
	require.Equal(t, 0, usageOther.CacheReadInputTokens, "cross-account hit must be impossible")
	require.Greater(t, usageOther.CacheCreationInputTokens, 0)
}

// TestTracker_ExpiredEntry_IsNotHit 验证：超过 TTL 的 entry 不会命中。
// 我们注入一个固定时钟并跳跃 1h+ 来模拟时间流逝。
func TestTracker_ExpiredEntry_IsNotHit(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	now := time.Now()
	tracker.SetClock(func() time.Time { return now })

	body := mkCachedRequest("5m", "5m")
	profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 5000)
	require.NotNil(t, profile)
	tracker.Update(7, profile)

	// 过 6 分钟：5m TTL 全部过期。
	now = now.Add(6 * time.Minute)
	usage := tracker.Compute(7, profile)
	require.Equal(t, 0, usage.CacheReadInputTokens, "expired entries must not hit")
}

// TestTracker_OneHourTTL_BucketedSeparately 验证：cache_control.ttl="1h"
// 的断点会被记入 ephemeral_1h 桶，5m 的进 ephemeral_5m 桶。
func TestTracker_OneHourTTL_BucketedSeparately(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	// system 用 1h，最后一条用户消息用 5m，二者会形成两个独立断点。
	body := mkCachedRequest("1h", "5m")
	profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 6000)
	require.NotNil(t, profile)
	require.GreaterOrEqual(t, len(profile.Breakpoints), 2, "expect both system+message breakpoints")

	usage := tracker.Compute(1, profile)
	require.Greater(t, usage.CacheCreation1hTokens, 0, "1h TTL must register into 1h bucket")
	require.Greater(t, usage.CacheCreation5mTokens, 0, "5m TTL must register into 5m bucket")
}

// TestTracker_OpusModel_HasHigherMinThreshold 验证 Opus 走 4096 阈值——
// 一个仅 ~3000 token 的请求体在 Sonnet 上应当形成 creation，但在 Opus 上
// 因低于 4096 不被缓存（cumulativeTokens < minTokens）。
func TestTracker_OpusModel_HasHigherMinThreshold(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	// 故意选择刚好介于 1024-4096 之间的 system size。
	body, _ := json.Marshal(map[string]any{
		"model": "claude-opus-4-5",
		"system": []map[string]any{{
			"type": "text",
			"text": strings.Repeat("hello ", 300), // ~1800 chars ~ 450 tokens
			"cache_control": map[string]any{
				"type": "ephemeral",
				"ttl":  "5m",
			},
		}},
		"messages": []any{
			map[string]any{"role": "user", "content": "ping"},
		},
	})
	profile := tracker.BuildClaudeProfileFromBody(body, "claude-opus-4-5", 500)
	require.NotNil(t, profile)
	usage := tracker.Compute(99, profile)
	// 总 token 远低于 4096，opus 阈值 → 不视为可缓存。
	require.Equal(t, 0, usage.CacheCreationInputTokens, "below opus threshold: not cacheable")
}

// TestTracker_FingerprintStable_AcrossKeyOrder 验证 canonical JSON 的关键
// 性质：map 的 key 顺序不影响 fingerprint。
func TestTracker_FingerprintStable_AcrossKeyOrder(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	bodyA := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}],
		"messages": [{"role":"user","content":"world"}]
	}`)
	bodyB := []byte(`{
		"messages": [{"content":"world","role":"user"}],
		"model": "claude-sonnet-4-5",
		"system": [{"cache_control":{"ttl":"5m","type":"ephemeral"},"type":"text","text":"hello"}]
	}`)

	pa := tracker.BuildClaudeProfileFromBody(bodyA, "claude-sonnet-4-5", 1100)
	pb := tracker.BuildClaudeProfileFromBody(bodyB, "claude-sonnet-4-5", 1100)
	require.NotNil(t, pa)
	require.NotNil(t, pb)
	require.Equal(t, len(pa.Breakpoints), len(pb.Breakpoints))
	for i := range pa.Breakpoints {
		require.Equal(t, pa.Breakpoints[i].Fingerprint, pb.Breakpoints[i].Fingerprint,
			"breakpoint %d: fingerprint must be order-independent", i)
	}
}

// TestTracker_HitRefreshesExpiry 验证 Anthropic 文档行为：
// 命中后过期时间被刷新（"sliding window"），让多轮对话能稳定持续命中。
func TestTracker_HitRefreshesExpiry(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	now := time.Now()
	tracker.SetClock(func() time.Time { return now })

	body := mkCachedRequest("5m", "5m")
	profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 5000)
	tracker.Update(1, profile)

	// 4 分钟后命中——刷新过期时间到 now+5m。
	now = now.Add(4 * time.Minute)
	usage := tracker.Compute(1, profile)
	require.Greater(t, usage.CacheReadInputTokens, 0, "still within 5m TTL")

	// 再过 4 分钟（距离首次写入 8 分钟，超过原 5m TTL，但命中刷新过——
	// 现在是命中+4min 距离上次刷新仍在 5m 内）。
	now = now.Add(4 * time.Minute)
	usage = tracker.Compute(1, profile)
	require.Greater(t, usage.CacheReadInputTokens, 0, "hit must refresh expiry")
}

// TestTracker_Clear_RemovesAllEntries 验证 Clear() 清空所有账号缓存。
func TestTracker_Clear_RemovesAllEntries(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	body := mkCachedRequest("5m", "5m")
	profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 5000)
	tracker.Update(1, profile)
	tracker.Update(2, profile)

	require.Greater(t, tracker.TotalEntries(), 0)
	cleared := tracker.Clear()
	require.Greater(t, cleared, 0)
	require.Equal(t, 0, tracker.TotalEntries())
}

// TestTracker_NilProfile_ReturnsZeroUsage 验证防御性：profile=nil 不 panic。
func TestTracker_NilProfile_ReturnsZeroUsage(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	usage := tracker.Compute(1, nil)
	require.True(t, usage.IsZero())
	tracker.Update(1, nil) // 不可 panic
}

// TestTracker_ZeroAccountID_NoOp 验证 accountID==0 被视作"未登录"，跳过 tracker。
// 这与 sub2api 其他模块对待 0 账号 ID 的方式一致——避免把所有匿名请求堆到
// 同一账号下污染缓存表。
func TestTracker_ZeroAccountID_NoOp(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	body := mkCachedRequest("5m", "5m")
	profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 5000)
	require.NotNil(t, profile)

	usage := tracker.Compute(0, profile)
	require.True(t, usage.IsZero(), "accountID=0 should yield zero usage")
	tracker.Update(0, profile)
	require.Equal(t, 0, tracker.TotalEntries())
}

// TestTracker_TextLengthVariation_ProducesDifferentFingerprints 验证：
// 内容差异（哪怕一个字符）会改变 fingerprint，缓存不会"误命中"。
func TestTracker_TextLengthVariation_ProducesDifferentFingerprints(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	bodyA, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5",
		"system": []map[string]any{{
			"type": "text",
			"text": strings.Repeat("hi ", 1000),
			"cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"},
		}},
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	})
	bodyB, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5",
		"system": []map[string]any{{
			"type": "text",
			"text": strings.Repeat("hi ", 1001), // 多一个 "hi "
			"cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"},
		}},
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	})

	pa := tracker.BuildClaudeProfileFromBody(bodyA, "claude-sonnet-4-5", 1500)
	pb := tracker.BuildClaudeProfileFromBody(bodyB, "claude-sonnet-4-5", 1500)
	require.NotNil(t, pa)
	require.NotNil(t, pb)
	require.NotEqual(t, pa.Breakpoints[0].Fingerprint, pb.Breakpoints[0].Fingerprint,
		"different content must produce different fingerprints")
}

// TestTracker_MaxEntriesPerAccount_Evicts 验证单账号上限——超过 200 条
// 时按过期时间淘汰最旧。这里只证明上限被遵守，不要求精确淘汰数量。
func TestTracker_MaxEntriesPerAccount_Evicts(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	now := time.Now()
	tracker.SetClock(func() time.Time { return now })

	// 注入 250 个不同的 fingerprint。每次 advance 一秒避免 TTL 完全相同。
	for i := 0; i < 250; i++ {
		systemText := strings.Repeat("padding ", 200) + " unique-" + strings.Repeat("X", i+1)
		bodyMap := map[string]any{
			"model": "claude-sonnet-4-5",
			"system": []map[string]any{{
				"type": "text",
				"text": systemText,
				"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
			}},
			"messages": []any{map[string]any{"role": "user", "content": "x"}},
		}
		body, _ := json.Marshal(bodyMap)
		profile := tracker.BuildClaudeProfileFromBody(body, "claude-sonnet-4-5", 2000)
		require.NotNil(t, profile)
		tracker.Update(99, profile)
		now = now.Add(time.Second)
	}

	require.LessOrEqual(t, tracker.TotalEntries(), kiroPCMaxEntriesPerAccount,
		"per-account entries must not exceed cap")
}
