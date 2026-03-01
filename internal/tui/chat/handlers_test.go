package chat

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestHandleKeyMsg_SessionListEnterResumesSession(t *testing.T) {
	sessionID := "sess-handler-resume-1"
	sess := &session.Session{ID: sessionID, Number: 11, Name: "picked session"}
	store := &mockStore{
		sessions: map[string]*session.Session{sessionID: sess},
	}

	m := newCmdTestModel(store)
	m.dialog.ShowSessionList([]DialogItem{
		{ID: sessionID, Label: "picked session"},
	}, "")

	result, _ := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyEnter})
	rm := result.(*Model)

	if rm.dialog.IsOpen() {
		t.Fatal("expected dialog to close after selecting a session")
	}
	if !rm.quitting {
		t.Fatal("expected selecting a session to quit for relaunch")
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
}

func TestHandleKeyMsg_StreamingCancelInterjectionClearsComposerAndShowsStopping(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Running shell sleep"
	m.pendingInterjection = "old"
	m.queuedInterjection = "old"
	m.setTextareaValue("stop sleeping")

	cancelCalls := 0
	m.streamCancelFunc = func() {
		cancelCalls++
	}

	_, _ = m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyEnter})

	if cancelCalls != 1 {
		t.Fatalf("expected stream cancel to be called once, got %d", cancelCalls)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("expected textarea to be cleared after cancel interjection, got %q", got)
	}
	if m.pendingInterjection != "" {
		t.Fatalf("expected pendingInterjection to be cleared, got %q", m.pendingInterjection)
	}
	if m.queuedInterjection != "" {
		t.Fatalf("expected queuedInterjection to be cleared, got %q", m.queuedInterjection)
	}
	if m.phase != "Stopping..." {
		t.Fatalf("expected stopping phase after cancel interjection, got %q", m.phase)
	}
}

func TestHandleKeyMsg_StreamingEnterOnEmptyComposerShowsHint(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.setTextareaValue("   ")

	cancelCalls := 0
	m.streamCancelFunc = func() {
		cancelCalls++
	}

	_, _ = m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyEnter})

	if cancelCalls != 0 {
		t.Fatalf("expected empty enter to avoid cancellation, got %d cancel calls", cancelCalls)
	}
	if m.phase != "Type to interject, or press Esc to cancel" {
		t.Fatalf("expected empty enter hint phase, got %q", m.phase)
	}
}

func TestHandleKeyMsg_StreamingEscCancelsActiveStream(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	cancelCalls := 0
	m.streamCancelFunc = func() {
		cancelCalls++
	}

	_, _ = m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyEsc})

	if cancelCalls != 1 {
		t.Fatalf("expected esc to call stream cancel once, got %d", cancelCalls)
	}
	if m.streaming {
		t.Fatal("expected esc to end streaming mode immediately")
	}
}
