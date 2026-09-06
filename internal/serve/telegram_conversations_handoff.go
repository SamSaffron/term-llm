package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/samsaffron/term-llm/internal/session"
)

// snapshotConversations runs only after the receiver and actual work have
// stopped. Holding the manager lock keeps chat membership stable as it captures
// each exact conversation. No session is reset or retired during preparation.
func (m *telegramSessionMgr) snapshotConversations(ctx context.Context) ([]json.RawMessage, error) {
	if !m.restartGate.Drained() {
		return nil, fmt.Errorf("Telegram work has not settled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]int64, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		raw, err := m.snapshotConversation(ctx, id, m.sessions[id])
		if err != nil {
			return nil, err
		}
		result = append(result, raw)
	}
	return result, nil
}

// restoreConversations publishes all chats together or none. A bad later chat
// must not leave earlier chats visible, or leak their fresh runtimes. This is a
// startup-only operation; the caller must keep receiving closed throughout and
// authenticate the one-shot handoff before calling it.
func (m *telegramSessionMgr) restoreConversations(ctx context.Context, states []json.RawMessage) error {
	m.mu.Lock()
	occupied := len(m.sessions) != 0
	m.mu.Unlock()
	if occupied {
		return fmt.Errorf("Telegram conversations already initialized")
	}
	// Check duplicate chat/session identities before invoking any runtime factory.
	chats := make(map[int64]bool, len(states))
	sessions := make(map[string]bool, len(states))
	revisions := make(map[string]int64, len(states))
	for _, raw := range states {
		var state telegramConversationState
		if err := json.Unmarshal(raw, &state); err != nil {
			return err
		}
		if state.SessionID == "" || chats[state.ChatID] || sessions[state.SessionID] {
			return fmt.Errorf("duplicate or missing Telegram checkpoint identity")
		}
		chats[state.ChatID] = true
		sessions[state.SessionID] = true
		revisions[state.SessionID] = state.Revision
	}
	restored := make(map[int64]*telegramSession, len(states))
	published := false
	defer func() {
		if !published {
			for _, sess := range restored {
				closeTelegramSession(sess)
			}
		}
	}()
	for _, raw := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		sess, chatID, err := m.restoreConversation(ctx, raw)
		if err != nil {
			return err
		}
		restored[chatID] = sess
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(m.sessions) != 0 {
		return fmt.Errorf("Telegram conversations initialized during recovery")
	}
	// Factories can take time. Recheck earlier transcripts before publishing,
	// rather than assuming their pre-construction revision is still current.
	if len(revisions) > 0 {
		index, ok := m.store.(session.TranscriptIndexer)
		if !ok {
			return fmt.Errorf("Telegram store does not support transcript fencing")
		}
		for id, expected := range revisions {
			current, err := index.TranscriptRev(ctx, id)
			if err != nil {
				return err
			}
			if current != expected {
				return session.ErrExecHandoffConflict
			}
		}
	}
	m.sessions = restored
	published = true
	return nil
}
