package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const promptHistoryDBLookupTimeout = 150 * time.Millisecond

type promptHistoryDirection int

const (
	promptHistoryPrevious promptHistoryDirection = iota
	promptHistoryNext
)

type promptHistoryLookupMsg struct {
	seq                 uint64
	direction           promptHistoryDirection
	entry               *session.PromptHistoryEntry
	err                 error
	keepActiveOnMissing bool
}

func (m *Model) handlePromptHistoryKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	up := key.Matches(msg, m.keyMap.HistoryUp)
	down := key.Matches(msg, m.keyMap.HistoryDown)
	if !up && !down {
		return false, nil
	}

	if m.promptHistory.lookupPending {
		return true, nil
	}

	// Once a history entry is recalled, keep Up/Down bound to history traversal
	// regardless of the cursor position we set for editing convenience.
	if m.promptHistory.active && m.textarea.Value() == m.promptHistory.recalledText {
		if up {
			return m.recallPreviousPrompt()
		}
		return m.recallNextPrompt()
	}

	if up {
		if m.tryMoveTextareaUp() {
			m.resetPromptHistoryIfEdited()
			m.updateTextareaHeight()
			return true, nil
		}
		if m.selectFirstImageAttachment() {
			return true, nil
		}
		return m.recallPreviousPrompt()
	}

	if m.tryMoveTextareaDown() {
		m.resetPromptHistoryIfEdited()
		m.updateTextareaHeight()
		return true, nil
	}
	return m.recallNextPrompt()
}

func (m *Model) tryMoveTextareaUp() bool {
	line := m.textarea.Line()
	col := m.textarea.Column()
	y := m.textarea.ScrollYOffset()
	m.textarea.CursorUp()
	return m.textarea.Line() != line || m.textarea.Column() != col || m.textarea.ScrollYOffset() != y
}

func (m *Model) tryMoveTextareaDown() bool {
	line := m.textarea.Line()
	col := m.textarea.Column()
	y := m.textarea.ScrollYOffset()
	m.textarea.CursorDown()
	return m.textarea.Line() != line || m.textarea.Column() != col || m.textarea.ScrollYOffset() != y
}

func (m *Model) recallPreviousPrompt() (bool, tea.Cmd) {
	current := m.textarea.Value()
	if !m.promptHistory.active {
		m.promptHistory = promptHistoryState{
			active:      true,
			draftText:   current,
			draftFiles:  append([]FileAttachment(nil), m.files...),
			draftImages: append([]ImageAttachment(nil), m.images...),
			draftPastes: clonePasteChunks(m.pasteChunks),
		}
	} else if current != m.promptHistory.recalledText {
		m.resetPromptHistory()
		return false, nil
	}

	// Current session history always comes first. Traverse it from newest user
	// prompt to oldest before falling through to cross-session SQLite history.
	if m.promptHistory.memoryMode {
		if index, text, ok := m.previousMemoryPrompt(m.promptHistory.memoryIndex); ok {
			m.applyMemoryPromptHistoryEntry(index, text, true)
			return true, nil
		}
		return m.recallPreviousExternalPrompt(0, time.Time{}, true)
	}

	// Already in external history: keep going older there.
	if m.promptHistory.cursorID != 0 {
		return m.recallPreviousExternalPrompt(m.promptHistory.cursorID, m.promptHistory.cursorCreatedAt, true)
	}

	if index, text, ok := m.previousMemoryPrompt(len(m.messages)); ok {
		m.applyMemoryPromptHistoryEntry(index, text, true)
		return true, nil
	}
	return m.recallPreviousExternalPrompt(0, time.Time{}, false)
}

func (m *Model) recallPreviousExternalPrompt(beforeID int64, beforeCreatedAt time.Time, keepActiveOnMissing bool) (bool, tea.Cmd) {
	history, ok := m.store.(session.PromptHistoryOutsideSessionStore)
	if !ok || history == nil {
		if !keepActiveOnMissing {
			m.resetPromptHistory()
		}
		return true, nil
	}

	return true, m.promptHistoryExternalLookupCmd(history, promptHistoryPrevious, beforeID, beforeCreatedAt, keepActiveOnMissing)
}

func (m *Model) recallNextPrompt() (bool, tea.Cmd) {
	if !m.promptHistory.active {
		// Down at the bottom of an inactive composer is still a prompt-history key;
		// consume it so it doesn't unexpectedly scroll the transcript.
		return true, nil
	}
	if m.textarea.Value() != m.promptHistory.recalledText {
		m.resetPromptHistory()
		return false, nil
	}

	if m.promptHistory.memoryMode {
		if index, text, ok := m.nextMemoryPrompt(m.promptHistory.memoryIndex); ok {
			m.applyMemoryPromptHistoryEntry(index, text, false)
			return true, nil
		}
		m.restorePromptHistoryDraft(false)
		return true, nil
	}

	return m.recallNextExternalPrompt()
}

func (m *Model) recallNextExternalPrompt() (bool, tea.Cmd) {
	history, ok := m.store.(session.PromptHistoryOutsideSessionStore)
	if !ok || history == nil {
		m.restorePromptHistoryDraft(false)
		return true, nil
	}

	return true, m.promptHistoryExternalLookupCmd(history, promptHistoryNext, m.promptHistory.cursorID, m.promptHistory.cursorCreatedAt, true)
}

func (m *Model) promptHistoryExternalLookupCmd(history session.PromptHistoryOutsideSessionStore, direction promptHistoryDirection, cursorID int64, cursorCreatedAt time.Time, keepActiveOnMissing bool) tea.Cmd {
	m.promptHistoryLookupSeq++
	seq := m.promptHistoryLookupSeq
	m.promptHistory.lookupSeq = seq
	m.promptHistory.lookupPending = true
	excludeSessionID := m.currentSessionIDForPromptHistory()

	return func() tea.Msg {
		ctx, cancel := promptHistoryDBContext()
		defer cancel()

		var entry *session.PromptHistoryEntry
		var err error
		switch direction {
		case promptHistoryPrevious:
			entry, err = history.PreviousUserPromptOutsideSession(ctx, excludeSessionID, cursorID, cursorCreatedAt)
		case promptHistoryNext:
			entry, err = history.NextUserPromptOutsideSession(ctx, excludeSessionID, cursorID, cursorCreatedAt)
		default:
			err = fmt.Errorf("unknown prompt history direction %d", direction)
		}
		return promptHistoryLookupMsg{
			seq:                 seq,
			direction:           direction,
			entry:               entry,
			err:                 err,
			keepActiveOnMissing: keepActiveOnMissing,
		}
	}
}

func (m *Model) handlePromptHistoryLookupMsg(msg promptHistoryLookupMsg) (tea.Model, tea.Cmd) {
	if !m.promptHistory.active || m.promptHistory.lookupSeq != msg.seq {
		return m, nil
	}
	m.promptHistory.lookupPending = false

	if msg.err != nil {
		if errors.Is(msg.err, context.DeadlineExceeded) || errors.Is(msg.err, context.Canceled) {
			return m, nil
		}
		m.showPromptHistoryError(msg.err)
		return m, nil
	}

	switch msg.direction {
	case promptHistoryPrevious:
		if msg.entry == nil {
			if !msg.keepActiveOnMissing {
				m.resetPromptHistory()
			}
			return m, nil
		}
		m.applyExternalPromptHistoryEntry(msg.entry, true)
		return m, nil

	case promptHistoryNext:
		if msg.entry != nil {
			m.applyExternalPromptHistoryEntry(msg.entry, false)
			return m, nil
		}
		// We reached the newest external prompt. The combined history order is:
		// current-session newest..oldest, then external newest..oldest, so moving
		// Down from external newest lands on the oldest current-session prompt.
		if index, text, ok := m.oldestMemoryPrompt(); ok {
			m.applyMemoryPromptHistoryEntry(index, text, false)
			return m, nil
		}
		m.restorePromptHistoryDraft(false)
		return m, nil
	}

	return m, nil
}

func (m *Model) previousMemoryPrompt(beforeIndex int) (int, string, bool) {
	if beforeIndex > len(m.messages) {
		beforeIndex = len(m.messages)
	}
	for i := beforeIndex - 1; i >= 0; i-- {
		if text, ok := memoryPromptText(m.messages[i]); ok {
			return i, text, true
		}
	}
	return 0, "", false
}

func (m *Model) nextMemoryPrompt(afterIndex int) (int, string, bool) {
	for i := afterIndex + 1; i < len(m.messages); i++ {
		if text, ok := memoryPromptText(m.messages[i]); ok {
			return i, text, true
		}
	}
	return 0, "", false
}

func (m *Model) oldestMemoryPrompt() (int, string, bool) {
	for i := 0; i < len(m.messages); i++ {
		if text, ok := memoryPromptText(m.messages[i]); ok {
			return i, text, true
		}
	}
	return 0, "", false
}

func memoryPromptText(msg session.Message) (string, bool) {
	if msg.Role != llm.RoleUser || msg.CompactionTail {
		return "", false
	}
	text := strings.TrimSpace(msg.TextContent)
	if text == "" || llm.IsInternalCompactionSummaryText(text) {
		return "", false
	}
	return msg.TextContent, true
}

func promptHistoryDBContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), promptHistoryDBLookupTimeout)
}

func (m *Model) currentSessionIDForPromptHistory() string {
	if m.sess == nil {
		return ""
	}
	return m.sess.ID
}

func (m *Model) applyExternalPromptHistoryEntry(entry *session.PromptHistoryEntry, cursorAtEnd bool) {
	m.promptHistory.cursorID = entry.ID
	m.promptHistory.cursorCreatedAt = entry.CreatedAt
	m.promptHistory.memoryMode = false
	m.promptHistory.memoryIndex = 0
	m.applyPromptHistoryText(entry.Text, cursorAtEnd)
}

func (m *Model) applyMemoryPromptHistoryEntry(index int, text string, cursorAtEnd bool) {
	m.promptHistory.cursorID = 0
	m.promptHistory.cursorCreatedAt = time.Time{}
	m.promptHistory.memoryMode = true
	m.promptHistory.memoryIndex = index
	m.applyPromptHistoryText(text, cursorAtEnd)
}

func (m *Model) applyPromptHistoryText(text string, cursorAtEnd bool) {
	m.promptHistory.recalledText = text
	m.textarea.SetValue(text)
	if cursorAtEnd {
		m.textarea.MoveToEnd()
	} else {
		m.textarea.MoveToBegin()
	}
	m.updateTextareaHeight()

	// Prompt history rows are text-only. Keep the draft attachments out of the
	// recalled prompt so Enter does not accidentally send stale files/images.
	m.files = nil
	m.images = nil
	m.selectedImage = -1
	m.pasteChunks = nil
}

func (m *Model) restorePromptHistoryDraft(cursorAtEnd bool) {
	draftText := m.promptHistory.draftText
	draftFiles := append([]FileAttachment(nil), m.promptHistory.draftFiles...)
	draftImages := append([]ImageAttachment(nil), m.promptHistory.draftImages...)
	draftPastes := clonePasteChunks(m.promptHistory.draftPastes)
	m.resetPromptHistory()
	m.textarea.SetValue(draftText)
	if cursorAtEnd {
		m.textarea.MoveToEnd()
	} else {
		m.textarea.MoveToBegin()
	}
	m.updateTextareaHeight()
	m.files = draftFiles
	m.images = draftImages
	m.selectedImage = -1
	m.pasteChunks = draftPastes
}

func (m *Model) resetPromptHistoryIfEdited() {
	if m.promptHistory.active && m.textarea.Value() != m.promptHistory.recalledText {
		m.resetPromptHistory()
	}
}

func (m *Model) resetPromptHistory() {
	m.promptHistory = promptHistoryState{}
}

func (m *Model) showPromptHistoryError(err error) {
	m.phase = fmt.Sprintf("Prompt history error: %v", err)
}

func clonePasteChunks(in map[int]string) map[int]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
