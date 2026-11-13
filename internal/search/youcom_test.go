package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

// recordingTransport captures the outgoing request (before it is redirected
// to the test server) so tests can assert on the real You.com URL.
type recordingTransport struct {
	base     http.RoundTripper
	target   string
	requests chan *http.Request
	bodies   chan []byte
}

func (t recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	t.requests <- req.Clone(req.Context())
	t.bodies <- body
	targetReq, err := http.NewRequestWithContext(req.Context(), req.Method, t.target, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	targetReq.Header = req.Header.Clone()
	return t.base.RoundTrip(targetReq)
}

func newRecordingYouCom(t *testing.T, apiKey string, handler http.HandlerFunc) (*YouComSearcher, chan *http.Request, chan []byte) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	requests := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	searcher := NewYouComSearcher(apiKey, ts.Client())
	searcher.client.Transport = recordingTransport{base: ts.Client().Transport, target: ts.URL, requests: requests, bodies: bodies}
	return searcher, requests, bodies
}

const youcomFixture = `{"results":{"web":[{"title":"Result 1","url":"https://example.com/1","description":"Snippet 1","snippets":["long body"]},{"title":"Result 2","url":"https://example.com/2","description":"","snippets":["Fallback A","Fallback B"]},{"title":"No URL","url":""}],"news":[{"title":"News 1","url":"https://news.example/1","description":"Breaking"}]},"metadata":{"query":"hello"}}`

func TestYouComSearcherKeyless(t *testing.T) {
	searcher, requests, _ := newRecordingYouCom(t, "", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, youcomFixture)
	})

	results, err := searcher.Search(context.Background(), "  hello  ", 5)
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	req := <-requests
	if req.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", req.Method)
	}
	if got := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path; got != youcomKeylessURL {
		t.Errorf("url = %s, want %s", got, youcomKeylessURL)
	}
	if want := (url.Values{"query": {"hello"}, "count": {"5"}}).Encode(); req.URL.RawQuery != want {
		t.Errorf("query = %q, want %q", req.URL.RawQuery, want)
	}
	if got := req.Header.Get("X-API-Key"); got != "" {
		t.Errorf("X-API-Key = %q, want empty", got)
	}
	if got := req.Header.Get("User-Agent"); got != youcomUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, youcomUserAgent)
	}

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3: %+v", len(results), results)
	}
	if results[0].Title != "Result 1" || results[0].URL != "https://example.com/1" || results[0].Snippet != "Snippet 1" {
		t.Fatalf("results[0] = %+v", results[0])
	}
	if results[1].Snippet != "Fallback A\n\nFallback B" {
		t.Fatalf("results[1].Snippet = %q, want joined snippets", results[1].Snippet)
	}
	if results[2].URL != "https://news.example/1" {
		t.Fatalf("results[2] = %+v, want the news result", results[2])
	}
}

func TestYouComSearcherKeyed(t *testing.T) {
	searcher, requests, bodies := newRecordingYouCom(t, " test-key ", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, youcomFixture)
	})

	results, err := searcher.Search(context.Background(), "hello", 2)
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	req := <-requests
	body := <-bodies
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.URL.String() != youcomKeyedURL {
		t.Errorf("url = %s, want %s", req.URL, youcomKeyedURL)
	}
	if got := req.Header.Get("X-API-Key"); got != "test-key" {
		t.Errorf("X-API-Key = %q, want test-key", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if decoded["query"] != "hello" || decoded["count"] != float64(2) {
		t.Errorf("body = %#v, want query hello and count 2", decoded)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (capped to maxResults)", len(results))
	}
}

func TestYouComSearcherMaxResults(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		wantCount string
	}{
		{name: "default", requested: 0, wantCount: "10"},
		{name: "cap", requested: 100, wantCount: "20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searcher, requests, _ := newRecordingYouCom(t, "", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"results":{"web":[]}}`)
			})
			if _, err := searcher.Search(context.Background(), "hello", tt.requested); err != nil {
				t.Fatalf("Search error = %v", err)
			}
			if got := (<-requests).URL.Query().Get("count"); got != tt.wantCount {
				t.Fatalf("count = %s, want %s", got, tt.wantCount)
			}
		})
	}
}

func TestYouComSearcherRejectsEmptyQuery(t *testing.T) {
	searcher := NewYouComSearcher("", nil)
	if _, err := searcher.Search(context.Background(), " \t\n", 10); err == nil || !strings.Contains(err.Error(), "empty query") {
		t.Fatalf("Search error = %v, want empty query", err)
	}
}

func TestYouComSearcherKeylessRateLimit(t *testing.T) {
	searcher, _, _ := newRecordingYouCom(t, "", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "rate limited")
	})
	_, err := searcher.Search(context.Background(), "hello", 2)
	var statusErr *providerhttp.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Search error = %T %v, want 429 *providerhttp.StatusError", err, err)
	}
	if !strings.Contains(err.Error(), "YDC_API_KEY") {
		t.Fatalf("Search error = %q, want a hint about YDC_API_KEY", err)
	}
}

func TestYouComSearcherMalformedSuccessResponse(t *testing.T) {
	searcher, _, _ := newRecordingYouCom(t, "", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":`)
	})
	if _, err := searcher.Search(context.Background(), "hello", 2); err == nil || !strings.Contains(err.Error(), "parse response") {
		t.Fatalf("Search error = %v, want parse response error", err)
	}
}

func TestNewSearcherYouComNeedsNoKey(t *testing.T) {
	t.Setenv("YDC_API_KEY", "")
	cfg := &config.Config{}
	cfg.Search.Provider = "youcom"

	if err := Available(cfg); err != nil {
		t.Fatalf("Available error = %v, want nil (keyless)", err)
	}
	searcher, err := NewSearcher(cfg)
	if err != nil {
		t.Fatalf("NewSearcher error = %v", err)
	}
	yc, ok := searcher.(*YouComSearcher)
	if !ok {
		t.Fatalf("searcher = %T, want *YouComSearcher", searcher)
	}
	if yc.apiKey != "" {
		t.Fatalf("apiKey = %q, want empty", yc.apiKey)
	}

	t.Setenv("YDC_API_KEY", "env-key")
	searcher, err = NewSearcher(cfg)
	if err != nil {
		t.Fatalf("NewSearcher error = %v", err)
	}
	if got := searcher.(*YouComSearcher).apiKey; got != "env-key" {
		t.Fatalf("apiKey = %q, want env-key from YDC_API_KEY", got)
	}
}
