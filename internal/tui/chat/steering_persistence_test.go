package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// clientIDUniqueStore mirrors the session store's unique index on
// (session_id, client_message_id), which only applies to non-empty identities.
type clientIDUniqueStore struct {
	*mockStore
	seen map[string]bool
}

func newClientIDUniqueStore() *clientIDUniqueStore {
	return &clientIDUniqueStore{mockStore: &mockStore{}, seen: map[string]bool{}}
}

func (s *clientIDUniqueStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	if id := strings.TrimSpace(msg.ClientMessageID); id != "" {
		key := sessionID + "\x00" + id
		if s.seen[key] {
			return &uniqueClientIDError{}
		}
		s.seen[key] = true
	}
	return s.mockStore.AddMessage(ctx, sessionID, msg)
}

type uniqueClientIDError struct{}

func (e *uniqueClientIDError) Error() string {
	return "insert message: constraint failed: UNIQUE constraint failed: messages.session_id, messages.client_message_id (2067)"
}

// TestPendingSteeringIDsSurviveProcessRestart pins the identity that goes on
// to become a persisted client_message_id. A per-process counter alone repeats
// "tui-steer-1" every time a session is resumed, and the store's unique
// index then rejects the row, silently dropping the steering.
func TestPendingSteeringIDsSurviveProcessRestart(t *testing.T) {
	first := newTestChatModel(false)
	second := newTestChatModel(false)

	firstIDs := []string{first.nextPendingSteeringID(), first.nextPendingSteeringID()}
	secondIDs := []string{second.nextPendingSteeringID(), second.nextPendingSteeringID()}

	seen := map[string]bool{}
	for _, id := range append(append([]string{}, firstIDs...), secondIDs...) {
		if !strings.HasPrefix(id, "tui-steer-") {
			t.Fatalf("id %q lost its recognisable prefix", id)
		}
		if seen[id] {
			t.Fatalf("id %q reused across models; a resumed session would collide on client_message_id", id)
		}
		seen[id] = true
	}
	if firstIDs[0] == secondIDs[0] {
		t.Fatalf("first id of each model matched (%q); process restart still repeats identities", firstIDs[0])
	}
}

// Duplicate receipts reconcile to their original row without losing identity.
func TestPersistSteeringReconcilesDuplicateReceipt(t *testing.T) {
	m := newTestChatModel(false)
	store := newClientIDUniqueStore()
	m.sess.ID = "steer-conflict"

	msg := llm.UserText("steer left")
	msg.ClientMessageID = "tui-steer-1"

	// An earlier process already wrote this identity into the same session.
	if err := store.AddMessage(context.Background(), m.sess.ID, session.NewMessage(m.sess.ID, msg, -1)); err != nil {
		t.Fatalf("seed existing steering: %v", err)
	}
	var warnings []string
	m.store = session.NewLoggingStore(store, func(format string, args ...any) {
		warnings = append(warnings, format)
	})

	m.persistSteering(context.Background(), "steer left", msg)

	if len(warnings) != 0 {
		t.Fatalf("recovered client id conflict emitted warnings: %v", warnings)
	}
	var texts []string
	for _, added := range store.added {
		texts = append(texts, added.TextContent)
	}
	if len(store.added) != 1 {
		t.Fatalf("duplicate receipt appended %d rows", len(store.added))
	}
	if store.added[0].ClientMessageID != msg.ClientMessageID {
		t.Fatal("identity was stripped")
	}
}

func TestPersistSteeringRejectsDifferentContentForSameID(t *testing.T) {
	m := newTestChatModel(false)
	store := newClientIDUniqueStore()
	m.sess.ID = "steer-retry-failure"

	msg := llm.UserText("steer left")
	msg.ClientMessageID = "tui-steer-1"
	if err := store.AddMessage(context.Background(), m.sess.ID, session.NewMessage(m.sess.ID, msg, -1)); err != nil {
		t.Fatalf("seed existing steering: %v", err)
	}
	store.addErr = context.DeadlineExceeded
	var warnings int
	m.store = session.NewLoggingStore(store, func(string, ...any) { warnings++ })

	msg.Parts = llm.UserText("different guidance").Parts
	m.persistSteering(context.Background(), "different guidance", msg)

	if warnings != 1 {
		t.Fatalf("warnings = %d, want content conflict surfaced once", warnings)
	}
}

// TestPersistSteeringDoesNotRetryUnrelatedFailures keeps a genuine store
// outage from writing the message twice.
func TestPersistSteeringDoesNotRetryUnrelatedFailures(t *testing.T) {
	m := newTestChatModel(false)
	store := &mockStore{addErr: context.DeadlineExceeded}
	var warnings int
	m.store = session.NewLoggingStore(store, func(string, ...any) { warnings++ })
	m.sess.ID = "steer-outage"

	msg := llm.UserText("steer left")
	msg.ClientMessageID = "tui-steer-1"
	m.persistSteering(context.Background(), "steer left", msg)

	if len(store.added) != 0 {
		t.Fatalf("persisted %d messages, want none after an unrelated failure", len(store.added))
	}
	if warnings != 1 {
		t.Fatalf("warnings = %d, want unrelated failure surfaced once", warnings)
	}
}
