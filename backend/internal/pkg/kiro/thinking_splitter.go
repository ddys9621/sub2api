package kiro

import "strings"

// ThinkingSplitter converts a stream of raw assistant text into two
// logical streams: thinking fragments and regular content fragments. It
// recognises the four canonical opening tags used by different Kiro
// model variants:
//
//	<thinking>...</thinking>
//	<think>...</think>
//	<reasoning>...</reasoning>
//	<thought>...</thought>
//
// Any of those opens a thinking block; the matching `</tag>` closes it.
// Nested or malformed tags are treated as plain content. The splitter is
// resilient to tags split across chunks — a common occurrence with
// streaming token-level output.
//
// Usage:
//
//	s := &ThinkingSplitter{}
//	for chunk := range stream {
//	    for _, ev := range s.Feed(chunk) {
//	        enc.Emit(ev)
//	    }
//	}
//	for _, ev := range s.Flush() { enc.Emit(ev) } // on EOF
//
// Hard cut-off: when MaxThinkingBytes > 0, the splitter will force-close
// a thinking block that has produced that many bytes without a closing
// tag. This guards against runaway reasoning that would otherwise eat
// the entire output budget. Default of 0 disables the safeguard — the
// model is expected to respect the `max_thinking_length` hint in the
// injected preamble.
type ThinkingSplitter struct {
	inThinking bool
	// closeTag is the specific closing tag we are waiting for once
	// inThinking is true (e.g. "</think>" when the opener was "<think>").
	closeTag string
	// pending buffers trailing characters that could be the start of a
	// tag boundary.
	pending string
	// MaxThinkingBytes caps runaway thinking. 0 disables.
	MaxThinkingBytes int
	thinkingBytes    int
}

// thinkingTagPair ties a specific opening tag to its closing tag so the
// splitter emits balanced output regardless of which variant the model
// chose. Order matters: the first match wins, so longer tags must come
// before their shorter prefixes ("<thinking>" before "<think>").
type thinkingTagPair struct {
	open  string
	close string
}

var thinkingTagPairs = []thinkingTagPair{
	{open: "<thinking>", close: "</thinking>"},
	{open: "<reasoning>", close: "</reasoning>"},
	{open: "<thought>", close: "</thought>"},
	{open: "<think>", close: "</think>"},
}

// maxThinkingTagPrefix is the longest useful pending buffer length —
// anything longer can be safely flushed as plain content.
const maxThinkingTagPrefix = len("</thinking>")

// Feed consumes a single chunk of assistant text and returns any
// emit-ready StreamEvents.
func (s *ThinkingSplitter) Feed(chunk string) []*StreamEvent {
	if chunk == "" {
		return nil
	}
	buf := s.pending + chunk
	s.pending = ""
	return s.consume(buf, false)
}

// Flush should be called when the upstream stream ends.
func (s *ThinkingSplitter) Flush() []*StreamEvent {
	if s.pending == "" {
		return nil
	}
	out := s.consume(s.pending, true)
	s.pending = ""
	return out
}

func (s *ThinkingSplitter) consume(buf string, isFinal bool) []*StreamEvent {
	out := make([]*StreamEvent, 0, 2)
	for len(buf) > 0 {
		if s.inThinking {
			// Enforce the optional byte cap.
			if s.MaxThinkingBytes > 0 && s.thinkingBytes >= s.MaxThinkingBytes {
				s.inThinking = false
				closeTag := s.closeTag
				s.closeTag = ""
				if idx := strings.Index(buf, closeTag); idx >= 0 && closeTag != "" {
					if idx > 0 {
						out = appendContent(out, buf[:idx])
					}
					buf = buf[idx+len(closeTag):]
					continue
				}
				out = appendContent(out, buf)
				return out
			}
			idx := strings.Index(buf, s.closeTag)
			if idx < 0 {
				if isFinal {
					out = appendThinking(out, buf)
					s.thinkingBytes += len(buf)
					return out
				}
				safe, tail := splitOnTagBoundary(buf, s.closeTag)
				if safe != "" {
					if s.MaxThinkingBytes > 0 && s.thinkingBytes+len(safe) > s.MaxThinkingBytes {
						room := s.MaxThinkingBytes - s.thinkingBytes
						if room < 0 {
							room = 0
						}
						if room > 0 {
							out = appendThinking(out, safe[:room])
							s.thinkingBytes += room
						}
						out = appendContent(out, safe[room:])
						s.inThinking = false
						s.closeTag = ""
					} else {
						out = appendThinking(out, safe)
						s.thinkingBytes += len(safe)
					}
				}
				s.pending = tail
				return out
			}
			if idx > 0 {
				out = appendThinking(out, buf[:idx])
				s.thinkingBytes += idx
			}
			buf = buf[idx+len(s.closeTag):]
			s.inThinking = false
			s.closeTag = ""
			continue
		}

		// In content mode: find the earliest opening tag among all
		// recognised variants.
		earliestIdx := -1
		var matchedPair thinkingTagPair
		for _, p := range thinkingTagPairs {
			i := strings.Index(buf, p.open)
			if i < 0 {
				continue
			}
			if earliestIdx < 0 || i < earliestIdx {
				earliestIdx = i
				matchedPair = p
			}
		}
		if earliestIdx < 0 {
			if isFinal {
				out = appendContent(out, buf)
				return out
			}
			// Keep a safe tail against the most ambiguous prefix — the
			// union of all open tags' incremental prefixes. We pick the
			// longest suffix that could start any of them.
			safe, tail := splitOnAnyTagBoundary(buf, thinkingTagsOpenList())
			if safe != "" {
				out = appendContent(out, safe)
			}
			s.pending = tail
			return out
		}
		if earliestIdx > 0 {
			out = appendContent(out, buf[:earliestIdx])
		}
		buf = buf[earliestIdx+len(matchedPair.open):]
		s.inThinking = true
		s.closeTag = matchedPair.close
		s.thinkingBytes = 0
	}
	return out
}

// thinkingTagsOpenList returns just the opening tags for boundary-split
// purposes. Cached trivially — the list is tiny.
func thinkingTagsOpenList() []string {
	out := make([]string, len(thinkingTagPairs))
	for i, p := range thinkingTagPairs {
		out[i] = p.open
	}
	return out
}

// splitOnTagBoundary returns (safeToEmit, mustKeepAsPending). The tail
// portion is kept small — at most len(tag)-1 bytes — and only when the
// trailing bytes could plausibly be the start of tag.
func splitOnTagBoundary(buf, tag string) (string, string) {
	maxKeep := len(tag) - 1
	if maxKeep > maxThinkingTagPrefix {
		maxKeep = maxThinkingTagPrefix
	}
	if maxKeep <= 0 || len(buf) == 0 {
		return buf, ""
	}
	for keep := maxKeep; keep > 0; keep-- {
		if keep > len(buf) {
			continue
		}
		suffix := buf[len(buf)-keep:]
		if strings.HasPrefix(tag, suffix) {
			return buf[:len(buf)-keep], suffix
		}
	}
	return buf, ""
}

// splitOnAnyTagBoundary is like splitOnTagBoundary but tries every tag
// in the provided set and keeps the longest matching suffix so no
// boundary is missed even with multi-tag recognition.
func splitOnAnyTagBoundary(buf string, tags []string) (string, string) {
	bestKeep := 0
	for _, tag := range tags {
		maxKeep := len(tag) - 1
		if maxKeep > maxThinkingTagPrefix {
			maxKeep = maxThinkingTagPrefix
		}
		if maxKeep <= 0 {
			continue
		}
		for keep := maxKeep; keep > bestKeep; keep-- {
			if keep > len(buf) {
				continue
			}
			suffix := buf[len(buf)-keep:]
			if strings.HasPrefix(tag, suffix) {
				if keep > bestKeep {
					bestKeep = keep
				}
				break
			}
		}
	}
	if bestKeep == 0 {
		return buf, ""
	}
	return buf[:len(buf)-bestKeep], buf[len(buf)-bestKeep:]
}

func appendThinking(out []*StreamEvent, s string) []*StreamEvent {
	if s == "" {
		return out
	}
	if n := len(out); n > 0 && out[n-1].Kind == "thinking" {
		out[n-1].Text += s
		return out
	}
	return append(out, &StreamEvent{Kind: "thinking", Text: s})
}

func appendContent(out []*StreamEvent, s string) []*StreamEvent {
	if s == "" {
		return out
	}
	if n := len(out); n > 0 && out[n-1].Kind == "content" {
		out[n-1].Text += s
		return out
	}
	return append(out, &StreamEvent{Kind: "content", Text: s})
}

// defaultThinkingByteCap preserved for backwards compatibility with
// callers that configured an explicit cap. Setting this to 0 (the new
// default) disables the cap and lets the model-side
// `max_thinking_length` prompt handle budget control, matching how the
// reference kiro-gateway implementation operates.
const defaultThinkingByteCap = 0
