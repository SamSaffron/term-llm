package session

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestFallbackTranscriptIndexerProvidesMonotonicCoherentContract(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	adapter := NewFallbackTranscriptIndexer(store)

	user := NewMessage(sess.ID, llm.Message{
		Role:            llm.RoleUser,
		Parts:           llm.UserText("hello").Parts,
		ClientMessageID: "client-fallback",
	}, -1)
	if err := store.AddMessage(ctx, sess.ID, user); err != nil {
		t.Fatal(err)
	}
	first, err := adapter.GetTranscriptSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Rev <= 0 || len(first.Items) != 1 || first.Items[0].ClientMessageID != "client-fallback" {
		t.Fatalf("first fallback snapshot = %#v", first)
	}

	assistant := NewMessage(sess.ID, llm.AssistantText("answer"), -1)
	assistant.ClientMessageID = "must-not-leak"
	if err := store.AddMessage(ctx, sess.ID, assistant); err != nil {
		t.Fatal(err)
	}
	second, err := adapter.GetTranscriptSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Rev <= first.Rev || len(second.Items) != 2 {
		t.Fatalf("second fallback snapshot = %#v, first rev=%d", second, first.Rev)
	}
	if second.Items[1].ClientMessageID != "" {
		t.Fatalf("assistant client_message_id leaked into sparse index: %#v", second.Items[1])
	}

	rev, bodies, err := adapter.GetMessagesByTranscriptRanges(ctx, sess.ID, []TranscriptRange{{
		StartSeq: second.Items[0].Seq,
		StartID:  second.Items[0].ID,
		EndSeq:   second.Items[1].Seq,
		EndID:    second.Items[1].ID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rev != second.Rev || len(bodies) != 2 {
		t.Fatalf("fallback bodies rev=%d len=%d, want rev=%d len=2", rev, len(bodies), second.Rev)
	}
}
