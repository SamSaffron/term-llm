package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

// guardianFallbackClassifyStub answers every question with one confidence so a
// threshold above it always produces a deterministic classifier denial. A
// non-nil err makes every classification fail instead.
type guardianFallbackClassifyStub struct {
	confidence float64
	err        error

	mu    sync.Mutex
	calls int
	state []byte
}

func (s *guardianFallbackClassifyStub) ListModels(context.Context) (*typesafe.ModelsResponse, error) {
	panic("not used")
}

func (s *guardianFallbackClassifyStub) Classify(ctx context.Context, req typesafe.Request) (*typesafe.Response, error) {
	s.mu.Lock()
	s.calls++
	s.state = append([]byte(nil), req.State...)
	s.mu.Unlock()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Second {
		panic("missing guardian deadline")
	}
	if s.err != nil {
		return nil, s.err
	}
	answers := map[string]typesafe.Answer{}
	for id, choice := range map[string]string{"risk_level": "low", "user_authorization": "explicit", "outcome": "allow"} {
		c, confidence := choice, s.confidence
		answers[id] = typesafe.Answer{Type: "choice", Choice: &c, Confidence: &confidence}
	}
	return &typesafe.Response{Model: req.Model, Answers: answers}, nil
}

func (s *guardianFallbackClassifyStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *guardianFallbackClassifyStub) stateBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.state)
}

// guardianEscalationStub records escalations written by the composite reviewer.
type guardianEscalationStub struct {
	mu      sync.Mutex
	records []guardian.Escalation
}

func (s *guardianEscalationStub) LogEscalation(record guardian.Escalation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}

func (s *guardianEscalationStub) snapshot() []guardian.Escalation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]guardian.Escalation(nil), s.records...)
}

func withGuardianClassifyClient(t *testing.T, stub classifyClient) {
	t.Helper()
	original := newGuardianClassifyClient
	newGuardianClassifyClient = func(typesafe.Options) (classifyClient, error) { return stub, nil }
	t.Cleanup(func() { newGuardianClassifyClient = original })
}

func withGuardianEscalationLogger(t *testing.T, logger guardian.EscalationLogger) {
	t.Helper()
	original := newGuardianEscalationLogger
	newGuardianEscalationLogger = func(*config.Config) guardian.EscalationLogger { return logger }
	t.Cleanup(func() { newGuardianEscalationLogger = original })
}

func guardianFallbackTestConfig(fallback config.GuardianFallbackConfig) *config.Config {
	return &config.Config{
		Guardian: config.GuardianConfig{
			Backend:        "classify",
			TimeoutSeconds: 2,
			Classify:       config.GuardianClassifyConfig{MinConfidence: 0.5},
			Fallback:       fallback,
		},
		Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"typesafe": {APIKey: "test-key"}}},
	}
}

func TestGuardianClassifyFallbackEscalatesDenial(t *testing.T) {
	stub := &guardianFallbackClassifyStub{confidence: 0.4}
	withGuardianClassifyClient(t, stub)
	logger := &guardianEscalationStub{}
	withGuardianEscalationLogger(t, logger)
	provider := llm.NewMockProvider("fallback").AddTextResponse(`{"outcome":"allow","risk_level":"low","user_authorization":"high","rationale":"ok"}`)
	factoryCalls := 0
	var gotName, gotModel string
	withGuardianProviderFactory(t, func(_ *config.Config, name, model string) (llm.Provider, error) {
		factoryCalls++
		gotName, gotModel = name, model
		return provider, nil
	})
	cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	defer mgr.Close()
	if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
		t.Fatal(err)
	}
	var events []tools.GuardianEvent
	mgr.GuardianEventFunc = func(event tools.GuardianEvent) { events = append(events, event) }

	outcome, err := mgr.CheckShellApproval("echo escalate", t.TempDir())
	if err != nil || outcome != tools.ProceedAlways {
		t.Fatalf("outcome=%v error=%v, want the fallback verdict", outcome, err)
	}
	if stub.callCount() != 1 || factoryCalls != 1 {
		t.Fatalf("classify calls=%d fallback factory calls=%d, want one each", stub.callCount(), factoryCalls)
	}
	if gotName != "chatgpt" || gotModel != "gpt-5.6-luna-low" {
		t.Fatalf("fallback provider factory called with (%q, %q)", gotName, gotModel)
	}
	if len(events) != 1 {
		t.Fatalf("events=%d, want 1", len(events))
	}
	event := events[0]
	if event.Outcome != tools.GuardianApproved || event.Model != "gpt-5.6-luna-low" || !strings.Contains(event.Message, "via fallback") {
		t.Fatalf("event = %+v", event)
	}
	if mgr.ApprovalMode() != tools.ModeAuto {
		t.Fatalf("mode = %s, want auto: an escalated allow must not trip the breaker", mgr.ApprovalMode())
	}
	records := logger.snapshot()
	if len(records) != 1 {
		t.Fatalf("escalations=%d, want 1", len(records))
	}
	record := records[0]
	if record.ClassifyStage != "decision" || !record.ClassifyRequestSent || record.ClassifyRequest == nil {
		t.Fatalf("classify trace = %+v", record)
	}
	if len(record.ClassifyRequest.State) != event.StateBytes || event.StateBytes == 0 {
		t.Fatalf("state bytes = %d, event = %d", len(record.ClassifyRequest.State), event.StateBytes)
	}
	if record.MinConfidence != 0.5 || record.FallbackProvider != "chatgpt" || record.FallbackModel != "gpt-5.6-luna-low" {
		t.Fatalf("record = %+v", record)
	}
	if record.FallbackDecision == nil || !record.FallbackDecision.Allowed() || record.ClassifyDecision == nil {
		t.Fatalf("verdicts = %+v / %+v", record.ClassifyDecision, record.FallbackDecision)
	}
}

func TestGuardianClassifyFallbackNotUsedOnAllow(t *testing.T) {
	stub := &guardianFallbackClassifyStub{confidence: 0.9}
	withGuardianClassifyClient(t, stub)
	logger := &guardianEscalationStub{}
	withGuardianEscalationLogger(t, logger)
	provider := llm.NewMockProvider("fallback")
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	defer mgr.Close()
	if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
		t.Fatal(err)
	}
	var events []tools.GuardianEvent
	mgr.GuardianEventFunc = func(event tools.GuardianEvent) { events = append(events, event) }

	outcome, err := mgr.CheckShellApproval("echo fast path", t.TempDir())
	if err != nil || outcome != tools.ProceedAlways {
		t.Fatalf("outcome=%v error=%v", outcome, err)
	}
	if len(provider.RecordedRequests()) != 0 {
		t.Fatalf("fallback provider was called for a classify allow: %#v", provider.RecordedRequests())
	}
	if len(logger.snapshot()) != 0 {
		t.Fatalf("classify allow was logged: %+v", logger.snapshot())
	}
	if len(events) != 1 || strings.Contains(events[0].Message, "via fallback") {
		t.Fatalf("events = %+v", events)
	}
}

func TestGuardianClassifyFallbackDenialTripsBreaker(t *testing.T) {
	stub := &guardianFallbackClassifyStub{confidence: 0.4}
	withGuardianClassifyClient(t, stub)
	logger := &guardianEscalationStub{}
	withGuardianEscalationLogger(t, logger)
	denial := `{"outcome":"deny","risk_level":"high","user_authorization":"low","rationale":"credential access"}`
	provider := llm.NewMockProvider("fallback").AddTextResponse(denial).AddTextResponse(denial).AddTextResponse(denial)
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	defer mgr.Close()
	if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
		t.Fatal(err)
	}

	for i, command := range []string{"echo one", "echo two", "echo three"} {
		outcome, err := mgr.CheckShellApproval(command, t.TempDir())
		if outcome != tools.Cancel || err == nil || !strings.Contains(err.Error(), "credential access") {
			t.Fatalf("review %d: outcome=%v error=%v", i, outcome, err)
		}
	}
	if mgr.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("mode = %s, want prompt after three fallback denials", mgr.ApprovalMode())
	}
	records := logger.snapshot()
	if len(records) != 3 {
		t.Fatalf("escalations=%d, want 3", len(records))
	}
	for _, record := range records {
		if record.FallbackDecision == nil || record.FallbackDecision.Allowed() {
			t.Fatalf("record = %+v", record)
		}
	}
}

func TestGuardianClassifyFallbackErrorAfterClassifyDenyCountsTowardBreaker(t *testing.T) {
	stub := &guardianFallbackClassifyStub{confidence: 0.4}
	withGuardianClassifyClient(t, stub)
	logger := &guardianEscalationStub{}
	withGuardianEscalationLogger(t, logger)
	transport := errors.New("fallback transport down")
	provider := llm.NewMockProvider("fallback").AddError(transport).AddError(transport).AddError(transport)
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	defer mgr.Close()
	if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
		t.Fatal(err)
	}
	var events []tools.GuardianEvent
	mgr.GuardianEventFunc = func(event tools.GuardianEvent) { events = append(events, event) }

	// The classifier denied, so its denial stands even though the fallback is
	// broken: the manager must count it as a policy denial.
	for i, command := range []string{"echo one", "echo two", "echo three"} {
		outcome, err := mgr.CheckShellApproval(command, t.TempDir())
		if outcome != tools.Cancel || err == nil || !strings.Contains(err.Error(), "guardian denied this action") {
			t.Fatalf("review %d: outcome=%v error=%v", i, outcome, err)
		}
		if !strings.Contains(err.Error(), "fallback unavailable") || !strings.Contains(err.Error(), "fallback transport down") {
			t.Fatalf("review %d: error = %v, want the classifier denial with the fallback failure", i, err)
		}
	}
	if mgr.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("mode = %s, want prompt after three classifier denials", mgr.ApprovalMode())
	}
	for _, event := range events {
		switch event.Outcome {
		case tools.GuardianDenied:
		case tools.GuardianWarning:
			// The breaker-trip suspension notice is expected on the third denial.
		default:
			t.Fatalf("event = %+v, want a policy denial", event)
		}
	}
	records := logger.snapshot()
	if len(records) != 3 {
		t.Fatalf("escalations=%d, want 3", len(records))
	}
	for _, record := range records {
		if !strings.Contains(record.FallbackError, "fallback transport down") || record.FallbackDecision != nil {
			t.Fatalf("record = %+v", record)
		}
		if record.ClassifyStage != "decision" || record.ClassifyDecision == nil || record.ClassifyDecision.Allowed() {
			t.Fatalf("classifier outcome missing: %+v", record)
		}
	}
}

func TestGuardianClassifyFallbackErrorAfterClassifyErrorDoesNotTripBreaker(t *testing.T) {
	stub := &guardianFallbackClassifyStub{err: errors.New("classify transport down")}
	withGuardianClassifyClient(t, stub)
	logger := &guardianEscalationStub{}
	withGuardianEscalationLogger(t, logger)
	transport := errors.New("fallback transport down")
	provider := llm.NewMockProvider("fallback").AddError(transport).AddError(transport).AddError(transport)
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	defer mgr.Close()
	if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
		t.Fatal(err)
	}

	// Neither reviewer produced a verdict, so this is a review failure that must
	// not count toward the breaker.
	for i, command := range []string{"echo one", "echo two", "echo three"} {
		outcome, err := mgr.CheckShellApproval(command, t.TempDir())
		if outcome != tools.Cancel || err == nil || !strings.Contains(err.Error(), "could not review") {
			t.Fatalf("review %d: outcome=%v error=%v", i, outcome, err)
		}
		if !strings.Contains(err.Error(), "classify transport down") || !strings.Contains(err.Error(), "fallback transport down") {
			t.Fatalf("review %d: error = %v, want both reviewer failures", i, err)
		}
	}
	if mgr.ApprovalMode() != tools.ModeAuto {
		t.Fatalf("mode = %s, want auto: reviewer failures do not count", mgr.ApprovalMode())
	}
	records := logger.snapshot()
	if len(records) != 3 {
		t.Fatalf("escalations=%d, want 3", len(records))
	}
	for _, record := range records {
		if record.ClassifyStage != "transport" || !strings.Contains(record.ClassifyError, "classify transport down") {
			t.Fatalf("classify error missing: %+v", record)
		}
		if !strings.Contains(record.FallbackError, "fallback transport down") || record.FallbackDecision != nil || record.ClassifyDecision != nil {
			t.Fatalf("record = %+v", record)
		}
	}
}

func TestGuardianClassifyFallbackContradictoryAllowIsDenied(t *testing.T) {
	stub := &guardianFallbackClassifyStub{confidence: 0.4}
	withGuardianClassifyClient(t, stub)
	logger := &guardianEscalationStub{}
	withGuardianEscalationLogger(t, logger)
	contradictory := `{"outcome":"allow","risk_level":"high","user_authorization":"low","rationale":"risky but allowed"}`
	provider := llm.NewMockProvider("fallback").AddTextResponse(contradictory).AddTextResponse(contradictory).AddTextResponse(contradictory)
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	defer mgr.Close()
	if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
		t.Fatal(err)
	}
	var events []tools.GuardianEvent
	mgr.GuardianEventFunc = func(event tools.GuardianEvent) { events = append(events, event) }

	for i, command := range []string{"echo one", "echo two", "echo three"} {
		outcome, err := mgr.CheckShellApproval(command, t.TempDir())
		if outcome != tools.Cancel || err == nil || !strings.Contains(err.Error(), "escalated from classify") {
			t.Fatalf("review %d: outcome=%v error=%v", i, outcome, err)
		}
	}
	if mgr.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("mode = %s, want prompt: contradictory allows count as denials", mgr.ApprovalMode())
	}
	denials := 0
	for _, event := range events {
		switch event.Outcome {
		case tools.GuardianDenied:
			denials++
		case tools.GuardianApproved:
			t.Fatalf("contradictory allow was approved: %+v", event)
		}
	}
	if denials != 3 {
		t.Fatalf("denial events = %d, want 3: %+v", denials, events)
	}
	records := logger.snapshot()
	if len(records) != 3 {
		t.Fatalf("escalations=%d, want 3", len(records))
	}
	for _, record := range records {
		// The log keeps the raw pre-enforcement verdict, which the manager denied.
		if record.FallbackDecision == nil || !record.FallbackDecision.Allowed() || record.FallbackDecision.RiskLevel != "high" {
			t.Fatalf("record = %+v", record)
		}
	}
}

func TestGuardianClassifyFallbackSetupFailures(t *testing.T) {
	stub := &guardianFallbackClassifyStub{confidence: 0.9}
	withGuardianClassifyClient(t, stub)
	withGuardianEscalationLogger(t, &guardianEscalationStub{})

	t.Run("fallback provider fails to construct", func(t *testing.T) {
		withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
			return nil, errors.New("no credentials")
		})
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
		mgr := tools.NewApprovalManager(tools.NewToolPermissions())
		defer mgr.Close()
		err := installGuardianReviewerCallbacks(cfg, mgr, true)
		if err == nil || !strings.Contains(err.Error(), "guardian fallback") || !strings.Contains(err.Error(), "no credentials") {
			t.Fatalf("install error = %v", err)
		}
		if mgr.GuardianReviewerAvailable() {
			t.Fatal("reviewer was installed despite the fallback failure")
		}
		if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err == nil || !strings.Contains(err.Error(), "auto approval unavailable") {
			t.Fatalf("headless error = %v", err)
		}
		var warnings bytes.Buffer
		if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{WarningWriter: &warnings}); err != nil || mgr.ApprovalMode() != tools.ModePrompt || !strings.Contains(warnings.String(), "guardian auto-approval unavailable") {
			t.Fatalf("interactive: %v %s", err, warnings.String())
		}
	})

	t.Run("fallback on the llm backend", func(t *testing.T) {
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
		cfg.Guardian.Backend = "llm"
		mgr := tools.NewApprovalManager(tools.NewToolPermissions())
		defer mgr.Close()
		if err := installGuardianReviewerCallbacks(cfg, mgr, true); err == nil || !strings.Contains(err.Error(), "guardian.fallback requires guardian.backend: classify") {
			t.Fatalf("install error = %v", err)
		}
		if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err == nil {
			t.Fatal("headless accepted a fallback on the llm backend")
		}
	})

	t.Run("log path without a reviewer", func(t *testing.T) {
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{LogPath: t.TempDir() + "/escalations.jsonl"})
		mgr := tools.NewApprovalManager(tools.NewToolPermissions())
		defer mgr.Close()
		if err := installGuardianReviewerCallbacks(cfg, mgr, true); err == nil || !strings.Contains(err.Error(), "guardian.fallback.log_path requires") {
			t.Fatalf("install error = %v", err)
		}
		if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err == nil {
			t.Fatal("headless accepted a log path without a fallback reviewer")
		}
	})

	t.Run("yolo skips guardian setup", func(t *testing.T) {
		var factoryCalls atomic.Int32
		withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
			factoryCalls.Add(1)
			return nil, errors.New("unused")
		})
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
		mgr := tools.NewApprovalManager(tools.NewToolPermissions())
		defer mgr.Close()
		if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeYolo}, approvalRuntimeOptions{Headless: true}); err != nil || !mgr.YoloEnabled() {
			t.Fatalf("yolo: %v", err)
		}
		if factoryCalls.Load() != 0 || stub.callCount() != 0 {
			t.Fatalf("yolo resolved reviewers: fallback=%d classify=%d", factoryCalls.Load(), stub.callCount())
		}
	})
}

func TestGuardianClassifyFallbackCleansReplacedPool(t *testing.T) {
	stub := &guardianFallbackClassifyStub{confidence: 0.9}
	withGuardianClassifyClient(t, stub)
	withGuardianEscalationLogger(t, &guardianEscalationStub{})
	var cleaned atomic.Int32
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
		return &delayedGuardianProvider{
			delegate: llm.NewMockProvider("mock").AddTextResponse(`{"outcome":"allow"}`),
			started:  make(chan struct{}, 1),
			release:  closedChannel(),
			cleaned:  &cleaned,
		}, nil
	})
	cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	if err := installGuardianReviewerCallbacks(cfg, mgr, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := installGuardianReviewerCallbacks(cfg, mgr, false); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := cleaned.Load(); got != 1 {
		t.Fatalf("cleanup after replacement = %d, want 1", got)
	}
	mgr.Close()
	if got := cleaned.Load(); got != 2 {
		t.Fatalf("cleanup after manager close = %d, want 2", got)
	}
}

func TestGuardianFallbackConfigKeysAndCompletion(t *testing.T) {
	for _, key := range []string{"guardian.fallback.provider", "guardian.fallback.model", "guardian.fallback.log_path"} {
		if !config.IsKnownKey(key) {
			t.Fatalf("known key missing %s", key)
		}
		if _, ok := config.GetDefaults()[key]; ok {
			t.Fatalf("%s must not carry a persisted default", key)
		}
	}
	if got := configValueCompletions("guardian.fallback.provider", "chat"); !slices.Contains(got, "chatgpt") {
		t.Fatalf("guardian.fallback.provider completions = %v, want provider names", got)
	}
}

// TestGuardianEscalationLoggerPathResolution covers the custom-path rules: a
// relative path is refused and a `~` path expands to an absolute file.
func TestGuardianEscalationLoggerPathResolution(t *testing.T) {
	t.Run("relative path disables logging", func(t *testing.T) {
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", LogPath: filepath.Join("logs", "escalations.jsonl")})
		if logger := newGuardianEscalationLogger(cfg); logger != nil {
			t.Fatalf("logger = %#v, want nil for a relative log_path", logger)
		}
	})

	t.Run("tilde path expands under home", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if resolved, err := os.UserHomeDir(); err != nil || resolved != home {
			t.Skip("platform resolves the home directory without HOME")
		}
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", LogPath: "~/.cache/guardian/escalations.jsonl"})
		fileLogger, ok := newGuardianEscalationLogger(cfg).(*guardian.FileEscalationLogger)
		if !ok {
			t.Fatal("expanded log_path did not produce a file logger")
		}
		if err := fileLogger.LogEscalation(guardian.Escalation{SchemaVersion: guardian.EscalationSchemaVersion}); err != nil {
			t.Fatalf("LogEscalation: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".cache", "guardian", "escalations.jsonl")); err != nil {
			t.Fatalf("expanded log file missing: %v", err)
		}
	})
}

// TestGuardianClassifyFallbackEscalationLogFile covers the real escalation
// logger through the wiring: the default XDG path, a custom path, and `off`.
func TestGuardianClassifyFallbackEscalationLogFile(t *testing.T) {
	escalate := func(t *testing.T) {
		t.Helper()
		withGuardianClassifyClient(t, &guardianFallbackClassifyStub{confidence: 0.4})
		withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
			return llm.NewMockProvider("fallback").AddTextResponse(`{"outcome":"allow","risk_level":"low","user_authorization":"high","rationale":"ok"}`), nil
		})
	}

	t.Run("default path", func(t *testing.T) {
		dataHome := t.TempDir()
		t.Setenv("XDG_DATA_HOME", dataHome)
		escalate(t)
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low"})
		mgr := tools.NewApprovalManager(tools.NewToolPermissions())
		defer mgr.Close()
		if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
			t.Fatal(err)
		}
		if outcome, err := mgr.CheckShellApproval("echo logged", t.TempDir()); err != nil || outcome != tools.ProceedAlways {
			t.Fatalf("outcome=%v error=%v", outcome, err)
		}

		path := filepath.Join(dataHome, "term-llm", "guardian", "escalations.jsonl")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("escalation log permissions = %o, want 600", perm)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) != 1 {
			t.Fatalf("escalation lines = %d, want 1", len(lines))
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
			t.Fatal(err)
		}
		if record["schema_version"] != float64(guardian.EscalationSchemaVersion) || record["classify_stage"] != "decision" || record["classify_request_sent"] != true {
			t.Fatalf("record = %+v", record)
		}
		request, ok := record["classify_request"].(map[string]any)
		if !ok || request["questions"] == nil {
			t.Fatalf("classify request missing from record: %+v", record)
		}
		if model, _ := request["model"].(string); model == "" {
			t.Fatalf("classify request model missing: %+v", request)
		} else if record["classify_model"] != model {
			t.Fatalf("classify_model = %v, want the reported response model %q", record["classify_model"], model)
		}
		if usage, ok := record["classify_usage"].(map[string]any); !ok || usage["input_tokens"] == nil || usage["output_tokens"] == nil {
			t.Fatalf("classify usage missing from record: %+v", record)
		}
		if usage, ok := record["fallback_usage"].(map[string]any); !ok || usage["input_tokens"] == nil || usage["output_tokens"] == nil {
			t.Fatalf("fallback usage missing from record: %+v", record)
		}
		state, ok := request["state"].(map[string]any)
		if !ok || state["action"] == nil || state["policy"] == nil {
			t.Fatalf("classify state missing from record: %+v", request)
		}
		fallbackDecision, ok := record["fallback_decision"].(map[string]any)
		if !ok || fallbackDecision["outcome"] != "allow" || record["fallback_provider"] != "chatgpt" {
			t.Fatalf("fallback verdict missing from record: %+v", record)
		}
	})

	t.Run("custom nested path", func(t *testing.T) {
		dataHome := t.TempDir()
		t.Setenv("XDG_DATA_HOME", dataHome)
		escalate(t)
		path := filepath.Join(t.TempDir(), "nested", "custom.jsonl")
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low", LogPath: path})
		mgr := tools.NewApprovalManager(tools.NewToolPermissions())
		defer mgr.Close()
		if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
			t.Fatal(err)
		}
		if outcome, err := mgr.CheckShellApproval("echo custom", t.TempDir()); err != nil || outcome != tools.ProceedAlways {
			t.Fatalf("outcome=%v error=%v", outcome, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"classify_stage":"decision"`) {
			t.Fatalf("custom log = %s", raw)
		}
		if _, err := os.Stat(filepath.Join(dataHome, "term-llm", "guardian", "escalations.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("custom log path still wrote the default location: %v", err)
		}
	})

	t.Run("off disables logging", func(t *testing.T) {
		dataHome := t.TempDir()
		t.Setenv("XDG_DATA_HOME", dataHome)
		escalate(t)
		cfg := guardianFallbackTestConfig(config.GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low", LogPath: "OFF"})
		mgr := tools.NewApprovalManager(tools.NewToolPermissions())
		defer mgr.Close()
		if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
			t.Fatal(err)
		}
		if outcome, err := mgr.CheckShellApproval("echo silent", t.TempDir()); err != nil || outcome != tools.ProceedAlways {
			t.Fatalf("outcome=%v error=%v", outcome, err)
		}
		if _, err := os.Stat(filepath.Join(dataHome, "term-llm", "guardian", "escalations.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("disabled logging still wrote a record: %v", err)
		}
	})
}
