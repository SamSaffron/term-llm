package cmd

import (
	"testing"
)

func TestEnsureRequestSessionID_GeneratesWithoutPersistence(t *testing.T) {
	got := EnsureRequestSessionID("", false)
	if got == "" {
		t.Fatal("expected non-empty request session ID")
	}
}

func TestEnsureRequestSessionID_PreservesExistingID(t *testing.T) {
	const existing = "sess-existing"
	if got := EnsureRequestSessionID(existing, false); got != existing {
		t.Fatalf("ensureRequestSessionID() = %q, want %q", got, existing)
	}
}

func TestEnsureRequestSessionID_DoesNotInventResumeID(t *testing.T) {
	if got := EnsureRequestSessionID("", true); got != "" {
		t.Fatalf("ensureRequestSessionID() = %q, want empty string for resume without session", got)
	}
}
