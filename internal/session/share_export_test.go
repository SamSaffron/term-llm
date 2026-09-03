package session

import (
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func shareTestMessage(id int64, sequence int, role llm.Role, text, responseID string) Message {
	var value llm.Message
	switch role {
	case llm.RoleUser:
		value = llm.UserText(text)
	case llm.RoleAssistant:
		value = llm.AssistantText(text)
	default:
		value = llm.Message{Role: role, Parts: []llm.Part{{Type: llm.PartText, Text: text}}}
	}
	return Message{
		ID: id, SessionID: "share-session", Role: role, Parts: value.Parts,
		TextContent: text, Sequence: sequence, ResponseID: responseID,
	}
}

func TestSelectShareMessagesResponseAggregatesOnlyAnchoredResponse(t *testing.T) {
	messages := []Message{
		shareTestMessage(1, 1, llm.RoleUser, "private prompt", ""),
		shareTestMessage(2, 2, llm.RoleAssistant, "first", "response-a"),
		shareTestMessage(3, 3, llm.RoleTool, "private tool output", ""),
		shareTestMessage(4, 4, llm.RoleAssistant, "second", "response-a"),
		shareTestMessage(5, 5, llm.RoleAssistant, "other response", "response-b"),
	}
	selected, err := SelectShareMessages(messages, 4, ShareScopeResponse)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].TextContent != "first\n\nsecond" {
		t.Fatalf("selected = %#v", selected)
	}
	joined := selected[0].TextContent
	if strings.Contains(joined, "prompt") || strings.Contains(joined, "tool") {
		t.Fatalf("response leaked context: %q", joined)
	}
	if len(selected[0].Parts) != 1 || selected[0].Parts[0].Text != joined {
		t.Fatalf("response parts were not reduced to display text: %#v", selected[0].Parts)
	}
}

func TestSelectShareMessagesLegacyResponseStopsAtUser(t *testing.T) {
	messages := []Message{
		shareTestMessage(1, 1, llm.RoleAssistant, "old answer", ""),
		shareTestMessage(2, 2, llm.RoleUser, "next question", ""),
		shareTestMessage(3, 3, llm.RoleAssistant, "part one", ""),
		shareTestMessage(4, 4, llm.RoleAssistant, "part two", ""),
	}
	selected, err := SelectShareMessages(messages, 4, ShareScopeResponse)
	if err != nil {
		t.Fatal(err)
	}
	if got := selected[0].TextContent; got != "part one\n\npart two" {
		t.Fatalf("response = %q", got)
	}
}

func TestSelectShareMessagesConversationEndsAtAnchorAndFiltersInternalRows(t *testing.T) {
	messages := []Message{
		shareTestMessage(1, 1, llm.RoleUser, "question", ""),
		shareTestMessage(2, 2, llm.RoleAssistant, "answer", "response-a"),
		shareTestMessage(3, 3, llm.RoleUser, "later", ""),
	}
	messages = append(messages, shareTestMessage(4, 0, llm.RoleAssistant, "retained duplicate", ""))
	messages[3].CompactionTail = true
	selected, err := SelectShareMessages(messages, 2, ShareScopeConversation)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].ID != 1 || selected[1].ID != 2 {
		t.Fatalf("selected = %#v", selected)
	}
}

func TestSelectShareMessagesRejectsInvalidScopeAndAnchor(t *testing.T) {
	messages := []Message{shareTestMessage(1, 1, llm.RoleUser, "question", "")}
	if _, err := SelectShareMessages(messages, 1, ShareScopeResponse); !errors.Is(err, ErrInvalidShareAnchor) {
		t.Fatalf("anchor error = %v", err)
	}
	messages = append(messages, shareTestMessage(2, 2, llm.RoleAssistant, "answer", ""))
	if _, err := SelectShareMessages(messages, 2, ShareScope("other")); !errors.Is(err, ErrInvalidShareScope) {
		t.Fatalf("scope error = %v", err)
	}
}
