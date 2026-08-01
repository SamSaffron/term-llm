package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

func TestParallelSearcherSearch(t *testing.T) {
	type recordedRequest struct {
		method      string
		apiKey      string
		contentType string
		accept      string
		body        map[string]any
		err         error
	}
	requests := make(chan recordedRequest, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := recordedRequest{
			method:      r.Method,
			apiKey:      r.Header.Get("x-api-key"),
			contentType: r.Header.Get("Content-Type"),
			accept:      r.Header.Get("Accept"),
		}
		recorded.err = json.NewDecoder(r.Body).Decode(&recorded.body)
		requests <- recorded
		fmt.Fprint(w, `{"search_id":"search_123","results":[{"title":"Result 1","url":"https://example.com/1","publish_date":"2026-07-01","excerpts":["Snippet 1","Snippet 2"]},{"title":null,"url":"https://example.com/2","publish_date":null,"excerpts":[]}],"warnings":null,"usage":[],"session_id":"session_123"}`)
	}))
	defer ts.Close()

	searcher := NewParallelSearcher("test-key", ts.Client())
	searcher.client.Transport = rewriteTransport{base: ts.Client().Transport, target: ts.URL}

	results, err := searcher.Search(context.Background(), "  hello  ", 2)
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	recorded := <-requests
	if recorded.err != nil {
		t.Fatalf("decode request: %v", recorded.err)
	}
	if recorded.method != http.MethodPost {
		t.Errorf("method = %s, want POST", recorded.method)
	}
	if recorded.apiKey != "test-key" {
		t.Errorf("x-api-key = %q, want test-key", recorded.apiKey)
	}
	if recorded.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", recorded.contentType)
	}
	if recorded.accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", recorded.accept)
	}
	queries, ok := recorded.body["search_queries"].([]any)
	if !ok || len(queries) != 1 || queries[0] != "hello" {
		t.Errorf("search_queries = %#v, want [hello]", recorded.body["search_queries"])
	}
	if got := recorded.body["mode"]; got != "basic" {
		t.Errorf("mode = %#v, want basic", got)
	}
	advanced, ok := recorded.body["advanced_settings"].(map[string]any)
	if !ok || advanced["max_results"] != float64(2) {
		t.Errorf("advanced_settings = %#v, want max_results 2", recorded.body["advanced_settings"])
	}
	if _, ok := recorded.body["max_results"]; ok {
		t.Error("unexpected top-level max_results; GA API expects advanced_settings.max_results")
	}

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Title != "Result 1" || results[0].URL != "https://example.com/1" || results[0].Snippet != "Snippet 1\n\nSnippet 2" {
		t.Fatalf("results[0] = %+v", results[0])
	}
	if results[1].Title != "" {
		t.Fatalf("nullable title = %q, want empty", results[1].Title)
	}
}

func TestParallelSearcherMaxResults(t *testing.T) {
	tests := []struct {
		name        string
		requested   int
		wantResults int
	}{
		{name: "default", requested: 0, wantResults: 10},
		{name: "negative defaults", requested: -1, wantResults: 10},
		{name: "cap", requested: 100, wantResults: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxResults := make(chan int, 1)
			decodeErr := make(chan error, 1)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					decodeErr <- err
				} else if advanced, ok := body["advanced_settings"].(map[string]any); ok {
					value, ok := advanced["max_results"].(float64)
					if !ok {
						decodeErr <- fmt.Errorf("invalid max_results: %#v", advanced["max_results"])
					} else {
						maxResults <- int(value)
					}
				} else {
					decodeErr <- fmt.Errorf("missing advanced_settings: %#v", body)
				}
				fmt.Fprint(w, `{"results":[]}`)
			}))
			defer ts.Close()

			searcher := NewParallelSearcher("test-key", ts.Client())
			searcher.client.Transport = rewriteTransport{base: ts.Client().Transport, target: ts.URL}
			if _, err := searcher.Search(context.Background(), "hello", tt.requested); err != nil {
				t.Fatalf("Search error = %v", err)
			}
			select {
			case err := <-decodeErr:
				t.Fatal(err)
			case got := <-maxResults:
				if got != tt.wantResults {
					t.Fatalf("max_results = %d, want %d", got, tt.wantResults)
				}
			}
		})
	}
}

func TestParallelSearcherRejectsEmptyQuery(t *testing.T) {
	searcher := NewParallelSearcher("test-key", nil)
	if _, err := searcher.Search(context.Background(), " \t\n", 10); err == nil || !strings.Contains(err.Error(), "empty query") {
		t.Fatalf("Search error = %v, want empty query", err)
	}
}

func TestParallelSearcherStatusError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"ref_id":"search_123","message":"rate limited"}}`)
	}))
	defer ts.Close()

	searcher := NewParallelSearcher("test-key", ts.Client())
	searcher.client.Transport = rewriteTransport{base: ts.Client().Transport, target: ts.URL}
	_, err := searcher.Search(context.Background(), "hello", 2)
	var statusErr *providerhttp.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("Search error = %T %v, want *providerhttp.StatusError", err, err)
	}
	if statusErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", statusErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "search_123") {
		t.Fatalf("Search error = %q, want API message and ref_id", err)
	}
}

func TestParallelSearcherMalformedSuccessResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":`)
	}))
	defer ts.Close()

	searcher := NewParallelSearcher("test-key", ts.Client())
	searcher.client.Transport = rewriteTransport{base: ts.Client().Transport, target: ts.URL}
	_, err := searcher.Search(context.Background(), "hello", 2)
	if err == nil || !strings.Contains(err.Error(), "parse response") {
		t.Fatalf("Search error = %v, want parse response error", err)
	}
}

func TestParallelSearcherNonJSONStatusError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "upstream unavailable")
	}))
	defer ts.Close()

	searcher := NewParallelSearcher("test-key", ts.Client())
	searcher.client.Transport = rewriteTransport{base: ts.Client().Transport, target: ts.URL}
	_, err := searcher.Search(context.Background(), "hello", 2)
	var statusErr *providerhttp.StatusError
	if !errors.As(err, &statusErr) || !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("Search error = %T %v, want typed error with raw message", err, err)
	}
}

func TestNewSearcherParallel(t *testing.T) {
	cfg := &config.Config{}
	cfg.Search.Provider = "parallel"

	if _, err := NewSearcher(cfg); err == nil || !strings.Contains(err.Error(), "PARALLEL_API_KEY") {
		t.Fatalf("NewSearcher error = %v, want missing key error", err)
	}

	cfg.Search.Parallel.APIKey = "test-key"
	searcher, err := NewSearcher(cfg)
	if err != nil {
		t.Fatalf("NewSearcher error = %v", err)
	}
	if _, ok := searcher.(*ParallelSearcher); !ok {
		t.Fatalf("searcher = %T, want *ParallelSearcher", searcher)
	}
}
