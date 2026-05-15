package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// kiro_prompt_cache_tracker.go 在反代侧追踪 cache_control 断点，
// 模拟 Anthropic 的 prompt caching 行为。
//
// 移植自 Kiro-account-manager/src/main/proxy/promptCacheTracker.ts。
// 设计目标：让客户端（Claude Code/Cline/Cursor 等）发的 cache_control
// 字段在 Kiro 路径上"看得见"——
//   1. Kiro 上游的 prefix cache 仅识别 cachePoint 标记，不解析客户端发的
//      cache_control.ttl ("5m"/"1h") 字段；客户端要的精细 TTL 拆分对账
//      （cache_creation.ephemeral_5m_input_tokens / ephemeral_1h_input_tokens）
//      在 Kiro 上永远拿不到。
//   2. 部分 Kiro 模型不支持 cachePoint，导致 tokenUsage.cacheReadInputTokens
//      永远是 0，客户端的"缓存命中"徽章永远不会亮。
//
// 本 tracker 在反代侧维护 (account_id → fingerprint → expires_at) 表，
// 每次请求复算最长前缀命中并产出 Anthropic 形态的 CacheUsage。
// 集成层应当采用 fallback 策略：上游真实 tokenUsage 已经报回 cache 字段
// 时优先使用上游值，否则回落到本地模拟。
//
// 注意：本 tracker 不影响真实计费——sub2api 的 Kiro 计费走 credit；
// CacheUsage 仅用于回填给客户端的 usage 字段，让 cache_control 字段
// 在统计/UI 上产生效果。

// ==== 常量（与 promptCacheTracker.ts 对齐）====

const (
	// kiroPCDefaultCacheTTL 是 Anthropic ephemeral 默认 TTL。
	kiroPCDefaultCacheTTL = 5 * time.Minute
	// kiroPCOneHourCacheTTL 是 Anthropic ephemeral "1h" TTL。
	kiroPCOneHourCacheTTL = 60 * time.Minute
	// kiroPCDefaultMinCacheableTokens 是非 Opus 模型的最小可缓存 token 阈值。
	// 与 Anthropic 文档对齐——上游对小于阈值的前缀不会真正缓存。
	kiroPCDefaultMinCacheableTokens = 1024
	// kiroPCOpusMinCacheableTokens 是 Opus 系列模型的最小阈值（更高）。
	kiroPCOpusMinCacheableTokens = 4096
	// kiroPCMaxCacheRatio 是单次请求中可被视作"已缓存"的最大比例。
	// 真实场景下最新追加的内容不可能 100% 命中，留 15% 表示当前 turn
	// 的新增上下文。
	kiroPCMaxCacheRatio = 0.85
	// kiroPCMaxEntriesPerAccount 限制单账号缓存条目数，防止内存爆炸。
	kiroPCMaxEntriesPerAccount = 200
	// kiroPCPruneInterval 是过期清理的最小间隔，避免每次请求都遍历。
	kiroPCPruneInterval = 60 * time.Second
)

// ==== 类型定义 ====

// kiroCacheBreakpoint 是单个 cache_control 断点的指纹与累积 token 数。
type kiroCacheBreakpoint struct {
	// Fingerprint 是从 profile 起点到本断点的所有 block 的累积 SHA-256 hash。
	// 同一前缀产出同一指纹，是"前缀缓存"的本质。
	Fingerprint string
	// CumulativeTokens 是到本断点为止的累积 token 数（含本块）。
	CumulativeTokens int
	// TTL 是本断点的有效期，由最近一次显式 cache_control.ttl 决定。
	TTL time.Duration
}

// KiroCacheProfile 是一次请求的缓存断点描述。
// nil 表示请求中没有任何 cache_control 标记，跳过 tracker 即可。
type KiroCacheProfile struct {
	Breakpoints      []kiroCacheBreakpoint
	TotalInputTokens int
	Model            string
}

// KiroCacheUsage 是模拟得到的 Anthropic 形态 cache 字段，
// 直接对应 ClaudeUsage 中的 CacheReadInputTokens/CacheCreationInputTokens。
type KiroCacheUsage struct {
	// CacheCreationInputTokens 是本次请求新写入缓存的 token 数。
	CacheCreationInputTokens int
	// CacheReadInputTokens 是本次请求命中缓存读出的 token 数。
	CacheReadInputTokens int
	// CacheCreation5mTokens 是按 5min ephemeral 分桶计的新缓存 token。
	// 用于后续填充 usage.cache_creation.ephemeral_5m_input_tokens。
	CacheCreation5mTokens int
	// CacheCreation1hTokens 是按 1h ephemeral 分桶计的新缓存 token。
	CacheCreation1hTokens int
}

// IsZero 表示该 usage 对象没有任何信号——所有字段都为 0。
// 集成层用它来判断是否该 fallback 到上游真实值。
func (u KiroCacheUsage) IsZero() bool {
	return u.CacheCreationInputTokens == 0 &&
		u.CacheReadInputTokens == 0 &&
		u.CacheCreation5mTokens == 0 &&
		u.CacheCreation1hTokens == 0
}

// kiroCacheEntry 是 tracker 中存储的单条缓存项。
type kiroCacheEntry struct {
	ExpiresAt time.Time
	TTL       time.Duration
}

// kiroCacheableBlock 是 flatten 后单个待 hash 的 block。
type kiroCacheableBlock struct {
	// Value 是 canonical JSON（带稳定 key 顺序）。
	Value string
	// Tokens 是该 block 的估算 token 数。
	Tokens int
	// TTL > 0 表示该 block 处于 cache_control 显式断点上。
	TTL time.Duration
	// IsMessageEnd 表示该 block 是 message 的最后一个 content block。
	// 隐式断点：只要前面出现过显式断点，每个 message 末尾都自动成为断点。
	IsMessageEnd bool
}

// ==== KiroPromptCacheTracker ====

// KiroPromptCacheTracker 是账号级别的 prompt cache 模拟器。
// 多 goroutine 并发安全（内部 RWMutex）。零值不可直接使用——请通过
// NewKiroPromptCacheTracker 构造。
type KiroPromptCacheTracker struct {
	mu                sync.Mutex
	entriesByAccount  map[int64]map[string]*kiroCacheEntry
	lastPrune         time.Time
	now               func() time.Time // 注入式时钟，方便测试
	estimateTokensFn  func(string) int // 注入式 token 估算器
}

// NewKiroPromptCacheTracker 构造一个全新的 tracker。
func NewKiroPromptCacheTracker() *KiroPromptCacheTracker {
	return &KiroPromptCacheTracker{
		entriesByAccount: make(map[int64]map[string]*kiroCacheEntry),
		lastPrune:        time.Now(),
		now:              time.Now,
		estimateTokensFn: estimateTokensForText,
	}
}

// SetClock 用于测试时注入固定时钟。生产代码不要调用。
func (t *KiroPromptCacheTracker) SetClock(clock func() time.Time) {
	if clock == nil {
		return
	}
	t.mu.Lock()
	t.now = clock
	t.lastPrune = clock()
	t.mu.Unlock()
}

// SetTokenEstimator 用于注入自定义 token 估算（默认走 estimateTokensForText）。
func (t *KiroPromptCacheTracker) SetTokenEstimator(fn func(string) int) {
	if fn == nil {
		return
	}
	t.mu.Lock()
	t.estimateTokensFn = fn
	t.mu.Unlock()
}

// BuildClaudeProfileFromBody 从 Anthropic /v1/messages 请求体生成断点 profile。
// 当请求中不存在任何带 cache_control 的 block 时返回 nil——上层据此跳过 tracker。
//
// totalInputTokens 是上游或本地估算的总 input tokens 数；用于"上限封顶"逻辑：
// matchedTokens 不可超过 totalInputTokens * 0.85。
//
// model 字段用于决定 minCacheableTokens（Opus 系 4096，其他 1024）。
func (t *KiroPromptCacheTracker) BuildClaudeProfileFromBody(body []byte, model string, totalInputTokens int) *KiroCacheProfile {
	if len(body) == 0 {
		return nil
	}
	blocks := t.flattenCacheBlocksFromBody(body)
	if len(blocks) == 0 {
		return nil
	}
	return t.buildProfileFromBlocks(blocks, model, totalInputTokens)
}

// Compute 计算给定 profile 在该账号下的命中情况。
// profile 为 nil 或没有断点时返回零值。
//
// 命中策略：从后往前找最长前缀（ fingerprint 匹配且未过期 ）。命中后
// 刷新该 entry 的过期时间——这与 Anthropic 文档描述的"每次命中重置 TTL"
// 一致，给 sub2api 的多轮对话带来正确的"持续命中"行为。
func (t *KiroPromptCacheTracker) Compute(accountID int64, profile *KiroCacheProfile) KiroCacheUsage {
	if profile == nil || len(profile.Breakpoints) == 0 || accountID == 0 {
		return KiroCacheUsage{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.pruneIfNeededLocked(now)

	minTokens := minCacheableTokensForModel(profile.Model)
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	lastTokens := last.CumulativeTokens
	if lastTokens > profile.TotalInputTokens {
		lastTokens = profile.TotalInputTokens
	}

	entries := t.entriesByAccount[accountID]
	// 首次请求路径：没有任何缓存条目 → 完全是 creation。
	if len(entries) == 0 {
		var creation int
		if lastTokens >= minTokens {
			creation = lastTokens
		}
		c5, c1 := computeTTLBreakdown(profile, 0)
		return KiroCacheUsage{
			CacheCreationInputTokens: creation,
			CacheReadInputTokens:     0,
			CacheCreation5mTokens:    c5,
			CacheCreation1hTokens:    c1,
		}
	}

	// 上限 85%——最新内容不可能 100% 命中。
	maxCacheable := int(float64(profile.TotalInputTokens) * kiroPCMaxCacheRatio)
	if lastTokens > maxCacheable {
		lastTokens = maxCacheable
	}

	// 从后往前找最长前缀命中。
	matchedTokens := 0
	for i := len(profile.Breakpoints) - 1; i >= 0; i-- {
		bp := profile.Breakpoints[i]
		if bp.CumulativeTokens < minTokens {
			continue
		}
		entry, ok := entries[bp.Fingerprint]
		if !ok || entry.ExpiresAt.Before(now) {
			continue
		}
		// 命中——刷新过期时间。
		entry.ExpiresAt = now.Add(entry.TTL)
		matched := bp.CumulativeTokens
		if matched > profile.TotalInputTokens {
			matched = profile.TotalInputTokens
		}
		if matched > lastTokens {
			matched = lastTokens
		}
		matchedTokens = matched
		break
	}

	creation := lastTokens - matchedTokens
	if creation < 0 {
		creation = 0
	}
	c5, c1 := computeTTLBreakdown(profile, matchedTokens)
	return KiroCacheUsage{
		CacheCreationInputTokens: creation,
		CacheReadInputTokens:     matchedTokens,
		CacheCreation5mTokens:    c5,
		CacheCreation1hTokens:    c1,
	}
}

// Update 在请求成功后写入/更新缓存条目。profile 为 nil 时无操作。
//
// 写入策略：所有 cumulativeTokens >= minCacheableTokens 的断点都会被
// 记下；超过单账号上限（200）时按过期时间从早到晚淘汰。
func (t *KiroPromptCacheTracker) Update(accountID int64, profile *KiroCacheProfile) {
	if profile == nil || len(profile.Breakpoints) == 0 || accountID == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	minTokens := minCacheableTokensForModel(profile.Model)
	now := t.now()

	entries := t.entriesByAccount[accountID]
	if entries == nil {
		entries = make(map[string]*kiroCacheEntry)
		t.entriesByAccount[accountID] = entries
	}

	for _, bp := range profile.Breakpoints {
		if bp.CumulativeTokens < minTokens {
			continue
		}
		entries[bp.Fingerprint] = &kiroCacheEntry{
			ExpiresAt: now.Add(bp.TTL),
			TTL:       bp.TTL,
		}
	}

	// 超出上限时按过期时间从早到晚淘汰最旧条目。
	if len(entries) > kiroPCMaxEntriesPerAccount {
		type kv struct {
			key string
			exp time.Time
		}
		all := make([]kv, 0, len(entries))
		for k, v := range entries {
			all = append(all, kv{key: k, exp: v.ExpiresAt})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].exp.Before(all[j].exp) })
		drop := len(entries) - kiroPCMaxEntriesPerAccount
		for i := 0; i < drop; i++ {
			delete(entries, all[i].key)
		}
	}
}

// Clear 清空所有账号缓存，返回被删除的条目总数。
// 主要用于运维 / 测试。
func (t *KiroPromptCacheTracker) Clear() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, entries := range t.entriesByAccount {
		count += len(entries)
	}
	t.entriesByAccount = make(map[int64]map[string]*kiroCacheEntry)
	return count
}

// TotalEntries 返回所有账号缓存条目的总数（含已过期未清理的）。
// 主要用于运维监控。
func (t *KiroPromptCacheTracker) TotalEntries() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, entries := range t.entriesByAccount {
		count += len(entries)
	}
	return count
}

// ==== 内部：profile 构造 ====

// flattenCacheBlocksFromBody 把 Anthropic 请求体扁平化为有序的 block 列表，
// 顺序与 Anthropic prompt cache 的实际排序一致：tools → system → messages。
func (t *KiroPromptCacheTracker) flattenCacheBlocksFromBody(body []byte) []kiroCacheableBlock {
	out := make([]kiroCacheableBlock, 0, 32)

	// 1) tools[]
	gjson.GetBytes(body, "tools").ForEach(func(_, tool gjson.Result) bool {
		obj := map[string]any{
			"kind":         "tool",
			"name":         tool.Get("name").String(),
			"description":  tool.Get("description").String(),
			"input_schema": parseJSONOrString(tool.Get("input_schema")),
		}
		value := canonicalJSON(obj)
		out = append(out, kiroCacheableBlock{
			Value:        value,
			Tokens:       t.estimateTokensFn(value),
			TTL:          extractTTLFromGJSON(tool.Get("cache_control")),
			IsMessageEnd: false,
		})
		return true
	})

	// 2) system 可能是 string 或 []block。
	system := gjson.GetBytes(body, "system")
	switch system.Type {
	case gjson.String:
		text := system.String()
		obj := map[string]any{"kind": "system", "type": "text", "text": text}
		value := canonicalJSON(obj)
		out = append(out, kiroCacheableBlock{
			Value:        value,
			Tokens:       t.estimateTokensFn(text),
			TTL:          0,
			IsMessageEnd: false,
		})
	default:
		if system.IsArray() {
			system.ForEach(func(_, block gjson.Result) bool {
				var raw any
				if block.Type == gjson.String {
					raw = map[string]any{"type": "text", "text": block.String()}
				} else {
					raw = parseJSONOrString(block)
				}
				obj := map[string]any{"kind": "system", "block": raw}
				value := canonicalJSON(obj)
				text := block.Get("text").String()
				if text == "" {
					text = block.Raw
				}
				out = append(out, kiroCacheableBlock{
					Value:        value,
					Tokens:       t.estimateTokensFn(text),
					TTL:          extractTTLFromGJSON(block.Get("cache_control")),
					IsMessageEnd: false,
				})
				return true
			})
		}
	}

	// 3) messages[]
	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		msgIdx := -1
		messages.ForEach(func(_, msg gjson.Result) bool {
			msgIdx++
			role := msg.Get("role").String()
			content := msg.Get("content")
			messageCC := extractTTLFromGJSON(msg.Get("cache_control"))

			switch content.Type {
			case gjson.String:
				text := content.String()
				obj := map[string]any{
					"kind":  "message",
					"role":  role,
					"index": msgIdx,
					"type":  "text",
					"text":  text,
				}
				value := canonicalJSON(obj)
				out = append(out, kiroCacheableBlock{
					Value:        value,
					Tokens:       t.estimateTokensFn(text),
					TTL:          messageCC,
					IsMessageEnd: true,
				})
			default:
				if !content.IsArray() {
					return true
				}
				arr := content.Array()
				lastIdx := len(arr) - 1
				for i, block := range arr {
					var blockObj any
					if block.Type == gjson.String {
						blockObj = map[string]any{"type": "text", "text": block.String()}
					} else {
						blockObj = parseJSONOrString(block)
					}
					obj := map[string]any{
						"kind":       "message",
						"role":       role,
						"index":      msgIdx,
						"blockIndex": i,
						"block":      blockObj,
					}
					value := canonicalJSON(obj)
					text := block.Get("text").String()
					if text == "" {
						text = block.Get("thinking").String()
					}
					if text == "" {
						text = block.Raw
					}
					blockTTL := extractTTLFromGJSON(block.Get("cache_control"))
					// Block 自己的 cache_control 优先；否则继承 message 级别的
					// cache_control（Anthropic 允许在 message 上打整体断点）。
					if blockTTL == 0 && i == lastIdx {
						blockTTL = messageCC
					}
					out = append(out, kiroCacheableBlock{
						Value:        value,
						Tokens:       t.estimateTokensFn(text),
						TTL:          blockTTL,
						IsMessageEnd: i == lastIdx,
					})
				}
			}
			return true
		})
	}

	return out
}

// buildProfileFromBlocks 把扁平 block 列表转为带断点的 profile。
// 关键算法：sha256 累积 hash + 显式/隐式断点判定。
func (t *KiroPromptCacheTracker) buildProfileFromBlocks(blocks []kiroCacheableBlock, model string, totalInputTokens int) *KiroCacheProfile {
	hasher := sha256.New()
	breakpoints := make([]kiroCacheBreakpoint, 0, 4)
	cumulative := 0
	var activeTTL time.Duration

	for _, block := range blocks {
		hashChunk(hasher, block.Value)
		cumulative += block.Tokens

		var bpTTL time.Duration
		if block.TTL > 0 {
			bpTTL = block.TTL
			activeTTL = block.TTL
		} else if block.IsMessageEnd && activeTTL > 0 {
			// 隐式断点：在出现过显式断点之后，每个 message 结束都视为断点
			// （它们都在已建立的缓存范围内）。
			bpTTL = activeTTL
		}

		if bpTTL <= 0 {
			continue
		}
		breakpoints = append(breakpoints, kiroCacheBreakpoint{
			Fingerprint: hex.EncodeToString(cloneSum(hasher)),
			CumulativeTokens: cumulative,
			TTL:              bpTTL,
		})
	}

	if len(breakpoints) == 0 {
		return nil
	}
	total := totalInputTokens
	if cumulative > total {
		total = cumulative
	}
	return &KiroCacheProfile{
		Breakpoints:      breakpoints,
		TotalInputTokens: total,
		Model:            model,
	}
}

// ==== 工具函数 ====

// computeTTLBreakdown 把 (matchedTokens, profile.TotalInputTokens] 区间按
// 各断点的 TTL 拆成 5min 桶 / 1h 桶。matchedTokens 之前的部分视为已命中
// 不计入 creation；之后的 token 才算"新写入"。
func computeTTLBreakdown(profile *KiroCacheProfile, matchedTokens int) (int, int) {
	if profile == nil {
		return 0, 0
	}
	var c5, c1 int
	previous := matchedTokens
	for _, bp := range profile.Breakpoints {
		current := bp.CumulativeTokens
		if current > profile.TotalInputTokens {
			current = profile.TotalInputTokens
		}
		if current <= previous {
			continue
		}
		delta := current - previous
		if bp.TTL >= kiroPCOneHourCacheTTL {
			c1 += delta
		} else {
			c5 += delta
		}
		previous = current
	}
	return c5, c1
}

// extractTTLFromGJSON 从 cache_control 子节点解析出 TTL。
//
// 行为对齐 Anthropic 文档与 promptCacheTracker.ts：
//   - type 必须是 "ephemeral"（大小写不敏感）；其他类型视为 0
//   - ttl == "1h"/"1H" → 1 小时
//   - ttl 是数字（秒）→ 直接换算
//   - 缺省 → 5 分钟
//   - cache_control 不存在 → 0（非断点）
func extractTTLFromGJSON(cc gjson.Result) time.Duration {
	if !cc.Exists() {
		return 0
	}
	if !strings.EqualFold(cc.Get("type").String(), "ephemeral") {
		return 0
	}
	ttl := cc.Get("ttl")
	switch {
	case !ttl.Exists():
		return kiroPCDefaultCacheTTL
	case ttl.Type == gjson.String:
		s := strings.TrimSpace(strings.ToLower(ttl.String()))
		if s == "1h" {
			return kiroPCOneHourCacheTTL
		}
		if s == "5m" || s == "" {
			return kiroPCDefaultCacheTTL
		}
		// 字符串形式数字也支持（容错处理）
		if d := ttl.Int(); d > 0 {
			return time.Duration(d) * time.Second
		}
		return kiroPCDefaultCacheTTL
	case ttl.Type == gjson.Number:
		if d := ttl.Int(); d > 0 {
			return time.Duration(d) * time.Second
		}
		return kiroPCDefaultCacheTTL
	}
	return kiroPCDefaultCacheTTL
}

// canonicalJSON 把任意 Go 对象序列化为 key 排序后的稳定 JSON 字符串。
// 配合 sha256 累积 hash 用于断点指纹——同样的输入必产出同样的指纹。
func canonicalJSON(v any) string {
	var sb strings.Builder
	writeCanonical(&sb, v)
	return sb.String()
}

func writeCanonical(sb *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		sb.WriteString("null")
	case bool:
		if x {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case string:
		writeJSONString(sb, x)
	case float64:
		sb.WriteString(formatJSONNumber(x))
	case int:
		sb.WriteString(formatJSONInt(int64(x)))
	case int64:
		sb.WriteString(formatJSONInt(x))
	case []any:
		sb.WriteByte('[')
		for i, it := range x {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeCanonical(sb, it)
		}
		sb.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeJSONString(sb, k)
			sb.WriteByte(':')
			writeCanonical(sb, x[k])
		}
		sb.WriteByte('}')
	default:
		// 兜底：parseJSONOrString 应该已经把所有 gjson 类型映射成
		// nil/bool/float64/string/[]any/map[string]any，理论上不会进入。
		// 仍然保留 fmt.Sprint 兜底以确保 hash 输入永远稳定。
		writeJSONString(sb, fmt.Sprint(x))
	}
}

func writeJSONString(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 {
				sb.WriteString(`\u00`)
				const hexDigits = "0123456789abcdef"
				sb.WriteByte(hexDigits[r>>4])
				sb.WriteByte(hexDigits[r&0xf])
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
}

// formatJSONNumber 实现 JSON 数字格式化。整数走 strconv.FormatInt，
// 非整数走 strconv.FormatFloat 的 'g' 格式（与 encoding/json 默认一致）。
func formatJSONNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func formatJSONInt(n int64) string {
	return strconv.FormatInt(n, 10)
}

// hashChunk 把单个 block 以 "<len>\0<value>\0" 形式写进累积 hasher。
// 长度前缀防止 ["ab","c"] 与 ["a","bc"] 产生同样的 hash。
func hashChunk(h hash.Hash, chunk string) {
	h.Write([]byte(formatJSONInt(int64(len(chunk)))))
	h.Write([]byte{0})
	h.Write([]byte(chunk))
	h.Write([]byte{0})
}

// cloneSum 在不破坏 hasher 内部状态的前提下取一次 SHA-256 摘要。
// hash.Hash 的 Sum(nil) 是 append 语义，但内部状态会被复用；为了多次
// 取累积摘要，先 clone 一份再 Sum。
func cloneSum(h hash.Hash) []byte {
	// hash.Hash 的内置 Clone 不在标准接口中，但 sha256.digest 实现了
	// 一个未导出的 Clone；我们走 BinaryMarshaler 路径绕过去。
	type marshaler interface {
		MarshalBinary() (data []byte, err error)
	}
	type unmarshaler interface {
		UnmarshalBinary(data []byte) error
	}
	if m, ok := h.(marshaler); ok {
		data, err := m.MarshalBinary()
		if err == nil {
			fresh := sha256.New()
			if u, ok := fresh.(unmarshaler); ok {
				if err := u.UnmarshalBinary(data); err == nil {
					return fresh.Sum(nil)
				}
			}
		}
	}
	// 兜底：直接 Sum 当前状态（一次性，调用者要确保此后不再追加）。
	return h.Sum(nil)
}

// minCacheableTokensForModel 选择对应模型的最小可缓存 token 阈值。
// 与 Anthropic 文档对齐：Opus 4096，其他系列 1024。
func minCacheableTokensForModel(model string) int {
	if strings.Contains(strings.ToLower(model), "opus") {
		return kiroPCOpusMinCacheableTokens
	}
	return kiroPCDefaultMinCacheableTokens
}

// pruneIfNeededLocked 周期性清理过期条目。调用方必须持有 mu。
func (t *KiroPromptCacheTracker) pruneIfNeededLocked(now time.Time) {
	if now.Sub(t.lastPrune) < kiroPCPruneInterval {
		return
	}
	t.lastPrune = now
	for accountID, entries := range t.entriesByAccount {
		for fp, entry := range entries {
			if entry.ExpiresAt.Before(now) {
				delete(entries, fp)
			}
		}
		if len(entries) == 0 {
			delete(t.entriesByAccount, accountID)
		}
	}
}

// parseJSONOrString 把 gjson.Result 转为 Go 原生类型用于 canonicalJSON。
// 复杂对象走 ParseRaw，简单类型直接断言。
func parseJSONOrString(r gjson.Result) any {
	switch r.Type {
	case gjson.Null:
		return nil
	case gjson.False:
		return false
	case gjson.True:
		return true
	case gjson.Number:
		return r.Num
	case gjson.String:
		return r.String()
	case gjson.JSON:
		// 用 gjson 自己的递归遍历重建 Go 结构——避免再引入 encoding/json。
		if r.IsArray() {
			arr := r.Array()
			out := make([]any, len(arr))
			for i, it := range arr {
				out[i] = parseJSONOrString(it)
			}
			return out
		}
		if r.IsObject() {
			out := make(map[string]any)
			r.ForEach(func(key, value gjson.Result) bool {
				out[key.String()] = parseJSONOrString(value)
				return true
			})
			return out
		}
	}
	return r.Raw
}

// 全局单例，与 promptCacheTracker.ts 的 export const promptCacheTracker 对齐。
// KiroGatewayService 在构造时持有引用；wire 层不需要显式管理生命周期。
var defaultKiroPromptCacheTracker = NewKiroPromptCacheTracker()

// DefaultKiroPromptCacheTracker 返回进程级单例。
func DefaultKiroPromptCacheTracker() *KiroPromptCacheTracker {
	return defaultKiroPromptCacheTracker
}
