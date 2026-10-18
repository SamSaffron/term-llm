package clibin

import "github.com/samsaffron/term-llm/internal/llm/core"

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsModelID(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

type inlineFlusher interface {
	RequestInlineFlush()
	SupportsInlineFlush() bool
}

type immediateInterrupter interface {
	SupportsImmediateInterruption() bool
}

// forkHelperAtBoundary mirrors llm.ForkHelperAtBoundary for providers that
// implement the boundary-fork seam.
func forkHelperAtBoundary(provider core.Provider, state []byte, requestMessages []core.Message) (core.Provider, bool) {
	if provider == nil || len(state) == 0 || len(requestMessages) == 0 {
		return nil, false
	}
	forker, ok := provider.(interface {
		ForkConversationAtBoundary(state []byte, requestMessages []core.Message) (core.Provider, bool)
	})
	if !ok {
		return nil, false
	}
	forked, ok := forker.ForkConversationAtBoundary(state, requestMessages)
	if !ok || forked == nil {
		return nil, false
	}
	return forked, true
}
