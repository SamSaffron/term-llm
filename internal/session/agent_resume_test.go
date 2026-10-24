package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestFailedAgentRunRequiresDurableHistory(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, tc := range []struct {
		name     string
		started  bool
		messages []core.Message
		want     bool
	}{
		{name: "initialization failure"},
		{name: "started without transcript", started: true},
		{name: "only initial prompt", started: true, messages: []core.Message{core.UserText("work")}},
		{name: "assistant history", started: true, messages: []core.Message{core.UserText("work"), core.AssistantText("partial")}, want: true},
		{name: "tool result", started: true, messages: []core.Message{core.ToolResultMessage("call", "shell", "committed", nil)}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := AgentRun{ID: tc.name, Status: "failed", Started: tc.started}
			if len(tc.messages) > 0 {
				if err := store.Create(ctx, &Session{ID: r.ID, Provider: "test", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				for _, msg := range tc.messages {
					if err := store.AddMessage(ctx, r.ID, NewMessage(r.ID, msg, -1)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got := AgentRunResumable(ctx, store, r); got != tc.want {
				t.Fatalf("resumable = %t, want %t", got, tc.want)
			}
			if AgentRunResumable(ctx, nil, r) {
				t.Fatal("failed run without persistence was resumable")
			}
		})
	}
}
