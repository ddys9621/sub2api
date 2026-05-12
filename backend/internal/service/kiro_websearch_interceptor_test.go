package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func newTestInterceptor(arn string) *kiroWebSearchInterceptor {
	return newKiroWebSearchInterceptor(
		context.Background(),
		nil,
		"token",
		&kiro.AnthropicRequest{},
		kiro.BuildOptions{ProfileArn: arn},
		"https://example.invalid",
		http.Header{},
		nil,
		1,
		"",
	)
}

func TestKiroWebSearchInterceptor_OnToolStart_MatchesWebSearch(t *testing.T) {
	interceptor := newTestInterceptor("arn:aws:codewhisperer:us-east-1:123:profile/default")

	require.True(t, interceptor.OnToolStart(context.Background(), &kiro.StreamEvent{
		Kind:     "tool_use_start",
		ToolName: "web_search",
	}))
	require.True(t, interceptor.OnToolStart(context.Background(), &kiro.StreamEvent{
		Kind:     "tool_use_start",
		ToolName: "WebSearch",
	}))
}

func TestKiroWebSearchInterceptor_OnToolStart_IgnoresOtherTools(t *testing.T) {
	interceptor := newTestInterceptor("")

	for _, name := range []string{"Read", "Edit", "Bash", "Grep", "WebFetch"} {
		require.Falsef(t, interceptor.OnToolStart(context.Background(), &kiro.StreamEvent{
			Kind:     "tool_use_start",
			ToolName: name,
		}), "interceptor should not match %q", name)
	}
}

func TestKiroWebSearchInterceptor_OnToolStart_NilEventIgnored(t *testing.T) {
	interceptor := newTestInterceptor("")
	require.False(t, interceptor.OnToolStart(context.Background(), nil))
}

func TestExtractQueryFromToolInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", `{"query":"golang mcp"}`, "golang mcp"},
		{"trailing whitespace", `{"query":"  hello world  "}`, "hello world"},
		{"extra fields", `{"query":"news","max_results":5}`, "news"},
		{"missing field", `{"foo":"bar"}`, ""},
		{"empty", "", ""},
		{"malformed json", `{"query":`, ""},
		{"non-string query", `{"query":42}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractQueryFromToolInput(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestKiroWebSearchInterceptor_DefaultsRegionWhenArnMissing(t *testing.T) {
	interceptor := newTestInterceptor("")
	require.Equal(t, kiro.DefaultMCPRegion, interceptor.region)
}

func TestKiroWebSearchInterceptor_PicksRegionFromArn(t *testing.T) {
	interceptor := newTestInterceptor(
		"arn:aws:codewhisperer:eu-west-1:123456789012:profile/dev")
	require.Equal(t, "eu-west-1", interceptor.region)
}
