package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// WaveTickMsg is sent to advance the wave animation
type WaveTickMsg struct{}

// WavePauseMsg is sent when wave pause ends
type WavePauseMsg struct{}

var flushDebugEnabled = os.Getenv("TERM_LLM_DEBUG_FLUSH") != ""

func debugFlushf(format string, args ...any) {
	if !flushDebugEnabled {
		return
	}
	fmt.Fprintf(os.Stderr, "[flush] "+format+"\n", args...)
}

// stripLeadingBlankLine removes exactly ONE leading blank line from content.
// A blank line is defined as a line containing only whitespace (spaces/tabs) and ANSI codes.
// This is used to prevent double blank lines when tea.Printf adds its own newline
// after each flush and the next flush starts with a blank line.
func stripLeadingBlankLine(s string) string {
	if s == "" {
		return s
	}
	// Find the first newline
	idx := strings.Index(s, "\n")
	if idx == -1 {
		return s
	}
	// Check if everything before the newline is whitespace/ANSI
	firstLine := s[:idx]
	if isBlankOrANSI(firstLine) {
		// Strip this one blank line
		return s[idx+1:]
	}
	return s
}

// stripAllLeadingBlankLines removes all leading blank lines from content.
// This is used for tool->text boundaries where the tool line already
// contributes the visual separator.
func stripAllLeadingBlankLines(s string) string {
	for {
		next := stripLeadingBlankLine(s)
		if next == s {
			return s
		}
		s = next
	}
}

func shouldTrimAllLeadingBlankLines(prevType, nextType SegmentType) bool {
	return prevType == SegmentTool && nextType == SegmentText
}

// isBlankOrANSI returns true if the string contains only whitespace and ANSI escape codes.
func isBlankOrANSI(s string) bool {
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' {
			// Skip ANSI escape sequence
			i++
			if i < len(s) && s[i] == '[' {
				i++
				// Skip until we hit the terminating letter
				for i < len(s) && !((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z')) {
					i++
				}
				if i < len(s) {
					i++ // Skip the terminating letter
				}
			}
		} else if s[i] == ' ' || s[i] == '\t' {
			i++
		} else {
			// Non-whitespace, non-ANSI character
			return false
		}
	}
	return true
}

// ToolTracker manages tool segment state and wave animation.
// Designed to be embedded in larger models (ask, chat) for consistent tool tracking.
type ToolTracker struct {
	Segments     []Segment
	WavePos      int
	WavePaused   bool
	LastActivity time.Time
	Version      uint64 // Incremented when content changes (segments added/modified)
	TextMode     bool   // When true, skip markdown rendering (plain text output)
	expanded     bool

	// Flush state for consistent spacing
	LastFlushedType SegmentType
	HasFlushed      bool
}

// NewToolTracker creates a new ToolTracker
func NewToolTracker() *ToolTracker {
	return &ToolTracker{
		Segments:     make([]Segment, 0),
		LastActivity: time.Now(),
	}
}

// SetExpanded controls whether tool segments render in expanded mode.
func (t *ToolTracker) SetExpanded(v bool) {
	t.expanded = v
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
func (t *ToolTracker) HandleToolStart(callID, toolName, toolInfo string, toolArgs json.RawMessage) bool {
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
		ToolArgs:   toolArgs,
		ToolStatus: ToolPending,
	})
	t.Version++
	return true
}

// HandleToolEnd updates the status of a pending tool by its call ID.
func (t *ToolTracker) HandleToolEnd(callID string, success bool) {
	t.RecordActivity()
	t.Segments = UpdateToolStatus(t.Segments, callID, success)
	t.Version++
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
			text := seg.GetText()
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
// width is used to create the streaming markdown renderer for proper formatting.
// Returns true if this created a new segment.
func (t *ToolTracker) AddTextSegment(text string, width int) bool {
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

			// Write to streaming renderer if present
			if last.StreamRenderer != nil {
				if err := last.StreamRenderer.Write(text); err != nil {
					// Disable renderer on error to avoid inconsistent output
					last.StreamRenderer = nil
				}
			}
			t.Version++
			return false
		}
	}

	// Create new text segment - clone text to avoid sharing memory with caller's buffer
	builder := &strings.Builder{}
	builder.WriteString(text)

	// Create streaming renderer for progressive markdown rendering
	// Skip in text mode to avoid glamour rendering entirely
	var renderer *TextSegmentRenderer
	if width > 0 && !t.TextMode {
		var err error
		renderer, err = NewTextSegmentRenderer(width)
		if err == nil {
			if err := renderer.Write(text); err != nil {
				// Disable renderer on write error
				renderer = nil
			}
		}
		// If renderer creation fails, fall back to raw text display
	}

	t.Segments = append(t.Segments, Segment{
		Type:            SegmentText,
		TextBuilder:     builder,
		TextSnapshot:    strings.Clone(text),
		TextSnapshotLen: len(text),
		StreamRenderer:  renderer,
	})
	t.Version++
	return true
}

func completeTextSegment(seg *Segment, renderFunc func(string) string) {
	if seg == nil || seg.Type != SegmentText || seg.Complete {
		return
	}

	// Finalize TextBuilder to Text - clone to avoid sharing memory with builder.
	if seg.TextBuilder != nil {
		seg.Text = strings.Clone(seg.TextBuilder.String())
		seg.TextBuilder = nil
	}

	// Flush and close streaming renderer, capturing its output.
	if seg.StreamRenderer != nil {
		if err := seg.StreamRenderer.Flush(); err != nil {
			// Flush failed, fall back to renderFunc.
			seg.StreamRenderer = nil
			if seg.Text != "" && renderFunc != nil {
				seg.Rendered = renderFunc(seg.Text)
			}
		} else {
			seg.Rendered = seg.StreamRenderer.RenderedAll()
			// Keep segment-level flush tracking in sync with the renderer. The
			// renderer may rewind this offset when markdown re-renders rewrite
			// earlier output (for example list tightening).
			seg.FlushedRenderedPos = seg.StreamRenderer.FlushedRenderedPos()
			if seg.FlushedRenderedPos > len(seg.Rendered) {
				seg.FlushedRenderedPos = len(seg.Rendered)
			}
			seg.StreamRenderer = nil
		}
	} else if seg.Text != "" && renderFunc != nil {
		// Fallback: no streaming renderer, render full text.
		seg.Rendered = renderFunc(seg.Text)
	}

	// Clamp in case caller-provided or stale state points past rendered content.
	if seg.FlushedRenderedPos < 0 {
		seg.FlushedRenderedPos = 0
	}
	if seg.FlushedRenderedPos > len(seg.Rendered) {
		seg.FlushedRenderedPos = len(seg.Rendered)
	}

	seg.Complete = true
}

// CompleteTextSegments marks all incomplete text segments as complete.
// Renders the full text with glamour on completion.
func (t *ToolTracker) CompleteTextSegments(renderFunc func(string) string) {
	for i := range t.Segments {
		completeTextSegment(&t.Segments[i], renderFunc)
	}
}

// MarkCurrentTextComplete marks the current text segment as complete before a tool starts.
// Renders the full text with glamour.
func (t *ToolTracker) MarkCurrentTextComplete(renderFunc func(string) string) {
	if len(t.Segments) > 0 {
		last := &t.Segments[len(t.Segments)-1]
		completeTextSegment(last, renderFunc)
	}
}

// AddPreRenderedTextSegment adds a text segment that is already fully rendered
// (e.g., styled interjection prompts). The segment is marked Complete immediately
// with the Rendered field set, so it bypasses markdown rendering entirely.
func (t *ToolTracker) AddPreRenderedTextSegment(rendered string) {
	t.RecordActivity()
	seg := Segment{
		Type:     SegmentText,
		Text:     rendered,
		Rendered: rendered,
		Complete: true,
	}
	t.Segments = append(t.Segments, seg)
	t.Version++
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
	t.Version++
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
	t.Version++
}

// AddDiffSegment adds a diff segment for inline display.
func (t *ToolTracker) AddDiffSegment(path, old, new string, line int) {
	if path == "" {
		return
	}
	t.RecordActivity()

	// Deduplicate: check if this diff already exists in the tracker
	for i := len(t.Segments) - 1; i >= 0; i-- {
		seg := t.Segments[i]
		if seg.Type == SegmentDiff && seg.DiffPath == path && seg.DiffOld == old && seg.DiffNew == new && seg.DiffLine == line {
			return
		}
	}

	t.Segments = append(t.Segments, Segment{
		Type:     SegmentDiff,
		DiffPath: path,
		DiffOld:  old,
		DiffNew:  new,
		DiffLine: line,
		Complete: true,
	})
	t.Version++
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
	segIdx := -1
	for i := range t.Segments {
		if t.Segments[i].Type == SegmentText && !t.Segments[i].Complete {
			seg = &t.Segments[i]
			segIdx = i
			break
		}
	}
	if seg == nil {
		return FlushStreamingTextResult{}
	}

	// First: flush any completed tool segments before the text segment.
	// Tool calls are always processed before text continues, so any tool segments
	// that appear before the current text segment are already complete.
	hadPriorFlush := t.HasFlushed
	var contentBuilder strings.Builder
	var toolsToFlush []*Segment
	for i := 0; i < segIdx; i++ {
		s := &t.Segments[i]
		if s.Type == SegmentTool && !s.Flushed && s.ToolStatus != ToolPending {
			toolsToFlush = append(toolsToFlush, s)
			s.Flushed = true
		}
	}

	if len(toolsToFlush) > 0 {
		toolContent := RenderSegments(toolsToFlush, width, -1, nil, true, t.expanded)
		if t.HasFlushed {
			toolContent = stripLeadingBlankLine(toolContent)
			prefix := t.FlushLeadingSeparator(toolsToFlush[0].Type)
			if prefix != "" {
				toolContent = prefix + toolContent
			}
		}
		contentBuilder.WriteString(toolContent)
		t.HasFlushed = true
		t.LastFlushedType = toolsToFlush[len(toolsToFlush)-1].Type
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
		debugFlushf("stream skip seg=%d reason=below-threshold textLen=%d flushedPos=%d unflushedLen=%d threshold=%d", segIdx, textLen, seg.FlushedPos, unflushedLen, threshold)
		// Return any tool content we already flushed, even if text doesn't need flushing
		if contentBuilder.Len() > 0 {
			return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
		}
		return FlushStreamingTextResult{}
	}

	debugFlushf("stream enter seg=%d textLen=%d flushedPos=%d unflushedLen=%d threshold=%d hasRenderer=%v", segIdx, textLen, seg.FlushedPos, unflushedLen, threshold, seg.StreamRenderer != nil)

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

	// If we have a streaming renderer, it's our source of truth for rendered output.
	// We MUST use its output instead of re-rendering raw markdown to avoid duplication
	// and drift.
	if seg.StreamRenderer != nil {
		// Use the committed markdown length from the renderer so raw/flushed state stays aligned.
		safeBoundary := seg.StreamRenderer.CommittedMarkdownLen()
		if safeBoundary <= seg.FlushedPos {
			// No new committed blocks to flush yet
			debugFlushf("stream skip seg=%d reason=no-committed committed=%d flushedPos=%d", segIdx, safeBoundary, seg.FlushedPos)
			// Return any tool content we already flushed
			if contentBuilder.Len() > 0 {
				return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
			}
			return FlushStreamingTextResult{}
		}
		rendered := seg.StreamRenderer.RenderedUnflushed()
		if rendered == "" {
			debugFlushf("stream skip seg=%d reason=empty-rendered committed=%d flushedPos=%d", segIdx, safeBoundary, seg.FlushedPos)
			// Return any tool content we already flushed
			if contentBuilder.Len() > 0 {
				return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
			}
			return FlushStreamingTextResult{}
		}

		if t.HasFlushed && seg.FlushedPos == 0 {
			if hadPriorFlush {
				// Crossing a tea.Printf boundary: strip one \n since tea.Printf added it
				contentBuilder.WriteString(t.FlushLeadingSeparator(SegmentText))
			} else {
				// Tool+text in same ToPrint: use full separator
				contentBuilder.WriteString(t.LeadingSeparator(SegmentText))
			}
		}

		toolToTextBoundary := seg.FlushedPos == 0 && t.HasFlushed && t.LastFlushedType == SegmentTool
		// Keep tool->text boundaries compact even if rendered markdown starts
		// with one or more blank lines.
		if toolToTextBoundary {
			rendered = stripAllLeadingBlankLines(rendered)
		} else if hadPriorFlush {
			rendered = stripLeadingBlankLine(rendered)
		}
		contentBuilder.WriteString(rendered)

		// Update tracker state
		seg.FlushedPos = safeBoundary
		t.HasFlushed = true
		t.LastFlushedType = SegmentText
		seg.StreamRenderer.MarkFlushed()
		seg.FlushedRenderedPos = seg.StreamRenderer.FlushedRenderedPos()

		debugFlushf("stream flush seg=%d committed=%d flushedPos=%d renderedUnflushedLen=%d flushedRenderedPos=%d", segIdx, safeBoundary, seg.FlushedPos, len(rendered), seg.FlushedRenderedPos)

		return FlushStreamingTextResult{
			ToPrint: contentBuilder.String(),
		}
	}

	// Find a safe boundary to flush up to (don't break markdown)
	safeBoundary := FindSafeBoundaryIncremental(fullText, seg.FlushedPos)
	if safeBoundary <= seg.FlushedPos {
		// No safe boundary found, can't flush yet
		debugFlushf("stream skip seg=%d reason=no-safe-boundary flushedPos=%d textLen=%d", segIdx, seg.FlushedPos, len(fullText))
		// Return any tool content we already flushed
		if contentBuilder.Len() > 0 {
			return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
		}
		return FlushStreamingTextResult{}
	}

	// Fallback for when no streaming renderer is available (should be rare)
	toFlush := fullText[seg.FlushedPos:safeBoundary]
	if toFlush == "" {
		debugFlushf("stream skip seg=%d reason=empty-toFlush safeBoundary=%d flushedPos=%d", segIdx, safeBoundary, seg.FlushedPos)
		// Return any tool content we already flushed
		if contentBuilder.Len() > 0 {
			return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
		}
		return FlushStreamingTextResult{}
	}

	if renderMd == nil {
		debugFlushf("stream skip seg=%d reason=no-renderer safeBoundary=%d flushedPos=%d", segIdx, safeBoundary, seg.FlushedPos)
		// Return any tool content we already flushed
		if contentBuilder.Len() > 0 {
			return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
		}
		return FlushStreamingTextResult{}
	}

	// Render the full committed markdown so inter-block spacing is preserved.
	renderedAll := renderMd(fullText[:safeBoundary], width)
	if renderedAll == "" {
		debugFlushf("stream skip seg=%d reason=empty-rendered-fallback safeBoundary=%d flushedPos=%d", segIdx, safeBoundary, seg.FlushedPos)
		// Return any tool content we already flushed
		if contentBuilder.Len() > 0 {
			return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
		}
		return FlushStreamingTextResult{}
	}

	rendered := renderedAll
	if seg.FlushedRenderedPos > 0 {
		if seg.FlushedRenderedPos >= len(renderedAll) {
			debugFlushf("stream skip seg=%d reason=rendered-pos>=len safeBoundary=%d flushedRenderedPos=%d renderedLen=%d", segIdx, safeBoundary, seg.FlushedRenderedPos, len(renderedAll))
			// Return any tool content we already flushed
			if contentBuilder.Len() > 0 {
				return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
			}
			return FlushStreamingTextResult{}
		}
		// Use safeANSISlice to avoid slicing mid-escape-sequence which would produce broken ANSI output
		rendered = safeANSISlice(renderedAll, seg.FlushedRenderedPos)
	}
	if rendered == "" {
		debugFlushf("stream skip seg=%d reason=empty-rendered-slice safeBoundary=%d flushedPos=%d", segIdx, safeBoundary, seg.FlushedPos)
		// Return any tool content we already flushed
		if contentBuilder.Len() > 0 {
			return FlushStreamingTextResult{ToPrint: contentBuilder.String()}
		}
		return FlushStreamingTextResult{}
	}

	// Only add leading separator if this is the very FIRST flush for this segment.
	if t.HasFlushed && seg.FlushedPos == 0 {
		if hadPriorFlush {
			// Crossing a tea.Printf boundary: strip one \n since tea.Printf added it
			contentBuilder.WriteString(t.FlushLeadingSeparator(SegmentText))
		} else {
			// Tool+text in same ToPrint: use full separator
			contentBuilder.WriteString(t.LeadingSeparator(SegmentText))
		}
	}
	toolToTextBoundary := seg.FlushedPos == 0 && t.HasFlushed && t.LastFlushedType == SegmentTool
	// Keep tool->text boundaries compact even if rendered markdown starts
	// with one or more blank lines.
	if toolToTextBoundary {
		rendered = stripAllLeadingBlankLines(rendered)
	} else if hadPriorFlush {
		rendered = stripLeadingBlankLine(rendered)
	}
	contentBuilder.WriteString(rendered)

	// Update flushed position/state
	seg.FlushedPos = safeBoundary
	seg.FlushedRenderedPos = len(renderedAll)
	t.HasFlushed = true
	t.LastFlushedType = SegmentText
	debugFlushf("stream flush seg=%d safeBoundary=%d flushedPos=%d toFlushLen=%d renderedLen=%d", segIdx, safeBoundary, seg.FlushedPos, len(toFlush), len(rendered))

	return FlushStreamingTextResult{
		ToPrint: contentBuilder.String(),
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

	// Keep at least some segments unflushed for View()
	// But always flush images and diffs immediately since they need to go to scrollback
	minKeep := maxViewLines
	debugFlushf("scrollback enter unflushedCount=%d minKeep=%d hasFlushed=%v lastFlushed=%d", unflushedCount, minKeep, t.HasFlushed, t.LastFlushedType)
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
			debugFlushf("scrollback skip reason=min-keep unflushedCount=%d minKeep=%d", unflushedCount, minKeep)
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
	if len(toFlush) == 0 {
		debugFlushf("scrollback skip reason=no-segments-to-flush")
	} else {
		debugFlushf("scrollback flushing count=%d hasFlushed=%v lastFlushed=%d", len(toFlush), t.HasFlushed, t.LastFlushedType)
	}

	// Render complete segments to flush
	if len(toFlush) > 0 {
		content := RenderSegments(toFlush, width, -1, renderMd, true, t.expanded)
		if t.HasFlushed {
			if shouldTrimAllLeadingBlankLines(t.LastFlushedType, toFlush[0].Type) {
				content = stripAllLeadingBlankLines(content)
			} else {
				// Strip leading blank line FIRST since tea.Printf adds a newline after each flush
				// This prevents double blank lines without removing intentional separator prefix
				content = stripLeadingBlankLine(content)
			}
			prefix := t.FlushLeadingSeparator(toFlush[0].Type)
			if prefix != "" {
				content = prefix + content
			}
		}
		if content != "" {
			contentBuilder.WriteString(content)
			t.HasFlushed = true
			t.LastFlushedType = toFlush[len(toFlush)-1].Type
			debugFlushf("scrollback flushed lastType=%d contentLen=%d", t.LastFlushedType, len(content))
			// Ensure streaming renderer matches flush state if this was a text segment
			for _, seg := range toFlush {
				if seg.StreamRenderer != nil {
					seg.StreamRenderer.MarkFlushed()
					seg.FlushedRenderedPos = seg.StreamRenderer.FlushedRenderedPos()
				} else if seg.Type == SegmentText && seg.Complete {
					// For completed text segments, mark the whole thing as flushed rendered
					if seg.Rendered != "" {
						seg.FlushedRenderedPos = len(seg.Rendered)
					} else {
						// This case shouldn't really happen if Rendered was set correctly
						// but as a fallback we could try to render it here if we had the width
					}
				}
			}
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

// ForceCompletePendingTools marks any pending tools as complete.
// Call this before FlushAllRemaining when streaming is done to ensure
// no tools are left in pending state (which would be skipped during flush).
func (t *ToolTracker) ForceCompletePendingTools() {
	for i := range t.Segments {
		seg := &t.Segments[i]
		if seg.Type == SegmentTool && seg.ToolStatus == ToolPending {
			seg.ToolStatus = ToolSuccess
		}
	}
}

// FlushAllRemaining returns any remaining unflushed content.
// Use this at the end of streaming to ensure all content is visible.
func (t *ToolTracker) FlushAllRemaining(
	width int,
	printedLines int,
	renderMd func(string, int) string,
) FlushToScrollbackResult {
	// Collect all unflushed segments (excluding pending tools)
	var toFlush []*Segment
	for i := range t.Segments {
		seg := &t.Segments[i]
		if seg.Flushed {
			continue
		}
		if seg.Type == SegmentTool && seg.ToolStatus == ToolPending {
			continue
		}
		toFlush = append(toFlush, seg)
	}

	if len(toFlush) == 0 {
		debugFlushf("flush-all skip reason=no-segments")
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	content := RenderSegments(toFlush, width, -1, renderMd, true, t.expanded)
	if t.HasFlushed {
		if shouldTrimAllLeadingBlankLines(t.LastFlushedType, toFlush[0].Type) {
			content = stripAllLeadingBlankLines(content)
		} else {
			// Strip leading blank line FIRST since tea.Printf adds a newline after each flush
			// This prevents double blank lines without removing intentional separator prefix
			content = stripLeadingBlankLine(content)
		}
		prefix := t.FlushLeadingSeparator(toFlush[0].Type)
		if prefix != "" {
			content = prefix + content
		}
	}

	// Always mark segments as flushed, even if content is empty (already streamed out)
	for _, seg := range toFlush {
		seg.Flushed = true
		if seg.StreamRenderer != nil {
			seg.StreamRenderer.MarkFlushed()
			seg.FlushedRenderedPos = seg.StreamRenderer.FlushedRenderedPos()
		} else if seg.Type == SegmentText && seg.Complete && seg.Rendered != "" {
			seg.FlushedRenderedPos = len(seg.Rendered)
		}
	}

	if content == "" {
		debugFlushf("flush-all skip reason=empty-render count=%d (segments marked flushed)", len(toFlush))
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	t.HasFlushed = true
	t.LastFlushedType = toFlush[len(toFlush)-1].Type
	debugFlushf("flush-all flushed count=%d lastType=%d contentLen=%d", len(toFlush), t.LastFlushedType, len(content))

	return FlushToScrollbackResult{
		ToPrint:         content,
		NewPrintedLines: 0,
	}
}

// FlushCompletedNow flushes all completed, unflushed segments immediately.
// Unlike FlushToScrollback, this does not keep any segments for View().
// Pending tools and incomplete text segments are never flushed.
func (t *ToolTracker) FlushCompletedNow(
	width int,
	renderMd func(string, int) string,
) FlushToScrollbackResult {
	var toFlush []*Segment
	for i := range t.Segments {
		seg := &t.Segments[i]
		if seg.Flushed {
			continue
		}
		if seg.Type == SegmentTool && seg.ToolStatus == ToolPending {
			continue
		}
		if seg.Type == SegmentText && !seg.Complete {
			continue
		}
		toFlush = append(toFlush, seg)
	}

	if len(toFlush) == 0 {
		debugFlushf("flush-now skip reason=no-completed-segments")
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	content := RenderSegments(toFlush, width, -1, renderMd, true, t.expanded)
	if t.HasFlushed {
		if shouldTrimAllLeadingBlankLines(t.LastFlushedType, toFlush[0].Type) {
			content = stripAllLeadingBlankLines(content)
		} else {
			content = stripLeadingBlankLine(content)
		}
		prefix := t.FlushLeadingSeparator(toFlush[0].Type)
		if prefix != "" {
			content = prefix + content
		}
	}

	// Always mark segments as flushed, even if rendered content is empty.
	for _, seg := range toFlush {
		seg.Flushed = true
		if seg.StreamRenderer != nil {
			seg.StreamRenderer.MarkFlushed()
			seg.FlushedRenderedPos = seg.StreamRenderer.FlushedRenderedPos()
		} else if seg.Type == SegmentText && seg.Complete && seg.Rendered != "" {
			seg.FlushedRenderedPos = len(seg.Rendered)
		}
	}

	if content == "" {
		debugFlushf("flush-now skip reason=empty-render count=%d (segments marked flushed)", len(toFlush))
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	t.HasFlushed = true
	t.LastFlushedType = toFlush[len(toFlush)-1].Type
	debugFlushf("flush-now flushed count=%d lastType=%d contentLen=%d", len(toFlush), t.LastFlushedType, len(content))

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
		debugFlushf("flush-external skip reason=no-segments")
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	content := RenderSegments(toFlush, width, -1, renderMd, true, t.expanded)
	if t.HasFlushed {
		if shouldTrimAllLeadingBlankLines(t.LastFlushedType, toFlush[0].Type) {
			content = stripAllLeadingBlankLines(content)
		} else {
			// Strip leading blank line FIRST since tea.Printf adds a newline after each flush
			// This prevents double blank lines without removing intentional separator prefix
			content = stripLeadingBlankLine(content)
		}
		prefix := t.FlushLeadingSeparator(toFlush[0].Type)
		if prefix != "" {
			content = prefix + content
		}
	}
	if content == "" {
		debugFlushf("flush-external skip reason=empty-render count=%d", len(toFlush))
		return FlushToScrollbackResult{NewPrintedLines: 0}
	}

	t.HasFlushed = true
	t.LastFlushedType = toFlush[len(toFlush)-1].Type
	// Ensure streaming renderer matches flush state if this was a text segment
	for _, seg := range toFlush {
		if seg.StreamRenderer != nil {
			seg.StreamRenderer.MarkFlushed()
		}
	}
	debugFlushf("flush-external flushed count=%d lastType=%d contentLen=%d", len(toFlush), t.LastFlushedType, len(content))

	return FlushToScrollbackResult{
		ToPrint:         content,
		NewPrintedLines: 0,
	}
}

// ResizeStreamRenderers updates all active streaming renderers with new width.
// On resize error, the renderer is disabled to avoid inconsistent output.
func (t *ToolTracker) ResizeStreamRenderers(width int) {
	for i := range t.Segments {
		if t.Segments[i].StreamRenderer != nil {
			if err := t.Segments[i].StreamRenderer.Resize(width); err != nil {
				// Disable renderer on resize error to avoid inconsistent output
				t.Segments[i].StreamRenderer = nil
			}
		}
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

// RenderUnflushed renders all non-pending, non-flushed segments with proper leading spacing.
func (t *ToolTracker) RenderUnflushed(width int, renderMd func(string, int) string, includeImages bool) string {
	unflushed := t.CompletedSegments()
	if len(unflushed) == 0 {
		return ""
	}

	var leading *Segment
	// Only add leading context if the FIRST unflushed segment is at its very start.
	if t.HasFlushed && unflushed[0].FlushedPos == 0 {
		leading = &Segment{Type: t.LastFlushedType}
	}

	return RenderSegmentsWithLeading(leading, unflushed, width, -1, renderMd, includeImages, t.expanded)
}

// LeadingSeparator returns the full spacing before a segment of the given type,
// based on the last flushed segment. Use this for View()/Render() output and
// when appending multiple parts within a single ToPrint (no tea.Printf between them).
// For spacing across tea.Printf boundaries, use FlushLeadingSeparator instead.
func (t *ToolTracker) LeadingSeparator(nextType SegmentType) string {
	if !t.HasFlushed {
		return ""
	}
	return SegmentSeparator(t.LastFlushedType, nextType)
}

// FlushLeadingSeparator returns the spacing before a flushed segment,
// accounting for the trailing newline that tea.Printf/tea.Println adds.
// Use this only when the previous content was printed via a separate tea.Printf call.
// For intra-ToPrint spacing (multiple parts in one flush), use LeadingSeparator.
func (t *ToolTracker) FlushLeadingSeparator(nextType SegmentType) string {
	if !t.HasFlushed {
		return ""
	}
	sep := SegmentSeparator(t.LastFlushedType, nextType)
	if len(sep) > 0 && sep[0] == '\n' {
		return sep[1:]
	}
	return sep
}
