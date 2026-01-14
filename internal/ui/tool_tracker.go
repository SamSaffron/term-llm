package ui

import (
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
	Segments   []Segment
	WavePos    int
	WavePaused bool
}

// NewToolTracker creates a new ToolTracker
func NewToolTracker() *ToolTracker {
	return &ToolTracker{
		Segments: make([]Segment, 0),
	}
}

// HandleToolStart adds a pending segment if none exists for this tool name.
// Returns true if a new segment was added (caller should start wave animation).
func (t *ToolTracker) HandleToolStart(toolName, toolInfo string) bool {
	// Check if we already have a pending segment for this tool name
	for i := len(t.Segments) - 1; i >= 0; i-- {
		seg := t.Segments[i]
		if seg.Type == SegmentTool &&
			seg.ToolStatus == ToolPending &&
			seg.ToolName == toolName {
			return false
		}
	}

	// Add new pending segment
	t.Segments = append(t.Segments, Segment{
		Type:       SegmentTool,
		ToolName:   toolName,
		ToolInfo:   toolInfo,
		ToolStatus: ToolPending,
	})
	return true
}

// HandleToolEnd updates the status of a pending tool.
func (t *ToolTracker) HandleToolEnd(toolName string, success bool) {
	t.Segments = UpdateToolStatus(t.Segments, toolName, "", success)
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
