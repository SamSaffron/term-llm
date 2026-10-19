package runtime

import (
	"sync"

	"github.com/samsaffron/term-llm/internal/tools"
)

const subagentProgressQueueSize = 128

type subagentProgressMsg struct {
	callID string
	event  tools.SubagentEvent
}

// SubagentProgressDispatcher decouples model event production from a UI sink,
// coalescing high-volume text while preserving boundary-event ordering.
type SubagentProgressDispatcher struct {
	send func(string, tools.SubagentEvent)

	mu          sync.Mutex
	cond        *sync.Cond
	queue       []subagentProgressMsg
	pendingText map[string]string
	maxQueue    int
}

// NewSubagentProgressDispatcher constructs and starts a dispatcher.
func NewSubagentProgressDispatcher(send func(string, tools.SubagentEvent)) *SubagentProgressDispatcher {
	d := NewSubagentProgressDispatcherForTest(send)
	go d.run()
	return d
}

// NewSubagentProgressDispatcherForTest constructs a dispatcher without its
// consumer goroutine so queue behavior can be tested deterministically.
func NewSubagentProgressDispatcherForTest(send func(string, tools.SubagentEvent)) *SubagentProgressDispatcher {
	d := &SubagentProgressDispatcher{
		send: send, queue: make([]subagentProgressMsg, 0, subagentProgressQueueSize),
		pendingText: make(map[string]string), maxQueue: subagentProgressQueueSize,
	}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// Callback enqueues one subagent event.
func (d *SubagentProgressDispatcher) Callback(callID string, event tools.SubagentEvent) {
	if d == nil || d.send == nil || callID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if event.Type == tools.SubagentEventText && len(d.queue) >= d.maxQueue {
		d.pendingText[callID] += event.Text
		d.cond.Signal()
		return
	}
	if event.Type != tools.SubagentEventText {
		if text := d.pendingText[callID]; text != "" {
			d.queue = append(d.queue, subagentProgressMsg{callID: callID, event: tools.SubagentEvent{Type: tools.SubagentEventText, Text: text}})
			delete(d.pendingText, callID)
		}
	}
	d.queue = append(d.queue, subagentProgressMsg{callID: callID, event: event})
	d.cond.Signal()
}

func (d *SubagentProgressDispatcher) run() {
	for {
		msg := d.next()
		d.send(msg.callID, msg.event)
	}
}

func (d *SubagentProgressDispatcher) next() subagentProgressMsg {
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
		return subagentProgressMsg{callID: callID, event: tools.SubagentEvent{Type: tools.SubagentEventText, Text: text}}
	}
	return subagentProgressMsg{}
}
