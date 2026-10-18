package cmd

import (
	"github.com/samsaffron/term-llm/internal/config"
	sharepkg "github.com/samsaffron/term-llm/internal/share"
	"github.com/spf13/cobra"
)

// newSessionSharePublisher is a seam so tests can inject a fake provider.
var newSessionSharePublisher = func(cfg config.ShareConfig) (sharepkg.Publisher, error) {
	return sharepkg.NewPublisher(cfg)
}

func runSessionsShare(cmd *cobra.Command, args []string) error {
	return sessionsApp().Share(cmd, args)
}
