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
	"github.com/samsaffron/term-llm/internal/worktree"
)

type worktreeOperationDoneMsg struct {
	op     string
	wt     *worktree.Worktree
	dir    string
	root   string
	branch string
	bound  bool
	merge  worktree.MergeResult
	diff   string
	err    error
}

func (m *Model) cmdWorktree(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.showSystemMessage("Usage: /worktree [new|list|switch|root|pwd|diff|merge|promote|rm]")
	}
	sub := strings.ToLower(args[0])
	if sub == "ls" {
		sub = "list"
	}
	if sub == "remove" {
		sub = "rm"
	}
	subArgs := args[1:]
	switch sub {
	case "pwd":
		return m.showSystemMessage(m.boundWorktreeDir())
	case "list":
		return m.cmdWorktreeList()
	case "new":
		return m.cmdWorktreeNew(subArgs)
	case "switch":
		return m.cmdWorktreeSwitch(subArgs)
	case "root":
		return m.cmdWorktreeRoot()
	case "diff":
		return m.cmdWorktreeDiff(subArgs)
	case "merge":
		return m.cmdWorktreeMerge(subArgs)
	case "promote":
		return m.cmdWorktreePromote(subArgs)
	case "rm":
		return m.cmdWorktreeRemove(subArgs)
	default:
		return m.showFooterError("Unknown /worktree subcommand: " + sub)
	}
}

func (m *Model) boundWorktreeDir() string {
	if m != nil && m.sess != nil && strings.TrimSpace(m.sess.WorktreeDir) != "" {
		return m.sess.WorktreeDir
	}
	if m != nil && m.sess != nil && strings.TrimSpace(m.sess.CWD) != "" {
		return m.sess.CWD
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

func (m *Model) worktreeOperationBusy() bool {
	return m != nil && strings.TrimSpace(m.worktreeOperation) != ""
}

func (m *Model) worktreeBusyMessage() (tea.Model, tea.Cmd) {
	return m.showFooterWarning("A worktree operation is already running.")
}

func (m *Model) repoRootForWorktree() (string, error) {
	start := m.boundWorktreeDir()
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	return worktree.MainRepoRoot(start)
}

func (m *Model) bindWorktreeDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err != nil {
		return fmt.Errorf("worktree directory is not accessible: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("worktree path is not a directory: %s", abs)
	}
	wt, err := worktree.Get(abs)
	if err != nil {
		return err
	}
	abs = wt.Dir
	if m.toolMgr != nil {
		if err := m.toolMgr.SetBaseDir(abs); err != nil {
			return err
		}
	}
	if m.approvedDirs != nil {
		_ = m.approvedDirs.AddDirectory(abs)
	}
	if m.sess != nil {
		changed := filepath.Clean(m.sess.WorktreeDir) != filepath.Clean(abs) || filepath.Clean(m.sess.CWD) != filepath.Clean(abs)
		m.sess.WorktreeDir = abs
		m.sess.CWD = abs
		if m.store != nil && changed {
			if err := m.store.Update(context.Background(), m.sess); err != nil {
				return err
			}
		}
	}
	_ = worktree.TouchLastBound(abs)
	return nil
}

func (m *Model) resolveWorktreeTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("worktree target is required")
	}
	if filepath.IsAbs(target) {
		return target, nil
	}
	root, err := m.repoRootForWorktree()
	if err != nil {
		return "", err
	}
	items, err := worktree.List(root)
	if err != nil {
		return "", err
	}
	for _, wt := range items {
		if wt.Name == target {
			return wt.Dir, nil
		}
	}
	if strings.ContainsRune(target, filepath.Separator) || strings.HasPrefix(target, ".") {
		return target, nil
	}
	return "", fmt.Errorf("unknown managed worktree %q", target)
}

func (m *Model) cmdWorktreeList() (tea.Model, tea.Cmd) {
	root, err := m.repoRootForWorktree()
	if err != nil {
		return m.showFooterError(err.Error())
	}
	items, err := worktree.List(root)
	if err != nil {
		return m.showFooterError(err.Error())
	}
	if len(items) == 0 {
		return m.showFooterMuted("No managed worktrees.")
	}
	var b strings.Builder
	b.WriteString("Managed worktrees:\n")
	for _, wt := range items {
		mark := " "
		if m.sess != nil && filepath.Clean(m.sess.WorktreeDir) == filepath.Clean(wt.Dir) {
			mark = "*"
		}
		ref := "detached@" + shortSHA(wt.HeadSHA)
		if wt.Branch != "" {
			ref = wt.Branch
		}
		fmt.Fprintf(&b, "%s %s  %s  dirty:%d  %s\n", mark, wt.Name, ref, wt.DirtyFiles, wt.Dir)
	}
	return m.showSystemMessage(b.String())
}

func (m *Model) cmdWorktreeNew(args []string) (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Cannot create/switch worktrees while a response is streaming.")
	}
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	root, err := m.repoRootForWorktree()
	if err != nil {
		return m.showFooterError(err.Error())
	}
	opts := worktree.CreateOptions{Base: "HEAD"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--base":
			if i+1 < len(args) {
				opts.Base = args[i+1]
				i++
			}
		case "-b", "--branch":
			if i+1 < len(args) {
				opts.Branch = args[i+1]
				i++
			}
		default:
			if opts.Name == "" {
				opts.Name = args[i]
			}
		}
	}
	if script := strings.TrimSpace(os.Getenv("TERM_LLM_WORKTREE_SETUP")); script != "" {
		opts.SetupScript = script
		opts.SetupTimeout = 10 * time.Minute
	}
	parentCtx := m.rootContext()
	m.worktreeOperation = "new"
	return m.showFooterMutedWithCmd("Creating worktree…", func() tea.Msg {
		ctx, cancel := context.WithTimeout(parentCtx, 15*time.Minute)
		defer cancel()
		wt, err := worktree.Create(ctx, root, opts)
		return worktreeOperationDoneMsg{op: "new", wt: wt, err: err}
	})
}

func (m *Model) cmdWorktreeSwitch(args []string) (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Cannot switch worktrees while a response is streaming.")
	}
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	if len(args) == 0 {
		return m.showFooterError("Usage: /worktree switch <name-or-dir>")
	}
	target := args[0]
	dir, err := m.resolveWorktreeTarget(target)
	if err != nil {
		return m.showFooterError(err.Error())
	}
	if err := m.bindWorktreeDir(dir); err != nil {
		return m.showFooterError(err.Error())
	}
	return m.showFooterSuccess("Switched worktree to " + dir)
}

func (m *Model) cmdWorktreeRoot() (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Cannot switch worktrees while a response is streaming.")
	}
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	root, err := m.repoRootForWorktree()
	if err != nil {
		return m.showFooterError(err.Error())
	}
	if m.toolMgr != nil {
		if err := m.toolMgr.SetBaseDir(root); err != nil {
			return m.showFooterError(err.Error())
		}
	}
	if m.sess != nil {
		changed := strings.TrimSpace(m.sess.WorktreeDir) != "" || filepath.Clean(m.sess.CWD) != filepath.Clean(root)
		m.sess.WorktreeDir = ""
		m.sess.CWD = root
		if m.store != nil && changed {
			if err := m.store.Update(context.Background(), m.sess); err != nil {
				return m.showFooterError(err.Error())
			}
		}
	}
	return m.showFooterSuccess("Back on root checkout: " + root)
}

func (m *Model) cmdWorktreeDiff(args []string) (tea.Model, tea.Cmd) {
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	dir := ""
	if len(args) > 0 {
		resolved, err := m.resolveWorktreeTarget(args[0])
		if err != nil {
			return m.showFooterError(err.Error())
		}
		dir = resolved
	} else if m.sess != nil {
		dir = strings.TrimSpace(m.sess.WorktreeDir)
	}
	if dir == "" {
		return m.showFooterMuted("No worktree is bound.")
	}
	m.worktreeOperation = "diff"
	return m.showFooterMutedWithCmd("Generating worktree diff…", func() tea.Msg {
		diff, err := worktree.Diff(dir)
		return worktreeOperationDoneMsg{op: "diff", dir: dir, diff: diff, err: err}
	})
}

func (m *Model) cmdWorktreeMerge(args []string) (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Cannot merge worktrees while a response is streaming.")
	}
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	dir := ""
	if m.sess != nil {
		dir = strings.TrimSpace(m.sess.WorktreeDir)
	}
	opts := worktree.MergeOptions{}
	target := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--commit":
			opts.Commit = true
		case "-m", "--message":
			if i+1 < len(args) {
				opts.Message = args[i+1]
				i++
			}
		case "--allow-dirty", "--force":
			opts.AllowDirty = true
		default:
			if target == "" && !strings.HasPrefix(args[i], "-") {
				target = args[i]
			}
		}
	}
	if target != "" {
		resolved, err := m.resolveWorktreeTarget(target)
		if err != nil {
			return m.showFooterError(err.Error())
		}
		dir = resolved
	}
	if dir == "" {
		return m.showFooterMuted("No worktree is bound.")
	}
	parentCtx := m.rootContext()
	m.worktreeOperation = "merge"
	return m.showFooterMutedWithCmd("Merging worktree changes…", func() tea.Msg {
		res, err := worktree.MergeBack(parentCtx, dir, opts)
		return worktreeOperationDoneMsg{op: "merge", dir: dir, merge: res, err: err}
	})
}

func (m *Model) cmdWorktreePromote(args []string) (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Cannot promote worktrees while a response is streaming.")
	}
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	dir := ""
	if m.sess != nil {
		dir = strings.TrimSpace(m.sess.WorktreeDir)
	}
	if dir == "" {
		return m.showFooterMuted("No worktree is bound.")
	}
	if len(args) == 0 {
		return m.showFooterError("Usage: /worktree promote <branch>")
	}
	branch := args[0]
	parentCtx := m.rootContext()
	m.worktreeOperation = "promote"
	return m.showFooterMutedWithCmd("Promoting worktree…", func() tea.Msg {
		err := worktree.Promote(parentCtx, dir, branch)
		return worktreeOperationDoneMsg{op: "promote", dir: dir, branch: branch, err: err}
	})
}

func (m *Model) cmdWorktreeRemove(args []string) (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Cannot remove worktrees while a response is streaming.")
	}
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	dir := ""
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		} else if dir == "" {
			dir = arg
		}
	}
	if dir == "" && m.sess != nil {
		dir = strings.TrimSpace(m.sess.WorktreeDir)
	}
	if dir == "" {
		return m.showFooterError("Usage: /worktree rm [name-or-dir] [--force]")
	}
	resolvedDir, err := m.resolveWorktreeTarget(dir)
	if err != nil {
		return m.showFooterError(err.Error())
	}
	dir = resolvedDir
	bound := m.sess != nil && filepath.Clean(m.sess.WorktreeDir) == filepath.Clean(dir)
	root := ""
	if bound {
		if r, err := worktree.MainRepoRoot(dir); err == nil {
			root = r
		}
	}
	if !force {
		inUse, err := m.otherSessionsUsingWorktree(dir)
		if err != nil {
			return m.showFooterError(err.Error())
		}
		if len(inUse) > 0 {
			return m.showFooterWarning(fmt.Sprintf("Worktree is used by %d other session(s); use --force to remove it.", len(inUse)))
		}
	}
	parentCtx := m.rootContext()
	m.worktreeOperation = "remove"
	return m.showFooterMutedWithCmd("Removing worktree…", func() tea.Msg {
		err := worktree.Remove(parentCtx, dir, worktree.RemoveOptions{Force: force})
		return worktreeOperationDoneMsg{op: "remove", dir: dir, root: root, bound: bound, err: err}
	})
}

func (m *Model) handleWorktreeOperationDone(msg worktreeOperationDoneMsg) (tea.Model, tea.Cmd) {
	if msg.op != "" && m.worktreeOperation == msg.op {
		m.worktreeOperation = ""
	} else if msg.op == "" || m.worktreeOperation != "" {
		m.worktreeOperation = ""
	}
	if msg.err != nil {
		switch {
		case msg.op == "merge" && errors.Is(msg.err, worktree.ErrConflict):
			return m.showSystemMessage("Merge conflicts; root checkout was reset cleanly. Conflicts:\n- " + strings.Join(msg.merge.Conflicts, "\n- "))
		case msg.op == "remove" && errors.Is(msg.err, worktree.ErrDirty):
			return m.showFooterWarning("Worktree has changes; use /worktree rm --force to remove it.")
		default:
			return m.showFooterError(msg.err.Error())
		}
	}
	switch msg.op {
	case "new":
		if msg.wt == nil {
			return m.showFooterError("worktree create failed: no worktree returned")
		}
		if err := m.bindWorktreeDir(msg.wt.Dir); err != nil {
			return m.showFooterError(err.Error())
		}
		return m.showFooterSuccess("Created and switched to worktree " + msg.wt.Name)
	case "diff":
		if strings.TrimSpace(msg.diff) == "" {
			return m.showFooterMuted("Worktree is clean.")
		}
		return m.showSystemMessage("```diff\n" + msg.diff + "\n```")
	case "merge":
		if msg.merge.Committed {
			return m.showFooterSuccess("Merged and committed worktree changes onto root.")
		}
		return m.showFooterSuccess("Merged worktree changes onto root (staged, uncommitted). Review, commit, then /worktree rm when ready.")
	case "promote":
		return m.showFooterSuccess("Promoted worktree to branch " + msg.branch)
	case "remove":
		if msg.bound && msg.root != "" && m.sess != nil {
			if m.toolMgr != nil {
				_ = m.toolMgr.SetBaseDir(msg.root)
			}
			m.sess.WorktreeDir = ""
			m.sess.CWD = msg.root
			if m.store != nil {
				_ = m.store.Update(context.Background(), m.sess)
			}
		}
		return m.showFooterSuccess("Removed worktree.")
	default:
		return m.showFooterMuted("Worktree operation finished.")
	}
}

func (m *Model) worktreeCompletionItems(query string) ([]Command, bool) {
	query = strings.TrimPrefix(query, "/")
	if strings.TrimSpace(query) == "" {
		return nil, false
	}
	trailingSpace := strings.HasSuffix(query, " ")
	parts := strings.Fields(query)
	if len(parts) < 2 {
		return nil, false
	}
	cmd := strings.ToLower(parts[0])
	if cmd != "worktree" && cmd != "wt" {
		return nil, false
	}
	sub := strings.ToLower(parts[1])
	switch sub {
	case "switch", "diff", "merge", "rm", "remove":
		return m.worktreeTargetCompletionItems(parts, trailingSpace, sub), true
	case "new":
		return worktreeOptionCompletionItems(parts, trailingSpace, []worktreeOptionCompletion{
			{Name: "--base", Description: "Base ref for the new worktree"},
			{Name: "--branch", Description: "Create and check out a branch"},
			{Name: "-b", Description: "Create and check out a branch"},
		}), true
	}
	return nil, false
}

type worktreeOptionCompletion struct {
	Name        string
	Description string
}

func worktreeOptionCompletionItems(parts []string, trailingSpace bool, options []worktreeOptionCompletion) []Command {
	prefixParts, partial := completionPrefixAndPartial(parts, trailingSpace)
	if partial != "" && !strings.HasPrefix(partial, "-") {
		return nil
	}
	partialLower := strings.ToLower(partial)
	used := map[string]bool{}
	for _, p := range parts[2:] {
		used[p] = true
	}
	var items []Command
	for _, opt := range options {
		if used[opt.Name] && opt.Name != "-m" && opt.Name != "--message" {
			continue
		}
		if partialLower != "" && !strings.HasPrefix(strings.ToLower(opt.Name), partialLower) {
			continue
		}
		nameParts := append(append([]string{}, prefixParts...), opt.Name)
		items = append(items, Command{Name: strings.Join(nameParts, " "), Description: opt.Description})
	}
	return items
}

func (m *Model) worktreeTargetCompletionItems(parts []string, trailingSpace bool, sub string) []Command {
	prefixParts, partial := completionPrefixAndPartial(parts, trailingSpace)
	if partial != "" && strings.HasPrefix(partial, "-") {
		if sub == "rm" || sub == "remove" {
			return worktreeOptionCompletionItems(parts, trailingSpace, []worktreeOptionCompletion{
				{Name: "--force", Description: "Remove even if dirty/in use"},
				{Name: "-f", Description: "Remove even if dirty/in use"},
			})
		}
		if sub == "merge" {
			return worktreeOptionCompletionItems(parts, trailingSpace, []worktreeOptionCompletion{
				{Name: "--commit", Description: "Commit after staging changes on root"},
				{Name: "--allow-dirty", Description: "Allow dirty root checkout"},
				{Name: "--force", Description: "Alias for --allow-dirty"},
				{Name: "--message", Description: "Commit/message text"},
				{Name: "-m", Description: "Commit/message text"},
			})
		}
		return nil
	}
	root, err := m.repoRootForWorktree()
	if err != nil {
		return nil
	}
	items, err := worktree.List(root)
	if err != nil {
		return nil
	}
	partialLower := strings.ToLower(partial)
	var out []Command
	for _, wt := range items {
		nameLower := strings.ToLower(wt.Name)
		dirLower := strings.ToLower(wt.Dir)
		if partialLower != "" && !strings.Contains(nameLower, partialLower) && !strings.Contains(dirLower, partialLower) {
			continue
		}
		ref := "detached@" + shortSHA(wt.HeadSHA)
		if wt.Branch != "" {
			ref = wt.Branch
		}
		desc := fmt.Sprintf("%s · dirty:%d · %s", ref, wt.DirtyFiles, wt.Dir)
		nameParts := append(append([]string{}, prefixParts...), wt.Name)
		out = append(out, Command{Name: strings.Join(nameParts, " "), Description: desc})
	}
	return out
}

func completionPrefixAndPartial(parts []string, trailingSpace bool) ([]string, string) {
	if len(parts) <= 2 {
		return append([]string{}, parts...), ""
	}
	if trailingSpace {
		return append([]string{}, parts...), ""
	}
	return append([]string{}, parts[:len(parts)-1]...), parts[len(parts)-1]
}

func (m *Model) otherSessionsUsingWorktree(dir string) ([]worktree.InUseSession, error) {
	if m == nil || m.store == nil {
		return nil, nil
	}
	inUse, err := worktree.InUse(context.Background(), m.store, dir)
	if err != nil {
		return nil, err
	}
	current := ""
	if m.sess != nil {
		current = strings.TrimSpace(m.sess.ID)
	}
	if current == "" {
		return inUse, nil
	}
	filtered := inUse[:0]
	for _, item := range inUse {
		if strings.TrimSpace(item.ID) != current {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
