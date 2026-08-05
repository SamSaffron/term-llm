package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
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

func branchMessagePreview(message session.Message) string {
	text := strings.TrimSpace(message.TextContent)
	if text == "" {
		text = "(attachment or tool content)"
	}
	return session.TruncateSummary(text)
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

func (m *Model) cmdTree(args []string) (tea.Model, tea.Cmd) {
	if len(args) != 0 {
		m.setTextareaValue("")
		return m.showFooterError("Usage: /tree")
	}
	if m.transcriptMutationBusy() {
		return m.showFooterWarning("Cannot open the conversation tree while work is active.")
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
		marker := "○"
		if node.SessionID == m.sess.ID {
			marker = "●"
		}
		label := strings.Repeat("  ", depths[node.SessionID]) + marker + " " + node.Title
		description := fmt.Sprintf("#%d", node.SessionNumber)
		if node.AnchorPreview != "" {
			description += " · forked after " + node.AnchorPreview
		}
		items = append(items, DialogItem{
			ID: "path:" + node.SessionID, Label: label, Description: description, Category: "Existing paths",
		})
	}

	m.branchTreeChoices = make(map[string]pendingConversationBranch)
	previousContinuationID := int64(0)
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
			roleLabel = "edit user"
		case llm.RoleAssistant:
			if !visibleAssistantBranchRow(message) {
				continue
			}
			roleLabel = "branch after assistant"
		case llm.RoleTool:
			roleLabel = "branch after tool"
		default:
			continue
		}
		choiceID := fmt.Sprintf("fork:%d:%d", anchorID, message.ID)
		choice := pendingConversationBranch{
			sourceSessionID: m.sess.ID,
			anchorMessageID: anchorID,
			expected:        state,
			idempotencyKey:  session.NewID(),
			prefill:         prefill,
		}
		m.branchTreeChoices[choiceID] = choice
		items = append(items, DialogItem{
			ID: choiceID, Label: roleLabel + ": " + branchMessagePreview(message),
			Description: fmt.Sprintf("message %d", message.Sequence+1), Category: "Branch points",
		})
		if message.Role == llm.RoleUser || message.Role == llm.RoleAssistant {
			previousContinuationID = message.ID
		}
	}
	m.dialog.ShowBranchTree(items, "path:"+m.sess.ID)
	m.setTextareaValue("")
	return m, nil
}

func (m *Model) handleBranchTreeSelection(choiceID string) (tea.Model, tea.Cmd) {
	if strings.HasPrefix(choiceID, "path:") {
		return m.requestResumeSession(strings.TrimPrefix(choiceID, "path:"))
	}
	choice, ok := m.branchTreeChoices[choiceID]
	if !ok {
		return m.showFooterError("That branch point is no longer available.")
	}
	m.pendingBranch = &choice
	m.setTextareaValue(choice.prefill)
	m.textarea.Focus()
	return m.showFooterWarning("Branch pending. Edit or type a prompt, then press Enter. Conversation context rewinds; filesystem and tool side effects do not.")
}

func (m *Model) submitPendingBranch(prefill string) (tea.Model, tea.Cmd) {
	if m.pendingBranch == nil {
		return m, nil
	}
	if m.transcriptMutationBusy() {
		return m.showFooterWarning("Cannot create a branch while work is active.")
	}
	if len(m.files) > 0 || len(m.images) > 0 {
		return m.showFooterWarning("Create the branch before attaching files or images.")
	}
	branchStore, ok := m.store.(session.ConversationBranchStore)
	if !ok {
		return m.showFooterError("Session storage does not support conversation branching.")
	}
	pending := *m.pendingBranch
	result, err := branchStore.CreateBranch(context.Background(), pending.sourceSessionID, session.CreateBranchOptions{
		AnchorMessageID: pending.anchorMessageID,
		ExpectedState:   &pending.expected,
		IdempotencyKey:  pending.idempotencyKey,
	})
	switch {
	case errors.Is(err, session.ErrBranchConflict):
		m.pendingBranch = nil
		return m.showFooterWarning("Conversation changed in another client; reopen /tree and try again.")
	case err != nil:
		return m.showFooterError(fmt.Sprintf("Create branch: %v", err))
	case result.Session == nil:
		return m.showFooterError("Create branch: storage returned no child session.")
	}
	if err := m.store.SetCurrent(context.Background(), result.Session.ID); err != nil {
		return m.showFooterError(fmt.Sprintf("Select branch: %v", err))
	}
	m.pendingBranch = nil
	m.pendingResumeSessionID = result.Session.ID
	m.pendingBranchPrefill = prefill
	m.clearSideQuestionHistory()
	m.quitting = true
	return m, m.quitCmd()
}
