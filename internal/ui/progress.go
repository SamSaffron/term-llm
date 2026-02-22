package ui

import (
	"fmt"
	"strings"
	"time"
)

// FormatTokenCount formats a token count in compact form: 1, 999, 1.5k, 12.3k, 1.1M
func FormatTokenCount(n int) string {
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
	Spinner        string // spinner.View() output
	Phase          string // "Thinking", "Searching", etc.
	Elapsed        time.Duration
	Tokens         int                      // 0 = don't show
	Status         string                   // optional status (e.g., "editing main.go")
	ShowCancel     bool                     // show "(esc to cancel)"
	HideProgress   bool                     // hide spinner/phase/tokens/time (shown in status line instead)
	Segments       []*Segment               // active tool segments for wave animation
	WavePos        int                      // current wave position
	Width          int                      // terminal width for markdown rendering
	RenderMarkdown func(string, int) string // markdown renderer for text segments

	// Flush state for leading spacing
	HasFlushed      bool
	LastFlushedType SegmentType
}

func flushBoundarySeparator(prevType, currType SegmentType) string {
	sep := SegmentSeparator(prevType, currType)
	if len(sep) > 0 && sep[0] == '\n' {
		return sep[1:]
	}
	return sep
}

// Render returns the formatted streaming indicator string
func (s StreamingIndicator) Render(styles *Styles) string {
	var b strings.Builder

	// Render active tools if any
	if len(s.Segments) > 0 {
		if s.HasFlushed {
			b.WriteString(flushBoundarySeparator(s.LastFlushedType, s.Segments[0].Type))
		}
		b.WriteString(RenderSegments(s.Segments, s.Width, s.WavePos, s.RenderMarkdown, false, false))
		// When tools are active, we don't show the spinner/phase line
		// as the wave animation provides the progress feedback.
	} else if !s.HideProgress {
		// No active tools, show leading spacing for the standard spinner and phase if needed
		if s.HasFlushed {
			// Spinner/Phase line is treated as a tool/status line for spacing
			b.WriteString(flushBoundarySeparator(s.LastFlushedType, SegmentTool))
		}

		b.WriteString(s.Spinner)
		b.WriteString(" ")
		b.WriteString(s.Phase)
		b.WriteString("...")
	}

	// Show tokens and time during spinner phase only (not during tool execution)
	if len(s.Segments) == 0 && !s.HideProgress {
		b.WriteString(" ")
		if s.Tokens > 0 {
			b.WriteString(fmt.Sprintf("%s tokens | ", FormatTokenCount(s.Tokens)))
		}
		b.WriteString(fmt.Sprintf("%.1fs", s.Elapsed.Seconds()))
	}

	if s.Status != "" {
		if b.Len() > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(s.Status)
	}

	if s.ShowCancel && !s.HideProgress {
		b.WriteString(" ")
		b.WriteString(styles.Muted.Render("(esc to cancel)"))
	}

	return b.String()
}
