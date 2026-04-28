package llm

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
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
	capturedTargetURLs := []string{}
	tool := NewReadURLTool()
	tool.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "example.com" {
				t.Fatalf("unexpected host %q", req.URL.Host)
			}

			capturedTargetURLs = append(capturedTargetURLs, req.URL.String())
			switch len(capturedTargetURLs) {
			case 1:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("ok")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			case 2:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: &failAfterNReadCloser{
						remaining: maxReadURLChars + 1,
						err:       readErr,
					},
					Header:  make(http.Header),
					Request: req,
				}, nil
			default:
				t.Fatalf("unexpected request count %d", len(capturedTargetURLs))
				return nil, nil
			}
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
	if got, want := len(capturedTargetURLs), 2; got != want {
		t.Fatalf("expected %d target requests, got %d (%v)", want, got, capturedTargetURLs)
	}
	for i, got := range capturedTargetURLs {
		if want := "https://example.com/article"; got != want {
			t.Fatalf("expected request %d URL %q, got %q", i, want, got)
		}
	}
	if strings.Contains(out.Content, readErr.Error()) {
		t.Fatalf("expected limited read to avoid body read error, got %q", out.Content)
	}

	if !strings.HasSuffix(out.Content, readURLTruncationSuffix) {
		start := len(out.Content) - len(readURLTruncationSuffix) - 20
		if start < 0 {
			start = 0
		}
		t.Fatalf("expected truncated content suffix, got %q", out.Content[start:])
	}
	if got, want := len(out.Content), maxReadURLChars+len(readURLTruncationSuffix); got != want {
		t.Fatalf("expected content length %d, got %d", want, got)
	}
	if out.Content[:32] != strings.Repeat("a", 32) {
		t.Fatalf("expected response body prefix to be preserved")
	}
}

func TestReadURLToolExecuteTruncatesByRunes(t *testing.T) {
	origLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { readURLLookupIP = origLookup }()

	body := strings.Repeat("界", maxReadURLChars) + "🙂tail"
	requests := 0

	tool := NewReadURLTool()
	tool.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "example.com" {
				t.Fatalf("unexpected host %q", req.URL.Host)
			}

			requests++
			switch requests {
			case 1:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("ok")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			case 2:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			default:
				t.Fatalf("unexpected request count %d", requests)
				return nil, nil
			}
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
	if !utf8.ValidString(out.Content) {
		t.Fatalf("expected valid UTF-8 output")
	}
	if !strings.HasSuffix(out.Content, readURLTruncationSuffix) {
		t.Fatalf("expected truncation suffix")
	}

	content := strings.TrimSuffix(out.Content, readURLTruncationSuffix)
	if got, want := utf8.RuneCountInString(content), maxReadURLChars; got != want {
		t.Fatalf("expected %d runes before suffix, got %d", want, got)
	}
	if strings.ContainsRune(content, '🙂') {
		t.Fatalf("expected content to exclude runes past the limit")
	}
	if strings.ContainsRune(content, '\uFFFD') {
		t.Fatalf("expected truncation not to introduce replacement characters")
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

func TestReadURLToolExecuteRejectsRedirectsToBlockedHosts(t *testing.T) {
	origLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "example.com" {
			t.Fatalf("unexpected host lookup %q", host)
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { readURLLookupIP = origLookup }()

	requests := []string{}
	tool := NewReadURLTool()
	tool.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req.URL.String())
			if req.URL.Host != "example.com" {
				t.Fatalf("unexpected request to %q", req.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusFound,
				Body:       io.NopCloser(strings.NewReader("redirect")),
				Header: http.Header{
					"Location": []string{"http://127.0.0.1/admin"},
				},
				Request: req,
			}, nil
		}),
	}

	args, err := json.Marshal(map[string]string{"url": "https://example.com/article"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	_, err = tool.Execute(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "url host is not allowed") {
		t.Fatalf("expected blocked redirect host error, got %v", err)
	}
	if got, want := len(requests), 1; got != want {
		t.Fatalf("expected %d outbound request before rejection, got %d (%v)", want, got, requests)
	}
	if got, want := requests[0], "https://example.com/article"; got != want {
		t.Fatalf("expected redirect check against %q, got %q", want, got)
	}
}

func TestReadURLToolExecuteFollowsAllowedRedirectsBeforeFetchingContent(t *testing.T) {
	origLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		switch host {
		case "example.com":
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		case "www.example.com":
			return []net.IP{net.ParseIP("93.184.216.35")}, nil
		default:
			t.Fatalf("unexpected host lookup %q", host)
			return nil, nil
		}
	}
	defer func() { readURLLookupIP = origLookup }()

	requests := []string{}
	wwwRequests := 0
	tool := NewReadURLTool()
	tool.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req.URL.String())
			switch req.URL.Host {
			case "example.com":
				return &http.Response{
					StatusCode: http.StatusMovedPermanently,
					Body:       io.NopCloser(strings.NewReader("redirect")),
					Header: http.Header{
						"Location": []string{"https://www.example.com/article"},
					},
					Request: req,
				}, nil
			case "www.example.com":
				wwwRequests++
				body := "ok"
				if wwwRequests == 2 {
					body = "content"
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			default:
				t.Fatalf("unexpected host %q", req.URL.Host)
				return nil, nil
			}
		}),
	}

	args, err := json.Marshal(map[string]string{"url": "https://example.com/start"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got, want := out.Content, "content"; got != want {
		t.Fatalf("expected content %q, got %q", want, got)
	}
	if got, want := len(requests), 3; got != want {
		t.Fatalf("expected %d total requests, got %d (%v)", want, got, requests)
	}
	if got, want := requests[0], "https://example.com/start"; got != want {
		t.Fatalf("expected first request %q, got %q", want, got)
	}
	if got, want := requests[1], "https://www.example.com/article"; got != want {
		t.Fatalf("expected second request %q, got %q", want, got)
	}
	if got, want := requests[2], "https://www.example.com/article"; got != want {
		t.Fatalf("expected third request %q, got %q", want, got)
	}
}

func TestReadURLToolExecutePinsValidatedIPsDuringFetch(t *testing.T) {
	origLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "example.com" {
			t.Fatalf("unexpected host lookup %q", host)
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { readURLLookupIP = origLookup }()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "content")
	}))
	defer server.Close()

	origDial := readURLDialContext
	dialedAddrs := []string{}
	readURLDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialedAddrs = append(dialedAddrs, address)
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer func() { readURLDialContext = origDial }()

	tool := NewReadURLTool()
	tool.client = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	args, err := json.Marshal(map[string]string{"url": "https://example.com/article"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got, want := out.Content, "content"; got != want {
		t.Fatalf("expected content %q, got %q", want, got)
	}
	if got, want := len(dialedAddrs), 2; got != want {
		t.Fatalf("expected %d dial attempts, got %d (%v)", want, got, dialedAddrs)
	}
	for i, got := range dialedAddrs {
		if want := "93.184.216.34:443"; got != want {
			t.Fatalf("expected dial %d address %q, got %q", i, want, got)
		}
	}
}

func TestResolveReadURLTargetPinsValidatedIPsDuringRedirectCheck(t *testing.T) {
	origLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "example.com" {
			t.Fatalf("unexpected host lookup %q", host)
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { readURLLookupIP = origLookup }()

	origDial := readURLDialContext
	dialedAddrs := []string{}
	readURLDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialedAddrs = append(dialedAddrs, address)
		return nil, errors.New("dial blocked for test")
	}
	defer func() { readURLDialContext = origDial }()

	_, err := resolveReadURLTarget(context.Background(), &http.Client{Transport: &http.Transport{}}, "https://example.com/article")
	if err == nil || !strings.Contains(err.Error(), "check url redirects") {
		t.Fatalf("expected redirect check error, got %v", err)
	}
	if got, want := len(dialedAddrs), 1; got != want {
		t.Fatalf("expected %d dial attempt, got %d (%v)", want, got, dialedAddrs)
	}
	if got, want := dialedAddrs[0], "93.184.216.34:443"; got != want {
		t.Fatalf("expected pinned dial address %q, got %q", want, got)
	}
}

var _ io.ReadCloser = (*failAfterNReadCloser)(nil)
