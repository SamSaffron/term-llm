package llm

// helperConversationIsolator is implemented by providers that must clone
// provider-local conversation state before a one-shot helper request.
type helperConversationIsolator interface {
	isolateHelperConversation() Provider
}

// helperConversationForker is implemented by providers that can branch from
// their current conversation without mutating the live provider state.
//
// This is the suffix-only contract: the branch resumes the provider's session
// and delivers only the messages handed to the clone, so the caller's request
// carries no parent transcript at all. Handover depends on it.
type helperConversationForker interface {
	forkHelperConversation() (Provider, bool)
}

// HelperBoundaryForker is implemented by providers that can branch from a
// previously captured conversation boundary rather than their live in-memory
// state. It is exported because the callers that need it live outside this
// package; they must still go through ForkHelperAtBoundary.
//
// This is the boundary-offset contract, and it is deliberately not the same as
// helperConversationForker: the branch treats requestMessages[:MessagesSent] as
// already delivered to the resumed session, per the captured state. Merging the
// two would strip handover's context, because importing a parent offset into a
// short suffix-only request invalidates the resume boundary.
//
// Implementations must validate the captured state against requestMessages and
// refuse rather than let the branch discover a mismatch itself: a provider's own
// recovery is an untrimmed replay of the full request, which is exactly what the
// caller's bounded fallback exists to avoid.
type HelperBoundaryForker interface {
	ForkConversationAtBoundary(state []byte, requestMessages []Message) (Provider, bool)
}

func isolatedConversationProvider(provider Provider) Provider {
	if isolator, ok := provider.(helperConversationIsolator); ok {
		if isolated := isolator.isolateHelperConversation(); isolated != nil {
			return isolated
		}
	}
	return provider
}

// ForkHelperAtBoundary branches provider from captured conversation state so a
// helper turn can reuse the provider's own session instead of replaying the
// transcript. requestMessages is the full request the branch will be given; the
// provider derives what still has to cross the wire from the captured offset.
//
// It reports false whenever the branch cannot be proven to line up with the
// request, in which case the caller must fall back to a bounded replay.
func ForkHelperAtBoundary(provider Provider, state []byte, requestMessages []Message) (Provider, bool) {
	if provider == nil || len(state) == 0 || len(requestMessages) == 0 {
		return nil, false
	}
	forker, ok := provider.(HelperBoundaryForker)
	if !ok {
		return nil, false
	}
	forked, forkedOK := forker.ForkConversationAtBoundary(state, requestMessages)
	if !forkedOK || forked == nil {
		return nil, false
	}
	return forked, true
}

// SupportsHelperBoundaryFork reports whether provider implements the
// boundary-offset fork contract at all. Eligibility still requires captured
// state that matches the request, which only ForkHelperAtBoundary can decide.
func SupportsHelperBoundaryFork(provider Provider) bool {
	_, ok := provider.(HelperBoundaryForker)
	return ok
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
