package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func newApprovalAutoTestManager(perms *ToolPermissions) *ApprovalManager {
	mgr := NewApprovalManager(perms)
	mgr.IgnoreProjectApprovals = true
	return mgr
}

func TestApprovalManagerAutoReviewerHandlesUnmatchedRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guardian-read.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	reviewCalls := 0
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		reviewCalls++
		if req.ToolName != ReadFileToolName || req.Path != path || req.IsWrite || req.Command != "" {
			t.Fatalf("read review request = %#v", req)
		}
		if len(req.Transcript) != 1 || req.Transcript[0] != (TranscriptEntry{Role: "user", Text: "inspect the generated report"}) {
			t.Fatalf("read review transcript = %#v", req.Transcript)
		}
		if !strings.Contains(req.ApprovalContext, `file_operation="read"`) || !strings.Contains(req.ApprovalContext, `file_path="`+path+`"`) {
			t.Fatalf("read approval context = %q", req.ApprovalContext)
		}
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high", Rationale: "requested inspection"}, nil
	}, nil)
	mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		t.Fatal("human prompt called after guardian approved read")
		return ApprovalResult{}, nil
	}
	var event GuardianEvent
	mgr.GuardianEventFunc = func(got GuardianEvent) { event = got }
	ctx := llm.ContextWithCallID(context.Background(), "read-call")
	ctx = llm.ContextWithApprovalTranscript(ctx, []llm.Message{llm.UserText("inspect the generated report")})

	outcome, err := mgr.CheckPathApprovalWithContext(ctx, ReadFileToolName, path, path, false)
	if err != nil || outcome != ProceedOnce {
		t.Fatalf("read approval = %v, %v", outcome, err)
	}
	if event.ToolCallID != "read-call" || event.ToolName != ReadFileToolName || event.Path != path || event.IsWrite || event.Outcome != GuardianApproved {
		t.Fatalf("read guardian event = %#v", event)
	}
	if outcome, err := mgr.CheckPathApprovalWithContext(ctx, ReadFileToolName, path, path, false); err != nil || outcome != ProceedOnce {
		t.Fatalf("repeated read approval = %v, %v", outcome, err)
	}
	if reviewCalls != 2 {
		t.Fatalf("one-shot read reviewer calls = %d, want 2", reviewCalls)
	}
}

func TestApprovalManagerAutoReadDenialEscalatesToHuman(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, RiskLevel: "high", UserAuthorization: "low", Rationale: "unrelated sensitive file"}, nil
	}, nil)
	prompted := false
	mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		prompted = true
		return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
	}

	outcome, err := mgr.CheckPathApprovalWithContext(context.Background(), ReadFileToolName, path, path, false)
	if err != nil || outcome != ProceedOnce || !prompted {
		t.Fatalf("denied read fallback = outcome %v, prompted %v, err %v", outcome, prompted, err)
	}
	mgr.guardianMu.RLock()
	denials := mgr.guardianConsecutiveDenials
	mgr.guardianMu.RUnlock()
	if denials != 0 {
		t.Fatalf("guardian denials after human approval = %d, want reset", denials)
	}
}

func TestApprovalManagerAutoReviewerHandlesUnmatchedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "write.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		if req.ToolName != WriteFileToolName || req.Path != path || !req.IsWrite {
			t.Fatalf("write review request = %#v", req)
		}
		if !strings.Contains(req.ApprovalContext, `file_operation="write"`) || !strings.Contains(req.ApprovalContext, `file_path="`+path+`"`) {
			t.Fatalf("write approval context = %q", req.ApprovalContext)
		}
		return PolicyDecision{Allowed: true, RiskLevel: "medium", UserAuthorization: "high", Rationale: "requested edit"}, nil
	}, nil)
	mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		t.Fatal("human prompt called after guardian approved write")
		return ApprovalResult{}, nil
	}

	outcome, err := mgr.CheckPathApprovalWithContext(context.Background(), WriteFileToolName, path, path, true)
	if err != nil || outcome != ProceedOnce {
		t.Fatalf("write approval = outcome %v, err %v", outcome, err)
	}
}

func TestApprovalManagerAutoReviewerReceivesDirectorySelector(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		if req.ToolName != GrepToolName || req.Path != dir || req.Selector != "AWS_SECRET_ACCESS_KEY" || !req.IsDirectory {
			t.Fatalf("directory review request = %#v", req)
		}
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high"}, nil
	}, nil)

	if outcome, err := mgr.CheckPathApprovalWithContext(context.Background(), GrepToolName, dir, "AWS_SECRET_ACCESS_KEY", false); err != nil || outcome != ProceedOnce {
		t.Fatalf("directory approval = %v, %v", outcome, err)
	}
}

func TestApprovalManagerAutoPathFastPathSkipsReviewer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowed.txt")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	perms := NewToolPermissions()
	if err := perms.AddReadDir(dir); err != nil {
		t.Fatal(err)
	}
	mgr := newApprovalAutoTestManager(perms)
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		t.Fatal("guardian reviewer called for deterministic path allow")
		return PolicyDecision{}, nil
	}, nil)

	if outcome, err := mgr.CheckPathApprovalWithContext(context.Background(), ReadFileToolName, path, path, false); err != nil || outcome != ProceedOnce {
		t.Fatalf("deterministic path approval = %v, %v", outcome, err)
	}
}

func TestApprovalManagerAutoPathHeadlessFailuresDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	if err := os.WriteFile(path, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		review func(context.Context, PolicyReviewRequest) (PolicyDecision, error)
	}{
		{name: "denial", review: func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
			return PolicyDecision{Allowed: false, Rationale: "blocked by policy"}, nil
		}},
		{name: "review error", review: func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
			return PolicyDecision{}, errors.New("review unavailable")
		}},
		{name: "contradictory allow", review: func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
			return PolicyDecision{Allowed: true, RiskLevel: "critical", UserAuthorization: "unknown"}, nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newApprovalAutoTestManager(NewToolPermissions())
			mgr.SetApprovalMode(ModeAuto)
			mgr.SetAutoHeadless(true)
			mgr.SetPolicyReviewFunc(tt.review, nil)
			outcome, err := mgr.CheckPathApprovalWithContext(context.Background(), ReadFileToolName, path, path, false)
			if outcome != Cancel || err == nil {
				t.Fatalf("headless path result = %v, %v; want denial", outcome, err)
			}
		})
	}
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
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}, nil)
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

func TestApprovalManagerLazyTranscriptSupplierSkipsFastPaths(t *testing.T) {
	t.Run("yolo", func(t *testing.T) {
		mgr := newApprovalAutoTestManager(NewToolPermissions())
		mgr.SetApprovalMode(ModeYolo)
		calls := 0
		outcome, err := mgr.checkShellApprovalWithContext(context.Background(), "echo hi", "", func() []TranscriptEntry {
			calls++
			return []TranscriptEntry{{Role: "user", Text: "expensive transcript"}}
		})
		if err != nil || outcome != ProceedOnce {
			t.Fatalf("outcome = %v, err = %v, want yolo allow", outcome, err)
		}
		if calls != 0 {
			t.Fatalf("transcript supplier called %d times on yolo fast path, want 0", calls)
		}
	})

	t.Run("deterministic allow", func(t *testing.T) {
		perms := NewToolPermissions()
		perms.ShellAllow = []string{"git *"}
		if err := perms.CompileShellPatterns(); err != nil {
			t.Fatal(err)
		}
		mgr := newApprovalAutoTestManager(perms)
		mgr.SetApprovalMode(ModeAuto)
		mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
			t.Fatal("guardian reviewer should not be called for deterministic allow")
			return PolicyDecision{}, nil
		}, nil)
		calls := 0
		outcome, err := mgr.checkShellApprovalWithContext(context.Background(), "git status", "", func() []TranscriptEntry {
			calls++
			return []TranscriptEntry{{Role: "user", Text: "expensive transcript"}}
		})
		if err != nil || outcome != ProceedOnce {
			t.Fatalf("outcome = %v, err = %v, want deterministic allow", outcome, err)
		}
		if calls != 0 {
			t.Fatalf("transcript supplier called %d times on deterministic fast path, want 0", calls)
		}
	})
}

func TestApprovalManagerLazyTranscriptSupplierInvokedForGuardian(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	wantTranscript := TranscriptEntry{Role: "user", Text: "please inspect before running"}
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		if len(req.Transcript) != 1 || req.Transcript[0] != wantTranscript {
			t.Fatalf("review transcript = %#v, want %#v", req.Transcript, []TranscriptEntry{wantTranscript})
		}
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high", Rationale: "ok"}, nil
	}, nil)
	calls := 0
	outcome, err := mgr.checkShellApprovalWithContext(context.Background(), "echo hi", t.TempDir(), func() []TranscriptEntry {
		calls++
		return []TranscriptEntry{wantTranscript}
	})
	if err != nil || outcome != ProceedAlways {
		t.Fatalf("outcome = %v, err = %v, want guardian allow", outcome, err)
	}
	if calls != 1 {
		t.Fatalf("transcript supplier called %d times for guardian review, want 1", calls)
	}
}

func TestApprovalManagerGuardianExactCacheDoesNotTreatStarAsPattern(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	calls := 0
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}, nil)
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
	parent.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
	}, nil)
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
			mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
				return PolicyDecision{}, reviewErr
			}, nil)
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
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: false, Rationale: "not requested"}, nil
	}, nil)
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
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, Rationale: "not requested"}, nil
	}, nil)
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
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}, nil)
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
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}, nil)
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
	root.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}, nil)
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
	entries := approvalTranscriptFromContext(llm.ContextWithApprovalTranscript(context.Background(), msgs))
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
			mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
				return PolicyDecision{Allowed: true, RiskLevel: "critical", UserAuthorization: "unknown", Rationale: "too risky"}, nil
			}, nil)
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
	mgr.SetPolicyReviewFunc(func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		got = req
		return PolicyDecision{Allowed: true, RiskLevel: "medium", UserAuthorization: "high", Rationale: "equivalent approved write"}, nil
	}, nil)
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
