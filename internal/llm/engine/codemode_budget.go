package engine

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// codeModeOutputBudget is fixed before an observation drains anything. The
// generic output/compaction projection must never discard queued script text.
type codeModeOutputBudget struct {
	tokens int
	chars  int
	callID string
}

func (e *Engine) codeModeOutputLimit() int {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	limit := e.maxToolOutputChars
	if cc := e.compactionConfig; cc != nil && cc.MaxToolResultChars > 0 && (limit <= 0 || cc.MaxToolResultChars < limit) {
		limit = cc.MaxToolResultChars
	}
	return limit
}

func (b codeModeOutputBudget) textBytes(result core.CodeModeCallResult) int {
	bytes := b.tokens * 4
	if b.chars <= 0 {
		return bytes
	}
	// Reserve all optional flags and the exact sanitized status/audit envelope,
	// plus slack for its separator and a possible elapsed-time digit change.
	result.OutputLimited = true
	result.HasMoreOutput = true
	result.OutputTruncated = true
	raw, err := core.MarshalCodeModeJSON(core.CodeModeEnvelopeFor(result, b.callID))
	if err != nil {
		return 0
	}
	return min(bytes, max(0, b.chars-len(raw)-256))
}
