package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// thinkingAsText controls whether thinking content is rendered as a
// markdown blockquote (the default) versus native Anthropic
// `thinking_delta` SSE events.
//
// Default: ON. Claude Code CLI with sk- API key auth does not render
// native thinking blocks, so we ship the content as a blockquote — the
// client's markdown renderer turns it into the familiar left-bar
// "💭 Thinking" UI that the user wanted. Set
// `SUB2API_KIRO_THINKING_NATIVE=1` to opt back into native thinking
// events (useful when the client is Claude Desktop or a custom one
// that actually renders them).
var thinkingAsText = os.Getenv("SUB2API_KIRO_THINKING_NATIVE") == ""

// AssistantEventPayload represents the JSON body of an "assistantResponseEvent"
// frame emitted by /generateAssistantResponse.
type AssistantEventPayload struct {
	Content string `json:"content,omitempty"`
}

// ToolUseEventPayload is emitted when the model decides to call a tool.
// Input 字段�?Kiro 上游可能是：
//   - string（partial JSON 片段，多帧追加后拼成完整 JSON 对象�?
//   - object（完整的参数对象，一帧到位）
//
// 参�?kiro-gateway 开源实现：两种形式都要支持�?
type ToolUseEventPayload struct {
	ToolUseID string `json:"toolUseId,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	Stop      bool   `json:"stop,omitempty"`
}

// toolInputAsPartialJSON �?toolUseEvent.input 统一转成客户端期望的
// partial_json 片段形式（纯 JSON 文本，不带外层引号）�?
//
//   - 空�?�?返回 ""
//   - string（已�?partial JSON，例�?`{"query": "hi`）→ 原样返回
//   - object / array / number / bool �?json.Marshal 后返�?
func toolInputAsPartialJSON(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// UsageEventPayload carries Kiro-side token accounting.
//
// Kiro emits two different shapes depending on the event type:
//
//  1. `usageEvent` (and some older Kiro models): flat top-level fields
//     `{inputTokens, outputTokens, cacheReadInputTokens, cacheWriteInputTokens}`.
//
//  2. `messageMetadataEvent` (the dominant shape on modern Claude-backed
//     Kiro deployments): nested under `tokenUsage`, with `uncachedInputTokens`
//     reported separately from cache reads / writes:
//
//     {
//       "tokenUsage": {
//         "uncachedInputTokens": 1000,
//         "cacheReadInputTokens": 500,
//         "cacheWriteInputTokens": 200,
//         "outputTokens": 100,
//         "totalTokens": 1800,
//         "contextUsagePercentage": 45.5
//       }
//     }
//
// We support both. After json.Unmarshal callers should invoke Flatten()
// to lift the nested TokenUsage fields into the top-level slots so the
// encoder code path is uniform. The mapping follows Anthropic's
// accounting convention: input_tokens excludes cached portions, with
// cache reads / writes reported separately.
type UsageEventPayload struct {
	InputTokens      int64           `json:"inputTokens,omitempty"`
	OutputTokens     int64           `json:"outputTokens,omitempty"`
	CacheReadTokens  int64           `json:"cacheReadInputTokens,omitempty"`
	CacheWriteTokens int64           `json:"cacheWriteInputTokens,omitempty"`
	TokenUsage       *KiroTokenUsage `json:"tokenUsage,omitempty"`
}

// KiroTokenUsage mirrors the nested tokenUsage block Kiro emits inside
// messageMetadataEvent. Field names match the upstream JSON exactly so
// json.Unmarshal can populate them directly. Mirrors the structure
// documented in Kiro-account-manager's src/main/proxy/kiroApi.ts.
type KiroTokenUsage struct {
	// UncachedInputTokens is the portion of the prompt that was NOT served
	// from the prefix cache. This is what Anthropic calls `input_tokens`.
	UncachedInputTokens int64 `json:"uncachedInputTokens,omitempty"`
	// InputTokens is an alternate name some Kiro builds use instead of
	// UncachedInputTokens. We treat it as a fallback when UncachedInputTokens
	// is absent.
	InputTokens int64 `json:"inputTokens,omitempty"`
	// OutputTokens is the assistant-generated token count.
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// CacheReadInputTokens is the portion of the prompt that hit the prefix
	// cache (charged at the cheaper read rate). Maps to Anthropic's
	// cache_read_input_tokens.
	CacheReadInputTokens int64 `json:"cacheReadInputTokens,omitempty"`
	// CacheWriteInputTokens is the portion newly written into the prefix
	// cache. Maps to Anthropic's cache_creation_input_tokens.
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens,omitempty"`
	// TotalTokens is the upstream-reported sum. Useful as a sanity check
	// or as a fallback when the per-category fields are missing.
	TotalTokens int64 `json:"totalTokens,omitempty"`
	// ContextUsagePercentage is duplicated here (the dedicated
	// contextUsageEvent carries the same value). We keep the field so
	// json.Unmarshal does not fail on strict decoders, but the encoder
	// reads it from the dedicated event.
	ContextUsagePercentage float64 `json:"contextUsagePercentage,omitempty"`
}

// Flatten lifts the nested TokenUsage fields into the top-level slots so
// downstream code (the SSE encoder / non-stream builder) can work with a
// single uniform shape. Top-level values reported by the upstream take
// precedence; nested values are only consulted when the corresponding
// top-level slot is still zero.
//
// Anthropic convention: input_tokens excludes cached portions. Kiro's
// upstream reports the uncached portion as `uncachedInputTokens`, so we
// map that onto InputTokens here.
func (p *UsageEventPayload) Flatten() {
	if p == nil || p.TokenUsage == nil {
		return
	}
	tu := p.TokenUsage
	if p.InputTokens == 0 {
		switch {
		case tu.UncachedInputTokens > 0:
			p.InputTokens = tu.UncachedInputTokens
		case tu.InputTokens > 0:
			p.InputTokens = tu.InputTokens
		}
	}
	if p.OutputTokens == 0 && tu.OutputTokens > 0 {
		p.OutputTokens = tu.OutputTokens
	}
	if p.CacheReadTokens == 0 && tu.CacheReadInputTokens > 0 {
		p.CacheReadTokens = tu.CacheReadInputTokens
	}
	if p.CacheWriteTokens == 0 && tu.CacheWriteInputTokens > 0 {
		p.CacheWriteTokens = tu.CacheWriteInputTokens
	}
}

// MeteringEventPayload carries the credit-level billing signal that Kiro
// emits at the end of every assistant turn. Example frame:
//
//	{"unit":"credit","unitPlural":"credits","usage":0.01077093}
type MeteringEventPayload struct {
	Unit        string  `json:"unit,omitempty"`
	UnitPlural  string  `json:"unitPlural,omitempty"`
	Usage       float64 `json:"usage,omitempty"`
	ServiceTier string  `json:"serviceTier,omitempty"`
}

// ContextUsageEventPayload reports how much of the model's context window
// has been consumed so far (percentage).
type ContextUsageEventPayload struct {
	Percentage float64 `json:"contextUsagePercentage,omitempty"`
}

// StreamEvent is the canonical shape emitted by our EventStream -> internal
// event pipeline. The handler layer converts it to Anthropic SSE framing.
type StreamEvent struct {
	// Kind is one of: content, thinking, tool_use_start, tool_use_delta,
	// tool_use_stop, usage, metering, context_usage, error, done
	Kind string
	// Text for content events; Thinking payload for thinking events.
	Text string
	// Tool use fields
	ToolName   string
	ToolUseID  string
	ToolInput  string // partial JSON fragment emitted together with tool_use_start
	ToolDelta  string // partial JSON for tool_use_delta
	StopReason string
	// Usage stats for usage event
	Usage UsageEventPayload
	// Metering (credit-based billing signal)
	Metering MeteringEventPayload
	// ContextUsagePct for context_usage event
	ContextUsagePct float64
	// Error details for error event
	ErrorMessage string
	ErrorType    string
}

// ParseEventStreamFrame attempts to read one logical StreamEvent from a
// decoded EventStream message. It returns (nil, nil) if the frame carried
// nothing interesting (e.g. unknown event type we want to swallow).
func ParseEventStreamFrame(msg *EventStreamMessage) (*StreamEvent, error) {
	if msg == nil {
		return nil, nil
	}
	if msg.MessageType() == "exception" || msg.MessageType() == "error" {
		errMsg := msg.StringValue(":exception-type")
		body := string(msg.Payload)
		if decoded, err := DecodeCBOR(msg.Payload); err == nil {
			if m, ok := decoded.(map[string]any); ok {
				if s := AsString(m, "message"); s != "" {
					body = s
				}
			}
		}
		return &StreamEvent{
			Kind:         "error",
			ErrorType:    errMsg,
			ErrorMessage: body,
		}, nil
	}

	switch msg.EventType() {
	case "assistantResponseEvent":
		var p AssistantEventPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return nil, fmt.Errorf("assistantResponseEvent json: %w", err)
		}
		if p.Content == "" {
			return nil, nil
		}
		return &StreamEvent{Kind: "content", Text: p.Content}, nil

	case "toolUseEvent":
		var p ToolUseEventPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return nil, fmt.Errorf("toolUseEvent json: %w", err)
		}
		if p.Stop {
			return &StreamEvent{Kind: "tool_use_stop", ToolUseID: p.ToolUseID}, nil
		}
		inputFragment := toolInputAsPartialJSON(p.Input)
		if p.Name != "" {
			return &StreamEvent{
				Kind:      "tool_use_start",
				ToolName:  p.Name,
				ToolUseID: p.ToolUseID,
				ToolInput: inputFragment,
			}, nil
		}
		if inputFragment != "" {
			return &StreamEvent{Kind: "tool_use_delta", ToolUseID: p.ToolUseID, ToolDelta: inputFragment}, nil
		}
		return nil, nil

	case "messageMetadataEvent", "followupPromptEvent", "codeReferenceEvent", "supplementaryWebLinksEvent":
		// messageMetadataEvent 在部�?Kiro 响应里携�?input/output token 计数�?
		// 尝试�?usage 负载解析；失败或没有字段时直接忽略该帧�?
		if msg.EventType() == "messageMetadataEvent" && len(msg.Payload) > 0 {
			var p UsageEventPayload
			if err := json.Unmarshal(msg.Payload, &p); err == nil {
				// Kiro's messageMetadataEvent nests its counters inside a
				// `tokenUsage` sub-object; Flatten lifts them to the top.
				p.Flatten()
				if p.InputTokens > 0 || p.OutputTokens > 0 || p.CacheReadTokens > 0 || p.CacheWriteTokens > 0 {
					return &StreamEvent{Kind: "usage", Usage: p}, nil
				}
			}
		}
		return nil, nil

	case "usageEvent":
		var p UsageEventPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return nil, fmt.Errorf("usageEvent json: %w", err)
		}
		// Some Kiro builds also nest tokenUsage inside usageEvent; Flatten
		// is a no-op when the upstream used the flat top-level shape.
		p.Flatten()
		return &StreamEvent{Kind: "usage", Usage: p}, nil

	case "meteringEvent":
		var p MeteringEventPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return nil, fmt.Errorf("meteringEvent json: %w", err)
		}
		return &StreamEvent{Kind: "metering", Metering: p}, nil

	case "contextUsageEvent":
		var p ContextUsageEventPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return nil, fmt.Errorf("contextUsageEvent json: %w", err)
		}
		return &StreamEvent{Kind: "context_usage", ContextUsagePct: p.Percentage}, nil
	}
	return nil, nil
}

// AnthropicSSEEncoder converts a stream of StreamEvents into the Anthropic
// /v1/messages Server-Sent Events wire format:
//
//	event: message_start\ndata: {...}\n\n
//	event: content_block_start\ndata: {...}\n\n
//	event: content_block_delta\ndata: {...}\n\n
//	event: content_block_stop\ndata: {...}\n\n
//	event: message_delta\ndata: {...}\n\n
//	event: message_stop\ndata: {...}\n\n
type AnthropicSSEEncoder struct {
	w             io.Writer
	flush         func()
	model         string
	messageID     string
	textBlockOpen bool
	// thinkingBlockOpen tracks whether a thinking content_block is
	// currently active. Mutually exclusive with textBlockOpen �?switching
	// between thinking and text closes the other block first.
	thinkingBlockOpen bool
	// thinkingFallbackOpen tracks whether we have emitted the opening
	// "> **💭 Thinking**" header in thinkingAsText mode. Used by the
	// content branch to insert a clean newline separator before regular
	// text starts, so Claude Code's markdown renderer flushes the
	// blockquote early and the user sees thinking → pause → answer
	// rather than thinking and answer appearing together.
	thinkingFallbackOpen bool
	// thinkingBuf accumulates the raw text of the current thinking block
	// so closeCurrentNonToolBlock can emit a stable synthetic
	// signature_delta. Claude Code's UI silently hides thinking blocks
	// that arrive without a signature, so we derive one deterministically
	// from the thinking text itself.
	thinkingBuf strings.Builder
	blockIndex  int
	toolBlocks  map[string]int // toolUseID -> block index
	// pendingTools buffers tool_use lifecycle until tool_use_stop arrives
	// (or the stream ends). Kiro's upstream frequently sends object-style
	// input in every frame (not partial JSON strings), so we cannot just
	// forward each frame to the client �?we would end up with
	// `{...}{...}{...}` which Claude Code rejects. Instead we aggregate
	// the canonical final input per tool and emit a single
	// start/delta/stop triple when the tool finishes.
	pendingTools map[string]*pendingToolBlock
	// toolOrder preserves tool_use_start order so Finish() can close
	// them in the same sequence they were opened.
	toolOrder       []string
	inputTokens     int64
	outputTokens    int64
	// cacheReadTokens / cacheWriteTokens mirror Kiro's tokenUsage.cacheReadInputTokens
	// and tokenUsage.cacheWriteInputTokens so the service layer can persist them
	// onto the usage log (and the SSE message_delta surfaces them to clients).
	cacheReadTokens  int64
	cacheWriteTokens int64
	meteringCredit   float64
	meteringUnit    string
	contextUsagePct float64
	started         bool
	outputStarted   bool
	onFirstOutput   func()
}

// pendingToolBlock holds the aggregated state for a single tool_use
// lifecycle before its block is flushed to the wire.
type pendingToolBlock struct {
	toolName string
	// input accumulates partial_json fragments (string-style input) or
	// records the latest complete JSON (object-style input). finalInput
	// supersedes it when a full object was seen.
	input      strings.Builder
	finalInput string
	inputSeen  bool
}

// NewAnthropicSSEEncoder builds an encoder. The flusher is called after each
// event so clients receive chunks in real time. inputTokensHint is an
// optional pre-computed estimate of the prompt token count; when > 0 it is
// surfaced in the message_start usage block so downstream consumers (e.g. a
// second sub2api instance acting as a relay) can record it without waiting
// for the message_delta at the end of the stream.
func NewAnthropicSSEEncoder(w io.Writer, flush func(), model string) *AnthropicSSEEncoder {
	return &AnthropicSSEEncoder{
		w:            w,
		flush:        flush,
		model:        model,
		messageID:    "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
		toolBlocks:   make(map[string]int),
		pendingTools: make(map[string]*pendingToolBlock),
	}
}

// SetInputTokensHint pre-populates the input token count that will be
// emitted in message_start. Call this before the first Emit/Start so the
// hint is visible to relay consumers from the very first SSE frame.
func (e *AnthropicSSEEncoder) SetInputTokensHint(n int64) {
	if n > 0 && e.inputTokens == 0 {
		e.inputTokens = n
	}
}

// SetFirstOutputCallback registers a hook fired when the first assistant
// output block/delta is emitted. It intentionally ignores message_start and
// usage/metering frames so callers can record time-to-first-token.
func (e *AnthropicSSEEncoder) SetFirstOutputCallback(cb func()) {
	e.onFirstOutput = cb
}

func (e *AnthropicSSEEncoder) markFirstOutput() {
	if e.outputStarted {
		return
	}
	e.outputStarted = true
	if e.onFirstOutput != nil {
		e.onFirstOutput()
	}
}

func (e *AnthropicSSEEncoder) write(event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, body); err != nil {
		return err
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}

// Start emits the message_start event.
func (e *AnthropicSSEEncoder) Start() error {
	if e.started {
		return nil
	}
	e.started = true
	msg := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            e.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         e.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  e.inputTokens,
				"output_tokens": 0,
			},
		},
	}
	return e.write("message_start", msg)
}

// mapToThinkingCapableModel is retained for completeness but unused
// while the thinking-injection path is disabled. When Kiro exposes a
// real thinking API (or Claude Code lifts the sk-auth rendering gate)
// we can re-enable it inside Start() above.
func mapToThinkingCapableModel(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "opus-4-1"):
		return "claude-opus-4-1-20250805"
	case strings.Contains(lower, "opus"):
		return "claude-opus-4-1-20250805"
	case strings.Contains(lower, "sonnet"):
		return "claude-sonnet-4-5-20250929"
	case strings.Contains(lower, "haiku"):
		return "claude-haiku-4-5-20251001"
	}
	return name
}

// Emit forwards a single internal event.
func (e *AnthropicSSEEncoder) Emit(ev *StreamEvent) error {
	if err := e.Start(); err != nil {
		return err
	}
	if ev == nil {
		return nil
	}
	switch ev.Kind {
	case "content":
		if e.thinkingBlockOpen {
			if err := e.closeCurrentNonToolBlock(); err != nil {
				return err
			}
		}
		if err := e.closeOpenToolBlocks(); err != nil {
			return err
		}
		// When coming off a thinking-as-text blockquote, close the
		// current text block and start a fresh one so Claude Code's
		// TUI is forced to repaint: keeping thinking + answer in the
		// same block defers the entire render to stream-end.
		if e.thinkingFallbackOpen {
			e.thinkingFallbackOpen = false
			e.thinkingBuf.Reset()
			if e.textBlockOpen {
				if err := e.write("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": e.blockIndex,
				}); err != nil {
					return err
				}
				e.textBlockOpen = false
				e.blockIndex++
			}
			if e.flush != nil {
				e.flush()
			}
		}
		if !e.textBlockOpen {
			e.textBlockOpen = true
			if err := e.write("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": e.blockIndex,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			}); err != nil {
				return err
			}
		}
		if err := e.emitTextDeltaByRune(ev.Text); err != nil {
			return err
		}
		e.outputTokens += int64(len(ev.Text) / 4) // rough
		return nil

	case "thinking":
		// When SUB2API_KIRO_THINKING_AS_TEXT is set, we surface thinking
		// content as a regular text block prefixed with a markdown
		// blockquote so it still looks like "thinking" in the UI. This
		// is a pragmatic fallback for clients that don't render native
		// thinking blocks (notably Claude Code CLI with sk- auth, where
		// thinking is gated behind a Claude.ai subscription OAuth
		// session).
		if thinkingAsText {
			rendered := renderThinkingAsQuoted(ev.Text, &e.thinkingBuf)
			if rendered == "" {
				return nil
			}
			e.thinkingFallbackOpen = true
			// Emit directly as a text delta instead of recursing into
			// the content case — the recursion would trip the
			// thinkingFallbackOpen→content transition path and reset
			// e.thinkingBuf, which makes every subsequent fragment
			// re-emit the "💭 Thinking" header.
			if err := e.closeOpenToolBlocks(); err != nil {
				return err
			}
			if !e.textBlockOpen {
				e.textBlockOpen = true
				if err := e.write("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": e.blockIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				}); err != nil {
					return err
				}
			}
			if err := e.emitTextDeltaByRune(rendered); err != nil {
				return err
			}
			e.outputTokens += int64(len(rendered) / 4)
			return nil
		}
		if e.textBlockOpen {
			if err := e.closeCurrentNonToolBlock(); err != nil {
				return err
			}
		}
		if err := e.closeOpenToolBlocks(); err != nil {
			return err
		}
		if !e.thinkingBlockOpen {
			e.thinkingBlockOpen = true
			// Anthropic's streaming wire format requires an empty
			// signature in content_block_start; the real signature
			// arrives in a trailing signature_delta just before
			// content_block_stop (see closeCurrentNonToolBlock).
			// Putting the signature in the start event instead causes
			// Claude Code to acknowledge the thinking block exists but
			// refuse to render its body.
			if err := e.write("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": e.blockIndex,
				"content_block": map[string]any{
					"type":      "thinking",
					"thinking":  "",
					"signature": "",
				},
			}); err != nil {
				return err
			}
		}
		if err := e.write("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.blockIndex,
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": ev.Text,
			},
		}); err != nil {
			return err
		}
		e.markFirstOutput()
		// Accumulate thinking text so Finish-time / block-close code can
		// produce a stable synthetic signature. Kiro has no native
		// signed-thinking output; without some signature at all the
		// Claude Code UI silently hides the thinking block.
		e.thinkingBuf.WriteString(ev.Text)
		e.outputTokens += int64(len(ev.Text) / 4)
		return nil

	case "tool_use_start":
		// Buffer the tool lifecycle until tool_use_stop arrives. Kiro
		// sends the canonical input in every frame (object form), and
		// only a single complete block can be forwarded to the client �?
		// see flushPendingTool() for the actual emission.
		if _, ok := e.pendingTools[ev.ToolUseID]; !ok {
			e.pendingTools[ev.ToolUseID] = &pendingToolBlock{toolName: ev.ToolName}
			e.toolOrder = append(e.toolOrder, ev.ToolUseID)
		} else if ev.ToolName != "" {
			// Repeated start with a name �?usually the same tool, but
			// refresh the name just in case Kiro promoted a placeholder.
			e.pendingTools[ev.ToolUseID].toolName = ev.ToolName
		}
		if ev.ToolInput != "" {
			e.recordToolInput(ev.ToolUseID, ev.ToolInput)
		}
		return nil

	case "tool_use_delta":
		if _, ok := e.pendingTools[ev.ToolUseID]; !ok {
			e.pendingTools[ev.ToolUseID] = &pendingToolBlock{}
			e.toolOrder = append(e.toolOrder, ev.ToolUseID)
		}
		if ev.ToolDelta != "" {
			e.recordToolInput(ev.ToolUseID, ev.ToolDelta)
		}
		return nil

	case "tool_use_stop":
		return e.flushPendingTool(ev.ToolUseID)

	case "usage":
		if ev.Usage.InputTokens > 0 {
			e.inputTokens = ev.Usage.InputTokens
		}
		if ev.Usage.OutputTokens > 0 {
			e.outputTokens = ev.Usage.OutputTokens
		}
		if ev.Usage.CacheReadTokens > 0 {
			e.cacheReadTokens = ev.Usage.CacheReadTokens
		}
		if ev.Usage.CacheWriteTokens > 0 {
			e.cacheWriteTokens = ev.Usage.CacheWriteTokens
		}
		return nil

	case "metering":
		// Kiro emits a terminal `meteringEvent` carrying the definitive
		// credit usage for this turn. We don't surface it in the SSE
		// framing (clients don't know about it) but stash the numbers so
		// the service layer can log/bill them later.
		if ev.Metering.Usage > 0 {
			e.meteringCredit += ev.Metering.Usage
		}
		if ev.Metering.Unit != "" {
			e.meteringUnit = ev.Metering.Unit
		}
		return nil

	case "context_usage":
		if ev.ContextUsagePct > 0 {
			e.contextUsagePct = ev.ContextUsagePct
		}
		return nil

	case "error":
		return e.write("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    firstNonEmpty(ev.ErrorType, "api_error"),
				"message": firstNonEmpty(ev.ErrorMessage, "upstream error"),
			},
		})
	}
	return nil
}

// closeCurrentNonToolBlock finalises whichever of text/thinking block is
// currently open and advances the block index. No-op when nothing is
// open. Tool blocks have their own lifecycle managed via toolBlocks and
// are untouched here.
func (e *AnthropicSSEEncoder) closeCurrentNonToolBlock() error {
	if !e.textBlockOpen && !e.thinkingBlockOpen {
		return nil
	}
	// Anthropic thinking blocks must close with a signature_delta
	// carrying the "signature" field. Claude Code's UI buffers the
	// thinking_delta events and waits for this signature before
	// rendering the block body — without it the block appears in the
	// state indicator but the contents stay hidden.
	if e.thinkingBlockOpen {
		sig := syntheticThinkingSignature()
		if err := e.write("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.blockIndex,
			"delta": map[string]any{
				"type":      "signature_delta",
				"signature": sig,
			},
		}); err != nil {
			return err
		}
		e.thinkingBuf.Reset()
	}
	if err := e.write("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": e.blockIndex,
	}); err != nil {
		return err
	}
	e.textBlockOpen = false
	e.thinkingBlockOpen = false
	e.blockIndex++
	return nil
}

// syntheticThinkingSignature returns a placeholder signature string for a
// thinking content block. Anthropic's native signatures are server-signed
// cryptographic blobs; we cannot forge those, but Claude Code and the
// other clients we target do not actually verify them — they only check
// that a non-empty, unique-ish signature is present. The shape
// `sig_<32hex>` matches what jwadow/kiro-gateway ships and has been
// confirmed to render fine in Claude Code.
//
// We seed with a per-block UUID (not a hash of the body) so two thinking
// blocks in the same stream get different signatures, matching how real
// Anthropic thinking blocks behave and avoiding any deduplication the
// client might apply when two blocks share a signature.
func syntheticThinkingSignature() string {
	return "sig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// closeOpenToolBlocks flushes every pending tool_use that has not yet
// received a stop frame. This is called before opening a new text /
// thinking / tool block to avoid index collisions �?Kiro's wire format
// skips the explicit tool_use_stop frame in some situations and without
// this flush the next block would inherit the stale index, causing
// Claude Code to raise "Content block is not a input_json block".
func (e *AnthropicSSEEncoder) closeOpenToolBlocks() error {
	if len(e.pendingTools) == 0 && len(e.toolBlocks) == 0 {
		return nil
	}
	// Flush pending tools (buffered start/delta/stop triple).
	for _, id := range e.toolOrder {
		if _, ok := e.pendingTools[id]; ok {
			if err := e.flushPendingTool(id); err != nil {
				return err
			}
		}
	}
	// Any legacy tool blocks that somehow entered toolBlocks without a
	// pending buffer (shouldn't happen with the new pipeline, but defend
	// in depth): close them by emitting stop.
	for id, idx := range e.toolBlocks {
		if err := e.write("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		}); err != nil {
			return err
		}
		delete(e.toolBlocks, id)
		e.blockIndex++
	}
	return nil
}

// recordToolInput keeps track of the latest tool input seen for a tool.
// Kiro sends input either as a complete JSON object (every frame carrying
// the same bytes) or as a streamed partial_json string (frames carry
// disjoint fragments). We detect the two modes:
//   - frame content identical to a previously seen frame �?drop (dup)
//   - frame that already looks like a complete JSON object �?overwrite
//     finalInput (object-style mode)
//   - otherwise append to the string buffer (partial-string mode)
func (e *AnthropicSSEEncoder) recordToolInput(id, fragment string) {
	p, ok := e.pendingTools[id]
	if !ok {
		p = &pendingToolBlock{}
		e.pendingTools[id] = p
		e.toolOrder = append(e.toolOrder, id)
	}
	trimmed := strings.TrimSpace(fragment)
	if trimmed == "" {
		return
	}
	// Object-style mode: fragment is a complete JSON object/array.
	if looksLikeCompleteJSON(trimmed) {
		p.finalInput = trimmed
		p.inputSeen = true
		return
	}
	// Partial-string mode: append to the buffer.
	if !p.inputSeen {
		p.inputSeen = true
	}
	p.input.WriteString(fragment)
}

// looksLikeCompleteJSON returns true if s is a syntactically complete
// JSON object or array. Used to distinguish between object-style input
// (complete every frame) and streamed partial_json (incremental).
func looksLikeCompleteJSON(s string) bool {
	if len(s) < 2 {
		return false
	}
	first, last := s[0], s[len(s)-1]
	if !((first == '{' && last == '}') || (first == '[' && last == ']')) {
		return false
	}
	return json.Valid([]byte(s))
}

// flushPendingTool emits the buffered tool_use as a single
// content_block_start �?content_block_delta �?content_block_stop triple
// and advances the block index. No-op when no buffer exists for the id.
func (e *AnthropicSSEEncoder) flushPendingTool(id string) error {
	p, ok := e.pendingTools[id]
	if !ok {
		return nil
	}
	// Close any other block type first.
	if e.textBlockOpen || e.thinkingBlockOpen {
		if err := e.closeCurrentNonToolBlock(); err != nil {
			return err
		}
	}

	// Resolve the final input string. Object-style wins; otherwise use
	// the accumulated partial_json. Parse to verify it's a JSON object;
	// Claude Code requires `input` to be an object, not a raw string.
	inputStr := p.finalInput
	if inputStr == "" {
		inputStr = p.input.String()
	}
	inputObj := map[string]any{}
	if trimmed := strings.TrimSpace(inputStr); trimmed != "" {
		_ = json.Unmarshal([]byte(trimmed), &inputObj)
	}

	idx := e.blockIndex
	e.markFirstOutput()
	if err := e.write("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    normalizeToolUseID(id),
			"name":  p.toolName,
			"input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	// Emit the final input as a single partial_json delta so the
	// client assembles it in one shot. We marshal the parsed object so
	// the output is canonical JSON (no duplicate keys, consistent
	// escaping).
	canonical, err := json.Marshal(inputObj)
	if err != nil {
		return err
	}
	if err := e.write("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": string(canonical),
		},
	}); err != nil {
		return err
	}
	if err := e.write("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	}); err != nil {
		return err
	}
	delete(e.pendingTools, id)
	// Drop the id from toolOrder without allocating a new slice.
	for i, oid := range e.toolOrder {
		if oid == id {
			e.toolOrder = append(e.toolOrder[:i], e.toolOrder[i+1:]...)
			break
		}
	}
	e.blockIndex++
	return nil
}

// Finish closes any open blocks and emits message_delta/message_stop.
func (e *AnthropicSSEEncoder) Finish(stopReason string) error {
	if err := e.Start(); err != nil {
		return err
	}
	// Flush any tool_use buffers that never received an explicit stop.
	for _, id := range append([]string{}, e.toolOrder...) {
		_ = e.flushPendingTool(id)
	}
	if e.textBlockOpen || e.thinkingBlockOpen {
		_ = e.closeCurrentNonToolBlock()
	}
	for id, idx := range e.toolBlocks {
		_ = e.write("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		})
		delete(e.toolBlocks, id)
		e.blockIndex++
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}
	finalUsage := map[string]any{
		"input_tokens":  e.inputTokens,
		"output_tokens": e.outputTokens,
	}
	if e.cacheWriteTokens > 0 {
		finalUsage["cache_creation_input_tokens"] = e.cacheWriteTokens
	}
	if e.cacheReadTokens > 0 {
		finalUsage["cache_read_input_tokens"] = e.cacheReadTokens
	}
	_ = e.write("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": finalUsage,
	})
	return e.write("message_stop", map[string]any{
		"type": "message_stop",
	})
}

// InputTokens returns the accumulated input token count (if upstream reported).
func (e *AnthropicSSEEncoder) InputTokens() int64 { return e.inputTokens }

// OutputTokens returns the accumulated output token count (estimated if
// upstream did not report).
func (e *AnthropicSSEEncoder) OutputTokens() int64 { return e.outputTokens }

// CacheReadTokens returns the accumulated cacheReadInputTokens reported by
// Kiro's tokenUsage frame. Non-zero means the upstream served part of the
// prompt from its prefix cache, billed at the cheaper cache-read rate.
func (e *AnthropicSSEEncoder) CacheReadTokens() int64 { return e.cacheReadTokens }

// CacheWriteTokens returns the accumulated cacheWriteInputTokens reported
// by Kiro's tokenUsage frame. Non-zero means the upstream wrote new tokens
// into the prefix cache for future hits (mapped to Anthropic's
// cache_creation_input_tokens).
func (e *AnthropicSSEEncoder) CacheWriteTokens() int64 { return e.cacheWriteTokens }

// MeteringCredit returns the sum of all `usage` values seen in meteringEvent
// frames. Unit is whatever Kiro reports in MeteringUnit() (usually "credit").
func (e *AnthropicSSEEncoder) MeteringCredit() float64 { return e.meteringCredit }

// MeteringUnit returns the last unit string observed (typically "credit").
func (e *AnthropicSSEEncoder) MeteringUnit() string { return e.meteringUnit }

// ContextUsagePct returns the highest context-usage percentage reported by
// the upstream (useful for UI dashboards).
func (e *AnthropicSSEEncoder) ContextUsagePct() float64 { return e.contextUsagePct }

// MessageID returns the stable message id used in the SSE stream.
func (e *AnthropicSSEEncoder) MessageID() string { return e.messageID }

// ToolCallInterceptor lets callers intercept a complete tool_use lifecycle
// (start �?deltas �?stop) and either let it pass through to the client or
// replace it with an arbitrary synthetic event stream. Typical use: fulfil
// Kiro's web_search tool_use server-side via /mcp so the client never sees
// the tool call �?it sees the search result as regular assistant text.
//
// The interceptor is consulted exactly once per tool_use lifecycle. When
// OnToolStart returns handled=true, the gateway buffers the whole
// lifecycle (start + any deltas + stop) until stop arrives, then calls
// OnToolStop. The interceptor uses the provided emit callback to stream
// any number of replacement events; returning nil without emitting drops
// the tool call entirely.
type ToolCallInterceptor interface {
	// OnToolStart is called when a tool_use_start event is observed. It
	// should quickly decide whether to intercept (buffer) this tool call.
	OnToolStart(ctx context.Context, start *StreamEvent) (handled bool)
	// OnToolStop is called once the matching tool_use_stop arrives, with
	// the full concatenated input string. Events are sent to the client
	// via emit; returning an error surfaces it as a stream-level error.
	OnToolStop(ctx context.Context, toolUseID, toolName, input string, emit func(*StreamEvent) error) error
}

// DriveEventStreamToAnthropic reads EventStream frames from r, converts them
// through ParseEventStreamFrame, and drives the encoder until the stream ends.
//
// Returns accumulated text (for non-stream use cases) and any error other than
// io.EOF.
func DriveEventStreamToAnthropic(ctx context.Context, r io.Reader, enc *AnthropicSSEEncoder) (string, error) {
	return DriveEventStreamToAnthropicWithInterceptor(ctx, r, enc, nil)
}

// DriveEventStreamToAnthropicWithInterceptor is the same as
// DriveEventStreamToAnthropic but lets the caller plug in a tool-call
// interceptor. Pass nil to get the default pass-through behaviour.
func DriveEventStreamToAnthropicWithInterceptor(
	ctx context.Context,
	r io.Reader,
	enc *AnthropicSSEEncoder,
	interceptor ToolCallInterceptor,
) (string, error) {
	reader := NewEventStreamReader(r)
	var textBuf bytes.Buffer

	// Per-intercepted-tool buffer: tool_use_start + its deltas are held
	// here until tool_use_stop is observed, at which point the interceptor
	// decides what to emit.
	pending := make(map[string]*interceptorPending)

	// Kiro's wire format emits every toolUseEvent frame with the same
	// `name` field populated, even though only the first one is a real
	// "start" and subsequent ones are partial-input continuations
	// (the reference kiro-gateway Python implementation handles this the
	// same way in parsers.py). We track which toolUseIds we've already
	// started so we can demote later frames from tool_use_start to
	// tool_use_delta before downstream logic runs.
	seenStarts := make(map[string]struct{})
	// lastToolInput remembers the last input fragment we forwarded for a
	// given toolUseId. When Kiro's upstream emits `input` as a complete
	// JSON object rather than a streamed-string partial, every frame
	// carries the *same* full JSON and the naive "demote to delta" logic
	// would concatenate the same JSON object multiple times �?Claude
	// Code then rejects the tool_use with "Content block is not a
	// input_json block" because `{...}{...}` is not valid JSON. We keep
	// the last forwarded fragment so duplicates are dropped.
	lastToolInput := make(map[string]string)

	// thinkSplitter converts raw assistant text �?which may include
	// <thinking>...</thinking> blocks when we injected the
	// thinking_mode=enabled prompt �?into a mix of content and thinking
	// events. Stateful across chunks to handle tags split at arbitrary
	// boundaries. The byte cap defensively protects against models that
	// ignore the prompt-level budget and would otherwise eat the whole
	// output token ceiling on reasoning alone.
	thinkSplitter := &ThinkingSplitter{MaxThinkingBytes: defaultThinkingByteCap}
	// bracketSplitter rescues tool_use invocations that the Kiro model
	// sometimes renders as literal text (e.g. "[tool_use Bash {...}]")
	// instead of proper toolUseEvent frames. When detected, the splitter
	// swaps the text for synthetic tool_use_start/stop events so Claude
	// Code actually executes the call instead of showing raw JSON.
	bracketSplitter := NewBracketToolSplitter()

	emit := func(ev *StreamEvent) error {
		if ev.Kind == "content" {
			textBuf.WriteString(ev.Text)
		}
		return enc.Emit(ev)
	}

	// emitParsed runs a (possibly content) event through the thinking
	// splitter, then the bracket splitter. Non-content events pass
	// straight through.
	emitParsed := func(ev *StreamEvent) error {
		if ev.Kind != "content" {
			return emit(ev)
		}
		for _, out := range thinkSplitter.Feed(ev.Text) {
			if out.Kind == "content" {
				for _, sub := range bracketSplitter.Feed(out.Text) {
					if err := emit(sub); err != nil {
						return err
					}
				}
				continue
			}
			if err := emit(out); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		if ctx.Err() != nil {
			return textBuf.String(), ctx.Err()
		}
		msg, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Flush any trailing bytes still held by the thinking
				// splitter so unterminated chunks reach the client.
				for _, out := range thinkSplitter.Flush() {
					if out.Kind == "content" {
						for _, sub := range bracketSplitter.Feed(out.Text) {
							if eerr := emit(sub); eerr != nil {
								return textBuf.String(), eerr
							}
						}
						continue
					}
					if eerr := emit(out); eerr != nil {
						return textBuf.String(), eerr
					}
				}
				for _, out := range bracketSplitter.Flush() {
					if eerr := emit(out); eerr != nil {
						return textBuf.String(), eerr
					}
				}
				// Flush any intercepted tool_use lifecycles that did not
				// receive an explicit stop frame. Kiro's streaming format
				// does not always emit a trailing stop so we finalise
				// here to avoid silent truncation.
				if interceptor != nil {
					for id, p := range pending {
						if !p.active {
							continue
						}
						delete(pending, id)
						if ierr := interceptor.OnToolStop(ctx, id, p.name, finalToolInput(p), emit); ierr != nil {
							_ = emit(&StreamEvent{
								Kind:         "error",
								ErrorType:    "tool_interceptor_error",
								ErrorMessage: ierr.Error(),
							})
						}
					}
				}
				return textBuf.String(), nil
			}
			return textBuf.String(), err
		}
		ev, perr := ParseEventStreamFrame(msg)
		if perr != nil {
			return textBuf.String(), perr
		}
		if ev == nil {
			continue
		}

		// Kiro's wire format emits every toolUseEvent frame with the same
		// `name` field populated, even though only the first one is a real
		// "start" and subsequent ones carry the full input again.
		// For the interceptor pipeline we still need to distinguish the
		// real start (invokes OnToolStart) from follow-up frames (feed
		// input buffer). Demote repeats into tool_use_delta for that
		// purpose. The encoder itself does not care because it
		// aggregates both kinds into the same pendingToolBlock.
		if ev.Kind == "tool_use_start" {
			if _, already := seenStarts[ev.ToolUseID]; already {
				if ev.ToolInput == lastToolInput[ev.ToolUseID] {
					// Exact duplicate �?still forward one delta so the
					// encoder's pendingToolBlock.finalInput gets set
					// (object-style input only appears in a dup frame
					// after the initial start). Skipping would leave
					// finalInput empty when the first start carried no
					// ToolInput string.
				}
				ev = &StreamEvent{
					Kind:      "tool_use_delta",
					ToolUseID: ev.ToolUseID,
					ToolDelta: ev.ToolInput,
				}
				lastToolInput[ev.ToolUseID] = ev.ToolDelta
			} else {
				seenStarts[ev.ToolUseID] = struct{}{}
				lastToolInput[ev.ToolUseID] = ev.ToolInput
			}
		} else if ev.Kind == "tool_use_delta" {
			lastToolInput[ev.ToolUseID] = ev.ToolDelta
		} else if ev.Kind == "tool_use_stop" {
			delete(seenStarts, ev.ToolUseID)
			delete(lastToolInput, ev.ToolUseID)
		}

		// Interceptor hook: buffer tool_use events of interest until stop.
		if interceptor != nil {
			switch ev.Kind {
			case "tool_use_start":
				if interceptor.OnToolStart(ctx, ev) {
					p := &interceptorPending{name: ev.ToolName, active: true}
					if ev.ToolInput != "" {
						addToolInputFragment(p, ev.ToolInput)
					}
					pending[ev.ToolUseID] = p
					continue
				}
			case "tool_use_delta":
				if p, ok := pending[ev.ToolUseID]; ok && p.active {
					addToolInputFragment(p, ev.ToolDelta)
					continue
				}
			case "tool_use_stop":
				if p, ok := pending[ev.ToolUseID]; ok && p.active {
					delete(pending, ev.ToolUseID)
					if ierr := interceptor.OnToolStop(ctx, ev.ToolUseID, p.name, finalToolInput(p), emit); ierr != nil {
						if eerr := emit(&StreamEvent{
							Kind:         "error",
							ErrorType:    "tool_interceptor_error",
							ErrorMessage: ierr.Error(),
						}); eerr != nil {
							return textBuf.String(), eerr
						}
					}
					continue
				}
			}
		}

		if err := emitParsed(ev); err != nil {
			return textBuf.String(), err
		}
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// normalizeToolUseID converts Kiro's tool_use ID format ("tooluse_xxx") to
// the Anthropic format ("toolu_xxx") that Claude Code expects. Without this
// prefix, Claude Code treats the tool_use block as plain text instead of
// executing the tool.
func normalizeToolUseID(id string) string {
	if strings.HasPrefix(id, "tooluse_") {
		return "toolu_" + id[len("tooluse_"):]
	}
	if strings.HasPrefix(id, "toolu_") {
		return id // already correct
	}
	return "toolu_" + id
}

// FollowUpEmitter is the callback shape used by DriveFollowUp to hand
// parsed events back to the caller. The caller is responsible for
// forwarding those events to the client encoder (or dropping/rewriting
// them), which is why DriveFollowUp does not take an encoder directly �?
// a follow-up turn shares the envelope of the primary turn.
type FollowUpEmitter interface {
	Emit(ev *StreamEvent) error
}

// DriveFollowUp reads a Kiro /generateAssistantResponse SSE stream the
// same way DriveEventStreamToAnthropic does �?crucially applying the
// "repeated tool_use_start is really a delta" Kiro wire-format rule �?and
// hands each normalised event to the caller-supplied emitter. It does
// NOT own an encoder, which lets a follow-up turn reuse the primary
// turn's encoder state (message_start / blockIndex / etc.) via the
// caller's emit closure.
//
// Returns the accumulated content text (for logging/diagnostics) and any
// error other than io.EOF.
func DriveFollowUp(ctx context.Context, r io.Reader, sink FollowUpEmitter) (string, error) {
	if sink == nil {
		return "", fmt.Errorf("kiro: DriveFollowUp requires a non-nil sink")
	}
	reader := NewEventStreamReader(r)
	var textBuf bytes.Buffer

	seenStarts := make(map[string]struct{})
	lastToolInput := make(map[string]string)
	// bracketSplitter rescues bracket-style tool_use invocations that
	// Kiro occasionally renders as plain text even inside follow-up
	// turns (WebFetch, Bash curl commands, etc). Mirrors the main
	// driver's pipeline.
	bracketSplitter := NewBracketToolSplitter()

	emitWithBracket := func(ev *StreamEvent) error {
		if ev.Kind != "content" {
			return sink.Emit(ev)
		}
		textBuf.WriteString(ev.Text)
		for _, sub := range bracketSplitter.Feed(ev.Text) {
			if err := sink.Emit(sub); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		if ctx.Err() != nil {
			return textBuf.String(), ctx.Err()
		}
		msg, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				for _, sub := range bracketSplitter.Flush() {
					if eerr := sink.Emit(sub); eerr != nil {
						return textBuf.String(), eerr
					}
				}
				return textBuf.String(), nil
			}
			return textBuf.String(), err
		}
		ev, perr := ParseEventStreamFrame(msg)
		if perr != nil {
			return textBuf.String(), perr
		}
		if ev == nil {
			continue
		}

		// Same normalisation rule as the main driver: the first
		// tool_use frame is the real start, every subsequent frame
		// with the same toolUseId is actually a partial-input delta.
		// When input is sent as a complete JSON object, every frame
		// repeats the same payload - drop duplicates to avoid
		// concatenating the same JSON multiple times.
		if ev.Kind == "tool_use_start" {
			if _, already := seenStarts[ev.ToolUseID]; already {
				if ev.ToolInput == lastToolInput[ev.ToolUseID] {
					continue
				}
				ev = &StreamEvent{
					Kind:      "tool_use_delta",
					ToolUseID: ev.ToolUseID,
					ToolDelta: ev.ToolInput,
				}
				lastToolInput[ev.ToolUseID] = ev.ToolDelta
			} else {
				seenStarts[ev.ToolUseID] = struct{}{}
				lastToolInput[ev.ToolUseID] = ev.ToolInput
			}
		} else if ev.Kind == "tool_use_delta" {
			if ev.ToolDelta == lastToolInput[ev.ToolUseID] {
				continue
			}
			lastToolInput[ev.ToolUseID] = ev.ToolDelta
		} else if ev.Kind == "tool_use_stop" {
			delete(seenStarts, ev.ToolUseID)
			delete(lastToolInput, ev.ToolUseID)
		}

		if eerr := emitWithBracket(ev); eerr != nil {
			return textBuf.String(), eerr
		}
	}
}

// AnthropicNonStreamBuilder accumulates StreamEvents and produces a single
// Anthropic /v1/messages JSON response body at the end. Used when the
// client issued a non-streaming request (`stream: false`) �?Kiro's
// upstream always returns an SSE stream, so we buffer it and collapse it
// to the classic JSON shape before replying.
//
// The builder mirrors AnthropicSSEEncoder's lifecycle (Start �?Emit(*) �?
// Finish) so the same driver function can drive either implementation.
// It is deliberately minimal: we only surface the fields Claude Code
// actually consumes (content blocks, stop_reason, usage) and skip
// metering/context-usage because those never leave the gateway layer.
type AnthropicNonStreamBuilder struct {
	model     string
	messageID string

	// Open-block tracking: mirrors the encoder so tool_use ID lifetimes
	// line up. We key by Kiro's native id and map to the index into
	// blocks[] so deltas can find the right partial_json buffer.
	blocks   []nonStreamBlock
	toolByID map[string]int
	textIdx  int // index into blocks of the current open text block, or -1
	thinkIdx int // index into blocks of the current open thinking block, or -1

	inputTokens    int64
	outputTokens   int64
	// cacheReadTokens / cacheWriteTokens mirror the streaming encoder so
	// non-stream responses end up with the same usage shape on disk.
	cacheReadTokens  int64
	cacheWriteTokens int64
	meteringCredit   float64
	meteringUnit   string
	contextUsage   float64
	stopReason     string
	finished       bool
	outputStarted  bool
	onFirstOutput  func()
}

// nonStreamBlock is a single content block accumulated during assembly.
type nonStreamBlock struct {
	kind       string // "text" | "thinking" | "tool_use"
	text       string
	thinking   string
	toolID     string // Kiro-native id; normalised at Finish()
	toolName   string
	toolInput  strings.Builder // partial_json accumulated across deltas
	finalInput string          // complete JSON object, when provided whole per frame
}

// NewAnthropicNonStreamBuilder constructs a fresh builder. The model name
// is echoed back in the final JSON response (same as what the SSE encoder
// emits in message_start).
func NewAnthropicNonStreamBuilder(model string) *AnthropicNonStreamBuilder {
	return &AnthropicNonStreamBuilder{
		model:     model,
		messageID: "msg_" + uuid.NewString(),
		toolByID:  make(map[string]int),
		textIdx:   -1,
		thinkIdx:  -1,
	}
}

// SetInputTokensHint pre-populates the input_tokens field so relay
// consumers can read it even if the upstream never reports usage. Mirrors
// AnthropicSSEEncoder.SetInputTokensHint.
func (b *AnthropicNonStreamBuilder) SetInputTokensHint(n int64) {
	if n > 0 && b.inputTokens == 0 {
		b.inputTokens = n
	}
}

// SetFirstOutputCallback registers a hook fired when the first assistant
// output event is observed while collapsing Kiro's upstream stream.
func (b *AnthropicNonStreamBuilder) SetFirstOutputCallback(cb func()) {
	b.onFirstOutput = cb
}

func (b *AnthropicNonStreamBuilder) markFirstOutput() {
	if b.outputStarted {
		return
	}
	b.outputStarted = true
	if b.onFirstOutput != nil {
		b.onFirstOutput()
	}
}

// Start is a no-op for the builder; retained so the interface matches
// AnthropicSSEEncoder and the driver code can call it unconditionally.
func (b *AnthropicNonStreamBuilder) Start() error { return nil }

// Emit consumes a single StreamEvent. The builder silently ignores error
// and done events �?stop_reason is derived from Finish()'s argument.
func (b *AnthropicNonStreamBuilder) Emit(ev *StreamEvent) error {
	if ev == nil {
		return nil
	}
	switch ev.Kind {
	case "content":
		if ev.Text == "" {
			return nil
		}
		b.markFirstOutput()
		b.closeOpenTools()
		if b.thinkIdx >= 0 {
			b.thinkIdx = -1
		}
		if b.textIdx < 0 {
			b.blocks = append(b.blocks, nonStreamBlock{kind: "text"})
			b.textIdx = len(b.blocks) - 1
		}
		b.blocks[b.textIdx].text += ev.Text
		b.outputTokens += int64(len(ev.Text) / 4)

	case "thinking":
		if ev.Text == "" {
			return nil
		}
		b.markFirstOutput()
		b.closeOpenTools()
		if b.textIdx >= 0 {
			b.textIdx = -1
		}
		if b.thinkIdx < 0 {
			b.blocks = append(b.blocks, nonStreamBlock{kind: "thinking"})
			b.thinkIdx = len(b.blocks) - 1
		}
		b.blocks[b.thinkIdx].thinking += ev.Text
		b.outputTokens += int64(len(ev.Text) / 4)

	case "tool_use_start":
		b.markFirstOutput()
		b.closeOpenTools()
		b.textIdx = -1
		b.thinkIdx = -1
		b.blocks = append(b.blocks, nonStreamBlock{
			kind:     "tool_use",
			toolID:   ev.ToolUseID,
			toolName: ev.ToolName,
		})
		idx := len(b.blocks) - 1
		b.toolByID[ev.ToolUseID] = idx
		if ev.ToolInput != "" {
			b.recordToolInput(idx, ev.ToolInput)
		}

	case "tool_use_delta":
		if idx, ok := b.toolByID[ev.ToolUseID]; ok {
			b.recordToolInput(idx, ev.ToolDelta)
		}

	case "tool_use_stop":
		delete(b.toolByID, ev.ToolUseID)

	case "usage":
		if ev.Usage.InputTokens > 0 {
			b.inputTokens = ev.Usage.InputTokens
		}
		if ev.Usage.OutputTokens > 0 {
			b.outputTokens = ev.Usage.OutputTokens
		}
		if ev.Usage.CacheReadTokens > 0 {
			b.cacheReadTokens = ev.Usage.CacheReadTokens
		}
		if ev.Usage.CacheWriteTokens > 0 {
			b.cacheWriteTokens = ev.Usage.CacheWriteTokens
		}

	case "metering":
		if ev.Metering.Usage > 0 {
			b.meteringCredit += ev.Metering.Usage
		}
		if ev.Metering.Unit != "" {
			b.meteringUnit = ev.Metering.Unit
		}

	case "context_usage":
		if ev.ContextUsagePct > b.contextUsage {
			b.contextUsage = ev.ContextUsagePct
		}
	}
	return nil
}

// closeOpenTools clears all open tool lifecycle trackers without emitting
// anything �?the builder just moves on to a new block.
func (b *AnthropicNonStreamBuilder) closeOpenTools() {
	if len(b.toolByID) == 0 {
		return
	}
	for id := range b.toolByID {
		delete(b.toolByID, id)
	}
}

// Finish seals the response with the given stop reason and returns the
// assembled Anthropic Messages JSON payload.
func (b *AnthropicNonStreamBuilder) Finish(stopReason string) ([]byte, error) {
	if stopReason == "" {
		stopReason = "end_turn"
	}
	b.stopReason = stopReason
	b.finished = true

	content := make([]map[string]any, 0, len(b.blocks))
	for _, blk := range b.blocks {
		switch blk.kind {
		case "text":
			content = append(content, map[string]any{
				"type": "text",
				"text": blk.text,
			})
		case "thinking":
			content = append(content, map[string]any{
				"type":      "thinking",
				"thinking":  blk.thinking,
				"signature": syntheticThinkingSignature(),
			})
		case "tool_use":
			// Parse accumulated partial_json into a concrete input object
			// so the Anthropic response carries an object (not a string).
			raw := blk.finalInput
			if raw == "" {
				raw = blk.toolInput.String()
			}
			inputObj := parseToolUseInput(raw)
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    normalizeToolUseID(blk.toolID),
				"name":  blk.toolName,
				"input": inputObj,
			})
		}
	}

	usage := map[string]any{
		"input_tokens":  b.inputTokens,
		"output_tokens": b.outputTokens,
	}
	if b.cacheWriteTokens > 0 {
		usage["cache_creation_input_tokens"] = b.cacheWriteTokens
	}
	if b.cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = b.cacheReadTokens
	}
	msg := map[string]any{
		"id":            b.messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         b.model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}
	return json.Marshal(msg)
}

// parseToolUseInput tries to interpret the accumulated partial_json as a
// full JSON object. On failure it returns an empty object. Downstream
// Claude Code will treat that as a malformed tool call.
func parseToolUseInput(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return map[string]any{}
	}
	return obj
}

// recordToolInput merges a single frame's input payload into the
// addressed tool block. Complete JSON object frames overwrite the final
// input (Kiro's object-style delivery); partial JSON fragments are
// appended (string-style delivery). Mirrors
// AnthropicSSEEncoder.recordToolInput.
func (b *AnthropicNonStreamBuilder) recordToolInput(idx int, fragment string) {
	if idx < 0 || idx >= len(b.blocks) {
		return
	}
	trimmed := strings.TrimSpace(fragment)
	if trimmed == "" {
		return
	}
	if looksLikeCompleteJSON(trimmed) {
		b.blocks[idx].finalInput = trimmed
		return
	}
	b.blocks[idx].toolInput.WriteString(fragment)
}

// MessageID returns the stable message id used in the assembled response.
func (b *AnthropicNonStreamBuilder) MessageID() string { return b.messageID }

// InputTokens/OutputTokens/CacheReadTokens/CacheWriteTokens/MeteringCredit/
// ContextUsagePct expose accumulated counters so the service layer can
// record usage the same way it does for the streaming encoder.
func (b *AnthropicNonStreamBuilder) InputTokens() int64      { return b.inputTokens }
func (b *AnthropicNonStreamBuilder) OutputTokens() int64     { return b.outputTokens }
func (b *AnthropicNonStreamBuilder) CacheReadTokens() int64  { return b.cacheReadTokens }
func (b *AnthropicNonStreamBuilder) CacheWriteTokens() int64 { return b.cacheWriteTokens }
func (b *AnthropicNonStreamBuilder) MeteringCredit() float64 { return b.meteringCredit }
func (b *AnthropicNonStreamBuilder) MeteringUnit() string    { return b.meteringUnit }
func (b *AnthropicNonStreamBuilder) ContextUsagePct() float64 {
	return b.contextUsage
}

// AnthropicEventSink is the common interface implemented by both the
// streaming encoder (AnthropicSSEEncoder) and the non-streaming builder
// (AnthropicNonStreamBuilder). The driver functions use it so callers can
// pick the right output mode at the call site.
type AnthropicEventSink interface {
	Start() error
	Emit(ev *StreamEvent) error
}

// DriveEventStreamToSink is a sink-generic version of
// DriveEventStreamToAnthropicWithInterceptor. Used by non-streaming paths
// that feed events into AnthropicNonStreamBuilder.
func DriveEventStreamToSink(
	ctx context.Context,
	r io.Reader,
	sink AnthropicEventSink,
	interceptor ToolCallInterceptor,
) (string, error) {
	// Reuse the SSE driver by wrapping the sink into an encoder-like
	// shim. The only method that matters is Emit; Start is idempotent.
	if enc, ok := sink.(*AnthropicSSEEncoder); ok {
		return DriveEventStreamToAnthropicWithInterceptor(ctx, r, enc, interceptor)
	}
	// For non-SSE sinks, run a miniature driver that only forwards
	// events. We still apply the Kiro repeat-start delta
	// normalisation so tool_use accumulation is correct.
	reader := NewEventStreamReader(r)
	var textBuf bytes.Buffer
	seenStarts := make(map[string]struct{})
	lastToolInput := make(map[string]string)
	pending := make(map[string]*interceptorPending)
	bracketSplitter := NewBracketToolSplitter()

	emit := func(ev *StreamEvent) error {
		if ev.Kind != "content" {
			return sink.Emit(ev)
		}
		textBuf.WriteString(ev.Text)
		for _, sub := range bracketSplitter.Feed(ev.Text) {
			if err := sink.Emit(sub); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		if ctx.Err() != nil {
			return textBuf.String(), ctx.Err()
		}
		msg, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Finalise any unterminated intercepted tool calls.
				if interceptor != nil {
					for id, p := range pending {
						if !p.active {
							continue
						}
						delete(pending, id)
						if ierr := interceptor.OnToolStop(ctx, id, p.name, finalToolInput(p), emit); ierr != nil {
							_ = emit(&StreamEvent{
								Kind:         "error",
								ErrorType:    "tool_interceptor_error",
								ErrorMessage: ierr.Error(),
							})
						}
					}
				}
				// Flush any remaining bracket-form tool call still
				// buffered by the splitter.
				for _, sub := range bracketSplitter.Flush() {
					if eerr := sink.Emit(sub); eerr != nil {
						return textBuf.String(), eerr
					}
				}
				return textBuf.String(), nil
			}
			return textBuf.String(), err
		}
		ev, perr := ParseEventStreamFrame(msg)
		if perr != nil {
			return textBuf.String(), perr
		}
		if ev == nil {
			continue
		}
		if ev.Kind == "tool_use_start" {
			if _, already := seenStarts[ev.ToolUseID]; already {
				if ev.ToolInput == lastToolInput[ev.ToolUseID] {
					continue
				}
				ev = &StreamEvent{
					Kind:      "tool_use_delta",
					ToolUseID: ev.ToolUseID,
					ToolDelta: ev.ToolInput,
				}
				lastToolInput[ev.ToolUseID] = ev.ToolDelta
			} else {
				seenStarts[ev.ToolUseID] = struct{}{}
				lastToolInput[ev.ToolUseID] = ev.ToolInput
			}
		} else if ev.Kind == "tool_use_delta" {
			if ev.ToolDelta == lastToolInput[ev.ToolUseID] {
				continue
			}
			lastToolInput[ev.ToolUseID] = ev.ToolDelta
		} else if ev.Kind == "tool_use_stop" {
			delete(seenStarts, ev.ToolUseID)
			delete(lastToolInput, ev.ToolUseID)
		}
		if interceptor != nil {
			switch ev.Kind {
			case "tool_use_start":
				if interceptor.OnToolStart(ctx, ev) {
					p := &interceptorPending{name: ev.ToolName, active: true}
					if ev.ToolInput != "" {
						addToolInputFragment(p, ev.ToolInput)
					}
					pending[ev.ToolUseID] = p
					continue
				}
			case "tool_use_delta":
				if p, ok := pending[ev.ToolUseID]; ok && p.active {
					addToolInputFragment(p, ev.ToolDelta)
					continue
				}
			case "tool_use_stop":
				if p, ok := pending[ev.ToolUseID]; ok && p.active {
					delete(pending, ev.ToolUseID)
					if ierr := interceptor.OnToolStop(ctx, ev.ToolUseID, p.name, finalToolInput(p), emit); ierr != nil {
						if eerr := emit(&StreamEvent{
							Kind:         "error",
							ErrorType:    "tool_interceptor_error",
							ErrorMessage: ierr.Error(),
						}); eerr != nil {
							return textBuf.String(), eerr
						}
					}
					continue
				}
			}
		}
		if err := emit(ev); err != nil {
			return textBuf.String(), err
		}
	}
}

// interceptorPending buffers a tool_use lifecycle while the interceptor
// decides what to do with it. Kiro's upstream may deliver input either
// as a complete JSON object repeated every frame, or as a stream of
// partial_json fragments. addToolInputFragment and finalToolInput handle
// both modes transparently so the interceptor always sees a single,
// canonical input string at OnToolStop time.
type interceptorPending struct {
	name       string
	input      strings.Builder
	finalInput string
	active     bool
}

// addToolInputFragment merges a single frame's input payload into a
// pending tool buffer. It recognises complete-JSON-object frames (which
// supersede any previously captured input) and falls back to appending
// streaming fragments.
func addToolInputFragment(p *interceptorPending, fragment string) {
	trimmed := strings.TrimSpace(fragment)
	if trimmed == "" {
		return
	}
	if looksLikeCompleteJSON(trimmed) {
		p.finalInput = trimmed
		return
	}
	p.input.WriteString(fragment)
}

// finalToolInput returns the canonical input string for the buffered
// tool call: the latest complete JSON object if one was seen, otherwise
// the accumulated partial_json fragments.
func finalToolInput(p *interceptorPending) string {
	if p.finalInput != "" {
		return p.finalInput
	}
	return p.input.String()
}

// renderThinkingAsQuoted converts a raw thinking fragment into
// display-ready text for the SUB2API_KIRO_THINKING_AS_TEXT fallback.
// We deliberately use plain ASCII dividers + a visible tag rather than
// markdown blockquote (`> `) because Claude Code's renderer buffers
// blockquote lines until a terminating blank line arrives — that makes
// the whole thinking section appear to land in a single "batch" at the
// same moment the final answer renders, which defeats the purpose of
// streaming the thinking at all.
//
// Streaming-friendly layout:
//
//	━━━ 💭 Thinking ━━━
//	...streaming thinking text unchanged...
//	━━━ 💬 Answer ━━━
//	...final answer...
//
// seenBuf carries insertion state across fragments so we only emit the
// header once per block. AnthropicSSEEncoder resets it via
// closeCurrentNonToolBlock when the thinking block ends, at which
// point we also emit the trailing divider.
// renderThinkingAsQuoted converts a raw thinking fragment into markdown
// blockquote text. Claude Code's markdown renderer turns lines prefixed
// with `> ` into a left-bar quote, which looks exactly like a "💭
// Thinking" UI widget — this is the rendering the user expects when
// thinking is enabled.
//
// We emit a one-time header on the very first fragment, then prefix
// every subsequent line start with `> ` so the whole thinking section
// shows as one continuous quote. seenBuf tracks insertion state across
// streaming fragments and is reset by the encoder when the thinking
// block ends.
func renderThinkingAsQuoted(raw string, seenBuf *strings.Builder) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	startOfBlock := seenBuf.Len() == 0
	if startOfBlock {
		b.WriteString("\n> **💭 Thinking**\n> \n> ")
	}
	// Track whether the last char in seenBuf was a newline so the next
	// fragment's first character gets a "> " marker.
	lastWasNewline := false
	if seenBuf.Len() > 0 {
		s := seenBuf.String()
		lastWasNewline = s[len(s)-1] == '\n'
	}
	for _, ch := range raw {
		if lastWasNewline && ch != '\n' {
			b.WriteString("> ")
			lastWasNewline = false
		}
		b.WriteRune(ch)
		if ch == '\n' {
			lastWasNewline = true
		}
	}
	seenBuf.WriteString(raw)
	return b.String()
}

// textDeltaRunesPerFrame controls how many unicode runes are packed
// into a single text_delta SSE event. Small chunks are reassembled by
// Claude Code into its own animation frames, so we don't need to be
// aggressive — 3 runes per frame keeps SSE overhead sane while still
// giving the client plenty of deltas to animate.
const textDeltaRunesPerFrame = 3

// textDeltaFrameInterval is the wall-clock gap between successive
// rune-level frames. Kept tiny so we don't artificially slow down the
// response when the client is capable of rendering fast.
const textDeltaFrameInterval = 0

// emitTextDeltaByRune splits a text chunk into rune-sized windows and
// emits each as its own content_block_delta SSE event. This keeps the
// client-side typing animation smooth even when Kiro sends big chunks
// (newline boundaries, markdown tables, …).
//
// Runs of pure ASCII whitespace and unicode whitespace at the start of
// a chunk are coalesced with the first visible window so we do not
// emit dozens of "invisible" frames at the beginning of a line.
func (e *AnthropicSSEEncoder) emitTextDeltaByRune(text string) error {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	// Fast path: short strings are emitted in one shot — splitting
	// would only add latency.
	if len(runes) <= textDeltaRunesPerFrame {
		return e.writeTextDelta(text)
	}
	for i := 0; i < len(runes); i += textDeltaRunesPerFrame {
		end := i + textDeltaRunesPerFrame
		if end > len(runes) {
			end = len(runes)
		}
		frame := string(runes[i:end])
		if err := e.writeTextDelta(frame); err != nil {
			return err
		}
		if end < len(runes) {
			time.Sleep(textDeltaFrameInterval)
		}
	}
	return nil
}

// writeTextDelta emits a single text_delta frame and forces an SSE
// flush so the byte actually lands in the client's socket buffer
// instead of sitting in the encoder's write buffer until the next call.
func (e *AnthropicSSEEncoder) writeTextDelta(s string) error {
	if s == "" {
		return nil
	}
	e.markFirstOutput()
	if err := e.write("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": e.blockIndex,
		"delta": map[string]any{
			"type": "text_delta",
			"text": s,
		},
	}); err != nil {
		return err
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}
