package llm

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type stubURLFetcher struct {
	gotURL string
	body   string
}

func (f *stubURLFetcher) FetchURL(ctx context.Context, url string) (string, error) {
	f.gotURL = url
	return f.body, nil
}

func TestReadURLToolWithFetcherUsesResolvedURL(t *testing.T) {
	origLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { readURLLookupIP = origLookup }()

	fetcher := &stubURLFetcher{body: "fetched via custom provider"}
	tool := NewReadURLToolWithFetcher(fetcher)
	tool.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	args, _ := json.Marshal(map[string]string{"url": "example.com/path"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if out.Content != "fetched via custom provider" {
		t.Fatalf("got output %q", out.Content)
	}
	if fetcher.gotURL != "https://example.com/path" {
		t.Fatalf("fetcher got URL %q, want normalized https URL", fetcher.gotURL)
	}
}
