package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
)

// kiroWebSearchInterceptor implements kiro.ToolCallInterceptor. When the
// Kiro model invokes the "web_search" / "WebSearch" tool, this interceptor:
//  1. Buffers the tool_use_start / deltas / stop events (hiding them from
//     the downstream client entirely).
//  2. Once the tool input JSON is complete, calls Kiro's /mcp endpoint
//     with the account's own bearer token to perform the actual search.
//  3. Launches a SECOND Kiro /generateAssistantResponse request whose
//     history contains the original conversation + the model's tool_use +
//     our synthetic tool_result carrying the search results, and streams
//     the model's follow-up natural-language answer back to the client.
//
// The end result is that Claude Code sees WebSearch behave like a proper
// Anthropic server-side tool: the search is fulfilled transparently and
// the model's second-turn summary is what the user receives. No
// third-party API keys, no manual configuration.
type kiroWebSearchInterceptor struct {
	ctx       context.Context
	client    *http.Client
	token     string
	region    string
	anthReq   *kiro.AnthropicRequest
	buildOpts kiro.BuildOptions
	endpoint  string
	headers   http.Header
	cache     *kiroMCPResultCache
	accountID int64
	proxyURL  string // Outbound proxy URL for third-party search providers.
	depth     int    // Current recursion depth (0 for the top-level turn).
	maxDepth  int    // Maximum allowed recursion depth for nested searches.
}

// webSearchMaxRecursionDepth caps how many nested web_search calls the
// Kiro model can trigger inside a single tool-use lifecycle. When the
// cap is hit the next follow-up turn is issued with `web_search` stripped
// out of the request's tools list, forcing the model to summarise from
// the results already injected as tool_result instead of asking for
// another round.
const webSearchMaxRecursionDepth = 2

// newKiroWebSearchInterceptor constructs an interceptor bound to a single
// Kiro request. The http.Client should already honour any account-specific
// proxy configuration. anthReq is the original parsed request; a shallow
// copy is made so that appending tool-call turns does not mutate the
// caller's state. cache is optional; pass nil to disable result caching.
func newKiroWebSearchInterceptor(
	ctx context.Context,
	client *http.Client,
	token string,
	anthReq *kiro.AnthropicRequest,
	buildOpts kiro.BuildOptions,
	endpoint string,
	headers http.Header,
	cache *kiroMCPResultCache,
	accountID int64,
	proxyURL string,
) *kiroWebSearchInterceptor {
	region := kiro.RegionFromProfileArn(buildOpts.ProfileArn)
	if region == "" {
		region = kiro.DefaultMCPRegion
	}
	return &kiroWebSearchInterceptor{
		ctx:       ctx,
		client:    client,
		token:     token,
		region:    region,
		anthReq:   anthReq,
		buildOpts: buildOpts,
		endpoint:  endpoint,
		headers:   headers,
		cache:     cache,
		accountID: accountID,
		proxyURL:  proxyURL,
		depth:     0,
		maxDepth:  webSearchMaxRecursionDepth,
	}
}

// OnToolStart matches Kiro's `web_search` tool invocation. Matching is
// case-insensitive and tolerates `web_search`, `WebSearch`, etc.
func (i *kiroWebSearchInterceptor) OnToolStart(_ context.Context, ev *kiro.StreamEvent) bool {
	if ev == nil {
		return false
	}
	return isKiroWebSearchToolName(ev.ToolName)
}

// isKiroWebSearchToolName recognises the tool-name variants that should be
// fulfilled via Kiro's /mcp endpoint. Matching is case-insensitive.
//
//	"web_search"  → rewritten by the request transformer
//	"WebSearch"   → Claude Code's built-in server-side tool (function-shape)
func isKiroWebSearchToolName(name string) bool {
	switch strings.ToLower(name) {
	case "web_search", "websearch":
		return true
	}
	return false
}

// OnToolStop:
//  1. Parses the reassembled tool input to extract the search query.
//  2. Calls Kiro /mcp to run the search.
//  3. Launches a follow-up /generateAssistantResponse turn whose input
//     includes the tool_use + tool_result, and streams the model's
//     natural-language response back through the provided emit callback.
//
// If any step fails, a short explanatory text event is emitted so the
// client gets *something* instead of silence.
func (i *kiroWebSearchInterceptor) OnToolStop(
	ctx context.Context,
	toolUseID, toolName, input string,
	emit func(*kiro.StreamEvent) error,
) error {
	query := extractQueryFromToolInput(input)
	if query == "" {
		slog.Warn("kiro web_search interceptor: empty query; skipping",
			"tool_use_id", toolUseID, "raw_input", input)
		return emit(&kiro.StreamEvent{
			Kind: "content",
			Text: "\n<web_search>\nNo query provided.\n</web_search>\n",
		})
	}

	slog.Info("kiro web_search interceptor: executing query",
		"tool_use_id", toolUseID, "query", query, "region", i.region)

	var mcpResp *kiro.MCPWebSearchResponse
	var providerLabel string
	if cached := i.cache.Get(ctx, i.accountID, query); cached != nil {
		slog.Info("kiro web_search interceptor: cache hit",
			"tool_use_id", toolUseID, "query", query,
			"results", len(cached.Results))
		mcpResp = cached
		providerLabel = "cache"
	} else {
		// 1) Prefer the configured third-party Provider Manager
		//    (Exa/Serper/Brave/Tavily) — these are typically faster and
		//    have much better Chinese coverage than Kiro's own index.
		if resp, provider, ok := i.tryManagerSearch(ctx, query); ok {
			mcpResp = resp
			providerLabel = provider
			i.cache.Set(ctx, i.accountID, query, mcpResp)
		} else {
			// 2) Fallback to Kiro's /mcp endpoint with the account's own
			//    token (zero-config path).
			resp, err := kiro.CallMCPWebSearch(ctx, i.client, i.region, i.token, query)
			if err != nil {
				slog.Error("kiro web_search interceptor: MCP call failed", "error", err)
				return emit(&kiro.StreamEvent{
					Kind: "content",
					Text: fmt.Sprintf(
						"\n<web_search>\nWeb search failed: %s\n</web_search>\n",
						err.Error(),
					),
				})
			}
			mcpResp = resp
			providerLabel = "kiro_mcp"
			i.cache.Set(ctx, i.accountID, query, mcpResp)
		}
	}

	slog.Info("kiro web_search interceptor: results ready",
		"tool_use_id", toolUseID, "provider", providerLabel,
		"results", len(mcpResp.Results))

	// The MCP payload is our source of truth for the follow-up turn.
	// Serialise it as JSON text so the model can quote it naturally.
	rawResultsJSON, merr := json.Marshal(mcpResp)
	if merr != nil {
		slog.Error("kiro web_search interceptor: marshal MCP response failed", "error", merr)
		return emit(&kiro.StreamEvent{
			Kind: "content",
			Text: kiro.FormatSearchSummary(mcpResp),
		})
	}

	// Ask Kiro to summarise the results. If this follow-up turn fails for
	// any reason we fall back to shipping the raw formatted summary so
	// the user still sees something useful.
	slog.Info("kiro web_search interceptor: launching follow-up summary turn",
		"tool_use_id", toolUseID, "results", len(mcpResp.Results))
	if summarised, err := i.requestFollowUpSummary(ctx, toolUseID, toolName, query, rawResultsJSON, emit); err != nil {
		slog.Warn("kiro web_search interceptor: follow-up summary failed; falling back to raw results",
			"tool_use_id", toolUseID, "error", err)
		if summarised {
			// We already streamed some content to the client; don't
			// duplicate it with the fallback — just signal stop.
			return nil
		}
		return emit(&kiro.StreamEvent{
			Kind: "content",
			Text: kiro.FormatSearchSummary(mcpResp),
		})
	}
	return nil
}

// requestFollowUpSummary builds a new Kiro request that continues the
// assistant turn with a synthetic `tool_result`, sends it through the same
// upstream HTTP path, and streams the response back using the provided
// emit callback.
//
// The first return value indicates whether *any* tokens were already
// streamed to the client. Callers use this to decide whether to emit the
// raw-results fallback on error.
func (i *kiroWebSearchInterceptor) requestFollowUpSummary(
	ctx context.Context,
	toolUseID, toolName, query string,
	resultsJSON []byte,
	emit func(*kiro.StreamEvent) error,
) (bool, error) {
	if i.anthReq == nil {
		return false, errors.New("no original request captured")
	}

	// Clone the original request and append two synthetic messages:
	//   - assistant with a tool_use block (the one the model just emitted)
	//   - user with a tool_result block (our MCP output)
	// Then re-issue it against Kiro. The model has full context and will
	// produce a natural-language summary.
	followUp := *i.anthReq
	followUp.Messages = append([]kiro.AnthropicMessage{}, i.anthReq.Messages...)

	assistantContent := []map[string]any{
		{
			"type":  "tool_use",
			"id":    toolUseID,
			"name":  toolName,
			"input": json.RawMessage(fmt.Sprintf(`{"query":%q}`, query)),
		},
	}
	assistantJSON, err := json.Marshal(assistantContent)
	if err != nil {
		return false, fmt.Errorf("marshal assistant content: %w", err)
	}
	userContent := []map[string]any{
		{
			"type":        "tool_result",
			"tool_use_id": toolUseID,
			"content":     string(resultsJSON),
		},
	}
	userJSON, err := json.Marshal(userContent)
	if err != nil {
		return false, fmt.Errorf("marshal tool result: %w", err)
	}

	followUp.Messages = append(followUp.Messages,
		kiro.AnthropicMessage{Role: "assistant", Content: assistantJSON},
		kiro.AnthropicMessage{Role: "user", Content: userJSON},
	)

	payload, err := kiro.BuildKiroPayload(&followUp, i.buildOpts)
	if err != nil {
		return false, fmt.Errorf("build follow-up payload: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("marshal follow-up payload: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, i.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build follow-up http request: %w", err)
	}
	httpReq.Header = i.headers.Clone()

	resp, err := i.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("follow-up http do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return false, fmt.Errorf("follow-up status %d: %s", resp.StatusCode, string(raw))
	}

	// Stream the follow-up response through the same driver used for the
	// primary turn. This is critical: the driver handles Kiro's wire
	// quirks (e.g. every tool_use frame repeats `name` and would otherwise
	// open multiple content blocks for one tool) and runs content through
	// the thinking splitter, so Claude Code renders tool calls from the
	// second turn (WebFetch, Read, …) as proper tool_use blocks rather
	// than inline text. We plug in a small handler that recursively
	// fulfils any further web_search calls up to maxDepth, and transparently
	// forwards every other tool so Claude Code can execute it.
	handler := &followUpStreamHandler{
		interceptor:   i,
		emit:          emit,
		parentHistory: followUp.Messages,
	}
	if _, err := kiro.DriveFollowUp(ctx, resp.Body, handler); err != nil && !errors.Is(err, context.Canceled) {
		return handler.wroteAny, err
	}
	// After the stream completes, resolve any pending nested web_search
	// calls the model made. Each one spawns its own MCP lookup +
	// follow-up turn, whose output is streamed back to the same emit
	// callback. This keeps the chain going until either the model stops
	// calling web_search or we hit maxDepth.
	if err := handler.resolvePending(ctx); err != nil {
		return handler.wroteAny, err
	}
	return handler.wroteAny, nil
}

// pendingNestedSearch captures the minimum data needed to fulfil a
// nested web_search tool call once its stop frame arrives.
type pendingNestedSearch struct {
	toolUseID  string
	toolName   string
	input      strings.Builder
	atDepthCap bool // true when the nested call would exceed maxDepth
}

// followUpStreamHandler drives a follow-up Kiro stream, forwarding
// content/tool events to the caller's emit callback while buffering any
// nested web_search tool-use lifecycle so it can be resolved after the
// primary follow-up stream finishes.
type followUpStreamHandler struct {
	interceptor   *kiroWebSearchInterceptor
	emit          func(*kiro.StreamEvent) error
	parentHistory []kiro.AnthropicMessage
	wroteAny      bool
	pending       map[string]*pendingNestedSearch
}

// Emit is the FollowUpEmitter hook. Recursive web_search calls are kept
// inside this handler; everything else passes through to the client.
func (h *followUpStreamHandler) Emit(ev *kiro.StreamEvent) error {
	if ev == nil {
		return nil
	}
	switch ev.Kind {
	case "tool_use_start":
		if isKiroWebSearchToolName(ev.ToolName) {
			if h.pending == nil {
				h.pending = make(map[string]*pendingNestedSearch)
			}
			// Flag whether this nested call has room for another
			// follow-up turn. When it doesn't, we still run the
			// search — but ship the raw result back as plain text
			// (see resolvePending) instead of spawning another Kiro
			// /generateAssistantResponse round so we don't burn
			// credits in a loop.
			atCap := h.interceptor.depth+1 >= h.interceptor.maxDepth
			h.pending[ev.ToolUseID] = &pendingNestedSearch{
				toolUseID: ev.ToolUseID,
				toolName:  ev.ToolName,
				atDepthCap: atCap,
			}
			if ev.ToolInput != "" {
				h.pending[ev.ToolUseID].input.WriteString(ev.ToolInput)
			}
			slog.Info("kiro web_search interceptor: buffering nested web_search",
				"tool_use_id", ev.ToolUseID,
				"depth", h.interceptor.depth+1,
				"at_cap", atCap)
			return nil
		}
		// Pass-through tool — forward to the client.
		h.wroteAny = true
		return h.emit(ev)
	case "tool_use_delta":
		if p, ok := h.pending[ev.ToolUseID]; ok {
			p.input.WriteString(ev.ToolDelta)
			return nil
		}
		return h.emit(ev)
	case "tool_use_stop":
		if _, ok := h.pending[ev.ToolUseID]; ok {
			// Keep the buffer alive; resolvePending consumes it.
			return nil
		}
		return h.emit(ev)
	case "content":
		h.wroteAny = true
		return h.emit(ev)
	case "usage", "metering", "context_usage":
		return h.emit(ev)
	}
	// Unknown event kinds: forward as-is to be safe.
	return h.emit(ev)
}

// resolvePending runs each buffered nested web_search call, stitches the
// result into a new Kiro follow-up turn, and streams that turn's output
// through this same handler recursively.
func (h *followUpStreamHandler) resolvePending(ctx context.Context) error {
	if len(h.pending) == 0 {
		return nil
	}
	// Deterministic resolution order based on insertion into the map is
	// not guaranteed in Go; we don't need strict ordering because each
	// search uses its own tool_use_id, but resolve them sequentially to
	// keep log output readable.
	for id, p := range h.pending {
		delete(h.pending, id)
		query := extractQueryFromToolInput(p.input.String())
		if query == "" {
			slog.Warn("kiro web_search interceptor: nested call has empty query; skipping",
				"tool_use_id", id)
			continue
		}

		// Run the search via the same manager/MCP fallback as the top-level.
		mcpResp, providerLabel := h.interceptor.runSearch(ctx, query)
		if mcpResp == nil {
			h.wroteAny = true
			if err := h.emit(&kiro.StreamEvent{
				Kind: "content",
				Text: "\n<web_search>\n(Nested search failed.)\n</web_search>\n",
			}); err != nil {
				return err
			}
			continue
		}
		slog.Info("kiro web_search interceptor: nested search resolved",
			"tool_use_id", id, "provider", providerLabel,
			"results", len(mcpResp.Results),
			"depth", h.interceptor.depth+1,
			"at_cap", p.atDepthCap)

		rawResultsJSON, merr := json.Marshal(mcpResp)
		if merr != nil {
			h.wroteAny = true
			if err := h.emit(&kiro.StreamEvent{
				Kind: "content",
				Text: kiro.FormatSearchSummary(mcpResp),
			}); err != nil {
				return err
			}
			continue
		}

		// Build the history for the nested follow-up: prior history +
		// our assistant/user tool pair for this nested call.
		nestedHistory := append([]kiro.AnthropicMessage{}, h.parentHistory...)

		assistantContent := []map[string]any{
			{
				"type":  "tool_use",
				"id":    id,
				"name":  p.toolName,
				"input": json.RawMessage(fmt.Sprintf(`{"query":%q}`, query)),
			},
		}
		assistantJSON, err := json.Marshal(assistantContent)
		if err != nil {
			return fmt.Errorf("marshal nested assistant content: %w", err)
		}
		userContent := []map[string]any{
			{
				"type":        "tool_result",
				"tool_use_id": id,
				"content":     string(rawResultsJSON),
			},
		}
		userJSON, err := json.Marshal(userContent)
		if err != nil {
			return fmt.Errorf("marshal nested tool result: %w", err)
		}
		nestedHistory = append(nestedHistory,
			kiro.AnthropicMessage{Role: "assistant", Content: assistantJSON},
			kiro.AnthropicMessage{Role: "user", Content: userJSON},
		)

		// Increment depth for the nested stream.
		childInterceptor := *h.interceptor
		childInterceptor.depth = h.interceptor.depth + 1

		nestedReq := *h.interceptor.anthReq
		nestedReq.Messages = nestedHistory
		// When the nested call is at the recursion cap, strip the
		// `web_search` / `WebSearch` tool from the request so the
		// model cannot ask for another round. It will be forced to
		// summarise from the tool_result we just injected.
		if p.atDepthCap {
			nestedReq.Tools = filterOutWebSearchTools(h.interceptor.anthReq.Tools)
			slog.Info("kiro web_search interceptor: at cap, forcing summary (web_search removed from tools)",
				"tool_use_id", id,
				"tools_before", len(h.interceptor.anthReq.Tools),
				"tools_after", len(nestedReq.Tools))
		}
		wrote, err := childInterceptor.streamFollowUp(ctx, &nestedReq, h.emit)
		if wrote {
			h.wroteAny = true
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// runSearch is a shared helper that returns an MCP-shaped response for the
// given query, preferring the third-party Manager and falling back to
// Kiro's /mcp endpoint.
func (i *kiroWebSearchInterceptor) runSearch(ctx context.Context, query string) (*kiro.MCPWebSearchResponse, string) {
	if cached := i.cache.Get(ctx, i.accountID, query); cached != nil {
		return cached, "cache"
	}
	if resp, provider, ok := i.tryManagerSearch(ctx, query); ok {
		i.cache.Set(ctx, i.accountID, query, resp)
		return resp, provider
	}
	resp, err := kiro.CallMCPWebSearch(ctx, i.client, i.region, i.token, query)
	if err != nil {
		slog.Error("kiro web_search interceptor: search failed", "error", err)
		return nil, ""
	}
	i.cache.Set(ctx, i.accountID, query, resp)
	return resp, "kiro_mcp"
}

// streamFollowUp issues a Kiro request with the provided history and
// streams the result through the same follow-up handler (supporting
// further nested searches). Returns whether any event was emitted.
func (i *kiroWebSearchInterceptor) streamFollowUp(
	ctx context.Context,
	req *kiro.AnthropicRequest,
	emit func(*kiro.StreamEvent) error,
) (bool, error) {
	payload, err := kiro.BuildKiroPayload(req, i.buildOpts)
	if err != nil {
		return false, fmt.Errorf("build nested payload: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("marshal nested payload: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, i.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build nested http request: %w", err)
	}
	httpReq.Header = i.headers.Clone()
	resp, err := i.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("nested http do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return false, fmt.Errorf("nested status %d: %s", resp.StatusCode, string(raw))
	}
	handler := &followUpStreamHandler{
		interceptor:   i,
		emit:          emit,
		parentHistory: req.Messages,
	}
	if _, err := kiro.DriveFollowUp(ctx, resp.Body, handler); err != nil && !errors.Is(err, context.Canceled) {
		return handler.wroteAny, err
	}
	if err := handler.resolvePending(ctx); err != nil {
		return handler.wroteAny, err
	}
	return handler.wroteAny, nil
}

// extractQueryFromToolInput parses the aggregated partial_json string that
// the Kiro model emits for the web_search tool and returns the "query"
// field. Returns an empty string if the JSON is malformed or the field is
// missing.
func extractQueryFromToolInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	if q, ok := m["query"].(string); ok {
		return strings.TrimSpace(q)
	}
	return ""
}

// tryManagerSearch attempts to fulfil the web_search tool call using the
// configured third-party search Manager (Brave/Tavily/Exa/Serper) before
// falling back to Kiro's own /mcp endpoint. Returns ok=false when no
// Manager is configured or every provider failed — callers should treat
// that as a signal to try the fallback path.
//
// The Manager's SearchResponse is adapted into kiro.MCPWebSearchResponse
// so the rest of the interceptor pipeline (cache, follow-up summary turn,
// FormatSearchSummary fallback) stays unchanged.
func (i *kiroWebSearchInterceptor) tryManagerSearch(
	ctx context.Context,
	query string,
) (*kiro.MCPWebSearchResponse, string, bool) {
	mgr := getWebSearchManager()
	if mgr == nil {
		return nil, "", false
	}
	resp, provider, err := mgr.SearchWithBestProvider(ctx, websearch.SearchRequest{
		Query:      query,
		MaxResults: webSearchDefaultMaxResults,
		ProxyURL:   i.proxyURL,
	})
	if err != nil {
		// Proxy unavailability is surfaced so the gateway can switch
		// accounts; for any other manager-level error we simply fall
		// through to Kiro's /mcp endpoint.
		if errors.Is(err, websearch.ErrProxyUnavailable) {
			slog.Warn("kiro web_search interceptor: manager proxy unavailable; falling back to kiro /mcp",
				"error", err)
			return nil, "", false
		}
		slog.Warn("kiro web_search interceptor: manager search failed; falling back to kiro /mcp",
			"error", err)
		return nil, "", false
	}
	return adaptWebSearchToMCP(query, resp), provider, true
}

// adaptWebSearchToMCP converts a generic search response into Kiro's MCP
// response shape. The only non-trivial field is publishedDate, which Kiro
// encodes as Unix milliseconds. We make a best-effort to parse the
// provider's human-readable timestamp and drop it on parse failure so
// FormatSearchSummary simply skips the line.
func adaptWebSearchToMCP(query string, resp *websearch.SearchResponse) *kiro.MCPWebSearchResponse {
	out := &kiro.MCPWebSearchResponse{
		Query:        query,
		TotalResults: len(resp.Results),
		Results:      make([]kiro.MCPWebSearchResult, 0, len(resp.Results)),
	}
	for _, r := range resp.Results {
		out.Results = append(out.Results, kiro.MCPWebSearchResult{
			Title:         r.Title,
			URL:           r.URL,
			Snippet:       r.Snippet,
			PublishedDate: parsePageAgeToUnixMilli(r.PageAge),
		})
	}
	return out
}

// parsePageAgeToUnixMilli best-effort converts a provider-supplied age
// string into Unix milliseconds. Supports:
//   - ISO-8601 / RFC3339
//   - "YYYY-MM-DD"
//   - empty → 0 (skipped in the UI)
func parsePageAgeToUnixMilli(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().UnixMilli()
		}
	}
	return 0
}


// filterOutWebSearchTools returns a copy of tools with any
// web_search / WebSearch entries removed. Used to force the model into
// summarising the already-fetched search results instead of recursing
// into yet another MCP lookup once we hit the depth cap.
func filterOutWebSearchTools(tools []kiro.AnthropicTool) []kiro.AnthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]kiro.AnthropicTool, 0, len(tools))
	for _, t := range tools {
		if isKiroWebSearchToolName(t.Name) {
			continue
		}
		out = append(out, t)
	}
	return out
}
