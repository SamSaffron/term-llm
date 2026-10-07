// Package classify defines the backend-neutral classification interface used
// by `term-llm classify`, the classify-backed Guardian, and the live voice
// router.
//
// The vocabulary follows TypeSafe System One, the first supported backend:
// a request carries a JSON state and a map of typed questions (choice, score,
// noul), and the response maps each question id to a typed answer. Backends
// such as internal/classify/typesafe and internal/classify/openai translate
// this shape to their own wire formats.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Question types.
const (
	TypeChoice = "choice"
	TypeScore  = "score"
	TypeNoul   = "noul"
)

// ErrImagesUnsupported reports image inputs sent to a backend that cannot
// evaluate them.
var ErrImagesUnsupported = errors.New("classify: image inputs are not supported by this provider; use a classify provider with type openai")

// Backend evaluates classification requests.
type Backend interface {
	Classify(context.Context, Request) (*Response, error)
	ListModels(context.Context) (*ModelsResponse, error)
}

// Request is a classification request.
type Request struct {
	State     json.RawMessage     `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
	// Images holds base64 data:image URLs evaluated alongside the state. Only
	// image-capable backends accept them; others return ErrImagesUnsupported.
	Images []string `json:"-"`
}

// Question describes one typed question. Criteria are a true/false object
// for noul, an option map for choice, and an ordered level array for score.
type Question struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// Response maps question ids to typed answers.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	// Raw is the backend response for --format json. Backends whose wire
	// format differs from this shape store the normalized JSON instead.
	Raw json.RawMessage `json:"-"`
}

// Usage contains token usage reported by the backend.
type Usage struct {
	InputTokens  *int `json:"input_tokens,omitempty"`
	OutputTokens *int `json:"output_tokens,omitempty"`
}

// Answer is a typed answer. Fields are populated according to Type: Noul is
// a probability, Choice an option key, and Score a probability-weighted level
// index. Probabilities are keyed by option key (choice) or level index
// (score); Legend maps score level indexes to their criteria.
type Answer struct {
	Type          string                     `json:"type"`
	Choice        *string                    `json:"choice,omitempty"`
	Score         *float64                   `json:"score,omitempty"`
	Noul          *float64                   `json:"noul,omitempty"`
	Confidence    *float64                   `json:"confidence,omitempty"`
	Probabilities map[string]float64         `json:"probabilities,omitempty"`
	Legend        map[string]json.RawMessage `json:"legend,omitempty"`
}

// ModelsResponse lists models available from a backend.
type ModelsResponse struct {
	Models []Model         `json:"models"`
	Raw    json.RawMessage `json:"-"`
}

// Model describes an available model.
type Model struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReleaseDate string `json:"release_date"`
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
	if value, ok := raw["probabilities"]; ok && !IsJSONNull(value) {
		var err error
		decoded.Probabilities, err = decodeProbabilityMap(value)
		if err != nil {
			return err
		}
	}
	*a = decoded
	return nil
}

// Validate checks the request before it is sent. State is required unless
// the request carries images.
func (r Request) Validate() error {
	if len(r.State) == 0 {
		if len(r.Images) == 0 {
			return errors.New("classify: state is required")
		}
	} else if err := validateJSON("state", r.State); err != nil {
		return err
	}
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("classify: model is required")
	}
	if len(r.Questions) == 0 {
		return errors.New("classify: at least one question is required")
	}
	for id, q := range r.Questions {
		if strings.TrimSpace(id) == "" {
			return errors.New("classify: question id must not be empty")
		}
		if err := q.validate(id); err != nil {
			return err
		}
	}
	return nil
}

func (q Question) validate(id string) error {
	prefix := fmt.Sprintf("classify: question %q", id)
	if q.Type != TypeNoul && q.Type != TypeChoice && q.Type != TypeScore {
		return fmt.Errorf("%s has unsupported type %q", prefix, q.Type)
	}
	if len(q.Instructions) == 0 {
		return fmt.Errorf("%s instructions are required", prefix)
	}
	if err := validateJSON(prefix+" instructions", q.Instructions); err != nil {
		return err
	}
	if !ValidEntry(q.Instructions) {
		return fmt.Errorf("%s instructions must be a JSON string, object, array, or null", prefix)
	}
	if len(q.Criteria) > 0 {
		if err := validateJSON(prefix+" criteria", q.Criteria); err != nil {
			return err
		}
	}
	switch q.Type {
	case TypeNoul:
		return q.validateNoul(prefix)
	case TypeChoice:
		return q.validateChoice(prefix)
	default:
		return q.validateScore(prefix)
	}
}

func (q Question) validateNoul(prefix string) error {
	if len(q.Criteria) == 0 || IsJSONNull(q.Criteria) {
		return nil
	}
	var criteria map[string]json.RawMessage
	if err := json.Unmarshal(q.Criteria, &criteria); err != nil {
		return fmt.Errorf("%s criteria must be an object or null", prefix)
	}
	for key, value := range criteria {
		if key != "true" && key != "false" {
			return fmt.Errorf("%s criteria may only contain true and false", prefix)
		}
		if !ValidEntry(value) {
			return fmt.Errorf("%s criteria.%s must be a JSON string, object, array, or null", prefix, key)
		}
	}
	return nil
}

func (q Question) validateChoice(prefix string) error {
	if len(q.Criteria) == 0 || IsJSONNull(q.Criteria) {
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
		if !ValidEntry(value) {
			return fmt.Errorf("%s criteria.%s must be a JSON string, object, array, or null", prefix, key)
		}
	}
	return nil
}

func (q Question) validateScore(prefix string) error {
	if len(q.Criteria) == 0 || IsJSONNull(q.Criteria) {
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
		if !ValidEntry(level) {
			return fmt.Errorf("%s criteria level %d must be a JSON string, object, array, or null", prefix, i)
		}
	}
	return nil
}

// Validate checks that the response answers every requested question with
// a well-formed answer of the requested type.
func (r Response) Validate(req Request) error {
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("classify: response missing model")
	}
	if len(r.Answers) == 0 {
		return errors.New("classify: response missing answers")
	}
	for id, question := range req.Questions {
		answer, ok := r.Answers[id]
		if !ok {
			return fmt.Errorf("classify: response missing answer for question %q", id)
		}
		if answer.Type != question.Type {
			return fmt.Errorf("classify: answer %q type %q does not match question type %q", id, answer.Type, question.Type)
		}
		if err := answer.Validate(id); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the answer's primary value and probability ranges.
func (a Answer) Validate(id string) error {
	prefix := fmt.Sprintf("classify: answer %q", id)
	if err := a.validatePrimary(prefix); err != nil {
		return err
	}
	if a.Confidence != nil && !unitInterval(*a.Confidence) {
		return fmt.Errorf("%s confidence must be between 0 and 1", prefix)
	}
	for key, probability := range a.Probabilities {
		if !unitInterval(probability) {
			return fmt.Errorf("%s probability %q must be between 0 and 1", prefix, key)
		}
	}
	return nil
}

func (a Answer) validatePrimary(prefix string) error {
	switch a.Type {
	case TypeNoul:
		if a.Noul == nil {
			return fmt.Errorf("%s missing noul", prefix)
		}
		if !unitInterval(*a.Noul) {
			return fmt.Errorf("%s noul must be between 0 and 1", prefix)
		}
	case TypeChoice:
		if a.Choice == nil {
			return fmt.Errorf("%s missing choice", prefix)
		}
	case TypeScore:
		if a.Score == nil {
			return fmt.Errorf("%s missing score", prefix)
		}
		// Scores are level indexes, so the range depends on the question. A
		// non-finite score is never usable and would otherwise print as NaN/+Inf.
		if math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) {
			return fmt.Errorf("%s score must be a finite number", prefix)
		}
	default:
		return fmt.Errorf("%s has unsupported type %q", prefix, a.Type)
	}
	return nil
}

func unitInterval(v float64) bool {
	return v >= 0 && v <= 1 && !math.IsNaN(v)
}

func validateJSON(name string, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("classify: %s must be valid JSON", name)
	}
	return nil
}

// IsJSONNull reports whether raw is the JSON null literal.
func IsJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// ValidEntry reports whether raw is a JSON string, object, array, or null,
// the forms accepted for instructions and criteria descriptions.
func ValidEntry(raw json.RawMessage) bool {
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

// EntryText renders an entry (string, object, array, or null) as plain text:
// strings are unquoted, structured values compacted, and null is empty.
func EntryText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || IsJSONNull(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		return compact.String()
	}
	return string(raw)
}

// Redact replaces the request state, in every form a backend error might
// echo it (raw, unquoted text, compact, and JSON-escaped), and any secrets
// with [redacted]. Longer values are matched first so a short state can never
// split a secret, and replacement text is never re-redacted.
func Redact(msg string, state json.RawMessage, secrets ...string) string {
	values := append(stateForms(state), secrets...)
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	sort.SliceStable(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var replacements []string
	for _, value := range values {
		if value != "" {
			replacements = append(replacements, value, "[redacted]")
		}
	}
	if len(replacements) == 0 {
		return msg
	}
	return strings.NewReplacer(replacements...).Replace(msg)
}

func stateForms(state json.RawMessage) []string {
	state = bytes.TrimSpace(state)
	if len(state) == 0 {
		return nil
	}
	escaped := func(s string) string {
		encoded, _ := json.Marshal(s)
		return string(encoded[1 : len(encoded)-1])
	}
	values := []string{string(state), escaped(string(state))}
	var text string
	if json.Unmarshal(state, &text) == nil {
		values = append(values, text)
	}
	var compact bytes.Buffer
	if json.Compact(&compact, state) == nil {
		values = append(values, compact.String(), escaped(compact.String()))
	}
	return values
}

func decodeProbabilityMap(raw json.RawMessage) (map[string]float64, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(values))
	for key, value := range values {
		if IsJSONNull(value) {
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
