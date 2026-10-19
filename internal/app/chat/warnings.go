package chat

import (
	"io"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
	tuichat "github.com/samsaffron/term-llm/internal/tui/chat"
)

// TUIWarningWriter routes background warnings raised while the chat TUI owns
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
type TUIWarningWriter struct {
	mu       sync.Mutex
	fallback io.Writer
	notify   func(string)
	last     string
}

// Child-run progress (MCP ready/start and approval status) is not a footer
// alert. Actual failures are returned via the child run result.
func ChildProgressWriter() io.Writer { return runtimeoutput.Writer() }

func newTUIWarningWriter(fallback io.Writer) *TUIWarningWriter {
	return &TUIWarningWriter{fallback: fallback}
}

// NewSessionWarningWriter forwards only build-time warnings to the
// preceding session. Long-lived writers must not retain the preceding session:
// adopted background runs can write after that session (and the UI) closes.
func NewSessionWarningWriter(commandWriter, buildWriter io.Writer) (*TUIWarningWriter, func()) {
	durable := runtimeoutput.Fallback(commandWriter)
	writer := newTUIWarningWriter(durable)
	if buildWriter == nil {
		return writer, func() {}
	}
	// Build runs synchronously while the preceding session still owns the UI.
	// Do not wrap this temporary route in Fallback: it must reach its footer.
	writer.setFallback(buildWriter)
	return writer, func() { writer.setFallback(durable) }
}

func (w *TUIWarningWriter) setFallback(fallback io.Writer) {
	w.mu.Lock()
	w.fallback = fallback
	w.mu.Unlock()
}

// Attach starts routing warnings to the running program.
func (w *TUIWarningWriter) Attach(p *tea.Program) {
	if w == nil || p == nil {
		return
	}
	w.attachNotifier(func(text string) {
		p.Send(tuichat.FooterNoticeMsg{Text: text})
	})
}

func (w *TUIWarningWriter) attachNotifier(notify func(string)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.notify = notify
	w.last = ""
	w.mu.Unlock()
}

// Detach restores fallback delivery once the program is no longer rendering.
func (w *TUIWarningWriter) Detach() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.notify = nil
	w.mu.Unlock()
}

func (w *TUIWarningWriter) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	w.mu.Lock()
	notify := w.notify
	fallback := w.fallback
	text := strings.TrimSpace(string(p))
	duplicate := notify != nil && text == w.last
	if notify != nil && text != "" && !duplicate {
		w.last = text
	}
	w.mu.Unlock()

	if notify == nil {
		if fallback == nil {
			return len(p), nil
		}
		return fallback.Write(p)
	}
	if text != "" && !duplicate {
		notify(text)
	}
	return len(p), nil
}
