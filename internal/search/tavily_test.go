package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTavilySearcherSearch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want Bearer test-key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		fmt.Fprint(w, `{"results":[{"title":"Result 1","url":"https://example.com/1","content":"Snippet 1"},{"title":"Result 2","url":"https://example.com/2","content":"Snippet 2"}]}`)
	}))
	defer ts.Close()

	searcher := NewTavilySearcher("test-key", ts.Client())
	searcher.client.Transport = rewriteTransport{base: ts.Client().Transport, target: ts.URL}

	results, err := searcher.Search(context.Background(), "hello", 2)
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Title != "Result 1" || results[0].URL != "https://example.com/1" || results[0].Snippet != "Snippet 1" {
		t.Fatalf("results[0] = %+v", results[0])
	}
}

type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	targetReq, err := http.NewRequestWithContext(cloned.Context(), cloned.Method, t.target, cloned.Body)
	if err != nil {
		return nil, err
	}
	targetReq.Header = cloned.Header.Clone()
	return t.base.RoundTrip(targetReq)
}
