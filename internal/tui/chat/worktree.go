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
	cleanup worktree.CleanupResult
	merge   worktree.MergeResult
	promote worktree.PromoteResult
	assist  worktree.AssistedMergeResult
	diff    string
	err     error
}

type pendingWorktreeRecovery struct {
	kind  string
	merge worktree.MergeResult
	bound bool
	inUse []worktree.InUseSession
}

func (m *Model) cmdWorktree(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.showWorktreeContent("Worktree Commands", "Usage: /worktree [new|browse|switch|root|pwd|diff|promote|rm]")
	}
	sub := strings.ToLower(args[0])
	if sub == "remove" {
		sub = "rm"
	}
	subArgs := args[1:]
	switch sub {
	case "pwd":
		m.clearWorktreeCommandComposer()
		return m.showSystemMessage(m.boundWorktreeDir())
	case "browse":
		return m.openWorktreeBrowser()
	case "new":
		return m.cmdWorktreeNew(subArgs)
	case "switch":
		return m.cmdWorktreeSwitch(subArgs)
	case "root":
		return m.cmdWorktreeRoot()
	case "diff":
		return m.cmdWorktreeDiff(subArgs)
	case "promote":
		return m.cmdWorktreePromote(subArgs)
	case "rm":
		return m.cmdWorktreeRemove(subArgs)
	default:
		return m.showFooterError("Unknown /worktree subcommand: " + sub)
	}
}

func (m *Model) activeWorktreeDir() string {
	if m != nil && m.sess != nil {
		cwd := strings.TrimSpace(m.sess.CWD)
		stored := strings.TrimSpace(m.sess.WorktreeDir)
		if cwd == "" {
			return stored
		}
		if sameWorktreePath(cwd, stored) {
			return cwd
		}
		if root, err := worktree.MainRepoRoot(cwd); err == nil && !sameWorktreePath(root, cwd) {
			return cwd
		}
	}
	return ""
}

func (m *Model) boundWorktreeDir() string {
	if m != nil && m.sess != nil && strings.TrimSpace(m.sess.CWD) != "" {
		return m.sess.CWD
	}
	if m != nil && m.sess != nil && strings.TrimSpace(m.sess.WorktreeDir) != "" {
		return m.sess.WorktreeDir
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// resolveHandoverPath returns the path pinned in the system prompt and the
// effective session's handover directory. The process-CWD directory is also a
// candidate for prompts created before a worktree switch or by older versions.
func (m *Model) resolveHandoverPath(prompt string) (path, handoverDir string, pinned bool, err error) {
	handoverDir, err = session.GetHandoverDir(m.boundWorktreeDir())
	if err != nil {
		return "", "", false, err
	}
	candidateDirs := []string{handoverDir}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		if processDir, dirErr := session.GetHandoverDir(cwd); dirErr == nil && filepath.Clean(processDir) != filepath.Clean(handoverDir) {
			candidateDirs = append(candidateDirs, processDir)
		}
	}
	path, pinned = session.ResolvePinnedHandoverPath(prompt, candidateDirs...)
	return path, handoverDir, pinned, nil
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

func (m *Model) showWorktreeContent(title, content string) (tea.Model, tea.Cmd) {
	m.clearWorktreeCommandComposer()
	m.clearFooterMessage()
	if m.dialog != nil {
		m.dialog.ShowContent(title, content)
		m.scrollToBottom = true
	}
	return m, nil
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
	return resolveWorktreeTargetFrom(m.boundWorktreeDir(), target)
}

func resolveWorktreeTargetFrom(start, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("worktree target is required")
	}
	if filepath.IsAbs(target) {
		return target, nil
	}
	if strings.TrimSpace(start) == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	root, err := worktree.MainRepoRoot(start)
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
	opts := worktree.CreateOptions{Base: "HEAD", MoveChanges: true}
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

	if !m.systemPromptOverridden {
		candidate.SystemPrompt = carryPinnedHandoverPath(m.currentSystemPromptText(), candidate.SystemPrompt)
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
	m.pendingTerminalDirectory = dir
	return nil
}

func carryPinnedHandoverPath(oldPrompt, candidatePrompt string) string {
	// Deliberately omit candidate directories: only the planner's explicit,
	// globally rooted assignment is durable enough to carry across projects.
	// Legacy directory-derived matches and ambiguous assignments must not be
	// guessed during a runtime prompt refresh.
	oldPath, oldPinned := session.ResolvePinnedHandoverPath(oldPrompt)
	candidatePath, candidatePinned := session.ResolvePinnedHandoverPath(candidatePrompt)
	if !oldPinned || oldPath == "" || !candidatePinned || candidatePath == "" || oldPath == candidatePath {
		return candidatePrompt
	}
	return strings.ReplaceAll(candidatePrompt, candidatePath, oldPath)
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
	} else {
		dir = m.activeWorktreeDir()
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

func (m *Model) cmdWorktreePromoteCurrent() (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Cannot promote worktrees while a response is streaming.")
	}
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	dir := m.activeWorktreeDir()
	if dir == "" {
		m.clearWorktreeCommandComposer()
		return m.showFooterMuted("No worktree is bound.")
	}
	sessionID := ""
	if m.sess != nil {
		sessionID = strings.TrimSpace(m.sess.ID)
	}
	parentCtx := m.rootContext()
	store := m.store
	m.worktreeOperation = "merge"
	m.clearWorktreeCommandComposer()
	return m.showFooterMutedWithCmd("Promoting worktree into root…", func() tea.Msg {
		res, cleanup, err := worktree.MergeBackAndCleanup(parentCtx, dir, worktree.MergeOptions{}, store, sessionID)
		return worktreeOperationDoneMsg{op: "merge", dir: dir, root: res.RootDir, bound: true, cleanup: cleanup, merge: res, err: err}
	})
}

func (m *Model) cmdWorktreePromote(args []string) (tea.Model, tea.Cmd) {
	switch {
	case len(args) == 0:
		return m.cmdWorktreePromoteCurrent()
	case len(args) == 1 && args[0] == "--branch":
		return m.cmdWorktreePromoteBranch()
	default:
		return m.showFooterError("Usage: /worktree promote [--branch]")
	}
}

func (m *Model) cmdWorktreePromoteBranch() (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Cannot promote worktrees while a response is streaming.")
	}
	if m.worktreeOperationBusy() {
		return m.worktreeBusyMessage()
	}
	dir := m.activeWorktreeDir()
	if dir == "" {
		m.clearWorktreeCommandComposer()
		return m.showFooterMuted("No worktree is bound.")
	}
	parentCtx := m.rootContext()
	m.worktreeOperation = "promote"
	m.clearWorktreeCommandComposer()
	return m.showFooterMutedWithCmd("Promoting worktree to its own branch…", func() tea.Msg {
		res, err := worktree.PromoteToRoot(parentCtx, dir, "", worktree.PromoteOptions{})
		return worktreeOperationDoneMsg{op: "promote", dir: dir, branch: res.Branch, promote: res, err: err}
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
	if dir == "" {
		dir = m.activeWorktreeDir()
	}
	if dir == "" {
		return m.showFooterError("Usage: /worktree rm [name-or-dir] [--force]")
	}
	resolvedDir, err := m.resolveWorktreeTarget(dir)
	if err != nil {
		return m.showFooterError(err.Error())
	}
	dir = resolvedDir
	bound := sameWorktreePath(m.activeWorktreeDir(), dir)
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
			return m, nil
		case msg.op == "merge" && errors.Is(msg.err, worktree.ErrRootDirty):
			pending := pendingWorktreeRecovery{kind: "dirty-root", merge: msg.merge}
			m.pendingWorktreeRecovery = &pending
			m.openWorktreeRecoveryPrompt(pending)
			return m, nil
		case msg.op == "merge" && errors.Is(msg.err, worktree.ErrMergeCleanupFailed):
			return m.showWorktreeContent("Worktree Promote", formatWorktreeMergeCleanupFailedMessage(msg.merge, msg.err))
		case msg.op == "promote" && errors.Is(msg.err, worktree.ErrRootDirty):
			return m.showWorktreeContent("Worktree Promote", formatWorktreePromoteDirtyRootMessage(msg.promote))
		case msg.op == "assist-merge" && errors.Is(msg.err, worktree.ErrRootDirty):
			return m.showWorktreeContent("Assisted Worktree Recovery", formatAssistedMergeRootDirtyMessage(msg.assist))
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
		return m.showWorktreeContent("Worktree Diff", msg.diff)
	case "merge":
		m.pendingWorktreeRecovery = nil
		if len(msg.cleanup.InUse) > 0 {
			pending := pendingWorktreeRecovery{kind: "remove-in-use", merge: msg.merge, bound: msg.bound, inUse: msg.cleanup.InUse}
			m.pendingWorktreeRecovery = &pending
			m.openWorktreeRecoveryPrompt(pending)
			return m, nil
		}
		if msg.cleanup.Removed {
			if msg.bound && msg.merge.RootDir != "" {
				if err := m.bindRootDir(msg.merge.RootDir); err != nil {
					return m.showFooterError(err.Error())
				}
			}
			return m.showWorktreeContent("Worktree Promote", formatWorktreeMergeSuccessMessage(msg.merge, msg.bound))
		}
		return m.showWorktreeContent("Worktree Promote", formatWorktreeMergeKeptMessage(msg.merge))
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
		return m.showWorktreeContent("Worktree Promote", formatWorktreePromoteSuccessMessage(msg.promote))
	case "remove":
		if msg.bound && msg.root != "" {
			if err := m.bindRootDir(msg.root); err != nil {
				return m.showFooterError(err.Error())
			}
		}
		if msg.merge.WorktreeDir != "" {
			return m.showWorktreeContent("Worktree Promote", formatWorktreeMergeSuccessMessage(msg.merge, msg.bound))
		}
		return m.showFooterSuccess("Removed worktree.")
	default:
		return m.showFooterMuted("Worktree operation finished.")
	}
}

func formatWorktreeMergeSuccessMessage(res worktree.MergeResult, rebound bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Promoted worktree %s → root checkout\n\n", worktreeDisplayName(res.WorktreeName))
	fmt.Fprintf(&b, "Source: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination: %s\n", res.RootDir)
	if res.SnapshotCommit != "" {
		fmt.Fprintf(&b, "Recovery snapshot: %s\n", shortSHA(res.SnapshotCommit))
	}
	if res.Committed {
		b.WriteString("Result: changes were committed on the root checkout.\n")
	} else if res.Applied {
		b.WriteString("Result: changes are staged and uncommitted on the root checkout.\n")
	} else {
		b.WriteString("Result: no worktree changes needed to be applied.\n")
	}
	b.WriteString("Cleanup: removed the source worktree.\n")
	if rebound {
		b.WriteString("Current session: rebound to the root checkout; `/shell` now opens root.\n")
	}
	appendLinesSection(&b, "Changed files", res.ChangedFiles, 20)
	b.WriteString("\nNext:\n")
	b.WriteString("  /shell\n")
	b.WriteString("  git status\n")
	if !res.Committed && res.Applied {
		b.WriteString("  git commit -m \"...\"\n")
	}
	return b.String()
}

func formatWorktreeMergeKeptMessage(res worktree.MergeResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Promoted worktree %s → root checkout\n\n", worktreeDisplayName(res.WorktreeName))
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
	b.WriteString("Cleanup: kept the source worktree because removal was declined.\n")
	b.WriteString("Current session: still bound to the source worktree. `/shell` still opens the worktree until you run `/worktree root`.\n")
	appendLinesSection(&b, "Changed files", res.ChangedFiles, 20)
	b.WriteString("\nNext:\n")
	b.WriteString("  /worktree root\n")
	b.WriteString("  /shell\n")
	b.WriteString("  git status\n")
	if !res.Committed && res.Applied {
		b.WriteString("  git commit -m \"...\"\n")
	}
	b.WriteString("  /worktree rm " + shellishName(res.WorktreeName, res.WorktreeDir) + " --force   # when you are done\n")
	return b.String()
}

func formatWorktreeMergeCleanupFailedMessage(res worktree.MergeResult, err error) string {
	var b strings.Builder
	b.WriteString("The promotion succeeded, but the source worktree could not be removed.\n\n")
	fmt.Fprintf(&b, "Root: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Source: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	if res.SnapshotCommit != "" {
		fmt.Fprintf(&b, "Recovery snapshot: %s\n", shortSHA(res.SnapshotCommit))
	}
	if res.Committed {
		b.WriteString("The promoted changes are committed on root.\n")
	} else {
		b.WriteString("The promoted changes are staged and uncommitted on root.\n")
	}
	fmt.Fprintf(&b, "Cleanup error: %v\n", err)
	b.WriteString("\nRetry cleanup with:\n  /worktree rm " + shellishName(res.WorktreeName, res.WorktreeDir) + " --force\n")
	return b.String()
}

func formatWorktreeMergeDirtyRootMessage(res worktree.MergeResult) string {
	var b strings.Builder
	b.WriteString("Promotion not attempted: the root checkout has uncommitted changes.\n\n")
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
	b.WriteString("  /worktree promote\n")
	appendMergeRecoveryPrompt(&b, res, "dirty root preflight refusal")
	return b.String()
}

func formatWorktreeMergeConflictMessage(res worktree.MergeResult) string {
	var b strings.Builder
	if res.ConflictReset {
		b.WriteString("Promotion conflict: the root checkout was reset cleanly.\n\n")
	} else {
		b.WriteString("Promotion conflict: the root checkout may still need cleanup.\n\n")
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
	b.WriteString("  A Yes/No prompt has opened in the TUI. Select Yes to resolve the promotion directly on the current root branch.\n")
	b.WriteString("  Select No to leave the root clean and keep working manually.\n")
	b.WriteString("\nNext options:\n")
	b.WriteString("  /worktree promote --branch   # continue from root on a branch based at the worktree HEAD\n")
	b.WriteString("  /worktree root && /shell     # inspect the clean root checkout\n")
	b.WriteString("  Ask the LLM to compare/rebase the worktree before retrying `/worktree promote`.\n")
	appendMergeRecoveryPrompt(&b, res, "promotion conflict after cherry-pick -n")
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
	b.WriteString("  /worktree promote --branch\n")
	return b.String()
}

func (m *Model) openWorktreeRecoveryPrompt(pending pendingWorktreeRecovery) {
	if m == nil || m.dialog == nil {
		return
	}
	title, question := worktreeRecoveryPromptText(pending)
	if pending.kind == "remove-in-use" {
		m.dialog.ShowWorktreeConfirmation(title, question, "Yes — remove it anyway", "No — keep the worktree")
	} else {
		m.dialog.ShowWorktreeRecovery(title, question)
	}
	m.scrollToBottom = true
}

func worktreeRecoveryPromptText(pending pendingWorktreeRecovery) (string, string) {
	res := pending.merge
	var details strings.Builder
	if res.WorktreeDir != "" {
		fmt.Fprintf(&details, "\n\nSource: %s\n", res.WorktreeDir)
	}
	if res.RootDir != "" {
		fmt.Fprintf(&details, "Root: %s\n", res.RootDir)
	}
	if pending.kind == "conflict" && len(res.Conflicts) > 0 {
		details.WriteString("Conflicts: ")
		details.WriteString(strings.Join(res.Conflicts[:min(8, len(res.Conflicts))], ", "))
		if len(res.Conflicts) > 8 {
			fmt.Fprintf(&details, " (+%d more)", len(res.Conflicts)-8)
		}
	}
	if pending.kind == "dirty-root" && strings.TrimSpace(res.RootStatus) != "" {
		lines := statusLinesForRecovery(res.RootStatus, 8)
		details.WriteString("Root status:\n")
		details.WriteString(strings.Join(lines, "\n"))
	}
	suffix := strings.TrimRight(details.String(), "\n")
	switch pending.kind {
	case "conflict":
		return "Assisted Worktree Recovery", "This worktree does not promote cleanly onto the current root branch. Would you like me to resolve it directly in the root checkout, leaving the result staged and uncommitted?" + suffix
	case "dirty-root":
		return "Assisted Worktree Recovery", "The root checkout is dirty. Would you like me to inspect the dirty root/worktree state and help sort it out before retrying?" + suffix
	case "remove-in-use":
		return "Remove Promoted Worktree?", fmt.Sprintf("The promotion succeeded, but this worktree is used by %d other session(s). Remove it anyway?%s", len(pending.inUse), suffix)
	default:
		return "Assisted Worktree Recovery", "Would you like assisted worktree promotion recovery?" + suffix
	}
}

func statusLinesForRecovery(status string, limit int) []string {
	lines := strings.Split(strings.TrimSpace(status), "\n")
	if len(lines) > limit {
		remaining := len(lines) - limit
		lines = append(lines[:limit], fmt.Sprintf("… and %d more", remaining))
	}
	return lines
}

func (m *Model) resolveWorktreeRecoveryPrompt(proceed bool) (tea.Model, tea.Cmd) {
	if m == nil {
		return m, nil
	}
	if m.pendingWorktreeRecovery == nil {
		if m.dialog != nil && m.dialog.IsOpen() && m.dialog.Type() == DialogWorktreeRecovery {
			m.dialog.Close()
		}
		return m.showFooterMuted("No pending worktree recovery.")
	}
	pending := *m.pendingWorktreeRecovery
	if proceed && pending.kind != "remove-in-use" {
		root := strings.TrimSpace(pending.merge.RootDir)
		if root == "" && pending.kind != "dirty-root" {
			return m.showFooterError("assisted promotion failed: missing root checkout")
		}
		if root != "" {
			if err := m.bindRootDir(root); err != nil {
				return m.showFooterError(err.Error())
			}
		}
	}
	if m.dialog != nil && m.dialog.IsOpen() && m.dialog.Type() == DialogWorktreeRecovery {
		m.dialog.Close()
	}
	m.pendingWorktreeRecovery = nil
	if proceed {
		return m.startPendingWorktreeRecovery(pending)
	}
	return m.showSystemMessage(formatWorktreeRecoveryDeclinedMessage(pending))
}

func formatWorktreeRecoveryDeclinedMessage(pending pendingWorktreeRecovery) string {
	if pending.kind == "remove-in-use" {
		return formatWorktreeMergeKeptMessage(pending.merge)
	}
	if pending.kind == "dirty-root" {
		return "Okay — leaving the root checkout unchanged. Clean/commit/stash root changes, then retry `/worktree promote` when ready."
	}
	return "Okay — leaving the root checkout clean. You can retry `/worktree promote`, use `/worktree promote --branch`, or ask for help later."
}

func (m *Model) startPendingWorktreeRecovery(pending pendingWorktreeRecovery) (tea.Model, tea.Cmd) {
	if pending.kind == "remove-in-use" {
		parentCtx := m.rootContext()
		m.worktreeOperation = "remove"
		return m.showSystemMessageWithCmd("Removing the promoted worktree…", func() tea.Msg {
			err := worktree.Remove(parentCtx, pending.merge.WorktreeDir, worktree.RemoveOptions{Force: true})
			return worktreeOperationDoneMsg{
				op:    "remove",
				dir:   pending.merge.WorktreeDir,
				root:  pending.merge.RootDir,
				bound: pending.bound,
				merge: pending.merge,
				err:   err,
			}
		})
	}
	if pending.kind == "dirty-root" {
		prompt := formatDirtyRootAssistedMergePrompt(pending.merge)
		return m.sendMessage(prompt)
	}
	parentCtx := m.rootContext()
	m.worktreeOperation = "assist-merge"
	message := fmt.Sprintf("Okay — switching to the root checkout and applying %s there. I will ask the LLM to resolve conflicts without committing or pushing.", worktreeDisplayName(pending.merge.WorktreeName))
	return m.showSystemMessageWithCmd(message, func() tea.Msg {
		res, err := worktree.StartAssistedMerge(parentCtx, pending.merge.WorktreeDir, worktree.AssistedMergeOptions{})
		return worktreeOperationDoneMsg{op: "assist-merge", dir: pending.merge.WorktreeDir, assist: res, err: err}
	})
}

func (m *Model) startAssistedMergeLLM(res worktree.AssistedMergeResult) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(res.RootDir) == "" {
		return m.showFooterError("assisted promotion failed: missing root checkout")
	}
	if len(res.ChangedFiles) == 0 {
		return m.showWorktreeContent("Assisted Worktree Recovery", formatAssistedMergeNothingToApplyMessage(res))
	}
	prompt := formatAssistedMergeLLMPrompt(res)
	return m.sendMessage(prompt)
}

func formatAssistedMergeRootDirtyMessage(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	b.WriteString("Assisted recovery could not start because the root checkout became dirty.\n\n")
	fmt.Fprintf(&b, "Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	appendStatusSection(&b, "Root status", res.RootStatus, 30)
	b.WriteString("\nThe root checkout was not changed. Clean/commit/stash root changes, then retry `/worktree promote`.\n")
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
	b.WriteString("The root checkout was not changed.\n")
	return b.String()
}

func formatAssistedMergeLLMPrompt(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	b.WriteString("The user confirmed interactive recovery for a failed `/worktree promote`. The worktree snapshot has been applied directly to the current root checkout branch.\n\n")
	b.WriteString("Goal:\n")
	b.WriteString("- Resolve/apply all source worktree changes on the current root checkout branch.\n")
	b.WriteString("- Leave the complete result staged and uncommitted in the root checkout.\n")
	b.WriteString("- Do not commit, push, switch branches, discard user changes, or remove the original worktree.\n\n")
	b.WriteString("State:\n")
	fmt.Fprintf(&b, "- Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "- Current root branch: %s\n", res.PreviousRootBranch)
	fmt.Fprintf(&b, "- Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
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
	b.WriteString("2. Resolve conflict markers or apply equivalent edits that preserve all source worktree intent on top of the current root branch.\n")
	b.WriteString("3. Stage resolved files with `git add` as appropriate.\n")
	b.WriteString("4. If a cherry-pick state remains after staging, run `git cherry-pick --quit` (not `--continue`) so the result stays uncommitted.\n")
	b.WriteString("5. Compare the final staged diff with the source worktree and ensure no source changes were omitted.\n")
	b.WriteString("6. Finish by running `git status --short` and summarizing what changed plus next commands: `git status`, `git commit -m \"...\"`, and `git push`.\n")
	return b.String()
}

func formatDirtyRootAssistedMergePrompt(res worktree.MergeResult) string {
	var b strings.Builder
	b.WriteString("The user confirmed interactive recovery for a `/worktree promote` that was blocked because the root checkout is dirty. You have permission to inspect and help sort this out safely.\n\n")
	b.WriteString("Goal:\n")
	b.WriteString("- Determine why the root checkout is dirty and recommend or perform safe steps to preserve those changes before retrying the worktree promotion.\n")
	b.WriteString("- Do not discard, overwrite, commit, push, or stash changes unless you clearly explain the action first and it is safe. If uncertain, ask the user.\n\n")
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", worktreeDisplayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination root: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Base SHA: %s\n", res.Base)
	fmt.Fprintf(&b, "Root HEAD: %s\n", res.RootHead)
	fmt.Fprintf(&b, "Worktree HEAD: %s\n", res.WorktreeHead)
	if strings.TrimSpace(res.RootStatus) != "" {
		fmt.Fprintf(&b, "Root status that blocked promotion:\n%s\n", res.RootStatus)
	}
	b.WriteString("\nSuggested first commands:\n")
	b.WriteString("- Use available shell/read/edit tools as needed; operate in the root checkout unless inspecting the source worktree.\n")
	b.WriteString("- `git status --short` in the root checkout\n")
	b.WriteString("- inspect relevant diffs before deciding whether to commit, stash, or ask the user\n")
	b.WriteString("- once root is clean, retry `/worktree promote` or use `/worktree promote --branch` if safer\n")
	return b.String()
}

func appendMergeRecoveryPrompt(b *strings.Builder, res worktree.MergeResult, reason string) {
	b.WriteString("\nLLM-assisted recovery prompt (send this if you want guidance):\n")
	b.WriteString("```text\n")
	b.WriteString("Help me choose a safe recovery strategy for a term-llm worktree promotion. Do not edit files or run commands until I confirm.\n")
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
	case "switch", "diff", "rm", "remove":
		return m.worktreeTargetCompletionItems(parts, trailingSpace, sub), true
	case "promote":
		return m.worktreePromoteCompletionItems(parts, trailingSpace), true
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

func (m *Model) worktreePromoteCompletionItems(parts []string, trailingSpace bool) []Command {
	prefix := strings.Join(parts[:2], " ")
	current := Command{Name: prefix, Description: "No current worktree; switch or browse first"}
	if dir := m.activeWorktreeDir(); dir != "" {
		current.Description = fmt.Sprintf("Current: %s · %s", filepath.Base(filepath.Clean(dir)), dir)
	}
	if len(parts) > 2 {
		arg := parts[2]
		if arg == "--branch" || !strings.HasPrefix(arg, "-") {
			return nil
		}
	}
	options := worktreeOptionCompletionItems(parts, trailingSpace, []worktreeOptionCompletion{
		{Name: "--branch", Description: "Promote on a new branch named after the current worktree"},
	})
	return append([]Command{current}, options...)
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
