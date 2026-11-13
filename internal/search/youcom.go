package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	// youcomKeylessURL is You.com's free keyless search tier (no account, about
	// 100 searches a day). count is capped at 50.
	youcomKeylessURL = "https://api.you.com/v1/agents/search"
	// youcomKeyedURL is the keyed Web Search API, used when YDC_API_KEY is set.
	youcomKeyedURL = "https://ydc-index.io/v1/search"
	// youcomUserAgent is sent only on requests to You.com hosts.
	youcomUserAgent    = "term-llm youdotcom-integration/samsaffron-term-llm"
	youcomKeylessLimit = 50
)

// YouComSearcher implements Searcher using You.com search. It works without an
// API key on the free keyless tier; with a key it calls the Web Search API.
type YouComSearcher struct {
	client *http.Client
	apiKey string
}

func NewYouComSearcher(apiKey string, client *http.Client) *YouComSearcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &YouComSearcher{
		client: client,
		apiKey: strings.TrimSpace(apiKey),
	}
}

type youcomKeyedRequest struct {
	Query string `json:"query"`
	Count int    `json:"count"`
}

type youcomResponse struct {
	Results struct {
		Web  []youcomResult `json:"web"`
		News []youcomResult `json:"news"`
	} `json:"results"`
}

type youcomResult struct {
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Description string   `json:"description"`
	Snippets    []string `json:"snippets"`
}

func (y *YouComSearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
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

	req, err := y.newRequest(ctx, query, maxResults)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := y.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusTooManyRequests && y.apiKey == "" {
			message += " (keyless daily limit reached; set YDC_API_KEY for higher limits)"
		}
		return nil, providerhttp.NewStatusErrorMessagef(resp, body, "you.com http %d: %s", resp.StatusCode, message)
	}

	var parsed youcomResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	items := append(parsed.Results.Web, parsed.Results.News...)
	results := make([]Result, 0, len(items))
	for _, r := range items {
		if r.URL == "" {
			continue
		}
		snippet := r.Description
		if snippet == "" {
			snippet = strings.Join(r.Snippets, "\n\n")
		}
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: snippet,
		})
		if len(results) == maxResults {
			break
		}
	}
	return results, nil
}

func (y *YouComSearcher) newRequest(ctx context.Context, query string, maxResults int) (*http.Request, error) {
	var req *http.Request
	var err error
	if y.apiKey != "" {
		jsonBody, merr := json.Marshal(youcomKeyedRequest{Query: query, Count: maxResults})
		if merr != nil {
			return nil, merr
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, youcomKeyedURL, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", y.apiKey)
	} else {
		count := min(maxResults, youcomKeylessLimit)
		params := url.Values{}
		params.Set("query", query)
		params.Set("count", strconv.Itoa(count))
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, youcomKeylessURL+"?"+params.Encode(), nil)
		if err != nil {
			return nil, err
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", youcomUserAgent)
	return req, nil
}
