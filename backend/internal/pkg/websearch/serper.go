package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	serperSearchEndpoint = "https://google.serper.dev/search"
	serperProviderName   = "serper"
	serperMaxResults     = 20
)

// SerperProvider implements web search via serper.dev — a Google Search
// reverse-proxy. Compared to Brave it has much better Chinese coverage
// (returns results from baidu/sogou/people.cn/cnblogs etc.) because the
// backend is Google itself.
type SerperProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewSerperProvider creates a serper.dev provider.
func NewSerperProvider(apiKey string, httpClient *http.Client) *SerperProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &SerperProvider{apiKey: apiKey, httpClient: httpClient}
}

func (s *SerperProvider) Name() string { return serperProviderName }

func (s *SerperProvider) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	n := req.MaxResults
	if n <= 0 {
		n = defaultMaxResults
	}
	if n > serperMaxResults {
		n = serperMaxResults
	}

	payload := serperRequest{
		Q:   req.Query,
		Num: n,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("serper: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, serperSearchEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("serper: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-KEY", s.apiKey)

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("serper: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("serper: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serper: status %d: %s", resp.StatusCode, truncateBody(raw))
	}

	var parsed serperResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("serper: decode response: %w", err)
	}

	results := make([]SearchResult, 0, len(parsed.Organic))
	for _, r := range parsed.Organic {
		results = append(results, SearchResult{
			URL:     r.Link,
			Title:   r.Title,
			Snippet: r.Snippet,
			PageAge: r.Date,
		})
	}
	return &SearchResponse{Results: results, Query: req.Query}, nil
}

// --- Serper wire types ---

type serperRequest struct {
	Q   string `json:"q"`
	Num int    `json:"num,omitempty"`
}

type serperResponse struct {
	Organic []serperOrganicResult `json:"organic"`
}

type serperOrganicResult struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	Snippet string `json:"snippet"`
	Date    string `json:"date,omitempty"`
}
