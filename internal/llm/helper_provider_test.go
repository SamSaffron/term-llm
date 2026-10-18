package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

type nilSuccessfulForkProvider struct {
	core.Provider
}

func (nilSuccessfulForkProvider) forkHelperConversation() (core.Provider, bool) {
	return nil, true
}

func TestForkConversationProviderRejectsNilSuccessfulFork(t *testing.T) {
	provider := nilSuccessfulForkProvider{Provider: mockprovider.NewMockProvider("live")}
	forked, ok := forkConversationProvider(provider)
	if ok || forked != nil {
		t.Fatalf("forkConversationProvider() = (%#v, %v), want (nil, false)", forked, ok)
	}
}
