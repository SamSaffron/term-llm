package chat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func commitTUIGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func commitTUIModel(t *testing.T) (*Model, string) {
	t.Helper()
	dir := t.TempDir()
	commitTUIGit(t, dir, "init", "-q")
	commitTUIGit(t, dir, "config", "user.name", "Test")
	commitTUIGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	commitTUIGit(t, dir, "add", "-A")
	commitTUIGit(t, dir, "commit", "-qm", "base")
	sess := &session.Session{ID: "tui-commit", CWD: dir}
	model := New(&config.Config{}, llm.NewMockProvider("mock"), nil, "", "", nil, 0, false, false, false, nil, "", "", false, "", nil, sess, false, nil, false, true, "", "", false)
	model.SetRootContext(context.Background())
	return model, dir
}
func applyCommitCmd(t *testing.T, m *Model, cmd tea.Cmd) *Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	updated, next := m.Update(msg)
	m = updated.(*Model)
	if next != nil && m.commit != nil && m.commit.Phase != CommitEditing {
		return applyCommitCmd(t, m, next)
	}
	return m
}

func TestCommitCommandRegisteredAndComPrefixAmbiguous(t *testing.T) {
	found := false
	for _, command := range AllCommands() {
		if command.Name == "commit" {
			found = true
		}
	}
	if !found {
		t.Fatal("commit command missing")
	}
	matches := FilterCommands("com")
	var names []string
	for _, match := range matches {
		if strings.HasPrefix(match.Name, "com") {
			names = append(names, match.Name)
		}
	}
	if len(names) != 2 || names[0] != "commit" || names[1] != "compact" {
		t.Fatalf("/com matches = %v", names)
	}
}

func TestCommitEmptyIndexAutomaticallyStagesAllAndOffersManualMessage(t *testing.T) {
	m, dir := commitTUIModel(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.ExecuteCommand("/commit")
	m = updated.(*Model)
	m = applyCommitCmd(t, m, cmd)
	if m.commit == nil || m.commit.Phase != CommitEditing {
		t.Fatalf("state=%+v", m.commit)
	}
	if got := commitTUICaptureGit(t, dir, "diff", "--cached", "--name-only"); got != "base.txt" {
		t.Fatalf("staged=%q", got)
	}
	if !strings.Contains(m.commit.Error, "write a message manually") {
		t.Fatalf("error=%q", m.commit.Error)
	}
}

func TestCommitExistingIndexRequiresExplicitChoice(t *testing.T) {
	m, dir := commitTUIModel(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("staged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	commitTUIGit(t, dir, "add", "base.txt")
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.ExecuteCommand("/commit only base")
	m = updated.(*Model)
	m = applyCommitCmd(t, m, cmd)
	if m.commit == nil || m.commit.Phase != CommitChoosing {
		t.Fatalf("state=%+v", m.commit)
	}
	updated, cmd = m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	m = updated.(*Model)
	m = applyCommitCmd(t, m, cmd)
	if m.commit.Phase != CommitEditing {
		t.Fatalf("phase=%s error=%s", m.commit.Phase, m.commit.Error)
	}
	if got := commitTUICaptureGit(t, dir, "diff", "--cached", "--name-only"); got != "base.txt" {
		t.Fatalf("staged-only changed index: %q", got)
	}
}
func commitTUICaptureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
