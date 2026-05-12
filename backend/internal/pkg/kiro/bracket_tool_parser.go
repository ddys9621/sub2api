package kiro

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// bracketToolCall captures one tool call extracted from assistant plain
// text. Kiro CodeWhisperer sometimes renders tool invocations as literal
// text blocks instead of proper toolUseEvent frames — for example:
//
//	[tool_use Bash {"command":"curl -sL https://...", ...}]
//	[Called get_weather with args: {"location":"Moscow"}]
//
// When that happens, Claude Code receives the text verbatim and the user
// sees JSON in the terminal instead of an executed tool call. We detect
// both shapes post-hoc, strip them from the visible text, and re-emit
// them as structured tool_use blocks.
type bracketToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// bracketPatterns lists the anchor prefixes we recognise. Each entry is
// (prefix literal, regex to match up to the opening brace so we can
// capture the tool name). Matching is case-insensitive.
type bracketSpec struct {
	prefix string
	re     *regexp.Regexp
}

var bracketSpecs = []bracketSpec{
	// Kiro's common rendering:  [tool_use ToolName {...json...}]
	// The second token is the tool name; payload starts at the first '{'.
	{
		prefix: "[tool_use ",
		re:     regexp.MustCompile(`(?i)\[tool_use\s+([A-Za-z_][A-Za-z0-9_-]*)\s*`),
	},
	// Legacy shape observed on older Kiro models (matches kiro-gateway-ref):
	//   [Called func_name with args: {...}]
	{
		prefix: "[Called ",
		re:     regexp.MustCompile(`(?i)\[Called\s+([A-Za-z_][A-Za-z0-9_-]*)\s+with\s+args:\s*`),
	},
}

// parseBracketToolCalls scans the assistant text for bracket-style tool
// calls and returns them in the order they appeared. Input JSON objects
// are parsed; malformed ones are skipped.
func parseBracketToolCalls(text string) []bracketToolCall {
	if !textContainsAnyPrefix(text, bracketSpecs) {
		return nil
	}
	var out []bracketToolCall
	for _, spec := range bracketSpecs {
		for _, loc := range spec.re.FindAllStringSubmatchIndex(text, -1) {
			if len(loc) < 4 {
				continue
			}
			// loc[0..1] = whole match; loc[2..3] = first capture (name)
			name := text[loc[2]:loc[3]]
			jsonStart := findOpeningBrace(text, loc[1])
			if jsonStart == -1 {
				continue
			}
			jsonEnd := findMatchingBrace(text, jsonStart)
			if jsonEnd == -1 {
				continue
			}
			raw := text[jsonStart : jsonEnd+1]
			var args map[string]any
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				continue
			}
			out = append(out, bracketToolCall{
				ID:    generateBracketToolID(),
				Name:  name,
				Input: args,
			})
		}
	}
	return out
}

// stripBracketToolCalls removes any bracket-style tool invocations from
// the text. Useful for cleaning up the visible text stream after
// re-emitting the calls as structured tool_use blocks.
func stripBracketToolCalls(text string) string {
	if !textContainsAnyPrefix(text, bracketSpecs) {
		return text
	}
	result := text
	// Iterate to a fixed point so overlapping calls all get cleaned.
	for changed := true; changed; {
		changed = false
		for _, spec := range bracketSpecs {
			loc := spec.re.FindStringIndex(result)
			if loc == nil {
				continue
			}
			jsonStart := findOpeningBrace(result, loc[1])
			if jsonStart == -1 {
				continue
			}
			jsonEnd := findMatchingBrace(result, jsonStart)
			if jsonEnd == -1 {
				continue
			}
			// Trim to closing ']' if present immediately after the JSON.
			cut := jsonEnd + 1
			if cut < len(result) && result[cut] == ']' {
				cut++
			}
			result = result[:loc[0]] + result[cut:]
			changed = true
		}
	}
	return strings.TrimSpace(result)
}

func textContainsAnyPrefix(text string, specs []bracketSpec) bool {
	lower := strings.ToLower(text)
	for _, s := range specs {
		if strings.Contains(lower, strings.ToLower(s.prefix)) {
			return true
		}
	}
	return false
}

// findOpeningBrace returns the index of the first '{' or '[' at or after
// `from`, skipping any whitespace. Returns -1 if none is found.
func findOpeningBrace(s string, from int) int {
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '{', '[':
			return i
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return -1
		}
	}
	return -1
}

// findMatchingBrace scans forward from an opening '{' or '[' and returns
// the index of the matching close, respecting nested objects/arrays and
// JSON string literals (including escapes). Returns -1 on mismatch.
func findMatchingBrace(s string, start int) int {
	if start >= len(s) {
		return -1
	}
	open := s[start]
	var closeCh byte
	switch open {
	case '{':
		closeCh = '}'
	case '[':
		closeCh = ']'
	default:
		return -1
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if inStr {
			switch c {
			case '\\':
				escape = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// generateBracketToolID synthesises an Anthropic-style tool_use id for a
// synthetic tool call recovered from plain text.
func generateBracketToolID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read effectively cannot fail on modern OSes; fall back to
		// a constant so tests stay deterministic rather than panicking.
		for i := range buf {
			buf[i] = 0
		}
	}
	return "toolu_br_" + hex.EncodeToString(buf)
}


// BracketToolSplitter buffers incoming assistant text, detects bracket
// tool_use shapes, and splits the stream into (sanitised content) +
// (synthetic tool_use_{start,stop}) events. Mirrors ThinkingSplitter's
// feed/flush contract so it can be plugged into DriveEventStream.
//
// Detection is greedy: as soon as we see a prefix like "[tool_use "
// anywhere in the buffer we wait for a matching closing ']' before
// committing. If no valid shape materialises within maxBufferedBytes we
// flush the buffered text as plain content so a malformed-looking line
// is never silently dropped.
type BracketToolSplitter struct {
	buf                 strings.Builder
	maxBufferedBytes    int
	pendingAnchorOffset int // index into buf of the earliest anchor seen; -1 if none
}

// NewBracketToolSplitter returns a splitter with reasonable defaults.
func NewBracketToolSplitter() *BracketToolSplitter {
	return &BracketToolSplitter{
		maxBufferedBytes:    64 * 1024,
		pendingAnchorOffset: -1,
	}
}

// Feed consumes a chunk of assistant text and returns zero or more
// StreamEvents ready for downstream emission. Events preserve ordering:
// every synthetic tool_use lifecycle is framed between the content it
// replaced.
func (b *BracketToolSplitter) Feed(chunk string) []*StreamEvent {
	if chunk == "" {
		return nil
	}
	b.buf.WriteString(chunk)
	return b.extract(false)
}

// Flush returns any remaining events after the upstream stream ends.
// Any buffered text that does not resolve into a tool_use is returned
// verbatim as a final content event.
func (b *BracketToolSplitter) Flush() []*StreamEvent {
	return b.extract(true)
}

// extract is the core state machine. When final=true we no longer wait
// for more data — unmatched bracket shapes are released as plain text.
func (b *BracketToolSplitter) extract(final bool) []*StreamEvent {
	if b.buf.Len() == 0 {
		return nil
	}
	var out []*StreamEvent
	for {
		text := b.buf.String()
		anchorIdx, anchorPrefix := earliestAnchor(text)
		if anchorIdx < 0 {
			if text == "" {
				return out
			}
			if final {
				out = append(out, &StreamEvent{Kind: "content", Text: text})
				b.buf.Reset()
				return out
			}
			// Keep a small safety tail in case a prefix is split across
			// chunk boundaries (e.g. "[tool_u" + "se Bash"). Release
			// everything else as content right now — if no tail
			// matches, the entire buffer flushes.
			tail := shortestUnmatchedPrefixTail(text)
			if tail < 0 || tail > len(text) {
				tail = 0
			}
			if tail < len(text) {
				out = append(out, &StreamEvent{Kind: "content", Text: text[:len(text)-tail]})
			}
			b.buf.Reset()
			if tail > 0 {
				b.buf.WriteString(text[len(text)-tail:])
			}
			return out
		}
		// Emit everything before the anchor as plain content.
		if anchorIdx > 0 {
			out = append(out, &StreamEvent{Kind: "content", Text: text[:anchorIdx]})
		}
		// Now try to parse the bracket shape starting at anchorIdx.
		rest := text[anchorIdx:]
		call, consumed, complete := tryConsumeBracketCall(rest, anchorPrefix)
		if !complete {
			if final {
				// Give up: leak the pending text verbatim so user sees it.
				out = append(out, &StreamEvent{Kind: "content", Text: rest})
				b.buf.Reset()
				return out
			}
			if b.buf.Len() >= b.maxBufferedBytes {
				out = append(out, &StreamEvent{Kind: "content", Text: rest})
				b.buf.Reset()
				return out
			}
			b.buf.Reset()
			b.buf.WriteString(rest)
			return out
		}
		// We have a complete bracket call. Emit synthetic tool_use.
		out = append(out, bracketCallToEvents(call)...)
		// Advance the buffer past the consumed bytes.
		b.buf.Reset()
		b.buf.WriteString(rest[consumed:])
	}
}

// earliestAnchor returns the index of the first bracket-tool prefix in
// text along with the spec whose prefix matched; -1/"" if none.
func earliestAnchor(text string) (int, string) {
	earliest := -1
	var prefix string
	lower := strings.ToLower(text)
	for _, s := range bracketSpecs {
		if idx := strings.Index(lower, strings.ToLower(s.prefix)); idx >= 0 {
			if earliest < 0 || idx < earliest {
				earliest = idx
				prefix = s.prefix
			}
		}
	}
	return earliest, prefix
}

// tryConsumeBracketCall tries to parse text (which starts with an anchor
// prefix) into a bracket call. Returns the call, how many bytes were
// consumed (including the closing ']'), and whether parsing reached
// completion.
func tryConsumeBracketCall(text, prefix string) (bracketToolCall, int, bool) {
	var spec bracketSpec
	for _, s := range bracketSpecs {
		if strings.EqualFold(s.prefix, prefix) {
			spec = s
			break
		}
	}
	if spec.re == nil {
		return bracketToolCall{}, 0, false
	}
	loc := spec.re.FindStringSubmatchIndex(text)
	if loc == nil || loc[0] != 0 {
		return bracketToolCall{}, 0, false
	}
	name := text[loc[2]:loc[3]]
	jsonStart := findOpeningBrace(text, loc[1])
	if jsonStart == -1 {
		return bracketToolCall{}, 0, false
	}
	jsonEnd := findMatchingBrace(text, jsonStart)
	if jsonEnd == -1 {
		// JSON not closed yet; wait for more data.
		return bracketToolCall{}, 0, false
	}
	raw := text[jsonStart : jsonEnd+1]
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return bracketToolCall{}, 0, false
	}
	consumed := jsonEnd + 1
	if consumed < len(text) && text[consumed] == ']' {
		consumed++
	}
	return bracketToolCall{
		ID:    generateBracketToolID(),
		Name:  name,
		Input: args,
	}, consumed, true
}

// bracketCallToEvents expands one bracket call into the three stream
// events an encoder needs (start + delta + stop).
func bracketCallToEvents(call bracketToolCall) []*StreamEvent {
	inputJSON, _ := json.Marshal(call.Input)
	return []*StreamEvent{
		{
			Kind:      "tool_use_start",
			ToolUseID: call.ID,
			ToolName:  call.Name,
			ToolInput: string(inputJSON),
		},
		{
			Kind:      "tool_use_stop",
			ToolUseID: call.ID,
		},
	}
}

// shortestUnmatchedPrefixTail returns the length of the shortest tail of
// text that *might* be the start of a bracket prefix and therefore
// should be held back across chunk boundaries. Returns 0 when there is
// no partial match.
func shortestUnmatchedPrefixTail(text string) int {
	for _, s := range bracketSpecs {
		p := s.prefix
		// Try every suffix of `text` that starts with '[' and is a
		// proper prefix of the anchor literal.
		for n := 1; n < len(p); n++ {
			if len(text) < n {
				break
			}
			tail := text[len(text)-n:]
			if strings.HasPrefix(p, tail) {
				if n > 0 {
					return n
				}
			}
		}
	}
	return 0
}
