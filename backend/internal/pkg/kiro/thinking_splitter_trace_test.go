package kiro

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestThinkingSplitter_RealKiroChunking reproduces the exact chunking
// pattern observed from a Kiro claude-opus-4-7 response when thinking
// injection was active. The first chunk is just the literal "<th" —
// the splitter must buffer it and recognise the tag once the second
// chunk arrives carrying "inking>\n...".
func TestThinkingSplitter_RealKiroChunking(t *testing.T) {
	s := &ThinkingSplitter{}
	all := []*StreamEvent{}
	chunks := []string{
		"<th",
		"inking>\nThe",
		" user is asking in",
		" Chinese why",
		" the sky is blue.",
		"\n</thinking",
		">\n\n天空",
		"呈蓝色",
	}
	for _, c := range chunks {
		all = append(all, s.Feed(c)...)
	}
	all = append(all, s.Flush()...)

	require.Equal(t, "\nThe user is asking in Chinese why the sky is blue.\n",
		byKind(all, "thinking"),
		"thinking content should be extracted from <thinking>...</thinking>")
	require.Equal(t, "\n\n天空呈蓝色",
		byKind(all, "content"),
		"content after closing tag should stream through")
}
