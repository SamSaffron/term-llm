package cmd

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestSubagentApprovalTranscriptPrefixMarksParentEvidence(t *testing.T) {
	parent := []core.Message{
		core.UserText("please run tests"),
		core.AssistantText("I will delegate"),
	}
	prefix := subagentApprovalTranscriptPrefix(core.ContextWithApprovalTranscript(context.Background(), parent))
	if len(prefix) != 2 {
		t.Fatalf("prefix len = %d, want 2", len(prefix))
	}
	if prefix[0].ApprovalRole != "parent_user" {
		t.Fatalf("first approval role = %q, want parent_user", prefix[0].ApprovalRole)
	}
	if prefix[1].ApprovalRole != "parent_assistant" {
		t.Fatalf("second approval role = %q, want parent_assistant", prefix[1].ApprovalRole)
	}
}
