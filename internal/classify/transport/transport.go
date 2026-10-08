// Package transport is the HTTP client shared by classification backends:
// bearer authentication, a single overall timeout, bounded retries for
// transient statuses, bounded response bodies, no redirects, and error
// messages that never contain the API key or request state.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	MaxBodyBytes = 1 << 20
	MaxRetries   = 2
	BackoffBase  = 200 * time.Millisecond
	BackoffMax   = 2 * time.Second
)

// Config configures a Client.
type Config struct {
	// Provider names the backend in errors, e.g. "typesafe".
	Provider       string
	APIKey         string
	BaseURL        string
	Timeout        time.Duration
	DefaultBaseURL string
	DefaultTimeout time.Duration
	// MissingKeyHint completes "API key is required; ...".
	MissingKeyHint string
	// ErrorMessage extracts a human message from an error response body.
	// The result is redacted and truncated by the transport. Nil uses the
	// raw body.
	ErrorMessage func(body []byte) string
}

// Client sends JSON requests to one classification API.
type Client struct {
	provider     string
	apiKey       string
	baseURL      *url.URL
	http         *http.Client
	timeout      time.Duration
	errorMessage func([]byte) string
}

// APIError reports a non-successful API response.
type APIError struct {
	*providerhttp.StatusError
	Provider string
	Method   string
	Path     string
	Message  string
}

func (e *APIError) Unwrap() error { return e.StatusError }

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s %s failed: %s", e.Provider, e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("%s %s %s failed: %s: %s", e.Provider, e.Method, e.Path, e.Status, e.Message)
}

// New validates cfg and creates a Client.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("%s: API key is required; %s", cfg.Provider, cfg.MissingKeyHint)
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = cfg.DefaultBaseURL
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("%s: parse base URL: %w", cfg.Provider, RedactURLError(err))
	}
	if !baseURL.IsAbs() || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, fmt.Errorf("%s: base URL must be an absolute http or https URL", cfg.Provider)
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, fmt.Errorf("%s: base URL must not include credentials, query, or fragment", cfg.Provider)
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = cfg.DefaultTimeout
	}
	if timeout < 0 {
		return nil, fmt.Errorf("%s: timeout must not be negative", cfg.Provider)
	}
	return &Client{
		provider: cfg.Provider,
		// Keys routinely arrive from files or command substitution with a
		// trailing newline, which makes the Authorization header invalid.
		apiKey:  strings.TrimSpace(cfg.APIKey),
		baseURL: baseURL,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		timeout:      timeout,
		errorMessage: cfg.ErrorMessage,
	}, nil
}

// Do sends body (nil for none) to path under the base URL and returns the
// successful response body. state is redacted from error messages. The
// timeout covers the whole operation, including retries.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, state json.RawMessage) ([]byte, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var nextDelay time.Duration
	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, nextDelay); err != nil {
				return nil, err
			}
		}
		respBody, retryDelayOverride, err := c.doOnce(ctx, method, path, body, state)
		if err == nil {
			return respBody, nil
		}
		lastErr = err
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}
		if !isRetryableError(err) || attempt == MaxRetries {
			return nil, err
		}
		nextDelay = retryDelay(attempt+1, retryDelayOverride)
		if nextDelay > BackoffMax {
			return nil, err
		}
		if deadline, ok := ctx.Deadline(); ok && nextDelay >= time.Until(deadline) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) doOnce(ctx context.Context, method, path string, body []byte, state json.RawMessage) ([]byte, *time.Duration, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), reader)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: create request: %w", c.provider, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%s %s %s request failed: %w", c.provider, method, path, RedactURLError(err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		defer resp.Body.Close()
		respBody, err := readBounded(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("%s %s %s read response: %w", c.provider, method, path, err)
		}
		return respBody, nil, nil
	}
	respBody := providerhttp.ReadBodyAndClose(resp, MaxBodyBytes)
	message := c.message(respBody, state)
	status := providerhttp.NewStatusErrorString(c.provider, resp.StatusCode, resp.Status, resp.Header, message)
	var delay *time.Duration
	if d, ok := status.RetryAfterDelay(); ok {
		delay = &d
	}
	return nil, delay, &APIError{StatusError: status, Provider: c.provider, Method: method, Path: path, Message: message}
}

func (c *Client) message(body []byte, state json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}
	msg := ""
	if c.errorMessage != nil {
		msg = c.errorMessage(body)
	}
	if msg == "" {
		msg = string(body)
	}
	return Truncate(classify.Redact(msg, state, c.apiKey))
}

// Redact redacts state and the client's API key from msg and bounds it.
func (c *Client) Redact(msg string, state json.RawMessage) string {
	return Truncate(classify.Redact(msg, state, c.apiKey))
}

// BaseURL returns the normalized base URL.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// Timeout returns the overall operation timeout.
func (c *Client) Timeout() time.Duration { return c.timeout }

func (c *Client) endpoint(path string) string {
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	return u.String()
}

func readBounded(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, errors.New("response body too large")
	}
	return body, nil
}

func isRetryableError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && providerhttp.RetryableStatus(apiErr.StatusCode)
}

func retryDelay(attempt int, override *time.Duration) time.Duration {
	if override != nil {
		return *override
	}
	d := BackoffBase << (attempt - 1)
	if d > BackoffMax {
		return BackoffMax
	}
	return d
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RedactURLError hides the request URL, which may embed account ids or
// gateway names, from transport errors.
func RedactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	copy := *urlErr
	copy.URL = "[redacted]"
	return &copy
}

// Truncate bounds an error message to 512 bytes on a UTF-8 boundary.
func Truncate(s string) string {
	s = strings.TrimSpace(s)
	const max = 512
	if len(s) <= max {
		return s
	}
	end := max
	for !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "..."
}
