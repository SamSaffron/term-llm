package ui

import (
	"strings"
	"testing"
)

func TestAddExternalUIResult(t *testing.T) {
	tracker := NewToolTracker()

	// Plain text summary (styling applied at render time)
	plainSummary := "Preference: Go"

	tracker.AddExternalUIResult(plainSummary)

	// Should have one segment
	if len(tracker.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(tracker.Segments))
	}

	seg := tracker.Segments[0]

	// Should be an ask_user result segment
	if seg.Type != SegmentAskUserResult {
		t.Errorf("expected SegmentAskUserResult, got %d", seg.Type)
	}

	// Should be marked complete
	if !seg.Complete {
		t.Error("expected segment to be complete")
	}

	// Should have Text set to plain summary
	if seg.Text != plainSummary {
		t.Errorf("expected Text=%q, got %q", plainSummary, seg.Text)
	}

	// When rendering, it should NOT go through markdown renderer
	// Convert to []*Segment for RenderSegments
	segments := make([]*Segment, len(tracker.Segments))
	for i := range tracker.Segments {
		segments[i] = &tracker.Segments[i]
	}
	rendered := RenderSegments(segments, 80, -1, func(s string, w int) string {
		// This markdown renderer should NOT be called for ask_user results
		return "MARKDOWN_PROCESSED:" + s
	}, true)

	// Should NOT contain "MARKDOWN_PROCESSED" since ask_user results have their own renderer
	if strings.Contains(rendered, "MARKDOWN_PROCESSED") {
		t.Error("ask_user result should not be passed through markdown renderer")
	}

	// Should contain styled output with checkmark and label
	if !strings.Contains(rendered, "✓") {
		t.Error("expected rendered output to contain checkmark")
	}
	if !strings.Contains(rendered, "Preference:") {
		t.Error("expected rendered output to contain label")
	}
	if !strings.Contains(rendered, "Go") {
		t.Error("expected rendered output to contain value")
	}
}

func TestAddExternalUIResult_Empty(t *testing.T) {
	tracker := NewToolTracker()

	tracker.AddExternalUIResult("")

	// Should not add a segment for empty summary
	if len(tracker.Segments) != 0 {
		t.Errorf("expected 0 segments for empty summary, got %d", len(tracker.Segments))
	}
}

func TestAddExternalUIResult_WithExistingSegments(t *testing.T) {
	tracker := NewToolTracker()

	// Add some text
	tracker.AddTextSegment("Hello ")
	tracker.AddTextSegment("world")

	// Add a tool
	tracker.HandleToolStart("call-1", "read_file", "test.go")
	tracker.HandleToolEnd("call-1", true)

	// Add external UI result
	tracker.AddExternalUIResult("User selected: Option A")

	// Should have 3 segments: text, tool, ask_user_result
	if len(tracker.Segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(tracker.Segments))
	}

	// Last segment should be the external result (ask_user_result type)
	last := tracker.Segments[2]
	if last.Type != SegmentAskUserResult {
		t.Errorf("expected last segment to be SegmentAskUserResult, got %d", last.Type)
	}
	if last.Text != "User selected: Option A" {
		t.Errorf("expected text=%q, got %q", "User selected: Option A", last.Text)
	}
	if !last.Complete {
		t.Error("expected last segment to be complete")
	}
}
