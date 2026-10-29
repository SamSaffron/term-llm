package mcp

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// headerTransport scopes all configured headers to the configured MCP origin.
// The SDK shares this transport across streamable requests and SSE streams.
// Do not rely on net/http's redirect filtering: it does not recognize arbitrary
// API-key headers, and injecting headers in RoundTrip bypasses that filtering.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
	origin  *url.URL
}

func newHeaderTransport(base http.RoundTripper, headers map[string]string, endpoint string) (*headerTransport, error) {
	origin, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse MCP header origin: %w", err)
	}
	if origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") {
		return nil, fmt.Errorf("MCP header origin must be an absolute HTTP(S) URL")
	}
	return &headerTransport{base: base, headers: headers, origin: origin}, nil
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	// Compare the configured endpoint, not the previous hop or first request:
	// alternate endpoints and reconnects must obey the same credential boundary.
	// Effective ports preserve same-origin default-port redirects without allowing
	// other ports or TLS downgrades to receive configured credentials.
	sameOrigin := sameHeaderOrigin(req.URL, t.origin)
	for k, v := range t.headers {
		if sameOrigin {
			req.Header.Set(k, v)
		} else {
			// Also remove any caller-supplied or redirect-inherited copies.
			req.Header.Del(k)
		}
	}
	return t.base.RoundTrip(req)
}

func sameHeaderOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) && headerOriginPort(a) == headerOriginPort(b)
}

func headerOriginPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}
