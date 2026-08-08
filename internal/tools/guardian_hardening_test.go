package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGuardianActionMessagesAreConsistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		path   bool
		review func(context.Context, PolicyReviewRequest) (PolicyDecision, error)
		setup  func(*ApprovalManager)
		want   string
	}{
		{
			name: "shell denial",
			review: func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
				return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
			},
			want: "guardian denied this action: blocked. " + guardianSafeNextStep,
		},
		{
			name: "path denial",
			path: true,
			review: func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
				return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
			},
			want: "guardian denied this action: blocked. " + guardianSafeNextStep,
		},
		{
			name: "contradictory allow",
			review: func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
				return PolicyDecision{Allowed: true, RiskLevel: "high", UserAuthorization: "low", Rationale: "unsafe allow"}, nil
			},
			want: "guardian denied this action: unsafe allow. " + guardianSafeNextStep,
		},
		{
			name: "review error",
			review: func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
				return PolicyDecision{}, errors.New("malformed response")
			},
			want: "guardian could not review this action: malformed response. " + guardianSafeNextStep,
		},
		{
			name: "reviewer unavailable",
			setup: func(mgr *ApprovalManager) {
				mgr.SetPolicyReviewFunc(nil, nil)
			},
			want: "guardian could not review this action: guardian policy reviewer is not configured. " + guardianSafeNextStep,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newApprovalAutoTestManager(NewToolPermissions())
			mgr.SetApprovalMode(ModeAuto)
			if tt.review != nil {
				mgr.SetPolicyReviewFunc(tt.review, nil)
			}
			if tt.setup != nil {
				tt.setup(mgr)
			}
			mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
				t.Fatal("PromptUIFunc called for terminal Guardian result")
				return ApprovalResult{}, nil
			}
			mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) {
				t.Fatal("PromptFunc called for terminal Guardian result")
				return Cancel, ""
			}

			var outcome ConfirmOutcome
			var err error
			if tt.path {
				outcome, err = mgr.CheckPathApprovalWithContext(context.Background(), ReadFileToolName, path, path, false)
			} else {
				outcome, err = mgr.CheckShellApproval("dangerous action", t.TempDir())
			}
			var toolErr *ToolError
			if outcome != Cancel || !errors.As(err, &toolErr) || toolErr.Type != ErrPermissionDenied || toolErr.Message != tt.want {
				t.Fatalf("result = %v, %#v; want PERMISSION_DENIED %q", outcome, err, tt.want)
			}
		})
	}
}

func TestDeniedPathToolOutputIsMarkedError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denied.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, Rationale: "not authorized"}, nil
	}, nil)
	tool := NewReadFileTool(mgr, OutputLimits{MaxBytes: 1024})
	args, err := json.Marshal(ReadFileArgs{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !output.IsError || !strings.Contains(output.Content, "guardian denied this action") {
		t.Fatalf("denied output = %#v, want IsError Guardian denial", output)
	}
}

func TestGuardianApprovalResetsConsecutiveNotTotal(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		if strings.Contains(req.Command, "allow") {
			return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high"}, nil
		}
		return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
	}, nil)
	_, _ = mgr.CheckShellApproval("deny one", t.TempDir())
	if outcome, err := mgr.CheckShellApproval("allow one", t.TempDir()); err != nil || outcome != ProceedAlways {
		t.Fatalf("approval = %v, %v", outcome, err)
	}
	mgr.guardianMu.RLock()
	consecutive, total := mgr.guardianConsecutiveDenials, mgr.guardianTotalDenials
	mgr.guardianMu.RUnlock()
	if consecutive != 0 || total != 1 {
		t.Fatalf("counts after approval = consecutive %d, total %d; want 0, 1", consecutive, total)
	}
}

func TestGuardianTotalDenialLimitSuspendsIntermittentDenials(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		if strings.HasPrefix(req.Command, "allow") {
			return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high"}, nil
		}
		return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
	}, nil)
	var warning string
	mgr.GuardianEventFunc = func(event GuardianEvent) {
		if event.Outcome == GuardianWarning && strings.Contains(event.Message, "auto mode suspended") {
			warning = event.Message
		}
	}
	for i := 1; i <= 20; i++ {
		outcome, err := mgr.CheckShellApproval(fmt.Sprintf("deny %d", i), t.TempDir())
		if outcome != Cancel || err == nil {
			t.Fatalf("denial %d = %v, %v", i, outcome, err)
		}
		if i < 20 {
			outcome, err = mgr.CheckShellApproval(fmt.Sprintf("allow %d", i), t.TempDir())
			if outcome != ProceedAlways || err != nil {
				t.Fatalf("intermittent approval %d = %v, %v", i, outcome, err)
			}
		}
	}
	if mgr.ApprovalMode() != ModePrompt || !strings.Contains(warning, "total denial limit") || !strings.Contains(warning, "consecutive=1, total=20") {
		t.Fatalf("mode=%v warning=%q, want total breaker", mgr.ApprovalMode(), warning)
	}
	prompted := false
	mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		prompted = true
		return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
	}
	if outcome, err := mgr.CheckShellApproval("next action", t.TempDir()); err != nil || outcome != ProceedOnce || !prompted {
		t.Fatalf("next action = %v, prompted=%t, err=%v; want prompt mode", outcome, prompted, err)
	}
}

func TestGuardianSuspendedPatternStillPromptsAfterBreaker(t *testing.T) {
	perms := NewToolPermissions()
	perms.ShellAllow = []string{"python *"}
	mgr := newApprovalAutoTestManager(perms)
	mgr.SetApprovalMode(ModeAuto)
	reviews := 0
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		reviews++
		return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
	}, nil)
	workDir := t.TempDir()
	for i := 0; i < 3; i++ {
		_, _ = mgr.CheckShellApproval(fmt.Sprintf("deny %d", i), workDir)
	}
	if mgr.ApprovalMode() != ModePrompt {
		t.Fatal("breaker did not suspend auto")
	}
	prompts := 0
	mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		prompts++
		return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
	}
	outcome, err := mgr.CheckShellApproval("python post_breaker.py", workDir)
	if err != nil || outcome != Cancel {
		t.Fatalf("post-breaker python pattern = %v, %v; want prompt denial", outcome, err)
	}
	if prompts != 1 {
		t.Fatalf("post-breaker prompts = %d, want 1", prompts)
	}
	if reviews != 3 {
		t.Fatalf("post-breaker Guardian reviews = %d, want only the 3 pre-breaker reviews", reviews)
	}
}

func TestGuardianAutoEpochTransitions(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
	}, nil)
	for i := 0; i < 3; i++ {
		_, _ = mgr.CheckShellApproval(fmt.Sprintf("deny %d", i), t.TempDir())
	}
	if mgr.ApprovalMode() != ModePrompt {
		t.Fatal("breaker did not suspend auto")
	}
	mgr.SetApprovalMode(ModeAuto)
	if mgr.ApprovalMode() != ModePrompt {
		t.Fatal("setting the same requested auto mode cleared suspension")
	}
	if !mgr.ResumeAuto() {
		t.Fatal("ResumeAuto did not clear breaker suspension")
	}
	if mgr.ApprovalMode() != ModeAuto {
		t.Fatal("ResumeAuto did not start a fresh auto epoch")
	}
	if mgr.ResumeAuto() {
		t.Fatal("ResumeAuto reported a resume without a breaker suspension")
	}
	mgr.SetApprovalMode(ModePrompt)
	mgr.SetApprovalMode(ModeAuto)
	if mgr.ApprovalMode() != ModeAuto {
		t.Fatal("leaving and re-entering auto did not start a fresh epoch")
	}
	mgr.guardianMu.RLock()
	consecutive, total, suspended := mgr.guardianConsecutiveDenials, mgr.guardianTotalDenials, mgr.guardianAutoSuspended
	mgr.guardianMu.RUnlock()
	if consecutive != 0 || total != 0 || suspended {
		t.Fatalf("fresh epoch state = %d/%d suspended=%t", consecutive, total, suspended)
	}
}

func TestGuardianConcurrentParentChildDenialsTripOnce(t *testing.T) {
	root := newApprovalAutoTestManager(NewToolPermissions())
	root.SetApprovalMode(ModeAuto)
	child := newApprovalAutoTestManager(NewToolPermissions())
	if err := child.SetParent(root); err != nil {
		t.Fatal(err)
	}
	const reviews = 8
	started := make(chan struct{}, reviews)
	release := make(chan struct{})
	root.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		started <- struct{}{}
		<-release
		return PolicyDecision{Allowed: false, Rationale: "blocked"}, nil
	}, nil)
	var warnings atomic.Int32
	root.GuardianEventFunc = func(event GuardianEvent) {
		if event.Outcome == GuardianWarning && strings.Contains(event.Message, "auto mode suspended") {
			warnings.Add(1)
		}
	}
	var wg sync.WaitGroup
	workDir := t.TempDir()
	for i := 0; i < reviews; i++ {
		wg.Add(1)
		mgr := root
		if i%2 == 1 {
			mgr = child
		}
		go func(i int, mgr *ApprovalManager) {
			defer wg.Done()
			_, _ = mgr.CheckShellApproval(fmt.Sprintf("deny concurrent %d", i), workDir)
		}(i, mgr)
	}
	for i := 0; i < reviews; i++ {
		<-started
	}
	close(release)
	wg.Wait()
	if got := warnings.Load(); got != 1 {
		t.Fatalf("breaker warnings = %d, want exactly 1", got)
	}
	if root.ApprovalMode() != ModePrompt || child.ApprovalMode() != ModePrompt {
		t.Fatalf("effective modes = root %v child %v, want prompt", root.ApprovalMode(), child.ApprovalMode())
	}
}

func TestArbitraryExecutionShellPatternPredicate(t *testing.T) {
	tests := map[string]bool{
		"*": true, "*/bin/*": true,
		"python *": true, "/usr/bin/python3 *": true, "python *.py": true,
		"node *": true, "node *.js": true, "bash *": true, "env *": true, "sudo *": true,
		"uv run *": true, "uv run *.py": true, "npx *": true, "npx *.js": true,
		"pipx run *": true, "pipx run tool?": true,
		"sudo systemctl status": false, "git status": false, "go test *": false,
		"npm test": false, "python script.py": false, "node app.js": false,
		"uv run pytest": false, "uv sync": false, "npx eslint": false, "pipx run black": false,
	}
	for pattern, want := range tests {
		if got := isArbitraryExecutionShellPattern(pattern); got != want {
			t.Errorf("isArbitraryExecutionShellPattern(%q) = %t, want %t", pattern, got, want)
		}
	}
}

func TestGuardianShellPatternFilteringBySource(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		perms := NewToolPermissions()
		perms.ShellAllow = []string{
			"python *.py", "python fixed.py", "node *.js", "node fixed.js",
			"uv run *", "uv run pytest", "npx *", "npx eslint", "pipx run *", "pipx run black",
			"git status",
		}
		mgr := newApprovalAutoTestManager(perms)
		mgr.SetApprovalMode(ModeAuto)
		for _, command := range []string{"python script.py", "node app.js", "uv run tool", "npx prettier", "pipx run ruff"} {
			if _, ok := mgr.checkShellApprovalNoPrompt(command, ""); ok {
				t.Fatalf("configured interpreter/dispatcher pattern approved %q in auto", command)
			}
		}
		for _, command := range []string{"python fixed.py", "node fixed.js", "uv run pytest", "npx eslint", "pipx run black", "git status"} {
			if outcome, ok := mgr.checkShellApprovalNoPrompt(command, ""); !ok || outcome != ProceedOnce {
				t.Fatalf("narrow configured pattern for %q = %v, %t", command, outcome, ok)
			}
		}
	})

	t.Run("session and ancestor", func(t *testing.T) {
		root := newApprovalAutoTestManager(NewToolPermissions())
		root.SetApprovalMode(ModeAuto)
		if err := root.ApproveShellPattern("python *"); err != nil {
			t.Fatal(err)
		}
		child := newApprovalAutoTestManager(NewToolPermissions())
		if err := child.SetParent(root); err != nil {
			t.Fatal(err)
		}
		if err := child.ApproveShellPattern("node *"); err != nil {
			t.Fatal(err)
		}
		for _, command := range []string{"python script.py", "node script.js"} {
			if _, ok := child.checkShellApprovalNoPrompt(command, ""); ok {
				t.Fatalf("session/ancestor pattern approved %q in auto", command)
			}
		}
	})

	t.Run("project", func(t *testing.T) {
		dir := t.TempDir()
		cmd := exec.Command("git", "init", "-q")
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Skipf("git init: %v", err)
		}
		root := DetectGitRepo(dir).Root
		mgr := NewApprovalManager(NewToolPermissions())
		mgr.SetApprovalMode(ModeAuto)
		mgr.projectCache[root] = &ProjectApprovals{RepoRoot: root, ShellPatterns: []string{"python *", "git status"}}
		if _, ok := mgr.checkShellApprovalNoPrompt("python script.py", dir); ok {
			t.Fatal("project interpreter pattern bypassed Guardian")
		}
		if _, ok := mgr.checkShellApprovalNoPrompt("git status", dir); !ok {
			t.Fatal("narrow project pattern should remain deterministic")
		}
	})
}

func TestGuardianClassifyAllShellAndExactApprovals(t *testing.T) {
	perms := NewToolPermissions()
	perms.ShellAllow = []string{"git status"}
	perms.AddScriptCommand("python exact.py")
	mgr := newApprovalAutoTestManager(perms)
	mgr.SetGuardianClassifyAllShell(true)
	mgr.SetApprovalMode(ModeAuto)
	if _, ok := mgr.checkShellApprovalNoPrompt("git status", ""); ok {
		t.Fatal("classify_all_shell did not suspend narrow configured pattern")
	}
	if _, ok := mgr.checkShellApprovalNoPrompt("python exact.py", ""); !ok {
		t.Fatal("classify_all_shell suspended exact configured script")
	}
	workDir := t.TempDir()
	if err := mgr.shellCache.AddCommand("node exact.js", workDir); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.checkShellApprovalNoPrompt("node exact.js", workDir); !ok {
		t.Fatal("classify_all_shell suspended exact session command")
	}
	mgr.addGuardianExactShell("ruby exact.rb", workDir)
	if _, ok := mgr.checkShellApprovalNoPrompt("ruby exact.rb", workDir); !ok {
		t.Fatal("classify_all_shell suspended exact Guardian command")
	}
}

func TestGuardianPatternFilteringPreservesModesAndCompoundBoundaries(t *testing.T) {
	perms := NewToolPermissions()
	perms.ShellAllow = []string{"python *", "git *"}
	mgr := newApprovalAutoTestManager(perms)
	if outcome, ok := mgr.checkShellApprovalNoPrompt("python script.py", ""); !ok || outcome != ProceedOnce {
		t.Fatalf("prompt mode broad pattern = %v, %t", outcome, ok)
	}
	mgr.SetApprovalMode(ModeYolo)
	if outcome, err := mgr.CheckShellApproval("anything", ""); err != nil || outcome != ProceedOnce {
		t.Fatalf("yolo changed = %v, %v", outcome, err)
	}
	mgr.SetApprovalMode(ModePrompt)
	if err := mgr.ApproveShellPattern("python *"); err != nil {
		t.Fatal(err)
	}
	mgr.SetApprovalMode(ModeAuto)
	if _, ok := mgr.checkShellApprovalNoPrompt("git status && python x.py", ""); ok {
		t.Fatal("patterns from configured and session sources were unioned")
	}
	if _, ok := mgr.checkShellApprovalNoPrompt("python x.py | head", ""); ok {
		t.Fatal("safe pipe target resurrected a suspended head pattern")
	}
}

func TestApprovalChoicePatternIsSuspendedAfterEnteringAuto(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	result := ApprovalResult{Choice: ApprovalChoicePattern, Pattern: "python *"}
	if outcome, err := mgr.handleShellApprovalResult(result, "python script.py", "", nil); err != nil || outcome != ProceedAlways {
		t.Fatalf("remember pattern = %v, %v", outcome, err)
	}
	mgr.SetApprovalMode(ModeAuto)
	if _, ok := mgr.checkShellApprovalNoPrompt("python script.py", ""); ok {
		t.Fatal("remembered interpreter pattern remained active in auto")
	}
}

func TestSuspendedPatternReachesGuardianAndCachesExact(t *testing.T) {
	perms := NewToolPermissions()
	perms.ShellAllow = []string{"python *"}
	mgr := newApprovalAutoTestManager(perms)
	mgr.SetApprovalMode(ModeAuto)
	calls := 0
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		calls++
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high"}, nil
	}, nil)
	workDir := t.TempDir()
	for i := 0; i < 2; i++ {
		if outcome, err := mgr.CheckShellApproval("python safe.py", workDir); err != nil || outcome != ProceedAlways {
			t.Fatalf("approval %d = %v, %v", i+1, outcome, err)
		}
	}
	if calls != 1 {
		t.Fatalf("Guardian calls = %d, want one then exact cache", calls)
	}
}
