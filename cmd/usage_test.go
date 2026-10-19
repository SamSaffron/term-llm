package cmd

import "testing"

func TestUsageProviderCompletionIncludesAgyBin(t *testing.T) {
	completions, _ := UsageProviderFlagCompletion(nil, nil, "agy")
	if len(completions) != 1 || completions[0] != "agy-bin" {
		t.Fatalf("completions = %v, want [agy-bin]", completions)
	}
}
