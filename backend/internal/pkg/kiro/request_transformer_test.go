package kiro

import (
	"encoding/json"
	"testing"
)

// TestBuildKiroPayload_RewritesWebSearchAndDropsOtherServerSideTools
// verifies that:
//   - Anthropic server-side web_search_* tools are rewritten into a plain
//     function tool named "web_search" so Kiro's model can invoke it
//     (the response transformer then fulfils the call via Kiro /mcp).
//   - Other server-side tools (computer_*, text_editor_*, bash_*, ...)
//     remain dropped because Kiro CodeWhisperer has no equivalent.
//   - User-defined tools pass through unchanged.
func TestBuildKiroPayload_RewritesWebSearchAndDropsOtherServerSideTools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	req := &AnthropicRequest{
		Model: "claude-sonnet-4.5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
		Tools: []AnthropicTool{
			// User-defined tool: should survive untouched.
			{Name: "Read", Description: "Read a file", InputSchema: schema},
			// Server-side web_search: should be rewritten (kept, as function tool).
			{Type: "web_search_20250305", Name: "web_search"},
			// Other server-side tools: should be dropped.
			{Type: "computer_20250124", Name: "computer"},
			{Type: "text_editor_20250124", Name: "str_replace_editor"},
			// function/custom: treated as user-defined.
			{Type: "function", Name: "Grep", InputSchema: schema},
			{Type: "custom", Name: "Bash", InputSchema: schema},
		},
	}

	payload, err := BuildKiroPayload(req, BuildOptions{ProfileArn: "arn:test"})
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}

	cs, _ := payload["conversationState"].(map[string]any)
	current, _ := cs["currentMessage"].(map[string]any)
	uim, _ := current["userInputMessage"].(map[string]any)
	ctx, _ := uim["userInputMessageContext"].(map[string]any)
	tools, _ := ctx["tools"].([]any)

	// Expect: Read, web_search (rewritten), Grep, Bash => 4 toolSpecification
	// entries, plus one trailing {cachePoint:{type:"default"}} entry injected
	// by addKiroCachePoints => 5 total tools entries.
	specs := filterToolSpecs(tools)
	if got, want := len(specs), 4; got != want {
		t.Fatalf("expected %d tool specs, got %d: %#v", want, got, tools)
	}
	assertToolsTailIsCachePoint(t, tools)

	gotNames := map[string]map[string]any{}
	for _, spec := range specs {
		name, _ := spec["name"].(string)
		gotNames[name] = spec
	}
	for _, want := range []string{"Read", "Grep", "Bash", "web_search"} {
		if _, ok := gotNames[want]; !ok {
			t.Errorf("expected tool %q to be present, got names: %v", want, keys(gotNames))
		}
	}
	for _, forbidden := range []string{"computer", "str_replace_editor"} {
		if _, ok := gotNames[forbidden]; ok {
			t.Errorf("unsupported server-side tool %q should have been dropped", forbidden)
		}
	}

	// The rewritten web_search tool must carry a concrete inputSchema that
	// Kiro will accept; otherwise upstream rejects the payload.
	ws := gotNames["web_search"]
	if ws == nil {
		t.Fatalf("web_search tool missing")
	}
	is, _ := ws["inputSchema"].(map[string]any)
	inner, _ := is["json"].(map[string]any)
	if inner == nil {
		t.Fatalf("web_search tool missing inputSchema.json: %#v", ws)
	}
	props, _ := inner["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Errorf("rewritten web_search tool should expose a 'query' property, got: %#v", props)
	}
	if desc, _ := ws["description"].(string); desc == "" {
		t.Errorf("rewritten web_search tool should have a non-empty description")
	}
}

func TestBuildKiroPayload_UsesKiroCLIOrigin(t *testing.T) {
	req := &AnthropicRequest{
		Model: "claude-sonnet-4-6",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
	}

	payload, err := BuildKiroPayload(req, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}

	uim := currentUserInputMessage(t, payload)
	if got, want := uim["origin"], "KIRO_CLI"; got != want {
		t.Fatalf("origin = %q, want %q", got, want)
	}
	if got, want := uim["modelId"], "claude-sonnet-4.6"; got != want {
		t.Fatalf("modelId = %q, want %q", got, want)
	}
}

func TestResolveModelUsesActualKiroIDs(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-7":            "claude-opus-4.7",
		"claude-opus-4-6":            "claude-opus-4.6",
		"claude-sonnet-4-6":          "claude-sonnet-4.6",
		"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
		"claude-3-7-sonnet-20250219": "claude-sonnet-4",
		"deepseek-3.2":               "deepseek-3.2",
		"qwen3-coder-next":           "qwen3-coder-next",
	}
	for input, want := range cases {
		if got := ResolveModel(input, nil); got != want {
			t.Errorf("ResolveModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveModelUsesWildcardAccountMapping(t *testing.T) {
	mapping := map[string]string{
		"claude-*":       "deepseek-3.2",
		"claude-opus-*":  "qwen3-coder-next",
		"claude-opus-4":  "glm-5",
		"claude-haiku-*": "minimax-m2.5",
	}

	if got, want := ResolveModel("claude-opus-4-7", mapping), "qwen3-coder-next"; got != want {
		t.Fatalf("ResolveModel wildcard = %q, want %q", got, want)
	}
	if got, want := ResolveModel("claude-opus-4", mapping), "glm-5"; got != want {
		t.Fatalf("ResolveModel exact = %q, want %q", got, want)
	}
	if got, want := ResolveModel("claude-haiku-4.5", mapping), "minimax-m2.5"; got != want {
		t.Fatalf("ResolveModel longer wildcard = %q, want %q", got, want)
	}
}

// TestBuildKiroPayload_LoneWebSearchToolIsRewrittenNotDropped confirms that
// when the only submitted tool is a server-side web_search_*, the resulting
// payload still ships a tools array with exactly one function tool. This
// is the common Claude Code "pure web search" scenario.
func TestBuildKiroPayload_LoneWebSearchToolIsRewrittenNotDropped(t *testing.T) {
	req := &AnthropicRequest{
		Model: "claude-sonnet-4.5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
		},
		Tools: []AnthropicTool{
			{Type: "web_search_20250305", Name: "web_search"},
		},
	}

	payload, err := BuildKiroPayload(req, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}
	cs, _ := payload["conversationState"].(map[string]any)
	current, _ := cs["currentMessage"].(map[string]any)
	uim, _ := current["userInputMessage"].(map[string]any)
	ctx, _ := uim["userInputMessageContext"].(map[string]any)
	if ctx == nil {
		t.Fatalf("expected userInputMessageContext to be present with rewritten web_search tool")
	}
	tools, _ := ctx["tools"].([]any)
	specs := filterToolSpecs(tools)
	if len(specs) != 1 {
		t.Fatalf("expected exactly 1 tool spec after rewrite, got %d: %#v", len(specs), tools)
	}
	assertToolsTailIsCachePoint(t, tools)
	if name, _ := specs[0]["name"].(string); name != "web_search" {
		t.Errorf("expected tool name 'web_search', got %q", name)
	}
}

// TestBuildKiroPayload_OmitsToolsWhenAllDropped ensures that if every
// submitted tool is an unsupported server-side tool (not web_search), the
// resulting payload omits the tools array entirely so Kiro doesn't choke
// on an empty one.
func TestBuildKiroPayload_OmitsToolsWhenAllDropped(t *testing.T) {
	req := &AnthropicRequest{
		Model: "claude-sonnet-4.5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
		},
		Tools: []AnthropicTool{
			{Type: "computer_20250124", Name: "computer"},
			{Type: "text_editor_20250124", Name: "editor"},
		},
	}

	payload, err := BuildKiroPayload(req, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}
	cs, _ := payload["conversationState"].(map[string]any)
	current, _ := cs["currentMessage"].(map[string]any)
	uim, _ := current["userInputMessage"].(map[string]any)
	ctx, ok := uim["userInputMessageContext"].(map[string]any)
	if !ok {
		return // empty context is fine too
	}
	if _, present := ctx["tools"]; present {
		t.Errorf("expected tools to be omitted when all entries are dropped, got context: %#v", ctx)
	}
}

func TestIsAnthropicServerSideWebSearch(t *testing.T) {
	cases := []struct {
		name string
		tool AnthropicTool
		want bool
	}{
		{"web_search_20250305", AnthropicTool{Type: "web_search_20250305", Name: "web_search"}, true},
		{"web_search plain", AnthropicTool{Type: "web_search", Name: "web_search"}, true},
		{"legacy shape by name", AnthropicTool{Name: "web_search"}, true},
		{"function type rejected", AnthropicTool{Type: "function", Name: "web_search"}, false},
		{"computer", AnthropicTool{Type: "computer_20250124", Name: "computer"}, false},
		{"empty tool rejected", AnthropicTool{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAnthropicServerSideWebSearch(tc.tool); got != tc.want {
				t.Errorf("isAnthropicServerSideWebSearch(%+v) = %v, want %v", tc.tool, got, tc.want)
			}
		})
	}
}

func currentUserInputMessage(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	cs, _ := payload["conversationState"].(map[string]any)
	current, _ := cs["currentMessage"].(map[string]any)
	uim, _ := current["userInputMessage"].(map[string]any)
	if uim == nil {
		t.Fatalf("current userInputMessage missing: %#v", payload)
	}
	return uim
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// filterToolSpecs returns just the {toolSpecification:{...}} entries from a
// Kiro tools array, ignoring trailing {cachePoint:{...}} markers that
// BuildKiroPayload appends for prefix caching.
func filterToolSpecs(tools []any) []map[string]any {
	specs := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		m, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		spec, ok := m["toolSpecification"].(map[string]any)
		if !ok {
			continue
		}
		specs = append(specs, spec)
	}
	return specs
}

// assertToolsTailIsCachePoint verifies that BuildKiroPayload appended a
// {cachePoint:{type:"default"}} marker as the final element of tools[]
// whenever the tools array is non-empty.
func assertToolsTailIsCachePoint(t *testing.T, tools []any) {
	t.Helper()
	if len(tools) == 0 {
		return
	}
	tail, _ := tools[len(tools)-1].(map[string]any)
	if tail == nil {
		t.Fatalf("expected last tools entry to be a map, got %#v", tools[len(tools)-1])
	}
	cp, ok := tail["cachePoint"].(map[string]any)
	if !ok {
		t.Fatalf("expected last tools entry to carry cachePoint, got %#v", tail)
	}
	if got, want := cp["type"], "default"; got != want {
		t.Errorf("tools tail cachePoint type = %v, want %q", got, want)
	}
}
