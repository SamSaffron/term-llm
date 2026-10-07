// Package typesafe implements the classify.Backend for TypeSafe System One.
package typesafe

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
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultTimeout = 10 * time.Second

	classifyPath = "/v1/systemone"
	modelsPath   = "/v1/models"

	maxBodyBytes = 1 << 20
	maxRetries   = 2
	backoffBase  = 200 * time.Millisecond
	backoffMax   = 2 * time.Second
)

// Options configures a TypeSafe API client.
type Options struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
}

// Client is a native HTTP client for the TypeSafe System One API. It
// implements classify.Backend.
type Client struct {
	apiKey  string
	baseURL *url.URL
	http    *http.Client
	timeout time.Duration
}

// APIError reports a non-successful TypeSafe API response.
type APIError struct {
	*providerhttp.StatusError
	Method  string
	Path    string
	Message string
}

func (e *APIError) Unwrap() error { return e.StatusError }

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("typesafe %s %s failed: %s", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("typesafe %s %s failed: %s: %s", e.Method, e.Path, e.Status, e.Message)
}

// NewClient creates a TypeSafe client.
func NewClient(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("typesafe: API key is required; set TYPESAFE_API_KEY (used only with the default endpoint) or classify.providers.<name>.api_key")
	}
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("typesafe: parse base URL: %w", redactURLError(err))
	}
	if !baseURL.IsAbs() || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, errors.New("typesafe: base URL must be an absolute http or https URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("typesafe: base URL must not include credentials, query, or fragment")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout < 0 {
		return nil, errors.New("typesafe: timeout must not be negative")
	}

	return &Client{
		// Keys routinely arrive from files or command substitution with a
		// trailing newline, which makes the Authorization header invalid.
		apiKey:  strings.TrimSpace(opts.APIKey),
		baseURL: baseURL,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		timeout: timeout,
	}, nil
}

// Classify evaluates a state against typed questions.
func (c *Client) Classify(ctx context.Context, req classify.Request) (*classify.Response, error) {
	if c == nil {
		return nil, errors.New("typesafe: nil client")
	}
	if len(req.Images) > 0 {
		return nil, classify.ErrImagesUnsupported
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: encode request: %w", err)
	}
	respBody, err := c.do(ctx, http.MethodPost, classifyPath, body)
	if err != nil {
		return nil, err
	}
	var resp classify.Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("typesafe: decode classify response: %w", err)
	}
	resp.Raw = append(json.RawMessage(nil), respBody...)
	if err := resp.Validate(req); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListModels lists models available to the account.
func (c *Client) ListModels(ctx context.Context) (*classify.ModelsResponse, error) {
	if c == nil {
		return nil, errors.New("typesafe: nil client")
	}
	respBody, err := c.do(ctx, http.MethodGet, modelsPath, nil)
	if err != nil {
		return nil, err
	}
	models, err := decodeModels(respBody)
	if err != nil {
		return nil, err
	}
	models.Raw = append(json.RawMessage(nil), respBody...)
	return models, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var nextDelay time.Duration
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, nextDelay); err != nil {
				return nil, err
			}
		}
		respBody, retryDelayOverride, err := c.doOnce(ctx, method, path, body)
		if err == nil {
			return respBody, nil
		}
		lastErr = err
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}
		if !isRetryableError(err) || attempt == maxRetries {
			return nil, err
		}
		nextDelay = retryDelay(attempt+1, retryDelayOverride)
		if nextDelay > backoffMax {
			return nil, err
		}
		if deadline, ok := ctx.Deadline(); ok && nextDelay >= time.Until(deadline) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) doOnce(ctx context.Context, method, path string, body []byte) ([]byte, *time.Duration, error) {
	endpoint := c.endpoint(path)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("typesafe: create request: %w", err)
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
		return nil, nil, fmt.Errorf("typesafe %s %s request failed: %w", method, path, redactURLError(err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		defer resp.Body.Close()
		respBody, err := readBounded(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("typesafe %s %s read response: %w", method, path, err)
		}
		return respBody, nil, nil
	}
	respBody := providerhttp.ReadBodyAndClose(resp, maxBodyBytes)
	message := errorMessage(respBody, c.apiKey, body)
	status := providerhttp.NewStatusErrorString("typesafe", resp.StatusCode, resp.Status, resp.Header, message)
	var delay *time.Duration
	if d, ok := status.RetryAfterDelay(); ok {
		delay = &d
	}
	return nil, delay, &APIError{StatusError: status, Method: method, Path: path, Message: message}
}

func (c *Client) endpoint(path string) string {
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	return u.String()
}

func decodeModels(body []byte) (*classify.ModelsResponse, error) {
	var object classify.ModelsResponse
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, fmt.Errorf("typesafe: decode models response: %w", err)
	}
	if err := validateModels(object.Models); err != nil {
		return nil, err
	}
	return &object, nil
}

func validateModels(models []classify.Model) error {
	if len(models) == 0 {
		return errors.New("typesafe: models response missing models")
	}
	for i, model := range models {
		if strings.TrimSpace(model.Name) == "" {
			return fmt.Errorf("typesafe: model %d missing name", i)
		}
	}
	return nil
}

func readBounded(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
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
	d := backoffBase << (attempt - 1)
	if d > backoffMax {
		return backoffMax
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

func errorMessage(body []byte, apiKey string, requestBody []byte) string {
	if len(body) == 0 {
		return ""
	}
	var msg string
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil {
		for _, key := range []string{"message", "error", "detail"} {
			if value, ok := obj[key]; ok {
				if s, ok := value.(string); ok {
					msg = s
					break
				}
				encoded, err := json.Marshal(value)
				if err == nil {
					msg = string(encoded)
					break
				}
			}
		}
	}
	if msg == "" {
		msg = string(body)
	}
	return truncate(redactMessage(msg, apiKey, requestBody))
}

func redactMessage(msg, apiKey string, requestBody []byte) string {
	var req classify.Request
	if len(requestBody) > 0 && json.Unmarshal(requestBody, &req) != nil {
		req.State = nil
	}
	return classify.Redact(msg, req.State, apiKey)
}

func redactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	copy := *urlErr
	copy.URL = "[redacted]"
	return &copy
}

func truncate(s string) string {
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
