package serve

import (
	"context"
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestFailureReconciliationRetriesIncrementalWrites(t *testing.T) {
	for _, mode := range []string{"input and output", "partial initial append", "pending output", "assistant update"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			_, store := newServeProjectTestServer(t)
			sess := &session.Session{ID: session.NewID(), Mode: session.ModeChat}
			if err := store.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			base := []core.Message{core.UserText("old question"), core.AssistantText("old answer")}
			for _, msg := range base {
				if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
					t.Fatal(err)
				}
			}
			prefix, err := store.GetMessages(ctx, sess.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			rt := &serveRuntime{store: store}
			input := []core.Message{core.UserText("new question")}
			if mode == "partial initial append" {
				input = append([]core.Message{serveRuntimeTextMessage(core.RoleDeveloper, "new instructions")}, input...)
			}
			p := newServeRunPersistence(rt, sess.ID, 1, serveRunSpec{stateful: true, persisted: true}, base, input, false, "")
			p.produced = []core.Message{core.AssistantText("partial output")}
			if mode == "pending output" || mode == "assistant update" {
				p.initialPersisted = true
				for _, msg := range input {
					if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "partial initial append" {
				if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, input[0], -1)); err != nil {
					t.Fatal(err)
				}
				p.initialAppendedIdx = 1
			}
			var assistantID int64
			if mode == "assistant update" {
				msg := session.NewMessage(sess.ID, core.AssistantText("partial"), -1)
				if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
					t.Fatal(err)
				}
				assistantID = msg.ID
				p.pendingAssistantIdx = 0
				p.pendingAssistantMsgID = msg.ID
				p.lastAppendedIdx = 1
				p.assistantSnapshotDirty = true
				p.assistantSnapshotNeedsReconcile = true
			}
			p.reconcileFailure(ctx, ctx, errors.New("stream failed"), "", func() {})
			stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			assertDurableMessagePrefix(t, prefix, stored)
			if len(stored) != len(base)+len(input)+1 {
				t.Fatalf("recovery duplicated or dropped messages: %+v", stored)
			}
			last := stored[len(stored)-1]
			if last.Role != core.RoleAssistant || last.TextContent != "partial output" {
				t.Fatalf("partial assistant was not recovered: %+v", last)
			}
			if assistantID != 0 && last.ID != assistantID {
				t.Fatalf("assistant upsert changed durable identity %d -> %d", assistantID, last.ID)
			}
		})
	}
}
