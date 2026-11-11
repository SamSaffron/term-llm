package engine

import (
	"strings"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// retainTextLocked keeps the final unread text tail without moving text across
// images. Observed output has already left parts and is never replaced.
func (a *codeModeAsyncCell) retainTextLocked(text string) {
	a.parts = append(a.parts, core.ToolContentPart{Type: core.ToolContentPartText, Text: text})
	a.pendingBytes += len(text)
	excess := a.pendingBytes - codemode.SessionLimits.RetainBytes
	if excess <= 0 {
		return
	}
	a.truncated = true
	keep := a.parts[:0]
	for _, part := range a.parts {
		if excess > 0 && part.Type == core.ToolContentPartText {
			cut := min(excess, len(part.Text))
			for cut < len(part.Text) && !utf8.RuneStart(part.Text[cut]) {
				cut++
			}
			part.Text = strings.Clone(part.Text[cut:])
			a.pendingBytes -= cut
			a.omittedBytes += cut
			excess -= cut
			if part.Text == "" {
				continue
			}
		}
		keep = append(keep, part)
	}
	// Do not retain removed text/image references in the backing array.
	clear(a.parts[len(keep):])
	a.parts = keep
}

// evictTerminal prefers fully observed, drained receipts; ties remain FIFO.
// The caller holds the session lock, as in lookup and teardown.
func (o *codeModeSessionCells) evictTerminal() {
	index := 0
	for i, cell := range o.terminal {
		cell.mu.Lock()
		drained := cell.observed && len(cell.parts) == 0
		cell.mu.Unlock()
		if drained {
			index = i
			break
		}
	}
	o.terminal[index].dropRetained()
	copy(o.terminal[index:], o.terminal[index+1:])
	o.terminal[len(o.terminal)-1] = nil
	o.terminal = o.terminal[:len(o.terminal)-1]
}
