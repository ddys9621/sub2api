package kiro

import (
	"encoding/json"
	"testing"
)

// TestAddKiroCachePoints_NilPayload_NoOp guards against panics when
// callers feed in a nil payload — addKiroCachePoints should silently
// return rather than dereferencing.
func TestAddKiroCachePoints_NilPayload_NoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("addKiroCachePoints(nil) panicked: %v", r)
		}
	}()
	addKiroCachePoints(nil)
}

// TestAddKiroCachePoints_MissingConversationState_NoOp covers the
// defensive branch that returns early when the payload shape does not
// match BuildKiroPayload's output.
func TestAddKiroCachePoints_MissingConversationState_NoOp(t *testing.T) {
	payload := map[string]any{"profileArn": "arn:test"}
	addKiroCachePoints(payload)
	if _, ok := payload["conversationState"]; ok {
		t.Fatalf("addKiroCachePoints should not synthesise missing conversationState")
	}
}

// TestAddKiroCachePoints_AlwaysSetsCurrentMessageCachePoint verifies the
// unconditional "last message" breakpoint mirroring Anthropic's
// addMessageCacheBreakpoints behaviour on messages[-1].
func TestAddKiroCachePoints_AlwaysSetsCurrentMessageCachePoint(t *testing.T) {
	payload := newTestPayload(nil, nil)
	addKiroCachePoints(payload)

	uim := mustCurrentUserInputMessage(t, payload)
	cp, _ := uim["cachePoint"].(map[string]any)
	if cp == nil {
		t.Fatalf("currentMessage.userInputMessage.cachePoint should be set, got %#v", uim)
	}
	if got, want := cp["type"], "default"; got != want {
		t.Errorf("currentMessage cachePoint.type = %v, want %q", got, want)
	}
}

// TestAddKiroCachePoints_AppendsToolsCachePointWhenToolsPresent verifies
// the tools[-1] equivalent: a {cachePoint:{type:"default"}} marker is
// appended after the last toolSpecification entry.
func TestAddKiroCachePoints_AppendsToolsCachePointWhenToolsPresent(t *testing.T) {
	tools := []any{
		map[string]any{"toolSpecification": map[string]any{"name": "Read"}},
		map[string]any{"toolSpecification": map[string]any{"name": "Grep"}},
	}
	payload := newTestPayload(nil, tools)
	addKiroCachePoints(payload)

	uim := mustCurrentUserInputMessage(t, payload)
	ctx, _ := uim["userInputMessageContext"].(map[string]any)
	got, _ := ctx["tools"].([]any)
	if len(got) != 3 {
		t.Fatalf("expected tools length 3 after cachePoint append, got %d: %#v", len(got), got)
	}
	tail, _ := got[len(got)-1].(map[string]any)
	cp, ok := tail["cachePoint"].(map[string]any)
	if !ok {
		t.Fatalf("expected last tools entry to be cachePoint marker, got %#v", tail)
	}
	if cp["type"] != "default" {
		t.Errorf("tools cachePoint.type = %v, want %q", cp["type"], "default")
	}
	// First two entries must remain pristine toolSpecification objects.
	for i := 0; i < 2; i++ {
		entry, _ := got[i].(map[string]any)
		if _, ok := entry["toolSpecification"]; !ok {
			t.Errorf("tools[%d] should still be a toolSpecification, got %#v", i, entry)
		}
	}
}

// TestAddKiroCachePoints_OmitsToolsCachePointWhenEmpty makes sure we do
// not materialise an empty tools array (or a tools entry containing only
// a cachePoint) when no tool was submitted — Kiro upstream rejects
// payloads with stray cache markers.
func TestAddKiroCachePoints_OmitsToolsCachePointWhenEmpty(t *testing.T) {
	payload := newTestPayload(nil, nil)
	addKiroCachePoints(payload)

	uim := mustCurrentUserInputMessage(t, payload)
	if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok {
		if tools, present := ctx["tools"]; present {
			t.Errorf("tools should remain absent when none were submitted, got %#v", tools)
		}
	}
}

// TestAddKiroCachePoints_MarksFirstUserInHistory anchors the "system
// prompt block" equivalent breakpoint: the first user message in history
// carries the merged system prompt and is the longest stable prefix.
func TestAddKiroCachePoints_MarksFirstUserInHistory(t *testing.T) {
	history := []any{
		userTurn("first user with system prompt merged"),
		assistantTurn("hi"),
	}
	payload := newTestPayload(history, nil)
	addKiroCachePoints(payload)

	first, _ := history[0].(map[string]any)
	firstUIM, _ := first["userInputMessage"].(map[string]any)
	if _, ok := firstUIM["cachePoint"]; !ok {
		t.Fatalf("expected first user turn in history to carry cachePoint, got %#v", firstUIM)
	}
	// The assistant entry must remain untouched.
	asst, _ := history[1].(map[string]any)
	asstMsg, _ := asst["assistantResponseMessage"].(map[string]any)
	if _, ok := asstMsg["cachePoint"]; ok {
		t.Errorf("addKiroCachePoints should not mark assistant turns, got %#v", asstMsg)
	}
}

// TestAddKiroCachePoints_SecondToLastUserTurn_OnlyWhenTotalGE4 mirrors
// the Anthropic gate `len(messages) >= 4`. With 3 total turns (history=2 +
// current=1) we expect ONLY the first-user + current breakpoints; with 4+
// total turns we also expect the history's last user turn to be marked.
func TestAddKiroCachePoints_SecondToLastUserTurn_OnlyWhenTotalGE4(t *testing.T) {
	// Case A: total = 3 (history 2 + current 1) → no second breakpoint.
	historyA := []any{
		userTurn("u1"),
		assistantTurn("a1"),
	}
	payloadA := newTestPayload(historyA, nil)
	addKiroCachePoints(payloadA)
	if _, ok := userTurnCachePoint(historyA, 0); !ok {
		t.Errorf("case A: expected first-user cachePoint on history[0]")
	}
	if cp, ok := userTurnCachePoint(historyA, 1); ok {
		t.Errorf("case A: history[1] is assistant, should not have userInputMessage cachePoint, got %#v", cp)
	}

	// Case B: total = 5 (history 4 + current 1) → second breakpoint expected.
	historyB := []any{
		userTurn("u1"),
		assistantTurn("a1"),
		userTurn("u2"),
		assistantTurn("a2"),
	}
	payloadB := newTestPayload(historyB, nil)
	addKiroCachePoints(payloadB)
	if _, ok := userTurnCachePoint(historyB, 0); !ok {
		t.Errorf("case B: expected first-user cachePoint on history[0]")
	}
	if _, ok := userTurnCachePoint(historyB, 2); !ok {
		t.Errorf("case B: expected second-to-last-user cachePoint on history[2]")
	}
}

// TestAddKiroCachePoints_SingleUserInHistory_NoDoubleMark guards against
// the case where the only user in history is also the "last user turn".
// We must not stamp the same userInputMessage twice; the function should
// fall through silently without re-touching history[firstUserIdx].
func TestAddKiroCachePoints_SingleUserInHistory_NoDoubleMark(t *testing.T) {
	// 4 history entries but only one user → totalTurns = 5 >= 4, yet the
	// second-pass loop must respect firstUserIdx and not regress to it.
	history := []any{
		userTurn("only user"),
		assistantTurn("a1"),
		assistantTurn("a2"),
		assistantTurn("a3"),
	}
	payload := newTestPayload(history, nil)
	addKiroCachePoints(payload)

	first, _ := history[0].(map[string]any)
	firstUIM, _ := first["userInputMessage"].(map[string]any)
	cp, _ := firstUIM["cachePoint"].(map[string]any)
	if cp == nil {
		t.Fatalf("expected first user cachePoint, got %#v", firstUIM)
	}
	// Ensure no assistant turn was accidentally given a cachePoint via
	// the user-only path (the helper userInputMessageOf rejects them).
	for i := 1; i < len(history); i++ {
		m, _ := history[i].(map[string]any)
		if uim, ok := m["userInputMessage"].(map[string]any); ok {
			if _, has := uim["cachePoint"]; has {
				t.Errorf("history[%d] unexpectedly carries cachePoint: %#v", i, uim)
			}
		}
	}
}

// TestBuildKiroPayload_IntegratesAddKiroCachePoints ensures the public
// entry point wires cachePoint injection on by default. This is an
// integration check that exercises the full flow (system merge, history
// build, tool emission) end to end.
func TestBuildKiroPayload_IntegratesAddKiroCachePoints(t *testing.T) {
	req := &AnthropicRequest{
		Model: "claude-sonnet-4.5",
		System: []map[string]any{
			{"type": "text", "text": "you are a coding assistant"},
		},
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"first question"`)},
			{Role: "assistant", Content: json.RawMessage(`"first answer"`)},
			{Role: "user", Content: json.RawMessage(`"second question"`)},
			{Role: "assistant", Content: json.RawMessage(`"second answer"`)},
			{Role: "user", Content: json.RawMessage(`"third question"`)},
		},
		Tools: []AnthropicTool{
			{Name: "Read", Description: "Read file", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	payload, err := BuildKiroPayload(req, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}

	// current message must always carry cachePoint
	uim := mustCurrentUserInputMessage(t, payload)
	if _, ok := uim["cachePoint"]; !ok {
		t.Errorf("currentMessage cachePoint missing: %#v", uim)
	}

	// tools tail must be a cachePoint marker
	ctx, _ := uim["userInputMessageContext"].(map[string]any)
	tools, _ := ctx["tools"].([]any)
	if len(tools) < 2 {
		t.Fatalf("expected tools to contain spec + cachePoint marker, got %#v", tools)
	}
	tail, _ := tools[len(tools)-1].(map[string]any)
	if _, ok := tail["cachePoint"]; !ok {
		t.Errorf("tools tail should be cachePoint marker, got %#v", tail)
	}

	// history must have at least one user turn with cachePoint
	cs, _ := payload["conversationState"].(map[string]any)
	history, _ := cs["history"].([]any)
	if len(history) == 0 {
		t.Fatalf("expected non-empty history with this multi-turn request")
	}
	if _, ok := userTurnCachePoint(history, 0); !ok {
		t.Errorf("expected history[0] (first user, system-prompt-merged) to carry cachePoint")
	}
}

// TestBuildKiroPayload_DisablePromptCaching_NoCachePoints verifies the
// opt-out switch: when DisablePromptCaching=true the payload must come
// out exactly as before (no cachePoint anywhere).
func TestBuildKiroPayload_DisablePromptCaching_NoCachePoints(t *testing.T) {
	req := &AnthropicRequest{
		Model: "claude-sonnet-4.5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"first"`)},
			{Role: "assistant", Content: json.RawMessage(`"hello"`)},
			{Role: "user", Content: json.RawMessage(`"second"`)},
		},
		Tools: []AnthropicTool{
			{Name: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	payload, err := BuildKiroPayload(req, BuildOptions{DisablePromptCaching: true})
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}

	uim := mustCurrentUserInputMessage(t, payload)
	if _, ok := uim["cachePoint"]; ok {
		t.Errorf("currentMessage should NOT carry cachePoint when DisablePromptCaching=true, got %#v", uim)
	}
	ctx, _ := uim["userInputMessageContext"].(map[string]any)
	tools, _ := ctx["tools"].([]any)
	for i, tool := range tools {
		m, _ := tool.(map[string]any)
		if _, ok := m["cachePoint"]; ok {
			t.Errorf("tools[%d] should NOT be a cachePoint marker when DisablePromptCaching=true, got %#v", i, m)
		}
	}
	cs, _ := payload["conversationState"].(map[string]any)
	if history, ok := cs["history"].([]any); ok {
		for i, entry := range history {
			m, _ := entry.(map[string]any)
			if uim, ok := m["userInputMessage"].(map[string]any); ok {
				if _, has := uim["cachePoint"]; has {
					t.Errorf("history[%d] user turn should NOT carry cachePoint when disabled, got %#v", i, uim)
				}
			}
		}
	}
}

// TestNewKiroCachePoint_FreshMapPerCall ensures successive calls do not
// alias the same backing map — otherwise mutating one breakpoint (e.g.
// stamping a TTL field in some future iteration) would silently mutate
// every other breakpoint in the payload.
func TestNewKiroCachePoint_FreshMapPerCall(t *testing.T) {
	a := newKiroCachePoint()
	b := newKiroCachePoint()
	a["mutated"] = true
	if _, ok := b["mutated"]; ok {
		t.Fatalf("newKiroCachePoint returned aliased maps; mutation leaked: %#v", b)
	}
}

// ---------- test helpers ----------

func newTestPayload(history []any, tools []any) map[string]any {
	uim := map[string]any{
		"content": "current question",
		"modelId": "claude-sonnet-4.5",
		"origin":  kiroRequestOrigin,
	}
	if len(tools) > 0 {
		uim["userInputMessageContext"] = map[string]any{"tools": tools}
	}
	state := map[string]any{
		"chatTriggerType": "MANUAL",
		"conversationId":  "test-conv",
		"currentMessage": map[string]any{
			"userInputMessage": uim,
		},
	}
	if len(history) > 0 {
		state["history"] = history
	}
	return map[string]any{"conversationState": state}
}

func userTurn(text string) any {
	return map[string]any{
		"userInputMessage": map[string]any{
			"content": text,
			"modelId": "claude-sonnet-4.5",
			"origin":  kiroRequestOrigin,
		},
	}
}

func assistantTurn(text string) any {
	return map[string]any{
		"assistantResponseMessage": map[string]any{
			"content": text,
		},
	}
}

func mustCurrentUserInputMessage(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	cs, _ := payload["conversationState"].(map[string]any)
	cm, _ := cs["currentMessage"].(map[string]any)
	uim, _ := cm["userInputMessage"].(map[string]any)
	if uim == nil {
		t.Fatalf("currentMessage.userInputMessage missing: %#v", payload)
	}
	return uim
}

// userTurnCachePoint returns the cachePoint map of history[idx] if it
// exists, plus whether the entry is a user turn at all.
func userTurnCachePoint(history []any, idx int) (map[string]any, bool) {
	if idx < 0 || idx >= len(history) {
		return nil, false
	}
	m, ok := history[idx].(map[string]any)
	if !ok {
		return nil, false
	}
	uim, ok := m["userInputMessage"].(map[string]any)
	if !ok {
		return nil, false
	}
	cp, ok := uim["cachePoint"].(map[string]any)
	return cp, ok
}
