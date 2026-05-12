package kiro

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
)

// DefaultModelMapping maps Anthropic model aliases to Kiro internal model IDs.
// Values correspond to modelId values returned by Kiro ListAvailableModels.
//
// Unknown Anthropic names fall through and are forwarded as-is so that a user
// group can map them to Kiro IDs through the existing model_mapping override.
var DefaultModelMapping = map[string]string{
	"auto": "auto",
	// Claude 4.7 family
	"claude-opus-4-7": "claude-opus-4.7",
	"claude-opus-4.7": "claude-opus-4.7",
	// Claude 4.6 family
	"claude-opus-4-6":   "claude-opus-4.6",
	"claude-opus-4.6":   "claude-opus-4.6",
	"claude-sonnet-4-6": "claude-sonnet-4.6",
	"claude-sonnet-4.6": "claude-sonnet-4.6",
	// Claude 4.5 family
	"claude-sonnet-4-5":          "claude-sonnet-4.5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	"claude-haiku-4-5":           "claude-haiku-4.5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4.5",
	"claude-opus-4-5":            "claude-opus-4.5",
	"claude-opus-4-5-20251101":   "claude-opus-4.5",
	// Claude 4 family
	"claude-sonnet-4":          "claude-sonnet-4",
	"claude-sonnet-4-20250514": "claude-sonnet-4",
	// Claude 3.7
	"claude-3-7-sonnet-20250219": "claude-sonnet-4",
	"claude-3-7-sonnet":          "claude-sonnet-4",
	"claude-3.7-sonnet":          "claude-sonnet-4",
	// Explicit passthrough for dotted names used by Kiro directly.
	"claude-sonnet-4.5": "claude-sonnet-4.5",
	"claude-haiku-4.5":  "claude-haiku-4.5",
	"claude-opus-4.5":   "claude-opus-4.5",
	// Non-Claude Kiro models returned by ListAvailableModels.
	"deepseek-3.2":     "deepseek-3.2",
	"minimax-m2.5":     "minimax-m2.5",
	"minimax-m2.1":     "minimax-m2.1",
	"glm-5":            "glm-5",
	"qwen3-coder-next": "qwen3-coder-next",
}

const kiroRequestOrigin = "KIRO_CLI"

// ResolveModel returns the Kiro-internal model id for the given Anthropic
// model name. It first consults accountMapping (per-account override), then
// falls back to DefaultModelMapping, otherwise returns the input unchanged.
func ResolveModel(modelName string, accountMapping map[string]string) string {
	if modelName == "" {
		return "claude-sonnet-4.5"
	}
	if v, ok := resolveModelInMapping(modelName, accountMapping); ok {
		return v
	}
	if v, ok := DefaultModelMapping[modelName]; ok {
		return v
	}
	return modelName
}

func resolveModelInMapping(modelName string, mapping map[string]string) (string, bool) {
	if modelName == "" || len(mapping) == 0 {
		return "", false
	}
	if v, ok := mapping[modelName]; ok && v != "" {
		return v, true
	}
	bestPrefixLen := -1
	bestTarget := ""
	for pattern, target := range mapping {
		if target == "" || !strings.HasSuffix(pattern, "*") {
			continue
		}
		prefix := strings.TrimSuffix(pattern, "*")
		if prefix == "" || !strings.HasPrefix(modelName, prefix) {
			continue
		}
		if len(prefix) > bestPrefixLen {
			bestPrefixLen = len(prefix)
			bestTarget = target
		}
	}
	if bestTarget != "" {
		return bestTarget, true
	}
	return "", false
}

// AnthropicRequest is a minimal shape of the Anthropic /v1/messages body we
// consume. Fields we do not care about are captured in Extra for passthrough.
type AnthropicRequest struct {
	Model       string             `json:"model"`
	System      any                `json:"system,omitempty"`
	Messages    []AnthropicMessage `json:"messages"`
	Tools       []AnthropicTool    `json:"tools,omitempty"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Thinking    *AnthropicThinking `json:"thinking,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
}

// AnthropicThinking represents the {"type":"enabled","budget_tokens":n} block.
type AnthropicThinking struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// AnthropicMessage mirrors the Anthropic Messages API message shape.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// KiroImage represents an image in the Kiro request payload.
type KiroImage struct {
	Format string         `json:"format"`
	Source KiroImageSource `json:"source"`
}

// KiroImageSource holds base64-encoded image bytes.
type KiroImageSource struct {
	Bytes string `json:"bytes"`
}

// AnthropicTool mirrors a single tool in the Anthropic request.
//
// Type distinguishes between user-defined tools (type: "" / "function" / "custom",
// carry an input_schema) and Anthropic server-side tools (type: "web_search_*",
// "computer_*", "text_editor_*", "bash_*", ...). Server-side tools are handled
// by Anthropic infrastructure and carry no JSON schema; Kiro CodeWhisperer does
// not understand them and rejects the request with "Invalid tool parameters"
// when they are forwarded verbatim.
type AnthropicTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// builtinKiroWebSearchToolName is the name used when the gateway rewrites
// an Anthropic server-side web_search_* tool into a regular function tool
// that Kiro CodeWhisperer can actually invoke. The name is chosen to be
// identical to what the Anthropic protocol uses ("web_search") so when the
// response transformer intercepts the tool call and reports it back to
// the client, the client sees the tool name it originally sent.
const builtinKiroWebSearchToolName = "web_search"

// defaultWebSearchDescription is surfaced to the Kiro model when the
// gateway synthesises a tool specification from an Anthropic server-side
// web_search_* entry that had no description of its own.
const defaultWebSearchDescription = "Search the public web. Use this when you need up-to-date information that is outside your training data. The tool accepts a single 'query' argument; respond with a concise query string."

// defaultWebSearchInputSchema is injected when rewriting an Anthropic
// server-side web_search_* tool into a Kiro function tool. Kiro requires a
// concrete JSON Schema on every toolSpecification.
var defaultWebSearchInputSchema = json.RawMessage(
	`{"type":"object","properties":{"query":{"type":"string","description":"Search query"}},"required":["query"],"additionalProperties":false}`,
)

// isAnthropicServerSideWebSearch reports whether the tool entry is the
// Anthropic server-side web_search_* variant that Kiro cannot execute
// natively. When true, the caller rewrites the entry into a function tool
// so the model can still trigger a search, and the gateway fulfils it by
// calling Kiro's /mcp endpoint instead.
func isAnthropicServerSideWebSearch(t AnthropicTool) bool {
	if t.Type == "" {
		// Legacy shape where only the name is meaningful.
		return t.Name == "web_search" && len(t.InputSchema) == 0
	}
	return strings.HasPrefix(t.Type, "web_search")
}

// rewriteToFunctionWebSearch normalises an Anthropic server-side web_search
// tool into a plain function tool that Kiro will invoke.
func rewriteToFunctionWebSearch(t AnthropicTool) AnthropicTool {
	out := t
	out.Type = "function"
	out.Name = builtinKiroWebSearchToolName
	if strings.TrimSpace(out.Description) == "" {
		out.Description = defaultWebSearchDescription
	}
	if len(out.InputSchema) == 0 {
		out.InputSchema = defaultWebSearchInputSchema
	}
	return out
}

// BuildOptions carries per-request knobs for transformers.
type BuildOptions struct {
	// ConversationID reuses an existing Kiro conversation id when stable
	// (empty => generate a new UUID).
	ConversationID string
	// ProfileArn must match the account's profileArn captured at OAuth time.
	ProfileArn string
	// ModelMapping is a per-account override map (anthropic -> kiro model id).
	ModelMapping map[string]string
	// DisablePromptCaching skips automatic cachePoint injection. Default
	// (false) means the gateway emits cachePoint markers on the merged
	// system prompt, the tools array tail, and the last two user turns to
	// maximise Kiro upstream prefix-cache hit rate. Set to true for
	// debugging scenarios where the raw, uncached payload is preferred.
	DisablePromptCaching bool
}

// BuildKiroPayload converts an Anthropic-style request into Kiro's
// conversationState payload shape expected by /generateAssistantResponse.
func BuildKiroPayload(req *AnthropicRequest, opts BuildOptions) (map[string]any, error) {
	if req == nil {
		return nil, fmt.Errorf("anthropic request is nil")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("no messages to send")
	}

	modelID := ResolveModel(req.Model, opts.ModelMapping)
	conversationID := opts.ConversationID
	if conversationID == "" {
		conversationID = uuid.NewString()
	}

	systemPrompt := extractSystemPrompt(req.System)
	// Append the thinking-mode legitimisation block to the system prompt
	// so the model treats the XML tags in the user message as proper
	// system instructions instead of a prompt-injection attempt.
	if isThinkingRequested(req.Thinking) {
		systemPrompt += thinkingSystemPromptAddition()
	}

	// Split messages: history = all but last; current = last.
	msgs := normaliseMessages(req.Messages)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no messages after normalisation")
	}

	// Prepend system prompt to the first user message in history (if any),
	// otherwise merge into the current message later.
	var history []any
	if len(msgs) > 1 {
		historyMsgs := msgs[:len(msgs)-1]
		if systemPrompt != "" {
			for i := range historyMsgs {
				if historyMsgs[i].Role == "user" {
					historyMsgs[i].Text = systemPrompt + "\n\n" + historyMsgs[i].Text
					break
				}
			}
		}
		history = buildHistory(historyMsgs, modelID)
	}

	current := msgs[len(msgs)-1]
	currentText := current.Text
	if systemPrompt != "" && len(history) == 0 {
		currentText = systemPrompt + "\n\n" + currentText
	}

	// Ensure current message ends up as a user role.
	if current.Role == "assistant" {
		history = append(history, map[string]any{
			"assistantResponseMessage": map[string]any{
				"content": currentText,
			},
		})
		currentText = "Continue"
	}
	if strings.TrimSpace(currentText) == "" {
		currentText = "Continue"
	}

	// Inject the thinking-mode preamble into the current user turn when
	// the client asked for extended thinking. The preamble wraps
	// `<thinking_mode>`, `<max_thinking_length>`, and
	// `<thinking_instruction>` XML tags that, together with the
	// matching system-prompt addendum, instruct the Kiro model to
	// produce `<thinking>...</thinking>` blocks at the start of its
	// reply. The response-side ThinkingSplitter then routes those
	// blocks into Anthropic `thinking_delta` events so Claude Code can
	// render them natively.
	// Inject the thinking-mode preamble into the current user turn when
	// the client asked for extended thinking. The preamble makes Kiro
	// produce `<thinking>...</thinking>` blocks; the response-side
	// ThinkingSplitter then routes them into thinking events which the
	// encoder renders as a markdown blockquote — Claude Code's renderer
	// shows that as the familiar left-bar "💭 Thinking" UI even without
	// native thinking-block support.
	if isThinkingRequested(req.Thinking) {
		slog.Info("kiro request: injecting thinking preamble",
			"budget_tokens", req.Thinking.BudgetTokens,
			"type", req.Thinking.Type)
		currentText = buildThinkingPreamble(req.Thinking) + currentText
	}

	// Build userInputMessageContext (tools + toolResults; images are inline).
	userInputContext := map[string]any{}
	if len(req.Tools) > 0 {
		kiroTools := make([]any, 0, len(req.Tools))
		var droppedCount, rewrittenCount int
		for _, t := range req.Tools {
			switch {
			case isAnthropicServerSideWebSearch(t):
				// Rewrite into a plain function tool so Kiro's model will
				// actually invoke it. The response transformer will
				// intercept the tool_use event and fulfil it via Kiro's
				// /mcp endpoint.
				t = rewriteToFunctionWebSearch(t)
				rewrittenCount++
			case t.Type != "" && t.Type != "function" && t.Type != "custom":
				// Drop other Anthropic server-side tools (computer_*,
				// text_editor_*, bash_*, ...). Kiro CodeWhisperer only
				// accepts user-defined toolSpecification entries with a
				// concrete inputSchema; forwarding server-side tools
				// verbatim causes the upstream to respond with "Invalid
				// tool parameters".
				droppedCount++
				continue
			}
			var schema any
			if len(t.InputSchema) > 0 {
				_ = json.Unmarshal(t.InputSchema, &schema)
			}
			spec := map[string]any{
				"name":        t.Name,
				"description": t.Description,
			}
			if schema != nil {
				spec["inputSchema"] = map[string]any{"json": schema}
			}
			kiroTools = append(kiroTools, map[string]any{"toolSpecification": spec})
		}
		if len(kiroTools) > 0 {
			userInputContext["tools"] = kiroTools
		}
		// Log rewrite/drop counts at Debug so normal traffic stays quiet.
		if rewrittenCount > 0 || droppedCount > 0 {
			slog.Debug("kiro request: tool-spec rewrite summary",
				"input_count", len(req.Tools),
				"output_count", len(kiroTools),
				"rewritten_web_search", rewrittenCount,
				"dropped_server_side", droppedCount,
			)
		}
	}

	// Extract images from the last (current) raw message for vision support.
	lastRawMsg := req.Messages[len(req.Messages)-1]
	images := extractImages(lastRawMsg.Content)
	if len(images) > 0 {
		slog.Info("kiro request: forwarding images", "count", len(images))
	}

	// Assemble userInputMessage
	userInputMessage := map[string]any{
		"content": currentText,
		"modelId": modelID,
		"origin":  kiroRequestOrigin,
	}
	if len(images) > 0 {
		userInputMessage["images"] = images
	}
	if len(userInputContext) > 0 {
		userInputMessage["userInputMessageContext"] = userInputContext
	}

	conversationState := map[string]any{
		"chatTriggerType": "MANUAL",
		"conversationId":  conversationID,
		"currentMessage": map[string]any{
			"userInputMessage": userInputMessage,
		},
	}
	if len(history) > 0 {
		conversationState["history"] = history
	}

	payload := map[string]any{
		"conversationState": conversationState,
	}
	if opts.ProfileArn != "" {
		payload["profileArn"] = opts.ProfileArn
	}

	// Inject cachePoint markers so Kiro's CodeWhisperer upstream can
	// re-use the cached prefix across turns. Mirrors the Anthropic
	// gateway's addMessageCacheBreakpoints + applyToolsLastCacheBreakpoint
	// behaviour. Opt-out via BuildOptions.DisablePromptCaching for
	// debugging.
	if !opts.DisablePromptCaching {
		addKiroCachePoints(payload)
	}
	return payload, nil
}

// uniMessage is the minimal per-message shape after we normalise content.
type uniMessage struct {
	Role string
	Text string
}

// normaliseMessages extracts text messages and merges adjacent same-role ones.
// Images are handled separately via extractImages on the current (last) message.
func normaliseMessages(in []AnthropicMessage) []uniMessage {
	out := make([]uniMessage, 0, len(in))
	for _, m := range in {
		text := extractMessageText(m.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		if len(out) > 0 && out[len(out)-1].Role == role {
			out[len(out)-1].Text += "\n" + text
			continue
		}
		out = append(out, uniMessage{Role: role, Text: text})
	}
	// Ensure first message is from user.
	if len(out) > 0 && out[0].Role != "user" {
		out = append([]uniMessage{{Role: "user", Text: ""}}, out...)
	}
	// Ensure alternating user/assistant roles by inserting synthetic assistant "..." between consecutive users.
	norm := make([]uniMessage, 0, len(out))
	for i, m := range out {
		norm = append(norm, m)
		if i < len(out)-1 && out[i+1].Role == m.Role {
			if m.Role == "user" {
				norm = append(norm, uniMessage{Role: "assistant", Text: "..."})
			} else {
				norm = append(norm, uniMessage{Role: "user", Text: "Continue"})
			}
		}
	}
	return norm
}

func buildHistory(msgs []uniMessage, modelID string) []any {
	history := make([]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "user":
			history = append(history, map[string]any{
				"userInputMessage": map[string]any{
					"content": m.Text,
					"modelId": modelID,
					"origin":  kiroRequestOrigin,
				},
			})
		case "assistant":
			history = append(history, map[string]any{
				"assistantResponseMessage": map[string]any{
					"content": m.Text,
				},
			})
		}
	}
	return history
}

func extractSystemPrompt(system any) string {
	if system == nil {
		return ""
	}
	switch s := system.(type) {
	case string:
		return s
	case []any:
		var parts []string
		for _, item := range s {
			if m, ok := item.(map[string]any); ok && m["type"] == "text" {
				if text, ok := m["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := s["text"].(string); ok {
			return text
		}
	}
	// Some callers pass RawMessage.
	if raw, ok := system.(json.RawMessage); ok {
		var asStr string
		if err := json.Unmarshal(raw, &asStr); err == nil {
			return asStr
		}
		var asSlice []map[string]any
		if err := json.Unmarshal(raw, &asSlice); err == nil {
			var parts []string
			for _, m := range asSlice {
				if m["type"] == "text" {
					if text, ok := m["text"].(string); ok && text != "" {
						parts = append(parts, text)
					}
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

// extractImages parses image content blocks from an Anthropic message and
// returns them in Kiro's format ({format, source:{bytes}}).
func extractImages(raw json.RawMessage) []KiroImage {
	if len(raw) == 0 {
		return nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var images []KiroImage
	for _, b := range blocks {
		if b["type"] != "image" {
			continue
		}
		src, ok := b["source"].(map[string]any)
		if !ok {
			continue
		}
		srcType, _ := src["type"].(string)
		switch srcType {
		case "base64":
			mediaType, _ := src["media_type"].(string)
			data, _ := src["data"].(string)
			if data == "" {
				continue
			}
			format := normalizeImageFormat(mediaType)
			if format == "" {
				continue
			}
			images = append(images, KiroImage{
				Format: format,
				Source: KiroImageSource{Bytes: data},
			})
		}
	}
	return images
}

// normalizeImageFormat extracts and normalises the image sub-type from a MIME
// media_type string (e.g. "image/png" -> "png", "image/jpeg" -> "jpeg").
func normalizeImageFormat(mediaType string) string {
	parts := strings.SplitN(mediaType, "/", 2)
	if len(parts) != 2 || parts[0] != "image" {
		return ""
	}
	f := strings.ToLower(parts[1])
	switch f {
	case "jpeg", "jpg":
		return "jpeg"
	case "png", "gif", "webp":
		return f
	}
	return ""
}

func extractMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return asStr
	}
	// Fallback to content blocks array.
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			switch b["type"] {
			case "text":
				if text, ok := b["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			case "tool_use":
				// Minimal MVP: serialise tool_use as text so it's preserved in history.
				if name, _ := b["name"].(string); name != "" {
					input, _ := json.Marshal(b["input"])
					parts = append(parts, fmt.Sprintf("[tool_use %s %s]", name, input))
				}
			case "tool_result":
				if content, ok := b["content"].(string); ok && content != "" {
					parts = append(parts, content)
				} else if arr, ok := b["content"].([]any); ok {
					for _, it := range arr {
						if m, ok := it.(map[string]any); ok {
							if text, ok := m["text"].(string); ok {
								parts = append(parts, text)
							}
						}
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// isThinkingRequested reports whether the caller asked for extended
// reasoning. Anthropic has two established shapes ("enabled" with a
// budget_tokens field, and a newer "adaptive" form without one); both
// are accepted here. An empty/missing Thinking struct returns false.
func isThinkingRequested(th *AnthropicThinking) bool {
	if th == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(th.Type)) {
	case "enabled", "adaptive":
		return true
	case "disabled":
		return false
	}
	// Tolerant fallback: some clients set budget_tokens without the type
	// field. Treat any positive budget as "thinking requested".
	return th.BudgetTokens > 0
}

// thinkingPreambleBudgetDefault is the default token budget for the
// injected thinking preamble when the client did not specify one.
const thinkingPreambleBudgetDefault = 4000

// thinkingPreambleBudgetCap is the absolute ceiling applied to any
// client-supplied budget. Beyond this, spending more tokens on thinking
// tends to starve the actual answer without improving quality.
const thinkingPreambleBudgetCap = 10000

// buildThinkingPreamble returns the instruction prepended to the current
// user turn so the Kiro model emits its reasoning inside
// <thinking>...</thinking> tags. Mirrors the reference kiro-gateway
// implementation: uses explicit XML tags (not bracket-style markers) so
// the accompanying system prompt addendum can legitimise them as
// system-level instructions rather than prompt-injection attempts.
func buildThinkingPreamble(th *AnthropicThinking) string {
	budget := thinkingPreambleBudgetDefault
	if th != nil && th.BudgetTokens > 0 {
		budget = th.BudgetTokens
	}
	if budget > thinkingPreambleBudgetCap {
		budget = thinkingPreambleBudgetCap
	}
	// The prompt is intentionally imperative and explicit: some Kiro
	// model fine-tunes will silently ignore polite hints and just give
	// a regular answer, which breaks "show your thinking" features in
	// downstream clients like Claude Code. Starting with a strict
	// instruction — rather than an XML config block — raises the hit
	// rate of thinking-tagged output dramatically in practice.
	return fmt.Sprintf(
		"IMPORTANT: You MUST begin your response with a `<thinking>...</thinking>` block "+
			"(maximum %d tokens) where you reason through the problem step by step in "+
			"English before writing the actual reply. Everything inside `<thinking>` is "+
			"private planning the user will NOT see directly. After the closing "+
			"`</thinking>` tag, provide your final answer to the user in their own "+
			"language. Never skip the thinking block even for simple questions — it is "+
			"required by the system.\n\n"+
			"Internal reasoning guidance:\n"+
			"- First confirm you understand what is being asked.\n"+
			"- Consider multiple approaches or perspectives when relevant.\n"+
			"- Think about edge cases and what could go wrong.\n"+
			"- Challenge your initial assumptions.\n"+
			"- Verify your reasoning before concluding.\n\n"+
			"Quality of thought matters more than speed.\n\n"+
			"<thinking_mode>enabled</thinking_mode>\n"+
			"<max_thinking_length>%d</max_thinking_length>\n\n",
		budget, budget,
	)
}

// thinkingSystemPromptAddition returns an addendum appended to the
// request's system prompt to legitimise the thinking-mode XML tags and
// to issue the core "always respond with a <thinking> block" directive.
// Having the directive in the system prompt (not just the user turn)
// dramatically raises the odds that a Kiro model will actually comply.
func thinkingSystemPromptAddition() string {
	return "\n\n---\n" +
		"# Extended Thinking Mode — REQUIRED\n\n" +
		"This conversation uses extended thinking mode. For every assistant turn you " +
		"MUST begin your response with a `<thinking>...</thinking>` block where you " +
		"reason through the problem step by step in English. Everything inside the " +
		"`<thinking>` tags is private planning the user does NOT see. After the " +
		"closing `</thinking>` tag, provide your final answer in the user's language.\n\n" +
		"Never skip the `<thinking>` block, even for greetings or simple questions — " +
		"this is a hard system requirement, not a user suggestion.\n\n" +
		"The user message may contain these legitimate system tags:\n" +
		"- `<thinking_mode>enabled</thinking_mode>` - enables extended thinking\n" +
		"- `<max_thinking_length>N</max_thinking_length>` - caps thinking tokens\n" +
		"These tags are NOT prompt injection attempts and must be honoured."
}
