package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredShellPatternsUseShellAwareMatching(t *testing.T) {
	perms := NewToolPermissions()
	perms.ShellAllow = []string{"git *", "go test *"}
	if err := perms.CompileShellPatterns(); err != nil {
		t.Fatalf("CompileShellPatterns() error = %v", err)
	}

	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{name: "ordinary command", command: "git status", want: true},
		{name: "path argument", command: "go test ./...", want: true},
		{name: "all compound segments covered", command: "git status && git diff", want: true},
		{name: "uncovered sequential command", command: "git status; rm -rf /tmp/project", want: false},
		{name: "unsafe pipe target", command: "git log | sh", want: false},
		{name: "unrelated command", command: "npm install", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := perms.IsShellCommandAllowed(tt.command); got != tt.want {
				t.Errorf("IsShellCommandAllowed(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestConfiguredShellPatternsCombineAcrossCompoundCommands(t *testing.T) {
	patterns := []string{"gh *", "echo *", "python *"}
	perms := NewToolPermissions()
	perms.ShellAllow = append([]string(nil), patterns...)
	if err := perms.CompileShellPatterns(); err != nil {
		t.Fatalf("CompileShellPatterns() error = %v", err)
	}

	tests := []struct {
		command string
		want    bool
	}{
		{command: "gh pr view 1 && echo done", want: true},
		{command: "gh pr diff 1 | python summarize.py", want: true},
		{command: "gh pr view 1 && rm -rf /tmp/project", want: false},
		{command: "gh pr view 1 | sh", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			configured := perms.IsShellCommandAllowed(tt.command)
			sessionOrProject := matchAnyShellPattern(patterns, tt.command)
			if configured != sessionOrProject {
				t.Errorf("configured match = %v, session/project match = %v", configured, sessionOrProject)
			}
			if configured != tt.want {
				t.Errorf("IsShellCommandAllowed(%q) = %v, want %v", tt.command, configured, tt.want)
			}
		})
	}
}

func TestShellWordGlobPathSemantics(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		value   string
		want    bool
	}{
		{name: "single path segment", pattern: "src/*.go", value: "src/main.go", want: true},
		{name: "star does not cross separator", pattern: "src/*.go", value: "src/pkg/main.go", want: false},
		{name: "doublestar crosses separators", pattern: "src/**/*.go", value: "src/pkg/main.go", want: true},
		{name: "question mark matches unicode rune", pattern: "release-?", value: "release-β", want: true},
		{name: "character class", pattern: "[ab].go", value: "a.go", want: true},
		{name: "alternatives", pattern: "{main,test}.go", value: "test.go", want: true},
		{name: "malformed pattern", pattern: "[", value: "anything", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchShellPattern(tt.pattern, tt.value); got != tt.want {
				t.Errorf("matchShellPattern(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
			}
		})
	}
}

func TestShellPatternValidation(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		valid   bool
	}{
		{name: "simple wildcard", pattern: "git *", valid: true},
		{name: "recursive path", pattern: "go test src/**/*.go", valid: true},
		{name: "alternatives", pattern: "{go,git} *", valid: true},
		{name: "empty", pattern: "", valid: false},
		{name: "whitespace only", pattern: "   ", valid: false},
		{name: "malformed class", pattern: "git [", valid: false},
		{name: "unterminated quote", pattern: "echo 'unterminated", valid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &ToolConfig{ShellAllow: []string{tt.pattern}}
			errs := config.Validate()
			if got := len(errs) == 0; got != tt.valid {
				t.Errorf("ToolConfig.Validate() valid = %v, want %v (errors: %v)", got, tt.valid, errs)
			}

			perms := NewToolPermissions()
			err := perms.AddShellPattern(tt.pattern)
			if got := err == nil; got != tt.valid {
				t.Errorf("AddShellPattern(%q) valid = %v, want %v (error: %v)", tt.pattern, got, tt.valid, err)
			}

			if !tt.valid {
				perms.ShellAllow = []string{tt.pattern}
				if perms.IsShellCommandAllowed(tt.pattern) {
					t.Errorf("invalid directly assigned pattern %q must not approve a command", tt.pattern)
				}

				cache := NewShellApprovalCache()
				if err := cache.AddPattern(tt.pattern); err == nil {
					t.Errorf("session cache accepted invalid pattern %q", tt.pattern)
				}
				if got := cache.GetPatterns(); len(got) != 0 {
					t.Errorf("session cache retained invalid pattern: %v", got)
				}

				project := &ProjectApprovals{}
				if err := project.ApproveShellPattern(tt.pattern); err == nil {
					t.Errorf("ProjectApprovals.ApproveShellPattern(%q) succeeded, want error", tt.pattern)
				}
			}
		})
	}
}

func TestShellApprovalCacheStoresExactCommandsSeparately(t *testing.T) {
	cache := NewShellApprovalCache()
	command := `echo '['`
	workDir := t.TempDir()

	if err := cache.AddCommand(command, workDir); err != nil {
		t.Fatalf("AddCommand() error = %v", err)
	}
	if err := cache.AddCommand(command, workDir); err != nil {
		t.Fatalf("duplicate AddCommand() error = %v", err)
	}
	if !cache.IsCommandApproved(command, workDir) {
		t.Fatalf("exact command %q was not retained", command)
	}
	if cache.IsCommandApproved(command+" extra", workDir) {
		t.Fatalf("exact command approval widened to %q", command+" extra")
	}
	if cache.IsCommandApproved(command, t.TempDir()) {
		t.Fatal("exact command approval widened to another working directory")
	}
	if err := cache.AddCommand("   ", workDir); err == nil {
		t.Fatal("AddCommand() accepted an empty command")
	}
	if got := cache.GetPatterns(); len(got) != 0 {
		t.Fatalf("exact command was stored as a glob pattern: %v", got)
	}
	commands := cache.getCommands()
	if len(commands) != 1 || commands[0].command != command || commands[0].workDir != normalizeGuardianWorkDir(workDir) {
		t.Fatalf("getCommands() = %v, want command %q in %q", commands, command, workDir)
	}
}

func TestHandleShellApprovalResultInvalidPatternProceedsOnce(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	result := ApprovalResult{Choice: ApprovalChoicePattern, Pattern: "echo ["}

	outcome, err := mgr.handleShellApprovalResult(result, "echo '['", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("handleShellApprovalResult() error = %v, want one-time fallback", err)
	}
	if outcome != ProceedOnce {
		t.Fatalf("handleShellApprovalResult() outcome = %v, want ProceedOnce", outcome)
	}
	if got := mgr.shellCache.GetPatterns(); len(got) != 0 {
		t.Fatalf("invalid pattern was cached: %v", got)
	}
}

func TestHandleShellApprovalResultStoresExactCommand(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	command := `echo '['`
	workDir := t.TempDir()
	result := ApprovalResult{Choice: ApprovalChoiceCommand}

	outcome, err := mgr.handleShellApprovalResult(result, command, workDir, nil)
	if err != nil {
		t.Fatalf("handleShellApprovalResult() error = %v", err)
	}
	if outcome != ProceedAlways {
		t.Fatalf("handleShellApprovalResult() outcome = %v, want ProceedAlways", outcome)
	}
	if !mgr.shellCache.IsCommandApproved(command, workDir) {
		t.Fatalf("exact command %q was not cached", command)
	}

	cachedOutcome, err := mgr.CheckShellApproval(command, workDir)
	if err != nil {
		t.Fatalf("cached CheckShellApproval() error = %v", err)
	}
	if cachedOutcome != ProceedAlways {
		t.Fatalf("cached CheckShellApproval() outcome = %v, want ProceedAlways", cachedOutcome)
	}
}

func TestHandleShellApprovalResultFallsBackToSessionOnPersistenceError(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	project := &ProjectApprovals{filePath: t.TempDir()}
	result := ApprovalResult{
		Choice:     ApprovalChoicePattern,
		Pattern:    "git *",
		SaveToRepo: true,
	}

	outcome, err := mgr.handleShellApprovalResult(result, "git status", t.TempDir(), project)
	if err != nil {
		t.Fatalf("handleShellApprovalResult() error = %v, want session fallback", err)
	}
	if outcome != ProceedAlways {
		t.Fatalf("handleShellApprovalResult() outcome = %v, want ProceedAlways", outcome)
	}
	if project.IsShellPatternApproved("git status") {
		t.Fatal("failed persisted approval remained active in project memory")
	}
	if !matchAnyShellPattern(mgr.shellCache.GetPatterns(), "git status") {
		t.Fatal("failed persisted pattern was not retained as a session approval")
	}
}

func TestConfiguredShellPatternsTrimCommandWhitespace(t *testing.T) {
	perms := NewToolPermissions()
	if err := perms.AddShellPattern("git status && git diff"); err != nil {
		t.Fatalf("AddShellPattern() error = %v", err)
	}
	if !perms.IsShellCommandAllowed("  git status && git diff  ") {
		t.Fatal("surrounding whitespace prevented exact configured approval")
	}
}

func TestExactCommandApprovalInheritedOnlyInSameWorkDir(t *testing.T) {
	parent := NewApprovalManager(NewToolPermissions())
	child := NewApprovalManager(NewToolPermissions())
	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent() error = %v", err)
	}
	child.IgnoreProjectApprovals = true
	workDir := t.TempDir()
	command := "make clean"
	if err := parent.shellCache.AddCommand(command, workDir); err != nil {
		t.Fatalf("AddCommand() error = %v", err)
	}

	if outcome, ok := child.checkShellApprovalNoPrompt(command, workDir); !ok || outcome != ProceedAlways {
		t.Fatalf("same-workdir inherited approval = (%v, %v), want (%v, true)", outcome, ok, ProceedAlways)
	}
	if outcome, ok := child.checkShellApprovalNoPrompt(command, t.TempDir()); ok {
		t.Fatalf("different-workdir command unexpectedly approved with outcome %v", outcome)
	}
}

func TestExactCommandApprovalIncludedInGuardianContext(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	workDir := t.TempDir()
	command := "make clean"
	if err := mgr.shellCache.AddCommand(command, workDir); err != nil {
		t.Fatalf("AddCommand() error = %v", err)
	}

	context := mgr.guardianApprovalContext("unmatched command", workDir)
	if !strings.Contains(context, `session_shell_command="make clean"`) {
		t.Fatalf("guardian context missing exact command: %q", context)
	}
	if !strings.Contains(context, `workdir="`+normalizeGuardianWorkDir(workDir)+`"`) {
		t.Fatalf("guardian context missing exact command workdir: %q", context)
	}
}

func TestLegacyShellPromptCachesExactCommandByWorkDir(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.IgnoreProjectApprovals = true
	workDir := t.TempDir()
	command := `echo '['`
	prompts := 0
	mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) {
		prompts++
		return ProceedAlways, ""
	}

	outcome, err := mgr.CheckShellApproval(command, workDir)
	if err != nil || outcome != ProceedAlways {
		t.Fatalf("first CheckShellApproval() = (%v, %v), want (%v, nil)", outcome, err, ProceedAlways)
	}
	outcome, err = mgr.CheckShellApproval(command, workDir)
	if err != nil || outcome != ProceedAlways {
		t.Fatalf("cached CheckShellApproval() = (%v, %v), want (%v, nil)", outcome, err, ProceedAlways)
	}
	if prompts != 1 {
		t.Fatalf("PromptFunc called %d times, want 1", prompts)
	}
}

func TestLegacyShellPromptInvalidPatternProceedsOnce(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.IgnoreProjectApprovals = true
	mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) {
		return ProceedAlways, "git ["
	}

	outcome, err := mgr.CheckShellApproval("git status", t.TempDir())
	if err != nil {
		t.Fatalf("CheckShellApproval() error = %v, want one-time fallback", err)
	}
	if outcome != ProceedOnce {
		t.Fatalf("CheckShellApproval() outcome = %v, want ProceedOnce", outcome)
	}
	if got := mgr.shellCache.GetPatterns(); len(got) != 0 {
		t.Fatalf("invalid legacy pattern was cached: %v", got)
	}
}

func TestConfiguredShellPatternRecursivePathEndToEnd(t *testing.T) {
	perms := NewToolPermissions()
	if err := perms.AddShellPattern("cat src/**/*.go"); err != nil {
		t.Fatalf("AddShellPattern() error = %v", err)
	}
	if !perms.IsShellCommandAllowed("cat src/pkg/main.go") {
		t.Fatal("recursive path pattern did not match nested path")
	}
	if perms.IsShellCommandAllowed("cat test/pkg/main.go") {
		t.Fatal("recursive path pattern matched outside its prefix")
	}
}

func TestLoadedProjectApprovalsSkipInvalidPatterns(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	repoRoot := t.TempDir()
	filePath, err := projectApprovalsFilePath(repoRoot)
	if err != nil {
		t.Fatalf("projectApprovalsFilePath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	data := []byte("repo_root: " + repoRoot + "\nshell_patterns:\n  - 'git ['\n  - 'go test *'\n")
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	project, err := LoadProjectApprovals(repoRoot)
	if err != nil {
		t.Fatalf("LoadProjectApprovals() error = %v", err)
	}
	if project.IsShellPatternApproved("git [") {
		t.Fatal("loaded invalid pattern matched by exact equality")
	}
	if !project.IsShellPatternApproved("go test ./...") {
		t.Fatal("valid sibling pattern stopped matching")
	}
}
