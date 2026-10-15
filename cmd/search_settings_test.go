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

func TestResumedSessionSearch(t *testing.T) {
	tests := []struct {
		name                                     string
		resolved, saved, searchFlagSet, noSearch bool
		want                                     bool
	}{
		{name: "saved search on, no flags", saved: true, want: true},
		{name: "saved search off, no flags", saved: false, want: false},
		{name: "no-search overrides saved search", saved: true, noSearch: true, want: false},
		{name: "no-search wins over search", resolved: true, saved: true, searchFlagSet: true, noSearch: true, want: false},
		{name: "search overrides saved off", resolved: true, saved: false, searchFlagSet: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resumedSessionSearch(tt.resolved, tt.saved, tt.searchFlagSet, tt.noSearch); got != tt.want {
				t.Fatalf("resumedSessionSearch() = %v, want %v", got, tt.want)
			}
		})
	}
}
