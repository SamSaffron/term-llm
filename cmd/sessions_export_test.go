package cmd

import (
	"strings"
	"testing"
)

func TestSessionsExportGistHelpDescribesSecretGistsAndGisthost(t *testing.T) {
	help := sessionsExportGistCmd.Long + "\n" + sessionsExportGistCmd.Flags().Lookup("public").Usage
	for _, want := range []string{"secret", "unlisted", "not private", "gisthost"} {
		if !strings.Contains(strings.ToLower(help), want) {
			t.Errorf("gist export help missing %q", want)
		}
	}
}
