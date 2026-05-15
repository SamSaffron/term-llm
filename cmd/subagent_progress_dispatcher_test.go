package cmd

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestSubagentProgressDispatcherDoesNotBlockWhenSendStalls(t *testing.T) {
	enteredSend := make(chan struct{})
	releaseSend := make(chan struct{})
	dispatcher := newSubagentProgressDispatcher(func(string, tools.SubagentEvent) {
		select {
		case enteredSend <- struct{}{}:
		default:
		}
		<-releaseSend
	})
	dispatcher.mu.Lock()
	dispatcher.maxQueue = 1
	dispatcher.mu.Unlock()

	dispatcher.Callback("call-1", tools.SubagentEvent{Type: tools.SubagentEventText, Text: "first"})
	select {
	case <-enteredSend:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start sending first event")
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			dispatcher.Callback("call-1", tools.SubagentEvent{Type: tools.SubagentEventText, Text: "x"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callbacks blocked while UI send was stalled")
	}

	close(releaseSend)
}

func TestSubagentProgressDispatcherCoalescesTextBeforeBoundaryEvent(t *testing.T) {
	var got []tools.SubagentEvent
	dispatcher := newSubagentProgressDispatcherForTest(func(_ string, event tools.SubagentEvent) {
		got = append(got, event)
	})
	dispatcher.maxQueue = 1

	// Exercise the queuing logic synchronously to avoid racing the worker in this
	// ordering-focused test.
	dispatcher.mu.Lock()
	dispatcher.queue = append(dispatcher.queue, subagentProgressMsg{
		callID: "call-1",
		event:  tools.SubagentEvent{Type: tools.SubagentEventText, Text: "queued"},
	})
	dispatcher.mu.Unlock()

	dispatcher.Callback("call-1", tools.SubagentEvent{Type: tools.SubagentEventText, Text: "A"})
	dispatcher.Callback("call-1", tools.SubagentEvent{Type: tools.SubagentEventText, Text: "B"})
	dispatcher.Callback("call-1", tools.SubagentEvent{Type: tools.SubagentEventDone})

	first := dispatcher.next()
	second := dispatcher.next()
	third := dispatcher.next()

	if first.event.Text != "queued" {
		t.Fatalf("first text = %q, want queued", first.event.Text)
	}
	if second.event.Type != tools.SubagentEventText || second.event.Text != "AB" {
		t.Fatalf("second event = %#v, want coalesced text AB", second.event)
	}
	if third.event.Type != tools.SubagentEventDone {
		t.Fatalf("third event = %#v, want done", third.event)
	}

	_ = got
}
