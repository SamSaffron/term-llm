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

// CompletedSegments returns non-pending, non-flushed segments up to (but not past) the first pending tool.
// This preserves interleaving order: text before a pending tool is shown, but text after it is held back
// until the tool completes. Returns pointers so mutations (like SafeRendered caching) persist.
func (t *ToolTracker) CompletedSegments() []*Segment {
	var completed []*Segment
	for i := range t.Segments {
		seg := &t.Segments[i]
		if seg.Flushed {
			continue
		}
		// Stop at the first pending tool - don't include segments after it
		if seg.Type == SegmentTool && seg.ToolStatus == ToolPending {
			break
		}
		completed = append(completed, seg)
	}
	return completed
}

// AllCompletedSegments returns all non-pending segments regardless of Flushed status.
// Use this for the final View() to ensure nothing is lost when segments were flushed
// to scrollback during streaming but we need the complete content for the final render.
func (t *ToolTracker) AllCompletedSegments() []*Segment {
	var completed []*Segment
	for i := range t.Segments {
		if t.Segments[i].Type == SegmentTool && t.Segments[i].ToolStatus == ToolPending {
			continue
		}
		completed = append(completed, &t.Segments[i])
	}
	return completed
}

// AllSegments returns all segments regardless of Flushed status.
// Use for alt screen mode where we render everything in View().
func (t *ToolTracker) AllSegments() []Segment {
	return t.Segments
}

// UnflushedSegments returns segments with unflushed content for final rendering.
// For text segments with partial flushes (FlushedPos > 0), creates a copy with only
// the unflushed portion. This ensures we don't duplicate content that was already
// printed to scrollback during streaming.
func (t *ToolTracker) UnflushedSegments() []*Segment {
	var result []*Segment
	for i := range t.Segments {
		seg := &t.Segments[i]
		// Skip fully flushed segments
		if seg.Flushed {
			continue
		}
		// Skip pending tools
		if seg.Type == SegmentTool && seg.ToolStatus == ToolPending {
			continue
		}
		// For text segments with partial flushes, create a copy with unflushed portion
		if seg.Type == SegmentText && seg.FlushedPos > 0 {
			text := seg.Text
			if text == "" && seg.TextBuilder != nil {
				text = seg.TextBuilder.String()
			}
			if seg.FlushedPos < len(text) {
				// Create a partial segment with only unflushed content
				partial := &Segment{
					Type:     SegmentText,
					Text:     text[seg.FlushedPos:],
					Complete: seg.Complete,
				}
				result = append(result, partial)
			}
			continue
		}
		result = append(result, seg)
	}
	return result
}

// AddTextSegment adds or appends to a text segment.
// Returns true if this created a new segment.
func (t *ToolTracker) AddTextSegment(text string) bool {
	t.RecordActivity()

	// Find or append to current incomplete text segment
	if len(t.Segments) > 0 {
		last := &t.Segments[len(t.Segments)-1]
		if last.Type == SegmentText && !last.Complete {
			if last.TextBuilder == nil {
				last.TextBuilder = &strings.Builder{}
				last.TextBuilder.WriteString(last.Text)
				last.Text = ""
			}
			last.TextBuilder.WriteString(text)
			last.TextSnapshot = strings.Clone(last.TextBuilder.String())
			last.TextSnapshotLen = last.TextBuilder.Len()
			return false
		}
	}

	// Create new text segment - clone text to avoid sharing memory with caller's buffer
	builder := &strings.Builder{}
	builder.WriteString(text)
	t.Segments = append(t.Segments, Segment{
		Type:            SegmentText,
		TextBuilder:     builder,
		TextSnapshot:    strings.Clone(text),
		TextSnapshotLen: len(text),
	})
	return true
}

// CompleteTextSegments marks all incomplete text segments as complete.
// Renders the full text with glamour on completion.
func (t *ToolTracker) CompleteTextSegments(renderFunc func(string) string) {
	for i := range t.Segments {
		if t.Segments[i].Type == SegmentText && !t.Segments[i].Complete {
			// Finalize TextBuilder to Text - clone to avoid sharing memory with builder
			if t.Segments[i].TextBuilder != nil {
				t.Segments[i].Text = strings.Clone(t.Segments[i].TextBuilder.String())
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
// Renders the full text with glamour.
func (t *ToolTracker) MarkCurrentTextComplete(renderFunc func(string) string) {
	if len(t.Segments) > 0 {
		last := &t.Segments[len(t.Segments)-1]
		if last.Type == SegmentText && !last.Complete {
			// Finalize TextBuilder to Text - clone to avoid sharing memory with builder
			if last.TextBuilder != nil {
				last.Text = strings.Clone(last.TextBuilder.String())
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

// FlushStreamingTextResult contains the result of a streaming text flush.
type FlushStreamingTextResult struct {
	// ToPrint is the rendered content to print to scrollback (empty if nothing to flush)
	ToPrint string
}

// FlushStreamingText flushes portions of in-progress text segments to scrollback
// when they exceed the threshold. Uses safe markdown boundaries to avoid breaking
// formatting. Returns rendered content to print, or empty if nothing to flush.
//
// Parameters:
//   - threshold: minimum bytes before attempting flush (e.g., 4000)
//   - width: terminal width for rendering
//   - renderMd: markdown render function (text, width) -> rendered
func (t *ToolTracker) FlushStreamingText(threshold int, width int, renderMd func(string, int) string) FlushStreamingTextResult {
	// Find the current incomplete text segment
	var seg *Segment
	for i := range t.Segments {
		if t.Segments[i].Type == SegmentText && !t.Segments[i].Complete {
			seg = &t.Segments[i]
			break
		}
	}
	if seg == nil {
		return FlushStreamingTextResult{}
	}

	// Get current text length
	textLen := 0
	if seg.TextBuilder != nil {
		textLen = seg.TextBuilder.Len()
	} else {
		textLen = len(seg.Text)
	}

	// Check if we have enough unflushed content to bother flushing
	unflushedLen := textLen - seg.FlushedPos
	if unflushedLen < threshold {
		return FlushStreamingTextResult{}
	}

	// Get the full text
	var fullText string
	if seg.TextBuilder != nil {
		fullText = seg.TextSnapshot
		if fullText == "" || seg.TextSnapshotLen != seg.TextBuilder.Len() {
			fullText = seg.TextBuilder.String()
		}
	} else {
		fullText = seg.Text
	}

	// Find a safe boundary to flush up to (don't break markdown)
	safeBoundary := FindSafeBoundaryIncremental(fullText, seg.FlushedPos)
	if safeBoundary <= seg.FlushedPos {
		// No safe boundary found, can't flush yet
		return FlushStreamingTextResult{}
	}

	// Render the portion to flush
	toFlush := fullText[seg.FlushedPos:safeBoundary]
	if toFlush == "" {
		return FlushStreamingTextResult{}
	}

	rendered := renderMd(toFlush, width)
	if rendered == "" {
		return FlushStreamingTextResult{}
	}

	// Update flushed position
	seg.FlushedPos = safeBoundary

	return FlushStreamingTextResult{
		ToPrint: rendered,
	}
}

// FlushToScrollbackResult contains the result of a scrollback flush operation.
type FlushToScrollbackResult struct {
	// ToPrint is the content to print to scrollback (empty if nothing to flush)
	ToPrint string
	// NewPrintedLines is kept for API compatibility but no longer used for tracking
	NewPrintedLines int
}

// FlushToScrollback flushes completed segments to scrollback.
// Incomplete text segments are NOT flushed - they show as raw text in View()
// and get rendered with glamour only when completed.
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
	var contentBuilder strings.Builder

	// Only flush complete segments - incomplete text shows as raw in View()
	isFlushable := func(seg *Segment) bool {
		if seg.Flushed {
			return false // already flushed
		}
		if seg.Type == SegmentTool && seg.ToolStatus == ToolPending {
			return false // pending tools can't be flushed
		}
		if seg.Type == SegmentText && !seg.Complete {
			return false // incomplete text can't be flushed as a whole
		}
		return true
	}

	// Count unflushed flushable segments
	var unflushedCount int
	for i := range t.Segments {
		if isFlushable(&t.Segments[i]) {
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
		if !hasUnflushedSpecial && contentBuilder.Len() == 0 {
			return FlushToScrollbackResult{NewPrintedLines: 0}
		}
	}

	// Collect segments to flush (all flushable segments except the last few)
	var toFlush []*Segment
	unflushedSeen := 0
	for i := range t.Segments {
		seg := &t.Segments[i]
		if !isFlushable(seg) {
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

	// Render complete segments to flush
	if len(toFlush) > 0 {
		content := RenderSegments(toFlush, width, -1, renderMd, true)
		if content != "" {
			contentBuilder.WriteString(content)
		}
	}

	if contentBuilder.Len() == 0 {
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	return FlushToScrollbackResult{
		ToPrint:         contentBuilder.String(),
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
	if content == "" {
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}
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
	if content == "" {
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}
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
