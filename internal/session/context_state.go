package session

import (
	"context"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// HasCompactionBoundary reports whether a session has an explicit persisted
// compaction boundary. Checking the count (or a positive sequence from older
// persisted sessions) avoids treating a zero-value Session as compacted at
// sequence 0.
func HasCompactionBoundary(sess *Session) bool {
	return sess != nil && sess.CompactionSeq >= 0 && (sess.CompactionCount > 0 || sess.CompactionSeq > 0)
}

// LoadActiveMessages loads the messages that should be sent as active LLM
// context for a session. If the session has been compacted, older scrollback
// rows are intentionally skipped and callers receive only rows at/after the
// compaction boundary.
func LoadActiveMessages(ctx context.Context, store Store, sess *Session) ([]Message, error) {
	if store == nil || sess == nil {
		return nil, nil
	}
	if HasCompactionBoundary(sess) {
		messages, err := store.GetMessagesFrom(ctx, sess.ID, sess.CompactionSeq)
		if err != nil || len(messages) > 0 {
			return messages, err
		}
		// A stale boundary should not silently erase active context. Fall back to
		// the full history and best-effort clear the boundary so later appends do
		// not make the stale sequence become active again.
		clearCompactionBoundary(ctx, store, sess)
		return store.GetMessages(ctx, sess.ID, 0, 0)
	}
	return store.GetMessages(ctx, sess.ID, 0, 0)
}

func clearCompactionBoundary(ctx context.Context, store Store, sess *Session) {
	if store == nil || sess == nil || !HasCompactionBoundary(sess) {
		return
	}
	sess.CompactionSeq = -1
	sess.CompactionCount = 0
	if clearer, ok := store.(interface {
		ClearCompactionBoundary(context.Context, string) error
	}); ok {
		_ = clearer.ClearCompactionBoundary(ctx, sess.ID)
	}
}

// LoadScrollbackWithBoundary loads all persisted messages for display while
// also returning the index where active LLM context begins.
func LoadScrollbackWithBoundary(ctx context.Context, store Store, sess *Session) ([]Message, int, error) {
	if store == nil || sess == nil {
		return nil, 0, nil
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		return nil, 0, err
	}
	if !HasCompactionBoundary(sess) {
		return messages, 0, nil
	}
	for i, msg := range messages {
		if msg.Sequence >= sess.CompactionSeq {
			return messages, i, nil
		}
	}
	// If the persisted boundary is stale (for example after an older replace), do
	// not hide all history from the model/UI. Treat the full scrollback as active
	// and best-effort clear the persisted boundary.
	clearCompactionBoundary(ctx, store, sess)
	return messages, 0, nil
}

// ApplyCompaction persists a compaction result, appends the compacted rows to
// the caller's scrollback snapshot, returns the new active start index, refreshes
// session metadata when possible, and best-effort clears any persisted context
// estimate baseline so future turns don't seed context management with
// pre-compaction token counts. The full argument is the caller's scrollback
// snapshot and may be nil for non-UI owners that only need persistence.
func ApplyCompaction(ctx context.Context, store Store, sess *Session, full []Message, result *llm.CompactionResult) ([]Message, int, *Session, error) {
	activeStart := len(full)
	if result == nil {
		return full, activeStart, sess, nil
	}

	newSessionMsgs := make([]Message, 0, len(result.NewMessages))
	sessionID := ""
	if sess != nil {
		sessionID = sess.ID
	}
	for _, msg := range result.NewMessages {
		newSessionMsgs = append(newSessionMsgs, *NewMessage(sessionID, msg, -1))
	}

	refreshed := sess
	if store != nil && sess != nil {
		if err := store.CompactMessages(ctx, sess.ID, newSessionMsgs); err != nil {
			return full, activeStart, refreshed, err
		}
		// Once CompactMessages succeeds, the persistence boundary has moved. Treat
		// the context-estimate reset and metadata refresh as best-effort so callers
		// still update their active in-memory context and don't diverge from the DB.
		bestEffortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = ResetContextEstimate(bestEffortCtx, store, sess)
		if got, err := store.Get(bestEffortCtx, sess.ID); err == nil && got != nil {
			refreshed = got
		}
	}

	updated := append(append([]Message(nil), full...), newSessionMsgs...)
	return updated, activeStart, refreshed, nil
}

// LLMActiveMessages converts the active slice of session messages into LLM
// messages, injecting systemPrompt only when the active context doesn't already
// include a system message, then applies provider-safe conversation filtering.
func LLMActiveMessages(messages []Message, activeStart int, systemPrompt string) []llm.Message {
	if activeStart > 0 {
		if activeStart < len(messages) {
			messages = messages[activeStart:]
		} else {
			messages = nil
		}
	}

	out := make([]llm.Message, 0, len(messages)+1)
	hasSystem := len(messages) > 0 && messages[0].Role == llm.RoleSystem
	if strings.TrimSpace(systemPrompt) != "" && !hasSystem {
		out = append(out, llm.SystemText(systemPrompt))
	}
	for i := range messages {
		out = append(out, messages[i].ToLLMMessage())
	}
	return llm.FilterConversationMessages(out)
}

// ResetContextEstimate clears the persisted context estimate baseline for the
// session after compaction.
func ResetContextEstimate(ctx context.Context, store Store, sess *Session) error {
	if store == nil || sess == nil {
		return nil
	}
	if err := store.UpdateContextEstimate(ctx, sess.ID, 0, 0); err != nil {
		return err
	}
	sess.LastTotalTokens = 0
	sess.LastMessageCount = 0
	return nil
}
