package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCreateHTTPTransport_RedirectCredentials(t *testing.T) {
	// Treat every configured header as potentially secret, not just the ones
	// net/http knows to strip. Include noncanonical names and caller-supplied
	// copies to cover both transport injection and redirect header inheritance.
	headers := map[string]string{
		"authorization":       "Bearer secret",
		"Proxy-Authorization": "Basic secret",
		"Cookie":              "session=secret",
		"x-api-key":           "api-secret",
		"X-Custom-Secret":     "custom-secret",
	}
	// GET covers SSE connection/reconnection requests; POST and DELETE cover
	// streamable RPC and session shutdown through the same SDK HTTP client.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		for _, destination := range []string{"same origin", "cross origin", "HTTPS downgrade"} {
			t.Run(method+"/"+destination, func(t *testing.T) {
				initial := make(chan http.Header, 1)
				final := make(chan http.Header, 1)
				finish := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					final <- r.Header.Clone()
					if r.Method != method {
						t.Errorf("redirect method = %s, want %s", r.Method, method)
					}
					if method == http.MethodGet {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					w.WriteHeader(http.StatusOK)
				})
				target := httptest.NewServer(finish)
				defer target.Close()
				redirectURL := target.URL + "/redirected"
				originHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirected" {
						finish.ServeHTTP(w, r)
						return
					}
					initial <- r.Header.Clone()
					http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
				})
				var origin *httptest.Server
				if destination == "HTTPS downgrade" {
					origin = httptest.NewTLSServer(originHandler)
				} else {
					origin = httptest.NewServer(originHandler)
				}
				defer origin.Close()
				if destination == "same origin" {
					redirectURL = origin.URL + "/redirected"
				}

				client := NewClient("redirect-test", ServerConfig{
					URL: origin.URL + "/mcp", Headers: headers,
					OAuth: &OAuthConfig{Disabled: true},
				})
				transport, err := client.createHTTPTransport()
				if err != nil {
					t.Fatal(err)
				}
				httpClient := transport.(*sdkmcp.StreamableClientTransport).HTTPClient
				wrapped := httpClient.Transport.(*headerTransport)
				base := wrapped.base.(*http.Transport)
				defer base.CloseIdleConnections()
				if destination == "HTTPS downgrade" {
					base.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				}
				req, err := http.NewRequestWithContext(t.Context(), method, client.config.URL, strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				for name, value := range headers {
					req.Header.Set(name, value)
				}
				req.Header.Set("X-Request-ID", "public-id")
				if method == http.MethodGet {
					req.Header.Set("Accept", "text/event-stream")
				}
				resp, err := httpClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				first := <-initial
				for name, want := range headers {
					if got := first.Get(name); got != want {
						t.Errorf("initial %s = %q, want %q", name, got, want)
					}
				}
				got := <-final
				for name, value := range headers {
					want := ""
					if destination == "same origin" {
						want = value
					}
					if got.Get(name) != want {
						t.Errorf("redirected %s = %q, want %q", name, got.Get(name), want)
					}
				}
				if got.Get("X-Request-ID") != "public-id" {
					t.Error("redirect lost unrelated request header")
				}
			})
		}
	}
}

func TestHeaderTransport_OriginBoundary(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
		want string
	}{
		{name: "same origin alternate endpoint", url: "https://example.com/messages?session=1", want: "configured-secret"},
		{name: "case insensitive host", url: "https://EXAMPLE.COM/messages", want: "configured-secret"},
		{name: "same host downgrade", url: "http://example.com/mcp"},
		{name: "different port", url: "https://example.com:8443/mcp"},
		{name: "subdomain", url: "https://other.example.com/mcp"},
		{name: "different host", url: "https://other.example/mcp"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var observed http.Header
			transport, err := newHeaderTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				observed = req.Header.Clone()
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			}), map[string]string{"x-secret": "configured-secret"}, "https://example.com/mcp")
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tt.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Start directly at the alternate endpoint, without a redirect, as
			// an SSE endpoint announcement or reconnect could do.
			req.Header.Set("X-Secret", "caller-secret")
			req.Header.Set("Accept", "text/event-stream")
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if got := observed.Get("X-Secret"); got != tt.want {
				t.Errorf("secret header = %q, want %q", got, tt.want)
			}
			if got := observed.Get("Accept"); got != "text/event-stream" {
				t.Errorf("Accept = %q, want text/event-stream", got)
			}
			if got := req.Header.Get("X-Secret"); got != "caller-secret" {
				t.Errorf("original request header mutated to %q", got)
			}
		})
	}
}
