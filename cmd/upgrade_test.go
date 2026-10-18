package cmd

import "testing"

func TestUpgradeFlagDefaults(t *testing.T) {
	t.Parallel()
	if got := upgradeCmd.Flags().Lookup("version").DefValue; got != "" {
		t.Fatalf("upgrade --version default = %q, want empty", got)
	}
}
