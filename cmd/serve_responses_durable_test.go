package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestValidateDurableContinuationInput(t *testing.T) {
	tests := []struct {
		name     string
		messages []llm.Message
		wantErr  bool
	}{
		{name: "one user", messages: []llm.Message{{Role: llm.RoleUser}}},
		{name: "one tool", messages: []llm.Message{{Role: llm.RoleTool}}},
		{name: "multiple tools", messages: []llm.Message{{Role: llm.RoleTool}, {Role: llm.RoleTool}}},
		{name: "empty", wantErr: true},
		{name: "multiple users", messages: []llm.Message{{Role: llm.RoleUser}, {Role: llm.RoleUser}}, wantErr: true},
		{name: "mixed user and tool", messages: []llm.Message{{Role: llm.RoleUser}, {Role: llm.RoleTool}}, wantErr: true},
		{name: "unsupported role", messages: []llm.Message{{Role: llm.RoleAssistant}}, wantErr: true},
		{name: "tools and unsupported role", messages: []llm.Message{{Role: llm.RoleTool}, {Role: llm.RoleDeveloper}}, wantErr: true},
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
	messages := []llm.Message{
		{Role: llm.RoleUser, ClientMessageID: "msg-first"},
		{Role: llm.RoleUser, ClientMessageID: "msg-second"},
	}
	if err := validateDurableContinuationInput(messages, true); err != nil {
		t.Fatalf("identified batch rejected: %v", err)
	}
	if err := validateDurableContinuationInput(messages, false); err == nil {
		t.Fatal("unflagged multiple-user continuation must remain rejected")
	}
}
