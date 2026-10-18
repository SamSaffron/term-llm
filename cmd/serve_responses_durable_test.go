package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestValidateDurableContinuationInput(t *testing.T) {
	tests := []struct {
		name     string
		messages []core.Message
		wantErr  bool
	}{
		{name: "one user", messages: []core.Message{{Role: core.RoleUser}}},
		{name: "one tool", messages: []core.Message{{Role: core.RoleTool}}},
		{name: "multiple tools", messages: []core.Message{{Role: core.RoleTool}, {Role: core.RoleTool}}},
		{name: "empty", wantErr: true},
		{name: "multiple users", messages: []core.Message{{Role: core.RoleUser}, {Role: core.RoleUser}}, wantErr: true},
		{name: "mixed user and tool", messages: []core.Message{{Role: core.RoleUser}, {Role: core.RoleTool}}, wantErr: true},
		{name: "unsupported role", messages: []core.Message{{Role: core.RoleAssistant}}, wantErr: true},
		{name: "tools and unsupported role", messages: []core.Message{{Role: core.RoleTool}, {Role: core.RoleDeveloper}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDurableContinuationInput(tt.messages, false)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateDurableContinuationInput_AllowsExplicitIdentifiedUserBatch(t *testing.T) {
	messages := []core.Message{
		{Role: core.RoleUser, ClientMessageID: "msg-first"},
		{Role: core.RoleUser, ClientMessageID: "msg-second"},
	}
	if err := validateDurableContinuationInput(messages, true); err != nil {
		t.Fatalf("identified batch rejected: %v", err)
	}
	if err := validateDurableContinuationInput(messages, false); err == nil {
		t.Fatal("unflagged multiple-user continuation must remain rejected")
	}
}
