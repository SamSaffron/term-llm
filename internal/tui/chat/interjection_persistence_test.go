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

// TestPendingInterjectionIDsSurviveProcessRestart pins the identity that goes on
// to become a persisted client_message_id. A per-process counter alone repeats
// "tui-interject-1" every time a session is resumed, and the store's unique
// index then rejects the row, silently dropping the interjection.
func TestPendingInterjectionIDsSurviveProcessRestart(t *testing.T) {
	first := newTestChatModel(false)
	second := newTestChatModel(false)

	firstIDs := []string{first.nextPendingInterjectionID(), first.nextPendingInterjectionID()}
	secondIDs := []string{second.nextPendingInterjectionID(), second.nextPendingInterjectionID()}

	seen := map[string]bool{}
	for _, id := range append(append([]string{}, firstIDs...), secondIDs...) {
		if !strings.HasPrefix(id, "tui-interject-") {
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

// TestPersistInterjectionSurvivesClientIDConflict keeps the interjection in the
// transcript even when its identity is already taken. Dropping it left the
// assistant's reply with no matching user turn, so a resumed conversation showed
// the model answering something nobody appeared to have said.
func TestPersistInterjectionSurvivesClientIDConflict(t *testing.T) {
	m := newTestChatModel(false)
	store := newClientIDUniqueStore()
	m.sess.ID = "interject-conflict"

	msg := llm.UserText("steer left")
	msg.ClientMessageID = "tui-interject-1"

	// An earlier process already wrote this identity into the same session.
	if err := store.AddMessage(context.Background(), m.sess.ID, session.NewMessage(m.sess.ID, msg, -1)); err != nil {
		t.Fatalf("seed existing interjection: %v", err)
	}
	var warnings []string
	m.store = session.NewLoggingStore(store, func(format string, args ...any) {
		warnings = append(warnings, format)
	})

	m.persistInterjection(context.Background(), "steer left", msg)

	if len(warnings) != 0 {
		t.Fatalf("recovered client id conflict emitted warnings: %v", warnings)
	}
	var texts []string
	for _, added := range store.added {
		texts = append(texts, added.TextContent)
	}
	if len(store.added) != 2 {
		t.Fatalf("persisted messages = %d (%v), want the conflicting interjection retained", len(store.added), texts)
	}
	retry := store.added[1]
	if retry.TextContent != "steer left" {
		t.Fatalf("retry text = %q, want the interjection text", retry.TextContent)
	}
	if retry.ClientMessageID != "" {
		t.Fatalf("retry kept client id %q; it must be dropped to clear the unique index", retry.ClientMessageID)
	}
	if retry.Role != llm.RoleUser {
		t.Fatalf("retry role = %q, want user", retry.Role)
	}
}

func TestPersistInterjectionLogsRetryFailureAfterClientIDConflict(t *testing.T) {
	m := newTestChatModel(false)
	store := newClientIDUniqueStore()
	m.sess.ID = "interject-retry-failure"

	msg := llm.UserText("steer left")
	msg.ClientMessageID = "tui-interject-1"
	if err := store.AddMessage(context.Background(), m.sess.ID, session.NewMessage(m.sess.ID, msg, -1)); err != nil {
		t.Fatalf("seed existing interjection: %v", err)
	}
	store.addErr = context.DeadlineExceeded
	var warnings int
	m.store = session.NewLoggingStore(store, func(string, ...any) { warnings++ })

	m.persistInterjection(context.Background(), "steer left", msg)

	if warnings != 1 {
		t.Fatalf("warnings = %d, want retry failure surfaced once", warnings)
	}
}

// TestPersistInterjectionDoesNotRetryUnrelatedFailures keeps a genuine store
// outage from writing the message twice.
func TestPersistInterjectionDoesNotRetryUnrelatedFailures(t *testing.T) {
	m := newTestChatModel(false)
	store := &mockStore{addErr: context.DeadlineExceeded}
	var warnings int
	m.store = session.NewLoggingStore(store, func(string, ...any) { warnings++ })
	m.sess.ID = "interject-outage"

	msg := llm.UserText("steer left")
	msg.ClientMessageID = "tui-interject-1"
	m.persistInterjection(context.Background(), "steer left", msg)

	if len(store.added) != 0 {
		t.Fatalf("persisted %d messages, want none after an unrelated failure", len(store.added))
	}
	if warnings != 1 {
		t.Fatalf("warnings = %d, want unrelated failure surfaced once", warnings)
	}
}
