package chat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/worktree"
)

// worktreeBaseDir returns a directory inside the session's repository: the bound
// worktree when set, otherwise the process working directory. Worktree git
// operations resolve the shared repo from any of its worktrees, so this is a
// valid entry point for list/create regardless of the current binding.
func (m *Model) worktreeBaseDir() string {
	if m.sess != nil && m.sess.WorktreeDir != "" {
		return m.sess.WorktreeDir
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if m.sess != nil && m.sess.CWD != "" {
		return m.sess.CWD
	}
	return "."
}

// boundWorktreeDir returns the session's bound worktree dir, or "".
func (m *Model) boundWorktreeDir() string {
	if m.sess == nil {
		return ""
	}
	return m.sess.WorktreeDir
}

// cmdWorktree dispatches the /worktree (alias /wt) command.
func (m *Model) cmdWorktree(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.cmdWorktreeList()
	}

	sub := strings.ToLower(args[0])
	rest := args[1:]

	switch sub {
	case "list", "ls":
		return m.cmdWorktreeList()
	case "new", "add":
		return m.createWorktree(strings.Join(rest, " "))
	case "pwd":
		return m.cmdWorktreePwd()
	case "diff":
		return m.cmdWorktreeDiff()
	case "promote":
		return m.cmdWorktreePromote(rest)
	case "rm", "remove", "delete":
		return m.cmdWorktreeRemove(rest)
	case "shell", "sh":
		return m.cmdWorktreeShell(rest)
	case "root":
		return m.cmdWorktreeSwitch("root")
	default:
		// Treat a bare argument as a worktree name to switch to.
		return m.cmdWorktreeSwitch(strings.Join(args, " "))
	}
}

// cmdWorktreeList prints the repo's worktrees, marking the current binding.
func (m *Model) cmdWorktreeList() (tea.Model, tea.Cmd) {
	base := m.worktreeBaseDir()
	if !worktree.IsGitRepo(base) {
		return m.showFooterWarning("Not in a git repository; /worktree is unavailable here.")
	}
	wts, err := worktree.List(base)
	if err != nil {
		return m.showFooterWarning(fmt.Sprintf("worktree list failed: %v", err))
	}

	bound := m.boundWorktreeDir()
	root := base
	if r, err := worktree.MainRepoRoot(base); err == nil {
		root = r
	}

	var b strings.Builder
	b.WriteString("## Worktrees\n\n")
	rootMark := "○"
	if bound == "" {
		rootMark = "●"
	}
	b.WriteString(fmt.Sprintf("%s **root** — checkout — `%s`%s\n", rootMark, root, currentTag(bound == "")))
	for _, wt := range wts {
		mark := "○"
		if pathsEqual(bound, wt.Dir) {
			mark = "●"
		}
		meta := "worktree"
		if wt.Branch != "" {
			meta += " ⎇ " + wt.Branch
		} else if wt.HeadSHA != "" {
			meta += " ⎇ detached@" + wt.HeadSHA
		}
		if wt.DirtyFiles > 0 {
			meta += fmt.Sprintf(" ±%d", wt.DirtyFiles)
		}
		b.WriteString(fmt.Sprintf("%s **%s** — %s — `%s`%s\n", mark, wt.Name, meta, wt.Dir, currentTag(pathsEqual(bound, wt.Dir))))
	}
	if len(wts) == 0 {
		b.WriteString("\n_No worktrees yet. Create one with_ `/worktree new [name]`.\n")
	}
	b.WriteString("\n**Commands:** `/worktree new [name]` · `/worktree <name>` (switch) · `/worktree root` · `/worktree pwd` · `/worktree diff` · `/worktree promote <branch>` · `/worktree rm [force]` · `/worktree shell [--tmux]`")
	m.setTextareaValue("")
	return m.showSystemMessage(b.String())
}

func currentTag(isCurrent bool) string {
	if isCurrent {
		return "  ← current"
	}
	return ""
}

// createWorktree creates a worktree off HEAD asynchronously, streaming progress
// to the footer spinner. On completion (worktreeCreateDoneMsg) the session is
// bound to it via bindWorktree. Guarded against concurrent ops and streaming.
func (m *Model) createWorktree(name string) (tea.Model, tea.Cmd) {
	base := m.worktreeBaseDir()
	if !worktree.IsGitRepo(base) {
		return m.showFooterWarning("Not in a git repository; cannot create a worktree.")
	}
	if m.streaming {
		return m.showFooterWarning("Cannot create a worktree while streaming. Cancel first (Esc).")
	}
	if m.worktreeBusy {
		return m.showFooterWarning("A worktree operation is already running.")
	}

	// Setup-script source for v1: the TERM_LLM_WORKTREE_SETUP env var (run in the
	// new worktree after creation — e.g. "npm install", copying gitignored .env).
	// Config-file precedence (repo-local → user) is a follow-up.
	setupScript := os.Getenv("TERM_LLM_WORKTREE_SETUP")

	m.worktreeBusy = true
	m.worktreeOpSeq++
	op := m.worktreeOpSeq
	m.worktreeProgress = "Creating worktree…"
	progress := make(chan worktree.Progress, 8)
	create := func() tea.Msg {
		opts := worktree.CreateOptions{Name: name, Base: "HEAD", SetupScript: setupScript, Progress: progress}
		wt, err := worktree.Create(context.Background(), base, opts)
		close(progress)
		return worktreeCreateDoneMsg{op: op, wt: wt, err: err}
	}
	m.setTextareaValue("")
	return m, tea.Batch(create, waitForWorktreeProgress(op, progress), m.spinner.Tick)
}

// waitForWorktreeProgress blocks on the next progress event from Create and
// turns it into a worktreeProgressMsg, rescheduling itself until the channel is
// closed (signalled as done).
func waitForWorktreeProgress(op uint64, progress <-chan worktree.Progress) tea.Cmd {
	if progress == nil {
		return nil
	}
	return func() tea.Msg {
		p, ok := <-progress
		if !ok {
			return worktreeProgressMsg{op: op, progress: progress, done: true}
		}
		return worktreeProgressMsg{op: op, progress: progress, message: p.Message}
	}
}

// cmdWorktreePwd prints and copies the bound worktree path.
func (m *Model) cmdWorktreePwd() (tea.Model, tea.Cmd) {
	dir := m.boundWorktreeDir()
	if dir == "" {
		m.setTextareaValue("")
		return m.showFooterMuted("On the root checkout (no worktree bound).")
	}
	_ = clipboard.CopyText(dir)
	_ = clipboard.CopyTextOSC52(dir)
	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf("Worktree path (copied to clipboard):\n\n`%s`", dir))
}

// cmdWorktreeDiff renders the worktree diff (vs base) in the scrollable pager.
func (m *Model) cmdWorktreeDiff() (tea.Model, tea.Cmd) {
	dir := m.boundWorktreeDir()
	if dir == "" {
		return m.showFooterWarning("Not bound to a worktree; nothing to diff.")
	}
	diff, err := worktree.Diff(dir)
	if err != nil {
		return m.showFooterWarning(fmt.Sprintf("worktree diff failed: %v", err))
	}
	m.setTextareaValue("")
	if strings.TrimSpace(diff) == "" {
		return m.showFooterMuted("No changes in this worktree.")
	}
	m.dialog.ShowContent("Worktree diff", diff)
	return m, nil
}

// cmdWorktreePromote converts the bound detached worktree to a named branch.
func (m *Model) cmdWorktreePromote(args []string) (tea.Model, tea.Cmd) {
	dir := m.boundWorktreeDir()
	if dir == "" {
		return m.showFooterWarning("Not bound to a worktree; nothing to promote.")
	}
	branch := strings.TrimSpace(strings.Join(args, " "))
	if branch == "" {
		return m.showFooterWarning("Usage: /worktree promote <branch>")
	}
	if err := worktree.Promote(dir, branch); err != nil {
		return m.showFooterWarning(fmt.Sprintf("promote failed: %v", err))
	}
	m.setTextareaValue("")
	return m.showFooterSuccess(fmt.Sprintf("Promoted worktree to branch %s.", branch))
}

// cmdWorktreeRemove removes the bound worktree and rebinds to root. It refuses on
// a dirty worktree unless "force" is given.
func (m *Model) cmdWorktreeRemove(args []string) (tea.Model, tea.Cmd) {
	dir := m.boundWorktreeDir()
	if dir == "" {
		return m.showFooterWarning("Not bound to a worktree; nothing to remove.")
	}
	force := false
	for _, a := range args {
		switch strings.ToLower(a) {
		case "force", "--force", "-f", "yes":
			force = true
		}
	}
	// Resolve the main checkout before removal so we can rebind afterwards.
	mainRoot, rootErr := worktree.MainRepoRoot(dir)
	if err := worktree.Remove(dir, force); err != nil {
		if err == worktree.ErrDirty {
			return m.showFooterWarning("Worktree has uncommitted changes. Run `/worktree rm force` to delete anyway.")
		}
		return m.showFooterWarning(fmt.Sprintf("remove failed: %v", err))
	}
	// Rebind the session to the root checkout.
	if rootErr == nil {
		_ = m.bindRoot(mainRoot)
	} else if m.sess != nil {
		m.sess.WorktreeDir = ""
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
	m.setTextareaValue("")
	return m.showFooterSuccess("Removed worktree; back on the root checkout.")
}

// cmdWorktreeShell drops into a shell in the bound worktree. Without --tmux it
// suspends the TUI and runs $SHELL (cwd = worktree); with --tmux it opens a tmux
// split/new-window. Both paths use tea.ExecProcess so the terminal is released
// and restored cleanly.
func (m *Model) cmdWorktreeShell(args []string) (tea.Model, tea.Cmd) {
	dir := m.boundWorktreeDir()
	if dir == "" {
		return m.showFooterWarning("Not bound to a worktree. Use /worktree new or switch first.")
	}
	useTmux := false
	for _, a := range args {
		if a == "--tmux" || a == "tmux" {
			useTmux = true
		}
	}
	return m.openWorktreeShell(dir, useTmux)
}

// openWorktreeShell suspends the TUI and runs an interactive shell with cwd set
// to dir, or — when useTmux is set — opens a tmux split for the worktree. It is
// the shared entry point for /worktree shell and the switcher's "s" key.
func (m *Model) openWorktreeShell(dir string, useTmux bool) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(dir) == "" {
		return m.showFooterWarning("No worktree directory selected.")
	}
	if useTmux && strings.TrimSpace(os.Getenv("TMUX")) == "" {
		return m.showFooterWarning("/worktree shell --tmux requires an existing tmux session.")
	}
	m.setTextareaValue("")
	cmd := worktreeShellCommand(dir, useTmux)
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return worktreeShellDoneMsg{err: err} })
}

// worktreeShellCommand builds the command that opens a command line in dir: a
// tmux split (falling back to a new window) when useTmux is set, otherwise an
// interactive $SHELL with its working directory set to the worktree.
func worktreeShellCommand(dir string, useTmux bool) *exec.Cmd {
	if useTmux {
		quoted := strconv.Quote(dir)
		return exec.Command("sh", "-c", "tmux split-window -c "+quoted+" || tmux new-window -c "+quoted)
	}
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	return cmd
}

// cmdWorktreeSwitch binds the session to an existing worktree by name, or to the
// root checkout when name is "root".
func (m *Model) cmdWorktreeSwitch(name string) (tea.Model, tea.Cmd) {
	base := m.worktreeBaseDir()
	if !worktree.IsGitRepo(base) {
		return m.showFooterWarning("Not in a git repository; /worktree is unavailable here.")
	}
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, "root") {
		mainRoot, err := worktree.MainRepoRoot(base)
		if err != nil {
			return m.showFooterWarning(fmt.Sprintf("could not resolve repo root: %v", err))
		}
		if err := m.bindRoot(mainRoot); err != nil {
			return m.showFooterWarning(fmt.Sprintf("switch failed: %v", err))
		}
		m.setTextareaValue("")
		return m.showFooterSuccess("Switched to the root checkout.")
	}

	wts, err := worktree.List(base)
	if err != nil {
		return m.showFooterWarning(fmt.Sprintf("worktree list failed: %v", err))
	}
	for _, wt := range wts {
		if strings.EqualFold(wt.Name, name) || pathsEqual(wt.Dir, name) {
			if err := m.bindWorktree(wt.Dir); err != nil {
				return m.showFooterWarning(fmt.Sprintf("switch failed: %v", err))
			}
			m.setTextareaValue("")
			return m.showFooterSuccess(fmt.Sprintf("Switched to worktree %s.", wt.Name))
		}
	}
	return m.showFooterWarning(fmt.Sprintf("No worktree named %q. Use /worktree list.", name))
}

// bindWorktree binds the session to dir: chdir (single-session TUI), persist the
// binding, and approve the directory for tools.
//
// Design note: §4.2 of the worktree design suggests per-session binding without
// a process chdir, because the web/server path shares one process across many
// sessions. The TUI runs exactly one active session per process, so chdir is the
// simplest correct mechanism here and makes every tool (which falls back to the
// process cwd) operate in the worktree with no per-tool threading. WorktreeDir
// remains the persisted source of truth for a future multi-session surface.
func (m *Model) bindWorktree(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.Chdir(abs); err != nil {
		return err
	}
	if m.approvedDirs != nil {
		_ = m.approvedDirs.AddDirectory(abs)
	}
	if m.sess != nil {
		m.sess.WorktreeDir = abs
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
	m.invalidateWorktreeSegment()
	return nil
}

// bindRoot rebinds the session to the root checkout (clears the binding).
func (m *Model) bindRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.Chdir(abs); err != nil {
		return err
	}
	if m.sess != nil {
		m.sess.WorktreeDir = ""
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
	m.invalidateWorktreeSegment()
	return nil
}

// RestoreWorktreeBinding re-applies a resumed session's worktree binding by
// chdir-ing into it. If the bound directory no longer exists, the session is
// rebound to the root checkout. Safe to call when no session/binding is set.
func (m *Model) RestoreWorktreeBinding() {
	if m.sess == nil || m.sess.WorktreeDir == "" {
		return
	}
	dir := m.sess.WorktreeDir
	if info, err := os.Stat(dir); err == nil && info.IsDir() && worktree.IsGitRepo(dir) {
		_ = os.Chdir(dir)
		return
	}
	// Stale binding: fall back to root.
	if root, err := worktree.MainRepoRoot(m.worktreeBaseDir()); err == nil {
		_ = m.bindRoot(root)
	} else {
		m.sess.WorktreeDir = ""
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
}

// worktreeSegmentTTL bounds how often the footer segment's git status is
// recomputed. The status line renders every frame; this keeps git calls rare.
const worktreeSegmentTTL = 2 * time.Second

// cachedWorktreeSegment returns the footer segment for the bound worktree,
// recomputing (via git) at most once per worktreeSegmentTTL. Returns "" with no
// git call when the session is on the root checkout.
func (m *Model) cachedWorktreeSegment() string {
	if m.boundWorktreeDir() == "" {
		m.worktreeSegCache = ""
		return ""
	}
	if m.worktreeSegCache != "" && time.Since(m.worktreeSegFetched) < worktreeSegmentTTL {
		return m.worktreeSegCache
	}
	m.worktreeSegCache = m.worktreeFooterSegment()
	m.worktreeSegFetched = time.Now()
	return m.worktreeSegCache
}

// invalidateWorktreeSegment forces the next status-line render to recompute the
// worktree footer segment. Called after a bind change.
func (m *Model) invalidateWorktreeSegment() {
	m.worktreeSegCache = ""
	m.worktreeSegFetched = time.Time{}
}

// worktreeFooterSegment returns a compact status segment for the bound worktree,
// or "" when on the root checkout. Example: "⌥ neon-canyon ⎇ detached@a1b2c3 ±3".
func (m *Model) worktreeFooterSegment() string {
	dir := m.boundWorktreeDir()
	if dir == "" {
		return ""
	}
	wt, err := worktree.Get(dir)
	if err != nil {
		return "⌥ " + filepath.Base(dir)
	}
	seg := "⌥ " + wt.Name
	if wt.Branch != "" {
		seg += "  ⎇ " + wt.Branch
	} else if wt.HeadSHA != "" {
		seg += "  ⎇ detached@" + wt.HeadSHA
	}
	if wt.DirtyFiles > 0 {
		seg += fmt.Sprintf("  ±%d", wt.DirtyFiles)
	}
	return seg
}

// pathsEqual reports whether two paths resolve to the same location.
func pathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	ca, err := filepath.Abs(a)
	if err != nil {
		ca = a
	}
	if r, err := filepath.EvalSymlinks(ca); err == nil {
		ca = r
	}
	cb, err := filepath.Abs(b)
	if err != nil {
		cb = b
	}
	if r, err := filepath.EvalSymlinks(cb); err == nil {
		cb = r
	}
	return filepath.Clean(ca) == filepath.Clean(cb)
}
