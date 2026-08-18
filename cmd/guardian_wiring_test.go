package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

type deadlineCapturingProvider struct {
	delegate *llm.MockProvider
	deadline time.Time
	ok       bool
}

type delayedGuardianProvider struct {
	delegate *llm.MockProvider
	started  chan<- struct{}
	release  <-chan struct{}
	cleaned  *atomic.Int32
}

func (p *delayedGuardianProvider) Name() string                   { return "delayed-guardian" }
func (p *delayedGuardianProvider) Credential() string             { return "mock" }
func (p *delayedGuardianProvider) Capabilities() llm.Capabilities { return p.delegate.Capabilities() }
func (p *delayedGuardianProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	select {
	case p.started <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return p.delegate.Stream(ctx, req)
}
func (p *delayedGuardianProvider) CleanupMCP() {
	if p.cleaned != nil {
		p.cleaned.Add(1)
	}
}

func withGuardianProviderFactory(t *testing.T, fn func(*config.Config, string, string) (llm.Provider, error)) {
	t.Helper()
	orig := newGuardianProviderByName
	newGuardianProviderByName = fn
	t.Cleanup(func() { newGuardianProviderByName = orig })
}

func (p *deadlineCapturingProvider) Name() string { return "deadline-capturing" }
func (p *deadlineCapturingProvider) Credential() string {
	return "mock"
}
func (p *deadlineCapturingProvider) Capabilities() llm.Capabilities {
	return p.delegate.Capabilities()
}
func (p *deadlineCapturingProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.deadline, p.ok = ctx.Deadline()
	return p.delegate.Stream(ctx, req)
}

func TestPreflightHeadlessApproval(t *testing.T) {
	calls := 0
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
		calls++
		return nil, errors.New("guardian unavailable")
	})
	cfg := &config.Config{DefaultProvider: "mock"}
	if err := preflightHeadlessApproval(cfg, resolvedApprovalMode{Mode: tools.ModePrompt}, "mock", "model"); err != nil {
		t.Fatalf("prompt preflight: %v", err)
	}
	if calls != 0 {
		t.Fatalf("prompt preflight initialized guardian %d times", calls)
	}
	if err := preflightHeadlessApproval(cfg, resolvedApprovalMode{Mode: tools.ModeAuto}, "mock", "model"); err == nil || !strings.Contains(err.Error(), "auto approval unavailable") {
		t.Fatalf("auto preflight error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("auto preflight guardian calls = %d, want 1", calls)
	}
}

func TestApplyResolvedApprovalModeInteractiveFailureWarnsOnceAndFallsBack(t *testing.T) {
	calls := 0
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
		calls++
		return nil, errors.New("guardian unavailable")
	})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	var warnings bytes.Buffer
	resolved := resolvedApprovalMode{Mode: tools.ModeAuto, Source: approvalModeSourceBuiltinDefault}

	if err := applyResolvedApprovalMode(&config.Config{DefaultProvider: "mock"}, mgr, resolved, "mock", "model", approvalRuntimeOptions{WarningWriter: &warnings}); err != nil {
		t.Fatalf("applyResolvedApprovalMode() error = %v", err)
	}
	if mgr.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("actual mode = %v, want prompt fallback", mgr.ApprovalMode())
	}
	if resolved.Mode != tools.ModeAuto {
		t.Fatalf("requested mode mutated to %v, want auto", resolved.Mode)
	}
	if calls != 1 || strings.Count(warnings.String(), "guardian auto-approval unavailable") != 1 {
		t.Fatalf("calls=%d warnings=%q, want one initialization and warning", calls, warnings.String())
	}
}

func TestApplyResolvedApprovalModePromptPreparationFailureWarnsOnce(t *testing.T) {
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
		return nil, errors.New("guardian unavailable")
	})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	var warnings bytes.Buffer
	resolved := resolvedApprovalMode{Mode: tools.ModePrompt, Source: approvalModeSourceSession}

	if err := applyResolvedApprovalMode(&config.Config{DefaultProvider: "mock"}, mgr, resolved, "mock", "model", approvalRuntimeOptions{PrepareCallbacks: true, WarningWriter: &warnings}); err != nil {
		t.Fatalf("applyResolvedApprovalMode() error = %v", err)
	}
	if mgr.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("actual mode = %v, want prompt", mgr.ApprovalMode())
	}
	if strings.Count(warnings.String(), "auto toggle disabled") != 1 {
		t.Fatalf("warnings = %q, want one auto-toggle warning", warnings.String())
	}
}

func TestApplyResolvedApprovalModeHeadlessFailureIsStartupError(t *testing.T) {
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
		return nil, errors.New("guardian unavailable")
	})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	resolved := resolvedApprovalMode{Mode: tools.ModeAuto, Source: approvalModeSourceBuiltinDefault}

	err := applyResolvedApprovalMode(&config.Config{DefaultProvider: "mock"}, mgr, resolved, "mock", "model", approvalRuntimeOptions{Headless: true})
	if err == nil || !strings.Contains(err.Error(), "auto approval unavailable") {
		t.Fatalf("applyResolvedApprovalMode() error = %v, want startup error", err)
	}
	if mgr.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("actual mode = %v, want prompt after failed startup", mgr.ApprovalMode())
	}
}

func TestInstallGuardianReviewerCallbacksDoesNotActivateModeButSupportsLaterAutoToggle(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse(`{"risk_level":"high","user_authorization":"low","outcome":"deny","rationale":"credential probing"}`)
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}

	if err := installGuardianReviewerCallbacks(cfg, mgr, "mock", "mock-model", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if mgr.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("mode = %v, want prompt", mgr.ApprovalMode())
	}
	if !mgr.GuardianReviewerAvailable() {
		t.Fatal("policy reviewer was not installed")
	}

	mgr.SetApprovalMode(tools.ModeAuto)
	outcome, err := mgr.CheckShellApproval("cat ~/.ssh/id_rsa", t.TempDir())
	if outcome != tools.Cancel || err == nil {
		t.Fatalf("outcome=%v err=%v, want guardian denial", outcome, err)
	}
	if !strings.Contains(err.Error(), "credential probing") {
		t.Fatalf("denial error = %v, want guardian rationale", err)
	}
}

func TestInstallGuardianReviewerCallbacksAppliesConfiguredTimeout(t *testing.T) {
	provider := &deadlineCapturingProvider{delegate: llm.NewMockProvider("mock").AddTextResponse(`{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"safe"}`)}
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{
		DefaultProvider: "mock",
		Guardian:        config.GuardianConfig{TimeoutSeconds: 7},
		Providers:       map[string]config.ProviderConfig{"mock": {Model: "mock-model"}},
	}

	if err := installGuardianReviewerCallbacks(cfg, mgr, "mock", "mock-model", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if _, err := mgr.ReviewPolicy(context.Background(), tools.PolicyReviewRequest{Command: "echo ok"}); err != nil {
		t.Fatalf("PolicyReviewFunc: %v", err)
	}
	assertDeadlineNear(t, provider.deadline, provider.ok, 7*time.Second)
}

func TestInstallGuardianReviewerCallbacksUsesDefaultTimeoutWhenUnset(t *testing.T) {
	provider := &deadlineCapturingProvider{delegate: llm.NewMockProvider("mock").AddTextResponse(`{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"safe"}`)}
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}

	if err := installGuardianReviewerCallbacks(cfg, mgr, "mock", "mock-model", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	defer mgr.Close()
	if _, err := mgr.ReviewPolicy(context.Background(), tools.PolicyReviewRequest{Command: "echo ok"}); err != nil {
		t.Fatalf("PolicyReviewFunc: %v", err)
	}
	assertDeadlineNear(t, provider.deadline, provider.ok, guardian.DefaultTimeout)
}

func TestInstallGuardianReviewerCallbacksRunsParallelReviewsConcurrently(t *testing.T) {
	started := make(chan struct{}, guardianReviewerPoolSize)
	release := make(chan struct{})
	peerFactoryEntered := make(chan struct{})
	var factoryCalls atomic.Int32
	var activeFactoryCalls atomic.Int32
	var maxActiveFactoryCalls atomic.Int32
	var cleaned atomic.Int32
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
		call := factoryCalls.Add(1)
		active := activeFactoryCalls.Add(1)
		defer activeFactoryCalls.Add(-1)
		for max := maxActiveFactoryCalls.Load(); active > max && !maxActiveFactoryCalls.CompareAndSwap(max, active); max = maxActiveFactoryCalls.Load() {
		}
		// Hold the first lazy expansion briefly to give the other expansion a
		// deterministic chance to overlap if provider construction is not serialized.
		if call == 2 {
			select {
			case <-peerFactoryEntered:
			case <-time.After(100 * time.Millisecond):
			}
		} else if call == 3 {
			close(peerFactoryEntered)
		}
		return &delayedGuardianProvider{
			delegate: llm.NewMockProvider("mock").AddTextResponse(`{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"safe"}`),
			started:  started,
			release:  release,
			cleaned:  &cleaned,
		}, nil
	})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	if err := installGuardianReviewerCallbacks(cfg, mgr, "mock", "mock-model", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("provider factory calls at install = %d, want one eager primary", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errs := make(chan error, guardianReviewerPoolSize)
	for _, command := range []string{"echo one", "echo two", "echo three"} {
		go func(command string) {
			decision, err := mgr.ReviewPolicy(ctx, tools.PolicyReviewRequest{Command: command})
			if err == nil && !decision.Allowed {
				err = errors.New("guardian unexpectedly denied command")
			}
			errs <- err
		}(command)
	}
	for i := 0; i < guardianReviewerPoolSize; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatalf("only %d parallel reviews reached providers: %v", i, ctx.Err())
		}
	}
	if got := factoryCalls.Load(); got != guardianReviewerPoolSize {
		t.Fatalf("provider factory calls under contention = %d, want %d", got, guardianReviewerPoolSize)
	}
	if got := maxActiveFactoryCalls.Load(); got != 1 {
		t.Fatalf("concurrent provider factory calls = %d, want serialized construction", got)
	}
	close(release)
	for i := 0; i < guardianReviewerPoolSize; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("PolicyReviewFunc: %v", err)
		}
	}
	mgr.Close()
	if got := cleaned.Load(); got != guardianReviewerPoolSize {
		t.Fatalf("provider cleanup calls = %d, want %d", got, guardianReviewerPoolSize)
	}
}

func TestInstallGuardianReviewerCallbacksCleansReplacedPool(t *testing.T) {
	var cleaned atomic.Int32
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) {
		return &delayedGuardianProvider{
			delegate: llm.NewMockProvider("mock").AddTextResponse(`{"outcome":"allow"}`),
			started:  make(chan struct{}, 1),
			release:  closedChannel(),
			cleaned:  &cleaned,
		}, nil
	})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{DefaultProvider: "mock"}
	if err := installGuardianReviewerCallbacks(cfg, mgr, "mock", "first", false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := installGuardianReviewerCallbacks(cfg, mgr, "mock", "second", false); err != nil {
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

func closedChannel() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestInstallGuardianReviewerCallbacksUsesPassedProviderNameWhenGuardianUnset(t *testing.T) {
	guardianProvider := llm.NewMockProvider("guardian").AddTextResponse(`{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"safe"}`)
	var gotName, gotModel string
	withGuardianProviderFactory(t, func(_ *config.Config, name, model string) (llm.Provider, error) {
		gotName = name
		gotModel = model
		return guardianProvider, nil
	})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{
		DefaultProvider: "configured-default",
		Providers: map[string]config.ProviderConfig{
			"configured-default": {Model: "default-model"},
			"active-provider":    {Model: "active-model"},
		},
	}

	if err := installGuardianReviewerCallbacks(cfg, mgr, "active-provider", "active-model", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if gotName != "active-provider" || gotModel != "active-model" {
		t.Fatalf("factory called with (%q, %q), want (active-provider, active-model)", gotName, gotModel)
	}
}

func TestInstallGuardianReviewerCallbacksUsesDedicatedProviderInstance(t *testing.T) {
	mainProvider := llm.NewMockProvider("main")
	guardianProvider := llm.NewMockProvider("guardian").AddTextResponse(`{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"safe"}`)
	var gotName, gotModel string
	withGuardianProviderFactory(t, func(_ *config.Config, name, model string) (llm.Provider, error) {
		gotName = name
		gotModel = model
		return guardianProvider, nil
	})
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{DefaultProvider: "claude-bin", Providers: map[string]config.ProviderConfig{"claude-bin": {Type: config.ProviderTypeClaudeBin, Model: "sonnet"}}}

	if err := installGuardianReviewerCallbacks(cfg, mgr, "claude-bin", "sonnet", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if _, err := mgr.ReviewPolicy(context.Background(), tools.PolicyReviewRequest{Command: "echo ok"}); err != nil {
		t.Fatalf("PolicyReviewFunc: %v", err)
	}
	if gotName != "claude-bin" || gotModel != "sonnet" {
		t.Fatalf("factory called with (%q, %q), want (claude-bin, sonnet)", gotName, gotModel)
	}
	if len(mainProvider.Requests) != 0 {
		t.Fatalf("main provider received guardian request: %#v", mainProvider.Requests)
	}
	if len(guardianProvider.Requests) != 1 {
		t.Fatalf("guardian provider requests = %d, want 1", len(guardianProvider.Requests))
	}
	if guardianProvider.Requests[0].Ephemeral {
		t.Fatalf("guardian request Ephemeral = true, want false for isolated process-local review session")
	}
}

func assertDeadlineNear(t *testing.T, deadline time.Time, ok bool, want time.Duration) {
	t.Helper()
	if !ok {
		t.Fatal("review context had no deadline")
	}
	remaining := time.Until(deadline)
	if remaining < want-2*time.Second || remaining > want+2*time.Second {
		t.Fatalf("deadline remaining = %v, want about %v", remaining, want)
	}
}

func TestApplyResolvedApprovalModeWiresClassifyAllShell(t *testing.T) {
	provider := llm.NewMockProvider("guardian").AddTextResponse(`{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"safe"}`)
	withGuardianProviderFactory(t, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil })
	perms := tools.NewToolPermissions()
	if err := perms.AddShellPattern("git status"); err != nil {
		t.Fatal(err)
	}
	mgr := tools.NewApprovalManager(perms)
	cfg := &config.Config{
		Guardian:  config.GuardianConfig{ClassifyAllShell: true},
		Providers: map[string]config.ProviderConfig{"active": {Model: "main", FastModel: "fast"}},
	}
	if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, "active", "main", approvalRuntimeOptions{}); err != nil {
		t.Fatalf("applyResolvedApprovalMode: %v", err)
	}
	if outcome, err := mgr.CheckShellApproval("git status", t.TempDir()); err != nil || outcome != tools.ProceedAlways {
		t.Fatalf("classified narrow pattern = %v, %v", outcome, err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("Guardian requests = %d, want 1", len(provider.Requests))
	}
}

func TestInstallGuardianReviewerCallbacksUsesFastProviderPair(t *testing.T) {
	provider := llm.NewMockProvider("guardian").AddTextResponse(`{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"safe"}`)
	var gotName, gotModel string
	withGuardianProviderFactory(t, func(_ *config.Config, name, model string) (llm.Provider, error) {
		gotName, gotModel = name, model
		return provider, nil
	})
	cfg := &config.Config{
		Guardian: config.GuardianConfig{Provider: "main"},
		Providers: map[string]config.ProviderConfig{
			"main": {Model: "large", FastProvider: "fast", FastModel: "small"},
			"fast": {Model: "other"},
		},
	}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	if err := installGuardianReviewerCallbacks(cfg, mgr, "main", "large", false); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if gotName != "fast" || gotModel != "small" {
		t.Fatalf("factory target = %s:%s, want fast:small", gotName, gotModel)
	}
}

func TestResolveGuardianTarget(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *config.Config
		activeProvider string
		activeModel    string
		want           guardianTarget
		wantErr        string
	}{
		{
			name: "explicit model prevents fast provider switch",
			cfg: &config.Config{
				Guardian:  config.GuardianConfig{Provider: "main", Model: "pinned"},
				Providers: map[string]config.ProviderConfig{"main": {FastProvider: "fast", FastModel: "small"}},
			},
			want: guardianTarget{Provider: "main", Model: "pinned"},
		},
		{
			name: "same-provider fast model",
			cfg: &config.Config{
				Guardian:  config.GuardianConfig{Provider: "main"},
				Providers: map[string]config.ProviderConfig{"main": {Model: "large", FastModel: "small"}},
			},
			want: guardianTarget{Provider: "main", Model: "small"},
		},
		{
			name: "fast provider pair",
			cfg: &config.Config{
				Guardian: config.GuardianConfig{Provider: "main"},
				Providers: map[string]config.ProviderConfig{
					"main": {Model: "large", FastProvider: "fast", FastModel: "small"},
					"fast": {Model: "other"},
				},
			},
			want: guardianTarget{Provider: "fast", Model: "small"},
		},
		{
			name: "normal model fallback",
			cfg: &config.Config{
				Guardian:  config.GuardianConfig{Provider: "custom"},
				Providers: map[string]config.ProviderConfig{"custom": {Model: "normal"}},
			},
			want: guardianTarget{Provider: "custom", Model: "normal"},
		},
		{
			name: "built-in fast fallback",
			cfg:  &config.Config{Guardian: config.GuardianConfig{Provider: "openai"}},
			want: guardianTarget{Provider: "openai", Model: llm.ProviderFastModels["openai"]},
		},
		{
			name:           "compatible active fallback",
			cfg:            &config.Config{},
			activeProvider: "custom-active",
			activeModel:    "active-model",
			want:           guardianTarget{Provider: "custom-active", Model: "active-model"},
		},
		{
			name:           "incompatible provider error",
			cfg:            &config.Config{Guardian: config.GuardianConfig{Provider: "custom-guardian"}},
			activeProvider: "custom-active",
			activeModel:    "active-model",
			wantErr:        "guardian provider \"custom-guardian\" has no configured model",
		},
		{
			name: "default provider selected when active absent",
			cfg: &config.Config{
				DefaultProvider: "default",
				Providers:       map[string]config.ProviderConfig{"default": {FastModel: "default-fast"}},
			},
			want: guardianTarget{Provider: "default", Model: "default-fast"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveGuardianTarget(tt.cfg, tt.activeProvider, tt.activeModel)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("target = %#v, err=%v; want %#v", got, err, tt.want)
			}
		})
	}
}

func TestSubagentApprovalTranscriptPrefixMarksParentEvidence(t *testing.T) {
	parent := []llm.Message{
		llm.UserText("please run tests"),
		llm.AssistantText("I will delegate"),
	}
	prefix := subagentApprovalTranscriptPrefix(llm.ContextWithApprovalTranscript(context.Background(), parent))
	if len(prefix) != 2 {
		t.Fatalf("prefix len = %d, want 2", len(prefix))
	}
	if prefix[0].ApprovalRole != "parent_user" {
		t.Fatalf("first approval role = %q, want parent_user", prefix[0].ApprovalRole)
	}
	if prefix[1].ApprovalRole != "parent_assistant" {
		t.Fatalf("second approval role = %q, want parent_assistant", prefix[1].ApprovalRole)
	}
}
