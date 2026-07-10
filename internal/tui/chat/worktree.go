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
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/worktree"
)

type worktreeOperationDoneMsg struct {
	op      string
	wt      *worktree.Worktree
	dir     string
	root    string
	branch  string
	bound   bool
	merge   worktree.MergeResult
	promote worktree.PromoteResult
	assist  worktree.AssistedMergeResult
	diff    string
	err     error
}

type pendingWorktreeRecovery struct {
	kind  string
	merge worktree.MergeResult
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
		m.clearWorktreeCommandComposer()
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

func (m *Model) clearWorktreeCommandComposer() {
	m.setTextareaValue("")
	if m.completions != nil {
		m.completions.Hide()
	}
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
	if err := m.applyRuntimeDirectory(abs, abs); err != nil {
		return err
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
		m.clearWorktreeCommandComposer()
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
	m.clearWorktreeCommandComposer()
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
	m.clearWorktreeCommandComposer()
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
	m.clearWorktreeCommandComposer()
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
	if err := m.bindRootDir(root); err != nil {
		return m.showFooterError(err.Error())
	}
	m.clearWorktreeCommandComposer()
	return m.showFooterSuccess("Back on root checkout: " + root)
}

func (m *Model) bindRootDir(root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return fmt.Errorf("root checkout is required")
	}
	if resolved, err := worktree.MainRepoRoot(root); err == nil {
		root = resolved
	}
	return m.applyRuntimeDirectory(root, "")
}

func (m *Model) applyRuntimeDirectory(dir, worktreeDir string) error {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "." || dir == "" {
		return fmt.Errorf("session directory is required")
	}
	if m.sess != nil && filepath.Clean(m.sess.CWD) == dir && filepath.Clean(m.sess.WorktreeDir) == filepath.Clean(worktreeDir) {
		return nil
	}

	candidate := m.runtimeSystemContext
	var err error
	if m.runtimeSystemContextResolver != nil {
		candidate, err = m.runtimeSystemContextResolver(m.currentAgent, m.providerKey, m.modelName, dir)
		if err != nil {
			return fmt.Errorf("resolve session context for %s: %w", dir, err)
		}
	}

	oldCWD, oldWorktree := "", ""
	if m.sess != nil {
		oldCWD, oldWorktree = m.sess.CWD, m.sess.WorktreeDir
	}
	rollbackBase := ""
	if m.toolMgr != nil {
		rollbackBase = runtimeRollbackBase(m.toolMgr.BaseDir(), oldCWD)
		if err := m.toolMgr.SetBaseDir(dir); err != nil {
			return err
		}
	}
	if m.sess != nil {
		m.sess.CWD, m.sess.WorktreeDir = dir, worktreeDir
		if m.store != nil {
			if err := m.store.Update(context.Background(), m.sess); err != nil {
				m.sess.CWD, m.sess.WorktreeDir = oldCWD, oldWorktree
				if m.toolMgr != nil && rollbackBase != "" {
					_ = m.toolMgr.SetBaseDir(rollbackBase)
				}
				return fmt.Errorf("persist session directory: %w", err)
			}
		}
	}

	prompt := candidate.SystemPrompt
	if m.systemPromptOverridden {
		prompt = m.systemPromptOverride
	}
	oldContext := m.runtimeSystemContext
	var oldMessage *session.Message
	m.messagesMu.Lock()
	for i := range m.messages {
		if m.messages[i].Role == llm.RoleSystem {
			copyMsg := m.messages[i]
			oldMessage = &copyMsg
			updated := copyMsg
			updated.Parts = []llm.Part{{Type: llm.PartText, Text: prompt}}
			updated.TextContent = prompt
			if m.store != nil && m.sess != nil && updated.ID != 0 {
				if err := m.store.UpdateMessage(context.Background(), m.sess.ID, &updated); err != nil {
					m.messagesMu.Unlock()
					m.rollbackRuntimeDirectory(rollbackBase, oldCWD, oldWorktree, oldMessage, oldContext)
					return fmt.Errorf("persist refreshed system prompt: %w", err)
				}
			}
			m.messages[i] = updated
			break
		}
	}
	m.messagesMu.Unlock()
	if candidate.ApplySkills != nil {
		candidate.ApplySkills(m.engine, m.toolMgr)
	}
	m.runtimeSystemContext = candidate
	if m.config != nil && !m.systemPromptOverridden {
		m.config.Chat.Instructions = prompt
	}
	if m.approvedDirs != nil {
		_ = m.approvedDirs.AddDirectory(dir)
	}
	m.invalidateHistoryCache()
	m.resetContextEstimateBaseline(context.Background())
	return nil
}

func runtimeRollbackBase(baseDir, sessionCWD string) string {
	if baseDir = strings.TrimSpace(baseDir); baseDir != "" {
		return baseDir
	}
	if sessionCWD = strings.TrimSpace(sessionCWD); sessionCWD != "" {
		return sessionCWD
	}
	cwd, _ := os.Getwd()
	return cwd
}

func (m *Model) rollbackRuntimeDirectory(rollbackBase, oldCWD, oldWorktree string, oldMessage *session.Message, oldContext RuntimeSystemContext) {
	if m.toolMgr != nil && rollbackBase != "" {
		_ = m.toolMgr.SetBaseDir(rollbackBase)
	}
	if m.sess != nil {
		m.sess.CWD, m.sess.WorktreeDir = oldCWD, oldWorktree
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
	if oldMessage != nil {
		if m.store != nil && m.sess != nil && oldMessage.ID != 0 {
			_ = m.store.UpdateMessage(context.Background(), m.sess.ID, oldMessage)
		}
	}
	if oldContext.ApplySkills != nil {
		oldContext.ApplySkills(m.engine, m.toolMgr)
	}
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
		m.clearWorktreeCommandComposer()
		return m.showFooterMuted("No worktree is bound.")
	}
	m.worktreeOperation = "diff"
	m.clearWorktreeCommandComposer()
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
	usingBound := target == ""
	if target != "" {
		resolved, err := m.resolveWorktreeTarget(target)
		if err != nil {
			return m.showFooterError(err.Error())
		}
		dir = resolved
	}
	if dir == "" {
		m.clearWorktreeCommandComposer()
		return m.showFooterMuted("No worktree is bound.")
	}
	wt, err := worktree.Get(dir)
	if err != nil {
		return m.showFooterError(err.Error())
	}
	dir = wt.Dir
	parentCtx := m.rootContext()
	m.worktreeOperation = "merge"
	preflight := formatWorktreeMergePreflight(wt, usingBound, opts)
	m.clearWorktreeCommandComposer()
	return m.showSystemMessageWithCmd(preflight, func() tea.Msg {
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
	branch := ""
	nonFlagArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return m.showFooterError("Usage: /worktree promote [name-or-dir] [branch]")
		}
		nonFlagArgs = append(nonFlagArgs, arg)
	}
	switch len(nonFlagArgs) {
	case 0:
		if dir == "" {
			m.clearWorktreeCommandComposer()
			return m.showFooterMuted("No worktree is bound.")
		}
	case 1:
		if dir == "" {
			resolved, err := m.resolveWorktreeTarget(nonFlagArgs[0])
			if err != nil {
				return m.showFooterError(err.Error())
			}
			dir = resolved
		} else {
			branch = nonFlagArgs[0]
		}
	case 2:
		resolved, err := m.resolveWorktreeTarget(nonFlagArgs[0])
		if err != nil {
			return m.showFooterError(err.Error())
		}
		dir = resolved
		branch = nonFlagArgs[1]
	default:
		return m.showFooterError("Usage: /worktree promote [name-or-dir] [branch]")
	}
	wt, err := worktree.Get(dir)
	if err != nil {
		return m.showFooterError(err.Error())
	}
	dir = wt.Dir
	if strings.TrimSpace(branch) == "" {
		branch = defaultWorktreePromoteBranch(wt)
	}
	if branch == "" {
		return m.showFooterError("Usage: /worktree promote [name-or-dir] [branch]")
	}
	parentCtx := m.rootContext()
	m.worktreeOperation = "promote"
	preflight := formatWorktreePromotePreflight(wt, branch)
	m.clearWorktreeCommandComposer()
	return m.showSystemMessageWithCmd(preflight, func() tea.Msg {
		res, err := worktree.PromoteToRoot(parentCtx, dir, branch, worktree.PromoteOptions{})
		return worktreeOperationDoneMsg{op: "promote", dir: dir, branch: branch, promote: res, err: err}
	})
}

func defaultWorktreePromoteBranch(wt *worktree.Worktree) string {
	if wt == nil {
		return ""
	}
	if name := strings.TrimSpace(wt.Name); name != "" {
		return name
	}
	if dir := strings.TrimSpace(wt.Dir); dir != "" {
		return filepath.Base(dir)
	}
	return ""
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
	m.clearWorktreeCommandComposer()
	return m.showFooterMutedWithCmd("Removing worktree…", func() tea.Msg {
		err := worktree.Remove(parentCtx, dir, worktree.RemoveOptions{Force: force})
		return worktreeOperationDoneMsg{op: "remove", dir: dir, root: root, bound: bound, err: err}
	})
}

func (m *Model) handleWorktreeOperationDone(msg worktreeOperationDoneMsg) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(m.worktreeOperation) != "" {
		if msg.op == "" || msg.op != m.worktreeOperation {
			return m, nil
		}
		m.worktreeOperation = ""
	}
	if msg.err != nil {
		switch {
		case msg.op == "merge" && errors.Is(msg.err, worktree.ErrConflict):
			pending := pendingWorktreeRecovery{kind: "conflict", merge: msg.merge}
			m.pendingWorktreeRecovery = &pending
			m.openWorktreeRecoveryPrompt(pending)
			return m.showSystemMessage(formatWorktreeMergeConflictMessage(msg.merge))
		case msg.op == "merge" && errors.Is(msg.err, worktree.ErrRootDirty):
			pending := pendingWorktreeRecovery{kind: "dirty-root", merge: msg.merge}
			m.pendingWorktreeRecovery = &pending
			m.openWorktreeRecoveryPrompt(pending)
			return m.showSystemMessage(formatWorktreeMergeDirtyRootMessage(msg.merge))
		case msg.op == "promote" && errors.Is(msg.err, worktree.ErrRootDirty):
			return m.showSystemMessage(formatWorktreePromoteDirtyRootMessage(msg.promote))
		case msg.op == "assist-merge" && errors.Is(msg.err, worktree.ErrRootDirty):
			return m.showSystemMessage(formatAssistedMergeRootDirtyMessage(msg.assist))
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
		m.pendingWorktreeRecovery = nil
		return m.showSystemMessage(formatWorktreeMergeSuccessMessage(msg.merge))
	case "assist-merge":
		m.pendingWorktreeRecovery = nil
		if msg.assist.RootDir != "" {
			if err := m.bindRootDir(msg.assist.RootDir); err != nil {
				return m.showFooterError(err.Error())
			}
		}
		return m.startAssistedMergeLLM(msg.assist)
	case "promote":
		if msg.promote.RootDir != "" {
			if err := m.bindRootDir(msg.promote.RootDir); err != nil {
				return m.showFooterError(err.Error())
			}
		}
		return m.showSystemMessage(formatWorktreePromoteSuccessMessage(msg.promote))
	case "remove":
		if msg.bound && msg.root != "" {
			if err := m.bindRootDir(msg.root); err != nil {
				return m.showFooterError(err.Error())
			}
		}
		return m.showFooterSuccess("Removed worktree.")
	default:
		return m.showFooterMuted("Worktree operation finished.")
	}
}

func formatWorktreeMergePreflight(wt *worktree.Worktree, usingBound bool, opts worktree.MergeOptions) string {
	var b strings.Builder
	name := worktreeDisplayName(wt.Name)
	fmt.Fprintf(&b, "Merging worktree %s (%s) into root checkout\n\n", name, wt.Dir)
	if usingBound {
		fmt.Fprintf(&b, "Using currently bound worktree: %s\n", name)
	}
	fmt.Fprintf(&b, "Source: %s (%s)\n", name, wt.Dir)
	fmt.Fprintf(&b, "Destination: %s\n", wt.RepoRoot)
	if opts.Commit {
		b.WriteString("Result if successful: commit the merged changes on the root checkout.\n")
	} else {
		b.WriteString("Result if successful: stage the changes on the root checkout (uncommitted).\n")
	}
	b.WriteString("The current session stays bound to the worktree until you run `/worktree root` or promote it.\n")
	return b.String()
}

func formatWorktreeMergeSuccessMessage(res worktree.MergeResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Merged worktree %s → root checkout\n\n", worktreeDisplayName(res.WorktreeName))
	fmt.Fprintf(&b, "Source: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination: %s\n", res.RootDir)
	if res.SnapshotCommit != "" {
		fmt.Fprintf(&b, "Snapshot: %s\n", shortSHA(res.SnapshotCommit))
	}
	if res.Committed {
		b.WriteString("Result: changes were committed on the root checkout.\n")
	} else if res.Applied {
		b.WriteString("Result: changes are staged and uncommitted on the root checkout.\n")
	} else {
		b.WriteString("Result: no worktree changes needed to be applied.\n")
	}
	b.WriteString("Current session: still bound to the source worktree. `/shell` still opens the worktree until you run `/worktree root`.\n")
	appendLinesSection(&b, "Changed files", res.ChangedFiles, 20)
	b.WriteString("\nNext:\n")
	b.WriteString("  /worktree root\n")
	b.WriteString("  /shell\n")
	b.WriteString("  git status\n")
	if !res.Committed && res.Applied {
		b.WriteString("  git commit -m \"...\"\n")
	}
	b.WriteString("  /worktree rm " + shellishName(res.WorktreeName, res.WorktreeDir) + "   # when you are done\n")
	return b.String()
}

func formatWorktreeMergeDirtyRootMessage(res worktree.MergeResult) string {
	var b strings.Builder
	b.WriteString("Merge not attempted: the root checkout has uncommitted changes.\n\n")
	fmt.Fprintf(&b, "Source: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination: %s\n", res.RootDir)
	appendStatusSection(&b, "Root status", res.RootStatus, 30)
	b.WriteString("\nInteractive recovery:\n")
	b.WriteString("  A Yes/No prompt has opened in the TUI. Select Yes to let me inspect the dirty root/worktree state and help sort it out before retrying.\n")
	b.WriteString("  Select No to leave everything unchanged.\n")
	b.WriteString("\nNext:\n")
	b.WriteString("  /worktree root\n")
	b.WriteString("  /shell\n")
	b.WriteString("  git status\n")
	b.WriteString("  git commit -m \"...\"    # or git stash / discard the root changes\n")
	b.WriteString("  /worktree merge " + shellishName(res.WorktreeName, res.WorktreeDir) + "\n")
	appendMergeRecoveryPrompt(&b, res, "dirty root preflight refusal")
	return b.String()
}

func formatWorktreeMergeConflictMessage(res worktree.MergeResult) string {
	var b strings.Builder
	if res.ConflictReset {
		b.WriteString("Merge conflict: the root checkout was reset cleanly.\n\n")
	} else {
		b.WriteString("Merge conflict: the root checkout may still need cleanup.\n\n")
	}
	fmt.Fprintf(&b, "Source: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination: %s\n", res.RootDir)
	if res.SnapshotCommit != "" {
		fmt.Fprintf(&b, "Snapshot: %s\n", shortSHA(res.SnapshotCommit))
	}
	if res.ConflictReset {
		b.WriteString("Cleanup: ran `git reset --merge` and `git cherry-pick --quit`; no merge state should remain in root.\n")
	} else {
		b.WriteString("Cleanup: attempted `git reset --merge` and `git cherry-pick --quit`, but cleanup did not fully complete. Inspect the root checkout with `git status` before retrying.\n")
		if strings.TrimSpace(res.ConflictCleanupError) != "" {
			fmt.Fprintf(&b, "Cleanup error: %s\n", res.ConflictCleanupError)
		}
	}
	appendLinesSection(&b, "Conflicts", res.Conflicts, 20)
	appendLinesSection(&b, "Changed files in source snapshot", res.ChangedFiles, 30)
	statusTitle := "Root status after cleanup"
	if !res.ConflictReset {
		statusTitle = "Root status after attempted cleanup"
	}
	appendStatusSection(&b, statusTitle, res.RootStatus, 30)
	b.WriteString("\nInteractive recovery:\n")
	b.WriteString("  A Yes/No prompt has opened in the TUI. Select Yes to merge this for you on a safe recovery branch.\n")
	b.WriteString("  Select No to leave the root clean and keep working manually.\n")
	b.WriteString("\nNext options:\n")
	b.WriteString("  /worktree promote <branch>   # continue from root on a branch based at the worktree HEAD\n")
	b.WriteString("  /worktree root && /shell     # inspect the clean root checkout\n")
	b.WriteString("  Ask the LLM to compare/rebase the worktree before retrying `/worktree merge`.\n")
	appendMergeRecoveryPrompt(&b, res, "merge conflict after cherry-pick -n")
	return b.String()
}

func formatWorktreePromotePreflight(wt *worktree.Worktree, branch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Promoting worktree %s (%s) to root branch %s\n\n", worktreeDisplayName(wt.Name), wt.Dir, branch)
	fmt.Fprintf(&b, "Source: %s (%s)\n", worktreeDisplayName(wt.Name), wt.Dir)
	fmt.Fprintf(&b, "Destination root: %s\n", wt.RepoRoot)
	b.WriteString("Result if successful: checkout the new branch in the root project checkout, apply dirty worktree changes there, and rebind this session to root.\n")
	return b.String()
}

func formatWorktreePromoteSuccessMessage(res worktree.PromoteResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Promoted %s → branch %s\n\n", worktreeDisplayName(res.WorktreeName), res.Branch)
	fmt.Fprintf(&b, "Checked out %s in root: %s\n", res.Branch, res.RootDir)
	if res.Applied {
		b.WriteString("Your dirty worktree changes are staged/uncommitted there.\n")
	} else {
		b.WriteString("No dirty worktree changes needed to be applied; the branch points at the worktree HEAD.\n")
	}
	b.WriteString("You are no longer bound to the worktree; `/shell` now opens the root checkout.\n")
	if res.OriginalWorktreeStillExists {
		fmt.Fprintf(&b, "Original worktree still exists at: %s\n", res.WorktreeDir)
	}
	if res.WorktreeHead != "" {
		fmt.Fprintf(&b, "Branch base/worktree HEAD: %s\n", shortSHA(res.WorktreeHead))
	}
	if res.PreviousRootBranch != "" || res.PreviousRootRef != "" {
		b.WriteString("Note: the new branch starts at the worktree HEAD, so it may be behind the previous root branch until you rebase/merge as needed.\n")
	}
	if res.PreviousRootBranch != "" {
		fmt.Fprintf(&b, "Previous root branch: %s (%s)\n", res.PreviousRootBranch, shortSHA(res.PreviousRootRef))
		fmt.Fprintf(&b, "Escape hatch: git checkout %s\n", res.PreviousRootBranch)
	} else if res.PreviousRootRef != "" {
		fmt.Fprintf(&b, "Previous root ref: %s\n", shortSHA(res.PreviousRootRef))
		fmt.Fprintf(&b, "Escape hatch: git checkout %s\n", res.PreviousRootRef)
	}
	appendLinesSection(&b, "Changed files", res.ChangedFiles, 20)
	appendStatusSection(&b, "Root status", res.RootStatus, 30)
	b.WriteString("\nNext:\n")
	b.WriteString("  /shell\n")
	b.WriteString("  git status\n")
	if res.Applied {
		b.WriteString("  git commit -m \"...\"\n")
	}
	fmt.Fprintf(&b, "  git push -u origin %s\n", res.Branch)
	return b.String()
}

func formatWorktreePromoteDirtyRootMessage(res worktree.PromoteResult) string {
	var b strings.Builder
	b.WriteString("Promote not attempted: the root checkout has uncommitted changes.\n\n")
	fmt.Fprintf(&b, "Source: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination root: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Requested branch: %s\n", res.Branch)
	appendStatusSection(&b, "Root status", res.RootStatus, 30)
	b.WriteString("\nNext:\n")
	b.WriteString("  /worktree root\n")
	b.WriteString("  /shell\n")
	b.WriteString("  git status\n")
	b.WriteString("  commit, stash, or discard root changes\n")
	fmt.Fprintf(&b, "  /worktree promote %s\n", res.Branch)
	return b.String()
}

func (m *Model) openWorktreeRecoveryPrompt(pending pendingWorktreeRecovery) {
	if m == nil || m.dialog == nil {
		return
	}
	title, question := worktreeRecoveryPromptText(pending)
	m.dialog.ShowWorktreeRecovery(title, question)
	m.scrollToBottom = true
}

func worktreeRecoveryPromptText(pending pendingWorktreeRecovery) (string, string) {
	switch pending.kind {
	case "conflict":
		return "Assisted Worktree Recovery", "This worktree does not merge cleanly. Would you like me to merge this for you on a safe recovery branch?"
	case "dirty-root":
		return "Assisted Worktree Recovery", "The root checkout is dirty. Would you like me to inspect the dirty root/worktree state and help sort it out before retrying?"
	default:
		return "Assisted Worktree Recovery", "Would you like assisted worktree merge recovery?"
	}
}

func (m *Model) resolveWorktreeRecoveryPrompt(proceed bool) (tea.Model, tea.Cmd) {
	if m == nil {
		return m, nil
	}
	if m.dialog != nil && m.dialog.IsOpen() && m.dialog.Type() == DialogWorktreeRecovery {
		m.dialog.Close()
	}
	if m.pendingWorktreeRecovery == nil {
		return m.showFooterMuted("No pending worktree recovery.")
	}
	pending := *m.pendingWorktreeRecovery
	m.pendingWorktreeRecovery = nil
	if proceed {
		return m.startPendingWorktreeRecovery(pending)
	}
	return m.showSystemMessage(formatWorktreeRecoveryDeclinedMessage(pending))
}

func formatWorktreeRecoveryDeclinedMessage(pending pendingWorktreeRecovery) string {
	if pending.kind == "dirty-root" {
		return "Okay — leaving the root checkout unchanged. Clean/commit/stash root changes, then retry `/worktree merge` when ready."
	}
	return "Okay — leaving the root checkout clean. You can retry `/worktree merge`, use `/worktree promote <branch>`, or ask for help later."
}

func (m *Model) startPendingWorktreeRecovery(pending pendingWorktreeRecovery) (tea.Model, tea.Cmd) {
	if pending.kind == "dirty-root" {
		if strings.TrimSpace(pending.merge.RootDir) != "" {
			if err := m.bindRootDir(pending.merge.RootDir); err != nil {
				return m.showFooterError(err.Error())
			}
		}
		prompt := formatDirtyRootAssistedMergePrompt(pending.merge)
		return m.sendMessage(prompt)
	}
	parentCtx := m.rootContext()
	m.worktreeOperation = "assist-merge"
	message := fmt.Sprintf("Okay — preparing a safe recovery branch for %s. I will ask the LLM to resolve it there without committing or pushing.", worktreeDisplayName(pending.merge.WorktreeName))
	return m.showSystemMessageWithCmd(message, func() tea.Msg {
		res, err := worktree.StartAssistedMerge(parentCtx, pending.merge.WorktreeDir, worktree.AssistedMergeOptions{})
		return worktreeOperationDoneMsg{op: "assist-merge", dir: pending.merge.WorktreeDir, assist: res, err: err}
	})
}

func (m *Model) startAssistedMergeLLM(res worktree.AssistedMergeResult) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(res.RootDir) == "" {
		return m.showFooterError("assisted merge failed: missing root checkout")
	}
	if len(res.ChangedFiles) == 0 {
		return m.showSystemMessage(formatAssistedMergeNothingToApplyMessage(res))
	}
	prompt := formatAssistedMergeLLMPrompt(res)
	_, systemCmd := m.showSystemMessage(formatAssistedMergeReadyMessage(res))
	model, streamCmd := m.sendMessage(prompt)
	return model, tea.Sequence(systemCmd, streamCmd)
}

func formatAssistedMergeRootDirtyMessage(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	b.WriteString("Assisted recovery could not start because the root checkout became dirty.\n\n")
	fmt.Fprintf(&b, "Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	appendStatusSection(&b, "Root status", res.RootStatus, 30)
	b.WriteString("\nNo recovery branch was created. Clean/commit/stash root changes, then retry `/worktree merge`.\n")
	return b.String()
}

func formatAssistedMergeNothingToApplyMessage(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	b.WriteString("Assisted recovery did not need to start: there are no worktree changes to apply.\n\n")
	fmt.Fprintf(&b, "Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	if res.SnapshotCommit != "" {
		fmt.Fprintf(&b, "Snapshot checked: %s\n", shortSHA(res.SnapshotCommit))
	}
	b.WriteString("No recovery branch was created.\n")
	return b.String()
}

func formatAssistedMergeReadyMessage(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Prepared assisted worktree merge on branch %s\n\n", res.Branch)
	fmt.Fprintf(&b, "Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	if res.NeedsResolution {
		b.WriteString("Status: conflicts are intentionally left on this recovery branch for the LLM to resolve.\n")
	} else if res.Applied {
		b.WriteString("Status: worktree changes applied cleanly, staged/uncommitted on the recovery branch.\n")
	}
	appendLinesSection(&b, "Conflicts", res.Conflicts, 20)
	appendStatusSection(&b, "Root status", res.RootStatus, 30)
	b.WriteString("\nI am sending the LLM a recovery task now. It may inspect/edit files and run local commands, but it must not commit or push.\n")
	b.WriteString("Abort manually if needed:\n")
	b.WriteString("  git reset --merge\n")
	b.WriteString("  git cherry-pick --quit\n")
	if res.PreviousRootBranch != "" {
		fmt.Fprintf(&b, "  git checkout %s\n", res.PreviousRootBranch)
	} else if res.PreviousRootRef != "" {
		fmt.Fprintf(&b, "  git checkout %s\n", res.PreviousRootRef)
	}
	fmt.Fprintf(&b, "  git branch -D %s\n", res.Branch)
	return b.String()
}

func formatAssistedMergeLLMPrompt(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	b.WriteString("The user confirmed interactive recovery for a failed `/worktree merge`. You have permission to sort this out on the prepared recovery branch.\n\n")
	b.WriteString("Goal:\n")
	b.WriteString("- Resolve/apply the source worktree changes onto the current root checkout branch.\n")
	b.WriteString("- Leave the result staged/uncommitted in the root checkout.\n")
	b.WriteString("- Do not commit, push, delete branches, discard user changes, or remove the original worktree unless the user explicitly confirms later.\n\n")
	b.WriteString("State:\n")
	fmt.Fprintf(&b, "- Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "- Recovery branch checked out in root: %s\n", res.Branch)
	fmt.Fprintf(&b, "- Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "- Previous root branch/ref: %s %s\n", res.PreviousRootBranch, res.PreviousRootRef)
	fmt.Fprintf(&b, "- Worktree base SHA: %s\n", res.Base)
	fmt.Fprintf(&b, "- Root HEAD before recovery: %s\n", res.RootHead)
	fmt.Fprintf(&b, "- Worktree HEAD: %s\n", res.WorktreeHead)
	fmt.Fprintf(&b, "- Snapshot commit being applied: %s\n", res.SnapshotCommit)
	if len(res.Conflicts) > 0 {
		fmt.Fprintf(&b, "- Conflict files: %s\n", strings.Join(res.Conflicts, ", "))
	}
	if len(res.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "- Changed files: %s\n", strings.Join(res.ChangedFiles, "; "))
	}
	if strings.TrimSpace(res.RootStatus) != "" {
		fmt.Fprintf(&b, "- Current root status:\n%s\n", res.RootStatus)
	}
	b.WriteString("Instructions:\n")
	b.WriteString("0. Use available shell/read/edit tools as needed; operate in the root checkout unless inspecting the source worktree.\n")
	b.WriteString("1. Start with `git status --short` in the root checkout and inspect conflicted files if any.\n")
	b.WriteString("2. Resolve conflict markers or apply equivalent edits that preserve the user's worktree intent on top of current root.\n")
	b.WriteString("3. Stage resolved files with `git add` as appropriate.\n")
	b.WriteString("4. If a cherry-pick state remains after staging, run `git cherry-pick --quit` (not `--continue`) so the result stays uncommitted.\n")
	b.WriteString("5. Finish by running `git status --short` and summarizing what changed plus next commands: `git status`, `git commit -m \"...\"`, and `git push -u origin <branch>`.\n")
	return b.String()
}

func formatDirtyRootAssistedMergePrompt(res worktree.MergeResult) string {
	var b strings.Builder
	target := shellishName(res.WorktreeName, res.WorktreeDir)
	b.WriteString("The user confirmed interactive recovery for a `/worktree merge` that was blocked because the root checkout is dirty. You have permission to inspect and help sort this out safely.\n\n")
	b.WriteString("Goal:\n")
	b.WriteString("- Determine why the root checkout is dirty and recommend or perform safe steps to preserve those changes before retrying the worktree merge.\n")
	b.WriteString("- Do not discard, overwrite, commit, push, or stash changes unless you clearly explain the action first and it is safe. If uncertain, ask the user.\n\n")
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination root: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Base SHA: %s\n", res.Base)
	fmt.Fprintf(&b, "Root HEAD: %s\n", res.RootHead)
	fmt.Fprintf(&b, "Worktree HEAD: %s\n", res.WorktreeHead)
	if strings.TrimSpace(res.RootStatus) != "" {
		fmt.Fprintf(&b, "Root status that blocked merge:\n%s\n", res.RootStatus)
	}
	b.WriteString("\nSuggested first commands:\n")
	b.WriteString("- Use available shell/read/edit tools as needed; operate in the root checkout unless inspecting the source worktree.\n")
	b.WriteString("- `git status --short` in the root checkout\n")
	b.WriteString("- inspect relevant diffs before deciding whether to commit, stash, or ask the user\n")
	b.WriteString("- once root is clean, retry `/worktree merge " + target + "` or use `/worktree promote <branch>` if safer\n")
	return b.String()
}

func appendMergeRecoveryPrompt(b *strings.Builder, res worktree.MergeResult, reason string) {
	b.WriteString("\nLLM-assisted recovery prompt (send this if you want guidance):\n")
	b.WriteString("```text\n")
	b.WriteString("Help me choose a safe recovery strategy for a term-llm worktree merge. Do not edit files or run commands until I confirm.\n")
	fmt.Fprintf(b, "Reason: %s\n", reason)
	fmt.Fprintf(b, "Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(b, "Destination root: %s\n", res.RootDir)
	fmt.Fprintf(b, "Base SHA: %s\n", res.Base)
	fmt.Fprintf(b, "Root HEAD: %s\n", res.RootHead)
	fmt.Fprintf(b, "Worktree HEAD: %s\n", res.WorktreeHead)
	fmt.Fprintf(b, "Snapshot commit: %s\n", res.SnapshotCommit)
	if len(res.Conflicts) > 0 {
		fmt.Fprintf(b, "Conflict files: %s\n", strings.Join(res.Conflicts, ", "))
	}
	if len(res.ChangedFiles) > 0 {
		fmt.Fprintf(b, "Changed files: %s\n", strings.Join(res.ChangedFiles, "; "))
	}
	if strings.TrimSpace(res.RootStatus) != "" {
		fmt.Fprintf(b, "Root status:\n%s\n", res.RootStatus)
	} else {
		b.WriteString("Root status: clean after cleanup/preflight.\n")
	}
	b.WriteString("Commands already run: snapshot commit, git cherry-pick -n (if merge reached apply), git reset --merge and git cherry-pick --quit on conflict.\n")
	b.WriteString("Please recommend one of: clean root then retry, promote to a branch, rebase/update the worktree, or manually apply a small patch.\n")
	b.WriteString("```\n")
}

func appendLinesSection(b *strings.Builder, title string, lines []string, max int) {
	if len(lines) == 0 {
		return
	}
	if max <= 0 || max > len(lines) {
		max = len(lines)
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, line := range lines[:max] {
		fmt.Fprintf(b, "- %s\n", line)
	}
	if len(lines) > max {
		fmt.Fprintf(b, "- … and %d more\n", len(lines)-max)
	}
}

func appendStatusSection(b *strings.Builder, title, status string, max int) {
	lines := nonEmptyLines(status)
	if len(lines) == 0 {
		fmt.Fprintf(b, "\n%s: clean\n", title)
		return
	}
	appendLinesSection(b, title, lines, max)
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func worktreeDisplayName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "worktree"
	}
	return name
}

func shellishName(name, dir string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return dir
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
	case "promote":
		if m == nil || m.sess == nil || strings.TrimSpace(m.sess.WorktreeDir) == "" {
			return m.worktreeTargetCompletionItems(parts, trailingSpace, sub), true
		}
		return nil, false
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
