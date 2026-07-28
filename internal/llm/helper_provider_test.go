package llm

import "testing"

type nilSuccessfulForkProvider struct {
	Provider
}

func (nilSuccessfulForkProvider) forkHelperConversation() (Provider, bool) {
	return nil, true
}

func TestForkConversationProviderRejectsNilSuccessfulFork(t *testing.T) {
	provider := nilSuccessfulForkProvider{Provider: NewMockProvider("live")}
	forked, ok := forkConversationProvider(provider)
	if ok || forked != nil {
		t.Fatalf("forkConversationProvider() = (%#v, %v), want (nil, false)", forked, ok)
	}
}
