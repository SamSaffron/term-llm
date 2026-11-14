package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/term-llm/term-llm/internal/providerhttp"
)

// PerplexitySearcher implements Searcher using the Perplexity Search API.
type PerplexitySearcher struct {
	client *http.Client
	apiKey string
}

func NewPerplexitySearcher(apiKey string, client *http.Client) *PerplexitySearcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &PerplexitySearcher{
		client: client,
		apiKey: apiKey,
	}
}

type perplexityRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type perplexityResponse struct {
	Results []perplexityResult `json:"results"`
}

type perplexityResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func (p *PerplexitySearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 20 {
		maxResults = 20
	}

	reqBody := perplexityRequest{
		Query:      query,
		MaxResults: maxResults,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.perplexity.ai/search", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, providerhttp.NewStatusErrorMessagef(resp, body, "perplexity http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var pplxResp perplexityResponse
	if err := json.Unmarshal(body, &pplxResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	results := make([]Result, 0, len(pplxResp.Results))
	for _, r := range pplxResp.Results {
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Snippet,
		})
	}

	return results, nil
}
