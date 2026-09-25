package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	APIKey         string
	BaseURL        string
	Timeout        time.Duration
	SupportsImages bool
}

// Client is a native HTTP client for the TypeSafe System One API.
type Client struct {
	apiKey         string
	baseURL        *url.URL
	http           *http.Client
	timeout        time.Duration
	supportsImages bool
}

// Request is the /v1/systemone classification request.
type Request struct {
	State     json.RawMessage     `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
	Images    []Image             `json:"images,omitempty"`
	Samples   int                 `json:"samples,omitempty"`
}

// Image carries an inline image to a SystemOne-compatible endpoint.
// Image preprocessing and model-specific limits belong to the server.
type Image struct {
	ContentType string `json:"content_type"`
	Base64      string `json:"base64"`
}

// Question describes one typed TypeSafe question.
type Question struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// Response is the /v1/systemone response.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Raw     json.RawMessage   `json:"-"`
}

// Usage contains token usage reported by TypeSafe.
type Usage struct {
	InputTokens  *int `json:"input_tokens,omitempty"`
	OutputTokens *int `json:"output_tokens,omitempty"`
}

// Answer is a TypeSafe primitive answer. Fields are populated according to Type.
type Answer struct {
	Type          string                     `json:"type"`
	Choice        *string                    `json:"choice,omitempty"`
	Score         *float64                   `json:"score,omitempty"`
	Noul          *float64                   `json:"noul,omitempty"`
	Confidence    *float64                   `json:"confidence,omitempty"`
	Probabilities map[string]float64         `json:"probabilities,omitempty"`
	Legend        map[string]json.RawMessage `json:"legend,omitempty"`
}

// ModelsResponse is the /v1/models response.
type ModelsResponse struct {
	Models []Model         `json:"models"`
	Raw    json.RawMessage `json:"-"`
}

// UnmarshalJSON rejects null numeric probability values.
func (a *Answer) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var decoded Answer
	for _, field := range []struct {
		name   string
		target any
	}{
		{"type", &decoded.Type},
		{"choice", &decoded.Choice},
		{"score", &decoded.Score},
		{"noul", &decoded.Noul},
		{"confidence", &decoded.Confidence},
		{"legend", &decoded.Legend},
	} {
		if value, ok := raw[field.name]; ok {
			if err := json.Unmarshal(value, field.target); err != nil {
				return err
			}
		}
	}
	if value, ok := raw["probabilities"]; ok {
		if !isJSONNull(value) {
			var err error
			decoded.Probabilities, err = decodeProbabilityMap(value)
			if err != nil {
				return err
			}
		}
	}
	*a = decoded
	return nil
}

// Model describes an available TypeSafe model.
type Model struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReleaseDate string `json:"release_date"`
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
		return nil, errors.New("typesafe: API key is required; set TYPESAFE_API_KEY or classify.providers.<name>.api_key")
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
		timeout:        timeout,
		supportsImages: opts.SupportsImages,
	}, nil
}

// Validate checks the request before it is sent to TypeSafe.
func (r Request) Validate() error {
	if len(r.State) == 0 {
		return errors.New("typesafe: state is required")
	}
	if err := validateJSON("state", r.State); err != nil {
		return err
	}
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("typesafe: model is required")
	}
	if len(r.Questions) == 0 {
		return errors.New("typesafe: at least one question is required")
	}
	for id, q := range r.Questions {
		if strings.TrimSpace(id) == "" {
			return errors.New("typesafe: question id must not be empty")
		}
		if err := q.validate(id); err != nil {
			return err
		}
	}
	return nil
}

func (q Question) validate(id string) error {
	prefix := fmt.Sprintf("typesafe: question %q", id)
	if q.Type != "noul" && q.Type != "choice" && q.Type != "score" {
		return fmt.Errorf("%s has unsupported type %q", prefix, q.Type)
	}
	if len(q.Instructions) == 0 {
		return fmt.Errorf("%s instructions are required", prefix)
	}
	if err := validateJSON(prefix+" instructions", q.Instructions); err != nil {
		return err
	}
	if !validEntryType(q.Instructions) {
		return fmt.Errorf("%s instructions must be a JSON string, object, array, or null", prefix)
	}
	if len(q.Criteria) > 0 {
		if err := validateJSON(prefix+" criteria", q.Criteria); err != nil {
			return err
		}
	}
	switch q.Type {
	case "noul":
		return q.validateNoul(prefix)
	case "choice":
		return q.validateChoice(prefix)
	case "score":
		return q.validateScore(prefix)
	}
	return nil
}

func (q Question) validateNoul(prefix string) error {
	if len(q.Criteria) > 0 && !isJSONNull(q.Criteria) {
		var criteria map[string]json.RawMessage
		if err := json.Unmarshal(q.Criteria, &criteria); err != nil {
			return fmt.Errorf("%s criteria must be an object or null", prefix)
		}
		for key, value := range criteria {
			if key != "true" && key != "false" {
				return fmt.Errorf("%s criteria may only contain true and false", prefix)
			}
			if !validEntryType(value) {
				return fmt.Errorf("%s criteria.%s must be a JSON string, object, array, or null", prefix, key)
			}
		}
	}
	return nil
}

func (q Question) validateChoice(prefix string) error {
	if len(q.Criteria) == 0 || isJSONNull(q.Criteria) {
		return fmt.Errorf("%s criteria are required", prefix)
	}
	var criteria map[string]json.RawMessage
	if err := json.Unmarshal(q.Criteria, &criteria); err != nil {
		return fmt.Errorf("%s criteria must be an object", prefix)
	}
	if len(criteria) == 0 {
		return fmt.Errorf("%s criteria must include at least one choice", prefix)
	}
	for key, value := range criteria {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s criteria choice id must not be empty", prefix)
		}
		if !validEntryType(value) {
			return fmt.Errorf("%s criteria.%s must be a JSON string, object, array, or null", prefix, key)
		}
	}
	return nil
}

func (q Question) validateScore(prefix string) error {
	if len(q.Criteria) == 0 || isJSONNull(q.Criteria) {
		return fmt.Errorf("%s criteria are required", prefix)
	}
	var levels []json.RawMessage
	if err := json.Unmarshal(q.Criteria, &levels); err != nil {
		return fmt.Errorf("%s criteria must be an array", prefix)
	}
	if len(levels) < 2 {
		return fmt.Errorf("%s criteria must include at least two levels", prefix)
	}
	for i, level := range levels {
		if !validEntryType(level) {
			return fmt.Errorf("%s criteria level %d must be a JSON string, object, array, or null", prefix, i)
		}
	}
	return nil
}

// Classify evaluates a state against typed questions.
func (c *Client) Classify(ctx context.Context, req Request) (*Response, error) {
	if c == nil {
		return nil, errors.New("typesafe: nil client")
	}
	if len(req.Images) > 0 && !c.supportsImages {
		return nil, errors.New("typesafe: selected provider does not support images; configure supports_images only for a compatible endpoint")
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
	var resp Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("typesafe: decode classify response: %w", err)
	}
	resp.Raw = append(json.RawMessage(nil), respBody...)
	if err := resp.validate(req); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListModels lists models available to the account.
func (c *Client) ListModels(ctx context.Context) (*ModelsResponse, error) {
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

func (r Response) validate(req Request) error {
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("typesafe: classify response missing model")
	}
	if len(r.Answers) == 0 {
		return errors.New("typesafe: classify response missing answers")
	}
	for id, question := range req.Questions {
		answer, ok := r.Answers[id]
		if !ok {
			return fmt.Errorf("typesafe: classify response missing answer for question %q", id)
		}
		if answer.Type != question.Type {
			return fmt.Errorf("typesafe: answer %q type %q does not match question type %q", id, answer.Type, question.Type)
		}
		if err := answer.validate(id); err != nil {
			return err
		}
	}
	return nil
}

func (a Answer) validate(id string) error {
	prefix := fmt.Sprintf("typesafe: answer %q", id)
	if err := a.validatePrimary(prefix); err != nil {
		return err
	}
	if a.Confidence != nil && (*a.Confidence < 0 || *a.Confidence > 1 || math.IsNaN(*a.Confidence)) {
		return fmt.Errorf("%s confidence must be between 0 and 1", prefix)
	}
	for key, probability := range a.Probabilities {
		if probability < 0 || probability > 1 || math.IsNaN(probability) {
			return fmt.Errorf("%s probability %q must be between 0 and 1", prefix, key)
		}
	}
	return nil
}

func (a Answer) validatePrimary(prefix string) error {
	switch a.Type {
	case "noul":
		return a.validateNoul(prefix)
	case "choice":
		return a.validateChoice(prefix)
	case "score":
		return a.validateScore(prefix)
	default:
		return fmt.Errorf("%s has unsupported type %q", prefix, a.Type)
	}
}

func (a Answer) validateNoul(prefix string) error {
	if a.Noul == nil {
		return fmt.Errorf("%s missing noul", prefix)
	}
	if *a.Noul < 0 || *a.Noul > 1 || math.IsNaN(*a.Noul) {
		return fmt.Errorf("%s noul must be between 0 and 1", prefix)
	}
	return nil
}

func (a Answer) validateChoice(prefix string) error {
	if a.Choice == nil {
		return fmt.Errorf("%s missing choice", prefix)
	}
	return nil
}

func (a Answer) validateScore(prefix string) error {
	if a.Score == nil {
		return fmt.Errorf("%s missing score", prefix)
	}
	// Scores are level indexes, so the range depends on the question. A
	// non-finite score is never usable and would otherwise print as NaN/+Inf.
	if math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) {
		return fmt.Errorf("%s score must be a finite number", prefix)
	}
	return nil
}

func decodeModels(body []byte) (*ModelsResponse, error) {
	var object ModelsResponse
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, fmt.Errorf("typesafe: decode models response: %w", err)
	}
	if err := validateModels(object.Models); err != nil {
		return nil, err
	}
	return &object, nil
}

func validateModels(models []Model) error {
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

func validateJSON(name string, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("typesafe: %s must be valid JSON", name)
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func validEntryType(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '"', '{', '[':
		return true
	case 'n':
		return string(trimmed) == "null"
	default:
		return false
	}
}

func decodeProbabilityMap(raw json.RawMessage) (map[string]float64, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(values))
	for key, value := range values {
		if isJSONNull(value) {
			return nil, fmt.Errorf("probability %q must be a number", key)
		}
		var probability float64
		if err := json.Unmarshal(value, &probability); err != nil {
			return nil, err
		}
		out[key] = probability
	}
	return out, nil
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
	values := requestSensitiveValues(requestBody)
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	values = append(values, apiKey)
	// Match whole state before its substrings, and never re-redact replacement text.
	sort.SliceStable(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var replacements []string
	for _, value := range values {
		if value != "" {
			replacements = append(replacements, value, "[redacted]")
		}
	}
	return strings.NewReplacer(replacements...).Replace(msg)
}

func requestSensitiveValues(body []byte) []string {
	var values []string
	var req Request
	if len(body) > 0 && json.Unmarshal(body, &req) == nil {
		for _, image := range req.Images {
			values = append(values, image.Base64)
		}
		state := bytes.TrimSpace(req.State)
		if len(state) > 0 {
			values = append(values, string(state))
			var text string
			if json.Unmarshal(state, &text) == nil {
				values = append(values, text)
			}
			// Nested JSON error details escape the state a second time.
			encoded, _ := json.Marshal(string(state))
			values = append(values, string(encoded[1:len(encoded)-1]))
			var compact bytes.Buffer
			if json.Compact(&compact, state) == nil {
				values = append(values, compact.String())
				encoded, _ := json.Marshal(compact.String())
				values = append(values, string(encoded[1:len(encoded)-1]))
			}
		}
	}
	return values
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
