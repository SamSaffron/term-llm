package chat

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/ui"
)

type streamCommandBuffer struct {
	cmds      []tea.Cmd
	flushCmds []tea.Cmd
}

func (m *Model) handleStreamEvent(msg streamEventMsg) (tea.Model, tea.Cmd, bool, []tea.Cmd, []tea.Cmd) {
	state := &streamCommandBuffer{}
	streamEventStart := time.Now()
	ev := msg.event
	if msg.mainRunID != "" && (msg.mainRunID != m.mainRunID || msg.mainRunSubscription != m.mainRunSubscription) {
		return m, nil, true, nil, nil
	}
	if msg.mainRunSeq > 0 {
		m.mainRunLastSeq = msg.mainRunSeq
		for len(m.mainRunReplay) > 0 && m.mainRunReplay[0].Sequence <= msg.mainRunSeq {
			m.mainRunReplay = m.mainRunReplay[1:]
		}
	}
	if m.shouldIgnoreStreamEvent(msg) {
		return m, nil, true, nil, nil
	}
	if m.streamPerf != nil {
		m.streamPerf.tracef("stream_event type=%v", ev.Type)
	}

	switch ev.Type {
	case ui.StreamEventError:
		updated, command, immediate, commands, flushCommands := m.handleStreamError(msg, ev, state, streamEventStart)
		if immediate {
			return updated, command, true, commands, flushCommands
		}
		state.cmds, state.flushCmds = commands, flushCommands
	case ui.StreamEventDone:
		updated, command, immediate, commands, flushCommands := m.handleStreamDone(msg, ev, state)
		if immediate {
			return updated, command, true, commands, flushCommands
		}
		state.cmds, state.flushCmds = commands, flushCommands
	default:
		m.handleStreamProgress(msg, ev, state)
	}

	// Continue listening for more events unless we're done or got an error.
	// Keep draining the provider stream immediately even when text rendering is
	// frame-paced, so bursty deltas don't back up behind the bounded adapter channel.
	if ev.Type != ui.StreamEventDone && ev.Type != ui.StreamEventError {
		state.cmds = append(state.cmds, m.listenForStreamEvents())
	}
	if m.streamPerf != nil {
		m.streamPerf.RecordDuration(durationMetricStreamEvent, time.Since(streamEventStart))
	}
	return m, nil, false, state.cmds, state.flushCmds
}
