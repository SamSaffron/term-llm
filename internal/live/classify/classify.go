package liveclassify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/typesafe"
)

const (
	IntentSteer         = "steer"
	IntentStatus        = "status"
	IntentNewSession    = "new_session"
	IntentSwitchSession = "switch_session"
	IntentSteerNow      = "steer_now"
	IntentSide          = "side"

	IntentQuestionID          = "intent"
	AlsoRequestQuestionID     = "also_request"
	UnambiguousStopQuestionID = "unambiguous_stop"

	IntentQuestionInstructions  = "Classify the primary intent of this spoken live delegation. Choose exactly one label. Prefer steer whenever the request is ordinary work or does not clearly fit a host action."
	AlsoRequestInstructions     = "Does this utterance both ask to switch/start a session and ask for work to be done there? Return the probability that it contains an additional work request beyond navigation."
	UnambiguousStopInstructions = "Is this an unambiguous request to stop or cancel the currently running task now?"

	// MaxMessageRunes keeps pathological recognition tokens and skipped long
	// requests from producing unbounded diagnostic rows.
	MaxMessageRunes = 4096
	// MaxTranscriptRunes keeps the recent spoken context useful without allowing a
	// long call to make every classification request grow without bound.
	MaxTranscriptRunes = 4096
	// MaxRecentSessionTitles is enough context for references to the visible recent
	// directory while keeping the state independent of the size of the session DB.
	MaxRecentSessionTitles = 12
	// MaxSessionTitleRunes bounds each title copied into classification state.
	MaxSessionTitleRunes = 160
)

// ErrInvalidAnswer identifies a response that arrived but did not satisfy the
// classifier's answer contract. Callers may safely record this category without
// persisting the provider's response body.
var ErrInvalidAnswer = errors.New("invalid live classify answer")

var intentLabels = []string{
	IntentSteer,
	IntentStatus,
	IntentNewSession,
	IntentSwitchSession,
	IntentSteerNow,
	IntentSide,
}

// State is the bounded, transport-independent context classified for one live
// delegation.
type State struct {
	Message             string   `json:"message"`
	TranscriptDelta     string   `json:"transcript_delta"`
	ActiveRun           bool     `json:"active_run"`
	ActiveTool          string   `json:"active_tool"`
	CurrentSessionTitle string   `json:"current_session_title"`
	RecentSessionTitles []string `json:"recent_session_titles"`
}

// Bounded returns a copy safe to persist and send to the classifier.
func (s State) Bounded() State {
	s.Message = headRunes(strings.TrimSpace(s.Message), MaxMessageRunes)
	s.TranscriptDelta = tailRunes(s.TranscriptDelta, MaxTranscriptRunes)
	s.ActiveTool = headRunes(strings.TrimSpace(s.ActiveTool), MaxSessionTitleRunes)
	s.CurrentSessionTitle = headRunes(strings.TrimSpace(s.CurrentSessionTitle), MaxSessionTitleRunes)
	if len(s.RecentSessionTitles) > MaxRecentSessionTitles {
		s.RecentSessionTitles = s.RecentSessionTitles[:MaxRecentSessionTitles]
	}
	titles := make([]string, 0, len(s.RecentSessionTitles))
	for _, title := range s.RecentSessionTitles {
		titles = append(titles, headRunes(strings.TrimSpace(title), MaxSessionTitleRunes))
	}
	s.RecentSessionTitles = titles
	return s
}

// MarshalState applies the state bounds and returns the exact JSON sent to the
// classification provider and, when enabled, the decision log.
func MarshalState(state State) ([]byte, error) {
	encoded, err := json.Marshal(state.Bounded())
	if err != nil {
		return nil, fmt.Errorf("marshal live classify state: %w", err)
	}
	return encoded, nil
}

// Decision is the validated classifier output used by Gate.
type Decision struct {
	Intent           string             `json:"intent"`
	Probabilities    map[string]float64 `json:"probabilities"`
	AlsoRequest      float64            `json:"also_request"`
	UnambiguousStop  float64            `json:"unambiguous_stop"`
	Model            string             `json:"model,omitempty"`
	InputTokens      *int               `json:"input_tokens,omitempty"`
	OutputTokens     *int               `json:"output_tokens,omitempty"`
	BoundedStateJSON json.RawMessage    `json:"-"`
}

// Client is the subset of typesafe.Client used by the live classifier.
type Client interface {
	Classify(context.Context, typesafe.Request) (*typesafe.Response, error)
}

// Classifier performs one stateless TypeSafe classification.
type Classifier struct {
	Client Client
	Model  string
}

// Classify sends the bounded state and validates all answers needed by both
// Phase 1 and the Phase 2 decision-log measurements.
func (c *Classifier) Classify(ctx context.Context, state State) (Decision, error) {
	var decision Decision
	if c == nil || c.Client == nil {
		return decision, errors.New("live classify client is nil")
	}
	if strings.TrimSpace(c.Model) == "" {
		return decision, errors.New("live classify model is required")
	}
	stateJSON, err := MarshalState(state)
	if err != nil {
		return decision, err
	}
	decision.BoundedStateJSON = append(json.RawMessage(nil), stateJSON...)
	response, err := c.Client.Classify(ctx, typesafe.Request{
		State: stateJSON, Model: c.Model, Questions: Questions(),
	})
	if err != nil {
		return decision, fmt.Errorf("live classify request: %w", err)
	}
	if response == nil {
		return decision, fmt.Errorf("%w: response is nil", ErrInvalidAnswer)
	}
	decision.Model = response.Model
	decision.InputTokens = response.Usage.InputTokens
	decision.OutputTokens = response.Usage.OutputTokens
	if err := validateAnswers(response.Answers, &decision); err != nil {
		return decision, fmt.Errorf("%w: %v", ErrInvalidAnswer, err)
	}
	return decision, nil
}

// Questions returns a fresh map because callers and HTTP test servers may retain
// or mutate request values.
func Questions() map[string]typesafe.Question {
	choiceCriteria := map[string]string{
		IntentSteer:         "Ordinary work, a question, guidance, or conversation for the bound session. Praise plus an ask, comments inviting a reply, and instructions such as 'say ok' are steer. General questions such as 'is it working?' about a feature are steer, not status.",
		IntentStatus:        "A request to report what term-llm sessions or scheduled jobs are currently running or recently completed, not whether a feature generally works.",
		IntentNewSession:    "A request to start a fresh empty conversation and move this voice call to it.",
		IntentSwitchSession: "A request to move this voice call to a different existing chat session.",
		IntentSteerNow:      "A request to interrupt the current running task and immediately replace or stop its work.",
		IntentSide:          "A private side question about the current conversation that should not interfere with or appear in the main transcript.",
	}
	intentInstructions, _ := json.Marshal(IntentQuestionInstructions)
	intentCriteria, _ := json.Marshal(choiceCriteria)
	alsoInstructions, _ := json.Marshal(AlsoRequestInstructions)
	alsoCriteria, _ := json.Marshal(map[string]string{
		"true":  "The request combines navigation with work, for example 'in the album chat, make the ornaments smaller'.",
		"false": "The request is only navigation, or it is ordinary work without navigation.",
	})
	stopInstructions, _ := json.Marshal(UnambiguousStopInstructions)
	stopCriteria, _ := json.Marshal(map[string]string{
		"true":  "The user clearly and directly asks to stop or cancel the active task now.",
		"false": "The request is guidance, ordinary work, ambiguous, or does not clearly ask for an immediate stop.",
	})
	return map[string]typesafe.Question{
		IntentQuestionID:          {Type: "choice", Instructions: intentInstructions, Criteria: intentCriteria},
		AlsoRequestQuestionID:     {Type: "noul", Instructions: alsoInstructions, Criteria: alsoCriteria},
		UnambiguousStopQuestionID: {Type: "noul", Instructions: stopInstructions, Criteria: stopCriteria},
	}
}

func validateAnswers(answers map[string]typesafe.Answer, decision *Decision) error {
	intent, ok := answers[IntentQuestionID]
	if !ok {
		return errors.New("live classify response missing intent")
	}
	if intent.Type != "choice" || intent.Choice == nil {
		return errors.New("live classify intent requires a choice answer")
	}
	decision.Intent = *intent.Choice
	decision.Probabilities = make(map[string]float64, len(intent.Probabilities))
	for label, probability := range intent.Probabilities {
		decision.Probabilities[label] = probability
	}
	if !validIntent(*intent.Choice) {
		return fmt.Errorf("live classify returned unknown intent %q", *intent.Choice)
	}
	if len(intent.Probabilities) == 0 {
		return errors.New("live classify intent probabilities are required")
	}
	probabilities := make(map[string]float64, len(intentLabels))
	for _, label := range intentLabels {
		probability, ok := intent.Probabilities[label]
		if !ok {
			return fmt.Errorf("live classify intent probability %q is missing", label)
		}
		if !validProbability(probability) {
			return fmt.Errorf("live classify intent probability %q must be finite and between 0 and 1", label)
		}
		probabilities[label] = probability
	}
	also, err := validatedNoul(answers, AlsoRequestQuestionID)
	if err != nil {
		return err
	}
	stop, err := validatedNoul(answers, UnambiguousStopQuestionID)
	if err != nil {
		return err
	}
	decision.Intent = *intent.Choice
	decision.Probabilities = probabilities
	decision.AlsoRequest = also
	decision.UnambiguousStop = stop
	return nil
}

func validatedNoul(answers map[string]typesafe.Answer, id string) (float64, error) {
	answer, ok := answers[id]
	if !ok {
		return 0, fmt.Errorf("live classify response missing %s", id)
	}
	if answer.Type != "noul" || answer.Noul == nil {
		return 0, fmt.Errorf("live classify %s requires a noul answer", id)
	}
	if !validProbability(*answer.Noul) {
		return 0, fmt.Errorf("live classify %s must be finite and between 0 and 1", id)
	}
	return *answer.Noul, nil
}

func validProbability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func validIntent(intent string) bool {
	for _, label := range intentLabels {
		if intent == label {
			return true
		}
	}
	return false
}

func headRunes(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}

func tailRunes(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[len(runes)-limit:])
}
