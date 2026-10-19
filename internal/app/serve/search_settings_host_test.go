package serve

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestResolveSettingsNoSearchDisablesAgentSearch(t *testing.T) {
	settings, err := ResolveSettings(&config.Config{}, &agents.Agent{Search: true}, CLIFlags{NoSearch: true}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.Search {
		t.Fatal("Search = true, want false when --no-search overrides agent search")
	}
}
