package ui

import tea "charm.land/bubbletea/v2"

// ComposeFlushFirstCommands builds a command pipeline where flush commands run
// first in-order, and all non-flush commands run afterward.
//
// This prevents output-printing commands from racing with follow-up commands
// when the caller needs deterministic boundary rendering.
func ComposeFlushFirstCommands(flushCmds []tea.Cmd, asyncCmds []tea.Cmd) tea.Cmd {
	flush := compactCommands(flushCmds)
	async := compactCommands(asyncCmds)

	switch {
	case len(flush) == 0 && len(async) == 0:
		return nil
	case len(flush) == 0:
		if len(async) == 1 {
			return async[0]
		}
		return tea.Batch(async...)
	case len(async) == 0:
		if len(flush) == 1 {
			return flush[0]
		}
		return tea.Sequence(flush...)
	default:
		seq := make([]tea.Cmd, 0, len(flush)+1)
		seq = append(seq, flush...)
		if len(async) == 1 {
			seq = append(seq, async[0])
		} else {
			seq = append(seq, tea.Batch(async...))
		}
		return tea.Sequence(seq...)
	}
}

func compactCommands(cmds []tea.Cmd) []tea.Cmd {
	if len(cmds) == 0 {
		return nil
	}
	compacted := make([]tea.Cmd, 0, len(cmds))
	for _, cmd := range cmds {
		if cmd != nil {
			compacted = append(compacted, cmd)
		}
	}
	if len(compacted) == 0 {
		return nil
	}
	return compacted
}
