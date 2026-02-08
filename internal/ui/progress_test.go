package ui

import (
	"strings"
	"testing"
	"time"
)

func TestStreamingIndicator_NoLeadingNewlineAfterFlush_WhenRenderingActiveTool(t *testing.T) {
	styles := DefaultStyles()

	out := StreamingIndicator{
		Segments: []*Segment{
			{
				Type:       SegmentTool,
				ToolName:   "web_search",
				ToolInfo:   "(query: latest updates)",
				ToolStatus: ToolPending,
			},
		},
		WavePos:         0,
		Width:           80,
		RenderMarkdown:  RenderMarkdown,
		HasFlushed:      true,
		LastFlushedType: SegmentText,
	}.Render(styles)

	plain := StripANSI(out)
	if strings.HasPrefix(plain, "\n") {
		t.Fatalf("active tool indicator should not start with a newline after flush; got %q", plain)
	}
	if !strings.Contains(plain, "web_search") {
		t.Fatalf("expected active tool text in indicator, got %q", plain)
	}
}

func TestStreamingIndicator_NoLeadingNewlineAfterFlush_WhenRenderingSpinner(t *testing.T) {
	styles := DefaultStyles()

	out := StreamingIndicator{
		Spinner:         "•",
		Phase:           "Thinking",
		Elapsed:         time.Second,
		Width:           80,
		HasFlushed:      true,
		LastFlushedType: SegmentText,
	}.Render(styles)

	plain := StripANSI(out)
	if strings.HasPrefix(plain, "\n") {
		t.Fatalf("spinner indicator should not start with a newline after flush; got %q", plain)
	}
	if !strings.Contains(plain, "Thinking") {
		t.Fatalf("expected phase text in indicator, got %q", plain)
	}
}
