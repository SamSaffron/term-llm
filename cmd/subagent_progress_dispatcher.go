package cmd

import (
	"sync"

	"github.com/samsaffron/term-llm/internal/tools"
)

const subagentProgressQueueSize = 128

type subagentProgressMsg struct {
	callID string
	event  tools.SubagentEvent
}

type subagentProgressDispatcher struct {
	send func(string, tools.SubagentEvent)

	mu          sync.Mutex
	cond        *sync.Cond
	queue       []subagentProgressMsg
	pendingText map[string]string
	maxQueue    int
}

func newSubagentProgressDispatcher(send func(string, tools.SubagentEvent)) *subagentProgressDispatcher {
	d := newSubagentProgressDispatcherForTest(send)
	go d.run()
	return d
}

func newSubagentProgressDispatcherForTest(send func(string, tools.SubagentEvent)) *subagentProgressDispatcher {
	d := &subagentProgressDispatcher{
		send:        send,
		queue:       make([]subagentProgressMsg, 0, subagentProgressQueueSize),
		pendingText: make(map[string]string),
		maxQueue:    subagentProgressQueueSize,
	}
	d.cond = sync.NewCond(&d.mu)
	return d
}

func (d *subagentProgressDispatcher) Callback(callID string, event tools.SubagentEvent) {
	if d == nil || d.send == nil || callID == "" {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if event.Type == tools.SubagentEventText && len(d.queue) >= d.maxQueue {
		// Text deltas are the only high-volume subagent event. When the UI falls
		// behind, coalesce text by call ID instead of blocking the subagent runner
		// or spawning one goroutine per token.
		d.pendingText[callID] += event.Text
		d.cond.Signal()
		return
	}

	// Preserve ordering for a call before reliable boundary events. If text was
	// coalesced while the queue was saturated, flush that pending text ahead of
	// tool/phase/usage/done events for the same call.
	if event.Type != tools.SubagentEventText {
		if text := d.pendingText[callID]; text != "" {
			d.queue = append(d.queue, subagentProgressMsg{
				callID: callID,
				event:  tools.SubagentEvent{Type: tools.SubagentEventText, Text: text},
			})
			delete(d.pendingText, callID)
		}
	}

	d.queue = append(d.queue, subagentProgressMsg{callID: callID, event: event})
	d.cond.Signal()
}

func (d *subagentProgressDispatcher) run() {
	for {
		msg := d.next()
		d.send(msg.callID, msg.event)
	}
}

func (d *subagentProgressDispatcher) next() subagentProgressMsg {
	d.mu.Lock()
	defer d.mu.Unlock()

	for len(d.queue) == 0 && len(d.pendingText) == 0 {
		d.cond.Wait()
	}

	if len(d.queue) > 0 {
		msg := d.queue[0]
		copy(d.queue, d.queue[1:])
		d.queue = d.queue[:len(d.queue)-1]
		return msg
	}

	for callID, text := range d.pendingText {
		delete(d.pendingText, callID)
		return subagentProgressMsg{
			callID: callID,
			event:  tools.SubagentEvent{Type: tools.SubagentEventText, Text: text},
		}
	}

	return subagentProgressMsg{}
}
