package tools

import "testing"

func TestApprovalTTYAvailableUsesInjectedChecker(t *testing.T) {
	old := approvalTTYAvailable
	t.Cleanup(func() { approvalTTYAvailable = old })

	approvalTTYAvailable = func() bool { return false }
	if ApprovalTTYAvailable() {
		t.Fatal("ApprovalTTYAvailable() = true, want false")
	}

	approvalTTYAvailable = func() bool { return true }
	if !ApprovalTTYAvailable() {
		t.Fatal("ApprovalTTYAvailable() = false, want true")
	}
}
