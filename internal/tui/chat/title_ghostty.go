package chat

import (
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const ghosttyProgressRefreshInterval = 5 * time.Second

func init() {
	registerTerminalTitleProviderFactory(newGhosttyProgressProviders)
}

func newGhosttyProgressProviders(mode TerminalTitleMode, env TerminalTitleEnvironment, progress bool) []terminalTitleProvider {
	if !progress || mode != TerminalTitleSmart || !isGhosttyTerminal(env) {
		return nil
	}
	return []terminalTitleProvider{newGhosttyProgressProvider(env)}
}

func isGhosttyTerminal(env TerminalTitleEnvironment) bool {
	if strings.EqualFold(strings.TrimSpace(env.Get("TERM_PROGRAM")), "ghostty") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(env.Get("TERM"))), "xterm-ghostty") {
		return true
	}
	return strings.TrimSpace(env.Get("GHOSTTY_RESOURCES_DIR")) != ""
}

type ghosttyProgressProvider struct {
	tmuxActive  bool
	active      bool
	tickPending bool
}

func newGhosttyProgressProvider(env TerminalTitleEnvironment) *ghosttyProgressProvider {
	return &ghosttyProgressProvider{tmuxActive: strings.TrimSpace(env.Get("TMUX")) != ""}
}

func (p *ghosttyProgressProvider) UpdateCmd(snapshot terminalTitleSnapshot) tea.Cmd {
	if p == nil {
		return nil
	}
	if !snapshot.InProgress {
		p.tickPending = false
		if !p.active {
			return nil
		}
		p.active = false
		return tea.Raw(p.wrapForTerminal(ghosttyProgressClearSequence()))
	}

	if p.active && p.tickPending {
		return nil
	}

	p.active = true
	p.tickPending = true
	return tea.Batch(
		tea.Raw(p.wrapForTerminal(ghosttyProgressIndeterminateSequence())),
		tea.Tick(ghosttyProgressRefreshInterval, func(time.Time) tea.Msg {
			return ghosttyProgressTickMsg{}
		}),
	)
}

func (p *ghosttyProgressProvider) HandleMsg(msg tea.Msg, snapshot terminalTitleSnapshot) (bool, tea.Cmd) {
	if _, ok := msg.(ghosttyProgressTickMsg); !ok {
		return false, nil
	}
	if p != nil {
		p.tickPending = false
	}
	return true, p.UpdateCmd(snapshot)
}

func (p *ghosttyProgressProvider) Restore() {
	if p == nil || !p.active {
		return
	}
	p.active = false
	_, _ = writeTerminalControlSequence(p.wrapForTerminal(ghosttyProgressClearSequence()))
}

func (p *ghosttyProgressProvider) wrapForTerminal(seq string) string {
	if p == nil || !p.tmuxActive || seq == "" {
		return seq
	}
	return tmuxPassthroughSequence(seq)
}

func writeTerminalControlSequence(seq string) (int, error) {
	if seq == "" {
		return 0, nil
	}
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		defer tty.Close()
		return tty.WriteString(seq)
	}
	return os.Stdout.WriteString(seq)
}

type ghosttyProgressTickMsg struct{}

func ghosttyProgressIndeterminateSequence() string {
	return "\x1b]9;4;3\x07"
}

func ghosttyProgressClearSequence() string {
	return "\x1b]9;4;0\x07"
}
