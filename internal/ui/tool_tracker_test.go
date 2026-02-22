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
	}, true, false)

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
	tracker.AddTextSegment("Hello ", 80)
	tracker.AddTextSegment("world", 80)

	// Add a tool
	tracker.HandleToolStart("call-1", "read_file", "test.go", nil)
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

// TestTextSnapshotNotCorrupted verifies that TextSnapshot isn't corrupted
// when subsequent text is appended to the TextBuilder.
// This is a regression test for a bug where strings.Builder.String() returns
// a string sharing memory with the internal buffer, which can be corrupted
// by subsequent WriteString calls if the buffer has enough capacity.
func TestTextSnapshotNotCorrupted(t *testing.T) {
	tracker := NewToolTracker()

	// Add first chunk
	tracker.AddTextSegment("Hello ", 80)
	seg := &tracker.Segments[0]

	// Get the text after first chunk
	text1 := seg.GetText()
	if text1 != "Hello " {
		t.Errorf("after first chunk: expected %q, got %q", "Hello ", text1)
	}

	// Add more chunks - these writes should NOT corrupt text1
	tracker.AddTextSegment("world! ", 80)
	tracker.AddTextSegment("How are you?", 80)

	// text1 should still be "Hello " (not corrupted)
	// NOTE: We can't check text1 directly since the snapshot was updated,
	// but we verify the current snapshot is correct
	text2 := seg.GetText()
	expected := "Hello world! How are you?"
	if text2 != expected {
		t.Errorf("after all chunks: expected %q, got %q", expected, text2)
	}

	// More importantly, check for corruption patterns that would occur
	// if strings.Builder.String() shares memory with the buffer
	// Corruption would show as text1 containing parts of later writes
	if strings.Contains(text2, "Hello Hello") {
		t.Error("detected corruption: duplicate content")
	}
	if !strings.HasPrefix(text2, "Hello ") {
		t.Error("detected corruption: prefix corrupted")
	}
}

// TestTextSnapshotStressTest adds many small chunks to trigger buffer reuse
func TestTextSnapshotStressTest(t *testing.T) {
	tracker := NewToolTracker()

	// Build expected content
	var expected strings.Builder
	chunks := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}

	for i, chunk := range chunks {
		tracker.AddTextSegment(chunk, 80)
		expected.WriteString(chunk)

		// Verify content after each append
		seg := &tracker.Segments[0]
		got := seg.GetText()
		want := expected.String()
		if got != want {
			t.Errorf("after chunk %d (%q): expected %q, got %q", i, chunk, want, got)
		}
	}
}

// TestAllCompletedSegments_IncludesFlushed verifies that AllCompletedSegments
// returns both flushed and unflushed segments, unlike CompletedSegments.
// This is critical for the final View() render to include all content.
func TestAllCompletedSegments_IncludesFlushed(t *testing.T) {
	tracker := NewToolTracker()

	// Add and mark first segment as flushed (simulates scrollback flush)
	tracker.AddTextSegment("First text", 80)
	tracker.Segments[0].Complete = true
	tracker.Segments[0].Flushed = true

	// Add unflushed segment
	tracker.AddTextSegment("Second text", 80)
	tracker.Segments[1].Complete = true

	// CompletedSegments should only return unflushed
	completed := tracker.CompletedSegments()
	if len(completed) != 1 {
		t.Errorf("CompletedSegments should return 1, got %d", len(completed))
	}
	if completed[0].GetText() != "Second text" {
		t.Errorf("CompletedSegments should return unflushed segment, got %q", completed[0].GetText())
	}

	// AllCompletedSegments should return both
	all := tracker.AllCompletedSegments()
	if len(all) != 2 {
		t.Errorf("AllCompletedSegments should return 2, got %d", len(all))
	}
	if all[0].GetText() != "First text" {
		t.Errorf("AllCompletedSegments[0] should be 'First text', got %q", all[0].GetText())
	}
	if all[1].GetText() != "Second text" {
		t.Errorf("AllCompletedSegments[1] should be 'Second text', got %q", all[1].GetText())
	}
}

// TestAllCompletedSegments_ExcludesPending verifies that pending tool segments
// are excluded from AllCompletedSegments.
func TestAllCompletedSegments_ExcludesPending(t *testing.T) {
	tracker := NewToolTracker()

	// Add a completed text segment
	tracker.AddTextSegment("Some text", 80)
	tracker.Segments[0].Complete = true

	// Add a pending tool segment
	tracker.HandleToolStart("call-1", "shell", "(git status)", nil)
	// Note: tool is pending, not ended

	// AllCompletedSegments should only return the text segment
	all := tracker.AllCompletedSegments()
	if len(all) != 1 {
		t.Errorf("AllCompletedSegments should return 1 (excluding pending tool), got %d", len(all))
	}
	if all[0].Type != SegmentText {
		t.Errorf("AllCompletedSegments should return text segment, got type %v", all[0].Type)
	}
}

// TestAllCompletedSegments_MixedScenario tests a realistic scenario with
// multiple segment types, some flushed, some not.
func TestAllCompletedSegments_MixedScenario(t *testing.T) {
	tracker := NewToolTracker()

	// Simulate: text -> tool (flushed) -> text (flushed) -> tool -> text
	tracker.AddTextSegment("First paragraph", 80)
	tracker.Segments[0].Complete = true

	tracker.HandleToolStart("call-1", "read_file", "main.go", nil)
	tracker.HandleToolEnd("call-1", true)
	tracker.Segments[1].Flushed = true // Flushed to scrollback

	tracker.AddTextSegment("Second paragraph", 80)
	tracker.Segments[2].Complete = true
	tracker.Segments[2].Flushed = true // Flushed to scrollback

	tracker.HandleToolStart("call-2", "shell", "(ls)", nil)
	tracker.HandleToolEnd("call-2", true)

	tracker.AddTextSegment("Third paragraph", 80)
	tracker.Segments[4].Complete = true

	// CompletedSegments: only unflushed (first text, second tool, third text)
	completed := tracker.CompletedSegments()
	if len(completed) != 3 {
		t.Errorf("CompletedSegments should return 3 unflushed, got %d", len(completed))
	}

	// AllCompletedSegments: all 5 segments
	all := tracker.AllCompletedSegments()
	if len(all) != 5 {
		t.Errorf("AllCompletedSegments should return 5, got %d", len(all))
	}
}

// TestFlushToScrollback_IncompleteTextNotFlushed verifies that incomplete
// text segments are NOT flushed to scrollback. This prevents content loss
// during streaming.
func TestFlushToScrollback_IncompleteTextNotFlushed(t *testing.T) {
	tracker := NewToolTracker()

	// Add incomplete text segment (simulates mid-stream)
	tracker.AddTextSegment("Streaming text...", 80)
	// Note: segment is NOT marked Complete

	// Add a completed tool segment
	tracker.HandleToolStart("call-1", "shell", "(pwd)", nil)
	tracker.HandleToolEnd("call-1", true)

	// Now we have 2 "unflushed" segments, but text is incomplete
	// FlushToScrollback should NOT flush the incomplete text

	mockRender := func(s string, w int) string { return s }
	_ = tracker.FlushToScrollback(80, 0, 1, mockRender)

	// The incomplete text should NOT be flushed
	if tracker.Segments[0].Flushed {
		t.Error("Incomplete text segment should NOT be flushed")
	}
}

// TestFlushToScrollback_CompleteTextCanBeFlushed verifies that complete
// text segments CAN be flushed when there are multiple segments.
func TestFlushToScrollback_CompleteTextCanBeFlushed(t *testing.T) {
	tracker := NewToolTracker()
	renderFn := func(s string) string { return s }

	// Add first text segment and complete it (using 0 width for fallback path)
	tracker.AddTextSegment("First paragraph\n\n", 0)
	tracker.MarkCurrentTextComplete(renderFn)

	// Add second text segment and complete it
	tracker.AddTextSegment("Second paragraph\n\n", 0)
	tracker.MarkCurrentTextComplete(renderFn)

	// Add third text segment and complete it
	tracker.AddTextSegment("Third paragraph\n\n", 0)
	tracker.MarkCurrentTextComplete(renderFn)

	// Should have 3 segments
	if len(tracker.Segments) != 3 {
		t.Fatalf("Expected 3 segments, got %d", len(tracker.Segments))
	}

	// With 3 segments and minKeep=1, we should flush 2
	mockRender := func(s string, w int) string { return s }
	result := tracker.FlushToScrollback(80, 0, 1, mockRender)

	// Should have flushed content
	if result.ToPrint == "" {
		t.Error("Expected some content to be flushed")
	}

	// First two segments should be flushed
	if !tracker.Segments[0].Flushed {
		t.Error("First complete segment should be flushed")
	}
	if !tracker.Segments[1].Flushed {
		t.Error("Second complete segment should be flushed")
	}
	// Last segment should NOT be flushed (minKeep=1)
	if tracker.Segments[2].Flushed {
		t.Error("Last segment should NOT be flushed (minKeep)")
	}
}

func TestFlushCompletedNow_FlushesAllCompletedSegments(t *testing.T) {
	tracker := NewToolTracker()
	renderFn := func(s string) string { return s }

	tracker.AddTextSegment("First paragraph\n\n", 0)
	tracker.MarkCurrentTextComplete(renderFn)

	tracker.AddTextSegment("Second paragraph\n\n", 0)
	tracker.MarkCurrentTextComplete(renderFn)

	tracker.HandleToolStart("call-1", "shell", "(pwd)", nil)
	tracker.HandleToolEnd("call-1", true)

	result := tracker.FlushCompletedNow(80, func(s string, w int) string { return s })
	if result.ToPrint == "" {
		t.Fatal("expected completed content to be flushed")
	}

	if !tracker.Segments[0].Flushed {
		t.Fatal("expected first text segment to be flushed")
	}
	if !tracker.Segments[1].Flushed {
		t.Fatal("expected second text segment to be flushed")
	}
	if !tracker.Segments[2].Flushed {
		t.Fatal("expected completed tool segment to be flushed")
	}
}

func TestFlushCompletedNow_DoesNotFlushPendingTools(t *testing.T) {
	tracker := NewToolTracker()

	tracker.HandleToolStart("call-1", "shell", "(pwd)", nil)

	result := tracker.FlushCompletedNow(80, func(s string, w int) string { return s })
	if result.ToPrint != "" {
		t.Fatalf("expected no output for pending tool-only state, got %q", result.ToPrint)
	}

	if len(tracker.Segments) != 1 {
		t.Fatalf("expected one tool segment, got %d", len(tracker.Segments))
	}
	if tracker.Segments[0].Flushed {
		t.Fatal("expected pending tool to remain unflushed")
	}
}

func TestMarkCurrentTextComplete_SyncsRendererFlushedOffsetBeforeBoundaryFlush(t *testing.T) {
	tracker := NewToolTracker()
	width := 80

	tracker.AddTextSegment("Paragraph one.\n\nParagraph two.", width)
	if len(tracker.Segments) != 1 {
		t.Fatalf("expected single text segment, got %d", len(tracker.Segments))
	}

	seg := &tracker.Segments[0]
	if seg.StreamRenderer == nil {
		t.Fatal("expected stream renderer to be initialized")
	}

	// Simulate stale tracker state: segment-level flushed rendered position can lag
	// behind renderer state after a renderer rewrite/rebase.
	seg.FlushedRenderedPos = len(seg.StreamRenderer.RenderedAll())
	if err := seg.StreamRenderer.Resize(width - 1); err != nil {
		t.Fatalf("expected resize to succeed: %v", err)
	}
	if seg.StreamRenderer.FlushedRenderedPos() != 0 {
		t.Fatalf("expected renderer flushed position reset after resize, got %d", seg.StreamRenderer.FlushedRenderedPos())
	}

	tracker.MarkCurrentTextComplete(func(text string) string {
		return RenderMarkdown(text, width)
	})
	tracker.HandleToolStart("call-1", "set_commit_message", "(msg)", nil)

	result := tracker.FlushCompletedNow(width, RenderMarkdown)
	if result.ToPrint == "" {
		t.Fatal("expected completed text to flush at tool boundary")
	}

	output := StripANSI(result.ToPrint)
	if !strings.Contains(output, "Paragraph one.") {
		t.Fatalf("expected first paragraph to be preserved before tool boundary, got:\n%s", output)
	}
	if !strings.Contains(output, "Paragraph two.") {
		t.Fatalf("expected second paragraph to be preserved before tool boundary, got:\n%s", output)
	}
}

// TestDebugStreamingSimulation simulates streaming where text accumulates
// in a single segment. With the streaming renderer, complete blocks are
// rendered immediately as they arrive.
func TestDebugStreamingSimulation(t *testing.T) {
	tracker := NewToolTracker()

	// Simulate streaming text - all goes into one segment
	chunks := []string{
		"# Debug\n\n",
		"This is a test.\n\n",
		"More content here.",
	}

	for _, chunk := range chunks {
		tracker.AddTextSegment(chunk, 80)
	}

	// Should have 1 segment (all text accumulated)
	if len(tracker.Segments) != 1 {
		t.Errorf("Expected 1 segment, got %d", len(tracker.Segments))
	}

	// Segment should be incomplete during streaming
	if tracker.Segments[0].Complete {
		t.Error("Segment should be incomplete during streaming")
	}

	// The streaming renderer should be active
	if tracker.Segments[0].StreamRenderer == nil {
		t.Error("StreamRenderer should be active during streaming")
	}

	// CompletedSegments should include incomplete text for View()
	segments := tracker.CompletedSegments()
	content := RenderSegments(segments, 80, -1, nil, false, false)

	// With the streaming renderer, complete blocks are rendered immediately
	// The heading "# Debug" becomes a styled heading, so check for "Debug" in content
	if !strings.Contains(content, "Debug") {
		t.Errorf("Should contain rendered heading during streaming: %q", content)
	}

	// Mark complete and render (this will flush the streaming renderer)
	tracker.CompleteTextSegments(func(s string) string { return "[FALLBACK:" + s + "]" })

	// Now segment should be complete with rendered content from streaming renderer
	if !tracker.Segments[0].Complete {
		t.Error("Segment should be complete after CompleteTextSegments")
	}
	// Rendered content comes from streaming renderer, not the callback (which is fallback)
	if tracker.Segments[0].Rendered == "" {
		t.Error("Should have rendered content from streaming renderer")
	}
	// Streaming renderer should be nil after completion
	if tracker.Segments[0].StreamRenderer != nil {
		t.Error("StreamRenderer should be nil after completion")
	}
}

// TestStreamingThenComplete verifies that streaming text is rendered progressively
// by the streaming renderer, then finalized on completion.
func TestStreamingThenComplete(t *testing.T) {
	tracker := NewToolTracker()

	// Add streaming content
	tracker.AddTextSegment("# Header\n\n", 80)
	tracker.AddTextSegment("Some **bold** text.\n\n", 80)

	// During streaming, nothing should be flushed (incomplete segment)
	result := tracker.FlushToScrollback(80, 0, 1, func(s string, w int) string {
		return "«" + s + "»"
	})
	if result.ToPrint != "" {
		t.Errorf("Should not flush incomplete segment: %q", result.ToPrint)
	}

	// Streaming renderer is active, so View shows rendered markdown
	segments := tracker.CompletedSegments()
	content := RenderSegments(segments, 80, -1, nil, false, false)
	// The streaming renderer renders complete blocks, so "Header" should be present
	if !strings.Contains(content, "Header") {
		t.Errorf("View should show rendered content during streaming: %q", content)
	}
	// Bold text "**bold**" in a complete paragraph should also be rendered
	if !strings.Contains(content, "bold") {
		t.Errorf("View should contain 'bold' text: %q", content)
	}

	// Complete the segment
	tracker.CompleteTextSegments(func(s string) string {
		return "«RENDERED:" + s + "»"
	})

	// Now it should be flushable
	result = tracker.FlushToScrollback(80, 0, 0, func(s string, w int) string {
		return "«" + s + "»"
	})
	// With minKeep=0, it should flush
	if !tracker.Segments[0].Complete {
		t.Error("Segment should be complete")
	}
}

func TestAddPreRenderedTextSegment(t *testing.T) {
	tracker := NewToolTracker()

	styledText := "\x1b[1;34m❯\x1b[0m hello world\n\n"
	tracker.AddPreRenderedTextSegment(styledText)

	if len(tracker.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(tracker.Segments))
	}

	seg := tracker.Segments[0]

	// Should be a text segment
	if seg.Type != SegmentText {
		t.Errorf("expected SegmentText, got %d", seg.Type)
	}

	// Should be marked complete
	if !seg.Complete {
		t.Error("expected segment to be complete")
	}

	// Should have Rendered set to the styled text
	if seg.Rendered != styledText {
		t.Errorf("expected Rendered=%q, got %q", styledText, seg.Rendered)
	}

	// Should have Text set for fallback
	if seg.Text != styledText {
		t.Errorf("expected Text=%q, got %q", styledText, seg.Text)
	}

	// Version should be incremented
	if tracker.Version == 0 {
		t.Error("expected Version to be incremented")
	}
}

func TestAddPreRenderedTextSegment_BypassesMarkdownRendering(t *testing.T) {
	tracker := NewToolTracker()

	styledText := "\x1b[1;34m❯\x1b[0m stop doing that\n\n"
	tracker.AddPreRenderedTextSegment(styledText)

	// Render through RenderSegments with a markdown function that should NOT be called
	segments := make([]*Segment, len(tracker.Segments))
	for i := range tracker.Segments {
		segments[i] = &tracker.Segments[i]
	}
	rendered := RenderSegments(segments, 80, -1, func(s string, w int) string {
		return "MARKDOWN_PROCESSED:" + s
	}, false, false)

	// Should NOT contain "MARKDOWN_PROCESSED" since pre-rendered segments
	// already have Rendered set and bypass markdown rendering
	if strings.Contains(rendered, "MARKDOWN_PROCESSED") {
		t.Error("pre-rendered segment should not pass through markdown renderer")
	}

	// Should contain the original styled text
	if !strings.Contains(rendered, "stop doing that") {
		t.Error("expected rendered output to contain original text")
	}
}

func TestAddPreRenderedTextSegment_SurvivesFlush(t *testing.T) {
	tracker := NewToolTracker()

	// Add some normal text first
	tracker.AddTextSegment("Normal text before.", 80)
	tracker.MarkCurrentTextComplete(func(text string) string {
		return "rendered:" + text
	})

	// Add pre-rendered interjection
	styledText := "\x1b[1;34m❯\x1b[0m my interjection\n\n"
	tracker.AddPreRenderedTextSegment(styledText)

	// Flush to scrollback
	result := tracker.FlushToScrollback(80, 0, 0, func(s string, w int) string {
		return "md:" + s
	})

	// Flushed content should contain the original styled text without re-rendering
	if !strings.Contains(result.ToPrint, "my interjection") {
		t.Errorf("expected flushed output to contain interjection text, got: %q", result.ToPrint)
	}

	// Should NOT contain "md:" prefix for the interjection (it has Rendered set)
	// The pre-rendered segment should be used as-is
	if strings.Contains(result.ToPrint, "md:\x1b[1;34m") {
		t.Error("pre-rendered segment should not pass through markdown renderer during flush")
	}
}

func TestAddPreRenderedTextSegment_RenderUnflushed(t *testing.T) {
	tracker := NewToolTracker()

	styledText := "\x1b[1;34m❯\x1b[0m queued message\n\n"
	tracker.AddPreRenderedTextSegment(styledText)

	// RenderUnflushed should return the styled text without re-rendering
	content := tracker.RenderUnflushed(80, func(s string, w int) string {
		return "SHOULD_NOT_APPEAR:" + s
	}, false)

	if strings.Contains(content, "SHOULD_NOT_APPEAR") {
		t.Error("pre-rendered segment should not pass through markdown renderer in RenderUnflushed")
	}

	if !strings.Contains(content, "queued message") {
		t.Error("expected RenderUnflushed to contain the pre-rendered text")
	}
}
