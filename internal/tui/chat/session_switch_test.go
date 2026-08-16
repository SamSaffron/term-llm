package chat

import (
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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

func TestBeginSessionSwitchSwapsModelInProcessWithoutQuitting(t *testing.T) {
	m := newTestChatModel(false)
	m.width, m.height = 100, 30
	next := newTestChatModel(false)
	var got SessionSwitchRequest
	m.SetSessionSwitcher(func(request SessionSwitchRequest) (*Model, error) {
		got = request
		return next, nil
	})

	_, cmd := m.beginSessionSwitch(SessionSwitchRequest{SessionID: "child", BranchAutoSend: "first message"})
	if cmd == nil || m.quitting || m.RequestedResumeSessionID() != "" {
		t.Fatalf("switch used quit path: cmd=%v quitting=%v resume=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID())
	}
	if !m.sessionSwitchPending {
		t.Fatal("switch not marked pending")
	}

	switched, ok := searchSessionSwitchedMsg(cmd)
	if !ok || switched.err != nil || switched.model != next {
		t.Fatalf("switch command result: ok=%v err=%v model=%p", ok, switched.err, switched.model)
	}
	if got.SessionID != "child" || got.BranchAutoSend != "first message" {
		t.Fatalf("switcher request = %#v", got)
	}

	updated, swapCmd := m.Update(switched)
	if updated != next {
		t.Fatalf("update did not swap to the replacement model: %T", updated)
	}
	if swapCmd == nil {
		t.Fatal("swap produced no init/resize commands")
	}
	if m.sessionSwitchPending {
		t.Fatal("pending flag survived the swap")
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
	switched, ok := searchSessionSwitchedMsg(cmd)
	if !ok || switched.err == nil {
		t.Fatalf("expected failed switch message, got ok=%v err=%v", ok, switched.err)
	}

	updated, _ := m.Update(switched)
	if updated != m {
		t.Fatalf("failed switch replaced the model: %T", updated)
	}
	if m.sessionSwitchPending || m.quitting {
		t.Fatalf("failed switch left state pending=%v quitting=%v", m.sessionSwitchPending, m.quitting)
	}
	if m.mainRunUIDetach == nil {
		t.Fatal("failed switch did not restore the visible session UI sink")
	}
	m.DetachMainRunUISink()
}

func TestFinishConversationBranchUsesInProcessSwitch(t *testing.T) {
	m, store, _ := newBranchChatModel()
	next := newTestChatModel(false)
	var got SessionSwitchRequest
	m.SetSessionSwitcher(func(request SessionSwitchRequest) (*Model, error) {
		got = request
		return next, nil
	})
	notes := &BranchPathNotesRequest{ChildSessionID: "new-child", SourceSessionID: m.sess.ID}
	point := conversationBranchPoint{prefill: "edit this", autoSend: "run it"}

	updated, cmd := m.finishConversationBranch(point, store.result, notes)
	m = updated.(*Model)
	if cmd == nil || m.quitting || m.RequestedResumeSessionID() != "" {
		t.Fatalf("branch finish used quit path: cmd=%v quitting=%v resume=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID())
	}
	if store.currentID != "new-child" {
		t.Fatalf("store current = %q, want new-child", store.currentID)
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
