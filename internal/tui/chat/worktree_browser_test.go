package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
	worktreesui "github.com/samsaffron/term-llm/internal/tui/worktrees"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func TestWorktreeCommandMetadataUsesBrowseOnly(t *testing.T) {
	var found *Command
	for _, command := range AllCommands() {
		if command.Name == "worktree" {
			copy := command
			found = &copy
			break
		}
	}
	if found == nil {
		t.Fatal("worktree command not found")
	}
	if strings.Contains(found.Usage, "list") || !strings.Contains(found.Usage, "browse") {
		t.Fatalf("unexpected usage: %q", found.Usage)
	}
	names := map[string]bool{}
	for _, sub := range found.Subcommands {
		names[sub.Name] = true
	}
	if !names["browse"] || names["list"] || names["ls"] {
		t.Fatalf("unexpected worktree subcommands: %#v", names)
	}
}

func TestCmdWorktreeBrowseOpensEmbeddedBrowser(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "browse", CWD: repo}
	m.setTextareaValue("/worktree browse")

	result, cmd := m.ExecuteCommand("/worktree browse")
	m = result.(*Model)
	if !m.worktreeBrowserMode || m.worktreeBrowserModel == nil {
		t.Fatal("worktree browser did not open")
	}
	if cmd == nil {
		t.Fatal("expected asynchronous initial refresh")
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want cleared", got)
	}
}

func TestCmdWorktreeBrowseRefusesActiveOperation(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "browse-busy", CWD: repo}
	m.worktreeOperation = "new"
	m.setTextareaValue("/worktree browse")

	result, _ := m.ExecuteCommand("/worktree browse")
	m = result.(*Model)
	if m.worktreeBrowserMode || m.worktreeBrowserModel != nil {
		t.Fatal("browser opened during an active worktree operation")
	}
	if got := m.textarea.Value(); got != "/worktree browse" {
		t.Fatalf("failed command cleared textarea: %q", got)
	}
}

func TestWorktreeBrowserOpenBindsAndCloses(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "browser-open"})
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "browser-open", CWD: repo}
	result, _ := m.ExecuteCommand("/worktree browse")
	m = result.(*Model)

	result, _ = m.Update(worktreesui.OpenMsg{Worktree: *wt})
	m = result.(*Model)
	if m.worktreeBrowserMode {
		t.Fatal("browser remained open after selection")
	}
	if !sameWorktreePath(m.sess.WorktreeDir, wt.Dir) || !sameWorktreePath(m.sess.CWD, wt.Dir) {
		t.Fatalf("session binding cwd=%q worktree=%q, want %q", m.sess.CWD, m.sess.WorktreeDir, wt.Dir)
	}
}

func TestBrowserRemoveBoundWorktreeRequiresRootFallback(t *testing.T) {
	m := newTestChatModel(false)
	missing := t.TempDir() + "/missing-worktree"
	m.sess = &session.Session{ID: "remove-bound", CWD: missing, WorktreeDir: missing}
	m.worktreeBrowserMode = true
	m.worktreeBrowserModel = worktreesui.New("", m.store, missing, 100, 20, m.styles)

	result, cmd := m.startBrowserWorktreeRemove(worktree.Worktree{Name: "missing", Dir: missing}, true)
	m = result.(*Model)
	if cmd != nil || m.worktreeOperation != "" {
		t.Fatal("bound removal started without a resolvable root fallback")
	}
	if !strings.Contains(m.worktreeBrowserModel.View().Content, "cannot resolve root checkout") {
		t.Fatal("missing root fallback error not shown")
	}
}

func TestWorktreeBrowserCreateFailureStaysOpen(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "browser-create-fail", CWD: repo}
	result, _ := m.ExecuteCommand("/worktree browse")
	m = result.(*Model)
	m.worktreeOperation = "new"
	m.worktreeBrowserOperation = "new"
	m.worktreeBrowserModel.SetBusy(true, "Creating worktree…")

	result, _ = m.Update(worktreeOperationDoneMsg{op: "new", err: errors.New("create failed")})
	m = result.(*Model)
	if m.worktreeOperation != "" || m.worktreeBrowserOperation != "" {
		t.Fatalf("completion was not routed: operation=%q browserOperation=%q", m.worktreeOperation, m.worktreeBrowserOperation)
	}
	result, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m = result.(*Model)
	if !m.worktreeBrowserMode || m.worktreeBrowserModel == nil {
		t.Fatal("browser closed after create failure")
	}
	if out := m.worktreeBrowserModel.View().Content; !strings.Contains(out, "create failed") {
		t.Fatalf("create failure not shown in browser: %q", out)
	}
}

func TestWorktreeBrowserClosePreservesCurrentDraftAndWindowSize(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "browse-close", CWD: repo}
	result, _ := m.ExecuteCommand("/worktree browse")
	m = result.(*Model)
	m.setTextareaValue("draft after browser opened")

	result, _ = m.Update(tea.WindowSizeMsg{Width: 57, Height: 19})
	m = result.(*Model)
	if m.worktreeBrowserModel.Width() != 57 || m.worktreeBrowserModel.Height() != 19 {
		t.Fatalf("browser size = %dx%d", m.worktreeBrowserModel.Width(), m.worktreeBrowserModel.Height())
	}

	result, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = result.(*Model)
	if cmd == nil {
		t.Fatal("escape did not emit close command")
	}
	result, _ = m.Update(cmd())
	m = result.(*Model)
	if m.worktreeBrowserMode || m.worktreeBrowserModel != nil {
		t.Fatal("browser remained open")
	}
	if got := m.textarea.Value(); got != "draft after browser opened" {
		t.Fatalf("draft = %q", got)
	}
}

func TestCmdWorktreeListAndLSAreUnknown(t *testing.T) {
	for _, command := range []string{"/worktree list", "/worktree ls"} {
		m := newTestChatModel(false)
		result, _ := m.ExecuteCommand(command)
		got := result.(*Model)
		if got.worktreeBrowserMode {
			t.Fatalf("%s unexpectedly opened browser", command)
		}
		if !strings.Contains(strings.ToLower(got.footerMessage), "unknown") {
			t.Fatalf("%s footer = %q, want unknown subcommand", command, got.footerMessage)
		}
	}
}
