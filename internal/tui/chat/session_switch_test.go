package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

// searchSessionSwitchedMsg walks a command tree in order and returns the first
// sessionSwitchedMsg, short-circuiting so unrelated timing commands never run.
func searchSessionSwitchedMsg(cmd tea.Cmd) (sessionSwitchedMsg, bool) {
	if cmd == nil {
		return sessionSwitchedMsg{}, false
	}
	switch msg := cmd().(type) {
	case sessionSwitchedMsg:
		return msg, true
	case tea.BatchMsg:
		for _, nested := range msg {
			if got, ok := searchSessionSwitchedMsg(nested); ok {
				return got, true
			}
		}
	}
	return sessionSwitchedMsg{}, false
}

func TestBeginSessionSwitchWithoutSwitcherFallsBackToQuitRelaunch(t *testing.T) {
	m := newTestChatModel(false)
	notes := &BranchPathNotesRequest{ChildSessionID: "child", SourceSessionID: "source"}
	_, cmd := m.beginSessionSwitch(SessionSwitchRequest{
		SessionID:       "child",
		BranchPrefill:   "draft",
		BranchPathNotes: notes,
		BranchAutoSend:  "go",
	})
	if cmd == nil || !m.quitting {
		t.Fatalf("fallback did not quit: cmd=%v quitting=%v", cmd != nil, m.quitting)
	}
	if m.RequestedResumeSessionID() != "child" || m.RequestedBranchPrefill() != "draft" ||
		m.RequestedBranchAutoSend() != "go" || m.RequestedBranchPathNotes() == nil {
		t.Fatalf("fallback relaunch state: resume=%q prefill=%q auto=%q notes=%v",
			m.RequestedResumeSessionID(), m.RequestedBranchPrefill(), m.RequestedBranchAutoSend(), m.RequestedBranchPathNotes())
	}
}

func TestFallbackSessionSwitchPreservesLiveTransitionDraft(t *testing.T) {
	m := newTestChatModel(false)
	m.sessionTransition = m.newSessionTransition(SessionSwitchRequest{BranchPrefill: "initial"})
	m.setTextareaValue("edited while preparing")

	_, _ = m.beginSessionSwitch(SessionSwitchRequest{SessionID: "child", BranchPrefill: "initial"})
	if got := m.RequestedBranchPrefill(); got != "edited while preparing" {
		t.Fatalf("fallback transition draft = %q", got)
	}
}

func TestSessionTransitionCtrlCUsesNormalInterruptExit(t *testing.T) {
	m := newTestChatModel(false)
	m.keyMap = DefaultKeyMap()
	m.sessionTransition = m.newSessionTransition(SessionSwitchRequest{SessionID: "child"})

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(*Model)
	if m.ctrlCExitArmedUntil.IsZero() || m.quitting {
		t.Fatalf("first Ctrl+C did not arm exit: armed=%v quitting=%v", !m.ctrlCExitArmedUntil.IsZero(), m.quitting)
	}
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(*Model)
	if cmd == nil || !m.quitting {
		t.Fatalf("second Ctrl+C did not quit: cmd=%v quitting=%v", cmd != nil, m.quitting)
	}
}

func TestBeginSessionSwitchImmediatelyShowsTargetAndKeepsComposerEditable(t *testing.T) {
	m := newTestChatModel(false)
	m.messages = []session.Message{{TextContent: "old conversation must be hidden"}}
	m.SetSessionSwitcher(func(SessionSwitchRequest) (*Model, error) {
		return newTestChatModel(false), nil
	})

	_, cmd := m.beginSessionSwitch(SessionSwitchRequest{
		SessionID:     "child-session-1234",
		TargetLabel:   "Fresh thread",
		TargetNumber:  42,
		BranchPrefill: "initial draft",
	})
	if cmd == nil || m.sessionTransition == nil {
		t.Fatalf("switch did not enter transition immediately: cmd=%v transition=%v", cmd != nil, m.sessionTransition != nil)
	}
	activity := ui.StripANSI(m.renderSessionTransitionActivity())
	if !strings.Contains(activity, "Fresh thread · #42") || strings.Contains(activity, "old conversation") {
		t.Fatalf("transition activity = %q", activity)
	}
	m.scrollOffset = 1
	frame := ui.StripANSI(m.View().Content)
	if !strings.Contains(frame, "Fresh thread · #42") || strings.Contains(frame, "old conversation") {
		t.Fatalf("transition frame = %q", frame)
	}
	if got := m.textarea.Value(); got != "initial draft" {
		t.Fatalf("transition prefill = %q", got)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	m = updated.(*Model)
	if got := m.textarea.Value(); got != "initial draft!" {
		t.Fatalf("transition draft after edit = %q", got)
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if got := m.textarea.Value(); got != "initial draft!" {
		t.Fatalf("Enter changed pending draft = %q", got)
	}
	if !strings.Contains(m.footerMessage, "still preparing") {
		t.Fatalf("pending Enter footer = %q", m.footerMessage)
	}
}

func TestBeginSessionSwitchSwapsModelInProcessWithoutQuitting(t *testing.T) {
	m := newTestChatModel(false)
	m.width, m.height = 100, 30
	next := newTestChatModel(false)
	var got SessionSwitchRequest
	m.SetSessionSwitcher(func(request SessionSwitchRequest) (*Model, error) {
		got = request
		return next, nil
	})

	_, cmd := m.beginSessionSwitch(SessionSwitchRequest{SessionID: "child"})
	if cmd == nil || m.quitting || m.RequestedResumeSessionID() != "" {
		t.Fatalf("switch used quit path: cmd=%v quitting=%v resume=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID())
	}
	if !m.sessionSwitchPending {
		t.Fatal("switch not marked pending")
	}
	m.setTextareaValue("drafted while preparing")
	m.images = []ImageAttachment{{MediaType: "image/png", Data: []byte("image")}}
	m.pasteChunks = map[int]string{1: "large paste"}

	switched, ok := searchSessionSwitchedMsg(cmd)
	if !ok || switched.err != nil || switched.model != next {
		t.Fatalf("switch command result: ok=%v err=%v model=%p", ok, switched.err, switched.model)
	}
	if got.SessionID != "child" {
		t.Fatalf("switcher request = %#v", got)
	}

	updated, swapCmd := m.Update(switched)
	if updated != next {
		t.Fatalf("update did not swap to the replacement model: %T", updated)
	}
	if swapCmd == nil {
		t.Fatal("swap produced no init/resize commands")
	}
	if m.sessionSwitchPending || m.sessionTransition != nil {
		t.Fatalf("pending transition survived swap: pending=%v transition=%v", m.sessionSwitchPending, m.sessionTransition != nil)
	}
	if got := next.textarea.Value(); got != "drafted while preparing" {
		t.Fatalf("replacement draft = %q", got)
	}
	if len(next.images) != 1 || string(next.images[0].Data) != "image" || next.pasteChunks[1] != "large paste" {
		t.Fatalf("replacement composer payload: images=%#v pastes=%#v", next.images, next.pasteChunks)
	}
}

func TestSessionSwitchPreservesDraftBehindBranchAutoSend(t *testing.T) {
	m := newTestChatModel(false)
	next := newTestChatModel(false)
	next.branchAutoSend = "send this first"
	m.SetSessionSwitcher(func(SessionSwitchRequest) (*Model, error) { return next, nil })

	_, cmd := m.beginSessionSwitch(SessionSwitchRequest{SessionID: "child"})
	m.setTextareaValue("newer draft")
	switched, ok := searchSessionSwitchedMsg(cmd)
	if !ok {
		t.Fatal("missing switch completion")
	}
	updated, _ := m.Update(switched)
	if updated != next {
		t.Fatalf("switch returned %T", updated)
	}
	if got := next.textarea.Value(); got != "send this first" {
		t.Fatalf("auto-send composer = %q", got)
	}
	if next.transitionAutoSendDraft == nil || next.transitionAutoSendDraft.content != "newer draft" {
		t.Fatalf("preserved transition draft = %#v", next.transitionAutoSendDraft)
	}
}

func TestBeginSessionSwitchFailureKeepsCurrentModel(t *testing.T) {
	m := newTestChatModel(false)
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	m.SetMainRunManager(manager)
	m.SetProgram(tea.NewProgram(m))
	m.SetSessionSwitcher(func(SessionSwitchRequest) (*Model, error) {
		return nil, errors.New("boom")
	})
	_, cmd := m.beginSessionSwitch(SessionSwitchRequest{SessionID: "child"})
	m.setTextareaValue("draft survives failure")
	switched, ok := searchSessionSwitchedMsg(cmd)
	if !ok || switched.err == nil {
		t.Fatalf("expected failed switch message, got ok=%v err=%v", ok, switched.err)
	}

	updated, _ := m.Update(switched)
	if updated != m {
		t.Fatalf("failed switch replaced the model: %T", updated)
	}
	if m.sessionSwitchPending || m.sessionTransition != nil || m.quitting {
		t.Fatalf("failed switch left state pending=%v transition=%v quitting=%v", m.sessionSwitchPending, m.sessionTransition != nil, m.quitting)
	}
	if got := m.textarea.Value(); got != "draft survives failure" {
		t.Fatalf("failed switch draft = %q", got)
	}
	if m.mainRunUIDetach == nil {
		t.Fatal("failed switch did not restore the visible session UI sink")
	}
	m.DetachMainRunUISink()
}

func completedUnvisitedRun(t *testing.T, manager *MainRunManager, sessionID string) MainRunSnapshot {
	t.Helper()
	snapshot, err := manager.Start(sessionID, MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	<-snapshot.Done
	if !manager.Status(sessionID).Unvisited {
		t.Fatalf("session %q was not marked unvisited", sessionID)
	}
	return snapshot
}

func TestSuccessfulSessionSwitchClearsTargetCompletionAttention(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	completedUnvisitedRun(t, manager, "target")

	current := newTestChatModel(false)
	next := newTestChatModel(false)
	next.sess.ID = "target"
	next.SetMainRunManager(manager)
	updated, _ := current.handleSessionSwitched(sessionSwitchedMsg{model: next})
	if updated != next {
		t.Fatalf("successful switch returned %T", updated)
	}
	if manager.Status("target").Unvisited {
		t.Fatal("successful switch preserved target completion attention")
	}
}

func TestSuccessfulSessionSwitchToActiveRunDoesNotConsumeFutureAttention(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	release := make(chan struct{})
	snapshot, err := manager.Start("target", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error {
		<-release
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}

	current := newTestChatModel(false)
	current.SetProgram(tea.NewProgram(current))
	next := newTestChatModel(false)
	next.sess.ID = "target"
	next.SetMainRunManager(manager)
	updated, _ := current.handleSessionSwitched(sessionSwitchedMsg{model: next})
	if updated != next {
		t.Fatalf("successful switch returned %T", updated)
	}
	next.DetachMainRunUISink() // Leave before the active run produces its result.
	close(release)
	<-snapshot.Done
	if !manager.Status("target").Unvisited {
		t.Fatal("visiting an active run consumed its future completion")
	}
}

func TestFailedSessionSwitchPreservesTargetCompletionAttention(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	completedUnvisitedRun(t, manager, "target")

	current := newTestChatModel(false)
	current.SetMainRunManager(manager)
	updated, _ := current.handleSessionSwitched(sessionSwitchedMsg{err: errors.New("boom")})
	if updated != current {
		t.Fatalf("failed switch returned %T", updated)
	}
	if !manager.Status("target").Unvisited {
		t.Fatal("failed switch cleared target completion attention")
	}
}

func TestPreparedConversationBranchUsesInProcessSwitch(t *testing.T) {
	m, _, _ := newBranchChatModel()
	next := newTestChatModel(false)
	var got SessionSwitchRequest
	m.SetSessionSwitcher(func(request SessionSwitchRequest) (*Model, error) {
		got = request
		return next, nil
	})
	notes := &BranchPathNotesRequest{ChildSessionID: "new-child", SourceSessionID: m.sess.ID}
	point := conversationBranchPoint{prefill: "edit this", autoSend: "run it"}

	updated, cmd := m.handleConversationBranchCreated(conversationBranchCreatedMsg{point: point, result: session.BranchResult{
		Session: &session.Session{ID: "new-child", Provider: "mock", Model: "mock"},
	}, pathNotes: notes})
	m = updated.(*Model)
	if cmd == nil || m.quitting || m.RequestedResumeSessionID() != "" {
		t.Fatalf("branch finish used quit path: cmd=%v quitting=%v resume=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID())
	}
	switched, ok := searchSessionSwitchedMsg(cmd)
	if !ok || switched.model != next {
		t.Fatalf("branch switch result: ok=%v model=%p", ok, switched.model)
	}
	if got.SessionID != "new-child" || got.BranchPrefill != "edit this" ||
		got.BranchAutoSend != "run it" || got.BranchPathNotes != notes {
		t.Fatalf("branch switch request = %#v", got)
	}
}
