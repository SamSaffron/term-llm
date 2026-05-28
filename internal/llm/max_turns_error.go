package llm

import (
	"errors"
	"fmt"
	"strings"
)

const maxTurnsExceededErrorText = "agentic loop exceeded max turns"

// MaxTurnsExceededError reports that the agentic loop exhausted its configured
// turn budget before reaching a natural completion.
type MaxTurnsExceededError struct {
	MaxTurns int
}

func (e *MaxTurnsExceededError) Error() string {
	if e != nil && e.MaxTurns > 0 {
		return fmt.Sprintf("%s (%d)", maxTurnsExceededErrorText, e.MaxTurns)
	}
	return maxTurnsExceededErrorText
}

// IsMaxTurnsExceeded reports whether err indicates agentic-loop turn exhaustion.
// It preserves compatibility with older string-only errors while preferring the
// typed error for new call sites.
func IsMaxTurnsExceeded(err error) bool {
	if err == nil {
		return false
	}
	var maxTurnsErr *MaxTurnsExceededError
	if errors.As(err, &maxTurnsErr) {
		return true
	}
	return strings.Contains(err.Error(), maxTurnsExceededErrorText)
}

// MaxTurnsExceededWarning returns the user-facing warning emitted before the
// stream terminates with MaxTurnsExceededError.
func MaxTurnsExceededWarning(maxTurns int) string {
	if maxTurns > 0 {
		turns := "turns"
		if maxTurns == 1 {
			turns = "turn"
		}
		return WarningPhasePrefix + fmt.Sprintf("agent is out of turns after %d %s. Send a follow-up with updated instructions or increase max_turns.", maxTurns, turns)
	}
	return WarningPhasePrefix + "agent is out of turns. Send a follow-up with updated instructions or increase max_turns."
}
