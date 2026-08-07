package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

type branchChatStore struct {
	*mockStore
	tree       session.BranchTree
	state      session.TranscriptMutationState
	result     session.BranchResult
	createErr  error
	createOpts []session.CreateBranchOptions
	currentID  string
	currentErr error
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
	return s.result, s.createErr
}

func (s *branchChatStore) SetCurrent(_ context.Context, id string) error {
	s.currentID = id
	return s.currentErr
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

func TestTreeUserSelectionAtTipOffersOnlyCleanAndRelaunchesWithPrefill(t *testing.T) {
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
	updated, cmd = m.handleBranchTreeSelection(choiceID)
	m = updated.(*Model)
	if cmd != nil || !m.dialog.IsOpen() || m.dialog.Type() != DialogBranchContext || len(m.dialog.filtered) != 1 || m.dialog.filtered[0].ID != "clean" {
		t.Fatalf("tip branch choices: cmd=%v open=%v type=%v items=%#v", cmd != nil, m.dialog.IsOpen(), m.dialog.Type(), m.dialog.filtered)
	}
	m.dialog.Close()
	updated, cmd = m.handleBranchContextSelection("clean")
	m = updated.(*Model)
	if cmd == nil || !m.quitting || m.RequestedResumeSessionID() != "new-child" || m.RequestedBranchPrefill() != "edit this question" {
		t.Fatalf("relaunch state: cmd=%v quitting=%v resume=%q prefill=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID(), m.RequestedBranchPrefill())
	}
	if store.currentID != "new-child" || len(store.createOpts) != 1 || store.createOpts[0].AnchorMessageID != messages[1].ID {
		t.Fatalf("store branch call/current = %#v / %q", store.createOpts, store.currentID)
	}
}

func TestFinishConversationBranchDoesNotCarryPathNotesWhenSelectionFails(t *testing.T) {
	m, store, _ := newBranchChatModel()
	store.currentErr = errors.New("database busy")
	request := &BranchPathNotesRequest{ChildSessionID: "new-child", SourceSessionID: m.sess.ID}

	updated, _ := m.finishConversationBranch(conversationBranchPoint{}, store.result, request)
	m = updated.(*Model)
	if m.RequestedBranchPathNotes() != nil || m.RequestedResumeSessionID() != "" {
		t.Fatalf("failed selection leaked relaunch state: notes=%#v resume=%q", m.RequestedBranchPathNotes(), m.RequestedResumeSessionID())
	}
}

func TestTreeUserSelectionAtTipOffersInheritedPathNoteContext(t *testing.T) {
	m, store, messages := newBranchChatModel()
	pathNote := session.NewPathNoteMessage(m.sess.ID, "- inherited parser finding", llm.PathNoteProvenance{SourceSessionID: "parent"}, 2)
	pathNote.ID = 14
	messages[2].Sequence = 3
	store.messages[m.sess.ID] = []session.Message{messages[0], messages[1], *pathNote, messages[2]}
	m.messages = store.messages[m.sess.ID]
	m.provider = llm.NewMockProvider("mock")

	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	var choiceID string
	for id, choice := range m.branchTreeChoices {
		if choice.prefill == "edit this question" {
			choiceID = id
			if choice.laterMessageCount != 1 {
				t.Fatalf("context count = %d, want inherited path note", choice.laterMessageCount)
			}
			break
		}
	}
	if choiceID == "" {
		t.Fatal("edit branch choice missing")
	}
	updated, _ = m.handleBranchTreeSelection(choiceID)
	m = updated.(*Model)
	if len(m.dialog.filtered) != 3 {
		t.Fatalf("branch context choices = %#v, want clean + useful + focused", m.dialog.filtered)
	}
	m.dialog.Close()
	updated, _ = m.handleBranchContextSelection("notes")
	m = updated.(*Model)
	request := m.RequestedBranchPathNotes()
	if request == nil {
		t.Fatal("path-note request missing")
	}
	var inherited bool
	for _, message := range request.SourceMessages {
		for _, part := range message.Parts {
			if part.Type == llm.PartPathNote && part.PathNote != nil && strings.Contains(llm.MessageText(message), "inherited parser finding") {
				inherited = true
			}
		}
	}
	if !inherited {
		t.Fatalf("inherited path note missing from helper request: %#v", request.SourceMessages)
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
	var afterTool *conversationBranchPoint
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

func TestTreeAssistantSelectionOffersContextChoiceAndExistingPathSwitches(t *testing.T) {
	m, store, messages := newBranchChatModel()
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
	updated, cmd := m.handleBranchTreeSelection(assistantChoice)
	m = updated.(*Model)
	if cmd != nil || !m.dialog.IsOpen() || m.dialog.Type() != DialogBranchContext {
		t.Fatalf("assistant selection did not offer context choice: cmd=%v open=%v type=%v", cmd != nil, m.dialog.IsOpen(), m.dialog.Type())
	}
	m.dialog.Close()
	updated, cmd = m.handleBranchContextSelection("clean")
	m = updated.(*Model)
	if cmd == nil || !m.quitting || m.RequestedResumeSessionID() != "new-child" || m.RequestedBranchPrefill() != "" {
		t.Fatalf("assistant selection did not immediately relaunch: quitting=%v resume=%q prefill=%q", m.quitting, m.RequestedResumeSessionID(), m.RequestedBranchPrefill())
	}
	if len(store.createOpts) != 1 || store.createOpts[0].AnchorMessageID != messages[1].ID {
		t.Fatalf("assistant branch call = %#v", store.createOpts)
	}

	m2, _, _ := newBranchChatModel()
	updated, _ = m2.handleBranchTreeSelection("path:existing-child")
	m2 = updated.(*Model)
	if !m2.quitting || m2.RequestedResumeSessionID() != "existing-child" {
		t.Fatalf("existing path did not request resume: %#v", m2)
	}
}

func TestTreeCurrentPathSelectionDoesNotRelaunch(t *testing.T) {
	m, store, _ := newBranchChatModel()
	updated, cmd := m.handleBranchTreeSelection("path:" + m.sess.ID)
	m = updated.(*Model)
	if cmd != nil || m.quitting || m.RequestedResumeSessionID() != "" || store.currentID != "" {
		t.Fatalf("current path selection relaunched: cmd=%v quitting=%v resume=%q current=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID(), store.currentID)
	}
}

func TestTreeSearchesMetadataAndShowsToolInvocation(t *testing.T) {
	m, store, _ := newBranchChatModel()
	sourceID := m.sess.ID
	store.messages[sourceID] = append(store.messages[sourceID],
		session.Message{
			ID: 14, SessionID: sourceID, Role: llm.RoleAssistant, Sequence: 3,
			Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{
				ID: "read-1", Name: "read_file", Arguments: []byte(`{"path":"cmd/agents.go"}`), ToolInfo: "(cmd/agents.go)",
			}}},
		},
		session.Message{
			ID: 15, SessionID: sourceID, Role: llm.RoleTool, Sequence: 4,
			Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{
				ID: "read-1", Name: "read_file", Content: "file contents",
			}}},
		},
	)

	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	rendered := m.dialog.View()
	plain := ui.StripANSI(rendered)
	theme := m.styles.Theme()
	styledSegments := []string{
		lipgloss.NewStyle().Bold(true).Foreground(theme.Primary).Background(theme.UserMsgBg).Render("user:"),
		lipgloss.NewStyle().Foreground(theme.Text).Background(theme.UserMsgBg).Render(" first question"),
		lipgloss.NewStyle().Bold(true).Foreground(theme.Secondary).Render("assistant:"),
		lipgloss.NewStyle().Foreground(theme.Text).Render(" first answer"),
	}
	for _, want := range styledSegments {
		if !strings.Contains(rendered, want) {
			t.Fatalf("tree view missing styled role/preview segment %q:\n%s", want, rendered)
		}
	}
	for _, want := range []string{"search:", "Existing paths", "#1", "Branch points", "tools hidden"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("tree view missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "branch after tool") {
		t.Fatalf("tool rows should be hidden by default:\n%s", plain)
	}

	for _, r := range "cmd/agents.go" {
		updated, _ = m.Update(tea.KeyPressMsg{Code: r})
		m = updated.(*Model)
	}
	if got := m.dialog.Query(); got != "cmd/agents.go" {
		t.Fatalf("tree query = %q", got)
	}
	selected := m.dialog.Selected()
	if selected == nil || !strings.Contains(selected.Label, "read_file") {
		t.Fatalf("filtered selection = %#v", selected)
	}
	plain = ui.StripANSI(m.dialog.View())
	for _, want := range []string{"branch after tool: read_file (cmd/agents.go)", "message 5", "tools shown"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("filtered tree view missing %q:\n%s", want, plain)
		}
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(*Model)
	if !m.dialog.IsOpen() || m.dialog.Query() != "" {
		t.Fatalf("first escape should clear search: open=%v query=%q", m.dialog.IsOpen(), m.dialog.Query())
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(*Model)
	if m.dialog.IsOpen() {
		t.Fatal("second escape should close tree")
	}
}

func TestTreeJumpsAcrossLongToolRunsAndTogglesToolRows(t *testing.T) {
	m, store, _ := newBranchChatModel()
	sourceID := m.sess.ID
	for i := range 100 {
		store.messages[sourceID] = append(store.messages[sourceID], session.Message{
			ID: int64(100 + i), SessionID: sourceID, Role: llm.RoleTool,
			TextContent: "tool output", Sequence: 3 + i,
		})
	}
	store.messages[sourceID] = append(store.messages[sourceID], session.Message{
		ID: 300, SessionID: sourceID, Role: llm.RoleUser,
		TextContent: "user after one hundred tools", Sequence: 103,
	})
	for i := range 5 {
		store.messages[sourceID] = append(store.messages[sourceID], session.Message{
			ID: int64(400 + i), SessionID: sourceID, Role: llm.RoleTool,
			TextContent: "later tool output", Sequence: 104 + i,
		})
	}
	store.state = session.TranscriptMutationState{Rev: 8, HeadID: 404}

	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	withoutTools := len(m.dialog.filtered)
	for _, item := range m.dialog.filtered {
		if item.TreeTool {
			t.Fatalf("tool row visible by default: %#v", item)
		}
	}

	for _, want := range []string{"first question", "edit this question"} {
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
		m = updated.(*Model)
		selected := m.dialog.Selected()
		if selected == nil || !strings.Contains(selected.Label, want) {
			t.Fatalf("next user turn = %#v, want %q", selected, want)
		}
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m = updated.(*Model)
	if got := len(m.dialog.filtered); got != withoutTools+100 {
		t.Fatalf("visible rows after right = %d, want %d", got, withoutTools+100)
	}
	if selected := m.dialog.Selected(); selected == nil || !strings.Contains(selected.Label, "edit this question") {
		t.Fatalf("right moved the selected user turn: %#v", selected)
	}
	if plain := ui.StripANSI(m.dialog.View()); !strings.Contains(plain, "▾ user: edit this question · 100 tools") {
		t.Fatalf("expanded turn missing disclosure indicator or tool count:\n%s", plain)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m = updated.(*Model)
	if got := len(m.dialog.filtered); got != withoutTools {
		t.Fatalf("visible rows after left = %d, want %d", got, withoutTools)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = updated.(*Model)
	if selected := m.dialog.Selected(); selected == nil || !strings.Contains(selected.Label, "user after one hundred tools") {
		t.Fatalf("next user turn after collapsed tools = %#v", selected)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	m = updated.(*Model)
	if got := len(m.dialog.filtered); got != withoutTools+105 {
		t.Fatalf("visible rows after ctrl+t = %d, want %d", got, withoutTools+105)
	}
	if selected := m.dialog.Selected(); selected == nil || !strings.Contains(selected.Label, "user after one hundred tools") {
		t.Fatalf("ctrl+t moved the selection: %#v", selected)
	}
}

func TestTreeRejectsAttachmentsBeforeOpening(t *testing.T) {
	m, _, _ := newBranchChatModel()
	m.files = []FileAttachment{{Path: "draft.txt", Name: "draft.txt"}}
	updated, cmd := m.cmdTree(nil)
	m = updated.(*Model)
	if cmd == nil || m.dialog.IsOpen() {
		t.Fatalf("tree with attachment: cmd=%v open=%v", cmd != nil, m.dialog.IsOpen())
	}
	if !strings.Contains(m.footerMessage, "before attaching") {
		t.Fatalf("attachment warning = %q", m.footerMessage)
	}
}

func TestTreeBranchConflictStaysInSourceSession(t *testing.T) {
	m, store, _ := newBranchChatModel()
	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	var choiceID string
	for id := range m.branchTreeChoices {
		choiceID = id
		break
	}
	store.createErr = session.ErrBranchConflict
	updated, _ = m.handleBranchTreeSelection(choiceID)
	m = updated.(*Model)
	m.dialog.Close()
	updated, cmd := m.handleBranchContextSelection("clean")
	m = updated.(*Model)
	if cmd == nil || m.quitting || m.RequestedResumeSessionID() != "" || store.currentID != "" {
		t.Fatalf("conflict changed sessions: cmd=%v quitting=%v resume=%q current=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID(), store.currentID)
	}
	if !strings.Contains(m.footerMessage, "changed in another client") {
		t.Fatalf("conflict warning = %q", m.footerMessage)
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
	const wantNotice = "Selected message restored as draft. Ctrl+U clears it."
	if got := m2.footerMessage; got != wantNotice {
		t.Fatalf("branch prefill footer = %q, want %q", got, wantNotice)
	}
	if m2.footerMessageTone != "muted" || m2.footerMessageSeq == 0 {
		t.Fatalf("branch prefill footer tone/sequence = %q/%d, want muted transient notice", m2.footerMessageTone, m2.footerMessageSeq)
	}
}

func TestTreeBringUsefulContextEntersChildBeforeGeneratingPathNotes(t *testing.T) {
	m, store, _ := newBranchChatModel()
	provider := llm.NewMockProvider("mock").AddTextResponse("- The abandoned attempt found a parser bug.")
	m.provider = provider
	m.modelName = "mock-model"
	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	var choiceID string
	for id, choice := range m.branchTreeChoices {
		if choice.anchorMessageID == 0 && choice.prefill == "first question" {
			choiceID = id
			break
		}
	}
	if choiceID == "" {
		t.Fatal("user retry branch choice missing")
	}
	updated, _ = m.handleBranchTreeSelection(choiceID)
	m = updated.(*Model)
	m.dialog.Close()
	updated, cmd := m.handleBranchContextSelection("notes")
	m = updated.(*Model)
	if cmd == nil || !m.quitting || m.RequestedResumeSessionID() != "new-child" {
		t.Fatalf("notes branch did not immediately relaunch: cmd=%v quitting=%v resume=%q", cmd != nil, m.quitting, m.RequestedResumeSessionID())
	}
	request := m.RequestedBranchPathNotes()
	if request == nil || request.SourceSessionID != "tui-tree-source" || len(request.SourceMessages) == 0 {
		t.Fatalf("relaunch path-note request = %#v", request)
	}
	if len(store.createOpts) != 1 || store.createOpts[0].PathNote != nil {
		t.Fatalf("initial branch should be clean and immediate: %#v", store.createOpts)
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("helper ran before child relaunch: %#v", provider.Requests)
	}

	child := newTestChatModel(false)
	child.store = store
	child.sess = store.result.Session
	child.provider = provider
	child.modelName = "mock-model"
	child.SetBranchPathNotes(request)
	worker := child.startPendingBranchPathNotes()
	if worker == nil || !child.branchContextInFlight() {
		t.Fatalf("child path-note worker did not start: worker=%v active=%v", worker != nil, child.branchContextInFlight())
	}
	if activity := ui.StripANSI(child.renderBranchPathNotesActivity()); !strings.Contains(activity, "○ Path notes from an earlier path") || strings.Contains(activity, ui.StripANSI(child.spinner.View())) {
		t.Fatalf("path-note activity = %q", activity)
	}
	workerMsg := worker()
	done, ok := workerMsg.(conversationBranchNotesDoneMsg)
	if !ok {
		t.Fatalf("worker message = %T", workerMsg)
	}
	updated, _ = child.handleConversationBranchNotesDone(done)
	child = updated.(*Model)
	if child.branchContextInFlight() {
		t.Fatal("path-note worker remained active after completion")
	}
	child.altScreen = true
	completed := ui.StripANSI(child.renderHistory())
	if !strings.Contains(completed, "● Path notes from an earlier path") || strings.Contains(completed, "○ Path notes from an earlier path") {
		t.Fatalf("completed path-note activity = %q", completed)
	}
	if len(store.added) == 0 {
		t.Fatal("path note was not persisted in child")
	}
	stored := store.added[len(store.added)-1]
	if _, ok := stored.PathNoteProvenance(); !ok || !strings.Contains(stored.PathNoteDisplayText(), "parser bug") {
		t.Fatalf("stored path note = %#v", stored)
	}
	if len(provider.Requests) != 1 || !provider.Requests[0].Ephemeral {
		t.Fatalf("helper requests = %#v", provider.Requests)
	}
}

func TestTreeQueuedSendWaitsForPathNoteThenStarts(t *testing.T) {
	_, store, _ := newBranchChatModel()
	provider := llm.NewMockProvider("mock").AddTextResponse("- Carry the parser finding.")
	child := newTestChatModel(false)
	child.store = store
	child.sess = store.result.Session
	child.provider = provider
	child.modelName = "mock-model"
	child.SetBranchPathNotes(&BranchPathNotesRequest{
		SourceSessionID: "tui-tree-source",
		AnchorMessageID: 0,
		SourceMessages:  []llm.Message{llm.UserText("abandoned parser attempt")},
	})
	worker := child.startPendingBranchPathNotes()
	if worker == nil {
		t.Fatal("path-note worker missing")
	}

	child.setTextareaValue("chart the new future")
	updated, _ := child.queueBranchMessage("chart the new future")
	child = updated.(*Model)
	if child.streaming || child.queuedBranchSend == nil || child.textarea.Value() != "" {
		t.Fatalf("early send was not queued: streaming=%v queued=%#v composer=%q", child.streaming, child.queuedBranchSend, child.textarea.Value())
	}
	activity := ui.StripANSI(child.renderBranchPathNotesActivity())
	if !strings.Contains(activity, "○ Path notes from an earlier path") || !strings.Contains(activity, "Message queued · chart the new future") {
		t.Fatalf("queued activity = %q", activity)
	}

	child.setTextareaValue("draft the following turn")
	done := worker().(conversationBranchNotesDoneMsg)
	updated, cmd := child.handleConversationBranchNotesDone(done)
	child = updated.(*Model)
	if cmd == nil || !child.streaming || child.queuedBranchSend != nil {
		t.Fatalf("queued send did not start after notes: cmd=%v streaming=%v queued=%#v", cmd != nil, child.streaming, child.queuedBranchSend)
	}
	if child.textarea.Value() != "draft the following turn" {
		t.Fatalf("newer draft was not preserved: %q", child.textarea.Value())
	}
	stored := store.messages[store.result.Session.ID]
	if len(stored) != 2 {
		t.Fatalf("child messages = %#v", stored)
	}
	if _, ok := stored[0].PathNoteProvenance(); !ok || stored[1].Role != llm.RoleUser || stored[1].TextContent != "chart the new future" {
		t.Fatalf("path note/user ordering = %#v", stored)
	}
}

func TestTreePathNoteFailureRestoresQueuedDraftWithoutSending(t *testing.T) {
	_, store, _ := newBranchChatModel()
	provider := llm.NewMockProvider("mock").AddError(errors.New("helper unavailable"))
	child := newTestChatModel(false)
	child.store = store
	child.sess = store.result.Session
	child.provider = provider
	child.modelName = "mock-model"
	child.SetBranchPathNotes(&BranchPathNotesRequest{
		SourceSessionID: "tui-tree-source",
		SourceMessages:  []llm.Message{llm.UserText("abandoned attempt")},
	})
	worker := child.startPendingBranchPathNotes()
	child.setTextareaValue("queued future")
	updated, _ := child.queueBranchMessage("queued future")
	child = updated.(*Model)
	child.setTextareaValue("newer draft")

	done := worker().(conversationBranchNotesDoneMsg)
	updated, _ = child.handleConversationBranchNotesDone(done)
	child = updated.(*Model)
	if child.streaming || child.queuedBranchSend != nil {
		t.Fatalf("failed helper sent queued message: streaming=%v queued=%#v", child.streaming, child.queuedBranchSend)
	}
	if got := child.textarea.Value(); !strings.Contains(got, "queued future") || !strings.Contains(got, "newer draft") {
		t.Fatalf("restored composer = %q", got)
	}
	if !strings.Contains(child.footerMessage, "helper unavailable") {
		t.Fatalf("failure footer = %q", child.footerMessage)
	}
}

func TestTreeKeyboardSelectionStartsUsefulContextWorker(t *testing.T) {
	m, _, _ := newBranchChatModel()
	m.provider = llm.NewMockProvider("mock").AddTextResponse("- carried finding")
	m.modelName = "mock-model"
	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	choiceID := ""
	for id, choice := range m.branchTreeChoices {
		if choice.anchorMessageID == 0 && choice.prefill == "first question" {
			choiceID = id
			break
		}
	}
	for i, item := range m.dialog.filtered {
		if item.ID == choiceID {
			m.dialog.SetCursor(i)
			break
		}
	}
	updated, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if cmd != nil || m.dialog.Type() != DialogBranchContext {
		t.Fatalf("enter branch point = cmd:%v dialog:%v", cmd != nil, m.dialog.Type())
	}
	updated, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(*Model)
	if selected := m.dialog.Selected(); selected == nil || selected.ID != "notes" {
		t.Fatalf("down selected %#v, want notes", selected)
	}
	updated, cmd = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if cmd == nil || !m.quitting || m.RequestedResumeSessionID() != "new-child" || m.RequestedBranchPathNotes() == nil {
		t.Fatalf("notes selection did not immediately enter child: cmd:%v quitting:%v resume:%q request:%#v", cmd != nil, m.quitting, m.RequestedResumeSessionID(), m.RequestedBranchPathNotes())
	}
}

func TestTreeLatestTipDoesNotOfferContextWithNothingToCarry(t *testing.T) {
	m, store, _ := newBranchChatModel()
	latest := session.Message{ID: 18, SessionID: m.sess.ID, Role: llm.RoleAssistant, TextContent: "latest answer", Parts: []llm.Part{{Type: llm.PartText, Text: "latest answer"}}, Sequence: 3}
	store.messages[m.sess.ID] = append(store.messages[m.sess.ID], latest)
	store.state = session.TranscriptMutationState{Rev: 8, HeadID: latest.ID}
	updated, _ := m.cmdTree(nil)
	m = updated.(*Model)
	choiceID := ""
	for id, choice := range m.branchTreeChoices {
		if choice.anchorMessageID == latest.ID {
			choiceID = id
			break
		}
	}
	if choiceID == "" {
		t.Fatal("latest assistant branch choice missing")
	}
	updated, _ = m.handleBranchTreeSelection(choiceID)
	m = updated.(*Model)
	if m.dialog.Type() != DialogBranchContext || len(m.dialog.filtered) != 1 || m.dialog.filtered[0].ID != "clean" {
		t.Fatalf("empty-suffix context choices = %#v", m.dialog.filtered)
	}
	m.dialog.Close()
	updated, cmd := m.handleBranchContextSelection("notes")
	m = updated.(*Model)
	if cmd == nil || m.transcriptMutationInFlight || !strings.Contains(m.footerMessage, "no later conversation") {
		t.Fatalf("empty-suffix notes = cmd:%v busy:%v footer:%q", cmd != nil, m.transcriptMutationInFlight, m.footerMessage)
	}
}
