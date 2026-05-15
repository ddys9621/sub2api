package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
)

// kiroUpstreamHostTemplate is the Amazon Q host used by Kiro IDE. Kiro IDE
// (Electron, aws-sdk-js) calls the path-style RPC endpoint
//
//	POST https://q.<region>.amazonaws.com/generateAssistantResponse
//
// — the operation name lives in the URL path, NOT in X-Amz-Target. We mirror
// that exact pattern in kiroUpstreamEndpointForAccount so our requests match
// the real IDE traffic and AWS abuse heuristics do not flag us as "aggregator
// CLI traffic". The trailing slash is kept here so non-streaming REST helpers
// (list-models / subscription endpoints) can build URLs by joining a path.
const kiroUpstreamHostTemplate = "https://q.%s.amazonaws.com/"

// kiroStreamingPath is the IDE path-style operation; Kiro IDE issues POSTs
// directly to /generateAssistantResponse instead of using X-Amz-Target.
const kiroStreamingPath = "generateAssistantResponse"

const (
	// kiroStreamingAmzTarget is kept for back-compat only — current Kiro IDE
	// traffic uses the path-style endpoint above and does NOT send this
	// header. We still expose the constant so non-IDE REST helpers and
	// integration tests can reference the canonical operation name.
	kiroStreamingAmzTarget           = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
	kiroListAvailableModelsAmzTarget = "AmazonCodeWhispererService.ListAvailableModels"
	kiroStreamingAPIName             = "codewhispererstreaming"
	kiroRuntimeAPIName               = "codewhispererruntime"
)

// Real Kiro IDE client identity. Keep these in sync with the public Kiro
// desktop build; AWS abuse heuristics compare User-Agent + machineId
// against expected aws-sdk-js fingerprints, so drift here directly raises
// the ban risk.
const (
	// kiroIDEVersion mirrors the latest public KiroIDE release. Bump this
	// when Kiro publishes a new IDE version (the format is the literal
	// value Kiro emits in User-Agent, NOT semver-validated by AWS).
	kiroIDEVersion = "0.12.155"
	// kiroIDESDKVersion is the aws-sdk-js version Kiro IDE ships with.
	kiroIDESDKVersion = "1.0.34"
	// kiroIDEAPIVersion is the streaming API version Kiro IDE advertises.
	kiroIDEAPIVersion = "1.0.34"
	// kiroIDEAgentModeIDE / CLI mirror Kiro's two upstream personas:
	// `spec` for the IDE GUI, `vibe` for the Amazon Q CLI shim. We
	// default to `spec` because every sub2api consumer is the IDE
	// completion endpoint; IDC (Identity Center) accounts may opt
	// into `vibe` via credentials.auth_method = "idc".
	kiroIDEAgentModeIDE = "spec"
	kiroIDEAgentModeCLI = "vibe"
)

// stableKiroMachineID returns the per-account device fingerprint that gets
// embedded into User-Agent. AWS abuse detection treats UA + machineId as a
// device tuple; if every account shares the same UA the whole pool is
// trivially linkable. Priority order:
//
//  1. credentials.machine_id (set by the admin or auto-bound at OAuth time)
//  2. sha256("kiro-device-<account.id>") deterministic fallback
//
// The fallback is stable across restarts so a given account always presents
// the same MachineGuid to AWS — matching real Kiro IDE behaviour where the
// GUID is read once from the local OS registry / /etc/machine-id and reused.
func stableKiroMachineID(account *Account) string {
	if account == nil {
		return ""
	}
	if mid := strings.TrimSpace(account.GetCredential("machine_id")); mid != "" {
		return mid
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("kiro-device-%d", account.ID)))
	return hex.EncodeToString(sum[:])
}

// kiroIDEUserAgent builds the User-Agent header in the exact format the real
// Kiro IDE emits. Example:
//
//	aws-sdk-js/1.0.34 ua/2.1 os/win32#10.0.19045 lang/js md/nodejs#22.22.0 \
//	  api/codewhispererstreaming#1.0.34 m/E KiroIDE-0.12.155-<machineId>
//
// The machineId suffix is what makes per-account flows look like distinct
// devices instead of one shared aggregator.
func kiroIDEUserAgent(machineID string) string {
	suffix := "KiroIDE-" + kiroIDEVersion
	if machineID != "" {
		suffix = "KiroIDE-" + kiroIDEVersion + "-" + machineID
	}
	return fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/win32#10.0.19045 lang/js md/nodejs#22.22.0 api/%s#%s m/E %s",
		kiroIDESDKVersion, kiroStreamingAPIName, kiroIDEAPIVersion, suffix,
	)
}

// kiroIDEAmzUserAgent builds X-Amz-User-Agent in Kiro IDE's exact format,
// e.g. `aws-sdk-js/1.0.34 KiroIDE 0.12.155 <machineId>`. Note the space
// separators (the User-Agent variant uses dashes).
func kiroIDEAmzUserAgent(machineID string) string {
	suffix := "KiroIDE-" + kiroIDEVersion
	if machineID != "" {
		suffix = "KiroIDE " + kiroIDEVersion + " " + machineID
	}
	return fmt.Sprintf("aws-sdk-js/%s %s", kiroIDESDKVersion, suffix)
}

// kiroAgentModeFor returns the value of the x-amzn-kiro-agent-mode header
// AWS expects on every Kiro IDE request. Identity Center accounts (Amazon
// Q for CLI behind the scenes) opt into `vibe`; the default IDE login
// path uses `spec`.
func kiroAgentModeFor(account *Account) string {
	if account == nil {
		return kiroIDEAgentModeIDE
	}
	if strings.EqualFold(strings.TrimSpace(account.GetCredential("auth_method")), "idc") {
		return kiroIDEAgentModeCLI
	}
	return kiroIDEAgentModeIDE
}

// kiroForwardTimeout bounds the total round-trip for a single Kiro request.
// Claude Sonnet 4.5 streaming responses complete in well under 2 minutes on
// average; we allow 3 minutes so slow tool-calling turns do not abort early.
const kiroForwardTimeout = 3 * time.Minute

// KiroGatewayService forwards Anthropic-style /v1/messages requests to Kiro's
// Amazon Q endpoint and translates the AWS EventStream response back into
// Anthropic SSE.
type KiroGatewayService struct {
	tokenProvider *KiroTokenProvider
	settings      *SettingService
	// channelService is optional and populated after construction via
	// SetWebSearchDeps to avoid a wire cycle (ChannelService is constructed
	// later in the wire graph than KiroGatewayService).
	channelService *ChannelService
	// mcpResultCache is an optional Redis-backed cache for web_search MCP
	// responses; populated via SetWebSearchMCPCache after construction so
	// we avoid adding a new required dependency to NewKiroGatewayService
	// (and therefore avoid regenerating wire). When nil, every search
	// round-trips to Kiro as before.
	mcpResultCache *kiroMCPResultCache
	// promptCacheTracker simulates Anthropic prompt-caching on Kiro accounts.
	// Kiro CodeWhisperer's native prefix cache reads cachePoint markers but
	// does not honour client-supplied cache_control.ttl ("5m"/"1h"). Some
	// Kiro models do not even emit tokenUsage.cacheReadInputTokens. The
	// tracker fills both gaps locally so the cache_control fields a client
	// sends actually surface in the response usage. Defaults to the global
	// singleton; tests can swap via SetPromptCacheTracker.
	promptCacheTracker *KiroPromptCacheTracker
	// httpClient is reused across requests; Kiro happily serves multiple
	// sequential streaming calls over the same HTTP/2 connection so a single
	// pool is fine.
	httpClient *http.Client
}

// NewKiroGatewayService constructs the service and its long-lived http client.
func NewKiroGatewayService(tokenProvider *KiroTokenProvider, settings *SettingService) *KiroGatewayService {
	return &KiroGatewayService{
		tokenProvider:      tokenProvider,
		settings:           settings,
		promptCacheTracker: DefaultKiroPromptCacheTracker(),
		httpClient: &http.Client{
			Timeout: 0, // set per-request via context
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          128,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
	}
}

// SetPromptCacheTracker swaps the prompt cache tracker. nil restores the
// process-level singleton. Used by integration tests that need an isolated
// tracker per test run.
func (s *KiroGatewayService) SetPromptCacheTracker(tracker *KiroPromptCacheTracker) {
	if s == nil {
		return
	}
	if tracker == nil {
		s.promptCacheTracker = DefaultKiroPromptCacheTracker()
		return
	}
	s.promptCacheTracker = tracker
}

// buildPromptCacheProfile inspects the original /v1/messages body for
// cache_control breakpoints and returns the corresponding profile. The
// profile is the only thing the simulator needs to compute hit/miss on
// the way back; nil means the request did not opt into caching at all,
// in which case the entire simulator is bypassed and the existing upstream
// transparency wins.
//
// totalInputTokens uses estimateKiroInputTokens — the same heuristic the
// rest of the Kiro path uses when the upstream omits usage.input_tokens.
func (s *KiroGatewayService) buildPromptCacheProfile(parsed *ParsedRequest) *KiroCacheProfile {
	if s == nil || s.promptCacheTracker == nil || parsed == nil || len(parsed.Body) == 0 {
		return nil
	}
	estimatedInput := estimateKiroInputTokens(parsed)
	return s.promptCacheTracker.BuildClaudeProfileFromBody(parsed.Body, parsed.Model, estimatedInput)
}

// applyPromptCacheTracking is the post-flight half of the simulator. It
//   1. computes the simulated cache_read/cache_creation against the current
//      account-scoped breakpoint store,
//   2. merges those numbers into result.Usage via applySimulatedCacheUsage
//      (upstream values still win when present),
//   3. records the request's breakpoints so the *next* call can hit them.
//
// Caller MUST gate this on a successful upstream stream/JSON drive — a
// partial response would write breakpoints the model never finished
// producing, and the next request would falsely "hit" them.
func (s *KiroGatewayService) applyPromptCacheTracking(account *Account, profile *KiroCacheProfile, result *ForwardResult) {
	if s == nil || s.promptCacheTracker == nil || profile == nil || account == nil || result == nil {
		return
	}
	sim := s.promptCacheTracker.Compute(account.ID, profile)
	applySimulatedCacheUsage(&result.Usage, sim)
	s.promptCacheTracker.Update(account.ID, profile)
}

// applySimulatedCacheUsage merges the local-tracker fallback into the live
// usage record. Strategy:
//   - Upstream Kiro tokenUsage already returned a non-zero cache field
//     (cache_read OR cache_creation): trust it; the upstream prefix cache
//     actually fired. Skip the simulator entirely so we don't double-count.
//   - Otherwise: write the simulator's numbers in place. This is what makes
//     the client-side cache_control field finally produce a visible effect
//     for Kiro accounts.
//
// The 5m/1h split is always taken from the simulator when it ran, because
// Kiro upstream never reports it.
func applySimulatedCacheUsage(usage *ClaudeUsage, sim KiroCacheUsage) {
	if usage == nil || sim.IsZero() {
		return
	}
	hasUpstreamCache := usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0
	if hasUpstreamCache {
		// Trust upstream; only fill the 5m/1h split if Kiro left it empty
		// (it always will today — Kiro upstream never reports the split).
		if usage.CacheCreation5mTokens == 0 && usage.CacheCreation1hTokens == 0 {
			usage.CacheCreation5mTokens = sim.CacheCreation5mTokens
			usage.CacheCreation1hTokens = sim.CacheCreation1hTokens
		}
		return
	}
	usage.CacheCreationInputTokens = sim.CacheCreationInputTokens
	usage.CacheReadInputTokens = sim.CacheReadInputTokens
	usage.CacheCreation5mTokens = sim.CacheCreation5mTokens
	usage.CacheCreation1hTokens = sim.CacheCreation1hTokens
}

// SetWebSearchDeps wires in the ChannelService used for channel-level
// "default" emulation mode lookups. Called by wire_gen.go after both
// services exist; safe to pass nil to keep the feature disabled.
func (s *KiroGatewayService) SetWebSearchDeps(ch *ChannelService) {
	if s == nil {
		return
	}
	s.channelService = ch
}

// SetWebSearchMCPCache wires in an optional Redis-backed cache for
// /mcp web_search responses. Skipping this call leaves the cache nil
// and search results are always re-fetched.
func (s *KiroGatewayService) SetWebSearchMCPCache(rdb *redis.Client) {
	if s == nil {
		return
	}
	s.mcpResultCache = newKiroMCPResultCache(rdb)
}

// kiroWebSearchDeps adapts *KiroGatewayService's local state to the shared
// webSearchEmulationDeps interface so the decision logic can be reused.
type kiroWebSearchDeps struct {
	settings *SettingService
	channels *ChannelService
}

func (d kiroWebSearchDeps) IsWebSearchEmulationEnabledGlobally(ctx context.Context) bool {
	if d.settings == nil {
		return false
	}
	return d.settings.IsWebSearchEmulationEnabled(ctx)
}

func (d kiroWebSearchDeps) ChannelForGroup(ctx context.Context, groupID int64) (*Channel, error) {
	if d.channels == nil {
		return nil, fmt.Errorf("kiro websearch: channel service unavailable")
	}
	return d.channels.GetChannelForGroup(ctx, groupID)
}

// Forward is the entry point called from the gateway handler. It mirrors the
// contract of AntigravityGatewayService.Forward: parse the request, select
// payload fields, perform the streaming POST, and write an Anthropic-shaped
// response to the gin context.
func (s *KiroGatewayService) Forward(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *ParsedRequest,
) (*ForwardResult, error) {
	start := time.Now()
	if account == nil {
		return nil, errors.New("kiro forward: account is nil")
	}
	if parsed == nil {
		return nil, errors.New("kiro forward: parsed request is nil")
	}

	// Web Search 模拟:Kiro CodeWhisperer 上游本身不支持 web_search 工具,
	// 若请求里恰好只携带一个 web_search 工具且模拟条件成立,直接走第三方
	// 搜索 Provider 构造 Anthropic 风格响应,跳过上游调用。
	if evaluateWebSearchEmulation(ctx, kiroWebSearchDeps{
		settings: s.settings,
		channels: s.channelService,
	}, account, parsed.GroupID, parsed.Body) {
		return executeWebSearchEmulation(ctx, c, account, parsed)
	}

	// Build the prompt-cache profile from the original request body before any
	// transformer mutates it. The profile is reused on the way out to compute
	// the simulated cache_read/cache_creation usage. nil means the request
	// carries no cache_control breakpoints — in that case the simulator stays
	// out of the way and the upstream tokenUsage is the only source of truth.
	cacheProfile := s.buildPromptCacheProfile(parsed)

	// Resolve token (refresh as needed).
	token, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("kiro token: %w", err)
	}
	profileArn := s.tokenProvider.ProfileArn(account)
	// profileArn may be empty for Builder ID accounts whose OIDC /token
	// response did not include it. BuildKiroPayload will simply omit the
	// field from the upstream request; Kiro accepts this for Builder ID
	// sessions (profileArn is only strictly required for Desktop Auth /
	// Google social login accounts).

	// Convert Anthropic payload to Kiro conversationState.
	anthropicReq, err := parsedToKiroAnthropic(parsed)
	if err != nil {
		return nil, fmt.Errorf("kiro forward: %w", err)
	}
	mapping := kiroModelMappingForAccount(account)
	payload, err := kiro.BuildKiroPayload(anthropicReq, kiro.BuildOptions{
		ProfileArn:     profileArn,
		ModelMapping:   mapping,
		ConversationID: conversationIDFromContext(c),
	})
	if err != nil {
		return nil, fmt.Errorf("kiro forward: build payload: %w", err)
	}

	upstreamModel := kiro.ResolveModel(parsed.Model, mapping)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("kiro forward: marshal payload: %w", err)
	}
	kiroEndpoint := kiroUpstreamEndpointForAccount(account, profileArn)

	forwardCtx, cancel := context.WithTimeout(ctx, kiroForwardTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(forwardCtx, http.MethodPost, kiroEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kiro forward: build request: %w", err)
	}
	httpReq.Header = kiroUpstreamHeaders(token, account)

	// Route through the account's outbound proxy (if configured).
	client := s.clientForAccount(account)
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kiro forward: http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		if resp.StatusCode == http.StatusBadRequest && kiro.IsInvalidModelError(raw) {
			fallbackModel := kiro.FallbackModelForInvalidModel(parsed.Model, upstreamModel)
			if fallbackModel != "" && fallbackModel != upstreamModel {
				slog.Warn("kiro upstream rejected model; retrying with fallback model",
					"requested_model", parsed.Model,
					"upstream_model", upstreamModel,
					"fallback_model", fallbackModel,
				)
				fallbackMapping := cloneKiroModelMapping(mapping)
				fallbackMapping[anthropicReq.Model] = fallbackModel
				fallbackPayload, berr := kiro.BuildKiroPayload(anthropicReq, kiro.BuildOptions{
					ProfileArn:     profileArn,
					ModelMapping:   fallbackMapping,
					ConversationID: conversationIDFromContext(c),
				})
				if berr != nil {
					return nil, fmt.Errorf("kiro forward: build fallback payload: %w", berr)
				}
				fallbackBody, merr := json.Marshal(fallbackPayload)
				if merr != nil {
					return nil, fmt.Errorf("kiro forward: marshal fallback payload: %w", merr)
				}
				_ = resp.Body.Close()
				fallbackReq, rerr := http.NewRequestWithContext(forwardCtx, http.MethodPost, kiroEndpoint, bytes.NewReader(fallbackBody))
				if rerr != nil {
					return nil, fmt.Errorf("kiro forward: build fallback request: %w", rerr)
				}
				fallbackReq.Header = kiroUpstreamHeaders(token, account)
				resp, err = client.Do(fallbackReq)
				if err != nil {
					return nil, fmt.Errorf("kiro forward: fallback http do: %w", err)
				}
				defer resp.Body.Close()
				mapping = fallbackMapping
				upstreamModel = fallbackModel
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					goto kiroResponseOK
				}
				raw, _ = io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			}
		}
		return nil, &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    raw,
			ResponseHeaders: resp.Header,
		}
	}

kiroResponseOK:
	requestID := resp.Header.Get("X-Amzn-Requestid")
	conversationID := resp.Header.Get("X-Amzn-Codewhisperer-Conversation-Id")

	if !parsed.Stream {
		// --- Non-streaming path: buffer the whole stream, collapse it
		// into a single Anthropic Messages JSON response body, and send
		// it back as application/json. Claude Code and several other
		// clients issue stream=false for certain tool-follow-up turns
		// (e.g. WebFetch polling). They choke on text/event-stream.
		return s.forwardNonStream(
			forwardCtx, c, resp.Body, account, parsed, anthropicReq,
			profileArn, mapping, requestID, conversationID, upstreamModel,
			start, token, client, kiroEndpoint, cacheProfile,
		)
	}

	// Prepare response for Anthropic SSE streaming.
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, _ := c.Writer.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	encoder := kiro.NewAnthropicSSEEncoder(c.Writer, flush, parsed.Model)
	var firstTokenMs *int
	encoder.SetFirstOutputCallback(func() {
		if firstTokenMs != nil {
			return
		}
		ms := int(time.Since(start).Milliseconds())
		firstTokenMs = &ms
	})

	// Pre-populate input_tokens in message_start so relay consumers (e.g. a
	// second sub2api instance) can record it without waiting for message_delta.
	// Kiro upstream does not report input_tokens; we estimate from body size.
	estimatedInput := estimateKiroInputTokens(parsed)
	if estimatedInput > 0 {
		encoder.SetInputTokensHint(int64(estimatedInput))
	}

	// Install the web_search interceptor: when the Kiro model invokes
	// `WebSearch` / `web_search`, the interceptor runs the search via
	// Kiro's /mcp endpoint and then launches a follow-up
	// /generateAssistantResponse turn whose history includes the synthetic
	// tool_result. The model's natural-language summary is streamed back
	// to the client; the underlying tool_use never leaks.
	interceptor := newKiroWebSearchInterceptor(
		forwardCtx,
		client,
		token,
		anthropicReq,
		kiro.BuildOptions{
			ProfileArn:     profileArn,
			ModelMapping:   mapping,
			ConversationID: conversationIDFromContext(c),
		},
		kiroEndpoint,
		kiroUpstreamHeaders(token, account),
		s.mcpResultCache,
		account.ID,
		resolveAccountProxyURL(account),
	)
	text, err := kiro.DriveEventStreamToAnthropicWithInterceptor(forwardCtx, resp.Body, encoder, interceptor)
	if err != nil && !errors.Is(err, context.Canceled) {
		// Stream already started; emit an error event so the client can fail gracefully.
		_ = encoder.Emit(&kiro.StreamEvent{Kind: "error", ErrorType: "upstream_error", ErrorMessage: err.Error()})
	}
	stopReason := "end_turn"
	if err != nil {
		stopReason = "error"
	}
	_ = encoder.Finish(stopReason)
	flush()

	// Compute coarse-grained usage; detailed metering comes from meteringEvent
	// frames emitted by Kiro. For billing purposes we surface input/output
	// tokens in the ForwardResult (the handler layer persists the usage log).
	duration := time.Since(start)
	_ = text // currently unused; could be logged for debug

	// Kiro 上游不返回 input_tokens（meteringEvent 只给 credit），这里用请求体
	// 字符数近似估算：Claude tokenizer 平均 ~3.5 字符/token，乘以 1.15 校正。
	// 真实计费以 credit 为准，此数字仅用于 UI 与统计展示。
	inputTokens := int(encoder.InputTokens())
	if inputTokens == 0 {
		inputTokens = estimateKiroInputTokens(parsed)
	}

	// Translate Kiro's tokenUsage cache fields into Anthropic-style names.
	// Kiro:        cacheReadInputTokens  / cacheWriteInputTokens
	// Anthropic:   cache_read_input_tokens / cache_creation_input_tokens
	// The usage log persistence layer reads from ClaudeUsage and writes the
	// Anthropic-named columns, so the frontend's existing cache-hit badges
	// (UsageView.vue) light up automatically once these fields are non-zero.
	result := &ForwardResult{
		RequestID:     kiroFirstNonEmpty(requestID, conversationID),
		Model:         parsed.Model,
		UpstreamModel: upstreamModel,
		Stream:        parsed.Stream,
		Duration:      duration,
		FirstTokenMs:  firstTokenMs,
		Usage: ClaudeUsage{
			InputTokens:              inputTokens,
			OutputTokens:             int(encoder.OutputTokens()),
			CacheReadInputTokens:     int(encoder.CacheReadTokens()),
			CacheCreationInputTokens: int(encoder.CacheWriteTokens()),
		},
		KiroMeteringCredit:  encoder.MeteringCredit(),
		KiroContextUsagePct: encoder.ContextUsagePct(),
	}
	// Apply the prompt-cache simulator (no-op when cacheProfile is nil or the
	// upstream already reported real cache fields). Only run for streams that
	// completed without an error — a partial/aborted stream might have left
	// the conversation truncated, and writing the breakpoints would let the
	// next request "hit" a prefix the model never actually consumed.
	if err == nil {
		s.applyPromptCacheTracking(account, cacheProfile, result)
	}
	return result, nil
}

// estimateKiroInputTokens approximates the prompt token count for a Kiro
// request by summing the character length of the system prompt and every
// message. Conversion constant: 3.5 chars/token * 1.15 Claude correction
// => ~1 token per 3 characters. Used only when Kiro upstream omits usage.
func estimateKiroInputTokens(parsed *ParsedRequest) int {
	if parsed == nil {
		return 0
	}
	var chars int
	chars += countAnyCharacters(parsed.System)
	for _, m := range parsed.Messages {
		chars += countAnyCharacters(m)
	}
	if chars == 0 {
		return 0
	}
	return chars / 3
}

// countAnyCharacters best-effort counts textual characters inside an arbitrary
// JSON-decoded value (string, map, slice, nested). Numbers / bools ignored.
func countAnyCharacters(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []any:
		n := 0
		for _, it := range x {
			n += countAnyCharacters(it)
		}
		return n
	case map[string]any:
		n := 0
		for _, vv := range x {
			n += countAnyCharacters(vv)
		}
		return n
	}
	return 0
}

// clientForAccount returns a client that honours the outbound proxy
// configured on the account. Falls back to the shared client when none.
func (s *KiroGatewayService) clientForAccount(account *Account) *http.Client {
	if account == nil || account.Proxy == nil {
		return s.httpClient
	}
	_, parsed, err := proxyurl.Parse(account.Proxy.URL())
	if err != nil || parsed == nil {
		return s.httpClient
	}
	transport := s.httpClient.Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(parsed)
	return &http.Client{Transport: transport, Timeout: 0}
}

// kiroUpstreamHeaders builds the request headers for the streaming
// GenerateAssistantResponse call. Headers exactly match the real Kiro IDE
// (Electron, aws-sdk-js) request shape: path-style operation in the URL,
// KiroIDE-stamped User-Agent that includes the per-account machine_id, and
// x-amzn-kiro-agent-mode set to "spec" (or "vibe" for IDC accounts). The
// X-Amz-Target / Accept / X-Amzn-Codewhisperer-Optout headers are NOT set
// because Kiro IDE itself omits them on this call — keeping them previously
// is what tagged sub2api traffic as Amazon-Q-CLI aggregator rather than IDE.
func kiroUpstreamHeaders(token string, account *Account) http.Header {
	return kiroIDEStreamingHeaders(token, account)
}

// kiroRESTHeaders builds headers for the RPC-style REST helpers (currently
// ListAvailableModels). These calls still POST to the host root with
// X-Amz-Target to select the operation; only the UA / agent-mode change.
func kiroRESTHeaders(token, target string, account *Account) http.Header {
	h := kiroIDECommonHeaders(token, account)
	h.Set("Content-Type", "application/x-amz-json-1.0")
	h.Set("Accept", "*/*")
	if target != "" {
		h.Set("X-Amz-Target", target)
	}
	// Kiro IDE still asks for opt-out on the model listing path; mirror it.
	h.Set("X-Amzn-Codewhisperer-Optout", "true")
	return h
}

// kiroIDEStreamingHeaders is the path-style /generateAssistantResponse
// header set. Content-Type is plain application/json (matching the IDE's
// fetch call) and the response is consumed as AWS EventStream so no
// Accept header is needed.
func kiroIDEStreamingHeaders(token string, account *Account) http.Header {
	h := kiroIDECommonHeaders(token, account)
	h.Set("Content-Type", "application/json")
	return h
}

// kiroIDECommonHeaders returns the headers every Kiro IDE request shares.
// We expose it as its own helper so REST and streaming paths cannot drift
// out of sync — every change to the device fingerprint must flow through
// here.
func kiroIDECommonHeaders(token string, account *Account) http.Header {
	machineID := stableKiroMachineID(account)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("User-Agent", kiroIDEUserAgent(machineID))
	h.Set("X-Amz-User-Agent", kiroIDEAmzUserAgent(machineID))
	h.Set("X-Amzn-Kiro-Agent-Mode", kiroAgentModeFor(account))
	h.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
	h.Set("Amz-Sdk-Request", "attempt=1; max=3")
	return h
}

// kiroUpstreamEndpointForAccount returns the path-style streaming endpoint
// the real Kiro IDE POSTs to:
//
//	https://q.<region>.amazonaws.com/generateAssistantResponse
//
// Region resolution mirrors the previous behaviour (account credentials
// → profile ARN → default us-east-1) so all account-region tests stay
// valid; only the URL shape changes (operation appended to the path).
func kiroUpstreamEndpointForAccount(account *Account, profileArn string) string {
	return kiroRESTBaseEndpoint(account, profileArn) + kiroStreamingPath
}

// kiroRESTBaseEndpoint returns the host root used by RPC-style helpers
// (ListAvailableModels et al). The trailing slash is part of the canonical
// AWS RPC URL and is required so callers can append a query string or no
// path at all.
func kiroRESTBaseEndpoint(account *Account, profileArn string) string {
	region := ""
	if account != nil {
		region = strings.TrimSpace(account.GetCredential("region"))
	}
	if region == "" {
		region = kiro.RegionFromProfileArn(profileArn)
	}
	if region == "" {
		region = kiro.DefaultMCPRegion
	}
	return fmt.Sprintf(kiroUpstreamHostTemplate, region)
}

func kiroRESTEndpointForAccount(account *Account, profileArn string, params url.Values) string {
	endpoint := kiroRESTBaseEndpoint(account, profileArn)
	if len(params) == 0 {
		return endpoint
	}
	return endpoint + "?" + params.Encode()
}

// ListAvailableModels asks Kiro for the model IDs available to this account.
// It uses the non-streaming AmazonCodeWhispererService target; callers should
// fall back to kiro.DefaultModels if the live probe fails.
func (s *KiroGatewayService) ListAvailableModels(ctx context.Context, account *Account) ([]kiro.Model, error) {
	if s == nil || account == nil {
		return nil, errors.New("kiro models: nil receiver or account")
	}
	token, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("kiro models: token: %w", err)
	}
	profileArn := s.tokenProvider.ProfileArn(account)

	bodyMap := map[string]any{"origin": "KIRO_CLI"}
	query := url.Values{}
	query.Set("origin", "KIRO_CLI")
	if strings.TrimSpace(profileArn) != "" {
		bodyMap["profileArn"] = profileArn
		query.Set("profileArn", profileArn)
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("kiro models: marshal: %w", err)
	}

	modelCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(
		modelCtx,
		http.MethodPost,
		kiroRESTEndpointForAccount(account, profileArn, query),
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("kiro models: build request: %w", err)
	}
	req.Header = kiroRESTHeaders(token, kiroListAvailableModelsAmzTarget, account)

	resp, err := s.clientForAccount(account).Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro models: http do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("kiro models: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kiro models: status %d: %s", resp.StatusCode, string(raw))
	}

	var decoded struct {
		Models []struct {
			ModelID     string `json:"modelId"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("kiro models: decode response: %w", err)
	}

	seen := make(map[string]bool, len(decoded.Models))
	models := make([]kiro.Model, 0, len(decoded.Models))
	for _, item := range decoded.Models {
		id := strings.TrimSpace(item.ModelID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, kiro.ModelFromID(id, strings.TrimSpace(item.DisplayName)))
	}
	if len(models) == 0 {
		return nil, errors.New("kiro models: empty model list")
	}
	return kiro.SortModelsByPreference(models), nil
}

// parsedToKiroAnthropic converts the gateway's internal parsed request into
// the kiro.AnthropicRequest shape consumed by BuildKiroPayload.
func parsedToKiroAnthropic(p *ParsedRequest) (*kiro.AnthropicRequest, error) {
	if p == nil {
		return nil, errors.New("parsed request is nil")
	}
	var body map[string]any
	if err := json.Unmarshal(p.Body, &body); err != nil {
		return nil, fmt.Errorf("unmarshal parsed body: %w", err)
	}

	// Opt-in body dump: when SUB2API_KIRO_DEBUG_DUMP is set the incoming
	// Kiro request body is written to disk as a JSON file for triage.
	// Tool names are not secrets; payload may still contain conversation
	// data so this stays gated by an explicit env flag.
	if os.Getenv("SUB2API_KIRO_DEBUG_DUMP") != "" {
		dumpDir := os.Getenv("SUB2API_KIRO_DEBUG_DUMP_DIR")
		if dumpDir == "" {
			dumpDir = filepath.Join(os.TempDir(), "sub2api-kiro-dumps")
		}
		if err := os.MkdirAll(dumpDir, 0o755); err == nil {
			name := fmt.Sprintf("req-%d.json", time.Now().UnixNano())
			if werr := os.WriteFile(filepath.Join(dumpDir, name), p.Body, 0o600); werr != nil {
				slog.Warn("kiro debug dump failed", "error", werr)
			} else {
				slog.Info("kiro debug dump written",
					"file", filepath.Join(dumpDir, name),
					"body_bytes", len(p.Body),
					"tools_count", gjson.GetBytes(p.Body, "tools.#").Int(),
				)
			}
		}
	}

	req := &kiro.AnthropicRequest{
		Model:  p.Model,
		Stream: p.Stream,
	}
	if raw, ok := body["system"]; ok {
		req.System = raw
	}
	if raw, ok := body["max_tokens"].(float64); ok {
		req.MaxTokens = int(raw)
	}
	// NOTE: Kiro CodeWhisperer does not honour an externally-supplied
	// max_tokens field — its upstream enforces its own tier-based limit.
	// We keep req.MaxTokens around only for downstream thinking-budget
	// calculations (buildThinkingPreamble caps thinking at max_tokens/3
	// so the model keeps headroom for the final answer).
	if raw, ok := body["temperature"].(float64); ok {
		v := raw
		req.Temperature = &v
	}
	if raw, ok := body["top_p"].(float64); ok {
		v := raw
		req.TopP = &v
	}
	if raw, ok := body["thinking"].(map[string]any); ok {
		thinking := &kiro.AnthropicThinking{}
		if t, ok := raw["type"].(string); ok {
			thinking.Type = t
		}
		if b, ok := raw["budget_tokens"].(float64); ok {
			thinking.BudgetTokens = int(b)
		}
		req.Thinking = thinking
		slog.Info("kiro: thinking field parsed",
			"type", thinking.Type, "budget_tokens", thinking.BudgetTokens)
	} else if rawAny, exists := body["thinking"]; exists {
		slog.Info("kiro: thinking field present but not an object",
			"type_go", fmt.Sprintf("%T", rawAny), "value", rawAny)
	}
	if raw, ok := body["messages"].([]any); ok {
		msgs := make([]kiro.AnthropicMessage, 0, len(raw))
		for _, it := range raw {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			contentRaw, _ := json.Marshal(m["content"])
			msgs = append(msgs, kiro.AnthropicMessage{Role: role, Content: contentRaw})
		}
		req.Messages = msgs
	}
	if raw, ok := body["tools"].([]any); ok {
		tools := make([]kiro.AnthropicTool, 0, len(raw))
		for _, it := range raw {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			t := kiro.AnthropicTool{}
			// Preserve the tool discriminator so BuildKiroPayload can filter
			// out Anthropic server-side tools (web_search_*, computer_*, ...)
			// that Kiro CodeWhisperer does not understand.
			if ty, ok := m["type"].(string); ok {
				t.Type = ty
			}
			if n, ok := m["name"].(string); ok {
				t.Name = n
			}
			if d, ok := m["description"].(string); ok {
				t.Description = d
			}
			if sch, ok := m["input_schema"]; ok {
				if schemaRaw, err := json.Marshal(sch); err == nil {
					t.InputSchema = schemaRaw
				}
			}
			tools = append(tools, t)
		}
		req.Tools = tools

		// Tool summary (Debug-level): lets operators confirm what arrived
		// from the client. Safe to log — tool names are not secrets.
		if len(tools) > 0 {
			slog.Debug("kiro gateway: incoming tools",
				"count", len(tools), "tools", summariseTools(tools))
		}
	}
	return req, nil
}

// summariseTools compresses tool metadata into a log-friendly slice. Kept
// small and allocation-free beyond what the caller already built.
func summariseTools(tools []kiro.AnthropicTool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.Name,
			"type":        t.Type,
			"has_schema":  len(t.InputSchema) > 0,
			"schema_size": len(t.InputSchema),
		})
	}
	return out
}

// kiroModelMappingForAccount resolves the effective Anthropic -> Kiro model
// mapping, giving per-account credentials.model_mapping precedence over the
// built-in default. Extra["model_mapping"] is kept as a legacy fallback.
func kiroModelMappingForAccount(account *Account) map[string]string {
	if account == nil {
		return kiro.DefaultModelMapping
	}
	if mapping := account.GetModelMapping(); len(mapping) > 0 {
		return mapping
	}
	if raw, ok := account.Extra["model_mapping"].(map[string]any); ok && len(raw) > 0 {
		out := make(map[string]string, len(raw))
		for k, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				out[k] = s
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return kiro.DefaultModelMapping
}

func cloneKiroModelMapping(mapping map[string]string) map[string]string {
	out := make(map[string]string, len(mapping)+1)
	for k, v := range mapping {
		out[k] = v
	}
	return out
}

// conversationIDFromContext returns a stable conversation id if the handler
// layer already derived one (session hash + group), otherwise the empty
// string (BuildKiroPayload will generate a UUID).
func conversationIDFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get("kiro.conversation_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// ensure url is imported (silence tree-shakers during partial builds).
var _ = url.URL{}

// forwardNonStream collapses the Kiro upstream SSE stream into a single
// Anthropic-format JSON response. It is invoked when the client requested
// stream=false — Claude Code uses that mode for certain tool-follow-up
// turns and reads $.input_tokens / $.content directly from the body.
func (s *KiroGatewayService) forwardNonStream(
	ctx context.Context,
	c *gin.Context,
	upstream io.Reader,
	account *Account,
	parsed *ParsedRequest,
	anthropicReq *kiro.AnthropicRequest,
	profileArn string,
	mapping map[string]string,
	requestID, conversationID, upstreamModel string,
	start time.Time,
	token string,
	client *http.Client,
	kiroEndpoint string,
	cacheProfile *KiroCacheProfile,
) (*ForwardResult, error) {
	builder := kiro.NewAnthropicNonStreamBuilder(parsed.Model)
	var firstTokenMs *int
	builder.SetFirstOutputCallback(func() {
		if firstTokenMs != nil {
			return
		}
		ms := int(time.Since(start).Milliseconds())
		firstTokenMs = &ms
	})

	// Carry over the same input-tokens hint logic the SSE path uses so
	// the JSON response's usage.input_tokens is never 0.
	estimatedInput := estimateKiroInputTokens(parsed)
	if estimatedInput > 0 {
		builder.SetInputTokensHint(int64(estimatedInput))
	}

	// Wire up the same web_search interceptor; it only needs an emit
	// callback and works fine against the non-stream builder because
	// builder.Emit swallows events into in-memory buffers.
	interceptor := newKiroWebSearchInterceptor(
		ctx,
		client,
		token,
		anthropicReq,
		kiro.BuildOptions{
			ProfileArn:     profileArn,
			ModelMapping:   mapping,
			ConversationID: conversationIDFromContext(c),
		},
		kiroEndpoint,
		kiroUpstreamHeaders(token, account),
		s.mcpResultCache,
		account.ID,
		resolveAccountProxyURL(account),
	)

	_, err := kiro.DriveEventStreamToSink(ctx, upstream, builder, interceptor)
	stopReason := "end_turn"
	if err != nil && !errors.Is(err, context.Canceled) {
		stopReason = "error"
	}
	body, ferr := builder.Finish(stopReason)
	if ferr != nil {
		return nil, fmt.Errorf("kiro forward: finalize non-stream body: %w", ferr)
	}
	c.Data(http.StatusOK, "application/json", body)

	duration := time.Since(start)
	inputTokens := int(builder.InputTokens())
	if inputTokens == 0 {
		inputTokens = estimateKiroInputTokens(parsed)
	}
	// Translate Kiro's tokenUsage cache fields into Anthropic-style names —
	// same mapping as the streaming Forward path. Without this the non-stream
	// /v1/messages turns (Claude Code's WebFetch polling etc.) would never
	// light up the cache-hit badges even when the upstream actually served
	// cached tokens.
	result := &ForwardResult{
		RequestID:     kiroFirstNonEmpty(requestID, conversationID),
		Model:         parsed.Model,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      duration,
		FirstTokenMs:  firstTokenMs,
		Usage: ClaudeUsage{
			InputTokens:              inputTokens,
			OutputTokens:             int(builder.OutputTokens()),
			CacheReadInputTokens:     int(builder.CacheReadTokens()),
			CacheCreationInputTokens: int(builder.CacheWriteTokens()),
		},
		KiroMeteringCredit:  builder.MeteringCredit(),
		KiroContextUsagePct: builder.ContextUsagePct(),
	}
	// Apply prompt-cache simulator (mirrors the streaming path). Only when
	// the underlying drive returned without an error — we don't want a
	// truncated/aborted JSON response to pollute the breakpoint store.
	if err == nil {
		s.applyPromptCacheTracking(account, cacheProfile, result)
	}
	return result, nil
}

func kiroFirstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// defaultKiroMaxTokensFloor is the default lower bound we raise client-
// sent max_tokens to before forwarding to Kiro. Claude Code CLI hard-
// codes 8000 in its SDK which collapses to zero room for the final
// answer when combined with a thinking budget. Kiro CodeWhisperer
// comfortably accepts much larger values; 32 000 is a safe compromise
// that leaves space for both extended reasoning and a full response.
const defaultKiroMaxTokensFloor = 32000

// maybeLiftMaxTokensFloor returns max(current, floor) unless the operator
// has opted out via SUB2API_KIRO_MAX_TOKENS_FLOOR. Settings:
//
//   - unset / invalid → use defaultKiroMaxTokensFloor (32 000)
//   - "0"             → disabled; return the client value untouched
//   - positive int    → use that as the floor
//
// Zero or negative input is left alone; we don't invent a floor when the
// client explicitly asked for no limit.
func maybeLiftMaxTokensFloor(current int) int {
	if current <= 0 {
		return current
	}
	floor := defaultKiroMaxTokensFloor
	if raw := strings.TrimSpace(os.Getenv("SUB2API_KIRO_MAX_TOKENS_FLOOR")); raw != "" {
		if parsed, err := parseNonNegativeInt(raw); err == nil {
			if parsed == 0 {
				return current
			}
			floor = parsed
		}
	}
	if current < floor {
		return floor
	}
	return current
}

// parseNonNegativeInt is a tiny strconv-free int parser so this package
// doesn't grow a strconv import for one call-site. Accepts decimal digits
// only.
func parseNonNegativeInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q", c)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// ProbeAccount sends a minimal /generateAssistantResponse request to Kiro
// using the account's current credentials and returns nil iff the upstream
// accepts the call (HTTP 2xx + non-error first frame). It does NOT write
// any response to a client — it is intended for background health checks
// that need to know "is this account actually usable right now?" without
// the side-effect of streaming data to a user.
//
// Used by the temp-unschedulable guards (KiroTokenProvider,
// TokenRefreshService): instead of blindly parking the account on the
// first refresh or request failure we probe here; only a failed probe
// triggers the stall. This fixes the case where a transient request
// error would otherwise sideline an account the user can still use.
func (s *KiroGatewayService) ProbeAccount(ctx context.Context, account *Account) error {
	if s == nil || account == nil {
		return errors.New("kiro probe: nil receiver or account")
	}
	token, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return fmt.Errorf("kiro probe: token: %w", err)
	}
	profileArn := s.tokenProvider.ProfileArn(account)
	mapping := kiroModelMappingForAccount(account)

	// Minimal payload — one-token ask, no tools, no streaming control.
	userContent, _ := json.Marshal([]map[string]any{{"type": "text", "text": "hi"}})
	anthReq := &kiro.AnthropicRequest{
		Model:     kiro.DefaultTestModel,
		MaxTokens: 8,
		Stream:    true,
		Messages:  []kiro.AnthropicMessage{{Role: "user", Content: userContent}},
	}
	payload, err := kiro.BuildKiroPayload(anthReq, kiro.BuildOptions{
		ProfileArn:   profileArn,
		ModelMapping: mapping,
	})
	if err != nil {
		return fmt.Errorf("kiro probe: build payload: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("kiro probe: marshal payload: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(
		probeCtx,
		http.MethodPost,
		kiroUpstreamEndpointForAccount(account, profileArn),
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("kiro probe: build request: %w", err)
	}
	req.Header = kiroUpstreamHeaders(token, account)

	client := s.clientForAccount(account)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("kiro probe: http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("kiro probe: status %d: %s", resp.StatusCode, string(raw))
	}

	// Drain a tiny prefix so the connection returns to the pool cleanly
	// but we don't waste bandwidth reading the full answer.
	_, _ = io.CopyN(io.Discard, resp.Body, 4*1024)
	return nil
}
