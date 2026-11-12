package serve

import (
	"context"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

type telegramPromptStore struct {
	session.NoopStore
	messages []session.Message
}

func (s *telegramPromptStore) AddMessage(_ context.Context, _ string, message *session.Message) error {
	s.messages = append(s.messages, *message)
	return nil
}

func (s *telegramPromptStore) ResetMessages(_ context.Context, _ string, messages []session.Message) error {
	s.messages = append([]session.Message(nil), messages...)
	return nil
}

func TestTelegramUsesRuntimeSkillPrompt(t *testing.T) {
	preloaded := "<preloaded_skills>Session-specific instructions and admitted module.</preloaded_skills>"
	empty := ""
	for _, tc := range []struct {
		name   string
		prompt *string
		want   string
	}{
		{name: "rendered runtime", prompt: &preloaded, want: preloaded},
		{name: "explicit empty runtime", prompt: &empty},
		{name: "legacy factory", want: "Shared catalog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &telegramPromptStore{}
			mgr := &telegramSessionMgr{settings: Settings{SystemPrompt: "Shared catalog"}, store: store}
			sess := &telegramSession{runtime: &SessionRuntime{SystemPrompt: tc.prompt}, meta: &session.Session{ID: "test"}}
			user := core.UserText("Question")
			messages := mgr.prepareRequestHistory(sess, user, nil, time.Unix(0, 0))
			var systems []string
			for _, message := range messages {
				if message.Role == core.RoleSystem {
					systems = append(systems, collectUserText(message))
				}
			}
			check := func(systems []string) {
				t.Helper()
				if tc.want == "" {
					if len(systems) != 0 {
						t.Fatalf("unexpected system prompt: %q", systems)
					}
				} else if len(systems) != 1 || systems[0] != tc.want {
					t.Fatalf("system prompts=%q, want %q", systems, tc.want)
				}
			}
			check(systems)
			checkStored := func() {
				t.Helper()
				var systems []string
				for _, message := range store.messages {
					if message.Role == core.RoleSystem {
						systems = append(systems, message.TextContent)
					}
				}
				check(systems)
			}
			if degraded, _ := mgr.persistTurnStart(context.Background(), sess, user, "Question", nil); degraded {
				t.Fatal("turn-start persistence failed")
			}
			checkStored()
			if !mgr.reconcileTelegramTranscript(context.Background(), sess, []core.Message{user}, true, "test") {
				t.Fatal("transcript reconciliation failed")
			}
			checkStored()
		})
	}
}
