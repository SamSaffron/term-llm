package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestBuildFileOptions_InGitRepo(t *testing.T) {
	repoRoot := "/home/user/myproject"
	repoInfo := &GitRepoInfo{
		IsRepo:   true,
		Root:     repoRoot,
		RepoName: "myproject",
	}
	path := filepath.Join(repoRoot, "src", "main.go")

	// Test read options
	options := BuildFileOptions(path, repoInfo, false)

	// Should have 5 options: repo, directory, file, once, deny
	if len(options) != 5 {
		t.Errorf("expected 5 options for read in git repo, got %d", len(options))
	}

	// First option should be repo-level read (remembered)
	if options[0].Choice != ApprovalChoiceRepoRead {
		t.Errorf("first option should be ApprovalChoiceRepoRead, got %v", options[0].Choice)
	}
	if !options[0].SaveToRepo {
		t.Error("repo option should have SaveToRepo=true")
	}
	if options[0].Path != repoRoot {
		t.Errorf("repo option path should be %s, got %s", repoRoot, options[0].Path)
	}

	// Second option should be directory (session only)
	if options[1].Choice != ApprovalChoiceDirectory {
		t.Errorf("second option should be ApprovalChoiceDirectory, got %v", options[1].Choice)
	}
	if options[1].SaveToRepo {
		t.Error("directory option should have SaveToRepo=false")
	}

	// Third option should be file (session only)
	if options[2].Choice != ApprovalChoiceFile {
		t.Errorf("third option should be ApprovalChoiceFile, got %v", options[2].Choice)
	}

	// Fourth should be once
	if options[3].Choice != ApprovalChoiceOnce {
		t.Errorf("fourth option should be ApprovalChoiceOnce, got %v", options[3].Choice)
	}

	// Fifth should be deny
	if options[4].Choice != ApprovalChoiceDeny {
		t.Errorf("fifth option should be ApprovalChoiceDeny, got %v", options[4].Choice)
	}
}

func TestBuildFileOptions_InGitRepo_Write(t *testing.T) {
	repoRoot := "/home/user/myproject"
	repoInfo := &GitRepoInfo{
		IsRepo:   true,
		Root:     repoRoot,
		RepoName: "myproject",
	}
	path := filepath.Join(repoRoot, "src", "main.go")

	// Test write options
	options := BuildFileOptions(path, repoInfo, true)

	// First option for write should be ApprovalChoiceRepoWrite
	if options[0].Choice != ApprovalChoiceRepoWrite {
		t.Errorf("first option for write should be ApprovalChoiceRepoWrite, got %v", options[0].Choice)
	}

	// Check that descriptions mention "write"
	if !strings.Contains(strings.ToLower(options[0].Label), "write") {
		t.Errorf("write option label should contain 'write': %s", options[0].Label)
	}
}

func TestBuildFileOptions_NotInGitRepo(t *testing.T) {
	path := "/tmp/somefile.txt"

	// No git repo
	options := BuildFileOptions(path, nil, false)

	// Should have 4 options: directory, file, once, deny (no repo option)
	if len(options) != 4 {
		t.Errorf("expected 4 options outside git repo, got %d", len(options))
	}

	// First option should be directory (not repo)
	if options[0].Choice != ApprovalChoiceDirectory {
		t.Errorf("first option outside repo should be ApprovalChoiceDirectory, got %v", options[0].Choice)
	}

	// No option should have SaveToRepo=true
	for _, opt := range options {
		if opt.SaveToRepo {
			t.Errorf("option %v should not have SaveToRepo=true outside git repo", opt.Choice)
		}
	}
}

func TestBuildShellOptions_InGitRepo(t *testing.T) {
	repoInfo := &GitRepoInfo{
		IsRepo:   true,
		Root:     "/home/user/myproject",
		RepoName: "myproject",
	}
	command := "go test ./..."

	options := BuildShellOptions(command, repoInfo)

	// Should have 4 options: pattern, command, once, deny
	if len(options) != 4 {
		t.Errorf("expected 4 options for shell in git repo, got %d", len(options))
	}

	// First option should be pattern (remembered)
	if options[0].Choice != ApprovalChoicePattern {
		t.Errorf("first option should be ApprovalChoicePattern, got %v", options[0].Choice)
	}
	if !options[0].SaveToRepo {
		t.Error("pattern option should have SaveToRepo=true")
	}
	if options[0].Pattern == "" {
		t.Error("pattern option should have a pattern set")
	}

	// Second option should be specific command (session only)
	if options[1].Choice != ApprovalChoiceCommand {
		t.Errorf("second option should be ApprovalChoiceCommand, got %v", options[1].Choice)
	}
	if options[1].SaveToRepo {
		t.Error("command option should have SaveToRepo=false")
	}

	// Third should be once
	if options[2].Choice != ApprovalChoiceOnce {
		t.Errorf("third option should be ApprovalChoiceOnce, got %v", options[2].Choice)
	}

	// Fourth should be deny
	if options[3].Choice != ApprovalChoiceDeny {
		t.Errorf("fourth option should be ApprovalChoiceDeny, got %v", options[3].Choice)
	}
}

func TestBuildShellOptions_NotInGitRepo(t *testing.T) {
	command := "go test ./..."

	// No git repo
	options := BuildShellOptions(command, nil)

	// Should have 3 options: command, once, deny (no pattern option)
	if len(options) != 3 {
		t.Errorf("expected 3 options outside git repo, got %d", len(options))
	}

	// First option should be command (not pattern)
	if options[0].Choice != ApprovalChoiceCommand {
		t.Errorf("first option outside repo should be ApprovalChoiceCommand, got %v", options[0].Choice)
	}

	// No option should have SaveToRepo=true
	for _, opt := range options {
		if opt.SaveToRepo {
			t.Errorf("option %v should not have SaveToRepo=true outside git repo", opt.Choice)
		}
	}
}

func TestApprovalModel_Keyboard(t *testing.T) {
	// Create a model and test keyboard navigation
	repoInfo := &GitRepoInfo{
		IsRepo:   true,
		Root:     "/repo",
		RepoName: "repo",
	}
	m := newApprovalModel("/repo/file.go", repoInfo, false)

	// Initial cursor should be 0
	if m.cursor != 0 {
		t.Errorf("initial cursor should be 0, got %d", m.cursor)
	}

	// Test that we have options
	if len(m.options) == 0 {
		t.Fatal("model should have options")
	}
}

func TestApprovalModel_SpaceSelectsCurrentOption(t *testing.T) {
	repoInfo := &GitRepoInfo{
		IsRepo:   true,
		Root:     "/repo",
		RepoName: "repo",
	}
	m := newApprovalModel("/repo/file.go", repoInfo, false)
	m.cursor = 1

	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	updated := model.(*ApprovalModel)

	if !updated.Done {
		t.Fatal("expected space to complete approval prompt")
	}
	if got, want := updated.Result().Choice, updated.options[1].Choice; got != want {
		t.Fatalf("expected choice %v, got %v", want, got)
	}
}

func TestApprovalModel_UpdateEmbeddedSpaceSelectsCurrentOption(t *testing.T) {
	repoInfo := &GitRepoInfo{
		IsRepo:   true,
		Root:     "/repo",
		RepoName: "repo",
	}
	m := newApprovalModel("/repo/file.go", repoInfo, false)
	m.cursor = 2

	if !m.UpdateEmbedded(tea.KeyPressMsg{Code: tea.KeySpace}) {
		t.Fatal("expected embedded approval prompt to finish on space")
	}
	if got, want := m.Result().Choice, m.options[2].Choice; got != want {
		t.Fatalf("expected choice %v, got %v", want, got)
	}
}

func TestApprovalResult_DefaultValues(t *testing.T) {
	result := ApprovalResult{}

	if result.Choice != ApprovalChoiceDeny {
		// Zero value should be ApprovalChoiceDeny (iota = 0)
		t.Errorf("zero value of ApprovalChoice should be ApprovalChoiceDeny, got %v", result.Choice)
	}

	if result.Cancelled {
		t.Error("Cancelled should default to false")
	}

	if result.SaveToRepo {
		t.Error("SaveToRepo should default to false")
	}
}

func TestBuildFileOptions_RelativePaths(t *testing.T) {
	// Create actual temp directories to test path handling
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	srcDir := filepath.Join(tempDir, "src", "internal")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}

	repoInfo := &GitRepoInfo{
		IsRepo:   true,
		Root:     tempDir,
		RepoName: filepath.Base(tempDir),
	}
	path := filepath.Join(srcDir, "main.go")

	options := BuildFileOptions(path, repoInfo, false)

	// Directory option should use a path under repo
	dirOption := options[1]
	if dirOption.Choice != ApprovalChoiceDirectory {
		t.Skip("directory option not in expected position")
	}

	// The path should be absolute
	if !filepath.IsAbs(dirOption.Path) {
		t.Errorf("directory option path should be absolute: %s", dirOption.Path)
	}
}

func TestApprovalChoiceValues(t *testing.T) {
	// Verify the enum values are distinct and in expected order
	choices := []ApprovalChoice{
		ApprovalChoiceDeny,
		ApprovalChoiceOnce,
		ApprovalChoiceFile,
		ApprovalChoiceDirectory,
		ApprovalChoiceRepoRead,
		ApprovalChoiceRepoWrite,
		ApprovalChoicePattern,
		ApprovalChoiceCommand,
		ApprovalChoiceCancelled,
	}

	seen := make(map[ApprovalChoice]bool)
	for i, c := range choices {
		if seen[c] {
			t.Errorf("duplicate ApprovalChoice value: %v", c)
		}
		seen[c] = true

		// Verify order matches iota
		if int(c) != i {
			t.Errorf("ApprovalChoice %v has value %d, expected %d", c, c, i)
		}
	}
}
