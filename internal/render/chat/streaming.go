package chat

import (
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/ui"
)

// builderPool reuses strings.Builder instances to reduce allocations in hot
// paths (e.g. Render is called on every animation tick).
var builderPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

// StreamingBlock handles rendering of the active streaming response.
// It manages segments (text, tools, images, diffs) and provides
// incremental flushing for inline mode.
type StreamingBlock struct {
	width            int
	markdownRenderer MarkdownRenderer

	// Segments from the streaming response
	tracker *ui.ToolTracker

	// State
	complete bool
	err      error

	// For flush tracking
	lastFlushTime time.Time
}

// NewStreamingBlock creates a new streaming block.
func NewStreamingBlock(width int, mdRenderer MarkdownRenderer) *StreamingBlock {
	return &StreamingBlock{
		width:            width,
		markdownRenderer: mdRenderer,
		tracker:          ui.NewToolTracker(),
	}
}

// Resize updates the width and invalidates cached renders.
func (s *StreamingBlock) Resize(width int) {
	s.width = width
	if s.tracker != nil {
		// Invalidate segment caches
		for i := range s.tracker.Segments {
			s.tracker.Segments[i].Rendered = ""
			s.tracker.Segments[i].SafeRendered = ""
			s.tracker.Segments[i].SafePos = 0
			s.tracker.Segments[i].DiffRendered = ""
			s.tracker.Segments[i].DiffWidth = 0
			for j := range s.tracker.Segments[i].SubagentDiffs {
				s.tracker.Segments[i].SubagentDiffs[j].Rendered = ""
				s.tracker.Segments[i].SubagentDiffs[j].Width = 0
			}
		}
		// Resize streaming renderers
		s.tracker.ResizeStreamRenderers(width)
	}
}

// AddText adds streaming text content.
func (s *StreamingBlock) AddText(text string) {
	if s.tracker != nil {
		s.tracker.AddTextSegment(text, s.width)
	}
}

// StartTool adds a new pending tool segment.
// Returns true if a new segment was added (for starting wave animation).
func (s *StreamingBlock) StartTool(callID, name, info string) bool {
	if s.tracker == nil {
		return false
	}
	// Mark current text as complete before adding tool
	s.tracker.MarkCurrentTextComplete(s.renderFunc())
	return s.tracker.HandleToolStart(callID, name, info)
}

// EndTool updates the status of a pending tool.
func (s *StreamingBlock) EndTool(callID string, success bool) {
	if s.tracker != nil {
		s.tracker.HandleToolEnd(callID, success)
	}
}

// AddImage adds an image segment.
func (s *StreamingBlock) AddImage(path string) {
	if s.tracker != nil {
		s.tracker.AddImageSegment(path)
	}
}

// AddDiff adds a diff segment.
func (s *StreamingBlock) AddDiff(path, old, new string, line int) {
	if s.tracker != nil {
		s.tracker.AddDiffSegment(path, old, new, line)
	}
}

// AddAskUserResult adds an ask_user result segment.
func (s *StreamingBlock) AddAskUserResult(summary string) {
	if s.tracker != nil {
		s.tracker.AddExternalUIResult(summary)
	}
}

// Complete marks the streaming as complete.
func (s *StreamingBlock) Complete() {
	s.complete = true
	if s.tracker != nil {
		s.tracker.CompleteTextSegments(s.renderFunc())
	}
}

// SetError sets an error on the streaming block.
func (s *StreamingBlock) SetError(err error) {
	s.err = err
}

// HasPendingTools returns true if there are pending tools.
func (s *StreamingBlock) HasPendingTools() bool {
	if s.tracker == nil {
		return false
	}
	return s.tracker.HasPending()
}

// StartWaveAnimation starts the wave animation for pending tools.
func (s *StreamingBlock) StartWaveAnimation() tea.Cmd {
	if s.tracker == nil {
		return nil
	}
	return s.tracker.StartWave()
}

// HandleWaveTick handles wave animation tick.
func (s *StreamingBlock) HandleWaveTick() tea.Cmd {
	if s.tracker == nil {
		return nil
	}
	return s.tracker.HandleWaveTick()
}

// HandleWavePause handles wave animation pause end.
func (s *StreamingBlock) HandleWavePause() tea.Cmd {
	if s.tracker == nil {
		return nil
	}
	return s.tracker.HandleWavePause()
}

// Render renders the streaming block content.
// wavePos is the wave animation position (-1 = paused).
// pausedForUI is true when paused for external UI (hide indicator).
// includeImages controls whether to render images inline.
func (s *StreamingBlock) Render(wavePos int, pausedForUI bool, includeImages bool) string {
	if s.tracker == nil {
		return ""
	}

	b := builderPool.Get().(*strings.Builder)
	b.Reset()
	defer builderPool.Put(b)

	if includeImages {
		// Alt-screen mode: show everything
		var completed, active []*ui.Segment
		for i := range s.tracker.Segments {
			seg := &s.tracker.Segments[i]
			if seg.Type == ui.SegmentTool && seg.ToolStatus == ui.ToolPending {
				active = append(active, seg)
			} else {
				completed = append(completed, seg)
			}
		}

		// Render completed segments
		content := ui.RenderSegments(completed, s.width, -1, s.mdRenderFunc(), true)
		if content != "" {
			b.WriteString(content)
			// Add separator before active tools if needed
			if len(active) > 0 {
				b.WriteString(ui.SegmentSeparator(completed[len(completed)-1].Type, ui.SegmentTool))
			}
		}

		// Render active tools
		if len(active) > 0 {
			activeContent := ui.RenderSegments(active, s.width, wavePos, s.mdRenderFunc(), false)
			b.WriteString(activeContent)
		}
	} else {
		// Inline mode: only unflushed
		content := s.tracker.RenderUnflushed(s.width, s.mdRenderFunc(), false)
		if content != "" {
			b.WriteString(content)
		}

		// If not paused for external UI, render active tools indicator
		active := s.tracker.ActiveSegments()
		if !pausedForUI && len(active) > 0 {
			// Add separator before active tools if we have content OR if something was flushed
			if b.Len() > 0 {
				// We have unflushed completed segments, find the last one's type
				unflushed := s.tracker.CompletedSegments()
				lastType := unflushed[len(unflushed)-1].Type
				b.WriteString(ui.SegmentSeparator(lastType, ui.SegmentTool))
			} else if s.tracker.HasFlushed {
				// Everything completed was flushed, use flush state for separator
				b.WriteString(s.tracker.FlushLeadingSeparator(ui.SegmentTool))
			}

			activeContent := ui.RenderSegments(active, s.width, wavePos, s.mdRenderFunc(), false)
			if activeContent != "" {
				b.WriteString(activeContent)
			}
		}
	}

	// Clone so the returned string doesn't share the builder's backing
	// array, which will be overwritten when the pool reuses this builder.
	return strings.Clone(b.String())
}

// Flush returns content to print to scrollback and marks it as flushed.
func (s *StreamingBlock) Flush() FlushResult {
	if s.tracker == nil {
		return FlushResult{}
	}

	// Don't flush too frequently
	if time.Since(s.lastFlushTime) < 100*time.Millisecond {
		return FlushResult{}
	}

	result := s.tracker.FlushToScrollback(s.width, 0, maxViewLines, s.mdRenderFunc())
	if result.ToPrint != "" {
		s.lastFlushTime = time.Now()
		return FlushResult{Content: result.ToPrint}
	}
	return FlushResult{}
}

// FlushAll flushes all remaining content.
func (s *StreamingBlock) FlushAll() FlushResult {
	if s.tracker == nil {
		return FlushResult{}
	}

	result := s.tracker.FlushAllRemaining(s.width, 0, s.mdRenderFunc())
	return FlushResult{Content: result.ToPrint}
}

// GetCompletedContent returns the full rendered content for caching.
// Used to preserve images/diffs after streaming ends in alt-screen mode.
func (s *StreamingBlock) GetCompletedContent() string {
	if s.tracker == nil {
		return ""
	}

	// Get all non-pending segments
	var segments []*ui.Segment
	for i := range s.tracker.Segments {
		seg := &s.tracker.Segments[i]
		if seg.Type == ui.SegmentTool && seg.ToolStatus == ui.ToolPending {
			continue
		}
		segments = append(segments, seg)
	}

	// Render images and diffs (these need to persist after streaming)
	return ui.RenderImagesAndDiffs(segments, s.width)
}

// renderFunc returns the markdown render function for completing text segments.
func (s *StreamingBlock) renderFunc() func(string) string {
	if s.markdownRenderer == nil {
		return nil
	}
	return func(text string) string {
		return s.markdownRenderer(text, s.width)
	}
}

// mdRenderFunc returns the markdown render function for RenderSegments.
func (s *StreamingBlock) mdRenderFunc() func(string, int) string {
	return s.markdownRenderer
}

// maxViewLines is the maximum number of lines to keep in View().
const maxViewLines = 8

// GetTracker returns the underlying ToolTracker.
// This is used for compatibility with existing code during migration.
func (s *StreamingBlock) GetTracker() *ui.ToolTracker {
	return s.tracker
}

// SetTracker sets the underlying ToolTracker.
// This is used for compatibility with existing code during migration.
func (s *StreamingBlock) SetTracker(tracker *ui.ToolTracker) {
	s.tracker = tracker
}
