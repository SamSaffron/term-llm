package chat

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/worktree"
)

// --- pure / guard-path tests (no git, no cwd dependence) ------------------

func TestWorktreeBaseDirPrefersBinding(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1", WorktreeDir: "/tmp/wt-xyz"}
	if got := m.worktreeBaseDir(); got != "/tmp/wt-xyz" {
		t.Errorf("worktreeBaseDir() = %q, want bound dir", got)
	}
	if got := m.boundWorktreeDir(); got != "/tmp/wt-xyz" {
		t.Errorf("boundWorktreeDir() = %q, want bound dir", got)
	}

	m.sess = &session.Session{ID: "s1"}
	if got := m.boundWorktreeDir(); got != "" {
		t.Errorf("boundWorktreeDir() = %q, want empty on root", got)
	}

	m.sess = nil
	if got := m.boundWorktreeDir(); got != "" {
		t.Errorf("boundWorktreeDir() with nil session = %q, want empty", got)
	}
}

func TestWorktreeCommandsGuardWhenUnbound(t *testing.T) {
	cases := []struct {
		name string
		call func(m *Model) (interface{}, error)
		want string
	}{
		{"pwd", func(m *Model) (interface{}, error) { r, _ := m.cmdWorktreePwd(); return r, nil }, "root checkout"},
		{"diff", func(m *Model) (interface{}, error) { r, _ := m.cmdWorktreeDiff(); return r, nil }, "Not bound"},
		{"promote", func(m *Model) (interface{}, error) { r, _ := m.cmdWorktreePromote([]string{"b"}); return r, nil }, "Not bound"},
		{"rm", func(m *Model) (interface{}, error) { r, _ := m.cmdWorktreeRemove(nil); return r, nil }, "Not bound"},
		{"shell", func(m *Model) (interface{}, error) { r, _ := m.cmdWorktreeShell(nil); return r, nil }, "Not bound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newCmdTestModel(&mockStore{})
			m.sess = &session.Session{ID: "s1"} // no WorktreeDir -> root checkout
			if _, err := tc.call(m); err != nil {
				t.Fatalf("call: %v", err)
			}
			if !strings.Contains(m.footerMessage, tc.want) {
				t.Errorf("footer = %q, want contains %q", m.footerMessage, tc.want)
			}
		})
	}
}

func TestCachedWorktreeSegmentEmptyOnRoot(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1"}
	if seg := m.cachedWorktreeSegment(); seg != "" {
		t.Errorf("segment on root = %q, want empty", seg)
	}
	// A stale cache must be cleared once unbound.
	m.worktreeSegCache = "⌥ stale"
	if seg := m.cachedWorktreeSegment(); seg != "" {
		t.Errorf("segment after unbind = %q, want empty", seg)
	}
}

func TestPathsEqual(t *testing.T) {
	if !pathsEqual("/tmp/a", "/tmp/a") {
		t.Error("identical paths should be equal")
	}
	if pathsEqual("/tmp/a", "/tmp/b") {
		t.Error("different paths should not be equal")
	}
	if pathsEqual("", "/tmp/a") {
		t.Error("empty vs non-empty should not be equal")
	}
	if !pathsEqual("", "") {
		t.Error("empty vs empty should be equal")
	}
}

// --- integration test (real git) -----------------------------------------

func initChatRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return root
}

// TestWorktreeNewBindDiffRemoveFlow drives the full TUI command surface against
// a real repo: create+bind, footer segment, pwd, diff, then force-remove back to
// root. It chdir's the process (the TUI's single-session binding mechanism), so
// it restores cwd and is not parallel-safe.
func TestWorktreeNewBindDiffRemoveFlow(t *testing.T) {
	repo := initChatRepo(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg"))

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir repo: %v", err)
	}

	store := &mockStore{sessions: map[string]*session.Session{}}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "s1"}

	// Create via the core and bind through the chdir model (bindWorktree). The
	// async /worktree-new UI path is covered by TestWorktreeProgressMsgUpdatesAndClears
	// and TestWorktreeCreateDoneBindsSession; this test exercises bind/diff/remove.
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "flowtest", Base: "HEAD"})
	if err != nil {
		t.Fatalf("worktree.Create: %v", err)
	}
	if err := m.bindWorktree(wt.Dir); err != nil {
		t.Fatalf("bindWorktree: %v", err)
	}
	if m.sess.WorktreeDir == "" {
		t.Fatal("session not bound after create")
	}
	if store.updated == nil || store.updated.WorktreeDir == "" {
		t.Fatal("binding not persisted via store.Update")
	}
	// Process cwd should now be inside the worktree.
	if cwd, _ := os.Getwd(); !pathsEqual(cwd, m.sess.WorktreeDir) {
		t.Errorf("cwd %q not the worktree %q", cwd, m.sess.WorktreeDir)
	}

	// Footer segment should reflect the binding.
	seg := m.cachedWorktreeSegment()
	if !strings.Contains(seg, "flowtest") {
		t.Errorf("footer segment = %q, want contains worktree name", seg)
	}
	if !strings.Contains(seg, "detached@") {
		t.Errorf("footer segment = %q, want detached marker", seg)
	}

	// pwd reports the bound path.
	m.cmdWorktreePwd()

	// Modify a tracked file and confirm diff surfaces it.
	if err := os.WriteFile(filepath.Join(m.sess.WorktreeDir, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("modify: %v", err)
	}
	m.invalidateWorktreeSegment()
	if seg := m.cachedWorktreeSegment(); !strings.Contains(seg, "±") {
		t.Errorf("dirty segment = %q, want dirty count", seg)
	}
	res, _ := m.cmdWorktreeDiff()
	rm := res.(*Model)
	if !strings.Contains(rm.dialog.Content(), "+world") {
		t.Errorf("diff dialog missing change:\n%s", rm.dialog.Content())
	}

	// Dirty remove without force must refuse.
	m.cmdWorktreeRemove(nil)
	if !strings.Contains(m.footerMessage, "uncommitted") {
		t.Errorf("dirty rm footer = %q, want refusal", m.footerMessage)
	}
	if m.sess.WorktreeDir == "" {
		t.Error("session should still be bound after refused remove")
	}

	// Forced remove succeeds and rebinds to root.
	m.cmdWorktreeRemove([]string{"force"})
	if m.sess.WorktreeDir != "" {
		t.Errorf("session still bound after force remove: %q", m.sess.WorktreeDir)
	}
	if cwd, _ := os.Getwd(); !pathsEqual(cwd, repo) {
		t.Errorf("cwd %q not back at root %q", cwd, repo)
	}
	if seg := m.cachedWorktreeSegment(); seg != "" {
		t.Errorf("segment after remove = %q, want empty", seg)
	}
}

// TestWorktreeSwitchToRootClearsBinding verifies "/worktree root" unbinds.
func TestWorktreeSwitchToRootClearsBinding(t *testing.T) {
	repo := initChatRepo(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg"))
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	store := &mockStore{sessions: map[string]*session.Session{}}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "s1"}

	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "rootswitch", Base: "HEAD"})
	if err != nil {
		t.Fatalf("worktree.Create: %v", err)
	}
	if err := m.bindWorktree(wt.Dir); err != nil {
		t.Fatalf("bindWorktree: %v", err)
	}
	if m.sess.WorktreeDir == "" {
		t.Fatalf("expected binding after create")
	}
	m.cmdWorktreeSwitch("root")
	if m.sess.WorktreeDir != "" {
		t.Errorf("binding not cleared after switch root: %q", m.sess.WorktreeDir)
	}
	if cwd, _ := os.Getwd(); !pathsEqual(cwd, repo) {
		t.Errorf("cwd %q not root %q", cwd, repo)
	}
	// Cleanup the created worktree so it doesn't linger in the repo metadata.
	t.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "prune")
		cmd.Dir = repo
		_ = cmd.Run()
	})
}

// TestWorktreeSwitcherEnterBindsHighlighted drives the modal switcher against a
// real repo: open it (verifying the root/worktree/new rows), highlight the
// worktree row, press enter, and confirm the session binds via the chdir model.
func TestWorktreeSwitcherEnterBindsHighlighted(t *testing.T) {
	repo := initChatRepo(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg"))
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "prune")
		cmd.Dir = repo
		_ = cmd.Run()
	})

	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "switcher", Base: "HEAD"})
	if err != nil {
		t.Fatalf("worktree.Create: %v", err)
	}

	store := &mockStore{sessions: map[string]*session.Session{}}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "s1"}

	if _, cmd := m.showWorktreeSwitcher(); cmd != nil {
		t.Fatal("showWorktreeSwitcher should open a dialog, not return a command")
	}
	if m.dialog.Type() != DialogWorktreePicker {
		t.Fatalf("dialog type = %v, want worktree picker", m.dialog.Type())
	}
	// Rows are: root (synthetic), the worktree, then the "+ new worktree…" row.
	if first := m.dialog.ItemAt(0); first == nil || first.ID != "__root__" {
		t.Fatalf("first row = %+v, want __root__", first)
	}
	wtIdx := -1
	for i := 0; ; i++ {
		it := m.dialog.ItemAt(i)
		if it == nil {
			if wtIdx < 0 {
				t.Fatal("worktree row not present in picker")
			}
			if last := m.dialog.ItemAt(i - 1); last == nil || last.ID != "__new__" {
				t.Fatalf("last row = %+v, want __new__", last)
			}
			break
		}
		if it.ID == wt.Dir {
			wtIdx = i
		}
	}
	m.dialog.SetCursor(wtIdx)

	res, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	rm := res.(*Model)
	if rm.dialog.IsOpen() {
		t.Fatal("dialog should close after switch")
	}
	if !pathsEqual(rm.sess.WorktreeDir, wt.Dir) {
		t.Errorf("bound to %q, want %q", rm.sess.WorktreeDir, wt.Dir)
	}
	if cwd, _ := os.Getwd(); !pathsEqual(cwd, wt.Dir) {
		t.Errorf("cwd %q not the worktree %q", cwd, wt.Dir)
	}
}

// TestWorktreeSwitcherDeleteRequiresTwoPresses verifies the in-place delete
// confirm: a first "d" arms the highlighted row and warns without deleting.
func TestWorktreeSwitcherDeleteRequiresTwoPresses(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1"}
	m.dialog.ShowWorktreePicker([]DialogItem{{ID: "/tmp/wt-x", Label: "neon-canyon"}}, "/tmp/wt-x")

	res, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'd'})
	rm := res.(*Model)
	if !rm.dialog.IsOpen() {
		t.Fatal("first delete press should keep the dialog open")
	}
	if rm.worktreeDeleteTarget != "/tmp/wt-x" {
		t.Fatalf("delete target = %q, want armed to /tmp/wt-x", rm.worktreeDeleteTarget)
	}
	if !strings.Contains(rm.footerMessage, "Press d again") {
		t.Fatalf("footer = %q, want two-press prompt", rm.footerMessage)
	}
	if rm.worktreeBusy {
		t.Fatal("first delete press must not start a removal")
	}
}

// TestWorktreeShellCommandConstruction checks the shell-drop command: an
// interactive $SHELL rooted in the worktree by default, and a tmux split with a
// new-window fallback (carrying the quoted dir) under --tmux.
func TestWorktreeShellCommandConstruction(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	cmd := worktreeShellCommand("/tmp/wt-abc", false)
	if filepath.Base(cmd.Path) != "zsh" {
		t.Errorf("shell path = %q, want zsh", cmd.Path)
	}
	if cmd.Dir != "/tmp/wt-abc" {
		t.Errorf("shell cwd = %q, want /tmp/wt-abc", cmd.Dir)
	}

	t.Setenv("SHELL", "")
	if def := worktreeShellCommand("/tmp/wt-abc", false); def.Path != "/bin/sh" {
		t.Errorf("default shell path = %q, want /bin/sh", def.Path)
	}

	tmux := worktreeShellCommand("/tmp/wt abc", true)
	if filepath.Base(tmux.Path) != "sh" {
		t.Errorf("tmux command path = %q, want sh", tmux.Path)
	}
	joined := strings.Join(tmux.Args, " ")
	if !strings.Contains(joined, "tmux split-window -c") || !strings.Contains(joined, "tmux new-window -c") {
		t.Errorf("tmux args = %q, want split-window + new-window fallback", joined)
	}
	if !strings.Contains(joined, strconv.Quote("/tmp/wt abc")) {
		t.Errorf("tmux args = %q, want quoted worktree dir", joined)
	}
	if tmux.Dir != "" {
		t.Errorf("tmux command cwd = %q, want empty (tmux -c carries it)", tmux.Dir)
	}
}

// TestOpenWorktreeShellGuards covers the no-dir and --tmux-without-tmux guards.
func TestOpenWorktreeShellGuards(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1"}

	m.openWorktreeShell("", false)
	if !strings.Contains(m.footerMessage, "No worktree directory selected") {
		t.Errorf("empty-dir footer = %q", m.footerMessage)
	}

	t.Setenv("TMUX", "")
	m.openWorktreeShell("/tmp/wt", true)
	if !strings.Contains(m.footerMessage, "requires an existing tmux session") {
		t.Errorf("tmux-guard footer = %q", m.footerMessage)
	}
}

// TestWorktreeShellDoneMsgClearsState verifies the shell-return handler reports
// success and refreshes the worktree footer segment.
func TestWorktreeShellDoneMsgClearsState(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1"}
	m.worktreeSegCache = "stale"

	res, _ := m.Update(worktreeShellDoneMsg{})
	rm := res.(*Model)
	if rm.worktreeSegCache != "" {
		t.Errorf("worktree segment cache = %q, want invalidated", rm.worktreeSegCache)
	}
	if !strings.Contains(rm.footerMessage, "Returned from worktree shell") {
		t.Errorf("footer = %q, want shell-return notice", rm.footerMessage)
	}
}

// TestWorktreeProgressMsgUpdatesAndClears drives the async create message flow:
// a progress event updates the live status text and reschedules the listener,
// then a create-done (error) clears the busy state and surfaces the failure.
func TestWorktreeProgressMsgUpdatesAndClears(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1"}
	m.worktreeBusy = true
	m.worktreeOpSeq = 7
	progress := make(chan worktree.Progress)

	result, cmd := m.Update(worktreeProgressMsg{op: 7, progress: progress, message: "running setup script"})
	rm := result.(*Model)
	if rm.worktreeProgress != "Creating worktree: running setup script" {
		t.Fatalf("worktree progress = %q", rm.worktreeProgress)
	}
	if cmd == nil {
		t.Fatal("expected progress listener to be rescheduled")
	}

	result, _ = rm.Update(worktreeCreateDoneMsg{op: 7, err: errors.New("boom")})
	rm = result.(*Model)
	if rm.worktreeBusy {
		t.Fatal("worktreeBusy should clear on create completion")
	}
	if rm.worktreeProgress != "" {
		t.Fatalf("worktree progress after completion = %q, want empty", rm.worktreeProgress)
	}
	if !strings.Contains(rm.footerMessage, "Worktree create failed") {
		t.Fatalf("footer = %q, want create failure", rm.footerMessage)
	}
}

// TestWorktreeCreateDoneBindsSession verifies the create-done handler binds the
// session to the new worktree via the chdir model on success.
func TestWorktreeCreateDoneBindsSession(t *testing.T) {
	repo := initChatRepo(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg"))
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	store := &mockStore{sessions: map[string]*session.Session{}}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "s1"}
	m.worktreeBusy = true
	m.worktreeOpSeq = 3

	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "donebind", Base: "HEAD"})
	if err != nil {
		t.Fatalf("worktree.Create: %v", err)
	}
	t.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "prune")
		cmd.Dir = repo
		_ = cmd.Run()
	})

	res, _ := m.Update(worktreeCreateDoneMsg{op: 3, wt: wt})
	rm := res.(*Model)
	if rm.worktreeBusy {
		t.Fatal("worktreeBusy should clear after create-done")
	}
	if !pathsEqual(rm.sess.WorktreeDir, wt.Dir) {
		t.Errorf("session bound to %q, want %q", rm.sess.WorktreeDir, wt.Dir)
	}
	if cwd, _ := os.Getwd(); !pathsEqual(cwd, wt.Dir) {
		t.Errorf("cwd %q not the worktree %q", cwd, wt.Dir)
	}
	if !strings.Contains(rm.footerMessage, "Created and switched") {
		t.Errorf("footer = %q, want success", rm.footerMessage)
	}
}
