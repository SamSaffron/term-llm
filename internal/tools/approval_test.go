package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		pattern string
		command string
		want    bool
	}{
		// Exact matches
		{"git status", "git status", true},
		{"npm test", "npm test", true},
		{"ls", "ls", true},

		// Wildcard patterns
		{"git *", "git status", true},
		{"git *", "git commit -m 'message'", true},
		{"go test *", "go test ./...", true},
		{"npm *", "npm install lodash", true},
		{"git *", "git status ; cat /tmp/secret", false},
		{"go test *", "go test ./... && rm -rf /", false},

		// Non-matches
		{"git *", "npm install", false},
		{"git status", "git commit", false},
		{"npm test", "npm install", false},
		{"", "anything", false},

		// Edge cases
		{"*", "anything", true},
		{"a*", "abc", true},
		{"a*", "bcd", false},

		// Commands with && where ALL sub-commands match the pattern
		{"bundle *", "bundle exec stree check a.rb && bundle exec stree write b.rb", true},
		{"go test *", "go test ./pkg1 && go test ./pkg2", true},
		{"bundle *", "bundle exec rake db:migrate && bundle exec rake db:seed", true},

		// Commands with && where some sub-commands DON'T match
		{"bundle *", "bundle exec stree check a.rb && rm -rf /", false},
		{"bundle *", "npm install && bundle exec rake", false},

		// Pipes: first command must match, downstream must be safe or match
		{"git *", "git log --oneline | head -20", true}, // head is a safe pipe target
		{"git *", "git log | git diff", true},           // both match "git *"
		{"git *", "git log --oneline | tail -30", true},
		{"git *", "git log --oneline | grep feature", true},
		{"git *", "git log --oneline | sort | uniq", true},
		{"bin/rspec *", "bin/rspec spec/foo | tail -30", true},

		// Unsafe pipe targets: NOT auto-approved
		{"git *", "git log | bash", false},
		{"git *", "git log | sh", false},
		{"git *", "git log | xargs rm", false},

		// Mixed sequential + pipe
		{"git *", "git log | head -5 && git status | tail -3", true},
		{"git *", "git log | head -5 && rm -rf /", false},
		{"bundle *", "bundle exec rspec | tail -20 && bundle exec rubocop | grep error", true},

		// Pipe target that matches pattern is OK even if not in safe list
		{"npm *", "npm test | npm run format", true},

		// $ in single quotes is not unsafe — normal match works
		{"bundle *", "bundle exec ruby -e '$stdout.puts 1'", true},

		// Non-wildcard patterns don't decompose (exact match only)
		{"git status", "git status && echo done", false},

		// Quoted && inside arguments is NOT a shell operator
		{"ruby *", "ruby -e 'true && false'", true},

		// Mixed: quoted && plus real &&
		{"echo *", "echo 'a && b' && echo c", true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.command, func(t *testing.T) {
			got := matchPattern(tt.pattern, tt.command)
			if got != tt.want {
				t.Errorf("matchPattern(%q, %q) = %v, want %v", tt.pattern, tt.command, got, tt.want)
			}
		})
	}
}

func TestApprovalManager_CheckPathApproval_PreApproved(t *testing.T) {
	// Create temp directory structure
	tempDir, err := os.MkdirTemp("", "test-approval-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	readDir := filepath.Join(tempDir, "allowed-read")
	writeDir := filepath.Join(tempDir, "allowed-write")
	if err := os.MkdirAll(readDir, 0755); err != nil {
		t.Fatalf("failed to create read dir: %v", err)
	}
	if err := os.MkdirAll(writeDir, 0755); err != nil {
		t.Fatalf("failed to create write dir: %v", err)
	}

	// Create test files (permissions check validates file exists)
	readFile := filepath.Join(readDir, "file.txt")
	writeFile := filepath.Join(writeDir, "file.txt")
	if err := os.WriteFile(readFile, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create read file: %v", err)
	}
	if err := os.WriteFile(writeFile, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create write file: %v", err)
	}

	// Create permissions with pre-approved directories
	perms := NewToolPermissions()
	perms.ReadDirs = []string{readDir}
	perms.WriteDirs = []string{writeDir}

	mgr := NewApprovalManager(perms)

	// Test read access to pre-approved read directory
	outcome, err := mgr.CheckPathApproval("read_file", readFile, "", false)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if outcome != ProceedOnce {
		t.Errorf("expected ProceedOnce for pre-approved read, got %v", outcome)
	}

	// Test write access to pre-approved write directory
	outcome, err = mgr.CheckPathApproval("write_file", writeFile, "", true)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if outcome != ProceedOnce {
		t.Errorf("expected ProceedOnce for pre-approved write, got %v", outcome)
	}

	// Note: In this implementation, WriteDirs do NOT automatically include read permission.
	// Read access must be explicitly granted via ReadDirs.
}

func TestApprovalManager_CheckPathApproval_SymlinkEscapeDeniedBySessionCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test not supported on Windows")
	}

	approvedDir := t.TempDir()
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	link := filepath.Join(approvedDir, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	mgr := NewApprovalManager(NewToolPermissions())
	mgr.dirCache.Set(approvedDir, ProceedAlways, false)

	outcome, err := mgr.CheckPathApproval(ReadFileToolName, link, link, false)
	if err == nil {
		t.Fatalf("expected symlink escape error, got outcome=%v", outcome)
	}
	if toolErr, ok := err.(*ToolError); !ok || toolErr.Type != ErrSymlinkEscape {
		t.Fatalf("expected symlink escape error, got %T %v", err, err)
	}
}

func TestApprovalManager_CheckPathApproval_SessionCache(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-approval-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	testDir := filepath.Join(tempDir, "test")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatalf("failed to create test dir: %v", err)
	}

	// Create test file
	testFile := filepath.Join(testDir, "file.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Manually add directory to session cache (read approval)
	mgr.dirCache.Set(testDir, ProceedAlways, false)

	// Check should succeed without prompting
	outcome, err := mgr.CheckPathApproval("read_file", testFile, "", false)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if outcome != ProceedAlways {
		t.Errorf("expected ProceedAlways from session cache, got %v", outcome)
	}
}

func TestApprovalManager_CheckPathApproval_RecheckSkipsQueuedPrompt(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-approval-queue-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	file1 := filepath.Join(tempDir, "file1.txt")
	file2 := filepath.Join(tempDir, "file2.txt")
	if err := os.WriteFile(file1, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create file1: %v", err)
	}
	if err := os.WriteFile(file2, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create file2: %v", err)
	}

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	promptStarted := make(chan struct{}, 1)
	promptRelease := make(chan struct{})
	var promptCalls int32

	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
		if atomic.AddInt32(&promptCalls, 1) > 1 {
			return ApprovalResult{}, fmt.Errorf("prompt called more than once")
		}
		promptStarted <- struct{}{}
		<-promptRelease
		return ApprovalResult{
			Choice: ApprovalChoiceDirectory,
			Path:   tempDir,
		}, nil
	}

	oldProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(oldProcs)

	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	run := func(path string) {
		defer wg.Done()
		<-start
		_, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false)
		errCh <- err
	}

	wg.Add(2)
	go run(file1)
	go run(file2)

	close(start)

	select {
	case <-promptStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prompt to start")
	}

	// Give the second goroutine time to queue behind the prompt lock.
	time.Sleep(50 * time.Millisecond)
	close(promptRelease)

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if got := atomic.LoadInt32(&promptCalls); got != 1 {
		t.Fatalf("expected 1 prompt call, got %d", got)
	}
}

func TestApprovalManager_CheckPathApproval_ProjectApprovals(t *testing.T) {
	// Create a git repo for testing
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed, skipping: %v", err)
	}

	// Set up config dir
	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	// Create test file
	testFile := filepath.Join(tempDir, "main.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Pre-approve the repo
	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("failed to load project approvals: %v", err)
	}
	if err := pa.ApproveRead(); err != nil {
		t.Fatalf("failed to approve read: %v", err)
	}

	// Check should succeed from project approvals
	outcome, err := mgr.CheckPathApproval("read_file", testFile, "", false)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if outcome != ProceedAlways {
		t.Errorf("expected ProceedAlways from project approvals, got %v", outcome)
	}
}

func TestApprovalManager_CheckPathApproval_IgnoreProjectApprovals(t *testing.T) {
	// Simulates serve mode: IgnoreProjectApprovals + PromptUIFunc set
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed, skipping: %v", err)
	}

	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)
	mgr.IgnoreProjectApprovals = true

	// Create test file
	testFile := filepath.Join(tempDir, "main.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Pre-approve the repo
	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("failed to load project approvals: %v", err)
	}
	if err := pa.ApproveRead(); err != nil {
		t.Fatalf("failed to approve read: %v", err)
	}

	// With IgnoreProjectApprovals=true, PromptUIFunc should be called
	prompted := false
	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
		prompted = true
		return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
	}

	outcome, err := mgr.CheckPathApproval("read_file", testFile, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !prompted {
		t.Error("expected PromptUIFunc to be called when IgnoreProjectApprovals=true")
	}
	if outcome != ProceedOnce {
		t.Errorf("expected ProceedOnce, got %v", outcome)
	}
}

func TestApprovalManager_CheckShellApproval_PreApproved(t *testing.T) {
	perms := NewToolPermissions()
	perms.ShellAllow = []string{"git *", "go test *"}
	if err := perms.CompileShellPatterns(); err != nil {
		t.Fatalf("failed to compile patterns: %v", err)
	}

	mgr := NewApprovalManager(perms)

	// Test matching pattern
	outcome, err := mgr.CheckShellApproval("git status", "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if outcome != ProceedOnce {
		t.Errorf("expected ProceedOnce for pre-approved command, got %v", outcome)
	}

	outcome, err = mgr.CheckShellApproval("go test ./...", "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if outcome != ProceedOnce {
		t.Errorf("expected ProceedOnce for pre-approved command, got %v", outcome)
	}
}

func TestApprovalManager_CheckShellApproval_SessionCache(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	// Add pattern to session cache
	mgr.shellCache.AddPattern("npm *")

	// Check should succeed without prompting
	outcome, err := mgr.CheckShellApproval("npm install lodash", "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if outcome != ProceedAlways {
		t.Errorf("expected ProceedAlways from session cache, got %v", outcome)
	}
}

func TestApprovalManager_CheckShellApproval_ProjectApprovals(t *testing.T) {
	// Create a git repo for testing
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed, skipping: %v", err)
	}

	// Set up config dir
	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	// Change to repo dir so shell approval detects it
	oldCwd, _ := os.Getwd()
	os.Chdir(tempDir)
	defer os.Chdir(oldCwd)

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	// Pre-approve shell pattern in project
	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("failed to load project approvals: %v", err)
	}
	if err := pa.ApproveShellPattern("make *"); err != nil {
		t.Fatalf("failed to approve pattern: %v", err)
	}

	// Check should succeed from project approvals
	outcome, err := mgr.CheckShellApproval("make build", "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if outcome != ProceedAlways {
		t.Errorf("expected ProceedAlways from project approvals, got %v", outcome)
	}
}

func TestApprovalManager_CheckShellApproval_WorkDirRepoSelection(t *testing.T) {
	// Create two separate git repos.
	repoA, err := os.MkdirTemp("", "test-repo-a-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(repoA)
	repoA, _ = filepath.EvalSymlinks(repoA)

	repoB, err := os.MkdirTemp("", "test-repo-b-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(repoB)
	repoB, _ = filepath.EvalSymlinks(repoB)

	for _, dir := range []string{repoA, repoB} {
		cmd := exec.Command("git", "init")
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Skipf("git init failed, skipping: %v", err)
		}
	}

	// Set up config dir
	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	// Process CWD is repo A.
	oldCwd, _ := os.Getwd()
	os.Chdir(repoA)
	defer os.Chdir(oldCwd)

	// Approve "make *" in repo B only.
	paB, err := LoadProjectApprovals(repoB)
	if err != nil {
		t.Fatalf("failed to load project approvals for repo B: %v", err)
	}
	if err := paB.ApproveShellPattern("make *"); err != nil {
		t.Fatalf("failed to approve pattern in repo B: %v", err)
	}

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	// With workDir pointing at repo B, the approval should match.
	outcome, ok := mgr.checkShellApprovalNoPrompt("make build", repoB)
	if !ok {
		t.Fatal("expected project approval to match when workDir points at repo B")
	}
	if outcome != ProceedAlways {
		t.Errorf("expected ProceedAlways, got %v", outcome)
	}

	// With empty workDir (falls back to CWD = repo A), should NOT match.
	outcome, ok = mgr.checkShellApprovalNoPrompt("make build", "")
	if ok {
		t.Errorf("expected no match when workDir is empty (CWD = repo A), got outcome %v", outcome)
	}
}

func TestApprovalManager_CheckPathApproval_NoPromptFunc(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-approval-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)
	// Don't set PromptFunc or PromptUIFunc

	// Should return error when path is not pre-approved
	outcome, err := mgr.CheckPathApproval("read_file", filepath.Join(tempDir, "file.txt"), "", false)
	if err == nil {
		t.Error("expected error when no prompt func set")
	}
	if outcome != Cancel {
		t.Errorf("expected Cancel outcome, got %v", outcome)
	}
}

func TestApprovalManager_CheckShellApproval_NoPromptFunc(t *testing.T) {
	// Create temp directory and change to it to avoid picking up project approvals
	tempDir, err := os.MkdirTemp("", "test-no-prompt-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	oldCwd, _ := os.Getwd()
	os.Chdir(tempDir)
	defer os.Chdir(oldCwd)

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)
	// Don't set PromptFunc or PromptUIFunc

	// Should return error when command is not pre-approved
	outcome, err := mgr.CheckShellApproval("rm -rf /", "")
	if err == nil {
		t.Error("expected error when no prompt func set")
	}
	if outcome != Cancel {
		t.Errorf("expected Cancel outcome, got %v", outcome)
	}
}

func TestApprovalManager_HandleFileApprovalResult(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	tempDir, err := os.MkdirTemp("", "test-handle-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	tests := []struct {
		name    string
		result  ApprovalResult
		want    ConfirmOutcome
		wantErr bool
	}{
		{
			name:    "cancelled",
			result:  ApprovalResult{Cancelled: true},
			want:    Cancel,
			wantErr: false,
		},
		{
			name:    "deny",
			result:  ApprovalResult{Choice: ApprovalChoiceDeny},
			want:    Cancel,
			wantErr: false,
		},
		{
			name:    "once",
			result:  ApprovalResult{Choice: ApprovalChoiceOnce},
			want:    ProceedOnce,
			wantErr: false,
		},
		{
			name:    "file",
			result:  ApprovalResult{Choice: ApprovalChoiceFile},
			want:    ProceedAlways,
			wantErr: false,
		},
		{
			name:    "directory",
			result:  ApprovalResult{Choice: ApprovalChoiceDirectory, Path: tempDir},
			want:    ProceedAlways,
			wantErr: false,
		},
		{
			name:    "repo_read",
			result:  ApprovalResult{Choice: ApprovalChoiceRepoRead, Path: tempDir},
			want:    ProceedAlways,
			wantErr: false,
		},
		{
			name:    "repo_write",
			result:  ApprovalResult{Choice: ApprovalChoiceRepoWrite, Path: tempDir},
			want:    ProceedAlways,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, err := mgr.handleFileApprovalResult(tt.result, tempDir, false, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("handleFileApprovalResult() error = %v, wantErr %v", err, tt.wantErr)
			}
			if outcome != tt.want {
				t.Errorf("handleFileApprovalResult() = %v, want %v", outcome, tt.want)
			}
		})
	}
}

func TestApprovalManager_HandleShellApprovalResult(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	tests := []struct {
		name    string
		result  ApprovalResult
		want    ConfirmOutcome
		wantErr bool
	}{
		{
			name:    "cancelled",
			result:  ApprovalResult{Cancelled: true},
			want:    Cancel,
			wantErr: false,
		},
		{
			name:    "deny",
			result:  ApprovalResult{Choice: ApprovalChoiceDeny},
			want:    Cancel,
			wantErr: false,
		},
		{
			name:    "once",
			result:  ApprovalResult{Choice: ApprovalChoiceOnce},
			want:    ProceedOnce,
			wantErr: false,
		},
		{
			name:    "command",
			result:  ApprovalResult{Choice: ApprovalChoiceCommand},
			want:    ProceedAlways,
			wantErr: false,
		},
		{
			name:    "pattern",
			result:  ApprovalResult{Choice: ApprovalChoicePattern, Pattern: "git *"},
			want:    ProceedAlways,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, err := mgr.handleShellApprovalResult(tt.result, "git status", nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("handleShellApprovalResult() error = %v, wantErr %v", err, tt.wantErr)
			}
			if outcome != tt.want {
				t.Errorf("handleShellApprovalResult() = %v, want %v", outcome, tt.want)
			}
		})
	}
}

func TestApprovalManager_HandleFileApprovalResult_Directory_AddedToCache(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	tempDir, err := os.MkdirTemp("", "test-cache-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	result := ApprovalResult{
		Choice: ApprovalChoiceDirectory,
		Path:   tempDir,
	}

	_, err = mgr.handleFileApprovalResult(result, tempDir, false, nil)
	if err != nil {
		t.Fatalf("handleFileApprovalResult failed: %v", err)
	}

	testFile := filepath.Join(tempDir, "subdir", "file.txt")
	if err := os.MkdirAll(filepath.Dir(testFile), 0755); err != nil {
		t.Fatalf("mkdir test file dir: %v", err)
	}
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	// Verify directory was added to session cache (read)
	if !mgr.dirCache.IsPathInApprovedDir(testFile, false) {
		t.Error("directory should be in approved cache after approval")
	}
	// Write should NOT be approved
	if mgr.dirCache.IsPathInApprovedDir(testFile, true) {
		t.Error("read-only directory approval should not grant write access")
	}
}

func TestApprovalManager_HandleFileApprovalResult_File_AddedToCache(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	tempDir, err := os.MkdirTemp("", "test-filecache-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	filePath := filepath.Join(tempDir, "image.png")
	if err := os.WriteFile(filePath, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	result := ApprovalResult{
		Choice: ApprovalChoiceFile,
		Path:   filePath,
	}

	_, err = mgr.handleFileApprovalResult(result, filePath, false, nil)
	if err != nil {
		t.Fatalf("handleFileApprovalResult failed: %v", err)
	}

	// The exact file should now be cached for read
	if !mgr.dirCache.IsPathInApprovedDir(filePath, false) {
		t.Error("file should be in approved cache after file-only approval")
	}

	// A different file in the same directory should NOT be cached
	otherFile := filepath.Join(tempDir, "other.png")
	if mgr.dirCache.IsPathInApprovedDir(otherFile, false) {
		t.Error("other files in same dir should not be approved by file-only approval")
	}

	// The same file should NOT be approved for write (only approved for read)
	if mgr.dirCache.IsPathInApprovedDir(filePath, true) {
		t.Error("read-only file approval should not grant write access")
	}
}

func TestApprovalManager_FileApproval_SuppressesReprompt(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	tempDir, err := os.MkdirTemp("", "test-reprompt-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	filePath := filepath.Join(tempDir, "data.txt")
	if err := os.WriteFile(filePath, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	promptCount := 0
	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
		promptCount++
		return ApprovalResult{
			Choice: ApprovalChoiceFile,
			Path:   path,
		}, nil
	}

	// First access — should prompt
	outcome, err := mgr.CheckPathApproval("read_file", filePath, filePath, false)
	if err != nil {
		t.Fatalf("first check failed: %v", err)
	}
	if outcome != ProceedAlways {
		t.Fatalf("expected ProceedAlways, got %v", outcome)
	}
	if promptCount != 1 {
		t.Fatalf("expected 1 prompt, got %d", promptCount)
	}

	// Second access — should NOT prompt (cached)
	outcome, err = mgr.CheckPathApproval("read_file", filePath, filePath, false)
	if err != nil {
		t.Fatalf("second check failed: %v", err)
	}
	if outcome != ProceedAlways {
		t.Fatalf("expected ProceedAlways on second check, got %v", outcome)
	}
	if promptCount != 1 {
		t.Fatalf("expected still 1 prompt after second check, got %d", promptCount)
	}
}

func TestApprovalManager_ReadApprovalDoesNotGrantWrite(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	tempDir, err := os.MkdirTemp("", "test-rw-escalation-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	filePath := filepath.Join(tempDir, "file.txt")
	if err := os.WriteFile(filePath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	promptCount := 0
	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
		promptCount++
		// Always approve the directory for the requested access type
		return ApprovalResult{
			Choice: ApprovalChoiceDirectory,
			Path:   tempDir,
		}, nil
	}

	// Approve read access
	outcome, err := mgr.CheckPathApproval("read_file", filePath, filePath, false)
	if err != nil {
		t.Fatalf("read check failed: %v", err)
	}
	if outcome != ProceedAlways {
		t.Fatalf("expected ProceedAlways for read, got %v", outcome)
	}
	if promptCount != 1 {
		t.Fatalf("expected 1 prompt for read, got %d", promptCount)
	}

	// Write access should still prompt (read approval doesn't grant write)
	outcome, err = mgr.CheckPathApproval("edit_file", filePath, filePath, true)
	if err != nil {
		t.Fatalf("write check failed: %v", err)
	}
	if outcome != ProceedAlways {
		t.Fatalf("expected ProceedAlways for write, got %v", outcome)
	}
	if promptCount != 2 {
		t.Fatalf("expected 2 prompts (read + write), got %d", promptCount)
	}

	// Second read should NOT prompt (still cached from first approval)
	outcome, err = mgr.CheckPathApproval("read_file", filePath, filePath, false)
	if err != nil {
		t.Fatalf("second read check failed: %v", err)
	}
	if promptCount != 2 {
		t.Fatalf("expected still 2 prompts after cached read, got %d", promptCount)
	}

	// Second write should NOT prompt (cached from second approval)
	outcome, err = mgr.CheckPathApproval("edit_file", filePath, filePath, true)
	if err != nil {
		t.Fatalf("second write check failed: %v", err)
	}
	if promptCount != 2 {
		t.Fatalf("expected still 2 prompts after cached write, got %d", promptCount)
	}
}

func TestApprovalManager_HandleShellApprovalResult_Pattern_AddedToCache(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	result := ApprovalResult{
		Choice:  ApprovalChoicePattern,
		Pattern: "cargo *",
	}

	_, err := mgr.handleShellApprovalResult(result, "cargo build", nil)
	if err != nil {
		t.Fatalf("handleShellApprovalResult failed: %v", err)
	}

	// Verify pattern was added to session cache
	patterns := mgr.shellCache.GetPatterns()
	found := false
	for _, p := range patterns {
		if p == "cargo *" {
			found = true
			break
		}
	}
	if !found {
		t.Error("pattern should be in session cache after approval")
	}
}

func TestDirCache_IsPathInApprovedDir(t *testing.T) {
	cache := NewDirCache()

	projectDir := t.TempDir()
	allowedDir := t.TempDir()
	projectFile := filepath.Join(projectDir, "src", "main.go")
	if err := os.MkdirAll(filepath.Dir(projectFile), 0755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(projectFile, []byte("package main"), 0644); err != nil {
		t.Fatalf("write project file: %v", err)
	}
	allowedFile := filepath.Join(allowedDir, "subdir", "file.txt")
	if err := os.MkdirAll(filepath.Dir(allowedFile), 0755); err != nil {
		t.Fatalf("mkdir allowed dir: %v", err)
	}
	if err := os.WriteFile(allowedFile, []byte("content"), 0644); err != nil {
		t.Fatalf("write allowed file: %v", err)
	}
	otherFile := filepath.Join(t.TempDir(), "file.go")
	if err := os.WriteFile(otherFile, []byte("content"), 0644); err != nil {
		t.Fatalf("write other file: %v", err)
	}

	// Add approved directories (read)
	cache.Set(projectDir, ProceedAlways, false)
	cache.Set(allowedDir, ProceedAlways, false)

	tests := []struct {
		path string
		want bool
	}{
		{projectFile, true},
		{projectDir, true},
		{otherFile, false},
		{allowedFile, true},
		{filepath.Join(t.TempDir(), "other"), false},
		{projectDir + "-extra", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := cache.IsPathInApprovedDir(tt.path, false)
			if got != tt.want {
				t.Errorf("IsPathInApprovedDir(%q, false) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestDirCache_ReadWriteSeparation(t *testing.T) {
	cache := NewDirCache()
	projectDir := t.TempDir()
	projectFile := filepath.Join(projectDir, "file.go")
	if err := os.WriteFile(projectFile, []byte("package main"), 0644); err != nil {
		t.Fatalf("write project file: %v", err)
	}
	writeOnlyDir := t.TempDir()
	writeOnlyFile := filepath.Join(writeOnlyDir, "file.txt")
	if err := os.WriteFile(writeOnlyFile, []byte("content"), 0644); err != nil {
		t.Fatalf("write write-only file: %v", err)
	}

	// Approve read for projectDir
	cache.Set(projectDir, ProceedAlways, false)

	// Read should work
	if !cache.IsPathInApprovedDir(projectFile, false) {
		t.Error("read should be approved after read approval")
	}
	// Write should NOT work
	if cache.IsPathInApprovedDir(projectFile, true) {
		t.Error("write should NOT be approved after read-only approval")
	}

	// Now approve write for projectDir
	cache.Set(projectDir, ProceedAlways, true)

	// Both should work
	if !cache.IsPathInApprovedDir(projectFile, false) {
		t.Error("read should still be approved")
	}
	if !cache.IsPathInApprovedDir(projectFile, true) {
		t.Error("write should be approved after write approval")
	}

	// Separate dir with only write approval
	cache.Set(writeOnlyDir, ProceedAlways, true)
	// Write approved
	if !cache.IsPathInApprovedDir(writeOnlyFile, true) {
		t.Error("write should be approved")
	}
	// Read should also work (write implies read)
	if !cache.IsPathInApprovedDir(writeOnlyFile, false) {
		t.Error("read should be approved when write is approved")
	}
}

func TestShellApprovalCache(t *testing.T) {
	cache := NewShellApprovalCache()

	// Initially empty
	if len(cache.GetPatterns()) != 0 {
		t.Error("new cache should be empty")
	}

	// Add patterns
	cache.AddPattern("git *")
	cache.AddPattern("npm test")
	cache.AddPattern("git *") // Duplicate

	patterns := cache.GetPatterns()
	if len(patterns) != 2 {
		t.Errorf("expected 2 patterns (no duplicates), got %d", len(patterns))
	}

	// Verify patterns are present
	hasGit, hasNpm := false, false
	for _, p := range patterns {
		if p == "git *" {
			hasGit = true
		}
		if p == "npm test" {
			hasNpm = true
		}
	}
	if !hasGit || !hasNpm {
		t.Error("expected both patterns to be present")
	}
}

func TestSplitShellCommands(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"cmd1 && cmd2", []string{"cmd1", "cmd2"}},
		{"cmd1 || cmd2", []string{"cmd1", "cmd2"}},
		{"cmd1 ; cmd2", []string{"cmd1", "cmd2"}},
		{"cmd1 | cmd2", []string{"cmd1", "cmd2"}},
		{"cmd1 && cmd2 && cmd3", []string{"cmd1", "cmd2", "cmd3"}},
		{"simple command", []string{"simple command"}},
		{"", nil},
		{"  ", nil},

		// Quoted && is NOT a shell operator
		{"cmd1 'has && inside' && cmd2", []string{"cmd1 'has && inside'", "cmd2"}},
		{`cmd1 "has && inside" && cmd2`, []string{`cmd1 "has && inside"`, "cmd2"}},
		{`ruby -c "true && false"`, []string{`ruby -c "true && false"`}},
		{`ruby -c "true && false" && echo done`, []string{`ruby -c "true && false"`, "echo done"}},

		// Escaped characters
		{`cmd1 \&\& cmd2`, []string{`cmd1 \&\& cmd2`}}, // escaped &&, not an operator
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := splitShellCommands(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitShellCommands(%q) = %v (len %d), want %v (len %d)",
					tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("splitShellCommands(%q)[%d] = %q, want %q", tt.input, i, got[i], w)
				}
			}
		})
	}
}

func TestSplitSequentialCommands(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"cmd1 && cmd2", []string{"cmd1", "cmd2"}},
		{"cmd1 || cmd2", []string{"cmd1", "cmd2"}},
		{"cmd1 ; cmd2", []string{"cmd1", "cmd2"}},
		// Pipe is NOT split — preserved as a single unit
		{"cmd1 | cmd2", []string{"cmd1 | cmd2"}},
		// Mixed: sequential split, pipe preserved
		{"cmd1 | tail && cmd2 | head", []string{"cmd1 | tail", "cmd2 | head"}},
		{"simple command", []string{"simple command"}},
		{"", nil},
		{"  ", nil},
		// Quoted operators preserved
		{"cmd1 'has && inside' && cmd2", []string{"cmd1 'has && inside'", "cmd2"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := splitSequentialCommands(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitSequentialCommands(%q) = %v (len %d), want %v (len %d)",
					tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("splitSequentialCommands(%q)[%d] = %q, want %q", tt.input, i, got[i], w)
				}
			}
		})
	}
}

func TestSplitPipeCommands(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"cmd1 | cmd2", []string{"cmd1", "cmd2"}},
		{"cmd1 | cmd2 | cmd3", []string{"cmd1", "cmd2", "cmd3"}},
		// || is NOT split (it's a sequential operator)
		{"cmd1 || cmd2", []string{"cmd1 || cmd2"}},
		// && is NOT split
		{"cmd1 && cmd2", []string{"cmd1 && cmd2"}},
		{"simple command", []string{"simple command"}},
		{"", nil},
		{"  ", nil},
		// Quoted pipe preserved
		{`cmd1 "a | b" | cmd2`, []string{`cmd1 "a | b"`, "cmd2"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := splitPipeCommands(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitPipeCommands(%q) = %v (len %d), want %v (len %d)",
					tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("splitPipeCommands(%q)[%d] = %q, want %q", tt.input, i, got[i], w)
				}
			}
		})
	}
}

func TestIsSafePipeTarget(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"head -20", true},
		{"tail -f", true},
		{"grep pattern", true},
		{"sort -r", true},
		{"jq '.data'", true},
		{"/usr/bin/tail -30", true},
		{"bash", false},
		{"sh -c 'evil'", false},
		{"xargs rm", false},
		{"python3 -c 'import os'", false},
		{"curl http://example.com", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := isSafePipeTarget(tt.command)
			if got != tt.want {
				t.Errorf("isSafePipeTarget(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestApprovalManager_ShellPatternCacheWorksWithUnsafeSyntax(t *testing.T) {
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	// Simulate user approving "bundle *" pattern
	mgr.shellCache.AddPattern("bundle *")

	// All these commands should be auto-approved from the session cache
	approved := []string{
		"bundle exec stree check lib/foo.rb",
		"bundle exec stree write lib/bar.rb",
		"bundle exec stree check lib/a.rb && bundle exec stree check lib/b.rb",
		"bundle exec ruby -e '$stdout.puts 1'",
		"bundle exec rake db:migrate && bundle exec rake db:seed",
		"bundle exec rspec spec/foo | tail -30",
		"bundle exec rspec | grep -v pending | sort",
	}

	for _, cmd := range approved {
		outcome, err := mgr.CheckShellApproval(cmd, "")
		if err != nil {
			t.Errorf("CheckShellApproval(%q) error: %v", cmd, err)
		}
		if outcome != ProceedAlways {
			t.Errorf("CheckShellApproval(%q) = %v, want ProceedAlways (session cache hit)", cmd, outcome)
		}
	}

	// These should NOT be auto-approved because not all sub-commands match
	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
		return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
	}

	notApproved := []string{
		"bundle exec stree check a.rb && rm -rf /",
		"npm install && bundle exec rake",
		"bundle exec rspec | bash",
		"bundle exec rspec | xargs rm -rf",
	}

	for _, cmd := range notApproved {
		outcome, _ := mgr.CheckShellApproval(cmd, "")
		if outcome == ProceedAlways {
			t.Errorf("CheckShellApproval(%q) should NOT be auto-approved from 'bundle *' pattern", cmd)
		}
	}
}
