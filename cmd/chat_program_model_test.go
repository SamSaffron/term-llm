package cmd

import (
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type chatProgramTestMsg struct{ value string }

func TestScopeChatProgramCmdTagsApplicationMessage(t *testing.T) {
	message := scopeChatProgramCmd(func() tea.Msg { return chatProgramTestMsg{value: "old"} }, 7)()
	scoped, ok := message.(chatProgramScopedMsg)
	if !ok {
		t.Fatalf("scoped command returned %T", message)
	}
	if scoped.generation != 7 || scoped.message != (chatProgramTestMsg{value: "old"}) {
		t.Fatalf("scoped message = %#v", scoped)
	}
}

func TestScopeChatProgramCmdRecursesThroughBatchAndSequence(t *testing.T) {
	leaf := func(value string) tea.Cmd {
		return func() tea.Msg { return chatProgramTestMsg{value: value} }
	}
	message := scopeChatProgramCmd(tea.Batch(
		leaf("batch"),
		tea.Sequence(leaf("first"), leaf("second")),
	), 3)()
	batch, ok := message.(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("scoped batch = %T len=%d", message, len(batch))
	}
	if _, ok := batch[0]().(chatProgramScopedMsg); !ok {
		t.Fatalf("batch leaf returned %T", batch[0]())
	}

	sequenceMessage := batch[1]()
	sequenceType := ""
	if sequenceMessage != nil {
		sequenceType = typeName(sequenceMessage)
	}
	if sequenceType != "sequenceMsg" {
		t.Fatalf("sequence control message became %T (%s)", sequenceMessage, sequenceType)
	}
}

func TestScopeChatProgramCmdPreservesBubbleTeaControlMessage(t *testing.T) {
	message := scopeChatProgramCmd(tea.Quit, 5)()
	if _, ok := message.(tea.QuitMsg); !ok {
		t.Fatalf("quit command returned %T", message)
	}
}

func typeName(value any) string {
	typeInfo := reflect.TypeOf(value)
	if typeInfo == nil {
		return ""
	}
	return typeInfo.Name()
}
