package providerhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// JSONRequestOptions configures the shared authenticated JSON request path used
// by simple provider transports.
type JSONRequestOptions struct {
	Client           *http.Client
	Method           string
	URL              string
	APIKey           string
	Payload          any
	Provider         string
	ExpectedStatus   int
	WrapRequestError func(error) error
	Headers          http.Header
	Debug            bool
	RequestLabel     string
	ResponseLabel    string
	Debugf           func(string, string, ...any)
	OnRequest        func([]byte)
	OnResponse       func(status int, contentType string, body []byte)
}

// DoJSONRequest sends an authenticated JSON request and returns the response body and content type.
func DoJSONRequest(ctx context.Context, opts JSONRequestOptions) ([]byte, string, error) {
	jsonBody, err := json.Marshal(opts.Payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal %s request: %w", opts.Provider, err)
	}
	if opts.Debug && opts.Debugf != nil {
		opts.Debugf(opts.RequestLabel, "%s %s\n%s", opts.Method, opts.URL, string(jsonBody))
	}
	if opts.OnRequest != nil {
		opts.OnRequest(jsonBody)
	}
	req, err := http.NewRequestWithContext(ctx, opts.Method, opts.URL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, "", fmt.Errorf("create %s request: %w", opts.Provider, err)
	}
	if opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, values := range opts.Headers {
		req.Header.Del(key)
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if opts.WrapRequestError != nil {
			return nil, "", opts.WrapRequestError(err)
		}
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read %s response: %w", opts.Provider, err)
	}
	contentType := resp.Header.Get("Content-Type")
	if opts.Debug && opts.Debugf != nil {
		opts.Debugf(opts.ResponseLabel, "status=%d content-type=%s body_len=%d", resp.StatusCode, contentType, len(body))
	}
	if opts.OnResponse != nil {
		opts.OnResponse(resp.StatusCode, contentType, body)
	}
	expected := opts.ExpectedStatus
	if expected == 0 {
		expected = http.StatusOK
	}
	if resp.StatusCode != expected {
		return nil, "", NewStatusError(opts.Provider, resp, body)
	}
	return body, contentType, nil
}
