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

// ExaSearcher implements Searcher using the Exa API.
type ExaSearcher struct {
	client *http.Client
	apiKey string
}

func NewExaSearcher(apiKey string, client *http.Client) *ExaSearcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &ExaSearcher{
		client: client,
		apiKey: apiKey,
	}
}

type exaRequest struct {
	Query      string `json:"query"`
	NumResults int    `json:"numResults"`
	Type       string `json:"type"`
}

type exaResponse struct {
	Results []exaResult `json:"results"`
}

type exaResult struct {
	Title      string   `json:"title"`
	URL        string   `json:"url"`
	Text       string   `json:"text"`
	Highlights []string `json:"highlights"`
}

func (e *ExaSearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 100 {
		maxResults = 100
	}

	reqBody := exaRequest{
		Query:      query,
		NumResults: maxResults,
		Type:       "auto",
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai/search", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exa http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var exaResp exaResponse
	if err := json.Unmarshal(body, &exaResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	results := make([]Result, 0, len(exaResp.Results))
	for _, r := range exaResp.Results {
		snippet := ""
		if len(r.Highlights) > 0 {
			snippet = strings.Join(r.Highlights, " ")
		} else if r.Text != "" {
			// Truncate text to ~200 chars for snippet
			snippet = r.Text
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
		}
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: snippet,
		})
	}

	return results, nil
}
