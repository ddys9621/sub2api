package kiro

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultMCPRegion is the fallback region used when the region cannot be
// derived from an account's profileArn. Kiro itself defaults to us-east-1
// in the IDE so we match that behaviour.
const DefaultMCPRegion = "us-east-1"

// mcpHostTemplate follows the Kiro IDE's own Q endpoint pattern used for
// MCP (Model Context Protocol) calls — same host/path format as the
// upstream kiro-gateway Python reference implementation.
const mcpHostTemplate = "https://q.%s.amazonaws.com/mcp"

// mcpCallTimeout bounds a single MCP call so a stuck upstream does not
// block the gateway thread forever.
const mcpCallTimeout = 30 * time.Second

// MCPWebSearchResult is a single hit in the MCP tools/call response.
// Field names mirror the JSON payload returned by Kiro's /mcp endpoint.
type MCPWebSearchResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet"`
	PublishedDate int64  `json:"publishedDate,omitempty"`
}

// MCPWebSearchResponse is the decoded payload of
// `result.content[0].text` AFTER the outer envelope is unwrapped.
// CRITICAL: Kiro wraps the actual results as a JSON-encoded string inside
// `content[0].text`, so the caller must double-decode.
type MCPWebSearchResponse struct {
	Query        string               `json:"query"`
	TotalResults int                  `json:"totalResults"`
	Results      []MCPWebSearchResult `json:"results"`
}

// mcpEnvelope is the outer JSON-RPC response shape from Kiro's /mcp API.
type mcpEnvelope struct {
	ID      string `json:"id"`
	JSONRPC string `json:"jsonrpc"`
	Result  *struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error json.RawMessage `json:"error,omitempty"`
}

// CallMCPWebSearch invokes Kiro's MCP `tools/call` endpoint with the
// web_search tool. It uses the caller-provided bearer token and region
// (derived from the account's profileArn), and returns the decoded result
// list ready to be formatted back into an SSE text chunk.
//
// The client argument is optional. When nil, a dedicated http.Client with a
// short timeout is constructed. Passing an existing client is recommended
// so that the account's configured proxy is honoured.
func CallMCPWebSearch(
	ctx context.Context,
	client *http.Client,
	region string,
	bearerToken string,
	query string,
) (*MCPWebSearchResponse, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("kiro mcp: empty query")
	}
	if strings.TrimSpace(bearerToken) == "" {
		return nil, fmt.Errorf("kiro mcp: missing bearer token")
	}
	if region = strings.TrimSpace(region); region == "" {
		region = DefaultMCPRegion
	}
	if client == nil {
		client = &http.Client{Timeout: mcpCallTimeout}
	}

	reqBody, err := buildMCPRequest(query)
	if err != nil {
		return nil, fmt.Errorf("kiro mcp: encode request: %w", err)
	}

	url := fmt.Sprintf(mcpHostTemplate, region)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("kiro mcp: build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+bearerToken)
	httpReq.Header.Set("x-amzn-codewhisperer-optout", "false")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kiro mcp: http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 8KB of error body for diagnostics.
		buf := make([]byte, 8192)
		n, _ := resp.Body.Read(buf)
		return nil, fmt.Errorf("kiro mcp: status %d: %s", resp.StatusCode, string(buf[:n]))
	}

	var env mcpEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("kiro mcp: decode envelope: %w", err)
	}
	// `error` may legitimately be JSON null, which comes through as a
	// 4-byte raw message ("null"). Treat only explicit, non-null error
	// payloads as failures.
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, fmt.Errorf("kiro mcp: jsonrpc error: %s", string(env.Error))
	}
	if env.Result == nil || len(env.Result.Content) == 0 {
		return nil, fmt.Errorf("kiro mcp: empty result")
	}

	// result.content[0].text is itself a JSON string — decode again.
	inner := env.Result.Content[0].Text
	if inner == "" {
		return &MCPWebSearchResponse{Query: query}, nil
	}
	var payload MCPWebSearchResponse
	if err := json.Unmarshal([]byte(inner), &payload); err != nil {
		return nil, fmt.Errorf("kiro mcp: decode inner payload: %w", err)
	}
	if payload.Query == "" {
		payload.Query = query
	}
	return &payload, nil
}

// buildMCPRequest constructs the JSON-RPC 2.0 envelope expected by Kiro's
// /mcp endpoint. The id format mirrors the Kiro IDE pattern so the request
// looks identical to what the desktop client emits.
func buildMCPRequest(query string) ([]byte, error) {
	id := generateMCPRequestID()
	req := map[string]any{
		"id":      id,
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "web_search",
			"arguments": map[string]any{"query": query},
		},
	}
	return json.Marshal(req)
}

// generateMCPRequestID produces an id resembling the native IDE's format:
// "web_search_tooluse_<22hex>_<unix_ms>_<8hex>".
func generateMCPRequestID() string {
	return fmt.Sprintf(
		"web_search_tooluse_%s_%d_%s",
		randomHex(11), // 22 hex chars = 11 random bytes
		time.Now().UnixMilli(),
		randomHex(4), // 8 hex chars = 4 random bytes
	)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read failure is vanishingly rare; fall back to time-based noise.
		for i := range buf {
			buf[i] = byte(time.Now().UnixNano() >> uint(i*8))
		}
	}
	return hex.EncodeToString(buf)
}

// RegionFromProfileArn extracts the AWS region from an AWS ARN of the form
// `arn:aws:codewhisperer:<region>:<account>:profile/...`. Returns an empty
// string if the ARN is malformed; callers should fall back to DefaultMCPRegion.
func RegionFromProfileArn(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 4 {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

// FormatSearchSummary renders an MCP web_search response into a
// human-readable text block that can be streamed as plain assistant text.
// The format is stable and wrapped in <web_search>...</web_search> tags so
// downstream log parsers can identify tool-sourced content if they need to.
func FormatSearchSummary(resp *MCPWebSearchResponse) string {
	if resp == nil || len(resp.Results) == 0 {
		if resp != nil && resp.Query != "" {
			return fmt.Sprintf("\n<web_search>\nNo results found for %q.\n</web_search>\n", resp.Query)
		}
		return "\n<web_search>\nNo search results.\n</web_search>\n"
	}
	var sb strings.Builder
	sb.WriteString("\n<web_search>\n")
	fmt.Fprintf(&sb, "Search results for %q:\n\n", resp.Query)
	for i, r := range resp.Results {
		fmt.Fprintf(&sb, "%d. Title: **%s**\n", i+1, r.Title)
		if r.PublishedDate > 0 {
			// Kiro emits publishedDate as milliseconds-since-epoch.
			t := time.UnixMilli(r.PublishedDate).UTC()
			fmt.Fprintf(&sb, "   Published: %s\n", t.Format("02 Jan 2006 15:04 UTC"))
		}
		if r.URL != "" {
			fmt.Fprintf(&sb, "   URL: %s\n", r.URL)
		}
		if r.Snippet != "" {
			fmt.Fprintf(&sb, "   %s\n", strings.ReplaceAll(strings.TrimSpace(r.Snippet), "\n", " "))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("</web_search>\n")
	return sb.String()
}
