package cmd

import (
	"github.com/spf13/cobra"
	"github.com/term-llm/term-llm/internal/config"
	sharepkg "github.com/term-llm/term-llm/internal/share"
)

// newSessionSharePublisher is a seam so tests can inject a fake provider.
var newSessionSharePublisher = func(cfg config.ShareConfig) (sharepkg.Publisher, error) {
	return sharepkg.NewPublisher(cfg)
}

func runSessionsShare(cmd *cobra.Command, args []string) error {
	return sessionsApp().Share(cmd, args)
}
