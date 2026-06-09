package cmd

import "testing"

func TestMemoryConsolidateApplyModeDefaultsToDryRun(t *testing.T) {
	apply, err := memoryConsolidateApplyMode(false, false)
	if err != nil {
		t.Fatalf("memoryConsolidateApplyMode default error = %v", err)
	}
	if apply {
		t.Fatal("memoryConsolidateApplyMode default returned apply=true, want dry-run")
	}
}

func TestMemoryConsolidateApplyModeRequiresNoDryRunConflict(t *testing.T) {
	apply, err := memoryConsolidateApplyMode(false, true)
	if err != nil {
		t.Fatalf("memoryConsolidateApplyMode apply error = %v", err)
	}
	if !apply {
		t.Fatal("memoryConsolidateApplyMode apply returned false")
	}

	if _, err := memoryConsolidateApplyMode(true, true); err == nil {
		t.Fatal("memoryConsolidateApplyMode with --dry-run and --apply expected error")
	}
}
