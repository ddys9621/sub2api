package kiro

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// withNativeThinking flips the encoder to native thinking_delta output
// for the duration of fn. The default ships thinking as a markdown
// blockquote text block (see thinkingAsText), which is what Claude
// Code actually renders; the native path is a fallback for clients
// that parse `thinking` blocks directly.
func withNativeThinking(t *testing.T, fn func()) {
	t.Helper()
	orig := thinkingAsText
	thinkingAsText = false
	t.Cleanup(func() { thinkingAsText = orig })
	fn()
}

// TestEncoder_ThinkingThenText_Native covers the native-thinking
// happy path: a thinking block followed by a regular text block in
// the same message. Every SSE content_block_start must be paired
// with a content_block_stop, and indices must strictly increase by
// one.
func TestEncoder_ThinkingThenText_Native(t *testing.T) {
	withNativeThinking(t, func() {
		var out bytes.Buffer
		enc := NewAnthropicSSEEncoder(&out, nil, "claude-opus-4-7")
		require.NoError(t, enc.Emit(&StreamEvent{Kind: "thinking", Text: "deliberating"}))
		require.NoError(t, enc.Emit(&StreamEvent{Kind: "content", Text: "Answer."}))
		require.NoError(t, enc.Finish("end_turn"))

		sse := out.String()
		require.Contains(t, sse, `"content_block_start"`)
		require.Contains(t, sse, `"type":"thinking"`)
		require.Contains(t, sse, `"thinking_delta"`)
		require.Contains(t, sse, `"text_delta"`)
		require.Contains(t, reassembleTextDeltas(sse), `Answer.`)

		starts := strings.Count(sse, `"type":"content_block_start"`)
		stops := strings.Count(sse, `"type":"content_block_stop"`)
		require.Equal(t, starts, stops, "starts == stops\nSSE:\n%s", sse)
		require.Equal(t, 2, starts, "expected 2 blocks, got:\n%s", sse)

		require.Equal(t, 1, strings.Count(sse, `"type":"message_delta"`))
		require.Equal(t, 1, strings.Count(sse, `"type":"message_stop"`))
	})
}

// TestEncoder_ThinkingOnlyThenFinish_Native — only a thinking block,
// no text. Finish must still close the thinking block cleanly.
func TestEncoder_ThinkingOnlyThenFinish_Native(t *testing.T) {
	withNativeThinking(t, func() {
		var out bytes.Buffer
		enc := NewAnthropicSSEEncoder(&out, nil, "claude-opus-4-7")
		require.NoError(t, enc.Emit(&StreamEvent{Kind: "thinking", Text: "just planning"}))
		require.NoError(t, enc.Finish("end_turn"))
		sse := out.String()
		require.Equal(t, strings.Count(sse, `"type":"content_block_start"`), strings.Count(sse, `"type":"content_block_stop"`))
		require.Contains(t, sse, `just planning`)
	})
}

// TestEncoder_ThinkingTextThinkingText_Native — interleaved kinds,
// each switch must close the previous block and open a new one.
func TestEncoder_ThinkingTextThinkingText_Native(t *testing.T) {
	withNativeThinking(t, func() {
		var out bytes.Buffer
		enc := NewAnthropicSSEEncoder(&out, nil, "claude-opus-4-7")
		require.NoError(t, enc.Emit(&StreamEvent{Kind: "thinking", Text: "t1"}))
		require.NoError(t, enc.Emit(&StreamEvent{Kind: "content", Text: "c1"}))
		require.NoError(t, enc.Emit(&StreamEvent{Kind: "thinking", Text: "t2"}))
		require.NoError(t, enc.Emit(&StreamEvent{Kind: "content", Text: "c2"}))
		require.NoError(t, enc.Finish("end_turn"))
		sse := out.String()
		require.Equal(t, 4, strings.Count(sse, `"type":"content_block_start"`))
		require.Equal(t, 4, strings.Count(sse, `"type":"content_block_stop"`))
	})
}

// TestEncoder_ThinkingAsBlockquote_Default verifies the default
// behaviour where thinking content is rendered as a markdown
// blockquote text block — the shape Claude Code actually displays.
// The thinking block is followed by a separate text block for the
// real answer, split via content_block_stop/start so the client
// repaints the thinking section before the answer finishes.
func TestEncoder_ThinkingAsBlockquote_Default(t *testing.T) {
	var out bytes.Buffer
	enc := NewAnthropicSSEEncoder(&out, nil, "claude-opus-4-7")
	require.NoError(t, enc.Emit(&StreamEvent{Kind: "thinking", Text: "deliberating"}))
	require.NoError(t, enc.Emit(&StreamEvent{Kind: "content", Text: "Answer."}))
	require.NoError(t, enc.Finish("end_turn"))

	sse := out.String()
	// Thinking goes out as blockquote markdown, not as a native
	// `thinking` block.
	require.NotContains(t, sse, `"type":"thinking"`)
	reassembled := reassembleTextDeltas(sse)
	require.Contains(t, reassembled, "💭 Thinking")
	require.Contains(t, reassembled, `deliberating`)
	require.Contains(t, reassembled, `Answer.`)

	// Two text blocks: blockquote + answer. Splitting them forces
	// Claude Code's TUI to paint the thinking section before the
	// answer completes.
	starts := strings.Count(sse, `"type":"content_block_start"`)
	stops := strings.Count(sse, `"type":"content_block_stop"`)
	require.Equal(t, starts, stops)
	require.Equal(t, 2, starts)
}
