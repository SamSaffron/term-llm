package tools

import "testing"

func TestLegacyApprovalPromptsFailClosedInCI(t *testing.T) {
	t.Setenv("CI", "1")
	req := &ApprovalRequest{Description: "allow access", Path: "test.txt"}

	if outcome, path := TTYApprovalPrompt(req); outcome != Cancel || path != "" {
		t.Fatalf("TTYApprovalPrompt() = (%v, %q), want cancelled", outcome, path)
	}
	if outcome, path := HuhApprovalPrompt(req); outcome != Cancel || path != "" {
		t.Fatalf("HuhApprovalPrompt() = (%v, %q), want cancelled", outcome, path)
	}
}
