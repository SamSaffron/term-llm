// Package openai implements the classify.Backend for the OpenAI Decisions
// API (POST /v1/decisions). Questions translate directly: noul maps to a
// predicate, and choice and score map to their Decisions equivalents.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultModel   = "gpt-6-luna"
	DefaultTimeout = 10 * time.Second

	decisionsPath = "/decisions"

	maxBodyBytes = 1 << 20
	maxRetries   = 2
	backoffBase  = 200 * time.Millisecond
	backoffMax   = 2 * time.Second
)

// Options configures a Decisions API client.
type Options struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
}

// Client calls the OpenAI Decisions API. It implements classify.Backend.
type Client struct {
	apiKey  string
	baseURL *url.URL
	http    *http.Client
	timeout time.Duration
}

// APIError reports a non-successful Decisions API response.
type APIError struct {
	*providerhttp.StatusError
	Message string
}

func (e *APIError) Unwrap() error { return e.StatusError }

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("openai decisions request failed: %s", e.Status)
	}
	return fmt.Sprintf("openai decisions request failed: %s: %s", e.Status, e.Message)
}

// NewClient creates a Decisions API client.
func NewClient(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("openai decisions: API key is required; set OPENAI_API_KEY (used only with the default endpoint) or classify.providers.<name>.api_key")
	}
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("openai decisions: parse base URL: %w", redactURLError(err))
	}
	if !baseURL.IsAbs() || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, errors.New("openai decisions: base URL must be an absolute http or https URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("openai decisions: base URL must not include credentials, query, or fragment")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout < 0 {
		return nil, errors.New("openai decisions: timeout must not be negative")
	}
	return &Client{
		apiKey:  strings.TrimSpace(opts.APIKey),
		baseURL: baseURL,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		timeout: timeout,
	}, nil
}

// ListModels reports the models the Decisions API accepts. The endpoint has
// no model listing of its own, so this is answered locally.
func (c *Client) ListModels(context.Context) (*classify.ModelsResponse, error) {
	return &classify.ModelsResponse{Models: []classify.Model{{
		Name:        DefaultModel,
		Description: "OpenAI Decisions API model",
	}}}, nil
}

// Classify evaluates the state and images against typed questions.
func (c *Client) Classify(ctx context.Context, req classify.Request) (*classify.Response, error) {
	if c == nil {
		return nil, errors.New("openai decisions: nil client")
	}
	wire, err := buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("openai decisions: encode request: %w", err)
	}
	respBody, err := c.do(ctx, body, req.State)
	if err != nil {
		return nil, err
	}
	resp, err := convertResponse(respBody, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// --- request mapping -------------------------------------------------------

type wireRequest struct {
	Model     string         `json:"model"`
	Input     any            `json:"input"`
	Questions []wireQuestion `json:"questions"`
}

type wireMessage struct {
	Role    string     `json:"role"`
	Content []wirePart `json:"content"`
}

type wirePart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type wireQuestion struct {
	Type         string       `json:"type"`
	Name         string       `json:"name"`
	Instructions string       `json:"instructions"`
	Choices      []wireChoice `json:"choices,omitempty"`
	Levels       []wireLevel  `json:"levels,omitempty"`
}

type wireChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type wireLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func buildRequest(req classify.Request) (wireRequest, error) {
	if err := req.Validate(); err != nil {
		return wireRequest{}, err
	}
	for i, image := range req.Images {
		if !strings.HasPrefix(image, "data:image/") || !strings.Contains(image, ";base64,") {
			return wireRequest{}, fmt.Errorf("openai decisions: image %d must be a base64 data:image/... URL", i+1)
		}
	}
	wire := wireRequest{Model: strings.TrimSpace(req.Model)}
	text := stateText(req.State)
	if len(req.Images) == 0 {
		wire.Input = text
	} else {
		msg := wireMessage{Role: "user"}
		if strings.TrimSpace(text) != "" {
			msg.Content = append(msg.Content, wirePart{Type: "input_text", Text: text})
		}
		for _, image := range req.Images {
			msg.Content = append(msg.Content, wirePart{Type: "input_image", ImageURL: image})
		}
		wire.Input = []wireMessage{msg}
	}
	ids := make([]string, 0, len(req.Questions))
	for id := range req.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		q, err := buildQuestion(id, req.Questions[id])
		if err != nil {
			return wireRequest{}, err
		}
		wire.Questions = append(wire.Questions, q)
	}
	return wire, nil
}

func buildQuestion(id string, q classify.Question) (wireQuestion, error) {
	out := wireQuestion{Name: id, Instructions: classify.EntryText(q.Instructions)}
	if strings.TrimSpace(out.Instructions) == "" {
		return out, fmt.Errorf("openai decisions: question %q instructions are required", id)
	}
	switch q.Type {
	case "noul":
		out.Type = "predicate"
		out.Instructions += predicateCriteria(q.Criteria)
	case "choice":
		out.Type = "choice"
		entries, err := orderedObject(q.Criteria)
		if err != nil {
			return out, fmt.Errorf("openai decisions: question %q criteria: %w", id, err)
		}
		for _, e := range entries {
			out.Choices = append(out.Choices, wireChoice{Value: e.key, Description: classify.EntryText(e.value)})
		}
	case "score":
		out.Type = "score"
		var levels []json.RawMessage
		if err := json.Unmarshal(q.Criteria, &levels); err != nil {
			return out, fmt.Errorf("openai decisions: question %q criteria must be an array", id)
		}
		for _, level := range levels {
			wire, err := scoreLevel(level)
			if err != nil {
				return out, fmt.Errorf("openai decisions: question %q level %d %w", id, len(out.Levels), err)
			}
			out.Levels = append(out.Levels, wire)
		}
	}
	return out, nil
}

// predicateCriteria folds TypeSafe's true/false descriptions into the
// instructions, since Decisions predicates have no separate criteria.
func predicateCriteria(raw json.RawMessage) string {
	if len(raw) == 0 || classify.IsJSONNull(raw) {
		return ""
	}
	var criteria map[string]json.RawMessage
	if json.Unmarshal(raw, &criteria) != nil {
		return ""
	}
	var b strings.Builder
	for _, key := range []string{"true", "false"} {
		if text := strings.TrimSpace(classify.EntryText(criteria[key])); text != "" {
			fmt.Fprintf(&b, "\n%s: %s", strings.ToUpper(key[:1])+key[1:], text)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n" + b.String()
}

// scoreLevel accepts a plain label, an object with label and optional
// description (each any entry form), or any other entry used as the label.
func scoreLevel(raw json.RawMessage) (wireLevel, error) {
	var obj map[string]json.RawMessage
	level := wireLevel{Label: classify.EntryText(raw)}
	if json.Unmarshal(raw, &obj) == nil {
		if label, ok := obj["label"]; ok {
			level = wireLevel{Label: classify.EntryText(label), Description: classify.EntryText(obj["description"])}
		}
	}
	if strings.TrimSpace(level.Label) == "" {
		return level, errors.New("must have a non-empty label")
	}
	return level, nil
}

func stateText(state json.RawMessage) string {
	return classify.EntryText(state)
}

type objectEntry struct {
	key   string
	value json.RawMessage
}

// orderedObject decodes a JSON object preserving key order, so choices keep
// the order the user wrote them in.
func orderedObject(raw json.RawMessage) ([]objectEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("must be an object")
	}
	var entries []objectEntry
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		entries = append(entries, objectEntry{key: key, value: value})
	}
	return entries, nil
}

// --- response mapping ------------------------------------------------------

type wireResponse struct {
	Model   string          `json:"model"`
	Answers []wireAnswer    `json:"answers"`
	Usage   json.RawMessage `json:"usage"`
}

type wireAnswer struct {
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	Probability   *float64          `json:"probability"`
	Choice        json.RawMessage   `json:"choice"`
	Score         *float64          `json:"score"`
	Confidence    *float64          `json:"confidence"`
	Probabilities []wireProbability `json:"probabilities"`
	Refusal       string            `json:"refusal"`
}

type wireProbability struct {
	Value       json.RawMessage `json:"value"`
	Label       *string         `json:"label"`
	Probability *float64        `json:"probability"`
}

// convertResponse maps Decisions answers onto classify.Response. Raw holds
// the normalized JSON so `classify --format json` output is the same shape
// for every backend.
func convertResponse(body []byte, req classify.Request) (*classify.Response, error) {
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("openai decisions: decode response: %w", err)
	}
	resp := &classify.Response{Model: wire.Model, Answers: make(map[string]classify.Answer, len(wire.Answers))}
	if strings.TrimSpace(resp.Model) == "" {
		resp.Model = req.Model
	}
	if len(wire.Usage) > 0 && !classify.IsJSONNull(wire.Usage) {
		if err := json.Unmarshal(wire.Usage, &resp.Usage); err != nil {
			return nil, fmt.Errorf("openai decisions: decode usage: %w", err)
		}
	}
	for _, a := range wire.Answers {
		if a.Type == "refusal" {
			msg := strings.TrimSpace(a.Refusal)
			if msg == "" {
				msg = "no reason given"
			}
			return nil, fmt.Errorf("openai decisions: question %q was refused: %s", a.Name, truncate(msg))
		}
		answer, err := convertAnswer(a)
		if err != nil {
			return nil, err
		}
		resp.Answers[a.Name] = answer
	}
	if err := resp.Validate(req); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("openai decisions: encode response: %w", err)
	}
	resp.Raw = raw
	return resp, nil
}

// convertAnswer maps one Decisions answer; classify.Response.Validate checks
// the values afterwards.
func convertAnswer(a wireAnswer) (classify.Answer, error) {
	out := classify.Answer{Confidence: a.Confidence, Score: a.Score}
	switch a.Type {
	case "predicate":
		out.Type, out.Noul = classify.TypeNoul, a.Probability
	case "choice":
		out.Type = classify.TypeChoice
		if len(bytes.TrimSpace(a.Choice)) > 0 && !classify.IsJSONNull(a.Choice) {
			choice := classify.EntryText(a.Choice)
			out.Choice = &choice
		}
	case "score":
		out.Type = classify.TypeScore
	default:
		return out, fmt.Errorf("openai decisions: answer %q has unsupported type %q", a.Name, a.Type)
	}
	return out, convertProbabilities(a, &out)
}

// convertProbabilities keys choice probabilities by option and score
// probabilities by level index, with score labels in the legend.
func convertProbabilities(a wireAnswer, out *classify.Answer) error {
	if len(a.Probabilities) == 0 {
		return nil
	}
	out.Probabilities = make(map[string]float64, len(a.Probabilities))
	for i, p := range a.Probabilities {
		key := classify.EntryText(p.Value)
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("openai decisions: answer %q probability %d is missing its value", a.Name, i)
		}
		if out.Type == classify.TypeScore {
			if p.Label != nil {
				if out.Legend == nil {
					out.Legend = map[string]json.RawMessage{}
				}
				out.Legend[key], _ = json.Marshal(*p.Label)
			}
		}
		if p.Probability == nil {
			return fmt.Errorf("openai decisions: answer %q probability %q must be a number", a.Name, key)
		}
		out.Probabilities[key] = *p.Probability
	}
	return nil
}

// --- transport ---------------------------------------------------------------

func (c *Client) do(ctx context.Context, body []byte, state json.RawMessage) ([]byte, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		respBody, delayOverride, err := c.doOnce(ctx, body, state)
		if err == nil {
			return respBody, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isRetryable(err) || attempt == maxRetries {
			return nil, err
		}
		delay := backoffBase << attempt
		if delay > backoffMax {
			delay = backoffMax
		}
		if delayOverride != nil {
			delay = *delayOverride
		}
		if delay > backoffMax {
			return nil, err
		}
		if deadline, ok := ctx.Deadline(); ok && delay >= time.Until(deadline) {
			return nil, err
		}
		if err := sleepContext(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) doOnce(ctx context.Context, body []byte, state json.RawMessage) ([]byte, *time.Duration, error) {
	u := *c.baseURL
	u.Path = c.baseURL.Path + decisionsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("openai decisions: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("openai decisions request failed: %w", redactURLError(err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		defer resp.Body.Close()
		respBody, err := readBounded(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("openai decisions: read response: %w", err)
		}
		return respBody, nil, nil
	}
	respBody := providerhttp.ReadBodyAndClose(resp, maxBodyBytes)
	message := errorMessage(respBody, c.apiKey, state)
	status := providerhttp.NewStatusErrorString("openai", resp.StatusCode, resp.Status, resp.Header, message)
	var delay *time.Duration
	if d, ok := status.RetryAfterDelay(); ok {
		delay = &d
	}
	return nil, delay, &APIError{StatusError: status, Message: message}
}

func isRetryable(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && providerhttp.RetryableStatus(apiErr.StatusCode)
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

func readBounded(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, errors.New("response body too large")
	}
	return body, nil
}

// errorMessage extracts OpenAI's error.message, redacting the key and state.
func errorMessage(body []byte, apiKey string, state json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}
	msg := string(body)
	var obj struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &obj) == nil && obj.Error.Message != "" {
		msg = obj.Error.Message
	}
	return truncate(classify.Redact(msg, state, apiKey))
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
