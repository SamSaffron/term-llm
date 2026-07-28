package llm

// helperConversationIsolator is implemented by providers that must clone
// provider-local conversation state before a one-shot helper request.
type helperConversationIsolator interface {
	isolateHelperConversation() Provider
}

// helperConversationForker is implemented by providers that can branch from
// their current conversation without mutating the live provider state.
type helperConversationForker interface {
	forkHelperConversation() (Provider, bool)
}

func isolatedConversationProvider(provider Provider) Provider {
	if isolator, ok := provider.(helperConversationIsolator); ok {
		if isolated := isolator.isolateHelperConversation(); isolated != nil {
			return isolated
		}
	}
	return provider
}

func forkConversationProvider(provider Provider) (Provider, bool) {
	if forker, ok := provider.(helperConversationForker); ok {
		forked, forkedOK := forker.forkHelperConversation()
		if forkedOK && forked != nil {
			return forked, true
		}
	}
	return nil, false
}
