package chat

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSendMessageTimeGroundingRequiresAgentOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled *bool
		want    bool
	}{
		{name: "omitted", want: false},
		{name: "enabled", enabled: boolPointer(true), want: true},
		{name: "disabled", enabled: boolPointer(false), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newTestChatModel(false)
			model.currentAgent = &agents.Agent{Name: "test", TimeGrounding: tc.enabled}
			_, _ = model.sendMessage("hello")
			got := false
			for i := range model.messages {
				got = got || llm.IsConversationStartMessage(model.messages[i].ToLLMMessage())
			}
			if got != tc.want {
				t.Fatalf("conversation start present = %v, want %v; messages = %#v", got, tc.want, model.messages)
			}
		})
	}
}

func boolPointer(value bool) *bool { return &value }

func withoutConversationStartMessages(messages []session.Message) []session.Message {
	out := make([]session.Message, 0, len(messages))
	for i := range messages {
		if llm.IsConversationStartMessage(messages[i].ToLLMMessage()) {
			continue
		}
		out = append(out, messages[i])
	}
	return out
}
