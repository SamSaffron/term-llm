package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// No payload is executed: these tests exercise the approval boundary only.
func TestGuardianReviewsUnapprovedPipelineStages(t *testing.T) {
	commands := []string{
		`git status | head -n "$(id)"`,
		"git status | head -n `id`",
		`git status | cat < /home/user/.ssh/id_ed25519`,
		`git status | cat > stolen`,
		`git status | cat <(id)`,
		`git status | ./head`,
		`git status | /tmp/head`,
		`git status | head`,
		`git status | sort -o stolen`,
		`git status | less`,
		`git status | jq .title`,
		`git status | cat secret`,
	}
	for _, source := range []string{"configured", "session", "ancestor", "project"} {
		t.Run(source, func(t *testing.T) {
			for _, command := range commands {
				t.Run(command, func(t *testing.T) {
					mgr, workDir := newPatternApprovalTestManager(t, ModeAuto, source, GenerateShellPattern("git status"))
					if _, ok := mgr.checkShellApprovalNoPrompt("git status", workDir); !ok {
						t.Fatal("test setup did not install the benign git status approval")
					}
					assertGuardianReviewsShell(t, mgr, command, workDir)
				})
			}
		})
	}
}

func TestGuardianReviewsBroadExecutionPatterns(t *testing.T) {
	for _, tt := range []struct{ pattern, command string }{
		{"find *", `find . -exec id \;`},
		{"find *", `find . -execdir id +`},
		{"git *", `git -c alias.pwn='!id' pwn`},
		{"git -c *", `git -c alias.pwn='!id' pwn`},
		{"make *", `make pwn`},
		{"make build *", `make build -f attacker.mk`},
		{"npm *", `npm run pwn`},
		{GenerateShellPattern("npm run build"), `npm run pwn`},
		{GenerateShellPattern("npm test"), `npm test -- --runInBand`},
		{GenerateShellPattern("make build"), `make build -f attacker.mk`},
		{"yarn *", `yarn pwn`},
		{"pnpm *", `pnpm run pwn`},
	} {
		t.Run(tt.pattern+"/"+tt.command, func(t *testing.T) {
			// Prove this is a matching grant that auto must suspend, not a
			// command that would already miss ordinary pattern matching.
			if !matchAnyShellPattern([]string{tt.pattern}, tt.command) {
				t.Fatalf("pattern %q does not cover probe %q", tt.pattern, tt.command)
			}
			for _, source := range []string{"configured", "session", "ancestor", "project"} {
				t.Run(source, func(t *testing.T) {
					mgr, workDir := newPatternApprovalTestManager(t, ModeAuto, source, tt.pattern)
					assertGuardianReviewsShell(t, mgr, tt.command, workDir)
				})
			}
		})
	}
}

func newPatternApprovalTestManager(t *testing.T, mode ApprovalMode, source string, patterns ...string) (*ApprovalManager, string) {
	t.Helper()
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(mode)
	workDir := t.TempDir()
	switch source {
	case "configured":
		mgr.permissions.ShellAllow = patterns
	case "session", "ancestor":
		owner := mgr
		if source == "ancestor" {
			owner = newApprovalAutoTestManager(NewToolPermissions())
			owner.SetApprovalMode(mode)
			if err := mgr.SetParent(owner); err != nil {
				t.Fatal(err)
			}
		}
		for _, pattern := range patterns {
			if err := owner.ApproveShellPattern(pattern); err != nil {
				t.Fatal(err)
			}
		}
	case "project":
		cmd := exec.Command("git", "init", "-q")
		cmd.Dir = workDir
		if err := cmd.Run(); err != nil {
			t.Skipf("git init: %v", err)
		}
		mgr.IgnoreProjectApprovals = false
		repo := DetectGitRepo(workDir)
		if !repo.IsRepo {
			t.Fatal("git repository was not detected after initialization")
		}
		mgr.projectCache[repo.Root] = &ProjectApprovals{RepoRoot: repo.Root, ShellPatterns: patterns}
	default:
		t.Fatalf("unknown pattern source %q", source)
	}
	return mgr, workDir
}

func assertGuardianReviewsShell(t *testing.T, mgr *ApprovalManager, command, workDir string) {
	t.Helper()
	reviews := 0
	mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		reviews++
		if req.Command != command {
			t.Errorf("Guardian command = %q, want full command %q", req.Command, command)
		}
		return PolicyDecision{Allowed: false, Rationale: "not authorized"}, nil
	}, nil)
	outcome, err := mgr.CheckShellApproval(command, workDir)
	if outcome != Cancel || err == nil || reviews != 1 {
		t.Fatalf("approval = %v, %v; Guardian reviews = %d; want denial after one review", outcome, err, reviews)
	}
}

func TestPipelineStagesRequireExplicitApproval(t *testing.T) {
	command := "git status | head -n 5"
	if matchPattern("git status *", command) || matchAnyShellPattern([]string{"git status *"}, command) {
		t.Fatal("unapproved head inherited the first stage's approval")
	}
	if !matchAnyShellPattern([]string{"git status *", "head -n *"}, command) {
		t.Fatal("independently approved pipeline stages should remain usable")
	}
	for _, command := range []string{
		`git status | head -n "$(id)"`,
		"git status | head -n `id`",
		`git status | cat < secret`,
		`git status | cat > stolen`,
		`git status | ./head`,
		`git status | /tmp/head`,
	} {
		if matchAnyShellPattern([]string{"git status *", "head *", "cat *"}, command) {
			t.Errorf("ordinary filter patterns approved unsafe pipeline %q", command)
		}
	}
}

func TestGuardianReviewsPathShadowedPipelineFilter(t *testing.T) {
	dir := t.TempDir()
	// Only the first stage is approved. The former basename whitelist would
	// implicitly approve this head too, without resolving its PATH identity.
	// This stage must require review even when a system head is available.
	if err := os.WriteFile(filepath.Join(dir, "head"), []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	if err := mgr.ApproveShellPattern(GenerateShellPattern("git status")); err != nil {
		t.Fatal(err)
	}
	assertGuardianReviewsShell(t, mgr, "git status | head", dir)
}

func TestUnapprovedPipelineFilterPromptsOutsideAuto(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	if err := mgr.ApproveShellPattern("git status *"); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	mgr.PromptUIFunc = func(command string, _ bool, isShell bool, _ string) (ApprovalResult, error) {
		prompts++
		if command != "git status | head" || !isShell {
			t.Fatalf("unexpected approval request %q (shell=%v)", command, isShell)
		}
		return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
	}
	if outcome, err := mgr.CheckShellApproval("git status | head", t.TempDir()); err != nil || outcome != Cancel || prompts != 1 {
		t.Fatalf("approval = %v, %v; prompts = %d; want one human denial", outcome, err, prompts)
	}
}

func TestApprovedPipelineStagesSkipGuardianAndPrompt(t *testing.T) {
	for _, mode := range []ApprovalMode{ModePrompt, ModeAuto} {
		t.Run(mode.String(), func(t *testing.T) {
			for _, source := range []string{"configured", "session", "ancestor", "project"} {
				t.Run(source, func(t *testing.T) {
					mgr, workDir := newPatternApprovalTestManager(t, mode, source, "git status *", "head -n *")
					reviews, prompts := 0, 0
					mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
						reviews++
						return PolicyDecision{Allowed: false, Rationale: "unexpected review"}, nil
					}, nil)
					mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
						prompts++
						return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
					}
					want := ProceedAlways
					if source == "configured" {
						want = ProceedOnce
					}
					// Use different commands, then repeat: neither exact-command
					// caching nor a single successful invocation can mask a miss.
					for _, command := range []string{"git status | head -n 5", "git status | head -n 10", "git status | head -n 5"} {
						if outcome, err := mgr.CheckShellApproval(command, workDir); err != nil || outcome != want {
							t.Fatalf("approval for %q = %v, %v; want %v", command, outcome, err, want)
						}
					}
					if reviews != 0 || prompts != 0 {
						t.Fatalf("reviews=%d prompts=%d; approved stages must skip both", reviews, prompts)
					}
				})
			}
		})
	}
}

func TestRememberedPipelineStagePatternsAreReusable(t *testing.T) {
	mgr, workDir := newPatternApprovalTestManager(t, ModePrompt, "session")
	var prompted []string
	mgr.PromptUIFunc = func(command string, _ bool, _ bool, _ string) (ApprovalResult, error) {
		prompted = append(prompted, command)
		// Exercise the UI's default pattern generation and session storage.
		return ApprovalResult{Choice: ApprovalChoicePattern}, nil
	}
	for _, command := range []string{"git status", "head -n 5", "git status | head -n 5", "git status | head -n 10"} {
		if outcome, err := mgr.CheckShellApproval(command, workDir); err != nil || outcome != ProceedAlways {
			t.Fatalf("approval for %q = %v, %v; want remembered approval", command, outcome, err)
		}
	}
	if len(prompted) != 2 || prompted[0] != "git status" || prompted[1] != "head -n 5" {
		t.Fatalf("prompts=%v; want only the two individual stage approvals", prompted)
	}
}
