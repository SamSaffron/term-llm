package memory

import "context"

// InsightsExpander wraps a Store and exposes a single Expand method for
// injecting behavioral insights at conversation start. A nil *InsightsExpander
// is always a safe no-op — callers never need to nil-check before calling.
type InsightsExpander struct {
	store     *Store
	agent     string
	maxTokens int
}

// NewInsightsExpander returns an expander configured for the given agent.
// maxTokens is the token budget for the injected block (0 → default 500).
// Returns nil when store is nil — the nil receiver is safe to call.
func NewInsightsExpander(store *Store, agent string, maxTokens int) *InsightsExpander {
	if store == nil {
		return nil
	}
	if maxTokens <= 0 {
		maxTokens = 500
	}
	return &InsightsExpander{store: store, agent: agent, maxTokens: maxTokens}
}

// Expand searches the insight bank using userText as the query and returns
// the formatted block ready to inject as a user message.
// Returns "" when disabled, no matches, or on any error.
func (e *InsightsExpander) Expand(ctx context.Context, userText string) string {
	if e == nil || e.store == nil {
		return ""
	}
	expanded, err := e.store.ExpandInsights(ctx, e.agent, userText, e.maxTokens)
	if err != nil {
		return ""
	}
	return expanded
}
