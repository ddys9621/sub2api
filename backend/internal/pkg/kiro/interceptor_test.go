package kiro

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeInterceptor is a stub ToolCallInterceptor used to verify that
// DriveEventStreamToAnthropicWithInterceptor hides the intercepted tool_use
// lifecycle from the encoder and emits replacement events instead.
type fakeInterceptor struct {
	matchName  string
	seenInput  string
	replaceTxt string
}

func (f *fakeInterceptor) OnToolStart(_ context.Context, ev *StreamEvent) bool {
	return ev != nil && ev.ToolName == f.matchName
}

func (f *fakeInterceptor) OnToolStop(_ context.Context, _, _ string, input string, emit func(*StreamEvent) error) error {
	f.seenInput = input
	return emit(&StreamEvent{Kind: "content", Text: f.replaceTxt})
}

// TestDriveEventStream_InterceptorSwallowsToolUseAndSubstitutesText verifies
// that when a tool_use lifecycle is intercepted, the encoder never sees any
// content_block_start/delta/stop frames for the tool — only the replacement
// text event is forwarded.
func TestDriveEventStream_InterceptorSwallowsToolUseAndSubstitutesText(t *testing.T) {
	stream := concat(
		testFrameJSON("assistantResponseEvent", map[string]any{"content": "Let me search."}),
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "t1", "name": "web_search", "input": `{"query":"`,
		}),
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "t1", "input": `kiro"}`,
		}),
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "t1", "stop": true,
		}),
		testFrameJSON("assistantResponseEvent", map[string]any{"content": "Done."}),
	)

	var out bytes.Buffer
	enc := NewAnthropicSSEEncoder(&out, nil, "claude-sonnet-4.5")

	interceptor := &fakeInterceptor{matchName: "web_search", replaceTxt: "[search results]"}
	text, err := DriveEventStreamToAnthropicWithInterceptor(
		context.Background(), bytes.NewReader(stream), enc, interceptor,
	)
	require.NoError(t, err)
	require.NoError(t, enc.Finish("end_turn"))

	require.Equal(t, `{"query":"kiro"}`, interceptor.seenInput,
		"interceptor must receive reassembled tool input JSON")

	require.Contains(t, text, "Let me search.")
	require.Contains(t, text, "[search results]")
	require.Contains(t, text, "Done.")

	sse := out.String()
	require.NotContains(t, sse, `"type":"tool_use"`,
		"tool_use block leaked to client SSE")
	require.NotContains(t, sse, "input_json_delta",
		"partial_json leaked to client SSE")
	require.Contains(t, reassembleTextDeltas(sse), "[search results]")
}

// TestDriveEventStream_MultipleStartFramesAccumulateInput verifies the
// real Kiro wire format: every toolUseEvent frame carries the `name`
// field, even the "delta" frames. The driver treats the first frame as
// a start and subsequent same-id frames as tool_use_delta, so the input
// gets reassembled correctly.
func TestDriveEventStream_MultipleStartFramesAccumulateInput(t *testing.T) {
	stream := concat(
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "tX", "name": "web_search", "input": `{"query":"today`,
		}),
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "tX", "name": "web_search", "input": ` news`,
		}),
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "tX", "name": "web_search", "input": ` 2026"}`,
		}),
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "tX", "stop": true,
		}),
	)

	var out bytes.Buffer
	enc := NewAnthropicSSEEncoder(&out, nil, "claude-sonnet-4.5")

	interceptor := &fakeInterceptor{matchName: "web_search", replaceTxt: "OK"}
	_, err := DriveEventStreamToAnthropicWithInterceptor(
		context.Background(), bytes.NewReader(stream), enc, interceptor,
	)
	require.NoError(t, err)
	require.Equal(t, `{"query":"today news 2026"}`, interceptor.seenInput)
}

// TestDriveEventStream_ThinkingSplitterRewritesTags verifies that raw
// assistant text containing <thinking>...</thinking> blocks is rewritten
// into separate SSE thinking + text content blocks by the driver's
// splitter, with tool_use frames still flowing through normally.
func TestDriveEventStream_ThinkingSplitterRewritesTags(t *testing.T) {
	orig := thinkingAsText
	thinkingAsText = false
	defer func() { thinkingAsText = orig }()

	stream := concat(
		testFrameJSON("assistantResponseEvent", map[string]any{
			"content": "<thinking>plan: check docs</thinking>Here's the answer.",
		}),
	)

	var out bytes.Buffer
	enc := NewAnthropicSSEEncoder(&out, nil, "claude-sonnet-4.5")
	_, err := DriveEventStreamToAnthropicWithInterceptor(
		context.Background(), bytes.NewReader(stream), enc, nil,
	)
	require.NoError(t, err)
	require.NoError(t, enc.Finish("end_turn"))

	sse := out.String()
	require.Contains(t, sse, `"type":"thinking"`,
		"thinking content_block should be emitted")
	require.Contains(t, sse, `"type":"thinking_delta"`,
		"thinking_delta should be emitted")
	require.Contains(t, reassembleThinkingDeltas(sse), "plan: check docs")
	require.Contains(t, sse, `"type":"text"`, "text content_block should follow")
	require.Contains(t, reassembleTextDeltas(sse), "Here's the answer.")
	require.NotContains(t, sse, "<thinking>",
		"raw <thinking> tag must not leak to the client")
}

// TestDriveEventStream_EOFFlushFinalisesPendingInterceptedTool verifies
// that if Kiro ends the stream without an explicit tool_use_stop frame,
// the driver still flushes the interceptor so the client sees a response.
func TestDriveEventStream_EOFFlushFinalisesPendingInterceptedTool(t *testing.T) {
	stream := concat(
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "tE", "name": "web_search", "input": `{"query":"x"}`,
		}),
		// No explicit stop frame; EOF follows immediately.
	)

	var out bytes.Buffer
	enc := NewAnthropicSSEEncoder(&out, nil, "claude-sonnet-4.5")

	interceptor := &fakeInterceptor{matchName: "web_search", replaceTxt: "[flushed]"}
	_, err := DriveEventStreamToAnthropicWithInterceptor(
		context.Background(), bytes.NewReader(stream), enc, interceptor,
	)
	require.NoError(t, err)
	require.NoError(t, enc.Finish("end_turn"))

	require.Equal(t, `{"query":"x"}`, interceptor.seenInput)
	require.Contains(t, reassembleTextDeltas(out.String()), "[flushed]")
}

// TestDriveEventStream_NonMatchingInterceptorPassesThrough verifies that
// when the interceptor returns handled=false, the tool_use lifecycle is
// emitted to the client unchanged.
func TestDriveEventStream_NonMatchingInterceptorPassesThrough(t *testing.T) {
	stream := concat(
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "t9", "name": "read_file", "input": `{"path":"/tmp"}`,
		}),
		testFrameJSON("toolUseEvent", map[string]any{
			"toolUseId": "t9", "stop": true,
		}),
	)

	var out bytes.Buffer
	enc := NewAnthropicSSEEncoder(&out, nil, "claude-sonnet-4.5")

	interceptor := &fakeInterceptor{matchName: "web_search", replaceTxt: "SHOULD NOT APPEAR"}
	_, err := DriveEventStreamToAnthropicWithInterceptor(
		context.Background(), bytes.NewReader(stream), enc, interceptor,
	)
	require.NoError(t, err)
	require.NoError(t, enc.Finish("end_turn"))

	sse := out.String()
	require.Contains(t, sse, `"type":"tool_use"`,
		"non-matching tool_use should pass through")
	require.Contains(t, sse, `"name":"read_file"`)
	require.NotContains(t, sse, "SHOULD NOT APPEAR")
}

// --- Helpers ---

// testFrameJSON encodes a single AWS EventStream frame with the standard
// two headers (:message-type="event", :event-type=<t>) and a JSON payload.
// Kept inline (rather than shared with eventstream_test.go) so this test
// file does not need the `unit` build tag.
func testFrameJSON(eventType string, payload any) []byte {
	raw, _ := json.Marshal(payload)
	return testBuildEventStreamFrame(map[string]string{
		":message-type": "event",
		":event-type":   eventType,
		":content-type": "application/json",
	}, raw)
}

func testBuildEventStreamFrame(headers map[string]string, payload []byte) []byte {
	var hbuf bytes.Buffer
	for name, value := range headers {
		hbuf.WriteByte(byte(len(name)))
		hbuf.WriteString(name)
		hbuf.WriteByte(0x07) // string header type
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(value)))
		hbuf.Write(lb[:])
		hbuf.WriteString(value)
	}

	hraw := hbuf.Bytes()
	totalLen := uint32(12 + len(hraw) + len(payload) + 4)
	headersLen := uint32(len(hraw))

	var prelude [8]byte
	binary.BigEndian.PutUint32(prelude[0:4], totalLen)
	binary.BigEndian.PutUint32(prelude[4:8], headersLen)
	preludeCRC := crc32.ChecksumIEEE(prelude[:])

	var out bytes.Buffer
	out.Write(prelude[:])
	_ = binary.Write(&out, binary.BigEndian, preludeCRC)
	out.Write(hraw)
	out.Write(payload)

	msgCRC := crc32.ChecksumIEEE(out.Bytes())
	_ = binary.Write(&out, binary.BigEndian, msgCRC)
	return out.Bytes()
}

func concat(chunks ...[]byte) []byte {
	var buf bytes.Buffer
	for _, c := range chunks {
		buf.Write(c)
	}
	return buf.Bytes()
}

// reassembleTextDeltas walks the SSE dump produced by the encoder and
// concatenates every `text_delta` payload so tests can match against
// the logical text independent of how the encoder sliced it into
// frames. emitTextDeltaByRune chops long chunks into rune-sized
// windows, so a naive `strings.Contains(sse, "<whole text>")` fails
// even though the user sees the full string.
func reassembleTextDeltas(sse string) string {
	return reassembleDeltas(sse, "text_delta", "text")
}

// reassembleThinkingDeltas is the thinking-block equivalent of
// reassembleTextDeltas.
func reassembleThinkingDeltas(sse string) string {
	return reassembleDeltas(sse, "thinking_delta", "thinking")
}

// reassembleDeltas scans SSE lines for `content_block_delta` frames of
// the given delta type and returns the concatenated field value.
func reassembleDeltas(sse, deltaType, fieldName string) string {
	var buf bytes.Buffer
	for _, line := range bytesSplitLines(sse) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		raw := bytes.TrimPrefix(line, []byte("data: "))
		var obj struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		if obj.Type != "content_block_delta" || obj.Delta.Type != deltaType {
			continue
		}
		switch fieldName {
		case "text":
			buf.WriteString(obj.Delta.Text)
		case "thinking":
			buf.WriteString(obj.Delta.Thinking)
		}
	}
	return buf.String()
}

func bytesSplitLines(s string) [][]byte {
	return bytes.Split([]byte(s), []byte("\n"))
}
