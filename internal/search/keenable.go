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

const (
	// keenablePublicURL is Keenable's keyless endpoint (1,000 requests per hour
	// per IP). It needs an X-Keenable-Title header naming the app.
	keenablePublicURL = "https://api.keenable.ai/v1/search/public"
	// keenableKeyedURL is used when KEENABLE_API_KEY is set; no hourly cap.
	keenableKeyedURL = "https://api.keenable.ai/v1/search"
	keenableAppTitle = "term-llm"
)

// KeenableSearcher implements Searcher using the Keenable Search API.
// The API key is optional.
type KeenableSearcher struct {
	client *http.Client
	apiKey string
}

func NewKeenableSearcher(apiKey string, client *http.Client) *KeenableSearcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &KeenableSearcher{
		client: client,
		apiKey: strings.TrimSpace(apiKey),
	}
}

type keenableRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type keenableResponse struct {
	Results []keenableResult `json:"results"`
}

type keenableResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func (k *KeenableSearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 20 {
		maxResults = 20
	}

	jsonBody, err := json.Marshal(keenableRequest{Query: query, MaxResults: maxResults})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := keenablePublicURL
	if k.apiKey != "" {
		endpoint = keenableKeyedURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k.apiKey != "" {
		req.Header.Set("X-API-Key", k.apiKey)
	} else {
		req.Header.Set("X-Keenable-Title", keenableAppTitle)
	}

	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusTooManyRequests && k.apiKey == "" {
			msg += " (set KEENABLE_API_KEY to remove the keyless hourly limit)"
		}
		return nil, providerhttp.NewStatusErrorMessagef(resp, body, "keenable http %d: %s", resp.StatusCode, msg)
	}

	var keenableResp keenableResponse
	if err := json.Unmarshal(body, &keenableResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	results := make([]Result, 0, len(keenableResp.Results))
	for _, r := range keenableResp.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Snippet,
		})
	}

	return results, nil
}
