package chat

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type branchChatStore struct {
	*mockStore
	tree       session.BranchTree
	state      session.TranscriptMutationState
	result     session.BranchResult
	createOpts []session.CreateBranchOptions
	currentID  string
}

func (s *branchChatStore) GetBranchTree(context.Context, string) (session.BranchTree, error) {
	return s.tree, nil
}

func (s *branchChatStore) TranscriptMutationState(context.Context, string) (session.TranscriptMutationState, error) {
	return s.state, nil
}

func (s *branchChatStore) UndoLastUserTurn(context.Context, string, session.TranscriptMutationState) (session.TranscriptMutationResult, error) {
	return session.TranscriptMutationResult{}, nil
}

func (s *branchChatStore) RedoLastUserTurn(context.Context, string, session.TranscriptMutationState) (session.TranscriptMutationResult, error) {
	return session.TranscriptMutationResult{}, nil
}

func (s *branchChatStore) CreateBranch(_ context.Context, _ string, opts session.CreateBranchOptions) (session.BranchResult, error) {
	s.createOpts = append(s.createOpts, opts)
	return s.result, nil
}

func (s *branchChatStore) SetCurrent(_ context.Context, id string) error {
	s.currentID = id
	return nil
}

func newBranchChatModel() (*Model, *branchChatStore, []session.Message) {
	const sourceID = "tui-tree-source"
	messages := []session.Message{
		{ID: 11, SessionID: sourceID, Role: llm.RoleUser, TextContent: "first question", Sequence: 0},
		{ID: 12, SessionID: sourceID, Role: llm.RoleAssistant, TextContent: "first answer", Sequence: 1},
		{ID: 13, SessionID: sourceID, Role: llm.RoleUser, TextContent: "edit this question", Sequence: 2},
	}
	base := &mockStore{
		sessions: map[string]*session.Session{
			sourceID: {ID: sourceID, Number: 1, Provider: "mock", Model: "mock"},
		},
		messages: map[string][]session.Message{sourceID: messages},
	}
	store := &branchChatStore{
		mockStore: base,
		state:     session.TranscriptMutationState{Rev: 7, HeadID: 13},
		tree: session.BranchTree{
			RootSessionID: sourceID, ActiveSessionID: sourceID, PathCount: 2,
			Nodes: []session.BranchTreeNode{
				{SessionID: sourceID, SessionNumber: 1, Title: "Original"},
				{SessionID: "existing-child", SessionNumber: 2, ParentSessionID: sourceID, Title: "Existing path", ForkAfterMessageID: 12},
			},
		},
		result: session.BranchResult{Session: &session.Session{ID: "new-child", Provider: "mock", Model: "mock"}},
	}
	m := newCmdTestModel(store)
	m.sess = base.sessions[sourceID]
	m.messages = messages
	return m, store, messages
}

func TestTreeUserSelectionTargetsPreviousBoundaryAndRelaunchesWithPrefill(t *testing.T) {
	m, store, messages := newBranchChatModel()
	updated, cmd := m.cmdTree(nil)
	m = updated.(*Model)
	if cmd != nil || !m.dialog.IsOpen() || m.dialog.Type() != DialogBranchTree {
		t.Fatalf("tree dialog = open:%v type:%v cmd:%v", m.dialog.IsOpen(), m.dialog.Type(), cmd != nil)
	}
	var choiceID string
	for id, choice := range m.branchTreeChoices {
		if choice.prefill == "edit this question" {
			choiceID = id
			if choice.anchorMessageID != messages[1].ID || choice.expected.Rev != 7 {
				t.Fatalf("user choice = %#v", choice)
			}
		}
	}
	if choiceID == "" {
		t.Fatal("edit branch choice missing")
	}
	m.dialog.Close()
	updated, _ = m.handleBranchTreeSelection(choiceID)
	m = updated.(*Model)
	if m.pendingBranch == nil || m.textarea.Value() != "edit this question" {
		t.Fatalf("pending branch/composer = %#v / %q", m.pendingBranch, m.textarea.Value())
	}
	m.setTextareaValue("edited question")
	updated, cmd = m.submitPendingBranch("edited question")
	m = updated.(*Model)
	if cmd == nil || !m.quitting || m.RequestedResumeSessionID() != "new-child" || m.RequestedBranchPrefill() != "edited question" {
		t.Fatalf("relaunch state: cmd=%v quitting=%v resume=%q prefill=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID(), m.RequestedBranchPrefill())
	}
	if store.currentID != "new-child" || len(store.createOpts) != 1 || store.createOpts[0].AnchorMessageID != messages[1].ID {
		t.Fatalf("store branch call/current = %#v / %q", store.createOpts, store.currentID)
	}
}

func TestTreeHidesCompactionArtifactsAndUserAnchorSkipsToolRows(t *testing.T) {
	m, store, messages := newBranchChatModel()
	sourceID := m.sess.ID
	tool := session.Message{ID: 14, SessionID: sourceID, Role: llm.RoleTool, TextContent: "orphan tool result", Sequence: 3}
	summary := session.Message{ID: 15, SessionID: sourceID, Role: llm.RoleUser, TextContent: "[Context Compaction]\ninternal summary", Sequence: 4}
	tail := session.Message{ID: 16, SessionID: sourceID, Role: llm.RoleAssistant, TextContent: "duplicate retained answer", Sequence: 5, CompactionTail: true}
	user := session.Message{ID: 17, SessionID: sourceID, Role: llm.RoleUser, TextContent: "after tool", Sequence: 6}
	store.messages[sourceID] = append(store.messages[sourceID], tool, summary, tail, user)
	store.state = session.TranscriptMutationState{Rev: 8, HeadID: user.ID}

	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	var afterTool *pendingConversationBranch
	for _, choice := range m.branchTreeChoices {
		if choice.prefill == "after tool" {
			copy := choice
			afterTool = &copy
		}
		if choice.anchorMessageID == summary.ID || choice.anchorMessageID == tail.ID || choice.prefill == summary.TextContent {
			t.Fatalf("hidden compaction artifact became a branch choice: %#v", choice)
		}
	}
	if afterTool == nil {
		t.Fatal("post-tool user branch choice missing")
	}
	if afterTool.anchorMessageID != messages[2].ID {
		t.Fatalf("post-tool user anchor = %d, want previous visible user %d", afterTool.anchorMessageID, messages[2].ID)
	}
}

func TestTreeAssistantSelectionUsesAssistantAnchorAndExistingPathSwitches(t *testing.T) {
	m, _, messages := newBranchChatModel()
	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	var assistantChoice string
	for id, choice := range m.branchTreeChoices {
		if choice.anchorMessageID == messages[1].ID && choice.prefill == "" {
			assistantChoice = id
			break
		}
	}
	if assistantChoice == "" {
		t.Fatal("assistant branch choice missing")
	}
	updated, _ = m.handleBranchTreeSelection(assistantChoice)
	m = updated.(*Model)
	if m.pendingBranch == nil || m.textarea.Value() != "" {
		t.Fatalf("assistant selection = %#v / %q", m.pendingBranch, m.textarea.Value())
	}

	m2, _, _ := newBranchChatModel()
	updated, _ = m2.handleBranchTreeSelection("path:existing-child")
	m2 = updated.(*Model)
	if !m2.quitting || m2.RequestedResumeSessionID() != "existing-child" {
		t.Fatalf("existing path did not request resume: %#v", m2)
	}
}

func TestTreeBusyGateAndBranchPrefillDoesNotAutoSend(t *testing.T) {
	m, _, _ := newBranchChatModel()
	m.streaming = true
	updated, cmd := m.cmdTree(nil)
	m = updated.(*Model)
	if cmd == nil || m.dialog.IsOpen() {
		t.Fatalf("busy tree opened dialog or omitted warning command: open=%v cmd=%v", m.dialog.IsOpen(), cmd != nil)
	}

	m2 := newTestChatModel(false)
	m2.SetBranchPrefill("confirm this branch prompt")
	_ = m2.Init()
	if got := m2.textarea.Value(); got != "confirm this branch prompt" {
		t.Fatalf("branch prefill = %q", got)
	}
	if m2.handoverAutoSend != "" {
		t.Fatalf("branch prefill scheduled auto send: %q", m2.handoverAutoSend)
	}
}
