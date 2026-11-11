package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestMemoryEvidenceRoleBoundaries(t *testing.T) {
	messages := []session.Message{
		{Role: core.RoleSystem, TextContent: "BOOT_SENTINEL"},
		{Role: core.RoleDeveloper, TextContent: "DEVELOPER_SENTINEL"},
		{Role: core.RoleEvent, TextContent: "EVENT_SENTINEL"},
		{Role: core.RoleUser, TextContent: "The corrected durable fact is user evidence."},
		{Role: core.RoleAssistant, TextContent: "Assistant confirmation."},
	}
	for _, transcript := range [][]transcriptMessage{buildTranscript(messages), buildInsightTranscript(messages)} {
		var user, assistant bool
		for _, m := range transcript {
			if strings.Contains(m.Text, "SENTINEL") {
				t.Fatal("privileged context entered evidence")
			}
			user = user || m.Role == "user"
			assistant = assistant || m.Role == "assistant"
		}
		if !user || !assistant {
			t.Fatal("conversation lost")
		}
	}
}

func TestMemoryExcludedRolesCursorAndPageBoundaries(t *testing.T) {
	opts := MineOptions{BatchSize: 1, MaxMessages: 1, PromptMaxTokens: 2000}
	t.Setenv("TERM_LLM_MEMORY_CONTENT_GUARD", "")
	candidate := memoryMineCandidate{Session: &session.Session{ID: "synthetic"}, Agent: "jarvis"}
	store := &trackingMemoryMineStore{messages: map[string][]session.Message{"synthetic": {
		{Sequence: 0, Role: core.RoleSystem, TextContent: strings.Repeat("boot ", 100000)},
		{Sequence: 3, Role: core.RoleDeveloper, TextContent: strings.Repeat("developer ", 100000)},
		{Sequence: 7, Role: core.RoleUser, TextContent: "Correction: preserve this real conversation."},
		{Sequence: 8, Role: core.RoleAssistant, TextContent: "Confirmed real assistant evidence."},
		{Sequence: 12, Role: core.RoleSystem, TextContent: "trailing context"},
	}}}
	first, err := opts.loadMessagesForMining(context.Background(), store, candidate, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.NextOffset != 8 || len(first.Messages) != 1 || first.TruncatedMessages != 0 {
		t.Fatalf("unexpected first boundary: %+v", first)
	}
	second, err := opts.loadMessagesForMining(context.Background(), store, candidate, first.NextOffset, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.NextOffset != 9 || len(second.Messages) != 1 || second.Messages[0].Role != core.RoleAssistant {
		t.Fatal("assistant tail skipped")
	}
	third, err := opts.loadMessagesForMining(context.Background(), store, candidate, second.NextOffset, "")
	if err != nil {
		t.Fatal(err)
	}
	if third.NextOffset != 13 || len(third.Messages) != 0 {
		t.Fatal("context-only tail did not advance")
	}
	fourth, err := opts.loadMessagesForMining(context.Background(), store, candidate, third.NextOffset, "")
	if err != nil || fourth.NextOffset != 13 || len(fourth.Messages) != 0 {
		t.Fatal("finished cursor retries or regresses")
	}
}
