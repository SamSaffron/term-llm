package core

import (
	"reflect"
	"testing"
)

func TestWithTurnContextPolicyPlacement(t *testing.T) {
	policy := Message{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "current context"}}}
	for _, test := range []struct {
		name     string
		messages []Message
		want     []Role
	}{
		{"empty", nil, []Role{RoleDeveloper}},
		{"no user", []Message{SystemText("system"), AssistantText("answer")}, []Role{RoleDeveloper, RoleSystem, RoleAssistant}},
		{"latest user", []Message{SystemText("system"), UserText("first"), AssistantText("answer"), UserText("follow-up")}, []Role{RoleSystem, RoleUser, RoleAssistant, RoleDeveloper, RoleUser}},
		{"trailing tool", []Message{UserText("question"), AssistantText("tool call"), {Role: RoleTool}}, []Role{RoleDeveloper, RoleUser, RoleAssistant, RoleTool}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Spare capacity catches accidental append-based mutation of history.
			backing := make([]Message, len(test.messages)+2)
			copy(backing, test.messages)
			backing[len(test.messages)] = UserText("capacity canary")
			before := append([]Message(nil), backing...)
			got := WithTurnContext(backing[:len(test.messages)], []Message{policy})
			roles := make([]Role, len(got))
			for i := range got {
				roles[i] = got[i].Role
			}
			if !reflect.DeepEqual(roles, test.want) {
				t.Fatalf("roles = %v, want %v", roles, test.want)
			}
			if !reflect.DeepEqual(backing, before) {
				t.Fatal("insertion mutated the source backing array")
			}
		})
	}
}

func TestWithTurnContextAbsent(t *testing.T) {
	messages := []Message{UserText("unchanged")}
	if got := WithTurnContext(messages, nil); &got[0] != &messages[0] {
		t.Fatal("absent context copied the transcript")
	}
	if got := WithTurnContext(nil, nil); got != nil {
		t.Fatalf("nil conversation became %#v", got)
	}
}
