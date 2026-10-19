package session

import (
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func shareTestMessage(id int64, sequence int, role core.Role, text, responseID string) Message {
	var value core.Message
	switch role {
	case core.RoleUser:
		value = core.UserText(text)
	case core.RoleAssistant:
		value = core.AssistantText(text)
	default:
		value = core.Message{Role: role, Parts: []core.Part{{Type: core.PartText, Text: text}}}
	}
	return Message{
		ID: id, SessionID: "share-session", Role: role, Parts: value.Parts,
		TextContent: text, Sequence: sequence, ResponseID: responseID,
	}
}

func TestSelectShareMessagesResponseAggregatesOnlyAnchoredResponse(t *testing.T) {
	messages := []Message{
		shareTestMessage(1, 1, core.RoleUser, "private prompt", ""),
		shareTestMessage(2, 2, core.RoleAssistant, "first", "response-a"),
		shareTestMessage(3, 3, core.RoleTool, "private tool output", ""),
		shareTestMessage(4, 4, core.RoleAssistant, "second", "response-a"),
		shareTestMessage(5, 5, core.RoleAssistant, "other response", "response-b"),
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
		shareTestMessage(1, 1, core.RoleAssistant, "old answer", ""),
		shareTestMessage(2, 2, core.RoleUser, "next question", ""),
		shareTestMessage(3, 3, core.RoleAssistant, "part one", ""),
		shareTestMessage(4, 4, core.RoleAssistant, "part two", ""),
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
		shareTestMessage(1, 1, core.RoleUser, "question", ""),
		shareTestMessage(2, 2, core.RoleAssistant, "answer", "response-a"),
		shareTestMessage(3, 3, core.RoleUser, "later", ""),
	}
	messages = append(messages, shareTestMessage(4, 0, core.RoleAssistant, "retained duplicate", ""))
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
	messages := []Message{shareTestMessage(1, 1, core.RoleUser, "question", "")}
	if _, err := SelectShareMessages(messages, 1, ShareScopeResponse); !errors.Is(err, ErrInvalidShareAnchor) {
		t.Fatalf("anchor error = %v", err)
	}
	messages = append(messages, shareTestMessage(2, 2, core.RoleAssistant, "answer", ""))
	if _, err := SelectShareMessages(messages, 2, ShareScope("other")); !errors.Is(err, ErrInvalidShareScope) {
		t.Fatalf("scope error = %v", err)
	}
}
