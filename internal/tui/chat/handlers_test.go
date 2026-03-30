package chat

import (
	"context"
	"fmt"
	"strings"
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

func stubClipboard(t *testing.T) {
	orig := readClipboardImage
	readClipboardImage = func() ([]byte, error) { return nil, nil }
	t.Cleanup(func() { readClipboardImage = orig })
}

func TestPasteCollapse_LargePasteBecomesInlinePlaceholder(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	// 100+ chars to trigger collapse
	pasteText := strings.Repeat("abcdefghij", 11) // 110 chars

	_, _ = m.handleKeyMsg(tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune(pasteText),
		Paste: true,
	})

	// Placeholder should be in the textarea, not the literal paste
	got := m.textarea.Value()
	if got == pasteText {
		t.Fatal("expected paste to be collapsed, but literal text appeared in textarea")
	}
	if !strings.Contains(got, "[Pasted text #1") {
		t.Fatalf("expected inline placeholder in textarea, got %q", got)
	}

	// Actual content stored in pasteChunks map
	if len(m.pasteChunks) != 1 {
		t.Fatalf("expected 1 paste chunk, got %d", len(m.pasteChunks))
	}
	if m.pasteChunks[1] != pasteText {
		t.Fatal("paste chunk content mismatch")
	}
}

func TestPasteCollapse_SmallPasteGoesToTextarea(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	// Under 100 chars — should pass through
	pasteText := "short paste that is under the hundred char threshold"

	_, _ = m.handleKeyMsg(tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune(pasteText),
		Paste: true,
	})

	if len(m.pasteChunks) != 0 {
		t.Fatalf("expected no collapsed paste for short text, got %d", len(m.pasteChunks))
	}
	if got := m.textarea.Value(); got != pasteText {
		t.Fatalf("expected literal paste in textarea, got %q", got)
	}
}

func TestPasteCollapse_MultiplePastesGetUniquePlaceholders(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	longPaste := strings.Repeat("x", 101)
	for i := 0; i < 3; i++ {
		_, _ = m.handleKeyMsg(tea.KeyMsg{
			Type:  tea.KeyRunes,
			Runes: []rune(longPaste),
			Paste: true,
		})
	}

	if len(m.pasteChunks) != 3 {
		t.Fatalf("expected 3 paste chunks, got %d", len(m.pasteChunks))
	}

	got := m.textarea.Value()
	for i := 1; i <= 3; i++ {
		placeholder := fmt.Sprintf("[Pasted text #%d", i)
		if !strings.Contains(got, placeholder) {
			t.Fatalf("expected %q in textarea, got %q", placeholder, got)
		}
	}
}

func TestPasteCollapse_ExpandPlaceholdersOnSend(t *testing.T) {
	m := newTestChatModel(false)
	content := strings.Repeat("y", 110)
	m.pasteChunks = map[int]string{
		1: content,
	}

	input := fmt.Sprintf("fix this: [Pasted text #1 +%d chars]", len(content))
	expanded := m.expandPastePlaceholders(input)

	expected := "fix this: " + content
	if expanded != expected {
		t.Fatalf("expected %q, got %q", expected, expanded)
	}
	if len(m.pasteChunks) != 0 {
		t.Fatal("expected pasteChunks cleared after expansion")
	}
}

func TestPasteCollapse_MultilinePlaceholderShowsLines(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	// Multi-line paste over 100 chars
	pasteText := "line one is here with some extra text to pad\nline two also has plenty of content in it\nline three as well with more words\nline four rounds it out nicely"

	_, _ = m.handleKeyMsg(tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune(pasteText),
		Paste: true,
	})

	got := m.textarea.Value()
	if !strings.Contains(got, "+4 lines]") {
		t.Fatalf("expected '+4 lines' in placeholder, got %q", got)
	}
}
