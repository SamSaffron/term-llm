package agentbarrier

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestPersistNoticeReusesOnlyTrustedExactGenerations(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock"}
	if err := store.Create(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	events := []session.AgentRun{{ID: "b", RunGeneration: 2, Status: "completed"}, {ID: "a", RunGeneration: 1, Status: "completed"}}
	notice := tools.AgentLifecycleMessage(events, true)
	// Matching human-authored text must never substitute for host provenance.
	quoted := core.UserText(core.MessageText(notice))
	human := session.NewMessage(parent.ID, quoted, -1)
	if err := store.AddMessage(t.Context(), parent.ID, human); err != nil {
		t.Fatal(err)
	}
	messages := []core.Message{quoted}
	boundary := runboundary.New("run", messages, human.ID, true, runboundary.ProviderContext{})
	ctx := core.ContextWithSessionID(t.Context(), parent.ID)
	stopped := &atomic.Bool{}
	owned, cancel := ExecutionContext(ctx, stopped)
	defer cancel()
	stopped.Store(true) // Human Stop raced publication; generic cancellation has not arrived.
	if owned.Err() != nil {
		t.Fatal("test requires a live context with newly recorded Stop intent")
	}
	if _, row, err := PersistNotice(owned, store, events, messages, boundary); !errors.Is(err, context.Canceled) || row != nil {
		t.Fatalf("late stop persisted a notice: row=%+v err=%v", row, err)
	}
	messages, row, err := PersistNotice(ctx, store, events, messages, boundary)
	if err != nil || row == nil || row.ID == human.ID {
		t.Fatalf("human text reused as a host notice: row=%+v err=%v", row, err)
	}
	anchor := row.ID
	// Conversion strips persistence-only markers; reuse must validate the
	// durable row's provenance, not expect markers in provider history.
	messages[len(messages)-1] = row.ToLLMMessage()
	events[0], events[1] = events[1], events[0]
	messages, row, err = PersistNotice(ctx, store, events, messages, boundary)
	if err != nil || row != nil || len(messages) != 2 || boundary.CompletedSnapshot().DurableAnchorID != anchor {
		t.Fatalf("retry changed notice or anchor: row=%+v messages=%d err=%v", row, len(messages), err)
	}
	events[0].RunGeneration++
	_, row, err = PersistNotice(ctx, store, events, messages, boundary)
	if err != nil || row == nil || row.ID == anchor {
		t.Fatalf("another generation reused a stale notice: row=%+v err=%v", row, err)
	}
	stored, err := store.GetMessages(ctx, parent.ID, 0, 0)
	if err != nil || len(stored) != 3 {
		t.Fatalf("unexpected persisted retry rows: rows=%d err=%v", len(stored), err)
	}
}
