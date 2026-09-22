package guardian

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

// ReviewFunc is the shape shared by every Guardian backend.
type ReviewFunc func(context.Context, Request) (Decision, error)

// EscalationSchemaVersion identifies the escalation record layout.
const EscalationSchemaVersion = 1

// Escalation records one classify → LLM handoff for later classifier tuning.
// It is a pre-enforcement reviewer result: the approval manager may still deny a
// fallback allow whose risk/authorization fields contradict policy.
type Escalation struct {
	SchemaVersion int       `json:"schema_version"`
	Timestamp     time.Time `json:"timestamp"`
	ScopeID       string    `json:"scope_id,omitempty"`

	// Classifier input, exactly as constructed. Request is nil when the request
	// could not be built (e.g. state over budget) and nothing was sent.
	ClassifyRequest     *typesafe.Request          `json:"classify_request,omitempty"` // State, Model, Questions
	ClassifyRequestSent bool                       `json:"classify_request_sent"`
	MinConfidence       float64                    `json:"min_confidence"`
	ClassifyStage       string                     `json:"classify_stage"` // "build" | "transport" | "validate" | "decision"
	ClassifyAnswers     map[string]typesafe.Answer `json:"classify_answers,omitempty"`
	ClassifyDecision    *Decision                  `json:"classify_decision,omitempty"` // set when stage == "decision" (a deny)
	ClassifyError       string                     `json:"classify_error,omitempty"`
	ClassifyModel       string                     `json:"classify_model,omitempty"` // the response model when the provider reported one
	ClassifyUsage       escalationUsage            `json:"classify_usage"`
	ClassifyDurationMS  float64                    `json:"classify_duration_ms"`

	FallbackProvider   string          `json:"fallback_provider"`
	FallbackModel      string          `json:"fallback_model"`
	FallbackDecision   *Decision       `json:"fallback_decision,omitempty"` // raw verdict incl. risk/auth/outcome/rationale
	FallbackError      string          `json:"fallback_error,omitempty"`
	FallbackUsage      escalationUsage `json:"fallback_usage"`
	FallbackDurationMS float64         `json:"fallback_duration_ms"`
}

// escalationUsage renders token accounting with stable snake_case keys: the
// record is a data format for later classifier tuning, not an internal struct.
type escalationUsage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	CacheWriteTokens  int `json:"cache_write_tokens"`
}

func newEscalationUsage(usage llm.Usage) escalationUsage {
	return escalationUsage{
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		CacheWriteTokens:  usage.CacheWriteTokens,
	}
}

// EscalationLogger persists escalation records. Implementations report their own
// failures; a logging failure never changes a review result.
type EscalationLogger interface{ LogEscalation(Escalation) error }

// FallbackReviewer runs the classify reviewer first and escalates a denial or a
// classifier failure to a standard LLM Guardian reviewer whose verdict becomes
// the decision.
type FallbackReviewer struct {
	Classify         *ClassifyReviewer
	Fallback         ReviewFunc
	FallbackProvider string
	FallbackModel    string
	Logger           EscalationLogger // optional
}

func (f *FallbackReviewer) Review(ctx context.Context, req Request) (Decision, error) {
	if f == nil || f.Classify == nil {
		return Decision{}, fmt.Errorf("guardian fallback reviewer requires a classify reviewer")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	decision, trace, classifyErr := f.Classify.reviewTraced(ctx, req)
	// Attribute the classifier's model, usage and budget even when the caller's
	// context is already done, so accounting stays meaningful.
	classifyAccounting := Decision{Model: decision.Model, Usage: decision.Usage, StateBytes: decision.StateBytes}
	if err := ctx.Err(); err != nil {
		// Never return a nil error once the caller's context is done: an allow
		// that raced cancellation must not proceed, and a classify denial must
		// not be recorded by the approval manager as a policy denial. Nothing is
		// escalated or logged because the review never completed.
		return classifyAccounting, err
	}
	// An allow is final: the fast path logs nothing and never reaches the LLM.
	if classifyErr == nil && decision.Allowed() {
		return decision, nil
	}

	reason := classifyEscalationReason(decision, classifyErr)
	fallbackStarted := time.Now()
	var fallback Decision
	var fallbackErr error
	if f.Fallback == nil {
		fallbackErr = fmt.Errorf("guardian fallback reviewer is not configured")
	} else {
		fallback, fallbackErr = f.Fallback(ctx, req)
	}
	fallbackDurationMS := float64(time.Since(fallbackStarted)) / float64(time.Millisecond)
	if err := ctx.Err(); err != nil {
		// The caller gave up while the fallback was queued or running, so the
		// review never completed whatever the fallback returned: a fallback
		// allow must not proceed, the classify denial must not count toward the
		// breaker, and no training record is written for an aborted handoff.
		// Only the caller's context is checked: a fallback timeout or a pool
		// closed by a reload cancels a child context and still counts below.
		return Decision{
			Model:      f.attributedFallbackModel(fallback),
			Usage:      fallback.Usage,
			StateBytes: classifyAccounting.StateBytes,
			Escalated:  true,
		}, err
	}
	if f.Logger != nil {
		// The logger warns once on its own failure signal; the verdict stands.
		_ = f.Logger.LogEscalation(f.escalation(req, decision, classifyErr, trace, fallback, fallbackErr, fallbackDurationMS))
	}

	if fallbackErr != nil {
		if classifyErr == nil {
			// The classifier's own denial stands. The fallback is an optional
			// second opinion, so a broken fallback provider must not turn a
			// policy denial into a non-counting review failure: the approval
			// manager still counts this denial and the breaker safety net keeps
			// working. The failure is noted in the rationale and the record.
			verdict := decision
			verdict.Rationale = fmt.Sprintf("classify %s; fallback unavailable: %v", reason, fallbackErr)
			return verdict, nil
		}
		// Fail closed without counting as a policy denial, exactly like the LLM
		// backend's own review failures. The classifier's budget still applies.
		return Decision{
			Model:      f.attributedFallbackModel(fallback),
			Usage:      fallback.Usage,
			StateBytes: classifyAccounting.StateBytes,
			Escalated:  true,
		}, fmt.Errorf("guardian fallback review: %w (after classify %s)", fallbackErr, reason)
	}
	return Decision{
		Model:             f.attributedFallbackModel(fallback),
		Usage:             fallback.Usage,
		StateBytes:        classifyAccounting.StateBytes,
		RiskLevel:         fallback.RiskLevel,
		UserAuthorization: fallback.UserAuthorization,
		Outcome:           fallback.Outcome,
		Rationale:         fmt.Sprintf("escalated from classify (%s); llm: %s", reason, fallbackRationale(fallback.Rationale)),
		Escalated:         true,
	}, nil
}

// attributedFallbackModel names the fallback verdict's model, preferring what
// the reviewer reported and falling back to the configured target.
func (f *FallbackReviewer) attributedFallbackModel(fallback Decision) string {
	if model := strings.TrimSpace(fallback.Model); model != "" {
		return model
	}
	return f.FallbackModel
}

func (f *FallbackReviewer) escalation(req Request, classifyDecision Decision, classifyErr error, trace classifyTrace, fallback Decision, fallbackErr error, fallbackDurationMS float64) Escalation {
	record := Escalation{
		SchemaVersion:       EscalationSchemaVersion,
		Timestamp:           time.Now().UTC(),
		ScopeID:             strings.TrimSpace(req.ScopeID),
		ClassifyRequest:     trace.Request,
		ClassifyRequestSent: trace.Sent,
		MinConfidence:       f.Classify.MinConfidence,
		ClassifyStage:       trace.Stage,
		ClassifyAnswers:     trace.Answers,
		ClassifyError:       escalationErrorString(classifyErr),
		ClassifyModel:       classifyDecision.Model,
		ClassifyUsage:       newEscalationUsage(classifyDecision.Usage),
		ClassifyDurationMS:  trace.DurationMS,
		FallbackProvider:    f.FallbackProvider,
		FallbackModel:       f.FallbackModel,
		FallbackError:       escalationErrorString(fallbackErr),
		FallbackUsage:       newEscalationUsage(fallback.Usage),
		FallbackDurationMS:  fallbackDurationMS,
	}
	if classifyErr == nil {
		// A well-formed classifier denial: keep its risk/authorization values and
		// the full rationale the merged decision shortens.
		verdict := classifyDecision
		record.ClassifyDecision = &verdict
	}
	if fallbackErr == nil {
		verdict := fallback
		record.FallbackDecision = &verdict
	}
	return record
}

// fallbackRationale keeps the merged rationale readable when the LLM reviewer
// returned no rationale of its own.
func fallbackRationale(rationale string) string {
	if strings.TrimSpace(rationale) == "" {
		return "no rationale provided"
	}
	return rationale
}

// classifyEscalationReason summarizes why the classifier did not allow the
// action. It stays short: the full classifier rationale is in the log record.
func classifyEscalationReason(decision Decision, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	rationale := strings.TrimSpace(decision.Rationale)
	if index := strings.LastIndex(rationale, "failed gates: "); index >= 0 {
		rationale = strings.TrimSpace(rationale[index:])
	}
	if rationale == "" {
		rationale = "denied"
	}
	return "denied: " + rationale
}

// escalationErrorString renders an optional reviewer error for the log record.
func escalationErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
