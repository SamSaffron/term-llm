package cmd

import (
	"testing"
)

// TestAskJSONFlagIsRegistered verifies the --json flag is wired onto askCmd.
func TestAskJSONFlagIsRegistered(t *testing.T) {
	f := askCmd.Flags().Lookup("json")
	if f == nil {
		t.Fatal("expected --json flag to be registered on askCmd")
	}
	if f.Value.Type() != "bool" {
		t.Fatalf("expected --json to be bool, got %s", f.Value.Type())
	}
	if f.DefValue != "false" {
		t.Fatalf("expected --json default = false, got %s", f.DefValue)
	}
}
