package kiro

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegionFromProfileArn(t *testing.T) {
	cases := map[string]string{
		"":  "",
		"arn:aws:codewhisperer:us-east-1:123456789012:profile/default":  "us-east-1",
		"arn:aws:codewhisperer:eu-west-1:000000000000:profile/anything": "eu-west-1",
		"arn:aws:codewhisperer":                                         "", // malformed: not enough parts
		"not-an-arn":                                                    "",
	}
	for in, want := range cases {
		require.Equalf(t, want, RegionFromProfileArn(in), "for arn %q", in)
	}
}

func TestBuildMCPRequest_HasExpectedShape(t *testing.T) {
	body, err := buildMCPRequest("latest news in AI")
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.Equal(t, "2.0", decoded["jsonrpc"])
	require.Equal(t, "tools/call", decoded["method"])

	params := decoded["params"].(map[string]any)
	require.Equal(t, "web_search", params["name"])
	args := params["arguments"].(map[string]any)
	require.Equal(t, "latest news in AI", args["query"])

	id, _ := decoded["id"].(string)
	require.True(t, strings.HasPrefix(id, "web_search_tooluse_"),
		"id should match Kiro IDE pattern, got %q", id)
}

func TestFormatSearchSummary_Empty(t *testing.T) {
	got := FormatSearchSummary(&MCPWebSearchResponse{Query: "cats"})
	require.Contains(t, got, "No results")
	require.Contains(t, got, `"cats"`)
}

func TestFormatSearchSummary_RendersAllFields(t *testing.T) {
	resp := &MCPWebSearchResponse{
		Query: "kiro release",
		Results: []MCPWebSearchResult{
			{
				Title:         "Kiro 0.8 Released",
				URL:           "https://example.com/kiro",
				Snippet:       "New features are rolling out.",
				PublishedDate: 1710000000000,
			},
			{
				Title:   "Follow-up blog",
				URL:     "https://example.com/blog",
				Snippet: "Detailed notes",
			},
		},
	}
	got := FormatSearchSummary(resp)
	require.Contains(t, got, "Kiro 0.8 Released")
	require.Contains(t, got, "https://example.com/kiro")
	require.Contains(t, got, "New features are rolling out.")
	require.Contains(t, got, "Follow-up blog")
	require.Contains(t, got, "<web_search>")
	require.Contains(t, got, "</web_search>")
}

// TestCallMCPWebSearch_DecodesJSONRPCEnvelope exercises the full decode path
// with a fake upstream that returns a canonical JSON-RPC response.
func TestCallMCPWebSearch_DecodesJSONRPCEnvelope(t *testing.T) {
	// Kiro's /mcp endpoint responds with an envelope whose result.content[0].text
	// is itself a JSON-encoded string — we need to double-decode.
	innerPayload := `{"query":"ai news","totalResults":1,"results":[{"title":"Headline","url":"https://x","snippet":"Body"}]}`
	outer := map[string]any{
		"id":      "stub",
		"jsonrpc": "2.0",
		"result": map[string]any{
			"isError": false,
			"content": []map[string]string{{"type": "text", "text": innerPayload}},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		require.NoError(t, json.Unmarshal(body, &req))
		require.Equal(t, "tools/call", req["method"])

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(outer)
	}))
	defer server.Close()

	// Swap the upstream URL for the test server by calling via a custom
	// roundtripper on the provided client.
	client := &http.Client{Transport: rewriteHostTransport{to: server.URL}}

	resp, err := CallMCPWebSearch(context.Background(), client, "us-east-1", "test-token", "ai news")
	require.NoError(t, err)
	require.Equal(t, "ai news", resp.Query)
	require.Equal(t, 1, resp.TotalResults)
	require.Len(t, resp.Results, 1)
	require.Equal(t, "Headline", resp.Results[0].Title)
}

func TestCallMCPWebSearch_RejectsEmptyInputs(t *testing.T) {
	_, err := CallMCPWebSearch(context.Background(), nil, "", "", "")
	require.Error(t, err)

	_, err = CallMCPWebSearch(context.Background(), nil, "us-east-1", "", "query")
	require.Error(t, err)

	_, err = CallMCPWebSearch(context.Background(), nil, "us-east-1", "tok", "")
	require.Error(t, err)
}

// TestCallMCPWebSearch_NullErrorIsNotAnError verifies that a JSON-RPC
// response with explicit `"error": null` is treated as success. Kiro's
// real /mcp endpoint uses this shape on every successful call.
func TestCallMCPWebSearch_NullErrorIsNotAnError(t *testing.T) {
	innerPayload := `{"query":"q","totalResults":0,"results":[]}`
	outer := map[string]any{
		"id":      "stub",
		"jsonrpc": "2.0",
		"error":   nil, // marshalled as JSON null
		"result": map[string]any{
			"isError": false,
			"content": []map[string]string{{"type": "text", "text": innerPayload}},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(outer)
	}))
	defer server.Close()

	client := &http.Client{Transport: rewriteHostTransport{to: server.URL}}
	resp, err := CallMCPWebSearch(context.Background(), client, "us-east-1", "t", "q")
	require.NoError(t, err)
	require.Equal(t, "q", resp.Query)
}

// rewriteHostTransport replaces the outgoing request's scheme+host with `to`
// so tests can point CallMCPWebSearch at an httptest server without mocking
// the URL template itself.
type rewriteHostTransport struct {
	to string
}

func (rw rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := parseURL(rw.to)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}

// parseURL is a tiny wrapper so the test file doesn't need to import net/url
// everywhere (import locality for clarity).
func parseURL(s string) (*urlShape, error) {
	// Extremely small parse: scheme://host
	parts := strings.SplitN(s, "://", 2)
	if len(parts) != 2 {
		return nil, errSimple("bad url: " + s)
	}
	return &urlShape{Scheme: parts[0], Host: parts[1]}, nil
}

type urlShape struct {
	Scheme string
	Host   string
}

type errSimple string

func (e errSimple) Error() string { return string(e) }
