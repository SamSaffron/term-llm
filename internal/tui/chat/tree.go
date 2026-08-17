package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	renderchat "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminaltext"
)

func branchTreeDepths(tree session.BranchTree) map[string]int {
	depths := make(map[string]int, len(tree.Nodes))
	for _, node := range tree.Nodes {
		depth := 0
		parent := node.ParentSessionID
		for parent != "" && depth < len(tree.Nodes) {
			depth++
			found := false
			for _, candidate := range tree.Nodes {
				if candidate.SessionID == parent {
					parent = candidate.ParentSessionID
					found = true
					break
				}
			}
			if !found {
				break
			}
		}
		depths[node.SessionID] = depth
	}
	return depths
}

func branchToolPreview(message session.Message, toolCalls map[string]*llm.ToolCall) string {
	for _, part := range message.Parts {
		if part.Type != llm.PartToolResult || part.ToolResult == nil {
			continue
		}
		result := part.ToolResult
		name := result.Name
		info := ""
		if call := toolCalls[result.ID]; call != nil {
			if name == "" {
				name = call.Name
			}
			info = call.ToolInfo
			if info == "" {
				info = llm.ExtractToolInfo(*call)
			}
		}
		preview := strings.TrimSpace(terminaltext.SanitizeSingleLine(strings.TrimSpace(name + " " + info)))
		if preview != "" {
			return session.TruncateSummary(preview)
		}
	}
	return ""
}

func branchMessagePreview(message session.Message, toolCalls map[string]*llm.ToolCall) string {
	if message.Role == llm.RoleTool {
		if preview := branchToolPreview(message, toolCalls); preview != "" {
			return preview
		}
	}
	text := strings.TrimSpace(message.TextContent)
	if text == "" {
		text = "(attachment or tool content)"
	}
	return session.TruncateSummary(terminaltext.SanitizeSingleLine(text))
}

func hiddenBranchTreeMessage(message session.Message) bool {
	if message.CompactionTail || llm.IsInternalCompactionSummaryText(message.TextContent) {
		return true
	}
	for _, part := range message.Parts {
		if part.Type == llm.PartText && llm.IsInternalCompactionSummaryText(part.Text) {
			return true
		}
	}
	return false
}

func branchContextSourceMessage(message session.Message) bool {
	if hiddenBranchTreeMessage(message) {
		return false
	}
	switch message.Role {
	case llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
		return true
	case llm.RoleDeveloper:
		_, ok := message.PathNoteProvenance()
		return ok
	default:
		return false
	}
}

func visibleAssistantBranchRow(message session.Message) bool {
	if strings.TrimSpace(message.TextContent) != "" {
		return true
	}
	for _, part := range message.Parts {
		if part.Type == llm.PartText && strings.TrimSpace(part.Text) != "" {
			return true
		}
	}
	return false
}

func lastSafeBranchMessageID(messages []session.Message) int64 {
	for i := len(messages) - 1; i >= 0; i-- {
		if session.IsBranchableMessage(messages[i]) {
			return messages[i].ID
		}
	}
	return 0
}

func messagesThroughBranchAnchor(messages []session.Message, anchorID int64) []session.Message {
	if anchorID <= 0 {
		return nil
	}
	for i := range messages {
		if messages[i].ID == anchorID {
			return append([]session.Message(nil), messages[:i+1]...)
		}
	}
	return nil
}

func (m *Model) branchShortcutState() ([]session.Message, session.TranscriptMutationState, bool, error) {
	if m.store == nil || m.sess == nil || strings.TrimSpace(m.sess.ID) == "" {
		return nil, session.TranscriptMutationState{}, false, errors.New("no stored conversation to branch")
	}
	if _, ok := m.store.(session.ConversationBranchStore); !ok {
		return nil, session.TranscriptMutationState{}, false, errors.New("session storage does not support conversation branching")
	}
	mutationStore, ok := m.store.(session.TranscriptUndoRedoStore)
	if !ok {
		return nil, session.TranscriptMutationState{}, false, errors.New("session storage does not support revision-safe branching")
	}
	messages, err := m.store.GetMessages(m.rootContext(), m.sess.ID, 0, 0)
	if err != nil {
		return nil, session.TranscriptMutationState{}, false, fmt.Errorf("load conversation: %w", err)
	}
	state, err := mutationStore.TranscriptMutationState(m.rootContext(), m.sess.ID)
	if err != nil {
		return nil, session.TranscriptMutationState{}, false, fmt.Errorf("load conversation revision: %w", err)
	}
	active := m.streaming || m.streamCancelFunc != nil || m.directShellRun != nil
	return messages, state, active, nil
}

func (m *Model) activeDurableBranchAnchor() (int64, bool) {
	if m.mainRunManager != nil {
		if boundary, active := m.mainRunManager.ActiveBoundary(m.SessionID()); active {
			return boundary.DurableAnchorID, boundary.Durable
		}
	}
	return m.activeBranchAnchorID, m.activeBranchAnchorID > 0
}

func (m *Model) cmdThread(rawMessage string) (tea.Model, tea.Cmd) {
	if len(m.files) > 0 || len(m.images) > 0 {
		return m.showFooterWarning("Start the thread before attaching files or images.")
	}
	messages, state, active, err := m.branchShortcutState()
	if err != nil {
		return m.showFooterError(err.Error())
	}
	if active && (m.mainRunManager == nil || !m.mainRunManager.HasActive(m.SessionID())) {
		return m.showFooterWarning("Cannot switch paths while work is active because background TUI runs are not available.")
	}
	contextMessages := messages
	if active {
		anchorID, available := m.activeDurableBranchAnchor()
		if !available {
			return m.showFooterWarning("The active run does not yet have a durable completed boundary to branch from.")
		}
		contextMessages = messagesThroughBranchAnchor(messages, anchorID)
	}
	sourceMessages, err := session.MessagesAfterBranchAnchor(contextMessages, 0)
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Load thread context: %v", err))
	}
	point := conversationBranchPoint{
		sourceSessionID:   m.sess.ID,
		anchorMessageID:   0,
		expected:          state,
		idempotencyKey:    session.NewID(),
		sourceMessages:    sourceMessages,
		laterMessageCount: len(sourceMessages),
		sourceRole:        llm.Role("conversation"),
		autoSend:          strings.TrimSpace(rawMessage),
		skipExpectedState: active,
	}
	m.pendingBranchPoint = &point
	m.dialog.ShowBranchContext(point.laterMessageCount, point.sourceRole, 0, "")
	m.setTextareaValue("")
	return m, nil
}

func (m *Model) cmdFork(rawMessage string) (tea.Model, tea.Cmd) {
	if len(m.files) > 0 || len(m.images) > 0 {
		return m.showFooterWarning("Create the fork before attaching files or images.")
	}
	messages, state, active, err := m.branchShortcutState()
	if err != nil {
		return m.showFooterError(err.Error())
	}
	if active && (m.mainRunManager == nil || !m.mainRunManager.HasActive(m.SessionID())) {
		return m.showFooterWarning("Cannot switch paths while work is active because background TUI runs are not available.")
	}
	anchorID := lastSafeBranchMessageID(messages)
	if active {
		var available bool
		anchorID, available = m.activeDurableBranchAnchor()
		if !available {
			return m.showFooterWarning("The active run does not yet have a durable completed boundary to fork from.")
		}
		if len(messagesThroughBranchAnchor(messages, anchorID)) == 0 {
			return m.showFooterWarning("The active branch boundary changed; retry after the transcript refreshes.")
		}
	}
	point := conversationBranchPoint{
		sourceSessionID:   m.sess.ID,
		anchorMessageID:   anchorID,
		expected:          state,
		idempotencyKey:    session.NewID(),
		autoSend:          strings.TrimSpace(rawMessage),
		skipExpectedState: active,
	}
	m.setTextareaValue("")
	return m.createConversationBranch(point)
}

func (m *Model) cmdTree(args []string) (tea.Model, tea.Cmd) {
	if len(args) != 0 {
		m.setTextareaValue("")
		return m.showFooterError("Usage: /tree")
	}
	if len(m.files) > 0 || len(m.images) > 0 {
		return m.showFooterWarning("Create the branch before attaching files or images.")
	}
	if m.store == nil || m.sess == nil || strings.TrimSpace(m.sess.ID) == "" {
		return m.showFooterError("No stored conversation to branch.")
	}
	branchStore, ok := m.store.(session.ConversationBranchStore)
	if !ok {
		return m.showFooterError("Session storage does not support conversation branching.")
	}
	ctx := context.Background()
	tree, err := branchStore.GetBranchTree(ctx, m.sess.ID)
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Load conversation tree: %v", err))
	}
	messages, err := m.store.GetMessages(ctx, m.sess.ID, 0, 0)
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Load branch points: %v", err))
	}
	active := m.streaming || m.streamCancelFunc != nil || m.directShellRun != nil
	if active && (m.mainRunManager == nil || !m.mainRunManager.HasActive(m.SessionID())) {
		return m.showFooterWarning("Cannot switch paths while work is active because background TUI runs are not available.")
	}
	if active {
		anchorID, available := m.activeDurableBranchAnchor()
		if !available {
			return m.showFooterWarning("The active run does not yet have a durable completed boundary to inspect.")
		}
		messages = messagesThroughBranchAnchor(messages, anchorID)
	}
	mutationStore, ok := m.store.(session.TranscriptUndoRedoStore)
	if !ok {
		return m.showFooterError("Session storage does not support revision-safe branching.")
	}
	state, err := mutationStore.TranscriptMutationState(ctx, m.sess.ID)
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Load conversation revision: %v", err))
	}

	items := make([]DialogItem, 0, len(tree.Nodes)+len(messages))
	depths := branchTreeDepths(tree)
	for _, node := range tree.Nodes {
		current := node.SessionID == m.sess.ID
		activeRun := m.mainRunManager != nil && m.mainRunManager.HasActive(node.SessionID)
		label := strings.Repeat("  ", depths[node.SessionID]) + node.Title
		description := fmt.Sprintf("#%d", node.SessionNumber)
		if current {
			description = "current · " + description
		}
		if node.AnchorPreview != "" {
			description += " · forked after " + node.AnchorPreview
		} else if node.ParentSessionID != "" && node.ForkAfterMessageID == 0 {
			description += " · new thread"
		}
		items = append(items, DialogItem{
			ID: "path:" + node.SessionID, Label: label, Description: description, Category: "Existing paths",
			TreePath: true, TreePathActive: activeRun,
		})
	}

	m.branchTreeChoices = make(map[string]conversationBranchPoint)
	toolCalls := make(map[string]*llm.ToolCall)
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.ID != "" {
				toolCalls[part.ToolCall.ID] = part.ToolCall
			}
		}
	}
	after := make(map[int64]int, len(messages))
	contextCount := 0
	for i := len(messages) - 1; i >= 0; i-- {
		after[messages[i].ID] = contextCount
		if branchContextSourceMessage(messages[i]) {
			contextCount++
		}
	}
	previousContinuationID := int64(0)
	currentUserTurnID := ""
	for _, message := range messages {
		if hiddenBranchTreeMessage(message) || message.Role == llm.RoleSystem || message.Role == llm.RoleDeveloper || message.Role == llm.RoleEvent {
			continue
		}
		anchorID := message.ID
		prefill := ""
		roleLabel := string(message.Role)
		switch message.Role {
		case llm.RoleUser:
			anchorID = previousContinuationID
			prefill = message.TextContent
			roleLabel = "user"
		case llm.RoleAssistant:
			if !visibleAssistantBranchRow(message) {
				continue
			}
			roleLabel = "assistant"
		case llm.RoleTool:
			roleLabel = "branch after tool"
		default:
			continue
		}
		choiceID := fmt.Sprintf("fork:%d:%d", anchorID, message.ID)
		if message.Role == llm.RoleUser {
			currentUserTurnID = choiceID
		}
		preview := branchMessagePreview(message, toolCalls)
		choice := conversationBranchPoint{
			sourceSessionID:     m.sess.ID,
			anchorMessageID:     anchorID,
			expected:            state,
			idempotencyKey:      session.NewID(),
			prefill:             prefill,
			sourceRole:          message.Role,
			sourceMessageNumber: message.Sequence + 1,
			sourcePreview:       preview,
			skipExpectedState:   active,
		}
		if active {
			sourceMessages, sourceErr := session.MessagesAfterBranchAnchor(messages, anchorID)
			if sourceErr != nil {
				continue
			}
			choice.sourceMessages = sourceMessages
		}
		// Editing a user turn rewinds to the previous continuation, so exclude the
		// selected prompt itself when deciding whether there is context to carry.
		choice.laterMessageCount = contextCount
		if anchorID > 0 {
			choice.laterMessageCount = after[anchorID]
		}
		if message.Role == llm.RoleUser && choice.laterMessageCount > 0 {
			choice.laterMessageCount--
		}
		m.branchTreeChoices[choiceID] = choice
		items = append(items, DialogItem{
			ID: choiceID, Label: roleLabel + ": " + preview,
			Description: fmt.Sprintf("message %d", message.Sequence+1), Category: "Branch points",
			TreeUserTurn: message.Role == llm.RoleUser, TreeTool: message.Role == llm.RoleTool,
			TreeTurnID: currentUserTurnID, TreeRole: message.Role, TreeRoleLabel: roleLabel, TreePreview: preview,
		})
		if message.Role == llm.RoleUser || message.Role == llm.RoleAssistant {
			previousContinuationID = message.ID
		}
	}
	m.dialog.ShowBranchTree(items, "path:"+m.sess.ID)
	m.setTextareaValue("")
	return m, nil
}

func (m *Model) refreshBranchTreeRunActivity() {
	if m.mainRunManager == nil || !m.dialog.IsOpen() || m.dialog.Type() != DialogBranchTree {
		return
	}
	changed := false
	for i := range m.dialog.items {
		item := &m.dialog.items[i]
		if !item.TreePath {
			continue
		}
		sessionID := strings.TrimPrefix(item.ID, "path:")
		active := m.mainRunManager.HasActive(sessionID)
		if item.TreePathActive != active {
			item.TreePathActive = active
			changed = true
		}
	}
	if changed {
		m.dialog.filterItems()
	}
}

func (m *Model) handleBranchTreeSelection(choiceID string) (tea.Model, tea.Cmd) {
	if strings.HasPrefix(choiceID, "path:") {
		sessionID := strings.TrimPrefix(choiceID, "path:")
		if m.sess != nil && sessionID == m.sess.ID {
			m.textarea.Focus()
			return m, nil
		}
		return m.requestResumeSession(sessionID)
	}
	choice, ok := m.branchTreeChoices[choiceID]
	if !ok {
		return m.showFooterError("That branch point is no longer available.")
	}
	m.pendingBranchPoint = &choice
	m.dialog.ShowBranchContext(choice.laterMessageCount, choice.sourceRole, choice.sourceMessageNumber, choice.sourcePreview)
	return m, nil
}

func (m *Model) handleBranchContextSelection(choice string) (tea.Model, tea.Cmd) {
	if m.pendingBranchPoint == nil {
		return m.showFooterError("That branch point is no longer available.")
	}
	switch choice {
	case "clean":
		point := *m.pendingBranchPoint
		m.pendingBranchPoint = nil
		return m.createConversationBranch(point)
	case "notes":
		return m.startConversationBranchWithNotes("")
	case "focused":
		m.branchFocusCapture = true
		m.setTextareaValue("")
		m.textarea.Focus()
		return m.showFooterMuted("What should the new path retain? Type your focus, then press Enter. Esc cancels.")
	default:
		return m, nil
	}
}

const (
	branchPathNotesLabel = "Path notes from an earlier path"
	branchContextStatus  = "Creating path notes"
)

func (m *Model) branchContextInFlight() bool {
	return m.branchPathNotesRequest != nil
}

type conversationBranchNotesDoneMsg struct {
	request BranchPathNotesRequest
	note    *session.Message
	usage   llm.Usage
	err     error
}

func (m *Model) startConversationBranchWithNotes(focus string) (tea.Model, tea.Cmd) {
	if m.pendingBranchPoint == nil {
		return m.showFooterError("That branch point is no longer available.")
	}
	point := *m.pendingBranchPoint
	if point.laterMessageCount == 0 {
		return m.showFooterWarning("There is no later conversation to bring from this point. Choose an earlier branch point or start clean.")
	}
	if m.transcriptMutationBusy() && !point.skipExpectedState {
		return m.showFooterWarning("Cannot create a branch while work is active.")
	}
	if m.provider == nil {
		return m.showFooterError("The current model is unavailable for preparing path context.")
	}
	branchStore, ok := m.store.(session.ConversationBranchStore)
	if !ok {
		return m.showFooterError("Session storage does not support conversation branching.")
	}
	if len(point.sourceMessages) == 0 {
		messages, err := m.store.GetMessages(m.rootContext(), point.sourceSessionID, 0, 0)
		if err != nil {
			return m.showFooterError(fmt.Sprintf("Load branch context: %v", err))
		}
		point.sourceMessages, err = session.MessagesAfterBranchAnchor(messages, point.anchorMessageID)
		if err != nil {
			return m.showFooterError(fmt.Sprintf("Load branch context: %v", err))
		}
	}

	// Materialize the inexpensive child first. Path-note generation starts only
	// after the TUI has relaunched into that child, so the user can begin drafting
	// immediately and the helper uses the child's fresh provider instance.
	branchOptions := session.CreateBranchOptions{
		AnchorMessageID: point.anchorMessageID,
		IdempotencyKey:  point.idempotencyKey,
	}
	if !point.skipExpectedState {
		branchOptions.ExpectedState = &point.expected
	}
	result, err := branchStore.CreateBranch(m.rootContext(), point.sourceSessionID, branchOptions)
	switch {
	case errors.Is(err, session.ErrBranchConflict):
		return m.showFooterWarning("Conversation changed in another client; reopen /tree and try again.")
	case err != nil:
		return m.showFooterError(fmt.Sprintf("Create branch: %v", err))
	case result.Session == nil:
		return m.showFooterError("Create branch: storage returned no child session.")
	}

	m.branchFocusCapture = false
	m.pendingBranchPoint = nil
	focus = strings.TrimSpace(focus)
	focusRunes := []rune(focus)
	if len(focusRunes) > 2_000 {
		focus = string(focusRunes[:2_000])
	}
	request := &BranchPathNotesRequest{
		ChildSessionID:  result.Session.ID,
		SourceSessionID: point.sourceSessionID,
		AnchorMessageID: point.anchorMessageID,
		SourceMessages:  append([]llm.Message(nil), point.sourceMessages...),
		Focus:           strings.TrimSpace(focus),
	}
	return m.finishConversationBranch(point, result, request)
}

func (m *Model) startPendingBranchPathNotes() tea.Cmd {
	if m.branchPathNotesRequest == nil || m.branchOperationCancel != nil || m.provider == nil || m.store == nil || m.sess == nil {
		return nil
	}
	if childID := strings.TrimSpace(m.branchPathNotesRequest.ChildSessionID); childID != "" && childID != m.sess.ID {
		m.branchPathNotesRequest = nil
		return nil
	}
	request := *m.branchPathNotesRequest
	request.SourceMessages = append([]llm.Message(nil), request.SourceMessages...)
	ctx, cancel := context.WithCancel(m.rootContext())
	m.branchOperationCancel = cancel
	m.branchOperationDone = make(chan struct{})
	m.branchOperationStarted = time.Now()
	done := m.branchOperationDone
	provider := m.provider
	model := m.modelName
	store := m.store
	childSessionID := m.sess.ID
	m.bumpContentVersion()

	return func() tea.Msg {
		defer close(done)
		notes, err := llm.GeneratePathNotes(ctx, provider, model, request.SourceMessages, llm.PathNotesConfig{Focus: request.Focus})
		msg := conversationBranchNotesDoneMsg{request: request, err: err}
		if notes != nil {
			msg.usage = notes.Usage
		}
		if err != nil {
			return msg
		}
		if notes == nil || strings.TrimSpace(notes.Notes) == "" {
			msg.err = errors.New("model returned no notes")
			return msg
		}
		provenance := llm.PathNoteProvenance{
			SourceSessionID: request.SourceSessionID,
			AnchorMessageID: request.AnchorMessageID,
			SourceMessages:  notes.SourceMessages,
			OmittedMessages: notes.OmittedMessages,
			ReadFiles:       notes.ReadFiles,
			ModifiedFiles:   notes.ModifiedFiles,
			Model:           notes.Model,
			Focus:           strings.TrimSpace(request.Focus),
		}
		note := session.NewPathNoteMessage(childSessionID, notes.Notes, provenance, -1)
		if err := store.AddMessage(ctx, childSessionID, note); err != nil {
			msg.err = fmt.Errorf("save path notes: %w", err)
			return msg
		}
		msg.note = note
		return msg
	}
}

func (m *Model) handleConversationBranchNotesDone(msg conversationBranchNotesDoneMsg) (tea.Model, tea.Cmd) {
	m.recordPathNoteUsage(context.Background(), msg.request.SourceSessionID, msg.usage)
	if m.branchOperationCancel != nil {
		m.branchOperationCancel()
		m.branchOperationCancel = nil
	}
	m.branchPathNotesRequest = nil
	m.branchOperationStarted = time.Time{}
	m.bumpContentVersion()

	if msg.err != nil {
		m.restoreQueuedBranchSend()
		switch {
		case errors.Is(msg.err, context.Canceled), errors.Is(msg.err, context.DeadlineExceeded):
			return m.showFooterMuted("Path-note creation cancelled; queued draft restored.")
		default:
			return m.showFooterError(fmt.Sprintf("Create path notes: %v. Queued draft restored.", msg.err))
		}
	}

	var completionCmd tea.Cmd
	if msg.note != nil {
		m.messages = append(m.messages, *msg.note)
		m.invalidateHistoryCache()
		completionCmd = m.printCompletedBranchPathNote(msg.note)
	}
	if m.queuedBranchSend != nil {
		updated, sendCmd := m.sendQueuedBranchMessage()
		return updated, tea.Batch(completionCmd, sendCmd)
	}
	updated, footerCmd := m.showFooterMuted("Path notes ready.")
	return updated, tea.Batch(completionCmd, footerCmd)
}

func (m *Model) printCompletedBranchPathNote(note *session.Message) tea.Cmd {
	if m.altScreen || note == nil {
		return nil
	}
	renderer := renderchat.NewMessageBlockRenderer(m.width, m.renderMd, m.toolsExpanded)
	completed := strings.TrimRight(renderer.Render(note).Rendered, "\n")
	if completed == "" {
		return nil
	}
	return tea.Println(completed)
}

func cloneBranchImages(images []ImageAttachment) []ImageAttachment {
	cloned := append([]ImageAttachment(nil), images...)
	for i := range cloned {
		cloned[i].Data = append([]byte(nil), cloned[i].Data...)
	}
	return cloned
}

func cloneBranchPasteChunks(chunks map[int]string) map[int]string {
	if len(chunks) == 0 {
		return nil
	}
	cloned := make(map[int]string, len(chunks))
	for key, value := range chunks {
		cloned[key] = value
	}
	return cloned
}

func (m *Model) queueBranchMessage(content string) (tea.Model, tea.Cmd) {
	if m.queuedBranchSend != nil {
		return m.showFooterWarning("A message is already queued until the path notes are ready.")
	}
	if _, err := m.agentMentionDelegationContext(content); err != nil {
		m.hideMentionPopup()
		return m.showFooterError(err.Error())
	}
	m.queuedBranchSend = &pendingBranchSend{
		content:       content,
		files:         append([]FileAttachment(nil), m.files...),
		images:        cloneBranchImages(m.images),
		selectedImage: m.selectedImage,
		pasteChunks:   cloneBranchPasteChunks(m.pasteChunks),
	}
	m.resetPromptHistory()
	m.setTextareaValue("")
	m.hideMentionPopup()
	m.files = nil
	m.images = nil
	m.selectedImage = -1
	m.pasteChunks = nil
	m.bumpContentVersion()
	return m.showFooterMuted("Message queued; it will send as soon as the path notes are ready.")
}

func (m *Model) sendQueuedBranchMessage() (tea.Model, tea.Cmd) {
	queued := m.queuedBranchSend
	m.queuedBranchSend = nil
	if queued == nil {
		return m, nil
	}

	// Preserve anything the user drafted after queuing the first message. The
	// normal send path owns attachment expansion and stream startup, so temporarily
	// swap in the queued attachment state and restore the newer draft afterwards.
	draftComposer := m.captureComposerSnapshot()
	draftFiles := m.files
	draftImages := m.images
	draftSelectedImage := m.selectedImage
	draftPastes := m.pasteChunks
	m.files = queued.files
	m.images = queued.images
	m.selectedImage = queued.selectedImage
	m.pasteChunks = queued.pasteChunks
	updated, cmd := m.sendMessage(queued.content)
	model := updated.(*Model)
	model.restoreComposerSnapshot(draftComposer)
	model.files = draftFiles
	model.images = draftImages
	model.selectedImage = draftSelectedImage
	model.pasteChunks = draftPastes
	model.updateTextareaHeight()
	return model, cmd
}

func (m *Model) restoreQueuedBranchSend() {
	queued := m.queuedBranchSend
	m.queuedBranchSend = nil
	if queued == nil {
		return
	}
	current := strings.TrimSpace(m.textarea.Value())
	text := queued.content
	if current != "" {
		text += "\n\n" + current
	}
	m.setTextareaValue(text)
	m.files = append(queued.files, m.files...)
	m.images = append(queued.images, m.images...)
	m.selectedImage = -1
	chunks := cloneBranchPasteChunks(queued.pasteChunks)
	if chunks == nil && len(m.pasteChunks) > 0 {
		chunks = make(map[int]string, len(m.pasteChunks))
	}
	for key, value := range m.pasteChunks {
		chunks[key] = value
	}
	m.pasteChunks = chunks
	m.updateTextareaHeight()
}

func (m *Model) createConversationBranch(point conversationBranchPoint) (tea.Model, tea.Cmd) {
	if m.transcriptMutationBusy() && !point.skipExpectedState {
		return m.showFooterWarning("Cannot create a branch while work is active.")
	}
	if len(m.files) > 0 || len(m.images) > 0 {
		return m.showFooterWarning("Create the branch before attaching files or images.")
	}
	branchStore, ok := m.store.(session.ConversationBranchStore)
	if !ok {
		return m.showFooterError("Session storage does not support conversation branching.")
	}
	branchOptions := session.CreateBranchOptions{
		AnchorMessageID: point.anchorMessageID,
		IdempotencyKey:  point.idempotencyKey,
	}
	if !point.skipExpectedState {
		branchOptions.ExpectedState = &point.expected
	}
	result, err := branchStore.CreateBranch(m.rootContext(), point.sourceSessionID, branchOptions)
	switch {
	case errors.Is(err, session.ErrBranchConflict):
		return m.showFooterWarning("Conversation changed in another client; reopen /tree and try again.")
	case err != nil:
		return m.showFooterError(fmt.Sprintf("Create branch: %v", err))
	case result.Session == nil:
		return m.showFooterError("Create branch: storage returned no child session.")
	}
	return m.finishConversationBranch(point, result, nil)
}

func (m *Model) finishConversationBranch(point conversationBranchPoint, result session.BranchResult, pathNotes *BranchPathNotesRequest) (tea.Model, tea.Cmd) {
	if result.Session == nil || strings.TrimSpace(result.Session.ID) == "" {
		return m.showFooterError("Create branch: storage returned no child session.")
	}
	if err := m.store.SetCurrent(m.rootContext(), result.Session.ID); err != nil {
		return m.showFooterError(fmt.Sprintf("Select branch: %v", err))
	}
	m.clearSideQuestionHistory()
	return m.beginSessionSwitch(SessionSwitchRequest{
		SessionID:       result.Session.ID,
		BranchPrefill:   point.prefill,
		BranchPathNotes: pathNotes,
		BranchAutoSend:  point.autoSend,
	})
}
