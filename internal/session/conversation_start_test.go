package session

import (
	"context"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestConversationStartSurvivesPersistenceAndCompactionBoundary(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: "conversation-start", Provider: "mock", Model: "mock", CreatedAt: time.Now(), UpdatedAt: time.Now(), CompactionSeq: -1}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	start := llm.ConversationStartMessage(time.Date(2026, time.September, 9, 13, 42, 0, 0, time.FixedZone("AEST", 10*60*60)))
	platform := llm.PlatformContextMessage("web context")
	for _, message := range []llm.Message{llm.SystemText("system"), platform, start, llm.UserText("question"), llm.AssistantText("answer")} {
		if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, message, -1)); err != nil {
			t.Fatal(err)
		}
	}

	result := llm.CompactionResultFromBrief("rebuilt system", "continue", []llm.Message{llm.SystemText("system"), platform, start, llm.UserText("question"), llm.AssistantText("answer")}, llm.CompactionConfig{})
	if _, _, refreshed, err := ApplyCompaction(ctx, store, sess, nil, result); err != nil {
		t.Fatal(err)
	} else if refreshed != nil {
		sess = refreshed
	}
	active, err := LoadActiveMessages(ctx, store, sess)
	if err != nil {
		t.Fatal(err)
	}
	startCount, platformCount := 0, 0
	for i := range active {
		message := active[i].ToLLMMessage()
		if llm.IsConversationStartMessage(message) {
			startCount++
		}
		if llm.IsPlatformContextMessage(message) {
			platformCount++
		}
	}
	if startCount != 1 || platformCount != 1 {
		t.Fatalf("active durable context counts = (start %d, platform %d), want (1, 1); messages = %#v", startCount, platformCount, active)
	}
}
