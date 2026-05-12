package kiro

import (
	"testing"
)

func TestBracketToolSplitter_RecognisesAllClaudeCodeTools(t *testing.T) {
	// Tool names harvested from a real Claude Code /v1/messages dump.
	// Every one of these must round-trip through the bracket splitter so
	// the downstream encoder can emit a real tool_use block and the user
	// does not see the raw "[tool_use ... {...}]" text.
	tools := []struct {
		name    string
		payload string
	}{
		{"Bash", `{"command":"curl -sL https://example.com","timeout":20000}`},
		{"Edit", `{"file_path":"/tmp/a.txt","old_string":"foo","new_string":"bar"}`},
		{"Read", `{"file_path":"/etc/hosts"}`},
		{"Write", `{"file_path":"/tmp/x","content":"hello"}`},
		{"Grep", `{"pattern":"TODO","path":"/src"}`},
		{"Glob", `{"pattern":"**/*.go"}`},
		{"WebFetch", `{"url":"https://cnn.com","prompt":"summarise"}`},
		{"WebSearch", `{"query":"hello world"}`},
		{"Task", `{"prompt":"run ci"}`},
		{"TaskCreate", `{"title":"x"}`},
		{"TaskGet", `{"id":"abc"}`},
		{"TaskList", `{}`},
		{"TaskStop", `{"id":"abc"}`},
		{"TaskUpdate", `{"id":"abc","title":"y"}`},
		{"TaskOutput", `{"id":"abc"}`},
		{"CronCreate", `{"schedule":"*/5 * * * *","command":"ls"}`},
		{"CronDelete", `{"id":"42"}`},
		{"CronList", `{}`},
		{"NotebookEdit", `{"path":"nb.ipynb","cell_id":"c1","content":"x"}`},
		{"Agent", `{"prompt":"do x"}`},
		{"AskUserQuestion", `{"question":"why?"}`},
		{"EnterPlanMode", `{}`},
		{"ExitPlanMode", `{"plan":"step 1"}`},
		{"EnterWorktree", `{"path":"/tmp/wt"}`},
		{"ExitWorktree", `{"path":"/tmp/wt"}`},
		{"Skill", `{"skill":"update-config"}`},
		{"ScheduleWakeup", `{"at":"2030-01-01T00:00:00Z"}`},
		{"RemoteTrigger", `{"target":"a","payload":{}}`},
		// MCP-style names with double underscores.
		{"mcp__claude_ai_Google_Drive__authenticate", `{"scope":"drive.file"}`},
		{"mcp__claude_ai_Google_Drive__complete_authentication", `{"code":"abc"}`},
	}

	for _, tc := range tools {
		text := "[tool_use " + tc.name + " " + tc.payload + "]"
		sp := NewBracketToolSplitter()
		events := sp.Feed(text)
		events = append(events, sp.Flush()...)

		if len(events) == 0 {
			t.Fatalf("tool %q: expected tool_use events, got none", tc.name)
		}
		var sawStart, sawStop bool
		for _, ev := range events {
			if ev.Kind == "tool_use_start" && ev.ToolName == tc.name {
				sawStart = true
			}
			if ev.Kind == "tool_use_stop" {
				sawStop = true
			}
		}
		if !sawStart {
			t.Errorf("tool %q: missing tool_use_start with matching name; got events=%+v",
				tc.name, events)
		}
		if !sawStop {
			t.Errorf("tool %q: missing tool_use_stop", tc.name)
		}
	}
}

func TestBracketToolSplitter_IgnoresNormalText(t *testing.T) {
	sp := NewBracketToolSplitter()
	events := sp.Feed("Here's a normal sentence without any tool invocations.")
	events = append(events, sp.Flush()...)
	if len(events) != 1 || events[0].Kind != "content" {
		t.Fatalf("plain text should pass through as a single content event; got %+v", events)
	}
}

func TestBracketToolSplitter_HandlesNestedJSONInToolInput(t *testing.T) {
	// The payload contains a stringified JSON object inside a value —
	// the bracket-matcher must count quotes correctly so the closing
	// '}' is only recognised at depth 0.
	payload := `{"command":"echo '{\"nested\": true}'","description":"test"}`
	sp := NewBracketToolSplitter()
	events := sp.Feed("[tool_use Bash " + payload + "]")
	events = append(events, sp.Flush()...)

	var gotStart bool
	for _, ev := range events {
		if ev.Kind == "tool_use_start" && ev.ToolName == "Bash" {
			gotStart = true
			if ev.ToolInput == "" {
				t.Fatalf("tool_use_start missing ToolInput")
			}
		}
	}
	if !gotStart {
		t.Fatalf("expected Bash tool_use_start; got %+v", events)
	}
}
