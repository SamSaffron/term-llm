package guardian

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

// recordingEscalationLogger captures escalation records for assertions.
type recordingEscalationLogger struct {
	mu      sync.Mutex
	records []Escalation
	err     error
}

func (l *recordingEscalationLogger) LogEscalation(record Escalation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, record)
	return l.err
}

func (l *recordingEscalationLogger) snapshot() []Escalation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Escalation(nil), l.records...)
}

// fallbackClassifyStub stands in for the TypeSafe client and records what was sent.
type fallbackClassifyStub struct {
	answers map[string]typesafe.Answer
	model   string
	usage   typesafe.Usage
	err     error
	onCall  func(context.Context)

	mu    sync.Mutex
	calls int
	state []byte
}

func (s *fallbackClassifyStub) Classify(ctx context.Context, req typesafe.Request) (*typesafe.Response, error) {
	s.mu.Lock()
	s.calls++
	s.state = append([]byte(nil), req.State...)
	s.mu.Unlock()
	if s.onCall != nil {
		s.onCall(ctx)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &typesafe.Response{Model: s.model, Answers: s.answers, Usage: s.usage}, nil
}

func (s *fallbackClassifyStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fallbackClassifyStub) stateBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.state)
}

func deniedAnswers() map[string]typesafe.Answer {
	answers := testAnswers()
	for id, answer := range answers {
		confidence := 0.4
		answer.Confidence = &confidence
		answers[id] = answer
	}
	return answers
}

func TestFallbackReviewerEscalation(t *testing.T) {
	transportErr := errors.New("transport failed")
	llmAllow := Decision{Outcome: "allow", RiskLevel: "low", UserAuthorization: "high", Rationale: "safe", Model: "fallback-model", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}}
	llmDeny := Decision{Outcome: "deny", RiskLevel: "high", UserAuthorization: "low", Rationale: "credential access", Model: "fallback-model"}
	// The fallback must never run in a fast path or after cancellation. The
	// constructor takes the subtest's *testing.T so the failure is attributed there.
	unexpectedFallback := func(reason string) func(*testing.T) ReviewFunc {
		return func(t *testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) {
				t.Fatalf("fallback ran %s", reason)
				return Decision{}, nil
			}
		}
	}

	tests := []struct {
		name              string
		client            *fallbackClassifyStub
		minConfidence     float64
		ctx               func() (context.Context, context.CancelFunc)
		cancelInFallback  bool // expire ctx while the fallback runs instead of during classify
		fallback          func(*testing.T) ReviewFunc
		logger            EscalationLogger
		request           Request
		wantErr           string
		wantFallbackCalls int
		wantRecords       int
		clientUnreached   bool
		check             func(t *testing.T, decision Decision, err error, records []Escalation)
	}{{
		name:          "classify allow is final",
		client:        &fallbackClassifyStub{answers: testAnswers(), model: "jev-live"},
		minConfidence: 0.5,
		fallback:      unexpectedFallback("for a classify allow"),
		wantRecords:   0,
		check: func(t *testing.T, decision Decision, err error, _ []Escalation) {
			if err != nil || !decision.Allowed() || decision.Escalated {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			if decision.Model != "jev-live" || decision.StateBytes == 0 {
				t.Fatalf("classify accounting lost: %+v", decision)
			}
		},
	}, {
		name:          "classify allow with an expired caller context never escalates",
		client:        &fallbackClassifyStub{answers: testAnswers(), model: "jev-live"},
		minConfidence: 0.5,
		ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		},
		fallback:    unexpectedFallback("with an expired caller context"),
		logger:      &recordingEscalationLogger{},
		wantErr:     context.Canceled.Error(),
		wantRecords: 0,
		check: func(t *testing.T, decision Decision, err error, _ []Escalation) {
			if decision.Allowed() || decision.Model != "jev-live" || decision.StateBytes == 0 {
				t.Fatalf("classify accounting lost: %+v", decision)
			}
		},
	}, {
		name:          "classify denial escalates to fallback allow",
		client:        &fallbackClassifyStub{answers: deniedAnswers(), model: "jev-live", usage: typesafe.Usage{InputTokens: ptrInt(11), OutputTokens: ptrInt(5)}},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) { return llmAllow, nil }
		},
		logger:            &recordingEscalationLogger{},
		wantFallbackCalls: 1,
		wantRecords:       1,
		check: func(t *testing.T, decision Decision, err error, records []Escalation) {
			if err != nil || !decision.Allowed() || !decision.Escalated {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			if decision.Model != "fallback-model" || decision.Usage.InputTokens != 7 || decision.RiskLevel != "low" || decision.UserAuthorization != "high" {
				t.Fatalf("fallback verdict not merged: %+v", decision)
			}
			if !strings.Contains(decision.Rationale, "escalated from classify (denied: failed gates:") || !strings.Contains(decision.Rationale, "llm: safe") {
				t.Fatalf("rationale = %q", decision.Rationale)
			}
			record := records[0]
			if record.SchemaVersion != EscalationSchemaVersion || EscalationSchemaVersion != 1 {
				t.Fatalf("schema_version = %d", record.SchemaVersion)
			}
			if record.ClassifyStage != classifyStageDecision || !record.ClassifyRequestSent || record.ClassifyRequest == nil {
				t.Fatalf("classify trace = %+v", record)
			}
			if len(record.ClassifyRequest.State) != decision.StateBytes || decision.StateBytes == 0 {
				t.Fatalf("state bytes = %d, decision = %d", len(record.ClassifyRequest.State), decision.StateBytes)
			}
			if len(record.ClassifyAnswers) != 3 || record.ClassifyError != "" {
				t.Fatalf("answers/error = %+v %q", record.ClassifyAnswers, record.ClassifyError)
			}
			if record.ClassifyModel != "jev-live" || record.ClassifyUsage.InputTokens != 11 || record.ClassifyUsage.OutputTokens != 5 {
				t.Fatalf("classify attribution = %+v", record)
			}
			if record.FallbackUsage.InputTokens != 7 || record.FallbackUsage.OutputTokens != 3 {
				t.Fatalf("fallback usage = %+v", record.FallbackUsage)
			}
			if record.ClassifyDecision == nil || record.ClassifyDecision.Allowed() {
				t.Fatalf("classify decision = %+v", record.ClassifyDecision)
			}
			if record.FallbackDecision == nil || !record.FallbackDecision.Allowed() || record.FallbackProvider != "chatgpt" || record.FallbackModel != "luna" {
				t.Fatalf("fallback verdict = %+v", record)
			}
			if record.MinConfidence != 0.5 || record.ClassifyDurationMS <= 0 || record.FallbackDurationMS < 0 || record.Timestamp.IsZero() {
				t.Fatalf("timing/threshold metadata = %+v", record)
			}
		},
	}, {
		name:          "fallback denial keeps both rationales",
		client:        &fallbackClassifyStub{answers: deniedAnswers()},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) { return llmDeny, nil }
		},
		logger:            &recordingEscalationLogger{},
		wantFallbackCalls: 1,
		wantRecords:       1,
		check: func(t *testing.T, decision Decision, err error, records []Escalation) {
			if err != nil || decision.Allowed() || !decision.Escalated {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			if !strings.Contains(decision.Rationale, "denied: failed gates: risk_level confidence") || !strings.Contains(decision.Rationale, "llm: credential access") {
				t.Fatalf("rationale = %q", decision.Rationale)
			}
			if records[0].FallbackDecision == nil || records[0].FallbackDecision.Allowed() {
				t.Fatalf("fallback verdict = %+v", records[0].FallbackDecision)
			}
		},
	}, {
		name:          "fallback verdict without a model or rationale stays readable",
		client:        &fallbackClassifyStub{answers: deniedAnswers()},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) {
				return Decision{Outcome: "allow", RiskLevel: "low", UserAuthorization: "high"}, nil
			}
		},
		wantFallbackCalls: 1,
		check: func(t *testing.T, decision Decision, err error, _ []Escalation) {
			if err != nil || !decision.Allowed() {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			if decision.Model != "luna" {
				t.Fatalf("model = %q, want the configured fallback model", decision.Model)
			}
			if !strings.HasSuffix(decision.Rationale, "llm: no rationale provided") {
				t.Fatalf("rationale = %q", decision.Rationale)
			}
		},
	}, {
		name:          "classify transport error escalates",
		client:        &fallbackClassifyStub{err: transportErr},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) { return llmAllow, nil }
		},
		logger:            &recordingEscalationLogger{},
		wantFallbackCalls: 1,
		wantRecords:       1,
		check: func(t *testing.T, decision Decision, err error, records []Escalation) {
			if err != nil || !decision.Allowed() {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			if !strings.Contains(decision.Rationale, "escalated from classify (error: guardian classify review: transport failed)") {
				t.Fatalf("rationale = %q", decision.Rationale)
			}
			record := records[0]
			if record.ClassifyStage != classifyStageTransport || !record.ClassifyRequestSent || record.ClassifyRequest == nil {
				t.Fatalf("classify trace = %+v", record)
			}
			if !strings.Contains(record.ClassifyError, "transport failed") || record.ClassifyDecision != nil {
				t.Fatalf("classify error = %q, decision = %+v", record.ClassifyError, record.ClassifyDecision)
			}
		},
	}, {
		name:          "malformed answers escalate with answers preserved",
		client:        &fallbackClassifyStub{answers: malformedAnswers()},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) { return llmAllow, nil }
		},
		logger:            &recordingEscalationLogger{},
		wantFallbackCalls: 1,
		wantRecords:       1,
		check: func(t *testing.T, decision Decision, err error, records []Escalation) {
			if err != nil || !decision.Allowed() {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			record := records[0]
			if record.ClassifyStage != classifyStageValidate || !record.ClassifyRequestSent {
				t.Fatalf("classify trace = %+v", record)
			}
			if !strings.Contains(record.ClassifyError, "outcome requires a choice and confidence") {
				t.Fatalf("classify error = %q", record.ClassifyError)
			}
			if answer, ok := record.ClassifyAnswers["risk_level"]; !ok || answer.Choice == nil || *answer.Choice != "low" {
				t.Fatalf("answers not preserved: %+v", record.ClassifyAnswers)
			}
		},
	}, {
		name:          "oversize exact action escalates without sending",
		client:        &fallbackClassifyStub{answers: testAnswers()},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) { return llmAllow, nil }
		},
		logger:            &recordingEscalationLogger{},
		request:           Request{Command: "echo " + strings.Repeat("x", 25000)},
		wantFallbackCalls: 1,
		wantRecords:       1,
		clientUnreached:   true,
		check: func(t *testing.T, decision Decision, err error, records []Escalation) {
			if err != nil || !decision.Allowed() || decision.StateBytes == 0 {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			record := records[0]
			if record.ClassifyStage != classifyStageBuild || record.ClassifyRequestSent || record.ClassifyRequest != nil {
				t.Fatalf("classify trace = %+v", record)
			}
			if !strings.Contains(record.ClassifyError, "manual approval") || record.ClassifyAnswers != nil {
				t.Fatalf("classify error = %q, answers = %+v", record.ClassifyError, record.ClassifyAnswers)
			}
		},
	}, {
		name:          "classify error with a fallback error reports both",
		client:        &fallbackClassifyStub{err: transportErr},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) {
				return Decision{Model: "fallback-model", Usage: llm.Usage{OutputTokens: 4}}, errors.New("fallback unavailable")
			}
		},
		logger:            &recordingEscalationLogger{},
		wantErr:           "guardian fallback review: fallback unavailable (after classify error: guardian classify review: transport failed)",
		wantFallbackCalls: 1,
		wantRecords:       1,
		check: func(t *testing.T, decision Decision, err error, records []Escalation) {
			if decision.Allowed() || !decision.Escalated || decision.Model != "fallback-model" || decision.Usage.OutputTokens != 4 || decision.StateBytes == 0 {
				t.Fatalf("accounting = %+v", decision)
			}
			record := records[0]
			if record.ClassifyError == "" || record.FallbackError != "fallback unavailable" || record.FallbackDecision != nil {
				t.Fatalf("record = %+v", record)
			}
		},
	}, {
		name:          "classify denial with a fallback error keeps the denial",
		client:        &fallbackClassifyStub{answers: deniedAnswers(), model: "jev-live"},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) {
				return Decision{}, errors.New("fallback unavailable")
			}
		},
		logger:            &recordingEscalationLogger{},
		wantFallbackCalls: 1,
		wantRecords:       1,
		check: func(t *testing.T, decision Decision, err error, records []Escalation) {
			if err != nil || decision.Allowed() || decision.Escalated {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			if !strings.Contains(decision.Rationale, "classify denied: failed gates: risk_level confidence") || !strings.Contains(decision.Rationale, "fallback unavailable: fallback unavailable") {
				t.Fatalf("rationale = %q", decision.Rationale)
			}
			if decision.Model != "jev-live" || decision.StateBytes == 0 || decision.RiskLevel != "low" {
				t.Fatalf("classify verdict lost: %+v", decision)
			}
			record := records[0]
			if record.ClassifyDecision == nil || record.ClassifyDecision.Allowed() || record.ClassifyModel != "jev-live" {
				t.Fatalf("classify verdict missing: %+v", record)
			}
			if record.FallbackError != "fallback unavailable" || record.FallbackDecision != nil {
				t.Fatalf("fallback error missing: %+v", record)
			}
		},
	}, {
		name:              "missing fallback reviewer keeps the classify denial",
		client:            &fallbackClassifyStub{answers: deniedAnswers()},
		minConfidence:     0.5,
		logger:            &recordingEscalationLogger{},
		wantRecords:       1,
		wantFallbackCalls: 0,
		check: func(t *testing.T, decision Decision, err error, records []Escalation) {
			if err != nil || decision.Allowed() || decision.Escalated {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			if !strings.Contains(decision.Rationale, "fallback unavailable: guardian fallback reviewer is not configured") {
				t.Fatalf("rationale = %q", decision.Rationale)
			}
			if records[0].FallbackError == "" {
				t.Fatalf("record = %+v", records[0])
			}
		},
	}, {
		name:          "classify denial with expired caller context never escalates",
		client:        &fallbackClassifyStub{answers: deniedAnswers()},
		minConfidence: 0.5,
		ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		},
		fallback:    unexpectedFallback("with an expired caller context"),
		logger:      &recordingEscalationLogger{},
		wantErr:     context.Canceled.Error(),
		wantRecords: 0,
		check: func(t *testing.T, decision Decision, err error, _ []Escalation) {
			if decision.Allowed() || decision.Model != "jev-test" || decision.StateBytes == 0 {
				t.Fatalf("classify accounting lost: %+v", decision)
			}
		},
	}, {
		name:          "caller cancelled during fallback never allows",
		client:        &fallbackClassifyStub{answers: deniedAnswers()},
		minConfidence: 0.5,
		ctx: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		cancelInFallback: true,
		fallback: func(*testing.T) ReviewFunc {
			// A verdict that raced the cancellation must not be applied.
			return func(context.Context, Request) (Decision, error) { return llmAllow, nil }
		},
		logger:            &recordingEscalationLogger{},
		wantErr:           context.Canceled.Error(),
		wantFallbackCalls: 1,
		wantRecords:       0,
		check: func(t *testing.T, decision Decision, err error, _ []Escalation) {
			if decision.Allowed() || decision.Model != "fallback-model" || decision.StateBytes == 0 {
				t.Fatalf("decision = %+v", decision)
			}
		},
	}, {
		name:          "caller cancelled during fallback never counts the classify denial",
		client:        &fallbackClassifyStub{answers: deniedAnswers()},
		minConfidence: 0.5,
		ctx: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		cancelInFallback: true,
		fallback: func(*testing.T) ReviewFunc {
			// The reviewer pool reports the caller's cancellation as its error.
			return func(ctx context.Context, _ Request) (Decision, error) { return Decision{}, ctx.Err() }
		},
		logger:            &recordingEscalationLogger{},
		wantErr:           context.Canceled.Error(),
		wantFallbackCalls: 1,
		wantRecords:       0,
		check: func(t *testing.T, decision Decision, err error, _ []Escalation) {
			if decision.Allowed() || decision.Model != "luna" || decision.StateBytes == 0 {
				t.Fatalf("decision = %+v", decision)
			}
		},
	}, {
		name:          "missing logger still escalates",
		client:        &fallbackClassifyStub{answers: deniedAnswers()},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) { return llmAllow, nil }
		},
		wantFallbackCalls: 1,
		wantRecords:       0,
		check: func(t *testing.T, decision Decision, err error, _ []Escalation) {
			if err != nil || !decision.Allowed() || !decision.Escalated {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
		},
	}, {
		name:          "logger failure does not change the verdict",
		client:        &fallbackClassifyStub{answers: deniedAnswers()},
		minConfidence: 0.5,
		fallback: func(*testing.T) ReviewFunc {
			return func(context.Context, Request) (Decision, error) { return llmAllow, nil }
		},
		logger:            &recordingEscalationLogger{err: errors.New("disk full")},
		wantFallbackCalls: 1,
		wantRecords:       1,
		check: func(t *testing.T, decision Decision, err error, _ []Escalation) {
			if err != nil || !decision.Allowed() || !decision.Escalated {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var cancel context.CancelFunc
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
				defer cancel()
				if !tc.cancelInFallback {
					// Expire the caller context while the classifier is still running.
					tc.client.onCall = func(context.Context) { cancel() }
				}
			}
			fallbackCalls := 0
			wantClassifyCalls := 1
			if tc.clientUnreached {
				wantClassifyCalls = 0
			}
			reviewer := &FallbackReviewer{
				Classify:         &ClassifyReviewer{Client: tc.client, Model: "jev-test", MinConfidence: tc.minConfidence, Timeout: time.Second},
				FallbackProvider: "chatgpt",
				FallbackModel:    "luna",
				Logger:           tc.logger,
			}
			if tc.fallback != nil {
				fallback := tc.fallback(t)
				reviewer.Fallback = func(ctx context.Context, req Request) (Decision, error) {
					fallbackCalls++
					if tc.cancelInFallback {
						// Expire the caller context while the fallback is running.
						cancel()
					}
					return fallback(ctx, req)
				}
			}
			decision, err := reviewer.Review(ctx, tc.request)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if tc.ctx != nil && (err == nil || !errors.Is(err, context.Canceled)) {
				t.Fatalf("expired caller context must surface: %v", err)
			}
			if fallbackCalls != tc.wantFallbackCalls {
				t.Fatalf("fallback calls = %d, want %d", fallbackCalls, tc.wantFallbackCalls)
			}
			records := []Escalation{}
			if recording, ok := tc.logger.(*recordingEscalationLogger); ok {
				records = recording.snapshot()
			}
			if len(records) != tc.wantRecords {
				t.Fatalf("records = %d, want %d", len(records), tc.wantRecords)
			}
			if tc.client.callCount() != wantClassifyCalls {
				t.Fatalf("classify calls = %d, want %d", tc.client.callCount(), wantClassifyCalls)
			}
			if wantClassifyCalls > 0 && tc.client.stateBytes() != decision.StateBytes {
				t.Fatalf("classify state bytes = %d, decision = %d", tc.client.stateBytes(), decision.StateBytes)
			}
			tc.check(t, decision, err, records)
		})
	}
}

func ptrInt(value int) *int { return &value }

func malformedAnswers() map[string]typesafe.Answer {
	answers := testAnswers()
	delete(answers, "outcome")
	return answers
}
