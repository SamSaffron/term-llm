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

	// Check if we already have a segment for this call ID (any status)
	// This prevents duplicate segments if duplicate events arrive
	for i := len(t.Segments) - 1; i >= 0; i-- {
		seg := t.Segments[i]
		if seg.Type == SegmentTool && seg.ToolCallID == callID {
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
// Returns pointers so mutations (like SafeRendered caching) persist.
func (t *ToolTracker) ActiveSegments() []*Segment {
	var active []*Segment
	for i := range t.Segments {
		if t.Segments[i].Type == SegmentTool && t.Segments[i].ToolStatus == ToolPending {
			active = append(active, &t.Segments[i])
		}
	}
	return active
}

// CompletedSegments returns all non-pending, non-flushed segments (for View() rendering).
// Returns pointers so mutations (like SafeRendered caching) persist.
func (t *ToolTracker) CompletedSegments() []*Segment {
	var completed []*Segment
	for i := range t.Segments {
		if t.Segments[i].Flushed {
			continue
		}
		if !(t.Segments[i].Type == SegmentTool && t.Segments[i].ToolStatus == ToolPending) {
			completed = append(completed, &t.Segments[i])
		}
	}
	return completed
}

// AllSegments returns all segments regardless of Flushed status.
// Use for alt screen mode where we render everything in View().
func (t *ToolTracker) AllSegments() []Segment {
	return t.Segments
}

// AddTextSegment adds or appends to a text segment.
// Returns true if this created a new segment.
func (t *ToolTracker) AddTextSegment(text string) bool {
	t.RecordActivity()

	// Find or create current text segment
	if len(t.Segments) > 0 {
		last := &t.Segments[len(t.Segments)-1]
		if last.Type == SegmentText && !last.Complete {
			// Use TextBuilder for O(1) append instead of O(n) string concat
			if last.TextBuilder == nil {
				// Migrate existing Text to TextBuilder (first append after segment creation)
				last.TextBuilder = &strings.Builder{}
				last.TextBuilder.WriteString(last.Text)
				last.Text = "" // Clear to avoid confusion
			}
			last.TextBuilder.WriteString(text)

			// Always update snapshot immediately after WriteString.
			// This ensures GetText() can return the cached snapshot without
			// calling TextBuilder.String() on every render tick.
			// The snapshot update is O(n) but happens once per append rather
			// than potentially multiple times per render cycle.
			//
			// IMPORTANT: We must clone the string because strings.Builder.String()
			// returns a string that shares memory with the internal buffer.
			// Subsequent WriteString calls can corrupt the previously returned
			// string if the buffer has enough capacity (no reallocation).
			currentLen := last.TextBuilder.Len()
			last.TextSnapshot = strings.Clone(last.TextBuilder.String())
			last.TextSnapshotLen = currentLen

			// Update safe boundary periodically (every ~100 chars since last check)
			// Use incremental scanning for O(delta) instead of O(n) complexity
			if currentLen-last.SafePos > 100 {
				// Use incremental boundary finder that only scans from last SafePos
				newSafe := FindSafeBoundaryIncremental(last.TextSnapshot, last.SafePos)
				if newSafe > last.SafePos {
					last.SafePos = newSafe
					last.SafeRendered = "" // Will be populated on next render
				}
			}
			return false
		}
	}

	// Create new text segment with TextBuilder for streaming
	builder := &strings.Builder{}
	builder.WriteString(text)
	t.Segments = append(t.Segments, Segment{
		Type:            SegmentText,
		TextBuilder:     builder,
		TextSnapshot:    text,
		TextSnapshotLen: len(text),
	})
	return true
}

// CompleteTextSegments marks all incomplete text segments as complete.
func (t *ToolTracker) CompleteTextSegments(renderFunc func(string) string) {
	for i := range t.Segments {
		if t.Segments[i].Type == SegmentText && !t.Segments[i].Complete {
			// Finalize TextBuilder to Text
			if t.Segments[i].TextBuilder != nil {
				t.Segments[i].Text = t.Segments[i].TextBuilder.String()
				t.Segments[i].TextBuilder = nil
			}
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
			// Finalize TextBuilder to Text
			if last.TextBuilder != nil {
				last.Text = last.TextBuilder.String()
				last.TextBuilder = nil
			}
			last.Complete = true
			if last.Text != "" && renderFunc != nil {
				last.Rendered = renderFunc(last.Text)
			}
		}
	}
}

// AddExternalUIResult adds a result from external UI (like ask_user) as a completed segment.
// The summary is plain text - styling is applied at render time to avoid ANSI corruption
// when passing through different tea.Program instances.
func (t *ToolTracker) AddExternalUIResult(summary string) {
	if summary == "" {
		return
	}
	t.RecordActivity()
	t.Segments = append(t.Segments, Segment{
		Type:     SegmentAskUserResult,
		Text:     summary,
		Complete: true,
	})
}

// AddImageSegment adds an image segment for inline display.
func (t *ToolTracker) AddImageSegment(path string) {
	if path == "" {
		return
	}
	t.RecordActivity()
	t.Segments = append(t.Segments, Segment{
		Type:      SegmentImage,
		ImagePath: path,
		Complete:  true,
	})
}

// AddDiffSegment adds a diff segment for inline display.
func (t *ToolTracker) AddDiffSegment(path, old, new string) {
	if path == "" {
		return
	}
	t.RecordActivity()
	t.Segments = append(t.Segments, Segment{
		Type:     SegmentDiff,
		DiffPath: path,
		DiffOld:  old,
		DiffNew:  new,
		Complete: true,
	})
}

// FlushToScrollbackResult contains the result of a scrollback flush operation.
type FlushToScrollbackResult struct {
	// ToPrint is the content to print to scrollback (empty if nothing to flush)
	ToPrint string
	// NewPrintedLines is kept for API compatibility but no longer used for tracking
	NewPrintedLines int
}

// FlushToScrollback checks if there are completed segments to flush to scrollback.
// Uses segment-based tracking: completed segments are marked as Flushed and won't
// appear in View() again.
//
// Parameters:
//   - width: terminal width for rendering
//   - printedLines: (unused, kept for API compatibility)
//   - maxViewLines: minimum completed segments to keep unflushed for View()
//   - renderMd: markdown render function (text, width) -> rendered
func (t *ToolTracker) FlushToScrollback(
	width int,
	printedLines int,
	maxViewLines int,
	renderMd func(string, int) string,
) FlushToScrollbackResult {
	// Count unflushed completed segments
	var unflushedCount int
	for i := range t.Segments {
		seg := &t.Segments[i]
		if !seg.Flushed && !(seg.Type == SegmentTool && seg.ToolStatus == ToolPending) {
			unflushedCount++
		}
	}

	// Keep at least 1 segment unflushed for View() (or maxViewLines worth)
	// But always flush images and diffs immediately since they need to go to scrollback
	minKeep := 1
	if unflushedCount <= minKeep {
		// Check if there's an image or diff that needs flushing
		hasUnflushedSpecial := false
		for i := range t.Segments {
			seg := &t.Segments[i]
			if (seg.Type == SegmentImage || seg.Type == SegmentDiff) && !seg.Flushed {
				hasUnflushedSpecial = true
				break
			}
		}
		if !hasUnflushedSpecial {
			return FlushToScrollbackResult{NewPrintedLines: 0}
		}
	}

	// Collect segments to flush (all unflushed completed segments except the last few)
	var toFlush []*Segment
	unflushedSeen := 0
	for i := range t.Segments {
		seg := &t.Segments[i]
		if seg.Flushed {
			continue
		}
		isPending := seg.Type == SegmentTool && seg.ToolStatus == ToolPending
		if isPending {
			continue
		}
		unflushedSeen++
		// Flush if: it's an image/diff OR we have more than minKeep unflushed segments
		shouldFlush := seg.Type == SegmentImage || seg.Type == SegmentDiff || unflushedSeen <= unflushedCount-minKeep
		if shouldFlush {
			toFlush = append(toFlush, seg)
			seg.Flushed = true
		}
	}

	if len(toFlush) == 0 {
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	// Render the segments to flush
	content := RenderSegments(toFlush, width, -1, renderMd, true)
	return FlushToScrollbackResult{
		ToPrint:         content,
		NewPrintedLines: 0, // No longer used
	}
}

// FlushAllRemaining returns any remaining unflushed content.
// Use this at the end of streaming to ensure all content is visible.
func (t *ToolTracker) FlushAllRemaining(
	width int,
	printedLines int,
	renderMd func(string, int) string,
) FlushToScrollbackResult {
	// Collect all unflushed completed segments
	var toFlush []*Segment
	for i := range t.Segments {
		seg := &t.Segments[i]
		if seg.Flushed {
			continue
		}
		isPending := seg.Type == SegmentTool && seg.ToolStatus == ToolPending
		if isPending {
			continue
		}
		toFlush = append(toFlush, seg)
		seg.Flushed = true
	}

	if len(toFlush) == 0 {
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	content := RenderSegments(toFlush, width, -1, renderMd, true)
	return FlushToScrollbackResult{
		ToPrint:         content,
		NewPrintedLines: 0,
	}
}

// FlushBeforeExternalUI flushes content to scrollback before showing external UI.
// Keeps some recent content visible for context.
func (t *ToolTracker) FlushBeforeExternalUI(
	width int,
	printedLines int,
	keepLines int,
	renderMd func(string, int) string,
) FlushToScrollbackResult {
	// Flush all but the last segment (keep some context visible)
	var toFlush []*Segment
	unflushedCompleted := 0

	// First count unflushed completed segments
	for i := range t.Segments {
		seg := &t.Segments[i]
		if !seg.Flushed && !(seg.Type == SegmentTool && seg.ToolStatus == ToolPending) {
			unflushedCompleted++
		}
	}

	// Flush all but the last one
	seen := 0
	for i := range t.Segments {
		seg := &t.Segments[i]
		if seg.Flushed {
			continue
		}
		isPending := seg.Type == SegmentTool && seg.ToolStatus == ToolPending
		if isPending {
			continue
		}
		seen++
		// Keep the last segment unflushed for context
		if seen < unflushedCompleted {
			toFlush = append(toFlush, seg)
			seg.Flushed = true
		}
	}

	if len(toFlush) == 0 {
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	content := RenderSegments(toFlush, width, -1, renderMd, true)
	return FlushToScrollbackResult{
		ToPrint:         content,
		NewPrintedLines: 0,
	}
}

// CountLines counts the number of newlines in content
func CountLines(content string) int {
	count := 0
	for _, c := range content {
		if c == '\n' {
			count++
		}
	}
	return count
}

// SplitLines splits content by newlines. Unlike strings.Split, this does NOT
// include an empty trailing element for content ending in newline.
// Example: "hello\nworld\n" → ["hello", "world"] (not ["hello", "world", ""])
func SplitLines(content string) []string {
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

// JoinLines joins lines with newlines
func JoinLines(lines []string) string {
	return strings.Join(lines, "\n")
}
