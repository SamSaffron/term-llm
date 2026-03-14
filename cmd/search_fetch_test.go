package cmd

import "testing"

func TestNoWebFetchFlagRegistration(t *testing.T) {
	cmd := askCmd
	flag := cmd.Flags().Lookup("no-web-fetch")
	if flag == nil {
		t.Fatal("expected --no-web-fetch flag to be registered on ask command")
	}
}
