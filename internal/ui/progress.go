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
	Segments       []*Segment               // live tool segments for wave/status rendering
	WavePos        int                      // current wave position
	Width          int                      // terminal width for markdown rendering
	RenderMarkdown func(string, int) string // markdown renderer for text segments
	ToolsExpanded  bool                     // render tool calls in expanded mode

	// Flush state for leading spacing
	HasFlushed      bool
	LastFlushedType SegmentType
}

// Render returns the formatted streaming indicator string
func (s StreamingIndicator) Render(styles *Styles) string {
	var b strings.Builder

	// Render active tools if any
	if len(s.Segments) > 0 {
		if s.HasFlushed {
			b.WriteString(FlushSegmentSeparator(s.LastFlushedType, s.Segments[0].Type))
		}
		b.WriteString(RenderSegments(s.Segments, s.Width, s.WavePos, s.RenderMarkdown, false, s.ToolsExpanded))
		// When tools are active, we don't show the spinner/phase line
		// as the wave animation provides the progress feedback.
	} else if !s.HideProgress {
		// Keep the idle spinner/status line compact after flushed content.
		b.WriteString(s.Spinner)
		b.WriteString(" ")
		b.WriteString(s.Phase)
	}

	// Show tokens and time during spinner phase only (not during tool execution)
	if len(s.Segments) == 0 && !s.HideProgress {
		b.WriteString(" ")
		if s.Tokens > 0 {
			b.WriteString(fmt.Sprintf("%s tokens | ", FormatTokenCount(s.Tokens)))
		}
		b.WriteString(FormatElapsedDuration(s.Elapsed))
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

// FormatElapsedDuration formats an elapsed duration for compact progress displays.
func FormatElapsedDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}

	totalSeconds := int64(d.Round(time.Second) / time.Second)
	if totalSeconds <= 0 {
		return "0s"
	}

	seconds := totalSeconds % 60
	totalMinutes := totalSeconds / 60
	minutes := totalMinutes % 60
	totalHours := totalMinutes / 60
	hours := totalHours % 24
	days := totalHours / 24

	if days > 0 {
		return fmt.Sprintf("%dd%02dh%02dm%02ds", days, hours, minutes, seconds)
	}
	if totalHours > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", totalHours, minutes, seconds)
	}
	if totalMinutes > 0 {
		return fmt.Sprintf("%dm%02ds", totalMinutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
