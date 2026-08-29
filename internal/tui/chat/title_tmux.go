package chat

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	tmuxWindowTitleMinInterval = time.Second
	tmuxCommandTimeout         = 750 * time.Millisecond
)

func init() {
	registerTerminalTitleProviderFactory(newTmuxTitleProviders)
}

func newTmuxTitleProviders(mode TerminalTitleMode, env TerminalTitleEnvironment) []terminalTitleProvider {
	if mode != TerminalTitleSmart || strings.TrimSpace(env.Get("TMUX")) == "" {
		return nil
	}
	return []terminalTitleProvider{newTmuxTitleProvider(env)}
}

type tmuxTitleProvider struct {
	pane                    string
	windowTarget            string
	originalAutomaticRename string
	originalWindowName      string
	windowRenameAttempted   bool
	inBandLastSent          string
	lastSent                string
	lastSentAt              time.Time
	debouncePending         bool
	resolvePending          bool
	renameInFlight          bool
	inFlightTitle           string
}

func newTmuxTitleProvider(env TerminalTitleEnvironment) *tmuxTitleProvider {
	return &tmuxTitleProvider{pane: strings.TrimSpace(env.Get("TMUX_PANE"))}
}

func (p *tmuxTitleProvider) UpdateCmd(snapshot terminalTitleSnapshot) tea.Cmd {
	if p == nil {
		return nil
	}
	var cmds []tea.Cmd
	if cmd := p.inBandTitleCmd(snapshot.Title); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := p.renameWindowCmd(snapshot.StableTitle); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return batchTerminalTitleCmds(cmds)
}

func (p *tmuxTitleProvider) inBandTitleCmd(title string) tea.Cmd {
	title = sanitizeTerminalTitle(title)
	if p == nil || title == "" || title == p.inBandLastSent {
		return nil
	}
	p.inBandLastSent = title
	return tea.Raw(tmuxWindowTitleSequence(title))
}

func (p *tmuxTitleProvider) renameWindowCmd(stableTitle string) tea.Cmd {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.windowTarget) == "" {
		return p.resolveTargetCmd()
	}
	title := sanitizeTerminalTitle(stableTitle)
	if title == "" || title == p.lastSent || (p.renameInFlight && title == p.inFlightTitle) {
		return nil
	}
	if p.renameInFlight {
		return nil
	}
	now := time.Now()
	if !p.lastSentAt.IsZero() {
		if remaining := tmuxWindowTitleMinInterval - now.Sub(p.lastSentAt); remaining > 0 {
			if p.debouncePending {
				return nil
			}
			p.debouncePending = true
			return tea.Tick(remaining, func(time.Time) tea.Msg {
				return tmuxTitleDebounceMsg{}
			})
		}
	}

	p.lastSentAt = now
	p.debouncePending = false
	p.renameInFlight = true
	p.windowRenameAttempted = true
	p.inFlightTitle = title
	target := strings.TrimSpace(p.windowTarget)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		return tmuxTitleRenameDoneMsg{title: title, err: tmuxRenameWindow(ctx, target, title)}
	}
}

func (p *tmuxTitleProvider) HandleMsg(msg tea.Msg, snapshot terminalTitleSnapshot) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case tmuxTitleDebounceMsg:
		p.debouncePending = false
		return true, p.UpdateCmd(snapshot)
	case tmuxTitleTargetResolvedMsg:
		p.resolvePending = false
		if msg.err != nil || strings.TrimSpace(msg.target) == "" {
			return true, nil
		}
		p.windowTarget = strings.TrimSpace(msg.target)
		p.originalAutomaticRename = strings.TrimSpace(msg.automaticRename)
		p.originalWindowName = strings.TrimSpace(msg.windowName)
		return true, p.UpdateCmd(snapshot)
	case tmuxTitleRenameDoneMsg:
		p.renameInFlight = false
		p.inFlightTitle = ""
		if msg.err == nil && strings.TrimSpace(msg.title) != "" {
			p.lastSent = strings.TrimSpace(msg.title)
			return true, p.UpdateCmd(snapshot)
		}
		// Failed tmux renames are best-effort; leave lastSent unchanged so a
		// future state change can retry the same title.
		return true, nil
	default:
		return false, nil
	}
}

func (p *tmuxTitleProvider) Restore() {
	if p == nil || strings.TrimSpace(p.windowTarget) == "" || !p.windowRenameAttempted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	target := strings.TrimSpace(p.windowTarget)
	if tmuxAutomaticRenameDisabled(p.originalAutomaticRename) && strings.TrimSpace(p.originalWindowName) != "" {
		_ = tmuxRenameWindow(ctx, target, p.originalWindowName)
	}
	if value := normalizeTmuxAutomaticRenameValue(p.originalAutomaticRename); value != "" {
		_ = tmuxSetAutomaticRename(ctx, target, value)
	}
}

func (p *tmuxTitleProvider) resolveTargetCmd() tea.Cmd {
	if p == nil || strings.TrimSpace(p.pane) == "" || strings.TrimSpace(p.windowTarget) != "" || p.resolvePending {
		return nil
	}
	p.resolvePending = true
	pane := strings.TrimSpace(p.pane)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		state, err := tmuxWindowStateForPane(ctx, pane)
		return tmuxTitleTargetResolvedMsg{target: state.target, automaticRename: state.automaticRename, windowName: state.windowName, err: err}
	}
}

type tmuxTitleDebounceMsg struct{}

type tmuxTitleTargetResolvedMsg struct {
	target          string
	automaticRename string
	windowName      string
	err             error
}

type tmuxTitleRenameDoneMsg struct {
	title string
	err   error
}

func tmuxWindowTitleSequence(title string) string {
	title = sanitizeTerminalTitle(title)
	if title == "" {
		return ""
	}
	return "\x1bk" + title + "\x1b\\"
}

func tmuxPassthroughSequence(seq string) string {
	if seq == "" {
		return ""
	}
	escaped := strings.ReplaceAll(seq, "\x1b", "\x1b\x1b")
	return "\x1bPtmux;" + escaped + "\x1b\\"
}

type tmuxWindowState struct {
	target          string
	automaticRename string
	windowName      string
}

var execCommandContext = exec.CommandContext

func tmuxWindowStateForPane(ctx context.Context, pane string) (tmuxWindowState, error) {
	target, err := tmuxWindowIDForPane(ctx, pane)
	if err != nil {
		return tmuxWindowState{}, err
	}
	state := tmuxWindowState{target: target}
	if automaticRename, err := tmuxWindowAutomaticRename(ctx, target); err == nil {
		state.automaticRename = automaticRename
	}
	if windowName, err := tmuxWindowName(ctx, target); err == nil {
		state.windowName = windowName
	}
	return state, nil
}

func tmuxWindowIDForPane(ctx context.Context, pane string) (string, error) {
	pane = strings.TrimSpace(pane)
	if pane == "" {
		return "", fmt.Errorf("tmux pane is empty")
	}
	out, err := execCommandContext(ctx, "tmux", "display-message", "-p", "-t", pane, "#{window_id}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func tmuxWindowAutomaticRename(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("tmux target is empty")
	}
	out, err := execCommandContext(ctx, "tmux", "show-window-options", "-qv", "-t", target, "automatic-rename").Output()
	if err != nil {
		return "", err
	}
	return normalizeTmuxAutomaticRenameValue(string(out)), nil
}

func tmuxWindowName(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("tmux target is empty")
	}
	out, err := execCommandContext(ctx, "tmux", "display-message", "-p", "-t", target, "#{window_name}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func tmuxRenameWindow(ctx context.Context, target, title string) error {
	target = strings.TrimSpace(target)
	title = sanitizeTerminalTitle(title)
	if target == "" || title == "" {
		return nil
	}
	return execCommandContext(ctx, "tmux", "rename-window", "-t", target, "--", title).Run()
}

func tmuxSetAutomaticRename(ctx context.Context, target, value string) error {
	target = strings.TrimSpace(target)
	value = normalizeTmuxAutomaticRenameValue(value)
	if target == "" || value == "" {
		return nil
	}
	return execCommandContext(ctx, "tmux", "set-window-option", "-t", target, "automatic-rename", value).Run()
}

func normalizeTmuxAutomaticRenameValue(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "on", "yes", "true":
		return "on"
	case "0", "off", "no", "false":
		return "off"
	default:
		return ""
	}
}

func tmuxAutomaticRenameDisabled(value string) bool {
	return normalizeTmuxAutomaticRenameValue(value) == "off"
}
