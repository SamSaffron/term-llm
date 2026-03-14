package chat

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
	"github.com/samsaffron/term-llm/internal/ui"
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

func TestResumeBrowserEnterRequestsRelaunch(t *testing.T) {
	sessionID := "sess-handler-resume-browser-1"
	store := &mockStore{
		summaries: []session.SessionSummary{{
			ID:        sessionID,
			Number:    11,
			Name:      "picked session",
			Summary:   "Discussed rollout checks and release notes",
			UpdatedAt: time.Now(),
		}},
	}

	m := newCmdTestModel(store)
	result, _ := m.cmdResume(nil)
	rm := result.(*Model)

	result, cmd := rm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	rm = result.(*Model)
	if cmd == nil {
		t.Fatal("expected enter to return a resume selection command")
	}

	msg := cmd()
	chatMsg, ok := msg.(sessionsui.ChatMsg)
	if !ok {
		t.Fatalf("expected sessions ChatMsg, got %T", msg)
	}
	if chatMsg.SessionID != sessionID {
		t.Fatalf("expected selected session ID %q, got %q", sessionID, chatMsg.SessionID)
	}

	result, quitCmd := rm.Update(chatMsg)
	rm = result.(*Model)
	if quitCmd == nil {
		t.Fatal("expected resume selection to request program quit")
	}
	if !rm.quitting {
		t.Fatal("expected selecting a browser session to quit for relaunch")
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
}

func TestResumeBrowserCloseReturnsToChatWithoutLosingDraft(t *testing.T) {
	sessionID := "sess-handler-resume-browser-close-1"
	store := &mockStore{
		summaries: []session.SessionSummary{{
			ID:        sessionID,
			Number:    12,
			Name:      "picked session",
			UpdatedAt: time.Now(),
		}},
	}

	m := newCmdTestModel(store)
	m.setTextareaValue("draft follow-up")
	result, _ := m.cmdResume(nil)
	rm := result.(*Model)

	result, cmd := rm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	rm = result.(*Model)
	if cmd == nil {
		t.Fatal("expected q to return a close command")
	}

	msg := cmd()
	if _, ok := msg.(sessionsui.CloseMsg); !ok {
		t.Fatalf("expected sessions CloseMsg, got %T", msg)
	}

	result, _ = rm.Update(msg)
	rm = result.(*Model)
	if rm.resumeBrowserMode {
		t.Fatal("expected resume browser mode to close")
	}
	if rm.quitting {
		t.Fatal("expected close to return to chat instead of quitting")
	}
	if got := rm.textarea.Value(); got != "draft follow-up" {
		t.Fatalf("expected draft input to be preserved, got %q", got)
	}
}

func TestHandleKeyMsg_StreamingCancelInterjectionRestoresComposerAndShowsStopping(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Running shell sleep"
	m.pendingInterjection = "old"
	m.setTextareaValue("stop sleeping")

	cancelCalls := 0
	m.streamCancelFunc = func() {
		cancelCalls++
	}

	_, _ = m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyEnter})

	if cancelCalls != 1 {
		t.Fatalf("expected stream cancel to be called once, got %d", cancelCalls)
	}
	if got := m.textarea.Value(); got != "stop sleeping" {
		t.Fatalf("expected textarea draft restored after cancel interjection, got %q", got)
	}
	if m.pendingInterjection != "" {
		t.Fatalf("expected pendingInterjection to be cleared, got %q", m.pendingInterjection)
	}
	if m.phase != "Stopping..." {
		t.Fatalf("expected stopping phase after cancel interjection, got %q", m.phase)
	}
	if got := m.interruptNotice; got == "" {
		t.Fatal("expected interrupt notice after cancellation")
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

func TestHandleKeyMsg_StreamingAsyncClassificationFeelsImmediate(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse("interject")
	m.setTextareaValue("also check the schema")

	_, cmd := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected async classification command")
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("expected textarea to clear immediately, got %q", got)
	}
	if got := m.pendingInterjection; got != "also check the schema" {
		t.Fatalf("expected pending interjection to render immediately, got %q", got)
	}
	if got := m.pendingInterruptUI; got != "deciding" {
		t.Fatalf("expected deciding state immediately, got %q", got)
	}

	msg := cmd()
	if _, ok := msg.(interruptClassifiedMsg); !ok {
		t.Fatalf("expected interruptClassifiedMsg, got %T", msg)
	}
	_, _ = m.handleInterruptClassified(msg.(interruptClassifiedMsg))

	if got := m.pendingInterruptUI; got != "interject" {
		t.Fatalf("expected interject state after classification, got %q", got)
	}
	if got := m.engine.DrainInterjection(); got != "also check the schema" {
		t.Fatalf("expected engine interjection to be queued, got %q", got)
	}
}

func TestHandleInterruptClassified_StreamAlreadyFinishedRestoresDraft(t *testing.T) {
	m := newTestChatModel(false)
	m.activeInterruptSeq = 7
	m.pendingInterjection = "keep sleeping"
	m.pendingInterruptUI = "deciding"

	_, cmd := m.handleInterruptClassified(interruptClassifiedMsg{
		RequestID: 7,
		Content:   "keep sleeping",
		Action:    llm.InterruptInterject,
	})
	if cmd != nil {
		t.Fatal("expected no follow-up command when restoring draft")
	}
	if got := m.textarea.Value(); got != "keep sleeping" {
		t.Fatalf("expected interjection text restored to composer, got %q", got)
	}
	if m.streaming {
		t.Fatal("expected stream to remain finished")
	}
	if got := m.pendingInterjection; got != "" {
		t.Fatalf("expected pending interjection cleared after restore, got %q", got)
	}
	if got := m.pendingInterruptUI; got != "" {
		t.Fatalf("expected pending interrupt UI cleared after restore, got %q", got)
	}
	if got := len(m.messages); got != 0 {
		t.Fatalf("expected restored draft not to auto-send, got %d messages", got)
	}
}

func TestStreamDone_PendingInterjectRestoresDraftWithoutEngineResidual(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.pendingInterjection = "keep sleeping"
	m.pendingInterruptUI = "interject"

	_, cmd := m.Update(streamEventMsg{event: ui.DoneEvent(0)})
	if cmd == nil {
		t.Fatal("expected command batch from stream completion")
	}
	if m.streaming {
		t.Fatal("expected streaming to stop after done event")
	}
	if got := m.textarea.Value(); got != "keep sleeping" {
		t.Fatalf("expected pending interjection restored to composer, got %q", got)
	}
	if got := m.pendingInterjection; got != "" {
		t.Fatalf("expected pending interjection cleared after restore, got %q", got)
	}
	if got := m.pendingInterruptUI; got != "" {
		t.Fatalf("expected pending interrupt UI cleared after restore, got %q", got)
	}
}

func TestStreamError_PendingInterjectRestoresDraftWithoutEngineResidual(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.pendingInterjection = "keep sleeping"
	m.pendingInterruptUI = "interject"

	_, cmd := m.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled)})
	if cmd != nil {
		t.Fatal("expected no follow-up command on error")
	}
	if m.streaming {
		t.Fatal("expected streaming to stop after error")
	}
	if got := m.textarea.Value(); got != "keep sleeping" {
		t.Fatalf("expected pending interjection restored to composer, got %q", got)
	}
	if got := m.pendingInterjection; got != "" {
		t.Fatalf("expected pending interjection cleared after restore, got %q", got)
	}
	if got := m.pendingInterruptUI; got != "" {
		t.Fatalf("expected pending interrupt UI cleared after restore, got %q", got)
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
