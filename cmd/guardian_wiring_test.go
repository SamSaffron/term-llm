package cmd

import (
	"context"
	"strings"
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

func TestInstallGuardianReviewerCallbacksDoesNotActivateModeButSupportsLaterAutoToggle(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse(`{"risk_level":"high","user_authorization":"low","outcome":"deny","rationale":"credential probing"}`)
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}

	if err := installGuardianReviewerCallbacks(cfg, mgr, provider, nil, "mock-model", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if mgr.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("mode = %v, want prompt", mgr.ApprovalMode())
	}
	if mgr.PolicyReviewFunc == nil {
		t.Fatal("PolicyReviewFunc was not installed")
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
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{
		DefaultProvider: "mock",
		Guardian:        config.GuardianConfig{TimeoutSeconds: 7},
		Providers:       map[string]config.ProviderConfig{"mock": {Model: "mock-model"}},
	}

	if err := installGuardianReviewerCallbacks(cfg, mgr, provider, nil, "mock-model", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if _, err := mgr.PolicyReviewFunc(context.Background(), tools.PolicyReviewRequest{Command: "echo ok"}); err != nil {
		t.Fatalf("PolicyReviewFunc: %v", err)
	}
	assertDeadlineNear(t, provider.deadline, provider.ok, 7*time.Second)
}

func TestInstallGuardianReviewerCallbacksUsesDefaultTimeoutWhenUnset(t *testing.T) {
	provider := &deadlineCapturingProvider{delegate: llm.NewMockProvider("mock").AddTextResponse(`{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"safe"}`)}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}

	if err := installGuardianReviewerCallbacks(cfg, mgr, provider, nil, "mock-model", true); err != nil {
		t.Fatalf("installGuardianReviewerCallbacks: %v", err)
	}
	if _, err := mgr.PolicyReviewFunc(context.Background(), tools.PolicyReviewRequest{Command: "echo ok"}); err != nil {
		t.Fatalf("PolicyReviewFunc: %v", err)
	}
	assertDeadlineNear(t, provider.deadline, provider.ok, guardian.DefaultTimeout)
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

func TestResolveGuardianModelNameUsesGuardianProviderConfig(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "anthropic-main",
		Guardian:        config.GuardianConfig{Provider: "openai-guardian"},
		Providers: map[string]config.ProviderConfig{
			"anthropic-main":  {Model: "claude-main", FastModel: "claude-fast"},
			"openai-guardian": {Type: config.ProviderTypeOpenAI, Model: "gpt-guardian", FastModel: "gpt-fast"},
		},
	}
	if got := resolveGuardianModelName(cfg, "claude-main"); got != "gpt-guardian" {
		t.Fatalf("model = %q, want guardian provider model", got)
	}
}

func TestResolveGuardianModelNameExplicitOverrideWins(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "anthropic-main",
		Guardian:        config.GuardianConfig{Provider: "openai-guardian", Model: "explicit-guardian"},
		Providers: map[string]config.ProviderConfig{
			"anthropic-main":  {Model: "claude-main"},
			"openai-guardian": {Type: config.ProviderTypeOpenAI, Model: "gpt-guardian"},
		},
	}
	if got := resolveGuardianModelName(cfg, "claude-main"); got != "explicit-guardian" {
		t.Fatalf("model = %q, want explicit guardian model", got)
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
