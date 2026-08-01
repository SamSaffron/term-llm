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

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const parallelSearchURL = "https://api.parallel.ai/v1/search"

// ParallelSearcher implements Searcher using the Parallel Search API.
type ParallelSearcher struct {
	client *http.Client
	apiKey string
}

func NewParallelSearcher(apiKey string, client *http.Client) *ParallelSearcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &ParallelSearcher{
		client: client,
		apiKey: apiKey,
	}
}

type parallelRequest struct {
	SearchQueries    []string                 `json:"search_queries"`
	Mode             string                   `json:"mode"`
	AdvancedSettings parallelAdvancedSettings `json:"advanced_settings"`
}

type parallelAdvancedSettings struct {
	MaxResults int `json:"max_results"`
}

type parallelResponse struct {
	Results []parallelResult `json:"results"`
}

type parallelResult struct {
	Title    *string  `json:"title"`
	URL      string   `json:"url"`
	Excerpts []string `json:"excerpts"`
}

type parallelErrorResponse struct {
	Error struct {
		RefID   string `json:"ref_id"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *ParallelSearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty query")
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 20 {
		maxResults = 20
	}

	reqBody := parallelRequest{
		SearchQueries: []string{query},
		Mode:          "basic",
		AdvancedSettings: parallelAdvancedSettings{
			MaxResults: maxResults,
		},
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parallelSearchURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parallelStatusError(resp, body)
	}

	var parallelResp parallelResponse
	if err := json.Unmarshal(body, &parallelResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	results := make([]Result, 0, len(parallelResp.Results))
	for _, r := range parallelResp.Results {
		title := ""
		if r.Title != nil {
			title = *r.Title
		}
		results = append(results, Result{
			Title:   title,
			URL:     r.URL,
			Snippet: strings.Join(r.Excerpts, "\n\n"),
		})
	}
	return results, nil
}

func parallelStatusError(resp *http.Response, body []byte) error {
	message := strings.TrimSpace(string(body))
	var apiErr parallelErrorResponse
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
		message = apiErr.Error.Message
		if apiErr.Error.RefID != "" {
			message += " (ref_id: " + apiErr.Error.RefID + ")"
		}
	}
	return providerhttp.NewStatusErrorMessagef(resp, body, "parallel http %d: %s", resp.StatusCode, message)
}
