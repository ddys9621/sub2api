package kiro

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// collectAll runs the splitter through a sequence of chunks plus a final
// Flush and returns the concatenated text of each kind in order.
func collectAll(chunks ...string) []*StreamEvent {
	s := &ThinkingSplitter{}
	out := []*StreamEvent{}
	for _, c := range chunks {
		out = append(out, s.Feed(c)...)
	}
	out = append(out, s.Flush()...)
	return out
}

// byKind returns the concatenated text for the given kind, in order.
func byKind(evs []*StreamEvent, kind string) string {
	var sb strings.Builder
	for _, e := range evs {
		if e.Kind == kind {
			sb.WriteString(e.Text)
		}
	}
	return sb.String()
}

func TestThinkingSplitter_PlainTextPassesThrough(t *testing.T) {
	out := collectAll("Hello ", "world")
	require.Equal(t, "Hello world", byKind(out, "content"))
	require.Empty(t, byKind(out, "thinking"))
}

func TestThinkingSplitter_SingleThinkingBlock(t *testing.T) {
	out := collectAll("Let me check. <thinking>analysing</thinking> Result: 42")
	require.Equal(t, "Let me check.  Result: 42", byKind(out, "content"))
	require.Equal(t, "analysing", byKind(out, "thinking"))
}

func TestThinkingSplitter_SplitAcrossChunks(t *testing.T) {
	// Each chunk only has a partial tag.
	out := collectAll("Before <thi", "nking>the secret</thi", "nking> after")
	require.Equal(t, "Before  after", byKind(out, "content"))
	require.Equal(t, "the secret", byKind(out, "thinking"))
}

func TestThinkingSplitter_BoundaryExactlyOnTag(t *testing.T) {
	out := collectAll("A", "<thinking>", "B", "</thinking>", "C")
	require.Equal(t, "AC", byKind(out, "content"))
	require.Equal(t, "B", byKind(out, "thinking"))
}

func TestThinkingSplitter_MultipleBlocks(t *testing.T) {
	out := collectAll(
		"Intro. <thinking>first</thinking> middle <thinking>second</thinking> end",
	)
	require.Equal(t, "Intro.  middle  end", byKind(out, "content"))
	require.Equal(t, "firstsecond", byKind(out, "thinking"))
}

func TestThinkingSplitter_UnclosedThinkingFlushesAsThinking(t *testing.T) {
	// Unterminated <thinking> means EOF arrives while we're still in
	// thinking mode — whatever we've accumulated should be emitted as
	// thinking rather than silently dropped.
	out := collectAll("Hi <thinking>still thinking")
	require.Equal(t, "Hi ", byKind(out, "content"))
	require.Equal(t, "still thinking", byKind(out, "thinking"))
}

func TestThinkingSplitter_StrayLessThanNotConfused(t *testing.T) {
	// "<" alone should not block emission forever.
	out := collectAll("1 < 2 is true")
	require.Equal(t, "1 < 2 is true", byKind(out, "content"))
	require.Empty(t, byKind(out, "thinking"))
}

func TestThinkingSplitter_CoalescesAdjacentSegmentsInSameFeed(t *testing.T) {
	// Within a single Feed invocation, the splitter coalesces adjacent
	// same-kind segments so the encoder sees one delta per boundary
	// rather than one per chunk.
	s := &ThinkingSplitter{}
	evs := s.Feed("abc<thinking>T1</thinking>def<thinking>T2</thinking>ghi")
	require.Len(t, evs, 5)
	require.Equal(t, "content", evs[0].Kind)
	require.Equal(t, "abc", evs[0].Text)
	require.Equal(t, "thinking", evs[1].Kind)
	require.Equal(t, "T1", evs[1].Text)
	require.Equal(t, "content", evs[2].Kind)
	require.Equal(t, "def", evs[2].Text)
	require.Equal(t, "thinking", evs[3].Kind)
	require.Equal(t, "T2", evs[3].Text)
	require.Equal(t, "content", evs[4].Kind)
	require.Equal(t, "ghi", evs[4].Text)
}

func TestThinkingSplitter_RecognisesThinkTag(t *testing.T) {
	out := collectAll("before <think>inner thought</think> after")
	require.Equal(t, "before  after", byKind(out, "content"))
	require.Equal(t, "inner thought", byKind(out, "thinking"))
}

func TestThinkingSplitter_RecognisesReasoningTag(t *testing.T) {
	out := collectAll("pre <reasoning>deliberating</reasoning> post")
	require.Equal(t, "pre  post", byKind(out, "content"))
	require.Equal(t, "deliberating", byKind(out, "thinking"))
}

func TestThinkingSplitter_RecognisesThoughtTag(t *testing.T) {
	out := collectAll("<thought>idea</thought>done")
	require.Equal(t, "done", byKind(out, "content"))
	require.Equal(t, "idea", byKind(out, "thinking"))
}

func TestThinkingSplitter_LongerTagBeatsShorterPrefix(t *testing.T) {
	// "<thinking>" must match before "<think>" when both exist as
	// prefixes — otherwise "<think>ing>" would parse as "<think>" +
	// content "ing>" which is wrong.
	out := collectAll("<thinking>body</thinking>rest")
	require.Equal(t, "rest", byKind(out, "content"))
	require.Equal(t, "body", byKind(out, "thinking"))
}

func TestThinkingSplitter_MixedTagsAcrossBlocks(t *testing.T) {
	out := collectAll(
		"Q. <think>plan A</think> answer <thinking>deeper plan</thinking> conclusion.",
	)
	require.Equal(t, "Q.  answer  conclusion.", byKind(out, "content"))
	require.Equal(t, "plan Adeeper plan", byKind(out, "thinking"))
}
