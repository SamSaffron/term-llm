package ui

import (
	"fmt"
	"strings"
	"time"
)

// formatTokenCount formats a token count in compact form: 1, 999, 1.5k, 12.3k, 1.1M
func formatTokenCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		k := float64(n) / 1000
		if k < 10 {
			return fmt.Sprintf("%.1fk", k)
		}
		return fmt.Sprintf("%.0fk", k)
	}
	m := float64(n) / 1000000
	if m < 10 {
		return fmt.Sprintf("%.1fM", m)
	}
	return fmt.Sprintf("%.0fM", m)
}

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
	Spinner        string                       // spinner.View() output
	Phase          string                       // "Thinking", "Searching", etc.
	Elapsed        time.Duration
	Tokens         int                          // 0 = don't show
	Status         string                       // optional status (e.g., "editing main.go")
	ShowCancel     bool                         // show "(esc to cancel)"
	Segments       []Segment                    // active tool segments for wave animation
	WavePos        int                          // current wave position
	Width          int                          // terminal width for markdown rendering
	RenderMarkdown func(string, int) string     // markdown renderer for text segments
}

// Render returns the formatted streaming indicator string
func (s StreamingIndicator) Render(styles *Styles) string {
	var b strings.Builder

	// Render active tools if any
	if len(s.Segments) > 0 {
		b.WriteString(RenderSegments(s.Segments, s.Width, s.WavePos, s.RenderMarkdown))
		// When tools are active, we don't show the spinner/phase line
		// as the wave animation provides the progress feedback.
	} else {
		// No active tools, show the standard spinner and phase
		b.WriteString(s.Spinner)
		b.WriteString(" ")
		b.WriteString(s.Phase)
		b.WriteString("...")
	}

	// Show tokens and time during spinner phase only (not during tool execution)
	if len(s.Segments) == 0 {
		b.WriteString(" ")
		if s.Tokens > 0 {
			b.WriteString(fmt.Sprintf("%s tokens | ", formatTokenCount(s.Tokens)))
		}
		b.WriteString(fmt.Sprintf("%.1fs", s.Elapsed.Seconds()))
	}

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
