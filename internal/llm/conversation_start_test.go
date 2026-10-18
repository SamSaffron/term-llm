package llm

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestConversationStartMessageUsesFixedMinutePrecision(t *testing.T) {
	zone := time.FixedZone("AEST", 10*60*60)
	message := ConversationStartMessage(time.Date(2026, time.September, 9, 13, 42, 57, 123, zone))

	if !IsConversationStartMessage(message) {
		t.Fatalf("message was not recognized: %#v", message)
	}
	want := "Conversation started at 2026-09-09 13:42 AEST (UTC+10:00). This timestamp is fixed and does not update during the conversation."
	if got := core.MessageText(message); got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestBeginConversationInsertsAfterSystemBeforeFirstUser(t *testing.T) {
	messages := []core.Message{core.SystemText("system"), core.UserText("hello")}
	got := BeginConversation(messages, time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC))
	if len(got) != 3 || got[0].Role != core.RoleSystem || !IsConversationStartMessage(got[1]) || got[2].Role != core.RoleUser {
		t.Fatalf("messages = %#v", got)
	}
	if len(messages) != 2 {
		t.Fatalf("input mutated: %#v", messages)
	}
}

func TestBeginConversationDoesNotRelabelEstablishedHistory(t *testing.T) {
	messages := []core.Message{core.UserText("old question"), core.AssistantText("old answer"), core.UserText("new question")}
	got := BeginConversation(messages, time.Now())
	if len(got) != len(messages) {
		t.Fatalf("inserted into established history: %#v", got)
	}
}

func TestWithoutConversationStartRemovesOnlyMarkedMessages(t *testing.T) {
	start := ConversationStartMessage(time.Now())
	messages := []core.Message{core.SystemText("system"), start, core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "keep"}}}, core.UserText("hello")}
	got := WithoutConversationStart(messages)
	if len(got) != 3 || got[0].Role != core.RoleSystem || core.MessageText(got[1]) != "keep" || got[2].Role != core.RoleUser {
		t.Fatalf("messages = %#v", got)
	}
	if len(messages) != 4 {
		t.Fatalf("input mutated: %#v", messages)
	}
}

func TestInsertConversationStartPreservesEarliestAndDeduplicates(t *testing.T) {
	first := ConversationStartMessage(time.Date(2026, time.January, 1, 1, 2, 0, 0, time.UTC))
	later := ConversationStartMessage(time.Date(2026, time.February, 2, 2, 3, 0, 0, time.UTC))
	destination := []core.Message{core.SystemText("system"), later, core.UserText("summary")}
	got := InsertConversationStart(destination, []core.Message{first, later})
	if len(got) != len(destination) || core.MessageText(got[1]) != core.MessageText(later) {
		t.Fatalf("existing destination marker was not preserved: %#v", got)
	}

	got = InsertConversationStart([]core.Message{core.SystemText("system"), core.UserText("summary")}, []core.Message{first, later})
	if len(got) != 3 || core.MessageText(got[1]) != core.MessageText(first) {
		t.Fatalf("earliest source marker not inserted: %#v", got)
	}
}
