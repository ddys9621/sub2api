package kiro

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageEventPayload_UnmarshalsCacheFields verifies the JSON contract
// between Kiro's tokenUsage frame and our typed payload. Kiro emits the
// Amazon Q camelCase names (`cacheReadInputTokens` / `cacheWriteInputTokens`)
// — we must NOT silently lose them just because the local struct lacks
// matching tags. This was the root cause of the original "cache hit badge
// never lights up on Kiro accounts" bug.
func TestUsageEventPayload_UnmarshalsCacheFields(t *testing.T) {
	raw := []byte(`{
		"inputTokens": 1234,
		"outputTokens": 567,
		"cacheReadInputTokens": 8910,
		"cacheWriteInputTokens": 1112
	}`)
	var p UsageEventPayload
	require.NoError(t, json.Unmarshal(raw, &p))
	require.Equal(t, int64(1234), p.InputTokens)
	require.Equal(t, int64(567), p.OutputTokens)
	require.Equal(t, int64(8910), p.CacheReadTokens)
	require.Equal(t, int64(1112), p.CacheWriteTokens)
}

// TestParseEventStreamFrame_MessageMetadataKeepsCacheWriteOnly guards a
// narrow but important edge: when Kiro reports ONLY cache_write tokens
// (cold-cache turn that primes a new prefix) the frame must still be
// surfaced as a usage event. The previous condition was missing
// CacheWriteTokens and would drop these on the floor, losing the
// "cache being primed" signal in the usage log.
func TestParseEventStreamFrame_MessageMetadataKeepsCacheWriteOnly(t *testing.T) {
	msg := &EventStreamMessage{
		Headers: map[string]EventStreamHeader{
			":message-type": {Type: 0x07, Value: "event"},
			":event-type":   {Type: 0x07, Value: "messageMetadataEvent"},
		},
		Payload: []byte(`{"cacheWriteInputTokens": 4096}`),
	}
	ev, err := ParseEventStreamFrame(msg)
	require.NoError(t, err)
	require.NotNil(t, ev, "metadata frame with only cache_write tokens should NOT be dropped")
	require.Equal(t, "usage", ev.Kind)
	require.Equal(t, int64(4096), ev.Usage.CacheWriteTokens)
}

// TestAnthropicSSEEncoder_AccumulatesCacheTokens verifies that the
// streaming encoder's `case "usage"` branch stamps both cache counters
// onto its internal state so the service layer can read them later via
// CacheReadTokens() / CacheWriteTokens().
func TestAnthropicSSEEncoder_AccumulatesCacheTokens(t *testing.T) {
	enc := NewAnthropicSSEEncoder(&bytes.Buffer{}, nil, "claude-sonnet-4.5")
	require.NoError(t, enc.Emit(&StreamEvent{
		Kind: "usage",
		Usage: UsageEventPayload{
			InputTokens:      1000,
			OutputTokens:     200,
			CacheReadTokens:  500,
			CacheWriteTokens: 300,
		},
	}))
	require.Equal(t, int64(1000), enc.InputTokens())
	require.Equal(t, int64(200), enc.OutputTokens())
	require.Equal(t, int64(500), enc.CacheReadTokens())
	require.Equal(t, int64(300), enc.CacheWriteTokens())
}

// TestAnthropicSSEEncoder_FinishEmitsCacheTokens checks that the
// message_delta frame carries the Anthropic-named cache fields when the
// upstream reported them. Downstream Claude Code reads message_delta.usage
// directly, so without this the client UI would never show cache hits on
// Kiro-served turns.
func TestAnthropicSSEEncoder_FinishEmitsCacheTokens(t *testing.T) {
	var out bytes.Buffer
	enc := NewAnthropicSSEEncoder(&out, nil, "claude-sonnet-4.5")
	require.NoError(t, enc.Emit(&StreamEvent{Kind: "content", Text: "hi"}))
	require.NoError(t, enc.Emit(&StreamEvent{
		Kind: "usage",
		Usage: UsageEventPayload{
			InputTokens:      2000,
			OutputTokens:     400,
			CacheReadTokens:  1500,
			CacheWriteTokens: 250,
		},
	}))
	require.NoError(t, enc.Finish("end_turn"))

	sse := out.String()
	deltaPayload := extractSSEEventPayload(t, sse, "message_delta")
	usage := deltaPayload["usage"].(map[string]any)
	require.EqualValues(t, 1500, usage["cache_read_input_tokens"])
	require.EqualValues(t, 250, usage["cache_creation_input_tokens"])
	require.EqualValues(t, 2000, usage["input_tokens"])
	require.EqualValues(t, 400, usage["output_tokens"])
}

// TestAnthropicSSEEncoder_FinishOmitsCacheTokensWhenZero verifies the
// opposite branch: when the upstream did NOT report cache tokens (e.g.
// non-Claude model, or a turn that skipped the prefix cache) the
// message_delta must not carry zero-valued cache fields. Some downstream
// billing pipelines treat `cache_read_input_tokens: 0` as "explicitly 0
// reported" vs absent meaning "unknown"; we preserve the latter.
func TestAnthropicSSEEncoder_FinishOmitsCacheTokensWhenZero(t *testing.T) {
	var out bytes.Buffer
	enc := NewAnthropicSSEEncoder(&out, nil, "claude-sonnet-4.5")
	require.NoError(t, enc.Emit(&StreamEvent{Kind: "content", Text: "hi"}))
	require.NoError(t, enc.Emit(&StreamEvent{
		Kind:  "usage",
		Usage: UsageEventPayload{InputTokens: 100, OutputTokens: 50},
	}))
	require.NoError(t, enc.Finish("end_turn"))

	deltaPayload := extractSSEEventPayload(t, out.String(), "message_delta")
	usage := deltaPayload["usage"].(map[string]any)
	_, hasRead := usage["cache_read_input_tokens"]
	_, hasWrite := usage["cache_creation_input_tokens"]
	require.False(t, hasRead, "cache_read_input_tokens must be omitted when zero")
	require.False(t, hasWrite, "cache_creation_input_tokens must be omitted when zero")
}

// TestAnthropicNonStreamBuilder_AccumulatesCacheTokens mirrors the
// streaming encoder test on the non-stream path: Claude Code's WebFetch
// polling fires stream=false turns, and they must report cache hits too.
func TestAnthropicNonStreamBuilder_AccumulatesCacheTokens(t *testing.T) {
	b := NewAnthropicNonStreamBuilder("claude-sonnet-4.5")
	require.NoError(t, b.Emit(&StreamEvent{
		Kind: "usage",
		Usage: UsageEventPayload{
			InputTokens:      900,
			OutputTokens:     180,
			CacheReadTokens:  600,
			CacheWriteTokens: 120,
		},
	}))
	require.Equal(t, int64(900), b.InputTokens())
	require.Equal(t, int64(180), b.OutputTokens())
	require.Equal(t, int64(600), b.CacheReadTokens())
	require.Equal(t, int64(120), b.CacheWriteTokens())
}

// TestAnthropicNonStreamBuilder_FinishEmitsCacheTokens verifies the
// non-stream JSON body carries the cache fields in usage. The fields use
// the Anthropic shape so existing client SDKs / billing pipelines that
// already understand cache_creation_input_tokens / cache_read_input_tokens
// keep working unchanged.
func TestAnthropicNonStreamBuilder_FinishEmitsCacheTokens(t *testing.T) {
	b := NewAnthropicNonStreamBuilder("claude-sonnet-4.5")
	require.NoError(t, b.Emit(&StreamEvent{Kind: "content", Text: "hello"}))
	require.NoError(t, b.Emit(&StreamEvent{
		Kind: "usage",
		Usage: UsageEventPayload{
			InputTokens:      1500,
			OutputTokens:     300,
			CacheReadTokens:  900,
			CacheWriteTokens: 200,
		},
	}))
	raw, err := b.Finish("end_turn")
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	usage := body["usage"].(map[string]any)
	require.EqualValues(t, 1500, usage["input_tokens"])
	require.EqualValues(t, 300, usage["output_tokens"])
	require.EqualValues(t, 900, usage["cache_read_input_tokens"])
	require.EqualValues(t, 200, usage["cache_creation_input_tokens"])
}

// TestAnthropicNonStreamBuilder_FinishOmitsCacheTokensWhenZero is the
// non-stream counterpart of the encoder's omit test — keeps the body
// shape compatible with the original "no cache fields when none seen"
// contract that older clients may have come to rely on.
func TestAnthropicNonStreamBuilder_FinishOmitsCacheTokensWhenZero(t *testing.T) {
	b := NewAnthropicNonStreamBuilder("claude-sonnet-4.5")
	require.NoError(t, b.Emit(&StreamEvent{Kind: "content", Text: "hi"}))
	require.NoError(t, b.Emit(&StreamEvent{
		Kind:  "usage",
		Usage: UsageEventPayload{InputTokens: 10, OutputTokens: 5},
	}))
	raw, err := b.Finish("end_turn")
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	usage := body["usage"].(map[string]any)
	_, hasRead := usage["cache_read_input_tokens"]
	_, hasWrite := usage["cache_creation_input_tokens"]
	require.False(t, hasRead, "cache_read_input_tokens must be omitted when zero")
	require.False(t, hasWrite, "cache_creation_input_tokens must be omitted when zero")
}

// ---------- nested tokenUsage tests ----------
//
// The following tests guard the real-world Kiro response shape that
// caused the "cache_read / cache_creation always 0 in usage_logs" bug.
// Kiro's messageMetadataEvent nests its counters under a `tokenUsage`
// sub-object with `uncachedInputTokens` reported separately from the
// cache fields — the original flat struct silently dropped everything.

// TestUsageEventPayload_UnmarshalsNestedTokenUsage verifies json.Unmarshal
// populates the TokenUsage sub-object verbatim from Kiro's real shape.
func TestUsageEventPayload_UnmarshalsNestedTokenUsage(t *testing.T) {
	raw := []byte(`{
		"tokenUsage": {
			"uncachedInputTokens": 1000,
			"cacheReadInputTokens": 500,
			"cacheWriteInputTokens": 200,
			"outputTokens": 100,
			"totalTokens": 1800,
			"contextUsagePercentage": 45.5
		}
	}`)
	var p UsageEventPayload
	require.NoError(t, json.Unmarshal(raw, &p))
	require.NotNil(t, p.TokenUsage, "tokenUsage sub-object must decode into TokenUsage field")
	require.Equal(t, int64(1000), p.TokenUsage.UncachedInputTokens)
	require.Equal(t, int64(500), p.TokenUsage.CacheReadInputTokens)
	require.Equal(t, int64(200), p.TokenUsage.CacheWriteInputTokens)
	require.Equal(t, int64(100), p.TokenUsage.OutputTokens)
	require.Equal(t, int64(1800), p.TokenUsage.TotalTokens)
	require.InDelta(t, 45.5, p.TokenUsage.ContextUsagePercentage, 0.001)
}

// TestUsageEventPayload_FlattenMapsUncachedToInput is the load-bearing
// test for the fix. It asserts that Flatten lifts uncachedInputTokens
// into the top-level InputTokens slot following Anthropic's accounting
// convention (input_tokens excludes cached portions).
func TestUsageEventPayload_FlattenMapsUncachedToInput(t *testing.T) {
	p := UsageEventPayload{
		TokenUsage: &KiroTokenUsage{
			UncachedInputTokens:   1000,
			CacheReadInputTokens:  500,
			CacheWriteInputTokens: 200,
			OutputTokens:          100,
		},
	}
	p.Flatten()
	require.Equal(t, int64(1000), p.InputTokens, "uncachedInputTokens must map to top-level InputTokens")
	require.Equal(t, int64(100), p.OutputTokens)
	require.Equal(t, int64(500), p.CacheReadTokens)
	require.Equal(t, int64(200), p.CacheWriteTokens)
}

// TestUsageEventPayload_FlattenFallsBackToNestedInputTokens covers the
// alternate shape where Kiro reports `tokenUsage.inputTokens` instead of
// `uncachedInputTokens`. Flatten must still lift it to the top slot.
func TestUsageEventPayload_FlattenFallsBackToNestedInputTokens(t *testing.T) {
	p := UsageEventPayload{
		TokenUsage: &KiroTokenUsage{
			InputTokens:           750,
			CacheReadInputTokens:  250,
			CacheWriteInputTokens: 0,
			OutputTokens:          80,
		},
	}
	p.Flatten()
	require.Equal(t, int64(750), p.InputTokens)
	require.Equal(t, int64(80), p.OutputTokens)
	require.Equal(t, int64(250), p.CacheReadTokens)
	require.Equal(t, int64(0), p.CacheWriteTokens)
}

// TestUsageEventPayload_FlattenPrefersTopLevel verifies Flatten is
// non-destructive when the top-level slot already carries a value. The
// flat usageEvent shape must take precedence over any (unexpected)
// nested counterpart.
func TestUsageEventPayload_FlattenPrefersTopLevel(t *testing.T) {
	p := UsageEventPayload{
		InputTokens:      9999,
		OutputTokens:     888,
		CacheReadTokens:  77,
		CacheWriteTokens: 6,
		TokenUsage: &KiroTokenUsage{
			UncachedInputTokens:   1,
			OutputTokens:          2,
			CacheReadInputTokens:  3,
			CacheWriteInputTokens: 4,
		},
	}
	p.Flatten()
	require.Equal(t, int64(9999), p.InputTokens, "top-level InputTokens must not be overwritten")
	require.Equal(t, int64(888), p.OutputTokens)
	require.Equal(t, int64(77), p.CacheReadTokens)
	require.Equal(t, int64(6), p.CacheWriteTokens)
}

// TestUsageEventPayload_FlattenNilSafe makes sure Flatten is a no-op on
// nil receivers or when no TokenUsage sub-object was supplied — the
// streaming hot path calls Flatten unconditionally after Unmarshal so
// any panic here would crash the gateway.
func TestUsageEventPayload_FlattenNilSafe(t *testing.T) {
	var nilPtr *UsageEventPayload
	require.NotPanics(t, func() { nilPtr.Flatten() })

	p := UsageEventPayload{InputTokens: 42}
	require.NotPanics(t, func() { p.Flatten() })
	require.Equal(t, int64(42), p.InputTokens, "Flatten on payload without TokenUsage must not zero existing values")
}

// TestParseEventStreamFrame_MessageMetadataWithNestedTokenUsage is the
// end-to-end test for the fix: feed an EventStreamMessage with the real
// Kiro shape and assert ParseEventStreamFrame returns a fully populated
// usage StreamEvent ready for the encoder. Regression coverage for the
// production bug where cache_creation_tokens / cache_read_tokens were
// always 0 in usage_logs because the nested shape was being ignored.
func TestParseEventStreamFrame_MessageMetadataWithNestedTokenUsage(t *testing.T) {
	msg := &EventStreamMessage{
		Headers: map[string]EventStreamHeader{
			":message-type": {Type: 0x07, Value: "event"},
			":event-type":   {Type: 0x07, Value: "messageMetadataEvent"},
		},
		Payload: []byte(`{
			"tokenUsage": {
				"uncachedInputTokens": 1234,
				"cacheReadInputTokens": 8910,
				"cacheWriteInputTokens": 1112,
				"outputTokens": 567,
				"totalTokens": 11823
			}
		}`),
	}
	ev, err := ParseEventStreamFrame(msg)
	require.NoError(t, err)
	require.NotNil(t, ev, "nested tokenUsage must produce a usage StreamEvent")
	require.Equal(t, "usage", ev.Kind)
	require.Equal(t, int64(1234), ev.Usage.InputTokens, "uncachedInputTokens must surface as InputTokens")
	require.Equal(t, int64(567), ev.Usage.OutputTokens)
	require.Equal(t, int64(8910), ev.Usage.CacheReadTokens)
	require.Equal(t, int64(1112), ev.Usage.CacheWriteTokens)
}

// ---------- test helpers ----------

// extractSSEEventPayload finds the first `event: <name>` block in raw SSE
// output, decodes its `data:` line into a map, and returns it. The helper
// is intentionally lenient about CRLF / extra trailing whitespace because
// our encoder writes "\n" line endings but real terminals may convert them.
func extractSSEEventPayload(t *testing.T, sse, eventName string) map[string]any {
	t.Helper()
	want := "event: " + eventName
	idx := strings.Index(sse, want)
	require.GreaterOrEqualf(t, idx, 0, "event %q not found in SSE output:\n%s", eventName, sse)
	rest := sse[idx+len(want):]
	dataIdx := strings.Index(rest, "data:")
	require.GreaterOrEqualf(t, dataIdx, 0, "data line missing after event %q", eventName)
	rest = rest[dataIdx+len("data:"):]
	end := strings.Index(rest, "\n")
	if end < 0 {
		end = len(rest)
	}
	line := strings.TrimSpace(rest[:end])
	var payload map[string]any
	require.NoErrorf(t, json.Unmarshal([]byte(line), &payload),
		"could not decode SSE data line for event %q: %s", eventName, line)
	return payload
}
