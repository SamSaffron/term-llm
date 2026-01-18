package ui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// WaveTickMsg is sent to advance the wave animation
type WaveTickMsg struct{}

// WavePauseMsg is sent when wave pause ends
type WavePauseMsg struct{}

// ToolTracker manages tool segment state and wave animation.
// Designed to be embedded in larger models (ask, chat) for consistent tool tracking.
type ToolTracker struct {
	Segments     []Segment
	WavePos      int
	WavePaused   bool
	LastActivity time.Time
}

// NewToolTracker creates a new ToolTracker
func NewToolTracker() *ToolTracker {
	return &ToolTracker{
		Segments:     make([]Segment, 0),
		LastActivity: time.Now(),
	}
}

// RecordActivity records the current time as the last activity.
// Call this when text is received, tools start, or tools end.
func (t *ToolTracker) RecordActivity() {
	t.LastActivity = time.Now()
}

// IsIdle returns true if there has been no activity for the given duration
// and there are no pending tools (tools have their own wave animation).
func (t *ToolTracker) IsIdle(d time.Duration) bool {
	return time.Since(t.LastActivity) > d && !t.HasPending()
}

// HandleToolStart adds a pending segment for this tool call.
// Uses the unique callID to track this specific invocation.
// Returns true if a new segment was added (caller should start wave animation).
func (t *ToolTracker) HandleToolStart(callID, toolName, toolInfo string) bool {
	t.RecordActivity()

	// Check if we already have a pending segment for this call ID
	for i := len(t.Segments) - 1; i >= 0; i-- {
		seg := t.Segments[i]
		if seg.Type == SegmentTool &&
			seg.ToolStatus == ToolPending &&
			seg.ToolCallID == callID {
			return false
		}
	}

	// Add new pending segment
	t.Segments = append(t.Segments, Segment{
		Type:       SegmentTool,
		ToolCallID: callID,
		ToolName:   toolName,
		ToolInfo:   toolInfo,
		ToolStatus: ToolPending,
	})
	return true
}

// HandleToolEnd updates the status of a pending tool by its call ID.
func (t *ToolTracker) HandleToolEnd(callID string, success bool) {
	t.RecordActivity()
	t.Segments = UpdateToolStatus(t.Segments, callID, success)
}

// HasPending returns true if there are any pending tool segments.
func (t *ToolTracker) HasPending() bool {
	return HasPendingTool(t.Segments)
}

// StartWave initializes and starts the wave animation.
// Returns the command to start the tick cycle.
func (t *ToolTracker) StartWave() tea.Cmd {
	t.WavePos = 0
	t.WavePaused = false
	return t.waveTickCmd()
}

// HandleWaveTick advances the wave animation.
// Returns the next command (tick, pause, or nil if no pending tools).
func (t *ToolTracker) HandleWaveTick() tea.Cmd {
	if !t.HasPending() || t.WavePaused {
		return nil
	}

	toolTextLen := GetPendingToolTextLen(t.Segments)
	t.WavePos++

	if t.WavePos >= toolTextLen {
		// Wave complete, start pause
		t.WavePaused = true
		t.WavePos = -1
		return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
			return WavePauseMsg{}
		})
	}

	return t.waveTickCmd()
}

// HandleWavePause handles the end of a wave pause.
// Returns the next tick command if there are still pending tools.
func (t *ToolTracker) HandleWavePause() tea.Cmd {
	if !t.HasPending() {
		return nil
	}

	t.WavePaused = false
	t.WavePos = 0
	return t.waveTickCmd()
}

// waveTickCmd returns the command for the next wave tick.
func (t *ToolTracker) waveTickCmd() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg {
		return WaveTickMsg{}
	})
}

// ActiveSegments returns only the pending tool segments (for rendering).
func (t *ToolTracker) ActiveSegments() []Segment {
	var active []Segment
	for _, s := range t.Segments {
		if s.Type == SegmentTool && s.ToolStatus == ToolPending {
			active = append(active, s)
		}
	}
	return active
}

// CompletedSegments returns all non-pending segments (for rendering).
func (t *ToolTracker) CompletedSegments() []Segment {
	var completed []Segment
	for _, s := range t.Segments {
		if !(s.Type == SegmentTool && s.ToolStatus == ToolPending) {
			completed = append(completed, s)
		}
	}
	return completed
}

// AddTextSegment adds or appends to a text segment.
// Returns true if this created a new segment.
func (t *ToolTracker) AddTextSegment(text string) bool {
	t.RecordActivity()

	// Find or create current text segment
	if len(t.Segments) > 0 {
		last := &t.Segments[len(t.Segments)-1]
		if last.Type == SegmentText && !last.Complete {
			last.Text += text
			return false
		}
	}

	// Create new text segment
	t.Segments = append(t.Segments, Segment{
		Type: SegmentText,
		Text: text,
	})
	return true
}

// CompleteTextSegments marks all incomplete text segments as complete.
func (t *ToolTracker) CompleteTextSegments(renderFunc func(string) string) {
	for i := range t.Segments {
		if t.Segments[i].Type == SegmentText && !t.Segments[i].Complete {
			t.Segments[i].Complete = true
			if t.Segments[i].Text != "" && renderFunc != nil {
				t.Segments[i].Rendered = renderFunc(t.Segments[i].Text)
			}
		}
	}
}

// MarkCurrentTextComplete marks the current text segment as complete before a tool starts.
func (t *ToolTracker) MarkCurrentTextComplete(renderFunc func(string) string) {
	if len(t.Segments) > 0 {
		last := &t.Segments[len(t.Segments)-1]
		if last.Type == SegmentText && !last.Complete {
			last.Complete = true
			if last.Text != "" && renderFunc != nil {
				last.Rendered = renderFunc(last.Text)
			}
		}
	}
}

// FlushToScrollbackResult contains the result of a scrollback flush operation.
type FlushToScrollbackResult struct {
	// ToPrint is the content to print to scrollback (empty if nothing to flush)
	ToPrint string
	// NewPrintedLines is the updated count of lines printed to scrollback
	NewPrintedLines int
}

// FlushToScrollback checks if content exceeds maxViewLines and returns content
// to print to scrollback, keeping View() small to avoid terminal scroll issues.
// Returns the content to print (if any) and the new printedLines value.
//
// Parameters:
//   - width: terminal width for rendering
//   - printedLines: number of lines already printed to scrollback
//   - maxViewLines: maximum lines to keep in View()
//   - renderMd: markdown render function (text, width) -> rendered
func (t *ToolTracker) FlushToScrollback(
	width int,
	printedLines int,
	maxViewLines int,
	renderMd func(string, int) string,
) FlushToScrollbackResult {
	// Render current completed content
	completed := t.CompletedSegments()
	content := RenderSegments(completed, width, -1, renderMd)
	totalLines := countLines(content)

	// If content exceeds threshold, calculate what to print
	if totalLines > maxViewLines+printedLines {
		lines := splitLines(content)
		splitAt := len(lines) - maxViewLines
		if splitAt > printedLines {
			toPrint := joinLines(lines[printedLines:splitAt])
			return FlushToScrollbackResult{
				ToPrint:         toPrint,
				NewPrintedLines: splitAt,
			}
		}
	}

	return FlushToScrollbackResult{
		NewPrintedLines: printedLines,
	}
}

// FlushAllRemaining returns any remaining content that hasn't been printed to scrollback.
// Use this at the end of streaming to ensure all content is visible.
func (t *ToolTracker) FlushAllRemaining(
	width int,
	printedLines int,
	renderMd func(string, int) string,
) FlushToScrollbackResult {
	completed := t.CompletedSegments()
	content := RenderSegments(completed, width, -1, renderMd)

	if content != "" {
		lines := splitLines(content)
		if printedLines < len(lines) {
			remaining := joinLines(lines[printedLines:])
			return FlushToScrollbackResult{
				ToPrint:         remaining,
				NewPrintedLines: len(lines),
			}
		}
	}

	return FlushToScrollbackResult{
		NewPrintedLines: printedLines,
	}
}

// countLines counts the number of newlines in content
func countLines(content string) int {
	count := 0
	for _, c := range content {
		if c == '\n' {
			count++
		}
	}
	return count
}

// splitLines splits content by newlines (similar to strings.Split but handles edge cases)
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	var lines []string
	start := 0
	for i, c := range content {
		if c == '\n' {
			lines = append(lines, content[start:i])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

// joinLines joins lines with newlines
func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}
