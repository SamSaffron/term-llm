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
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/transport"
)

const (
	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultModel   = "gpt-6-luna"
	DefaultTimeout = 10 * time.Second

	decisionsPath = "/decisions"
)

// Options configures a Decisions API client.
type Options struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
}

// Client calls the OpenAI Decisions API. It implements classify.Backend.
type Client struct {
	http *transport.Client
}

// APIError reports a non-successful Decisions API response.
type APIError = transport.APIError

// NewClient creates a Decisions API client.
func NewClient(opts Options) (*Client, error) {
	http, err := transport.New(transport.Config{
		Provider:       "openai decisions",
		APIKey:         opts.APIKey,
		BaseURL:        opts.BaseURL,
		Timeout:        opts.Timeout,
		DefaultBaseURL: DefaultBaseURL,
		DefaultTimeout: DefaultTimeout,
		MissingKeyHint: "set OPENAI_API_KEY (used only with the default endpoint) or classify.providers.<name>.api_key",
		ErrorMessage:   errorMessage,
	})
	if err != nil {
		return nil, err
	}
	return &Client{http: http}, nil
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
	respBody, err := c.http.Do(ctx, http.MethodPost, decisionsPath, body, req.State)
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
			return nil, fmt.Errorf("openai decisions: question %q was refused: %s", a.Name, transport.Truncate(msg))
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

// errorMessage extracts OpenAI's error.message.
func errorMessage(body []byte) string {
	var obj struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	return obj.Error.Message
}
