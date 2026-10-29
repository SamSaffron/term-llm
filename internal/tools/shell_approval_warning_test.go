package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestBuildShellOptionsCodeExecutionWarning(t *testing.T) {
	repo := &GitRepoInfo{IsRepo: true, RepoName: "project"}
	for _, command := range []string{
		"python build.py", "python3 build.py", "/usr/bin/python3.12 build.py",
		`python -c 'print("hello")'`, "node build.js", "perl build.pl", "ruby build.rb",
		`bash -c 'echo hello'`, "sh build.sh", "make build", "npm run build",
		"yarn test", "pnpm test", "go run ./cmd/build", "go test ./...",
		"cargo test", "pytest tests", "uv run pytest", "npx eslint", "cmake --build build",
	} {
		t.Run(command, func(t *testing.T) {
			options := BuildShellOptions(command, repo)
			var pattern *ApprovalOption
			for i := range options {
				if options[i].Choice == ApprovalChoicePattern {
					pattern = &options[i]
				}
			}
			if pattern == nil {
				t.Fatal("missing explicit advanced pattern choice")
			}
			if !strings.HasPrefix(pattern.Label, "Advanced:") || !strings.Contains(pattern.Label, "code execution") || !strings.Contains(pattern.Label, pattern.Pattern) {
				t.Errorf("pattern label does not disclose scope: %q", pattern.Label)
			}
			if !strings.Contains(pattern.Description, "arbitrary code execution") || !strings.Contains(pattern.Description, "OS user's access") {
				t.Errorf("pattern description does not disclose authority: %q", pattern.Description)
			}
			if !strings.Contains(pattern.Description, "Press Enter to confirm") {
				t.Errorf("pattern does not explain explicit confirmation: %q", pattern.Description)
			}
			if options[0].Choice != ApprovalChoiceOnce {
				t.Errorf("default choice = %v, want allow once", options[0].Choice)
			}
		})
	}

	options := BuildShellOptions("git status", repo)
	if strings.Contains(options[2].Label, "code execution") {
		t.Errorf("narrow Git inspection grant has an execution warning: %q", options[2].Label)
	}
	if !strings.Contains(options[2].Description, "not a filesystem or network sandbox") {
		t.Errorf("pattern should not promise containment: %q", options[2].Description)
	}
}

func TestShellApprovalEnterDefaultsToOnce(t *testing.T) {
	dir := t.TempDir()
	repo := &GitRepoInfo{IsRepo: true, Root: dir, RepoName: "project"}
	for name, model := range map[string]*ApprovalModel{
		"standalone": newShellApprovalModel("python build.py", dir, repo),
		"embedded":   NewEmbeddedShellApprovalModel("python build.py", dir, 100),
	} {
		t.Run(name, func(t *testing.T) {
			if !model.UpdateEmbedded(tea.KeyPressMsg{Code: tea.KeyEnter}) {
				t.Fatal("Enter did not complete prompt")
			}
			if result := model.Result(); result.Choice != ApprovalChoiceOnce || result.SaveToRepo || result.Pattern != "" {
				t.Errorf("default selection = %+v, want one-time execution without a grant", result)
			}
		})
	}
}

func TestShellApprovalSafeChoicesDoNotWiden(t *testing.T) {
	const command = "python build.py"
	for _, choice := range []ApprovalChoice{ApprovalChoiceOnce, ApprovalChoiceCommand} {
		t.Run(map[ApprovalChoice]string{ApprovalChoiceOnce: "once", ApprovalChoiceCommand: "exact"}[choice], func(t *testing.T) {
			mgr, dir := newPatternApprovalTestManager(t, ModePrompt, "session")
			project := &ProjectApprovals{filePath: filepath.Join(dir, "approvals.yaml")}
			options := BuildShellOptions(command, &GitRepoInfo{IsRepo: true, RepoName: "project"})
			for _, option := range options {
				if option.Choice != choice {
					continue
				}
				result := ApprovalResult{Choice: option.Choice, Pattern: option.Pattern, SaveToRepo: option.SaveToRepo}
				if outcome, err := mgr.handleShellApprovalResult(result, command, dir, project); err != nil || outcome == Cancel {
					t.Fatalf("approval failed: %v, %v", outcome, err)
				}
			}
			if _, err := os.Stat(project.filePath); !os.IsNotExist(err) {
				t.Fatalf("safe choice persisted an approval file: %v", err)
			}
			for _, probe := range []string{`python -c 'print("different program")'`, "python other.py", "python build.py --different"} {
				if _, ok := mgr.checkShellApprovalNoPrompt(probe, dir); ok {
					t.Errorf("safe choice unexpectedly approved %q", probe)
				}
			}
			_, exact := mgr.checkShellApprovalNoPrompt(command, dir)
			if exact != (choice == ApprovalChoiceCommand) {
				t.Errorf("exact command remembered = %t for choice %v", exact, choice)
			}
			if _, ok := mgr.checkShellApprovalNoPrompt(command, t.TempDir()); ok {
				t.Error("exact command approval leaked to another working directory")
			}
		})
	}
}

func TestShellApprovalAdvancedPatternRequiresExplicitChoice(t *testing.T) {
	mgr, dir := newPatternApprovalTestManager(t, ModePrompt, "session")
	project := &ProjectApprovals{filePath: filepath.Join(dir, "approvals.yaml")}
	options := BuildShellOptions("python build.py", &GitRepoInfo{IsRepo: true, RepoName: "project"})
	advanced := options[2]
	if advanced.Choice != ApprovalChoicePattern || advanced.Pattern != "python *" || !advanced.SaveToRepo {
		t.Fatalf("advanced choice = %+v, want explicit broad grant", advanced)
	}
	result := ApprovalResult{Choice: advanced.Choice, Pattern: advanced.Pattern, SaveToRepo: advanced.SaveToRepo}
	if outcome, err := mgr.handleShellApprovalResult(result, "python build.py", dir, project); err != nil || outcome != ProceedAlways {
		t.Fatalf("explicit advanced approval = %v, %v", outcome, err)
	}
	if _, ok := mgr.checkShellApprovalNoPrompt(`python -c 'print("another program")'`, dir); !ok {
		t.Error("explicit broad grant no longer works in prompt mode")
	}
	data, err := os.ReadFile(project.filePath)
	if err != nil || !strings.Contains(string(data), "python *") {
		t.Fatalf("explicit broad grant was not persisted: %s, %v", data, err)
	}
}

func TestShellApprovalPatternNumberRequiresConfirmation(t *testing.T) {
	dir := t.TempDir()
	model := newShellApprovalModel("python build.py", dir, &GitRepoInfo{IsRepo: true, Root: dir, RepoName: "project"})
	for range 2 {
		if model.UpdateEmbedded(tea.KeyPressMsg{Code: '3', Text: "3"}) || model.IsDone() {
			t.Fatal("number shortcut immediately confirmed a persistent broad grant")
		}
		if model.cursor != 2 || model.options[model.cursor].Choice != ApprovalChoicePattern {
			t.Fatal("number shortcut did not select the advanced pattern for review")
		}
	}
	if !model.UpdateEmbedded(tea.KeyPressMsg{Code: tea.KeyEnter}) {
		t.Fatal("explicit Enter did not confirm the selected pattern")
	}
	if result := model.Result(); result.Choice != ApprovalChoicePattern || result.Pattern != "python *" || !result.SaveToRepo {
		t.Fatalf("explicitly confirmed pattern = %+v", result)
	}
}

func TestFixedGitInspectionGrantsRejectArguments(t *testing.T) {
	patterns := []string{"git diff", "git log"}
	for _, command := range []string{"git diff", "git log"} {
		if !matchAnyShellPattern(patterns, command) {
			t.Errorf("fixed inspection grant does not cover %q", command)
		}
	}
	for _, command := range []string{
		"git diff /dev/null /etc/hostname", "git diff --output=outside", "git log --output=outside",
		"git difftool", "git log-alias", "git diff --cached", "git log --oneline",
	} {
		if matchAnyShellPattern(patterns, command) {
			t.Errorf("argument-free Git grants unexpectedly cover %q", command)
		}
	}
}

func TestSharedShellExactApprovalDisclosesMutableContents(t *testing.T) {
	for _, option := range BuildSharedShellOptions("python build.py") {
		if option.Choice == ApprovalChoiceCommand {
			if !strings.Contains(option.Description, "script and executable contents can change") || strings.Contains(option.Description, "working directory") {
				t.Errorf("shared exact command caveat = %q", option.Description)
			}
			return
		}
	}
	t.Fatal("missing exact shared-shell command option")
}
