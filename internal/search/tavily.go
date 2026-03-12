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
)

// TavilySearcher implements Searcher using the Tavily Search API.
type TavilySearcher struct {
	client *http.Client
	apiKey string
}

func NewTavilySearcher(apiKey string, client *http.Client) *TavilySearcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &TavilySearcher{
		client: client,
		apiKey: apiKey,
	}
}

type tavilyRequest struct {
	Query         string `json:"query"`
	SearchDepth   string `json:"search_depth"`
	MaxResults    int    `json:"max_results"`
	Topic         string `json:"topic"`
	IncludeAnswer bool   `json:"include_answer"`
}

type tavilyResponse struct {
	Results []tavilyResult `json:"results"`
}

type tavilyResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

func (t *TavilySearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 20 {
		maxResults = 20
	}

	reqBody := tavilyRequest{
		Query:         query,
		SearchDepth:   "basic",
		MaxResults:    maxResults,
		Topic:         "general",
		IncludeAnswer: false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.apiKey)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tavilyResp tavilyResponse
	if err := json.Unmarshal(body, &tavilyResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	results := make([]Result, 0, len(tavilyResp.Results))
	for _, r := range tavilyResp.Results {
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
		})
	}

	return results, nil
}
