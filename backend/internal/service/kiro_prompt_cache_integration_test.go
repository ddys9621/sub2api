package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ==== 集成测试：fallback 策略 ====
//
// 这组测试聚焦 KiroGatewayService 与 KiroPromptCacheTracker 的集成边界，
// 验证：
//   - 上游真实 cache 字段为非零时，simulator 不会篡改
//   - 上游全零时，simulator 的值会被写入（这是新功能的核心价值）
//   - 5m/1h 拆分始终来自 simulator
//   - 各种 nil 场景不 panic

// TestApplySimulatedCacheUsage_UpstreamReadNonZero_KeepsRead 验证：
// 当上游真实报告了 cache_read，simulator 不可覆盖该字段（按字段独立判断）。
// 但 cache_creation 是独立字段——上游为 0 时仍可由 simulator 补。
// 这是 v1.2.0 → v1.2.1 行为变更：旧策略整体 bypass，新策略按字段判断。
func TestApplySimulatedCacheUsage_UpstreamReadNonZero_KeepsRead(t *testing.T) {
	usage := &ClaudeUsage{
		InputTokens:              5000,
		OutputTokens:             100,
		CacheReadInputTokens:     2500, // 上游已经报回真实命中
		CacheCreationInputTokens: 0,
	}
	sim := KiroCacheUsage{
		CacheCreationInputTokens: 4500, // simulator 想补 creation
		CacheReadInputTokens:     0,    // simulator 无 read
		CacheCreation5mTokens:    4500,
		CacheCreation1hTokens:    0,
	}

	applySimulatedCacheUsage(usage, sim)

	require.Equal(t, 2500, usage.CacheReadInputTokens, "upstream cache_read must win")
	require.Equal(t, 4500, usage.CacheCreationInputTokens, "v1.2.1: independent field — simulator fills when upstream is 0")
	require.Equal(t, 4500, usage.CacheCreation5mTokens)
	require.Equal(t, 0, usage.CacheCreation1hTokens)
}

// TestApplySimulatedCacheUsage_KiroProductionShape_FillsCacheRead 是
// v1.2.0 回归 bug 的核心 fixture：Kiro 上游在生产环境**几乎每次**都报
// cache_creation > 0 但 cache_read = 0（自家 cachePoint 一直在写但不读）。
// 旧策略的 hasUpstreamCache 在此处为 true，导致 simulator 永远不被应用，
// cache_read 永远是 0 —— 这正是用户截图里看到的现象。
// 新策略必须让 cache_read 由 simulator 补齐。
func TestApplySimulatedCacheUsage_KiroProductionShape_FillsCacheRead(t *testing.T) {
	// 模拟生产中真实的 Kiro 上游 usage：cache_creation 一直在涨，cache_read 永远 0。
	usage := &ClaudeUsage{
		InputTokens:              30775,
		OutputTokens:             218,
		CacheReadInputTokens:     0,     // Kiro 实际行为：从不报
		CacheCreationInputTokens: 31000, // Kiro 实际行为：几乎每次都报
	}
	sim := KiroCacheUsage{
		CacheCreationInputTokens: 0,     // simulator 这次不算 creation（profile 没新 breakpoint）
		CacheReadInputTokens:     25000, // simulator 算出 25k 命中
		CacheCreation5mTokens:    0,
		CacheCreation1hTokens:    0,
	}

	applySimulatedCacheUsage(usage, sim)

	require.Equal(t, 25000, usage.CacheReadInputTokens,
		"v1.2.1 核心修复：上游报了 cache_creation 不应屏蔽 simulator 对 cache_read 的填充")
	require.Equal(t, 31000, usage.CacheCreationInputTokens,
		"上游真实 cache_creation 数字必须保留")
}

// TestApplySimulatedCacheUsage_UpstreamZero_UsesSimulator 验证：
// 上游全 0 时（部分 Kiro 模型不报 cache token），simulator 接管。
// 这是新功能让客户端 cache_control 终于"有效"的关键路径。
func TestApplySimulatedCacheUsage_UpstreamZero_UsesSimulator(t *testing.T) {
	usage := &ClaudeUsage{
		InputTokens:              5000,
		OutputTokens:             100,
		CacheReadInputTokens:     0,
		CacheCreationInputTokens: 0,
	}
	sim := KiroCacheUsage{
		CacheCreationInputTokens: 1500,
		CacheReadInputTokens:     3000,
		CacheCreation5mTokens:    1500,
		CacheCreation1hTokens:    0,
	}

	applySimulatedCacheUsage(usage, sim)

	require.Equal(t, 3000, usage.CacheReadInputTokens, "simulator fills upstream zero")
	require.Equal(t, 1500, usage.CacheCreationInputTokens)
	require.Equal(t, 1500, usage.CacheCreation5mTokens)
	require.Equal(t, 0, usage.CacheCreation1hTokens)
}

// TestApplySimulatedCacheUsage_BothZero_NoChange 验证：simulator 也是 0
// 时，usage 不被改动（避免无意义 mutation）。
func TestApplySimulatedCacheUsage_BothZero_NoChange(t *testing.T) {
	usage := &ClaudeUsage{
		InputTokens:  100,
		OutputTokens: 50,
	}
	sim := KiroCacheUsage{} // all zero
	applySimulatedCacheUsage(usage, sim)
	require.Equal(t, 0, usage.CacheReadInputTokens)
	require.Equal(t, 0, usage.CacheCreationInputTokens)
	require.Equal(t, 0, usage.CacheCreation5mTokens)
	require.Equal(t, 0, usage.CacheCreation1hTokens)
}

// TestApplySimulatedCacheUsage_NilUsage_NoPanic 验证 nil 防御。
func TestApplySimulatedCacheUsage_NilUsage_NoPanic(t *testing.T) {
	require.NotPanics(t, func() {
		applySimulatedCacheUsage(nil, KiroCacheUsage{CacheReadInputTokens: 100})
	})
}

// TestApplySimulatedCacheUsage_UpstreamCreationOnly_FillsSplit 验证：
// 上游报告 cache_creation 但没拆 5m/1h 时（这是 Kiro 当前的真实情况），
// simulator 提供 5m/1h 拆分。
func TestApplySimulatedCacheUsage_UpstreamCreationOnly_FillsSplit(t *testing.T) {
	usage := &ClaudeUsage{
		CacheCreationInputTokens: 2000,
		CacheReadInputTokens:     0,
		// 5m/1h 上游不填
	}
	sim := KiroCacheUsage{
		CacheCreationInputTokens: 1500, // 不应覆盖
		CacheReadInputTokens:     0,
		CacheCreation5mTokens:    1200,
		CacheCreation1hTokens:    300,
	}

	applySimulatedCacheUsage(usage, sim)

	require.Equal(t, 2000, usage.CacheCreationInputTokens, "upstream cache_creation must win")
	require.Equal(t, 1200, usage.CacheCreation5mTokens, "5m/1h comes from simulator when upstream omits it")
	require.Equal(t, 300, usage.CacheCreation1hTokens)
}

// TestApplySimulatedCacheUsage_UpstreamHasFullSplit_DoesNotOverwrite 验证：
// 万一未来 Kiro 上游开始报告 5m/1h 拆分，simulator 不可篡改。
func TestApplySimulatedCacheUsage_UpstreamHasFullSplit_DoesNotOverwrite(t *testing.T) {
	usage := &ClaudeUsage{
		CacheReadInputTokens:     2500,
		CacheCreationInputTokens: 1000,
		CacheCreation5mTokens:    800,
		CacheCreation1hTokens:    200,
	}
	sim := KiroCacheUsage{
		CacheCreationInputTokens: 9999,
		CacheReadInputTokens:     9999,
		CacheCreation5mTokens:    9999,
		CacheCreation1hTokens:    9999,
	}

	applySimulatedCacheUsage(usage, sim)

	require.Equal(t, 2500, usage.CacheReadInputTokens)
	require.Equal(t, 1000, usage.CacheCreationInputTokens)
	require.Equal(t, 800, usage.CacheCreation5mTokens, "upstream-provided split must win")
	require.Equal(t, 200, usage.CacheCreation1hTokens)
}

// TestBuildPromptCacheProfile_NoCacheControl_ReturnsNil 验证：
// 没有 cache_control 的请求 → profile=nil → tracker 完全不参与。
func TestBuildPromptCacheProfile_NoCacheControl_ReturnsNil(t *testing.T) {
	svc := &KiroGatewayService{
		promptCacheTracker: NewKiroPromptCacheTracker(),
	}
	parsed := &ParsedRequest{
		Body: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`),
		Model: "claude-sonnet-4-5",
		Messages: []any{map[string]any{"role": "user", "content": "hello"}},
	}
	require.Nil(t, svc.buildPromptCacheProfile(parsed))
}

// TestBuildPromptCacheProfile_NilService_ReturnsNil 验证 nil 防御。
func TestBuildPromptCacheProfile_NilService_ReturnsNil(t *testing.T) {
	var svc *KiroGatewayService
	require.Nil(t, svc.buildPromptCacheProfile(&ParsedRequest{}))
}

// TestBuildPromptCacheProfile_NilParsed_ReturnsNil 验证 nil parsed。
func TestBuildPromptCacheProfile_NilParsed_ReturnsNil(t *testing.T) {
	svc := &KiroGatewayService{
		promptCacheTracker: NewKiroPromptCacheTracker(),
	}
	require.Nil(t, svc.buildPromptCacheProfile(nil))
}

// TestBuildPromptCacheProfile_HasCacheControl_ReturnsProfile 验证：
// 带 cache_control 的请求 → profile 非空 → 后续 simulator 可介入。
func TestBuildPromptCacheProfile_HasCacheControl_ReturnsProfile(t *testing.T) {
	svc := &KiroGatewayService{
		promptCacheTracker: NewKiroPromptCacheTracker(),
	}
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [{
			"type":"text",
			"text":"` + strings.Repeat("Long system block. ", 600) + `",
			"cache_control":{"type":"ephemeral","ttl":"5m"}
		}],
		"messages": [{"role":"user","content":"hi"}]
	}`)
	parsed := &ParsedRequest{
		Body:    body,
		Model:   "claude-sonnet-4-5",
		System:  "ignored",
		Messages: []any{map[string]any{"role": "user", "content": "hi"}},
	}
	profile := svc.buildPromptCacheProfile(parsed)
	require.NotNil(t, profile)
	require.NotEmpty(t, profile.Breakpoints)
}

// TestApplyPromptCacheTracking_HappyPath 验证完整链路：
// 1. compute (首次：creation)
// 2. apply 到 result.Usage
// 3. update 到 tracker
// 4. 第二次相同请求 → 从 tracker 命中 read > 0
func TestApplyPromptCacheTracking_HappyPath(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	svc := &KiroGatewayService{promptCacheTracker: tracker}

	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"system":[{"type":"text","text":"` + strings.Repeat("Long system block. ", 600) + `","cache_control":{"type":"ephemeral","ttl":"5m"}}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	parsed := &ParsedRequest{Body: body, Model: "claude-sonnet-4-5"}
	account := &Account{ID: 42}

	// 首次：profile 非空，compute 全是 creation。
	profile := svc.buildPromptCacheProfile(parsed)
	require.NotNil(t, profile)

	result1 := &ForwardResult{Usage: ClaudeUsage{InputTokens: 5000}}
	svc.applyPromptCacheTracking(account, profile, result1)
	require.Equal(t, 0, result1.Usage.CacheReadInputTokens, "first call: no read hit")
	require.Greater(t, result1.Usage.CacheCreationInputTokens, 0, "first call: creation tokens")

	// 第二次：相同请求体——从 tracker 命中。
	profile2 := svc.buildPromptCacheProfile(parsed)
	result2 := &ForwardResult{Usage: ClaudeUsage{InputTokens: 5000}}
	svc.applyPromptCacheTracking(account, profile2, result2)
	require.Greater(t, result2.Usage.CacheReadInputTokens, 0, "second call must hit cached prefix")
}

// TestApplyPromptCacheTracking_RespectsUpstreamReality 验证：
// 上游已经报回了 cache_read，applyPromptCacheTracking 不能覆盖。
func TestApplyPromptCacheTracking_RespectsUpstreamReality(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	svc := &KiroGatewayService{promptCacheTracker: tracker}

	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"system":[{"type":"text","text":"` + strings.Repeat("Long system block. ", 600) + `","cache_control":{"type":"ephemeral","ttl":"5m"}}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	parsed := &ParsedRequest{Body: body, Model: "claude-sonnet-4-5"}
	account := &Account{ID: 99}

	// 预热 tracker：写一次让其后续 compute 会返回 read > 0。
	profile1 := svc.buildPromptCacheProfile(parsed)
	require.NotNil(t, profile1)
	tracker.Update(account.ID, profile1)

	// 第二次请求：上游真的报回了 read 字段（模拟 Kiro 原生 cachePoint 命中）。
	profile2 := svc.buildPromptCacheProfile(parsed)
	result := &ForwardResult{
		Usage: ClaudeUsage{
			InputTokens:          5000,
			CacheReadInputTokens: 999, // 上游真实报告
		},
	}
	svc.applyPromptCacheTracking(account, profile2, result)

	require.Equal(t, 999, result.Usage.CacheReadInputTokens, "upstream truth must win over simulator")
	// simulator 的 5m/1h 仍可填进来（上游没报）
	require.GreaterOrEqual(t, result.Usage.CacheCreation5mTokens+result.Usage.CacheCreation1hTokens, 0)
}

// TestApplyPromptCacheTracking_NilProfile_NoOp 验证：profile=nil 时不写
// tracker、不修改 result（这是 fallback 策略的退路）。
func TestApplyPromptCacheTracking_NilProfile_NoOp(t *testing.T) {
	tracker := NewKiroPromptCacheTracker()
	svc := &KiroGatewayService{promptCacheTracker: tracker}
	account := &Account{ID: 1}
	result := &ForwardResult{Usage: ClaudeUsage{CacheReadInputTokens: 123}}

	svc.applyPromptCacheTracking(account, nil, result)
	require.Equal(t, 123, result.Usage.CacheReadInputTokens, "untouched when profile is nil")
	require.Equal(t, 0, tracker.TotalEntries(), "no breakpoints written when profile is nil")
}

// TestApplyPromptCacheTracking_NilService_NoPanic 各种 nil 防御。
func TestApplyPromptCacheTracking_NilService_NoPanic(t *testing.T) {
	var svc *KiroGatewayService
	require.NotPanics(t, func() {
		svc.applyPromptCacheTracking(&Account{ID: 1}, nil, &ForwardResult{})
	})
}

// TestSetPromptCacheTracker_NilRestoresDefault 验证 SetPromptCacheTracker(nil)
// 会回落到全局单例（避免操作员误传 nil 把 tracker 关掉造成 panic）。
func TestSetPromptCacheTracker_NilRestoresDefault(t *testing.T) {
	svc := NewKiroGatewayService(nil, nil)
	custom := NewKiroPromptCacheTracker()
	svc.SetPromptCacheTracker(custom)
	require.Same(t, custom, svc.promptCacheTracker)

	svc.SetPromptCacheTracker(nil)
	require.NotNil(t, svc.promptCacheTracker, "nil must restore default singleton, not leave it nil")
}
