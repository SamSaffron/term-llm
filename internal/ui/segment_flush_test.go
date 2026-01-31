package ui

import (
	"strings"
	"testing"
)

func TestFlushSpacingConsistency(t *testing.T) {
	// mockRender mimics RenderMarkdown's trimming behavior (no trailing newline).
	mockRender := func(s string, w int) string {
		return strings.TrimSpace(s)
	}

	tests := []struct {
		name     string
		segments []Segment
	}{
		{
			name: "text then tool",
			segments: []Segment{
				{Type: SegmentText, Text: "Some text", Complete: true},
				{Type: SegmentTool, ToolName: "test_tool", ToolStatus: ToolSuccess, Complete: true},
			},
		},
		{
			name: "tool then text",
			segments: []Segment{
				{Type: SegmentTool, ToolName: "test_tool", ToolStatus: ToolSuccess, Complete: true},
				{Type: SegmentText, Text: "More text", Complete: true},
			},
		},
		{
			name: "tool then tool",
			segments: []Segment{
				{Type: SegmentTool, ToolName: "tool1", ToolStatus: ToolSuccess, Complete: true},
				{Type: SegmentTool, ToolName: "tool2", ToolStatus: ToolSuccess, Complete: true},
			},
		},
		{
			name: "text then tool then text",
			segments: []Segment{
				{Type: SegmentText, Text: "Intro", Complete: true},
				{Type: SegmentTool, ToolName: "mid_tool", ToolStatus: ToolSuccess, Complete: true},
				{Type: SegmentText, Text: "Outro", Complete: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. Get reference output from full render
			refSegs := make([]*Segment, len(tt.segments))
			for i := range tt.segments {
				refSegs[i] = &tt.segments[i]
			}
			reference := RenderSegments(refSegs, 80, -1, mockRender, true)

			// 2. Get concatenated output from individual flushes
			var concatenated strings.Builder
			tracker := NewToolTracker()
			// Need to deep copy segments or at least ensure we don't modify tt.segments
			tracker.Segments = make([]Segment, len(tt.segments))
			copy(tracker.Segments, tt.segments)

			for i := 0; i < len(tt.segments); i++ {
				// FlushToScrollback(width, printedLines, maxViewLines, renderMd)
				// We want it to flush EXACTLY one segment if possible, or just let it flush what it wants
				// until everything is flushed.
				res := tracker.FlushToScrollback(80, 0, len(tt.segments)-i-1, mockRender)
				concatenated.WriteString(res.ToPrint)
			}

			if concatenated.String() != reference {
				t.Errorf("Concatenated flush output does not match reference.\nReference:\n%q\nConcatenated:\n%q", reference, concatenated.String())
			}
		})
	}
}

func TestPartialTextFlushSpacing(t *testing.T) {
	renderMd := func(s string, w int) string {
		return RenderMarkdown(s, w)
	}

	tracker := NewToolTracker()
	// Add text that will be flushed in two parts
	tracker.AddTextSegment("First part.\n\n", 80)

	// Flush first part
	res1 := tracker.FlushStreamingText(0, 80, renderMd)

	// Add second part
	tracker.AddTextSegment("Second part.", 80)
	tracker.CompleteTextSegments(func(s string) string { return renderMd(s, 80) })

	// Flush remaining of segment 0
	res2 := tracker.FlushAllRemaining(80, 0, renderMd)

	// Add a tool
	tracker.HandleToolStart("tool-1", "tool", "info")
	tracker.HandleToolEnd("tool-1", true)

	// Flush tool
	res3 := tracker.FlushAllRemaining(80, 0, renderMd)

	total := res1.ToPrint + res2.ToPrint + res3.ToPrint

	// Reference:
	refTracker := NewToolTracker()
	refTracker.AddTextSegment("First part.\n\nSecond part.", 80)
	refTracker.CompleteTextSegments(func(s string) string { return renderMd(s, 80) })
	refTracker.HandleToolStart("tool-1", "tool", "info")
	refTracker.HandleToolEnd("tool-1", true)

	refSegs := make([]*Segment, len(refTracker.Segments))
	for i := range refTracker.Segments {
		refSegs[i] = &refTracker.Segments[i]
	}
	reference := RenderSegments(refSegs, 80, -1, renderMd, true)

	if total != reference {
		t.Errorf("Partial flush concatenated output mismatch.\nReference:\n%q\nTotal:\n%q", reference, total)
	}
}

func TestStreamingFlushNoDuplication(t *testing.T) {
	// Use real markdown rendering to ensure streaming renderer is exercised
	renderMd := func(s string, w int) string {
		return RenderMarkdown(s, w)
	}

	tracker := NewToolTracker()
	width := 80

	// 1. Add first part of markdown (a header and start of a paragraph)
	part1 := "# Title\n\nThis is the first "
	tracker.AddTextSegment(part1, width)

	// It WILL flush "# Title\n\n" because it's a safe boundary
	res1 := tracker.FlushStreamingText(0, width, renderMd)
	if res1.ToPrint == "" {
		t.Error("Expected to flush Title header")
	}

	// 2. Add more text to complete the paragraph
	part2 := "paragraph.\n\n"
	tracker.AddTextSegment(part2, width)

	// Now it should flush the rest of the first paragraph
	res2 := tracker.FlushStreamingText(0, width, renderMd)
	if res2.ToPrint == "" {
		t.Error("Expected to flush completed paragraph")
	}

	// 3. Add second paragraph
	part3 := "This is the second paragraph.\n\n"
	tracker.AddTextSegment(part3, width)

	// Flush again
	res3 := tracker.FlushStreamingText(0, width, renderMd)
	if res3.ToPrint == "" {
		t.Error("Expected to flush second paragraph")
	}

	// 4. Complete and flush remaining
	tracker.CompleteTextSegments(func(s string) string { return renderMd(s, width) })
	res4 := tracker.FlushAllRemaining(width, 0, renderMd)

	totalOutput := res1.ToPrint + res2.ToPrint + res3.ToPrint + res4.ToPrint

	// Strip ANSI for easier counting
	stripANSI := func(s string) string {
		var b strings.Builder
		inEscape := false
		for i := 0; i < len(s); i++ {
			if s[i] == '\x1b' {
				inEscape = true
				continue
			}
			if inEscape {
				if (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z') {
					inEscape = false
				}
				continue
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}

	plainTotal := stripANSI(totalOutput)

	// Check for duplication of "Title" or "first paragraph"
	if strings.Count(plainTotal, "Title") != 1 {
		t.Errorf("'Title' appeared %d times in output. Total output: %q", strings.Count(plainTotal, "Title"), plainTotal)
	}
	if !strings.Contains(strings.ToLower(plainTotal), "first paragraph") {
		t.Errorf("'first paragraph' not found in output: %q", plainTotal)
	}
	if strings.Count(strings.ToLower(plainTotal), "first paragraph") > 1 {
		t.Errorf("'first paragraph' duplicated: %d times", strings.Count(strings.ToLower(plainTotal), "first paragraph"))
	}

	t.Logf("Total output length: %d", len(totalOutput))
}

func TestFallbackFlushPreservesHeadingSpacing(t *testing.T) {
	width := 80
	renderMd := func(s string, w int) string {
		return RenderMarkdown(s, w)
	}

	tracker := NewToolTracker()
	tracker.TextMode = true // force fallback flush path (no streaming renderer)

	input := "### Header 3\n#### Header 4\n"

	tracker.AddTextSegment("### Header 3\n", width)
	res1 := tracker.FlushStreamingText(0, width, renderMd)

	tracker.AddTextSegment("#### Header 4\n", width)
	res2 := tracker.FlushStreamingText(0, width, renderMd)

	tracker.CompleteTextSegments(func(s string) string { return renderMd(s, width) })
	res3 := tracker.FlushAllRemaining(width, 0, renderMd)

	total := res1.ToPrint + res2.ToPrint + res3.ToPrint
	reference := RenderMarkdown(input, width)

	if StripANSI(total) != StripANSI(reference) {
		t.Fatalf("Fallback flush output mismatch.\nReference:\n%q\nTotal:\n%q", StripANSI(reference), StripANSI(total))
	}
}
