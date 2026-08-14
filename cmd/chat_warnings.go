package cmd

import (
	"io"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

// tuiWarningWriter routes background warnings raised while the chat TUI owns
// the terminal into the TUI itself.
//
// Subsystems such as the session store's LoggingStore report failures by
// writing to an io.Writer. Pointing that at stderr is correct for one-shot
// commands but corrupts the chat TUI: Bubble Tea owns the alt screen, so the
// bytes land wherever the cursor happens to be — typically straight across the
// prompt line — and survive until the next full repaint.
//
// Before the program starts (and after it exits) writes fall through to the
// fallback writer so startup and shutdown diagnostics are still visible.
type tuiWarningWriter struct {
	mu       sync.Mutex
	fallback io.Writer
	notify   func(string)
}

func newTUIWarningWriter(fallback io.Writer) *tuiWarningWriter {
	return &tuiWarningWriter{fallback: fallback}
}

// attach starts routing warnings to the running program.
func (w *tuiWarningWriter) attach(p *tea.Program) {
	if w == nil || p == nil {
		return
	}
	w.attachNotifier(func(text string) {
		p.Send(chat.FooterNoticeMsg{Text: text})
	})
}

func (w *tuiWarningWriter) attachNotifier(notify func(string)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.notify = notify
	w.mu.Unlock()
}

// detach restores fallback delivery once the program is no longer rendering.
func (w *tuiWarningWriter) detach() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.notify = nil
	w.mu.Unlock()
}

func (w *tuiWarningWriter) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	w.mu.Lock()
	notify := w.notify
	fallback := w.fallback
	w.mu.Unlock()

	if notify == nil {
		if fallback == nil {
			return len(p), nil
		}
		return fallback.Write(p)
	}
	if text := strings.TrimSpace(string(p)); text != "" {
		notify(text)
	}
	return len(p), nil
}
