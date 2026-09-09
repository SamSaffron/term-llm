package llm

import (
	"testing"
	"time"
)

func TestConversationStartMessageUsesFixedMinutePrecision(t *testing.T) {
	zone := time.FixedZone("AEST", 10*60*60)
	message := ConversationStartMessage(time.Date(2026, time.September, 9, 13, 42, 57, 123, zone))

	if !IsConversationStartMessage(message) {
		t.Fatalf("message was not recognized: %#v", message)
	}
	want := "Conversation started at 2026-09-09 13:42 AEST (UTC+10:00). This timestamp is fixed and does not update during the conversation."
	if got := MessageText(message); got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestBeginConversationInsertsAfterSystemBeforeFirstUser(t *testing.T) {
	messages := []Message{SystemText("system"), UserText("hello")}
	got := BeginConversation(messages, time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC))
	if len(got) != 3 || got[0].Role != RoleSystem || !IsConversationStartMessage(got[1]) || got[2].Role != RoleUser {
		t.Fatalf("messages = %#v", got)
	}
	if len(messages) != 2 {
		t.Fatalf("input mutated: %#v", messages)
	}
}

func TestBeginConversationDoesNotRelabelEstablishedHistory(t *testing.T) {
	messages := []Message{UserText("old question"), AssistantText("old answer"), UserText("new question")}
	got := BeginConversation(messages, time.Now())
	if len(got) != len(messages) {
		t.Fatalf("inserted into established history: %#v", got)
	}
}

func TestWithoutConversationStartRemovesOnlyMarkedMessages(t *testing.T) {
	start := ConversationStartMessage(time.Now())
	messages := []Message{SystemText("system"), start, Message{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "keep"}}}, UserText("hello")}
	got := WithoutConversationStart(messages)
	if len(got) != 3 || got[0].Role != RoleSystem || MessageText(got[1]) != "keep" || got[2].Role != RoleUser {
		t.Fatalf("messages = %#v", got)
	}
	if len(messages) != 4 {
		t.Fatalf("input mutated: %#v", messages)
	}
}

func TestInsertConversationStartPreservesEarliestAndDeduplicates(t *testing.T) {
	first := ConversationStartMessage(time.Date(2026, time.January, 1, 1, 2, 0, 0, time.UTC))
	later := ConversationStartMessage(time.Date(2026, time.February, 2, 2, 3, 0, 0, time.UTC))
	destination := []Message{SystemText("system"), later, UserText("summary")}
	got := InsertConversationStart(destination, []Message{first, later})
	if len(got) != len(destination) || MessageText(got[1]) != MessageText(later) {
		t.Fatalf("existing destination marker was not preserved: %#v", got)
	}

	got = InsertConversationStart([]Message{SystemText("system"), UserText("summary")}, []Message{first, later})
	if len(got) != 3 || MessageText(got[1]) != MessageText(first) {
		t.Fatalf("earliest source marker not inserted: %#v", got)
	}
}
