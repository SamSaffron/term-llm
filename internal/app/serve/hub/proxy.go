package hub

import (
	"net"
	"net/http"
	"strings"
	"time"
)

// StripHopByHopHeaders removes hop-specific response headers.
func StripHopByHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
	for _, key := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		h.Del(key)
	}
}

// ParseNodePath splits a Hub node mount into ID and slash-prefixed remainder.
func ParseNodePath(p string) (id, rest string, ok bool) {
	const prefix = "/node/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	tail := p[len(prefix):]
	if tail == "" {
		return "", "", false
	}
	if slash := strings.IndexByte(tail, '/'); slash >= 0 {
		return tail[:slash], tail[slash:], true
	}
	return tail, "", true
}

// JoinBasePath joins the node base path and proxied remainder.
func JoinBasePath(base, rest string) string {
	base = strings.TrimRight(base, "/")
	if rest == "" {
		return base + "/"
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return base + rest
}

// ForwardedProto reports the public protocol of the incoming Hub request.
func ForwardedProto(r *http.Request) string {
	if r != nil && (r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")) {
		return "https"
	}
	return "http"
}

// NewTransport bounds connection/header waits but leaves streaming responses open.
func NewTransport() *http.Transport {
	return &http.Transport{
		// No environment proxy: the hub dials known node hosts directly, and
		// routing a token-injected request through an HTTP_PROXY would leak
		// the per-node bearer token to that proxy.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}
