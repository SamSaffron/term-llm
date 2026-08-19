package llm

import (
	"strings"
	"testing"
)

func TestEnsureToolCallIDsAreStableAndUniqueAcrossInvocations(t *testing.T) {
	first := ensureToolCallIDs([]ToolCall{{Name: "first"}, {ID: "native-id", Name: "native"}})
	second := ensureToolCallIDs([]ToolCall{{Name: "second"}})

	if !strings.HasPrefix(first[0].ID, "call_") || !strings.HasPrefix(second[0].ID, "call_") {
		t.Fatalf("synthetic IDs = %q and %q, want call_ prefixes", first[0].ID, second[0].ID)
	}
	if first[0].ID == second[0].ID {
		t.Fatalf("synthetic ID was reused across invocations: %q", first[0].ID)
	}
	if first[1].ID != "native-id" {
		t.Fatalf("provider ID changed to %q", first[1].ID)
	}

	originalID := first[0].ID
	ensureToolCallIDs(first)
	if first[0].ID != originalID {
		t.Fatalf("existing synthetic ID changed from %q to %q", originalID, first[0].ID)
	}
}
