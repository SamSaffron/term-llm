package session

import (
	"context"
	"errors"
	"sync"
	"time"
)

// WarnFunc is a function that logs warnings.
type WarnFunc func(format string, args ...any)

// LoggingStore wraps a Store and logs errors instead of silently discarding them.
// This preserves the best-effort semantics (operations don't fail the caller)
// while providing visibility into persistence issues.
type LoggingStore struct {
	Store
	warnFunc WarnFunc
	mu       sync.Mutex
	warned   map[string]bool // Rate-limit warnings by operation type
}

// NewLoggingStore creates a new LoggingStore wrapper.
// The warnFunc is called when persistence operations fail.
func NewLoggingStore(store Store, warnFunc WarnFunc) *LoggingStore {
	return &LoggingStore{
		Store:    store,
		warnFunc: warnFunc,
		warned:   make(map[string]bool),
	}
}

// logOnce logs a warning only once per operation type to avoid spamming.
func (s *LoggingStore) logOnce(op string, err error) {
	if err == nil || s.warnFunc == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.warned[op] {
		return
	}
	s.warned[op] = true
	s.warnFunc("session %s failed: %v", op, err)
}

// Create wraps Store.Create with error logging.
func (s *LoggingStore) Create(ctx context.Context, sess *Session) error {
	err := s.Store.Create(ctx, sess)
	s.logOnce("Create", err)
	return err
}

// Update wraps Store.Update with error logging.
func (s *LoggingStore) Update(ctx context.Context, sess *Session) error {
	err := s.Store.Update(ctx, sess)
	s.logOnce("Update", err)
	return err
}

// UpdateGoal wraps the optional goal-only update path with error logging.
func (s *LoggingStore) UpdateGoal(ctx context.Context, id string, goal *Goal) error {
	updater, ok := s.Store.(GoalUpdater)
	if !ok {
		err := UpdateGoal(ctx, s.Store, id, goal)
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.logOnce("UpdateGoal", err)
		}
		return err
	}
	err := updater.UpdateGoal(ctx, id, goal)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logOnce("UpdateGoal", err)
	}
	return err
}

// UpdateShare wraps the optional share-only update path with error logging.
func (s *LoggingStore) UpdateShare(ctx context.Context, id string, share *ShareState) error {
	updater, ok := s.Store.(ShareUpdater)
	if !ok {
		err := UpdateShare(ctx, s.Store, id, share)
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.logOnce("UpdateShare", err)
		}
		return err
	}
	err := updater.UpdateShare(ctx, id, share)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logOnce("UpdateShare", err)
	}
	return err
}

// AddMessage wraps Store.AddMessage with error logging.
func (s *LoggingStore) AddMessage(ctx context.Context, sessionID string, msg *Message) error {
	err := s.Store.AddMessage(ctx, sessionID, msg)
	s.logOnce("AddMessage", err)
	return err
}

// UpdateMessage wraps Store.UpdateMessage with error logging.
// ErrNotFound is returned to the caller verbatim (no logging) so upsert
// callers can fall back to AddMessage without noise.
func (s *LoggingStore) UpdateMessage(ctx context.Context, sessionID string, msg *Message) error {
	err := s.Store.UpdateMessage(ctx, sessionID, msg)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logOnce("UpdateMessage", err)
	}
	return err
}

// UpdateStreamingMessage wraps the optional streaming-aware update path with the
// same error logging semantics as UpdateMessage.
func (s *LoggingStore) UpdateStreamingMessage(ctx context.Context, sessionID string, msg *Message, finalizeText bool) error {
	updater, ok := s.Store.(StreamingMessageUpdater)
	if !ok {
		return s.UpdateMessage(ctx, sessionID, msg)
	}
	err := updater.UpdateStreamingMessage(ctx, sessionID, msg, finalizeText)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logOnce("UpdateStreamingMessage", err)
	}
	return err
}

// UpdateGeneratedTitle wraps the optional title-only update path with error logging.
func (s *LoggingStore) UpdateGeneratedTitle(ctx context.Context, id, shortTitle, longTitle string, generatedAt time.Time, basisMsgSeq int) error {
	updater, ok := s.Store.(GeneratedTitleUpdater)
	if !ok {
		sess, err := s.Get(ctx, id)
		if err != nil {
			s.logOnce("Get", err)
			return err
		}
		if sess == nil {
			return ErrNotFound
		}
		err = UpdateGeneratedTitle(ctx, s.Store, sess, shortTitle, longTitle, generatedAt, basisMsgSeq)
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.logOnce("UpdateGeneratedTitle", err)
		}
		return err
	}
	err := updater.UpdateGeneratedTitle(ctx, id, shortTitle, longTitle, generatedAt, basisMsgSeq)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logOnce("UpdateGeneratedTitle", err)
	}
	return err
}

// GetMessageByID wraps Store.GetMessageByID with error logging.
func (s *LoggingStore) GetMessageByID(ctx context.Context, msgID int64) (*Message, error) {
	msg, err := s.Store.GetMessageByID(ctx, msgID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logOnce("GetMessageByID", err)
	}
	return msg, err
}

// GetLatestVisibleMessageID returns the latest persisted user/assistant message id for a session.
func (s *LoggingStore) GetLatestVisibleMessageID(ctx context.Context, sessionID string) (int64, error) {
	getter, ok := s.Store.(interface {
		GetLatestVisibleMessageID(context.Context, string) (int64, error)
	})
	if ok {
		msgID, err := getter.GetLatestVisibleMessageID(ctx, sessionID)
		if err != nil {
			s.logOnce("GetLatestVisibleMessageID", err)
		}
		return msgID, err
	}

	msgs, err := s.Store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		s.logOnce("GetLatestVisibleMessageID", err)
		return 0, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		role := string(msgs[i].Role)
		if (role == "user" || role == "assistant") && !msgs[i].CompactionTail {
			return msgs[i].ID, nil
		}
	}
	return 0, nil
}

// GetMessagesPageDescending returns a reverse-ordered page of messages. It
// delegates when the wrapped store supports efficient paging; otherwise it falls
// back to in-memory paging over GetMessages.
func (s *LoggingStore) GetMessagesPageDescending(ctx context.Context, sessionID string, beforeSeq, limit int) ([]Message, error) {
	getter, ok := s.Store.(MessagesDescendingPager)
	if ok {
		msgs, err := getter.GetMessagesPageDescending(ctx, sessionID, beforeSeq, limit)
		if err != nil {
			s.logOnce("GetMessagesPageDescending", err)
		}
		return msgs, err
	}

	msgs, err := s.Store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		s.logOnce("GetMessagesPageDescending", err)
		return nil, err
	}

	capHint := len(msgs)
	if limit > 0 && limit < capHint {
		capHint = limit
	}
	page := make([]Message, 0, capHint)
	for i := len(msgs) - 1; i >= 0; i-- {
		if beforeSeq > 0 && msgs[i].Sequence >= beforeSeq {
			continue
		}
		page = append(page, msgs[i])
		if limit > 0 && len(page) >= limit {
			break
		}
	}
	return page, nil
}

// PreviousUserPrompt delegates the optional PromptHistoryStore capability when
// the wrapped store supports it.
func (s *LoggingStore) PreviousUserPrompt(ctx context.Context, agent string, beforeID int64) (*PromptHistoryEntry, error) {
	history, ok := s.Store.(PromptHistoryStore)
	if !ok {
		return nil, nil
	}
	entry, err := history.PreviousUserPrompt(ctx, agent, beforeID)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.logOnce("PreviousUserPrompt", err)
	}
	return entry, err
}

// NextUserPrompt delegates the optional PromptHistoryStore capability when the
// wrapped store supports it.
func (s *LoggingStore) NextUserPrompt(ctx context.Context, agent string, afterID int64) (*PromptHistoryEntry, error) {
	history, ok := s.Store.(PromptHistoryStore)
	if !ok {
		return nil, nil
	}
	entry, err := history.NextUserPrompt(ctx, agent, afterID)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.logOnce("NextUserPrompt", err)
	}
	return entry, err
}

// PreviousUserPromptOutsideSession delegates the optional global prompt-history
// capability when the wrapped store supports it.
func (s *LoggingStore) PreviousUserPromptOutsideSession(ctx context.Context, excludeSessionID string, beforeID int64, beforeCreatedAt time.Time) (*PromptHistoryEntry, error) {
	history, ok := s.Store.(PromptHistoryOutsideSessionStore)
	if !ok {
		return nil, nil
	}
	entry, err := history.PreviousUserPromptOutsideSession(ctx, excludeSessionID, beforeID, beforeCreatedAt)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.logOnce("PreviousUserPromptOutsideSession", err)
	}
	return entry, err
}

// NextUserPromptOutsideSession delegates the optional global prompt-history
// capability when the wrapped store supports it.
func (s *LoggingStore) NextUserPromptOutsideSession(ctx context.Context, excludeSessionID string, afterID int64, afterCreatedAt time.Time) (*PromptHistoryEntry, error) {
	history, ok := s.Store.(PromptHistoryOutsideSessionStore)
	if !ok {
		return nil, nil
	}
	entry, err := history.NextUserPromptOutsideSession(ctx, excludeSessionID, afterID, afterCreatedAt)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.logOnce("NextUserPromptOutsideSession", err)
	}
	return entry, err
}

// SaveProviderState delegates optional provider resume state persistence.
func (s *LoggingStore) SaveProviderState(ctx context.Context, sessionID, providerKey string, state []byte) error {
	store, ok := s.Store.(ProviderStateStore)
	if !ok {
		return nil
	}
	err := store.SaveProviderState(ctx, sessionID, providerKey, state)
	s.logOnce("SaveProviderState", err)
	return err
}

// LoadProviderState delegates optional provider resume state loading.
func (s *LoggingStore) LoadProviderState(ctx context.Context, sessionID, providerKey string) ([]byte, error) {
	store, ok := s.Store.(ProviderStateStore)
	if !ok {
		return nil, nil
	}
	state, err := store.LoadProviderState(ctx, sessionID, providerKey)
	s.logOnce("LoadProviderState", err)
	return state, err
}

// DeleteProviderState delegates optional provider resume state deletion.
func (s *LoggingStore) DeleteProviderState(ctx context.Context, sessionID, providerKey string) error {
	store, ok := s.Store.(ProviderStateStore)
	if !ok {
		return nil
	}
	err := store.DeleteProviderState(ctx, sessionID, providerKey)
	s.logOnce("DeleteProviderState", err)
	return err
}

// ReplaceCompactedMessages wraps optional Store.ReplaceCompactedMessages with error logging.
func (s *LoggingStore) ReplaceCompactedMessages(ctx context.Context, sessionID string, messages []Message) error {
	replacer, ok := s.Store.(interface {
		ReplaceCompactedMessages(context.Context, string, []Message) error
	})
	if !ok {
		return nil
	}
	err := replacer.ReplaceCompactedMessages(ctx, sessionID, messages)
	s.logOnce("ReplaceCompactedMessages", err)
	return err
}

// UpdateMetrics wraps Store.UpdateMetrics with error logging.
func (s *LoggingStore) UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens int) error {
	err := s.Store.UpdateMetrics(ctx, id, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens)
	s.logOnce("UpdateMetrics", err)
	return err
}

// UpdateContextEstimate wraps Store.UpdateContextEstimate with error logging.
func (s *LoggingStore) UpdateContextEstimate(ctx context.Context, id string, lastTotalTokens, lastMessageCount int) error {
	err := s.Store.UpdateContextEstimate(ctx, id, lastTotalTokens, lastMessageCount)
	s.logOnce("UpdateContextEstimate", err)
	return err
}

// ClearCompactionBoundary wraps optional Store.ClearCompactionBoundary with error logging.
func (s *LoggingStore) ClearCompactionBoundary(ctx context.Context, id string) error {
	clearer, ok := s.Store.(interface {
		ClearCompactionBoundary(context.Context, string) error
	})
	if !ok {
		return nil
	}
	err := clearer.ClearCompactionBoundary(ctx, id)
	s.logOnce("ClearCompactionBoundary", err)
	return err
}

// UpdateStatus wraps Store.UpdateStatus with error logging.
func (s *LoggingStore) UpdateStatus(ctx context.Context, id string, status SessionStatus) error {
	err := s.Store.UpdateStatus(ctx, id, status)
	s.logOnce("UpdateStatus", err)
	return err
}

// IncrementUserTurns wraps Store.IncrementUserTurns with error logging.
func (s *LoggingStore) IncrementUserTurns(ctx context.Context, id string) error {
	err := s.Store.IncrementUserTurns(ctx, id)
	s.logOnce("IncrementUserTurns", err)
	return err
}

// SetCurrent wraps Store.SetCurrent with error logging.
func (s *LoggingStore) SetCurrent(ctx context.Context, sessionID string) error {
	err := s.Store.SetCurrent(ctx, sessionID)
	s.logOnce("SetCurrent", err)
	return err
}
