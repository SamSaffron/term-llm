package chat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// TestCmdHandoverFileModeUsesSessionPromptPath ensures /handover reads the
// exact file this session's agent was told about via {{handover_path}} (as
// recorded in its persisted system prompt), even when another .md in the
// handover directory — e.g. from a concurrent session — has a newer
// modification time.
func TestCmdHandoverFileModeUsesSessionPromptPath(t *testing.T) {
	tmp := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg-data"))

	planPath, err := session.GetHandoverPath(".", time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("GetHandoverPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(planPath, []byte("the session plan"), 0o644); err != nil {
		t.Fatalf("WriteFile plan: %v", err)
	}

	// A concurrent session's document with a newer mtime must not shadow it.
	decoy := filepath.Join(filepath.Dir(planPath), "2026-01-01-other-session-plan.md")
	if err := os.WriteFile(decoy, []byte("someone else's plan"), 0o644); err != nil {
		t.Fatalf("WriteFile decoy: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(decoy, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	m := newFileHandoverTestModel(planPath, "prompt-path-handover")
	_, cmd := m.cmdHandover([]string{"@developer"})
	assertHandoverDocument(t, cmd, "the session plan")
}

// TestCmdHandoverFileModeIgnoresDecoyWhenPinnedUnwritten ensures that when
// the prompt names a pinned plan file that has not been written yet, file
// mode does NOT fall back to a newer .md from a concurrent session.
func TestCmdHandoverFileModeIgnoresDecoyWhenPinnedUnwritten(t *testing.T) {
	tmp := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg-data"))

	planPath, err := session.GetHandoverPath(".", time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("GetHandoverPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	decoy := filepath.Join(filepath.Dir(planPath), "2026-01-01-other-session-plan.md")
	if err := os.WriteFile(decoy, []byte("someone else's plan"), 0o644); err != nil {
		t.Fatalf("WriteFile decoy: %v", err)
	}

	m := newFileHandoverTestModel(planPath, "unwritten-pinned")
	assertHandoverDoesNotComplete(t, m)
}

func TestCmdHandoverFileModeUsesBoundWorktreePath(t *testing.T) {
	processDir := t.TempDir()
	worktreeDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(processDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg-data"))

	planPath, err := session.GetHandoverPath(worktreeDir, time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("GetHandoverPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatalf("MkdirAll plan: %v", err)
	}
	prettyPath := filepath.Join(filepath.Dir(planPath), "2026-07-10-worktree-plan.md")
	if err := os.WriteFile(prettyPath, []byte("the worktree plan"), 0o644); err != nil {
		t.Fatalf("WriteFile plan: %v", err)
	}
	if err := os.Symlink(filepath.Base(prettyPath), planPath); err != nil {
		t.Fatalf("Symlink plan: %v", err)
	}

	processHandoverDir, err := session.GetHandoverDir(processDir)
	if err != nil {
		t.Fatalf("GetHandoverDir process: %v", err)
	}
	if err := os.MkdirAll(processHandoverDir, 0o755); err != nil {
		t.Fatalf("MkdirAll decoy: %v", err)
	}
	decoy := filepath.Join(processHandoverDir, "2026-07-10-other-session-plan.md")
	if err := os.WriteFile(decoy, []byte("the process decoy"), 0o644); err != nil {
		t.Fatalf("WriteFile decoy: %v", err)
	}

	m := newFileHandoverTestModel(planPath, "worktree-pinned")
	m.sess.CWD = worktreeDir
	m.sess.WorktreeDir = worktreeDir
	assertHandoverDocument(t, handoverCommand(t, m), "the worktree plan")
}

func TestCmdHandoverFileModeBoundWorktreeUnwrittenNeverUsesProcessDecoy(t *testing.T) {
	processDir := t.TempDir()
	worktreeDir := t.TempDir()
	oldWD, _ := os.Getwd()
	if err := os.Chdir(processDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg-data"))

	planPath, err := session.GetHandoverPath(worktreeDir, time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("GetHandoverPath: %v", err)
	}
	processHandoverDir, _ := session.GetHandoverDir(processDir)
	if err := os.MkdirAll(processHandoverDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(processHandoverDir, "2026-07-10-other-session-plan.md"), []byte("the process decoy"), 0o644); err != nil {
		t.Fatalf("WriteFile decoy: %v", err)
	}

	m := newFileHandoverTestModel(planPath, "worktree-unwritten")
	m.sess.CWD = worktreeDir
	m.sess.WorktreeDir = worktreeDir
	assertHandoverDoesNotComplete(t, m)
}

func TestResolveHandoverPathAfterWorktreeSwitchUsesOriginalProcessPath(t *testing.T) {
	processDir := t.TempDir()
	worktreeDir := t.TempDir()
	oldWD, _ := os.Getwd()
	if err := os.Chdir(processDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg-data"))

	planPath, _ := session.GetHandoverPath(processDir, "2026-07-10")
	m := newFileHandoverTestModel(planPath, "switched")
	m.sess.CWD = worktreeDir
	m.sess.WorktreeDir = worktreeDir

	got, _, pinned, err := m.resolveHandoverPath(m.currentSystemPromptText())
	if err != nil || !pinned || got != planPath {
		t.Fatalf("resolveHandoverPath = (%q, %v, %v), want (%q, true, nil)", got, pinned, err, planPath)
	}
}

func TestMaybeNameHandoverUsesBoundWorktreePath(t *testing.T) {
	processDir := t.TempDir()
	worktreeDir := t.TempDir()
	oldWD, _ := os.Getwd()
	if err := os.Chdir(processDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg-data"))

	planPath, _ := session.GetHandoverPath(worktreeDir, "2026-07-10")
	m := newFileHandoverTestModel(planPath, "name-worktree")
	m.sess.CWD = worktreeDir
	m.sess.WorktreeDir = worktreeDir
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse("worktree plan")

	cmd := m.maybeNameHandoverCmd("fix worktree handovers")
	if cmd == nil {
		t.Fatal("expected rename command")
	}
	msg := cmd()
	if done, ok := msg.(handoverRenameDoneMsg); !ok || done.err != nil {
		t.Fatalf("rename command = %#v", msg)
	}
	if fi, err := os.Lstat(planPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("worktree plan was not prettified: info=%v err=%v", fi, err)
	}
}

func TestMaybeRenameHandoverUsesBoundWorktreePath(t *testing.T) {
	processDir := t.TempDir()
	worktreeDir := t.TempDir()
	oldWD, _ := os.Getwd()
	if err := os.Chdir(processDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg-data"))

	worktreeHandoverDir, _ := session.GetHandoverDir(worktreeDir)
	processHandoverDir, _ := session.GetHandoverDir(processDir)
	if err := os.MkdirAll(worktreeHandoverDir, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := os.MkdirAll(processHandoverDir, 0o755); err != nil {
		t.Fatalf("MkdirAll process: %v", err)
	}
	planPath := filepath.Join(worktreeHandoverDir, "2026-07-10-amber-creek-bloom.md")
	if err := os.WriteFile(planPath, []byte(string(make([]byte, 1200))), 0o644); err != nil {
		t.Fatalf("WriteFile plan: %v", err)
	}
	decoy := filepath.Join(processHandoverDir, "2026-07-10-frost-cedar-oak.md")
	if err := os.WriteFile(decoy, []byte(string(make([]byte, 1200))), 0o644); err != nil {
		t.Fatalf("WriteFile decoy: %v", err)
	}

	m := newFileHandoverTestModel(planPath, "rename-worktree")
	m.sess.CWD = worktreeDir
	m.sess.WorktreeDir = worktreeDir
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse("worktree handover")

	cmd := m.maybeRenameHandoverCmd()
	if cmd == nil {
		t.Fatal("expected rename command")
	}
	msg := cmd()
	if done, ok := msg.(handoverRenameDoneMsg); !ok || done.err != nil {
		t.Fatalf("rename command = %#v", msg)
	}
	if fi, err := os.Lstat(planPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("worktree plan was not renamed: info=%v err=%v", fi, err)
	}
	if fi, err := os.Lstat(decoy); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("process decoy was changed: info=%v err=%v", fi, err)
	}
}

func TestCmdHandoverFileModeLegacyPromptScansEffectiveDirectory(t *testing.T) {
	worktreeDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg-data"))
	handoverDir, err := session.GetHandoverDir(worktreeDir)
	if err != nil {
		t.Fatalf("GetHandoverDir: %v", err)
	}
	if err := os.MkdirAll(handoverDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacyPath := filepath.Join(handoverDir, "2026-07-10-legacy-plan.md")
	if err := os.WriteFile(legacyPath, []byte("legacy session plan"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := newFileHandoverTestModel("", "legacy-prompt")
	m.sess.CWD = worktreeDir
	m.sess.WorktreeDir = worktreeDir
	m.messages = []session.Message{*session.NewMessage(m.sess.ID, llm.SystemText("Legacy planner prompt without an assigned path."), -1)}
	assertHandoverDocument(t, handoverCommand(t, m), "legacy session plan")
}

func newFileHandoverTestModel(planPath, id string) *Model {
	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.config = &config.Config{}
	m.sess = &session.Session{ID: id, CreatedAt: time.Now().Add(-time.Minute)}
	m.agentName = "planner"
	m.currentAgent = &agents.Agent{Name: "planner", EnableHandover: true, HandoverMode: "file"}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return &agents.Agent{Name: name, SystemPrompt: "You are target."}, nil
	}
	sysPrompt := "Your plan lives at exactly this path, decided upfront and fixed for this session:\n\n`" + planPath + "`"
	m.messages = []session.Message{*session.NewMessage(m.sess.ID, llm.SystemText(sysPrompt), -1)}
	return m
}

func handoverCommand(t *testing.T, m *Model) tea.Cmd {
	t.Helper()
	_, cmd := m.cmdHandover([]string{"@developer"})
	if cmd == nil {
		t.Fatal("expected handover command")
	}
	return cmd
}

func assertHandoverDocument(t *testing.T, cmd tea.Cmd, want string) {
	t.Helper()
	msg := cmd()
	done, ok := msg.(handoverDoneMsg)
	if !ok || done.result == nil {
		t.Fatalf("handover command returned %T, want handoverDoneMsg", msg)
	}
	if done.result.Document != want {
		t.Fatalf("handover document = %q, want %q", done.result.Document, want)
	}
}

func assertHandoverDoesNotComplete(t *testing.T, m *Model) {
	t.Helper()
	_, cmd := m.cmdHandover([]string{"@developer"})
	if cmd != nil {
		if done, ok := cmd().(handoverDoneMsg); ok && done.result != nil {
			t.Fatalf("handover must not proceed with another session's document: %q", done.result.Document)
		}
	}
}
