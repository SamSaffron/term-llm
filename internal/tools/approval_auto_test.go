package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func newApprovalAutoTestManager(perms *ToolPermissions) *ApprovalManager {
	mgr := NewApprovalManager(perms)
	mgr.IgnoreProjectApprovals = true
	return mgr
}

func TestApprovalManagerApprovalModeParentInheritance(t *testing.T) {
	parent := newApprovalAutoTestManager(NewToolPermissions())
	child := newApprovalAutoTestManager(NewToolPermissions())
	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if child.ApprovalMode() != ModePrompt {
		t.Fatalf("initial mode = %v, want prompt", child.ApprovalMode())
	}
	parent.SetApprovalMode(ModeAuto)
	if child.ApprovalMode() != ModeAuto {
		t.Fatalf("child mode = %v, want auto", child.ApprovalMode())
	}
	parent.SetApprovalMode(ModeYolo)
	if !child.YoloEnabled() {
		t.Fatal("expected yolo inheritance")
	}
}

func TestApprovalManagerAutoReviewerOnlyAfterDeterministicMiss(t *testing.T) {
	perms := NewToolPermissions()
	perms.ShellAllow = []string{"git *"}
	if err := perms.CompileShellPatterns(); err != nil {
		t.Fatal(err)
	}
	mgr := newApprovalAutoTestManager(perms)
	mgr.SetApprovalMode(ModeAuto)
	calls := 0
	mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}
	outcome, err := mgr.CheckShellApproval("git status", "")
	if err != nil || outcome != ProceedOnce {
		t.Fatalf("deterministic outcome = %v, err=%v", outcome, err)
	}
	if calls != 0 {
		t.Fatalf("reviewer called %d times on deterministic allow", calls)
	}
	outcome, err = mgr.CheckShellApproval("echo hello", "")
	if err != nil || outcome != ProceedAlways {
		t.Fatalf("guardian outcome = %v, err=%v", outcome, err)
	}
	if calls != 1 {
		t.Fatalf("reviewer calls = %d, want 1", calls)
	}
	outcome, err = mgr.CheckShellApproval("echo hello", "")
	if err != nil || outcome != ProceedAlways {
		t.Fatalf("guardian exact-cache outcome = %v, err=%v", outcome, err)
	}
	if calls != 1 {
		t.Fatalf("reviewer calls after exact cache = %d, want 1", calls)
	}
	outcome, err = mgr.CheckShellApproval("echo goodbye", "")
	if err != nil || outcome != ProceedAlways {
		t.Fatalf("guardian second outcome = %v, err=%v", outcome, err)
	}
	if calls != 2 {
		t.Fatalf("guardian exact cache widened to a different command; calls = %d, want 2", calls)
	}
}

func TestApprovalManagerGuardianExactCacheDoesNotTreatStarAsPattern(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	calls := 0
	mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}
	if outcome, err := mgr.CheckShellApproval("git add *", ""); err != nil || outcome != ProceedAlways {
		t.Fatalf("first approval = %v, %v", outcome, err)
	}
	if outcome, err := mgr.CheckShellApproval("git add secret.txt", ""); err != nil || outcome != ProceedAlways {
		t.Fatalf("second approval = %v, %v", outcome, err)
	}
	if calls != 2 {
		t.Fatalf("guardian exact cache treated '*' as a pattern; calls = %d, want 2", calls)
	}
}

func TestApprovalManagerGuardianCircuitBreakerTripsParentFromChild(t *testing.T) {
	parent := newApprovalAutoTestManager(NewToolPermissions())
	parent.SetApprovalMode(ModeAuto)
	parent.SetAutoHeadless(true)
	parent.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
	}
	child := newApprovalAutoTestManager(NewToolPermissions())
	if err := child.SetParent(parent); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, _ = child.CheckShellApproval("bad command", "")
	}
	if got := parent.ApprovalMode(); got != ModePrompt {
		t.Fatalf("parent mode after child denials = %v, want prompt", got)
	}
	if got := child.ApprovalMode(); got != ModePrompt {
		t.Fatalf("child effective mode after circuit breaker = %v, want prompt", got)
	}
}

func TestApprovalManagerAutoReviewerFailureFallback(t *testing.T) {
	reviewErr := errors.New("bad json")
	for _, tt := range []struct {
		name     string
		headless bool
		wantErr  bool
	}{
		{name: "interactive falls back to prompt", headless: false, wantErr: false},
		{name: "headless denies", headless: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newApprovalAutoTestManager(NewToolPermissions())
			mgr.SetApprovalMode(ModeAuto)
			mgr.SetAutoHeadless(tt.headless)
			mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
				return PolicyDecision{}, reviewErr
			}
			mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
				return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
			}
			outcome, err := mgr.CheckShellApproval("echo hi", "")
			if tt.wantErr {
				if err == nil || outcome != Cancel {
					t.Fatalf("outcome=%v err=%v, want denial", outcome, err)
				}
				return
			}
			if err != nil || outcome != ProceedOnce {
				t.Fatalf("outcome=%v err=%v, want prompt fallback allow", outcome, err)
			}
		})
	}
}

func TestApprovalManagerAutoDenialPromptsHumanInInteractiveMode(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	calls := 0
	mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: false, Rationale: "not requested"}, nil
	}
	prompted := false
	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
		prompted = true
		return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
	}
	outcome, err := mgr.CheckShellApproval("rm -rf important", "")
	if err != nil || outcome != ProceedOnce {
		t.Fatalf("outcome=%v err=%v, want human prompt allow", outcome, err)
	}
	if !prompted {
		t.Fatal("expected guardian denial to escalate to human prompt")
	}
	if calls != 1 {
		t.Fatalf("guardian calls = %d, want 1 before human prompt", calls)
	}
}

func TestApprovalManagerAutoDenialIncludesNoWorkaroundsInHeadlessMode(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetAutoHeadless(true)
	mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, Rationale: "not requested"}, nil
	}
	outcome, err := mgr.CheckShellApproval("rm -rf important", "")
	if outcome != Cancel || err == nil {
		t.Fatalf("outcome=%v err=%v, want denial", outcome, err)
	}
	if !strings.Contains(err.Error(), "Do not attempt to achieve this outcome via workarounds") {
		t.Fatalf("denial missing workaround warning: %v", err)
	}
}

func TestApprovalManagerGuardianExactCacheIsScopedToWorkdir(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	calls := 0
	mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}
	if outcome, err := mgr.CheckShellApproval("rm -rf ./build", t.TempDir()); err != nil || outcome != ProceedAlways {
		t.Fatalf("first approval = %v, %v", outcome, err)
	}
	if outcome, err := mgr.CheckShellApproval("rm -rf ./build", t.TempDir()); err != nil || outcome != ProceedAlways {
		t.Fatalf("second approval = %v, %v", outcome, err)
	}
	if calls != 2 {
		t.Fatalf("guardian exact cache ignored workdir; calls = %d, want 2", calls)
	}
}

func TestApprovalManagerGuardianExactCacheClearedWhenLeavingAuto(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	calls := 0
	mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}
	workDir := t.TempDir()
	if outcome, err := mgr.CheckShellApproval("echo cached", workDir); err != nil || outcome != ProceedAlways {
		t.Fatalf("guardian approval = %v, %v", outcome, err)
	}
	mgr.SetApprovalMode(ModePrompt)
	prompted := false
	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, wd string) (ApprovalResult, error) {
		prompted = true
		return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
	}
	if outcome, err := mgr.CheckShellApproval("echo cached", workDir); err != nil || outcome != ProceedOnce {
		t.Fatalf("prompt outcome after leaving auto = %v, %v", outcome, err)
	}
	if !prompted {
		t.Fatal("expected prompt after leaving auto; guardian exact cache should not apply")
	}
	if calls != 1 {
		t.Fatalf("guardian calls after leaving auto = %d, want 1", calls)
	}
}

func TestApprovalManagerNestedChildFindsRootGuardianCallbacks(t *testing.T) {
	root := newApprovalAutoTestManager(NewToolPermissions())
	root.SetApprovalMode(ModeAuto)
	calls := 0
	root.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}
	child := newApprovalAutoTestManager(NewToolPermissions())
	if err := child.SetParent(root); err != nil {
		t.Fatal(err)
	}
	grandchild := newApprovalAutoTestManager(NewToolPermissions())
	if err := grandchild.SetParent(child); err != nil {
		t.Fatal(err)
	}
	if outcome, err := grandchild.CheckShellApproval("echo nested", t.TempDir()); err != nil || outcome != ProceedAlways {
		t.Fatalf("nested approval = %v, %v", outcome, err)
	}
	if calls != 1 {
		t.Fatalf("root guardian callback calls = %d, want 1", calls)
	}
}

func TestShellApprovalTranscriptIncludesToolCallsResultsAndApprovalRole(t *testing.T) {
	args, err := json.Marshal(map[string]string{"command": "cat .env"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{
		{Role: llm.RoleUser, ApprovalRole: "parent_agent_task", Parts: []llm.Part{{Type: llm.PartText, Text: "inspect env"}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "shell", Arguments: args}}}},
		llm.ToolResultMessage("call-1", "shell", "SECRET=value", nil),
	}
	entries := shellApprovalTranscriptFromContext(llm.ContextWithApprovalTranscript(context.Background(), msgs))
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %#v", len(entries), entries)
	}
	if entries[0].Role != "parent_agent_task" {
		t.Fatalf("first role = %q, want parent_agent_task", entries[0].Role)
	}
	if !strings.Contains(entries[1].Text, `tool_call name="shell"`) || !strings.Contains(entries[1].Text, `"command": "cat .env"`) {
		t.Fatalf("tool call missing from transcript: %#v", entries[1])
	}
	if !strings.Contains(entries[2].Text, `tool_result name="shell"`) || !strings.Contains(entries[2].Text, "SECRET=value") {
		t.Fatalf("tool result missing from transcript: %#v", entries[2])
	}
}

func TestApprovalManagerGuardianContradictoryAllowEscalatesOrDenies(t *testing.T) {
	for _, tt := range []struct {
		name     string
		headless bool
	}{
		{name: "interactive escalates", headless: false},
		{name: "headless denies", headless: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newApprovalAutoTestManager(NewToolPermissions())
			mgr.SetApprovalMode(ModeAuto)
			mgr.SetAutoHeadless(tt.headless)
			mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
				return PolicyDecision{Allowed: true, RiskLevel: "critical", UserAuthorization: "unknown", Rationale: "too risky"}, nil
			}
			prompted := false
			mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
				prompted = true
				return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
			}
			outcome, err := mgr.CheckShellApproval("rm -rf /", t.TempDir())
			if tt.headless {
				if err == nil || outcome != Cancel {
					t.Fatalf("outcome=%v err=%v, want headless denial", outcome, err)
				}
				return
			}
			if err != nil || outcome != ProceedOnce || !prompted {
				t.Fatalf("outcome=%v err=%v prompted=%v, want human escalation", outcome, err, prompted)
			}
		})
	}
}

func TestApprovalManagerGuardianReceivesApprovalContext(t *testing.T) {
	writeDir := t.TempDir()
	perms := NewToolPermissions()
	if err := perms.AddWriteDir(writeDir); err != nil {
		t.Fatal(err)
	}
	mgr := newApprovalAutoTestManager(perms)
	mgr.SetApprovalMode(ModeAuto)
	var got PolicyReviewRequest
	mgr.PolicyReviewFunc = func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		got = req
		return PolicyDecision{Allowed: true, RiskLevel: "medium", UserAuthorization: "high", Rationale: "equivalent approved write"}, nil
	}
	if outcome, err := mgr.CheckShellApproval("cat >> file.go <<'EOF'\nhi\nEOF", writeDir); err != nil || outcome != ProceedAlways {
		t.Fatalf("approval = %v, %v", outcome, err)
	}
	if !strings.Contains(got.ApprovalContext, "configured_write_dir") || !strings.Contains(got.ApprovalContext, writeDir) {
		t.Fatalf("approval context missing write dir %q:\n%s", writeDir, got.ApprovalContext)
	}
	if !strings.Contains(got.ApprovalContext, "narrow equivalent") {
		t.Fatalf("approval context missing equivalence guidance:\n%s", got.ApprovalContext)
	}
}
