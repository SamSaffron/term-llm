package mcp

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// roundTripFuncTransport adapts a function to http.RoundTripper.
// (roundTripFunc is already defined in client_test.go; use a distinct name.)
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func makeResponse(statusCode int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// --- Unit tests for limitedBodyTransport ---

func TestLimitedBodyTransport_SmallBodyPassesThrough(t *testing.T) {
	body := `{"result":"ok"}`
	tr := newLimitedBodyTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		return makeResponse(200, "application/json", body), nil
	}))
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestLimitedBodyTransport_OversizedBodyReturnsError(t *testing.T) {
	// Build a body just over the limit (use a small test limit to avoid 16 MiB alloc).
	const testLimit int64 = 1024
	big := strings.Repeat("x", int(testLimit)+1)
	tr := &limitedBodyTransport{
		base: rtFunc(func(*http.Request) (*http.Response, error) {
			return makeResponse(200, "application/json", big), nil
		}),
		limit: testLimit,
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp", nil)
	_, err := tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error for oversized body, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error message should mention 'exceeds', got: %v", err)
	}
}

func TestLimitedBodyTransport_ExactLimitBodyPassesThrough(t *testing.T) {
	// A body exactly at the limit must succeed (not be rejected).
	const testLimit int64 = 512
	exact := strings.Repeat("y", int(testLimit))
	tr := &limitedBodyTransport{
		base: rtFunc(func(*http.Request) (*http.Response, error) {
			return makeResponse(200, "application/json", exact), nil
		}),
		limit: testLimit,
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("exact-limit body should not error: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if int64(len(got)) != testLimit {
		t.Errorf("body length = %d, want %d", len(got), testLimit)
	}
}

func TestLimitedBodyTransport_SSEPassesThroughUnlimited(t *testing.T) {
	// SSE responses must not be capped at the connection level.
	// Use a body larger than the limit to verify no error is returned.
	const testLimit int64 = 64
	big := strings.Repeat("data: event\n\n", 20) // well over testLimit
	tr := &limitedBodyTransport{
		base: rtFunc(func(*http.Request) (*http.Response, error) {
			return makeResponse(200, "text/event-stream", big), nil
		}),
		limit: testLimit,
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/mcp/sse", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("SSE response should pass through without error: %v", err)
	}
	// Body must be readable in full (not truncated).
	got, _ := io.ReadAll(resp.Body)
	if string(got) != big {
		t.Errorf("SSE body was modified; got %d bytes, want %d bytes", len(got), len(big))
	}
}

func TestLimitedBodyTransport_SSEWithMimeParams(t *testing.T) {
	// text/event-stream may carry charset parameters; still must pass through.
	const testLimit int64 = 32
	body := strings.Repeat(":", 100)
	tr := &limitedBodyTransport{
		base: rtFunc(func(*http.Request) (*http.Response, error) {
			resp := makeResponse(200, "text/event-stream; charset=utf-8", body)
			return resp, nil
		}),
		limit: testLimit,
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/sse", nil)
	_, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("SSE with charset param must pass through: %v", err)
	}
}

func TestLimitedBodyTransport_NilBodyPassesThrough(t *testing.T) {
	tr := newLimitedBodyTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Header: http.Header{}, Body: nil}, nil
	}))
	req, _ := http.NewRequest(http.MethodDelete, "https://example.com/mcp", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("nil body should not error: %v", err)
	}
	if resp.Body != nil {
		t.Error("expected nil body preserved")
	}
}

func TestLimitedBodyTransport_BaseErrorPropagated(t *testing.T) {
	wantErr := fmt.Errorf("connection refused")
	tr := newLimitedBodyTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, wantErr
	}))
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp", nil)
	_, err := tr.RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("base transport error not propagated: %v", err)
	}
}

func TestLimitedBodyTransport_AuthHeaderPreserved(t *testing.T) {
	// Verify that when limitedBodyTransport wraps a headerTransport, auth
	// credentials added by headerTransport are still present in the request
	// that reaches the base transport.
	var capturedAuth string
	base := rtFunc(func(req *http.Request) (*http.Response, error) {
		capturedAuth = req.Header.Get("Authorization")
		return makeResponse(200, "application/json", `{}`), nil
	})
	ht, err := newHeaderTransport(base, map[string]string{"Authorization": "Bearer secret"}, "https://example.com")
	if err != nil {
		t.Fatalf("newHeaderTransport: %v", err)
	}
	tr := newLimitedBodyTransport(ht)

	req, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if capturedAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", capturedAuth, "Bearer secret")
	}
}

// --- Integration test against a real HTTP server ---

func TestLimitedBodyTransport_RealHTTPServer_OversizedBody(t *testing.T) {
	// Use a small limit so the test doesn't allocate 16 MiB.
	const testLimit int64 = 2048
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write more than testLimit bytes.
		for i := int64(0); i <= testLimit; i++ {
			fmt.Fprint(w, "x")
		}
	}))
	defer srv.Close()

	tr := &limitedBodyTransport{base: http.DefaultTransport, limit: testLimit}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	_, err := tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error for oversized HTTP response, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention 'exceeds': %v", err)
	}
}

func TestLimitedBodyTransport_RealHTTPServer_SmallBody(t *testing.T) {
	body := `{"jsonrpc":"2.0","result":"ok"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	tr := newLimitedBodyTransport(http.DefaultTransport)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestLimitedBodyTransport_RealHTTPServer_SSENoConnectionCap(t *testing.T) {
	// Use a small limit and an SSE content-type to verify the connection
	// passes through without an oversized error even when body > limit.
	const testLimit int64 = 64
	body := strings.Repeat("data: ping\n\n", 20) // 240 bytes > 64 bytes limit

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	tr := &limitedBodyTransport{base: http.DefaultTransport, limit: testLimit}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("SSE must not be capped at connection level: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("SSE body truncated: got %d bytes, want %d bytes", len(got), len(body))
	}
}

// --- buildProgrammatic image byte cap ---

func TestBuildProgrammatic_OversizedImageRejectsWithNil(t *testing.T) {
	// A single image exceeding programmaticMaxImageBytes must be rejected.
	imageBytes := make([]byte, programmaticMaxImageBytes+1)
	result := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.ImageContent{MIMEType: "image/png", Data: imageBytes},
		},
	}
	raw, oversized := buildProgrammatic(result)
	if !oversized {
		t.Fatal("expected oversized=true for image exceeding 16 MiB")
	}
	if raw != nil {
		t.Error("expected nil Programmatic when image oversized")
	}
}

func TestBuildProgrammatic_SmallImageWithinBudget(t *testing.T) {
	// An image smaller than programmaticMaxImageBytes must be accepted (even
	// though the total JSON will also include base64 overhead).
	imageBytes := make([]byte, 1024) // well within 16 MiB
	result := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.TextContent{Text: "caption"},
			&sdkmcp.ImageContent{MIMEType: "image/png", Data: imageBytes},
		},
	}
	raw, oversized := buildProgrammatic(result)
	if oversized {
		t.Fatal("small image should not trigger oversized")
	}
	if raw == nil {
		t.Fatal("expected non-nil Programmatic for small image")
	}
}

func TestLimitedBodyTransport_ErrorBodyCannotClaimSSEExemption(t *testing.T) {
	tr := newLimitedBodyTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		return makeResponse(http.StatusInternalServerError, "text/event-stream", strings.Repeat("x", 2048)), nil
	}))
	tr.limit = 1024
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
	if resp, err := tr.RoundTrip(req); err == nil || resp != nil {
		t.Fatalf("error body cap bypassed: %v %v", resp, err)
	}
}
