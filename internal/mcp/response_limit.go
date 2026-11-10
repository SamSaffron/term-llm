package mcp

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// mcpHTTPBodyMaxBytes is the per-response body limit for ordinary (non-SSE) MCP
// HTTP responses. When a response body exceeds this limit, RoundTrip returns an
// error rather than delivering a partial body, so callers never silently process
// a truncated JSON envelope.
//
// SSE connections (Content-Type: text/event-stream) are explicitly exempt: they
// are long-lived streams with per-event framing enforced by the SDK scanner.
// A lifetime byte cap on SSE would sever legitimate long-running sessions.
const mcpHTTPBodyMaxBytes int64 = 16 * 1024 * 1024 // 16 MiB

// limitedBodyTransport wraps an http.RoundTripper and enforces mcpHTTPBodyMaxBytes
// on ordinary HTTP response bodies. SSE responses pass through without a
// connection-level limit.
//
// The wrapper sits at the outermost layer of the httpClient transport chain so
// that auth-credential transports (headerTransport, OAuthHandler) remain below
// it and are unaffected by the response-body interception.
type limitedBodyTransport struct {
	base  http.RoundTripper
	limit int64
}

// newLimitedBodyTransport returns a limitedBodyTransport capped at
// mcpHTTPBodyMaxBytes wrapping base.
func newLimitedBodyTransport(base http.RoundTripper) *limitedBodyTransport {
	return &limitedBodyTransport{base: base, limit: mcpHTTPBodyMaxBytes}
}

// RoundTrip delegates to the base transport and, for non-SSE responses, reads
// the full body into a bounded buffer before returning. If the body exceeds the
// configured limit, RoundTrip returns a descriptive error rather than a partial
// body.
//
// Auth headers injected by inner transports (headerTransport) are applied before
// the network request; this wrapper only observes the response body.
func (t *limitedBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}

	// SSE connections must not be capped at the connection level. The SDK's
	// scanner already enforces a per-event limit (16 MiB by default), which is
	// the right granularity for a streaming transport.
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct == "text/event-stream" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	// Buffer the entire ordinary response body up to limit+1 bytes.
	// Using limit+1 lets us distinguish "exactly limit" (OK) from "over limit"
	// (error) without an extra round-trip, and prevents the SDK's unbounded
	// io.ReadAll from allocating beyond the budget.
	lr := io.LimitReader(resp.Body, t.limit+1)
	data, readErr := io.ReadAll(lr)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("MCP HTTP response body read: %w", readErr)
	}
	if int64(len(data)) > t.limit {
		return nil, fmt.Errorf("MCP HTTP response body exceeds %d MiB limit (%d bytes read)",
			t.limit/(1024*1024), len(data))
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}
