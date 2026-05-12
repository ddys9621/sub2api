//go:build unit

package kiro

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKiroCacheTokens_E2E_StreamingWireToEncoder is the end-to-end
// regression test for the "cache_creation_tokens / cache_read_tokens
// always 0 in usage_logs" bug.
//
// It simulates the exact wire-format frames Kiro emits over its SSE
// stream — including the nested `tokenUsage` sub-object that the
// production gateway was silently dropping — and drives them through
// the real read path:
//
//	wire bytes ─▶ EventStreamReader.Next ─▶ ParseEventStreamFrame ─▶
//	  AnthropicSSEEncoder.Emit ─▶ encoder counters
//
// The fix lives in UsageEventPayload.Flatten(): without it, every
// counter accessor returns 0 because Kiro buries the numbers inside
// `tokenUsage` while the original struct only carried top-level slots.
//
// Numbers chosen to be obviously non-default so any silent zero would
// make the failure trivial to read in CI logs.
func TestKiroCacheTokens_E2E_StreamingWireToEncoder(t *testing.T) {
	// --- Build the upstream wire stream ---------------------------------
	// Two text frames followed by the canonical messageMetadataEvent
	// shape Kiro emits for Claude-backed models. tokenUsage matches the
	// real-world payload documented in Kiro-account-manager.
	var wire bytes.Buffer
	wire.Write(buildEventStreamFrame(
		map[string]string{
			":message-type": "event",
			":event-type":   "assistantResponseEvent",
			":content-type": "application/json",
		},
		[]byte(`{"content":"Hello "}`),
	))
	wire.Write(buildEventStreamFrame(
		map[string]string{
			":message-type": "event",
			":event-type":   "assistantResponseEvent",
			":content-type": "application/json",
		},
		[]byte(`{"content":"world!"}`),
	))
	wire.Write(buildEventStreamFrame(
		map[string]string{
			":message-type": "event",
			":event-type":   "messageMetadataEvent",
			":content-type": "application/json",
		},
		[]byte(`{
			"tokenUsage": {
				"uncachedInputTokens": 1234,
				"cacheReadInputTokens": 8910,
				"cacheWriteInputTokens": 1112,
				"outputTokens": 567,
				"totalTokens": 11823,
				"contextUsagePercentage": 12.5
			}
		}`),
	))

	// --- Drive the read path --------------------------------------------
	enc := NewAnthropicSSEEncoder(io.Discard, nil, "claude-sonnet-4.5")
	reader := NewEventStreamReader(&wire)
	for {
		frame, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "frame decode")
		ev, err := ParseEventStreamFrame(frame)
		require.NoError(t, err, "frame parse")
		if ev == nil {
			continue
		}
		require.NoError(t, enc.Emit(ev), "encoder emit")
	}

	// --- Assert the counters that ForwardResult.Usage will read --------
	require.Equal(t, int64(1234), enc.InputTokens(),
		"InputTokens must come from uncachedInputTokens (not the 0 the old code produced)")
	require.Equal(t, int64(567), enc.OutputTokens(),
		"OutputTokens must come from tokenUsage.outputTokens")
	require.Equal(t, int64(8910), enc.CacheReadTokens(),
		"CacheReadTokens must come from tokenUsage.cacheReadInputTokens — the headline fix")
	require.Equal(t, int64(1112), enc.CacheWriteTokens(),
		"CacheWriteTokens must come from tokenUsage.cacheWriteInputTokens — the headline fix")
}

// TestKiroCacheTokens_E2E_NonStreamingWireToBuilder mirrors the
// streaming test for the non-stream path (Anthropic Messages JSON
// response). Same wire bytes, but consumed by AnthropicNonStreamBuilder
// instead of AnthropicSSEEncoder. Both code paths populate the
// ForwardResult.Usage that ultimately hits usage_logs, so both must
// honor the nested tokenUsage shape.
func TestKiroCacheTokens_E2E_NonStreamingWireToBuilder(t *testing.T) {
	var wire bytes.Buffer
	wire.Write(buildEventStreamFrame(
		map[string]string{
			":message-type": "event",
			":event-type":   "assistantResponseEvent",
			":content-type": "application/json",
		},
		[]byte(`{"content":"Quick test"}`),
	))
	wire.Write(buildEventStreamFrame(
		map[string]string{
			":message-type": "event",
			":event-type":   "messageMetadataEvent",
			":content-type": "application/json",
		},
		[]byte(`{
			"tokenUsage": {
				"uncachedInputTokens": 4096,
				"cacheReadInputTokens": 16384,
				"cacheWriteInputTokens": 2048,
				"outputTokens": 256,
				"totalTokens": 22784
			}
		}`),
	))

	builder := NewAnthropicNonStreamBuilder("claude-sonnet-4.5")
	reader := NewEventStreamReader(&wire)
	for {
		frame, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "frame decode")
		ev, err := ParseEventStreamFrame(frame)
		require.NoError(t, err, "frame parse")
		if ev == nil {
			continue
		}
		require.NoError(t, builder.Emit(ev), "builder emit")
	}

	require.Equal(t, int64(4096), builder.InputTokens())
	require.Equal(t, int64(256), builder.OutputTokens())
	require.Equal(t, int64(16384), builder.CacheReadTokens(),
		"non-stream path must surface cacheReadInputTokens too")
	require.Equal(t, int64(2048), builder.CacheWriteTokens(),
		"non-stream path must surface cacheWriteInputTokens too")

	// Final JSON body must carry the cache counters under Anthropic's
	// canonical names — this is what Claude Code / clients see and what
	// the front-end usage view renders into the cache-hit badges.
	raw, err := builder.Finish("end_turn")
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	usage, _ := body["usage"].(map[string]any)
	require.NotNil(t, usage, "Anthropic body must include `usage`")
	require.EqualValues(t, 4096, usage["input_tokens"])
	require.EqualValues(t, 256, usage["output_tokens"])
	require.EqualValues(t, 16384, usage["cache_read_input_tokens"])
	require.EqualValues(t, 2048, usage["cache_creation_input_tokens"])
}

// TestKiroCacheTokens_E2E_StreamingPureCacheHit covers the warm-cache
// turn where the entire prompt is served from the prefix cache:
// uncachedInputTokens is 0, cacheReadInputTokens carries the full
// prompt, and cacheWriteInputTokens stays at 0. This is the case that
// produces the most dramatic token savings and the one the front-end
// cache-hit badge is designed to highlight, so it must round-trip
// cleanly.
func TestKiroCacheTokens_E2E_StreamingPureCacheHit(t *testing.T) {
	var wire bytes.Buffer
	wire.Write(buildEventStreamFrame(
		map[string]string{
			":message-type": "event",
			":event-type":   "assistantResponseEvent",
			":content-type": "application/json",
		},
		[]byte(`{"content":"OK."}`),
	))
	wire.Write(buildEventStreamFrame(
		map[string]string{
			":message-type": "event",
			":event-type":   "messageMetadataEvent",
			":content-type": "application/json",
		},
		[]byte(`{
			"tokenUsage": {
				"uncachedInputTokens": 0,
				"cacheReadInputTokens": 50000,
				"cacheWriteInputTokens": 0,
				"outputTokens": 8,
				"totalTokens": 50008
			}
		}`),
	))

	enc := NewAnthropicSSEEncoder(io.Discard, nil, "claude-sonnet-4.5")
	reader := NewEventStreamReader(&wire)
	for {
		frame, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		ev, err := ParseEventStreamFrame(frame)
		require.NoError(t, err)
		if ev != nil {
			require.NoError(t, enc.Emit(ev))
		}
	}

	require.Equal(t, int64(0), enc.InputTokens(),
		"pure cache hit reports zero uncached input — must NOT be backfilled by char estimation in the encoder")
	require.Equal(t, int64(50000), enc.CacheReadTokens(),
		"50k tokens served from cache must surface intact")
	require.Equal(t, int64(0), enc.CacheWriteTokens())
	require.Equal(t, int64(8), enc.OutputTokens())
}
