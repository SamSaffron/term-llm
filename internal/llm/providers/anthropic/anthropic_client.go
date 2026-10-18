package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/buildinfo"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
	"golang.org/x/net/http/httpguts"
)

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicAPIVersion     = "2023-06-01"
	// anthropicResponseHeaderTimeout bounds the wait for response headers; it
	// does not limit how long a stream may run.
	anthropicResponseHeaderTimeout = 10 * time.Minute
)

// anthropicClient is a minimal Anthropic Messages API client. When bedrock is
// set, requests are re-enveloped, signed, and decoded for AWS Bedrock.
type anthropicClient struct {
	baseURL    *url.URL // always ends with "/"; nil when baseErr is set
	baseErr    error    // invalid configured endpoint, reported on use
	apiKey     string
	headers    http.Header // configured extra headers, sent with every request
	httpClient *http.Client
	bedrock    Transport
}

// anthropicCall carries per-request transport options.
type anthropicCall struct {
	beta    bool        // use the beta endpoint (?beta=true)
	betas   []string    // anthropic-beta header values
	headers http.Header // extra headers (for example gateway attribution)
}

// EventSource yields raw stream events. Next returns io.EOF at a clean end of
// stream.
type EventSource interface {
	Next() (eventType string, data []byte, err error)
	Close() error
}

type anthropicEventSource = EventSource

// Transport replaces the direct Messages API transport (AWS Bedrock). It
// receives the marshalled Messages body plus the per-call options.
type Transport interface {
	StreamMessages(ctx context.Context, req TransportRequest) (EventSource, error)
}

// TransportRequest is one streaming Messages request handed to a Transport.
type TransportRequest struct {
	Model         string
	Body          []byte
	Beta          bool        // use the beta endpoint
	Betas         []string    // anthropic-beta values for the call
	CallHeaders   http.Header // per-call extra headers
	ClientHeaders http.Header // headers configured on the client
	HTTPClient    *http.Client
}

// APIVersion is the anthropic-version header value sent with every request.
const APIVersion = anthropicAPIVersion

// NewHTTPClient returns the HTTP client used for Anthropic traffic.
func NewHTTPClient() *http.Client { return newAnthropicHTTPClient() }

// RefuseRedirect is the CheckRedirect policy for Anthropic traffic.
func RefuseRedirect(req *http.Request, via []*http.Request) error {
	return refuseAnthropicRedirect(req, via)
}

// ApplyCallHeaders adds betas and replaces headers on h the way the direct
// transport does.
func ApplyCallHeaders(h http.Header, betas []string, headers http.Header) {
	applyAnthropicCallHeaders(h, anthropicCall{betas: betas, headers: headers})
}

// SSEResponse validates resp and wraps its body in an SSE event source;
// non-2xx statuses become typed HTTP status errors.
func SSEResponse(resp *http.Response, provider string) (EventSource, error) {
	return anthropicSSEResponse(resp, provider)
}

func newAnthropicHTTPClient() *http.Client {
	client := &http.Client{Transport: http.DefaultTransport, CheckRedirect: refuseAnthropicRedirect}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t = t.Clone()
		t.ResponseHeaderTimeout = anthropicResponseHeaderTimeout
		client.Transport = t
	}
	return client
}

// refuseAnthropicRedirect stops redirects: Go forwards custom credential
// headers such as X-Api-Key across hosts, and the Messages API never
// redirects. The 3xx surfaces as an HTTPStatusError instead.
func refuseAnthropicRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func parseAnthropicBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("must include scheme and host")
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
		if u.RawPath != "" {
			u.RawPath += "/"
		}
	}
	return u, nil
}

// newAnthropicClient builds a direct API client. An empty baseURL falls back to
// ANTHROPIC_BASE_URL and then the public endpoint, matching the official SDK.
func newAnthropicClient(apiKey, baseURL string, httpClient *http.Client) (*anthropicClient, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = anthropicDefaultBaseURL
	}
	u, err := parseAnthropicBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Anthropic base_url %q: %w", baseURL, err)
	}
	if httpClient == nil {
		httpClient = newAnthropicHTTPClient()
	}
	return &anthropicClient{baseURL: u, apiKey: apiKey, httpClient: httpClient}, nil
}

func (c *anthropicClient) providerName() string {
	if c.bedrock != nil {
		return "bedrock"
	}
	return "anthropic"
}

func (c *anthropicClient) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	if c.baseURL == nil {
		if c.baseErr != nil {
			return nil, c.baseErr
		}
		return nil, errors.New("anthropic client has no base URL")
	}
	ref, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL.ResolveReference(ref).String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Version", anthropicAPIVersion)
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	applyAnthropicCallHeaders(req.Header, anthropicCall{headers: c.headers})
	return req, nil
}

// newAnthropicExtraHeaders validates configured extra headers. Values are never
// included in errors because they may carry credentials.
func newAnthropicExtraHeaders(raw map[string]string) (http.Header, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	headers := make(http.Header, len(raw))
	for name, value := range raw {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !httpguts.ValidHeaderFieldName(name) {
			return nil, fmt.Errorf("invalid Anthropic header name %q", name)
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("invalid value for Anthropic header %q", name)
		}
		if value == "" {
			continue
		}
		headers.Set(name, value)
	}
	return headers, nil
}

// streamMessages sends a streaming Messages request and returns its events.
func (c *anthropicClient) streamMessages(ctx context.Context, params *anthropicMessagesRequest, call anthropicCall) (anthropicEventSource, error) {
	params.Stream = true
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal Anthropic request: %w", err)
	}
	if c.bedrock != nil {
		return c.bedrock.StreamMessages(ctx, TransportRequest{
			Model: params.Model, Body: body, Beta: call.beta, Betas: call.betas,
			CallHeaders: call.headers, ClientHeaders: c.headers, HTTPClient: c.httpClient,
		})
	}
	path := "v1/messages"
	if call.beta {
		path += "?beta=true"
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	applyAnthropicCallHeaders(req.Header, call)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return anthropicSSEResponse(resp, c.providerName())
}

func applyAnthropicCallHeaders(h http.Header, call anthropicCall) {
	for _, beta := range call.betas {
		h.Add("Anthropic-Beta", beta)
	}
	for key, values := range call.headers {
		h.Del(key)
		for _, v := range values {
			h.Add(key, v)
		}
	}
}

// anthropicSSEResponse validates the HTTP status and wraps the body in an SSE
// event source. Non-2xx responses become typed status errors so the retry
// wrapper can honor Retry-After.
func anthropicSSEResponse(resp *http.Response, provider string) (anthropicEventSource, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newHTTPStatusError(provider, resp, readAnthropicErrorBody(resp))
	}
	return &anthropicSSESource{body: resp.Body, decoder: stream.NewDecoder(resp.Body, stream.DecoderOptions{})}, nil
}

func readAnthropicErrorBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body
}

type anthropicSSESource struct {
	body    io.ReadCloser
	decoder *stream.Decoder
}

func (s *anthropicSSESource) Next() (string, []byte, error) {
	return s.decoder.Next()
}

func (s *anthropicSSESource) Close() error {
	return s.body.Close()
}

// anthropicSupport is the {"supported": bool} capability shape.
type anthropicSupport struct {
	Supported bool `json:"supported"`
}

// anthropicModelRecord is one entry from GET /v1/models. Capability maps keep
// unknown future levels/types instead of hardcoding them.
type anthropicModelRecord struct {
	ID             string    `json:"id"`
	DisplayName    string    `json:"display_name"`
	CreatedAt      time.Time `json:"created_at"`
	MaxInputTokens int       `json:"max_input_tokens"`
	MaxTokens      int       `json:"max_tokens"`
	Lifecycle      string    `json:"lifecycle"`
	Capabilities   struct {
		Effort   map[string]json.RawMessage `json:"effort"`
		Thinking struct {
			Supported bool                        `json:"supported"`
			Types     map[string]anthropicSupport `json:"types"`
		} `json:"thinking"`
	} `json:"capabilities"`
}

type anthropicModelList struct {
	Data    []anthropicModelRecord `json:"data"`
	HasMore bool                   `json:"has_more"`
	LastID  string                 `json:"last_id"`
}

// anthropicModelListMaxPages bounds pagination against a misbehaving server.
const anthropicModelListMaxPages = 20

// anthropicModelListMaxBytes bounds one catalog page from an untrusted gateway.
const anthropicModelListMaxBytes = 16 << 20

func (c *anthropicClient) listModels(ctx context.Context) ([]anthropicModelRecord, error) {
	var all []anthropicModelRecord
	after := ""
	complete := false
	for page := 0; page < anthropicModelListMaxPages; page++ {
		path := "v1/models?limit=1000"
		if after != "" {
			path += "&after_id=" + url.QueryEscape(after)
		}
		list, err := c.listModelsPage(ctx, path)
		if err != nil {
			return nil, err
		}
		all = append(all, list.Data...)
		if !list.HasMore {
			complete = true
			break
		}
		if list.LastID == "" || list.LastID == after {
			// A partial catalog would silently drop models.
			return nil, errors.New("anthropic model list: has_more without a new last_id")
		}
		after = list.LastID
	}
	if !complete {
		return nil, fmt.Errorf("anthropic model list: more than %d pages", anthropicModelListMaxPages)
	}
	return all, nil
}

func (c *anthropicClient) listModelsPage(ctx context.Context, path string) (*anthropicModelList, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newHTTPStatusError(c.providerName(), resp, readAnthropicErrorBody(resp))
	}
	defer resp.Body.Close()
	var list anthropicModelList
	if err := json.NewDecoder(io.LimitReader(resp.Body, anthropicModelListMaxBytes)).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode Anthropic model list: %w", err)
	}
	return &list, nil
}

// usesPublicAPI reports whether the client talks to Anthropic's public API,
// whose catalog is shared across accounts and safe to cache globally.
func (c *anthropicClient) usesPublicAPI() bool {
	u := c.baseURL
	return c.bedrock == nil && u != nil && u.Scheme == "https" && u.User == nil && strings.EqualFold(u.Hostname(), "api.anthropic.com") &&
		(u.Port() == "" || u.Port() == "443") && u.Path == "/" && u.RawQuery == ""
}

// anthropicStreamError is an error delivered inside the event stream (an SSE
// "error" event or a Bedrock exception frame).
type anthropicStreamError struct {
	Type    string
	Message string
}

func (e *anthropicStreamError) Error() string {
	return fmt.Sprintf("received error while streaming: %s: %s", e.Type, e.Message)
}

// anthropicStreamStatusError attaches the HTTP status equivalent of a known
// in-stream error type so retry classification matches the HTTP error path.
type anthropicStreamStatusError struct {
	*anthropicStreamError
	status int
}

func (e *anthropicStreamStatusError) HTTPStatusCode() int { return e.status }

func (e *anthropicStreamStatusError) Unwrap() error { return e.anthropicStreamError }

var anthropicStreamErrorStatus = map[string]int{
	"rate_limit_error":            http.StatusTooManyRequests,
	"overloaded_error":            529,
	"api_error":                   http.StatusInternalServerError,
	"throttlingException":         http.StatusTooManyRequests,
	"internalServerException":     http.StatusInternalServerError,
	"serviceUnavailableException": http.StatusServiceUnavailable,
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func newAnthropicStreamError(errType, message string) error {
	if errType == "" {
		errType = "UnknownError"
	}
	if message == "" {
		message = errType
	}
	base := &anthropicStreamError{Type: errType, Message: message}
	// Bedrock reports exception types in either camelCase (event stream) or
	// PascalCase (__type), so the lookup ignores the first letter's case.
	if status, ok := anthropicStreamErrorStatus[lowerFirst(errType)]; ok {
		return &anthropicStreamStatusError{anthropicStreamError: base, status: status}
	}
	return base
}
