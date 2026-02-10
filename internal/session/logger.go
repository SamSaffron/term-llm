package session

import (
	"context"
	"sync"
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

// AddMessage wraps Store.AddMessage with error logging.
func (s *LoggingStore) AddMessage(ctx context.Context, sessionID string, msg *Message) error {
	err := s.Store.AddMessage(ctx, sessionID, msg)
	s.logOnce("AddMessage", err)
	return err
}

// UpdateMetrics wraps Store.UpdateMetrics with error logging.
func (s *LoggingStore) UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens int) error {
	err := s.Store.UpdateMetrics(ctx, id, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens)
	s.logOnce("UpdateMetrics", err)
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
