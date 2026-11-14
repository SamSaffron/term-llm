package reasoning

import "github.com/term-llm/term-llm/internal/config"

// InteractiveSourceAllows gates transport independently of the browser display preference.
func InteractiveSourceAllows(kind string, cfg config.ReasoningConfig) bool {
	switch NormalizeKind(kind) {
	case KindSummary:
		return SourceAllowsSummary(cfg)
	case KindRaw:
		return SourceAllowsProviderThinking(cfg)
	default:
		return false
	}
}
