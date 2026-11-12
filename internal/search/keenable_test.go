package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func newKeenableTestServer(t *testing.T, check func(r *http.Request, body keenableRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		var body keenableRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		check(r, body)
		fmt.Fprint(w, `{"query":"hello","mode":"pro","results":[{"title":"Result 1","url":"https://example.com/1","snippet":"Snippet 1"},{"title":"No URL","url":"","snippet":"dropped"},{"title":"Result 2","url":"https://example.com/2","snippet":"Snippet 2"}]}`)
	}))
}

func TestKeenableSearcherKeyless(t *testing.T) {
	var gotURL string
	ts := newKeenableTestServer(t, func(r *http.Request, body keenableRequest) {
		if got := r.Header.Get("X-Keenable-Title"); got != "term-llm" {
			t.Fatalf("X-Keenable-Title = %q, want term-llm", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "" {
			t.Fatalf("X-API-Key = %q, want empty", got)
		}
		if body.Query != "hello" || body.MaxResults != 2 {
			t.Fatalf("body = %+v", body)
		}
	})
	defer ts.Close()

	searcher := NewKeenableSearcher("", ts.Client())
	searcher.client.Transport = recordingRewriteTransport{base: ts.Client().Transport, target: ts.URL, gotURL: &gotURL}

	results, err := searcher.Search(context.Background(), "hello", 2)
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if gotURL != keenablePublicURL {
		t.Fatalf("url = %q, want %q", gotURL, keenablePublicURL)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (entry without URL dropped)", len(results))
	}
	if results[0].Title != "Result 1" || results[0].URL != "https://example.com/1" || results[0].Snippet != "Snippet 1" {
		t.Fatalf("results[0] = %+v", results[0])
	}
}

func TestKeenableSearcherKeyed(t *testing.T) {
	var gotURL string
	ts := newKeenableTestServer(t, func(r *http.Request, body keenableRequest) {
		if got := r.Header.Get("X-API-Key"); got != "test-key" {
			t.Fatalf("X-API-Key = %q, want test-key", got)
		}
		if got := r.Header.Get("X-Keenable-Title"); got != "" {
			t.Fatalf("X-Keenable-Title = %q, want empty", got)
		}
		if body.MaxResults != 20 {
			t.Fatalf("max_results = %d, want clamp to 20", body.MaxResults)
		}
	})
	defer ts.Close()

	searcher := NewKeenableSearcher(" test-key ", ts.Client())
	searcher.client.Transport = recordingRewriteTransport{base: ts.Client().Transport, target: ts.URL, gotURL: &gotURL}

	if _, err := searcher.Search(context.Background(), "hello", 50); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if gotURL != keenableKeyedURL {
		t.Fatalf("url = %q, want %q", gotURL, keenableKeyedURL)
	}
}

func TestKeenableSearcherRateLimitHint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"code":"public_rate_limit_hourly"}`)
	}))
	defer ts.Close()

	searcher := NewKeenableSearcher("", ts.Client())
	searcher.client.Transport = rewriteTransport{base: ts.Client().Transport, target: ts.URL}

	_, err := searcher.Search(context.Background(), "hello", 5)
	if err == nil || !strings.Contains(err.Error(), "KEENABLE_API_KEY") {
		t.Fatalf("err = %v, want KEENABLE_API_KEY hint", err)
	}
}

func TestKeenableSearcherEmptyQuery(t *testing.T) {
	if _, err := NewKeenableSearcher("", nil).Search(context.Background(), "  ", 5); err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestKeenableFactoryNeedsNoKey(t *testing.T) {
	t.Setenv("KEENABLE_API_KEY", "")
	cfg := &config.Config{}
	cfg.Search.Provider = "keenable"
	if err := Available(cfg); err != nil {
		t.Fatalf("Available() = %v, want nil", err)
	}
	s, err := NewSearcher(cfg)
	if err != nil {
		t.Fatalf("NewSearcher() = %v", err)
	}
	if _, ok := s.(*KeenableSearcher); !ok {
		t.Fatalf("NewSearcher() = %T, want *KeenableSearcher", s)
	}
}

// TestKeenableSearcherLive calls the real keyless endpoint.
// Run with KEENABLE_LIVE=1 go test ./internal/search -run Live.
func TestKeenableSearcherLive(t *testing.T) {
	if os.Getenv("KEENABLE_LIVE") == "" {
		t.Skip("set KEENABLE_LIVE=1 to call the live API")
	}
	results, err := NewKeenableSearcher("", nil).Search(context.Background(), "golang context package", 3)
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		t.Fatalf("status error: %v", err)
	}
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if len(results) == 0 || !strings.HasPrefix(results[0].URL, "http") {
		t.Fatalf("results = %+v", results)
	}
}

type recordingRewriteTransport struct {
	base   http.RoundTripper
	target string
	gotURL *string
}

func (t recordingRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*t.gotURL = req.URL.String()
	return rewriteTransport{base: t.base, target: t.target}.RoundTrip(req)
}
