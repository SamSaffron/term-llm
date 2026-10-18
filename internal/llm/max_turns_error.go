package llm

import "github.com/samsaffron/term-llm/internal/llm/engine"

// Deprecated: use engine.MaxTurnsExceededError in new packages.
type MaxTurnsExceededError = engine.MaxTurnsExceededError

// IsMaxTurnsExceeded reports whether an error indicates agentic-loop turn exhaustion.
// Deprecated: use engine.IsMaxTurnsExceeded in new packages.
func IsMaxTurnsExceeded(err error) bool { return engine.IsMaxTurnsExceeded(err) }

// MaxTurnsExceededWarning formats the warning emitted before turn exhaustion.
// Deprecated: use engine.MaxTurnsExceededWarning in new packages.
func MaxTurnsExceededWarning(maxTurns int) string { return engine.MaxTurnsExceededWarning(maxTurns) }
