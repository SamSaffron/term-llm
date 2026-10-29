package mcp

import (
	"net/http"
	"net/url"
	"testing"
)

func TestHeaderTransportEffectiveOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, target string
		allowed                bool
	}{
		{"HTTPS default explicit", "https://example.com/mcp", "https://example.com:443/next", true},
		{"HTTPS default implicit", "https://example.com:443/mcp", "https://example.com/next", true},
		{"HTTP default explicit", "http://example.com/mcp", "http://example.com:80/next", true},
		{"HTTP default implicit", "http://example.com:80/mcp", "http://example.com/next", true},
		{"IPv6 default", "https://[::1]/mcp", "https://[::1]:443/next", true},
		{"nondefault equal", "https://example.com:8443/mcp", "https://example.com:8443/next", true},
		{"different port", "https://example.com/mcp", "https://example.com:8443/next", false},
		{"downgrade default", "https://example.com:443/mcp", "http://example.com:80/next", false},
		{"downgrade same port", "https://example.com:443/mcp", "http://example.com:443/next", false},
		{"other host", "https://example.com/mcp", "https://other.example.com:443/next", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var observed string
			transport, err := newHeaderTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				observed = req.Header.Get("X-Secret")
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			}), map[string]string{"X-Secret": "configured-secret"}, tc.endpoint)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Secret", "caller-secret")
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			want := ""
			if tc.allowed {
				want = "configured-secret"
			}
			if observed != want {
				t.Errorf("%s -> %s secret=%q, want %q", tc.endpoint, tc.target, observed, want)
			}
		})
	}
}

func BenchmarkMCPOriginComparison(b *testing.B) {
	origin, err := url.Parse("https://example.com/mcp")
	if err != nil {
		b.Fatal(err)
	}
	target, err := url.Parse("https://example.com:443/messages")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = sameHeaderOrigin(target, origin)
	}
}
