package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	worktreesui "github.com/samsaffron/term-llm/internal/tui/worktrees"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func (m *Model) openWorktreeBrowser() (tea.Model, tea.Cmd) {
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	root, err := m.repoRootForWorktree()
	if err != nil {
		return m.showFooterError(err.Error())
	}
	browser := worktreesui.New(root, m.store, m.boundWorktreeDir(), m.width, m.height, m.styles)
	m.worktreeBrowserMode = true
	m.worktreeBrowserModel = browser
	m.worktreeBrowserRoot = root
	m.clearWorktreeCommandComposer()
	return m, browser.Init()
}

func (m *Model) closeWorktreeBrowser() (tea.Model, tea.Cmd) {
	m.worktreeBrowserMode = false
	m.worktreeBrowserModel = nil
	m.worktreeBrowserRoot = ""
	m.worktreeBrowserOperation = ""
	m.textarea.Focus()
	return m, nil
}

func (m *Model) updateWorktreeBrowserMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.applyWindowSize(msg)
		return m.delegateWorktreeBrowser(msg)
	case worktreesui.CloseMsg:
		return m.closeWorktreeBrowser()
	case worktreesui.OpenMsg:
		if m.streaming {
			m.worktreeBrowserModel.Error(fmt.Errorf("cannot switch worktrees while a response is streaming"))
			return m, nil
		}
		if m.worktreeOperationBusy() {
			m.worktreeBrowserModel.Error(fmt.Errorf("a worktree operation is already running"))
			return m, nil
		}
		if err := m.bindWorktreeDir(msg.Worktree.Dir); err != nil {
			m.worktreeBrowserModel.Error(err)
			return m, nil
		}
		m.closeWorktreeBrowser()
		return m.showFooterSuccess("Switched worktree to " + msg.Worktree.Dir)
	case worktreesui.CreateMsg:
		return m.startBrowserWorktreeCreate(msg.Options)
	case worktreesui.RemoveMsg:
		return m.startBrowserWorktreeRemove(msg.Worktree, msg.Force)
	case worktreeOperationDoneMsg:
		return m.handleBrowserWorktreeOperationDone(msg)
	default:
		return m.delegateWorktreeBrowser(msg)
	}
}

func (m *Model) delegateWorktreeBrowser(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.worktreeBrowserModel == nil {
		return m, nil
	}
	updated, cmd := m.worktreeBrowserModel.Update(msg)
	if browser, ok := updated.(*worktreesui.Model); ok {
		m.worktreeBrowserModel = browser
	}
	return m, cmd
}

func (m *Model) startBrowserWorktreeCreate(opts worktree.CreateOptions) (tea.Model, tea.Cmd) {
	if m.worktreeBrowserModel == nil {
		return m, nil
	}
	if m.streaming {
		m.worktreeBrowserModel.ReportCreateResult(fmt.Errorf("cannot create worktrees while a response is streaming"))
		return m, nil
	}
	if m.worktreeOperationBusy() {
		m.worktreeBrowserModel.ReportCreateResult(fmt.Errorf("a worktree operation is already running"))
		return m, nil
	}
	if strings.TrimSpace(opts.Name) == "" {
		m.worktreeBrowserModel.ReportCreateResult(fmt.Errorf("name is required"))
		return m, nil
	}
	if strings.TrimSpace(opts.Base) == "" {
		opts.Base = "HEAD"
	}
	opts.MoveChanges = true
	if script := strings.TrimSpace(os.Getenv("TERM_LLM_WORKTREE_SETUP")); script != "" {
		opts.SetupScript = script
		opts.SetupTimeout = 10 * time.Minute
	}
	root := m.worktreeBrowserRoot
	parentCtx := m.rootContext()
	m.worktreeOperation = "new"
	m.worktreeBrowserOperation = "new"
	m.worktreeBrowserModel.SetBusy(true, "Creating worktree…")
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parentCtx, 15*time.Minute)
		defer cancel()
		wt, err := worktree.Create(ctx, root, opts)
		return worktreeOperationDoneMsg{op: "new", wt: wt, err: err}
	}
}

func (m *Model) startBrowserWorktreeRemove(wt worktree.Worktree, force bool) (tea.Model, tea.Cmd) {
	if m.worktreeBrowserModel == nil {
		return m, nil
	}
	if m.streaming {
		m.worktreeBrowserModel.Error(fmt.Errorf("cannot remove worktrees while a response is streaming"))
		return m, nil
	}
	if m.worktreeOperationBusy() {
		m.worktreeBrowserModel.Error(fmt.Errorf("a worktree operation is already running"))
		return m, nil
	}
	if !force {
		inUse, err := m.otherSessionsUsingWorktree(wt.Dir)
		if err != nil {
			m.worktreeBrowserModel.Error(err)
			return m, nil
		}
		if len(inUse) > 0 {
			risks := []string{fmt.Sprintf("used by %d other session(s)", len(inUse))}
			if wt.DirtyFiles > 0 {
				risks = append(risks, fmt.Sprintf("contains %d dirty file(s)", wt.DirtyFiles))
			}
			for _, s := range inUse {
				name := s.Name
				if name == "" {
					name = s.ID
				}
				risks = append(risks, fmt.Sprintf("#%d %s", s.Number, name))
			}
			m.worktreeBrowserModel.EscalateRemove(risks)
			return m, nil
		}
	}
	bound := m.sess != nil && sameWorktreePath(m.sess.WorktreeDir, wt.Dir)
	root := ""
	if bound {
		var err error
		root, err = worktree.MainRepoRoot(wt.Dir)
		if err != nil {
			m.worktreeBrowserModel.Error(fmt.Errorf("cannot resolve root checkout before removing bound worktree: %w", err))
			return m, nil
		}
	}
	parentCtx := m.rootContext()
	m.worktreeOperation = "remove"
	m.worktreeBrowserOperation = "remove"
	m.worktreeBrowserModel.SetBusy(true, "Removing worktree…")
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parentCtx, 5*time.Minute)
		defer cancel()
		err := worktree.Remove(ctx, wt.Dir, worktree.RemoveOptions{Force: force})
		return worktreeOperationDoneMsg{op: "remove", dir: wt.Dir, root: root, bound: bound, err: err}
	}
}

func (m *Model) handleBrowserWorktreeOperationDone(msg worktreeOperationDoneMsg) (tea.Model, tea.Cmd) {
	if msg.op == "" || msg.op != m.worktreeOperation || msg.op != m.worktreeBrowserOperation {
		return m, nil
	}
	m.worktreeOperation = ""
	m.worktreeBrowserOperation = ""
	browser := m.worktreeBrowserModel
	if browser == nil {
		return m, nil
	}
	if msg.err != nil {
		if msg.op == "remove" && errors.Is(msg.err, worktree.ErrDirty) {
			browser.EscalateRemove([]string{"worktree has uncommitted changes", "force permanently discards them"})
			return m, nil
		}
		if msg.op == "new" {
			browser.ReportCreateResult(msg.err)
		} else {
			browser.ReportRemoveResult(msg.err)
		}
		return m, nil
	}
	switch msg.op {
	case "new":
		if msg.wt == nil {
			browser.ReportCreateResult(fmt.Errorf("worktree create failed: no worktree returned"))
			return m, nil
		}
		if err := m.bindWorktreeDir(msg.wt.Dir); err != nil {
			browser.ReportCreateResult(err)
			return m, nil
		}
		name := msg.wt.Name
		m.closeWorktreeBrowser()
		return m.showFooterSuccess("Created and switched to worktree " + name)
	case "remove":
		if msg.bound {
			if msg.root == "" {
				browser.ReportRemoveResult(fmt.Errorf("worktree was removed, but no root checkout was available to rebind the session"))
				return m, nil
			}
			if err := m.bindRootDir(msg.root); err != nil {
				browser.ReportRemoveResult(fmt.Errorf("worktree was removed, but rebinding the session to %s failed: %w", msg.root, err))
				return m, nil
			}
			browser.SetBusy(false, "")
			// The current marker now refers to the root checkout, not another row.
		}
		return m, browser.ReportRemoveResult(nil)
	}
	return m, nil
}

func sameWorktreePath(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && filepath.Clean(resolvedA) == filepath.Clean(resolvedB)
}
