package chat

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestHandoverModelSwapMarkerEffort(t *testing.T) {
	for _, tt := range []struct{ name, explicit, want string }{
		{"suffix", "", "high"},
		{"explicit", "low", "low"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newCmdTestModel(&mockStore{})
			m.providerKey, m.modelName, m.agentName = "openai", "gpt-5-high", "dev"
			m.sess = &session.Session{ReasoningEffort: tt.explicit}
			target := &session.Session{ProviderKey: "claude-bin", Model: "opus-max", Agent: "reviewer"}
			marker, ok := core.ParseModelSwapMarker(m.handoverModelSwapMarker(target, "dev"))
			if !ok || marker.FromProvider != "openai" || marker.FromModel != "gpt-5" || marker.FromEffort != tt.want || marker.ToProvider != "claude-bin" || marker.ToModel != "opus" || marker.ToEffort != "max" || marker.FromAgent != "dev" || marker.ToAgent != "reviewer" {
				t.Fatalf("handover marker = %+v, parsed %v", marker, ok)
			}
		})
	}
}

func TestInsertHandoverMarker(t *testing.T) {
	for _, prompt := range []string{"", "system"} {
		t.Run(prompt, func(t *testing.T) {
			messages := engine.ReconstructHandoverHistory(prompt, "doc", "dev", "reviewer")
			marker := core.ModelSwapEventMessage(core.ModelSwapMarker{Strategy: "handover"})
			got := insertHandoverMarker(messages, marker)
			index := 0
			if prompt != "" {
				index = 1
			}
			if got[index].Role != core.RoleEvent || got[index+1].Role != core.RoleUser || len(got) != len(messages)+1 {
				t.Fatalf("handover messages = %+v", got)
			}
			if len(llm.FilterConversationMessages(got)) != len(messages) {
				t.Fatal("event entered provider history")
			}
		})
	}
}
