package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ReadURLToolName         = "read_url"
	maxReadURLChars         = 50000
	maxReadURLRedirects     = 10
	readURLTruncationSuffix = "\n\n[Content truncated at 50,000 characters]"
)

var readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

var readURLDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}

type readURLTarget struct {
	url string
	ips []net.IP
}

// ReadURLTool fetches web pages with SSRF protections.
type ReadURLTool struct {
	client *http.Client
}

func NewReadURLTool() *ReadURLTool {
	return &ReadURLTool{
		client: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}
}

func (t *ReadURLTool) Spec() ToolSpec {
	return ReadURLToolSpec()
}

func (t *ReadURLTool) Preview(args json.RawMessage) string {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &payload); err != nil || payload.URL == "" {
		return ""
	}
	return payload.URL
}

// ReadURLToolSpec returns the tool spec for reading web pages.
func ReadURLToolSpec() ToolSpec {
	return ToolSpec{
		Name:        ReadURLToolName,
		Description: "Fetch and read a web page. Returns the page content. Use this to read full content from URLs found in search results.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"description": "The URL to fetch and read",
				},
			},
			"required":             []string{"url"},
			"additionalProperties": false,
		},
	}
}

func (t *ReadURLTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return ToolOutput{}, fmt.Errorf("parse read_url args: %w", err)
	}

	if payload.URL == "" {
		return ToolOutput{}, fmt.Errorf("url is required")
	}

	target, err := resolveReadURLTarget(ctx, t.client, payload.URL)
	if err != nil {
		return ToolOutput{}, err
	}

	fetchClient := newReadURLRedirectClient(t.client, target.ips)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.url, nil)
	if err != nil {
		return ToolOutput{}, fmt.Errorf("create request: %w", err)
	}

	resp, err := fetchClient.Do(req)
	if err != nil {
		return TextOutput(fmt.Sprintf("Error fetching URL: %v", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Return HTTP errors as content so LLM can handle gracefully
		statusText := http.StatusText(resp.StatusCode)
		if statusText == "" {
			statusText = "Unknown"
		}
		return TextOutput(fmt.Sprintf("Error: HTTP %d %s - Unable to fetch this URL.", resp.StatusCode, statusText)), nil
	}

	content, truncated, err := readURLContent(resp.Body)
	if err != nil {
		return TextOutput(fmt.Sprintf("Error reading response: %v", err)), nil
	}

	if truncated {
		content += readURLTruncationSuffix
	}

	return TextOutput(content), nil
}

func readURLContent(r io.Reader) (string, bool, error) {
	reader := bufio.NewReader(r)
	var content strings.Builder

	for i := 0; i < maxReadURLChars; i++ {
		r, _, err := reader.ReadRune()
		if err != nil {
			if err == io.EOF {
				return content.String(), false, nil
			}
			return "", false, err
		}
		content.WriteRune(r)
	}

	_, _, err := reader.ReadRune()
	if err != nil {
		if err == io.EOF {
			return content.String(), false, nil
		}
		return "", false, err
	}

	return content.String(), true, nil
}

func resolveReadURLTarget(ctx context.Context, client *http.Client, rawURL string) (readURLTarget, error) {
	target, err := normalizeReadURLTarget(ctx, rawURL)
	if err != nil {
		return readURLTarget{}, err
	}

	for range maxReadURLRedirects {
		redirectClient := newReadURLRedirectClient(client, target.ips)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.url, nil)
		if err != nil {
			return readURLTarget{}, fmt.Errorf("create redirect check request: %w", err)
		}

		resp, err := redirectClient.Do(req)
		if err != nil {
			return readURLTarget{}, fmt.Errorf("check url redirects: %w", err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return target, nil
		}

		location := resp.Header.Get("Location")
		if location == "" {
			return readURLTarget{}, fmt.Errorf("redirect response missing location header")
		}

		nextURL, err := req.URL.Parse(location)
		if err != nil {
			return readURLTarget{}, fmt.Errorf("parse redirect location: %w", err)
		}

		target, err = normalizeReadURLTarget(ctx, nextURL.String())
		if err != nil {
			return readURLTarget{}, err
		}
	}

	return readURLTarget{}, fmt.Errorf("too many redirects")
}

func newReadURLRedirectClient(client *http.Client, ips []net.IP) *http.Client {
	redirectClient := *client
	redirectClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	transport := cloneReadURLTransport(client.Transport)
	if transport == nil {
		return &redirectClient
	}

	dialContext := readURLDialContext
	if transport.DialContext != nil {
		dialContext = transport.DialContext
	}
	transport.DialTLSContext = nil
	transport.DialTLS = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		var lastErr error
		for _, ip := range ips {
			conn, err := dialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("no resolved IPs available")
	}
	redirectClient.Transport = transport

	return &redirectClient
}

func cloneReadURLTransport(rt http.RoundTripper) *http.Transport {
	if rt == nil {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil
		}
		return transport.Clone()
	}

	transport, ok := rt.(*http.Transport)
	if !ok {
		return nil
	}
	return transport.Clone()
}

func normalizeReadURLTarget(ctx context.Context, rawURL string) (readURLTarget, error) {
	targetURL := rawURL
	if !strings.HasPrefix(strings.ToLower(targetURL), "http://") && !strings.HasPrefix(strings.ToLower(targetURL), "https://") {
		targetURL = "https://" + targetURL
	}

	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return readURLTarget{}, fmt.Errorf("invalid url: %w", err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return readURLTarget{}, fmt.Errorf("url scheme must be http or https")
	}

	host := strings.TrimSuffix(strings.ToLower(parsedURL.Hostname()), ".")
	if host == "" {
		return readURLTarget{}, fmt.Errorf("url host is required")
	}
	if isBlockedReadURLHost(host) {
		return readURLTarget{}, fmt.Errorf("url host is not allowed")
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedReadURLIP(ip) {
			return readURLTarget{}, fmt.Errorf("url host is not allowed")
		}
		return readURLTarget{url: targetURL, ips: []net.IP{ip}}, nil
	}

	ips, err := readURLLookupIP(ctx, host)
	if err != nil {
		return readURLTarget{}, fmt.Errorf("resolve url host: %w", err)
	}
	if len(ips) == 0 {
		return readURLTarget{}, fmt.Errorf("resolve url host: no IP addresses found")
	}
	for _, ip := range ips {
		if isBlockedReadURLIP(ip) {
			return readURLTarget{}, fmt.Errorf("url host is not allowed")
		}
	}

	return readURLTarget{url: targetURL, ips: ips}, nil
}

func isBlockedReadURLHost(host string) bool {
	switch host {
	case "localhost", "metadata.google.internal", "metadata.goog":
		return true
	}

	return strings.HasSuffix(host, ".localhost")
}

func isBlockedReadURLIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}
