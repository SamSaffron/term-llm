package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpShowsExamplesAtBottom(t *testing.T) {
	var output bytes.Buffer
	oldOutput := rootCmd.OutOrStdout()
	rootCmd.SetOut(&output)
	t.Cleanup(func() { rootCmd.SetOut(oldOutput) })

	if err := rootCmd.Help(); err != nil {
		t.Fatalf("rootCmd.Help() error = %v", err)
	}

	help := output.String()
	examplesHeading := "Examples:\n"
	if got := strings.Count(help, examplesHeading); got != 1 {
		t.Fatalf("Examples heading count = %d, want 1\nhelp:\n%s", got, help)
	}
	if examplesAt, flagsAt := strings.Index(help, examplesHeading), strings.Index(help, "Flags:\n"); examplesAt < flagsAt {
		t.Fatalf("Examples appear before flags\nhelp:\n%s", help)
	}
	if !strings.HasSuffix(help, examplesHeading+rootExamples+"\n") {
		t.Fatalf("Examples are not the final help section\nhelp:\n%s", help)
	}
}
