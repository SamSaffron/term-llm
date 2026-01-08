package ui

import (
	"fmt"
	"strings"
	"time"
)

// ProgressUpdate represents a progress update during long-running operations.
type ProgressUpdate struct {
	// OutputTokens is the number of tokens generated so far.
	OutputTokens int

	// Status is the current status text (e.g., "editing main.go").
	Status string

	// Milestone is a completed milestone to print above the spinner
	// (e.g., "✓ Found edit for main.go").
	Milestone string

	// Phase is the current phase of the operation (e.g., "Thinking", "Responding").
	// Used to show state transitions in the spinner.
	Phase string
}

// StreamingIndicator renders a consistent streaming status line
type StreamingIndicator struct {
	Spinner    string        // spinner.View() output
	Phase      string        // "Thinking", "Searching", etc.
	Elapsed    time.Duration
	Tokens     int    // 0 = don't show
	Status     string // optional status (e.g., "editing main.go")
	ShowCancel bool   // show "(esc to cancel)"
}

// Render returns the formatted streaming indicator string
func (s StreamingIndicator) Render(styles *Styles) string {
	var b strings.Builder

	b.WriteString(s.Spinner)
	b.WriteString(" ")
	b.WriteString(s.Phase)
	b.WriteString("...")

	if s.Tokens > 0 {
		b.WriteString(fmt.Sprintf(" %d tokens |", s.Tokens))
	}

	b.WriteString(fmt.Sprintf(" %.1fs", s.Elapsed.Seconds()))

	if s.Status != "" {
		b.WriteString(" | ")
		b.WriteString(s.Status)
	}

	if s.ShowCancel {
		b.WriteString(" ")
		b.WriteString(styles.Muted.Render("(esc to cancel)"))
	}

	return b.String()
}
