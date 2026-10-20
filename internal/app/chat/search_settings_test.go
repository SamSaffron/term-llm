package chat

import (
	"testing"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
)

func TestApplyChatSearchDefaultEnablesNewBareChats(t *testing.T) {
	settings := appserve.SessionSettings{}

	applyChatSearchDefault(&settings, false, nil, false)

	if !settings.Search {
		t.Fatal("Search = false, want true for new bare interactive chats by default")
	}
}

func TestApplyChatSearchDefaultHonorsAgentSearchSetting(t *testing.T) {
	settings := appserve.SessionSettings{}

	applyChatSearchDefault(&settings, false, nil, true)

	if settings.Search {
		t.Fatal("Search = true, want false for agents that did not enable search")
	}
}

func TestApplyChatSearchDefaultHonorsNoSearch(t *testing.T) {
	settings := appserve.SessionSettings{}

	applyChatSearchDefault(&settings, true, nil, false)

	if settings.Search {
		t.Fatal("Search = true, want false with --no-search")
	}
}
