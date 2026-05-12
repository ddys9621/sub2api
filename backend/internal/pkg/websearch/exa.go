package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	exaSearchEndpoint = "https://api.exa.ai/search"
	exaProviderName   = "exa"
	exaMaxResults     = 25
)

// ExaProvider implements web search via the Exa.ai neural/keyword search API.
//
// Exa covers both Latin-script sources (CNN, Reuters, AP…) and keeps fresh
// indexing for news, which makes it a strong complement to Brave for
// English-heavy queries. We default to the "auto" search type so Exa can
// pick between neural and keyword search per query.
type ExaProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewExaProvider creates an Exa search provider. httpClient may be nil; the
// caller is expected to configure proxy/timeouts where required.
func NewExaProvider(apiKey string, httpClient *http.Client) *ExaProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &ExaProvider{apiKey: apiKey, httpClient: httpClient}
}

func (e *ExaProvider) Name() string { return exaProviderName }

func (e *ExaProvider) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	n := req.MaxResults
	if n <= 0 {
		n = defaultMaxResults
	}
	if n > exaMaxResults {
		n = exaMaxResults
	}

	body := exaRequest{
		Query:      req.Query,
		NumResults: n,
		Type:       "auto",
		Contents: exaContents{
			Text: exaContentsText{
				MaxCharacters: 600,
			},
			Highlights: &exaContentsHighlights{
				NumSentences:     3,
				HighlightsPerURL: 1,
			},
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("exa: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, exaSearchEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("exa: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", e.apiKey)

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("exa: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("exa: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exa: status %d: %s", resp.StatusCode, truncateBody(raw))
	}

	var parsed exaResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("exa: decode response: %w", err)
	}

	results := make([]SearchResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		snippet := buildExaSnippet(r)
		results = append(results, SearchResult{
			URL:     r.URL,
			Title:   r.Title,
			Snippet: snippet,
			PageAge: formatExaPageAge(r.PublishedDate),
		})
	}
	return &SearchResponse{Results: results, Query: req.Query}, nil
}

// buildExaSnippet picks the most informative text: prefer highlights (the
// query-relevant sentences) and fall back to the page's summary/text snippet.
func buildExaSnippet(r exaResult) string {
	if len(r.Highlights) > 0 {
		var b strings.Builder
		for i, h := range r.Highlights {
			if i > 0 {
				b.WriteString(" … ")
			}
			b.WriteString(strings.TrimSpace(h))
		}
		out := b.String()
		if out != "" {
			return out
		}
	}
	if r.Summary != "" {
		return strings.TrimSpace(r.Summary)
	}
	text := strings.TrimSpace(r.Text)
	if len(text) > 600 {
		text = text[:600] + "…"
	}
	return text
}

// formatExaPageAge converts an ISO-8601 timestamp into a short absolute
// date string (YYYY-MM-DD) for display; empty input yields empty output.
func formatExaPageAge(iso string) string {
	if iso == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	// Fall back to the raw date portion if the timestamp is malformed.
	if idx := strings.Index(iso, "T"); idx > 0 {
		return iso[:idx]
	}
	return iso
}

// --- Exa wire types ---

type exaRequest struct {
	Query      string      `json:"query"`
	NumResults int         `json:"numResults"`
	Type       string      `json:"type,omitempty"` // "neural" | "keyword" | "auto"
	Contents   exaContents `json:"contents"`
}

type exaContents struct {
	Text       exaContentsText        `json:"text"`
	Highlights *exaContentsHighlights `json:"highlights,omitempty"`
}

type exaContentsText struct {
	MaxCharacters int `json:"maxCharacters,omitempty"`
}

type exaContentsHighlights struct {
	NumSentences     int `json:"numSentences,omitempty"`
	HighlightsPerURL int `json:"highlightsPerUrl,omitempty"`
}

type exaResponse struct {
	Results []exaResult `json:"results"`
}

type exaResult struct {
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	PublishedDate string   `json:"publishedDate"`
	Text          string   `json:"text"`
	Summary       string   `json:"summary"`
	Highlights    []string `json:"highlights"`
}
