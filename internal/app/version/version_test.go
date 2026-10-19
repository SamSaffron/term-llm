package version

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

func TestRun(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	options := Options{Version: "v1", Commit: "abc", Date: "today"}
	options.Run(cmd, nil)
	if got, want := out.String(), "term-llm version v1 (commit: abc, built: today)\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
