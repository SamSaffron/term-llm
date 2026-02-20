package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GoogleSearcher implements Searcher using Google Custom Search API.
type GoogleSearcher struct {
	client *http.Client
	apiKey string
	cx     string // Custom Search Engine ID
}

func NewGoogleSearcher(apiKey, cx string, client *http.Client) *GoogleSearcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GoogleSearcher{
		client: client,
		apiKey: apiKey,
		cx:     cx,
	}
}

type googleResponse struct {
	Items []googleItem `json:"items"`
	Error *googleError `json:"error,omitempty"`
}

type googleItem struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	Snippet string `json:"snippet"`
}

type googleError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (g *GoogleSearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 10 {
		maxResults = 10 // Google Custom Search API max is 10 per request
	}

	params := url.Values{}
	params.Set("key", g.apiKey)
	params.Set("cx", g.cx)
	params.Set("q", query)
	params.Set("num", strconv.Itoa(maxResults))

	reqURL := "https://www.googleapis.com/customsearch/v1?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var googleResp googleResponse
	if err := json.Unmarshal(body, &googleResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if googleResp.Error != nil {
		if googleResp.Error.Code == 429 {
			return nil, fmt.Errorf("google search rate limited")
		}
		return nil, fmt.Errorf("google search error %d: %s", googleResp.Error.Code, googleResp.Error.Message)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	results := make([]Result, 0, len(googleResp.Items))
	for _, item := range googleResp.Items {
		results = append(results, Result{
			Title:   item.Title,
			URL:     item.Link,
			Snippet: item.Snippet,
		})
	}

	return results, nil
}
