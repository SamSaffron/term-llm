package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestApplyChatSearchDefaultEnablesNewBareChats(t *testing.T) {
	settings := SessionSettings{}

	applyChatSearchDefault(&settings, false, nil, false)

	if !settings.Search {
		t.Fatal("Search = false, want true for new bare interactive chats by default")
	}
}

func TestApplyChatSearchDefaultHonorsAgentSearchSetting(t *testing.T) {
	settings := SessionSettings{}

	applyChatSearchDefault(&settings, false, nil, true)

	if settings.Search {
		t.Fatal("Search = true, want false for agents that did not enable search")
	}
}

func TestApplyChatSearchDefaultHonorsNoSearch(t *testing.T) {
	settings := SessionSettings{}

	applyChatSearchDefault(&settings, true, nil, false)

	if settings.Search {
		t.Fatal("Search = true, want false with --no-search")
	}
}

func TestResolveSettingsNoSearchDisablesAgentSearch(t *testing.T) {
	settings, err := ResolveSettings(&config.Config{}, &agents.Agent{Search: true}, CLIFlags{NoSearch: true}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.Search {
		t.Fatal("Search = true, want false when --no-search overrides agent search")
	}
}
