package cmd

import (
	"bytes"
	"testing"
)

func TestVersionCommandAndFlagMatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "subcommand", args: []string{"version"}},
		{name: "flag", args: []string{"--version"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			rootCmd.SetOut(&stdout)
			rootCmd.SetErr(&stderr)
			rootCmd.SetArgs(tc.args)

			if _, err := rootCmd.ExecuteC(); err != nil {
				t.Fatalf("ExecuteC() error = %v", err)
			}

			got := stdout.String()
			want := "term-llm version " + versionString() + "\n"
			if got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}
