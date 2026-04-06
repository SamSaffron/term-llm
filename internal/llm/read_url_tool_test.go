package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type failAfterNReadCloser struct {
	remaining int
	err       error
}

func (r *failAfterNReadCloser) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, r.err
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'a'
	}
	r.remaining -= len(p)
	return len(p), nil
}

func (r *failAfterNReadCloser) Close() error {
	return nil
}

func TestReadURLToolExecuteLimitsBodyReadBeforeTruncating(t *testing.T) {
	origLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { readURLLookupIP = origLookup }()

	readErr := errors.New("read past limit")
	capturedURL := ""
	tool := NewReadURLTool()
	tool.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			capturedURL = req.URL.String()
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: &failAfterNReadCloser{
					remaining: maxReadURLChars + 1,
					err:       readErr,
				},
				Header: make(http.Header),
			}, nil
		}),
	}

	args, err := json.Marshal(map[string]string{"url": "example.com/article"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got, want := capturedURL, "https://r.jina.ai/https://example.com/article"; got != want {
		t.Fatalf("expected fetch URL %q, got %q", want, got)
	}
	if strings.Contains(out.Content, readErr.Error()) {
		t.Fatalf("expected limited read to avoid body read error, got %q", out.Content)
	}

	const truncationSuffix = "\n\n[Content truncated at 50,000 characters]"
	if !strings.HasSuffix(out.Content, truncationSuffix) {
		start := len(out.Content) - len(truncationSuffix) - 20
		if start < 0 {
			start = 0
		}
		t.Fatalf("expected truncated content suffix, got %q", out.Content[start:])
	}
	if got, want := len(out.Content), maxReadURLChars+len(truncationSuffix); got != want {
		t.Fatalf("expected content length %d, got %d", want, got)
	}
	if out.Content[:32] != strings.Repeat("a", 32) {
		t.Fatalf("expected response body prefix to be preserved")
	}
}

func TestReadURLToolExecuteRejectsPrivateHosts(t *testing.T) {
	blockedURLs := []string{
		"localhost",
		"http://127.0.0.1/admin",
		"https://10.0.0.1/status",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"https://metadata.google.internal/computeMetadata/v1/",
	}

	for _, rawURL := range blockedURLs {
		t.Run(rawURL, func(t *testing.T) {
			called := false
			tool := NewReadURLTool()
			tool.client = &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					called = true
					return nil, errors.New("request should not be sent")
				}),
			}

			args, err := json.Marshal(map[string]string{"url": rawURL})
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}

			_, err = tool.Execute(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "url host is not allowed") {
				t.Fatalf("expected blocked host error, got %v", err)
			}
			if called {
				t.Fatalf("expected blocked host to prevent outbound request")
			}
		})
	}
}

func TestReadURLToolExecuteRejectsHostsResolvingToPrivateIPs(t *testing.T) {
	origLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "intranet.example.com" {
			t.Fatalf("unexpected host lookup %q", host)
		}
		return []net.IP{net.ParseIP("10.0.0.25")}, nil
	}
	defer func() { readURLLookupIP = origLookup }()

	called := false
	tool := NewReadURLTool()
	tool.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("request should not be sent")
		}),
	}

	args, err := json.Marshal(map[string]string{"url": "https://intranet.example.com/status"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	_, err = tool.Execute(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "url host is not allowed") {
		t.Fatalf("expected blocked host error, got %v", err)
	}
	if called {
		t.Fatalf("expected blocked host to prevent outbound request")
	}
}

var _ io.ReadCloser = (*failAfterNReadCloser)(nil)
