package ui

import (
	"fmt"
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
			reference := RenderSegments(refSegs, 80, -1, mockRender, true, false)

			// 2. Get concatenated output from individual flushes.
			// Simulate tea.Printf's trailing newline between flushes.
			var parts []string
			tracker := NewToolTracker()
			tracker.Segments = make([]Segment, len(tt.segments))
			copy(tracker.Segments, tt.segments)

			for i := 0; i < len(tt.segments); i++ {
				res := tracker.FlushToScrollback(80, 0, len(tt.segments)-i-1, mockRender)
				if res.ToPrint != "" {
					parts = append(parts, res.ToPrint)
				}
			}

			// Join with "\n" to simulate tea.Printf's trailing newline
			concatenated := strings.Join(parts, "\n")

			if concatenated != reference {
				t.Errorf("Concatenated flush output does not match reference.\nReference:\n%q\nConcatenated:\n%q", reference, concatenated)
			}
		})
	}
}

func TestFlushToolToTool_NoBlankLine(t *testing.T) {
	// Regression test: when two tool segments are flushed separately via tea.Printf,
	// there should be exactly one newline between them (no blank line).
	// tea.Printf adds a trailing \n after each flush, so FlushLeadingSeparator
	// must strip the leading \n from the separator to avoid \n\n.
	tracker := NewToolTracker()
	tracker.HandleToolStart("t1", "web_search", "query one", nil)
	tracker.HandleToolEnd("t1", true)

	// Flush first tool
	res1 := tracker.FlushCompletedNow(80, nil)
	if res1.ToPrint == "" {
		t.Fatal("expected first tool to flush")
	}

	tracker.HandleToolStart("t2", "web_search", "query two", nil)
	tracker.HandleToolEnd("t2", true)

	// Flush second tool
	res2 := tracker.FlushCompletedNow(80, nil)
	if res2.ToPrint == "" {
		t.Fatal("expected second tool to flush")
	}

	// Simulate tea.Printf: each flush gets a trailing \n
	output := res1.ToPrint + "\n" + res2.ToPrint + "\n"
	stripped := stripAnsiForTest(output)

	// Find the two tool indicators
	first := strings.Index(stripped, "web_search")
	if first == -1 {
		t.Fatalf("expected first 'web_search' in output, got: %q", stripped)
	}
	rest := stripped[first+len("web_search"):]
	second := strings.Index(rest, "web_search")
	if second == -1 {
		t.Fatalf("expected second 'web_search' in output, got: %q", stripped)
	}
	between := rest[:second]

	// Should be exactly one newline between (tool line ending + tea \n),
	// not two (which would be a blank line).
	newlines := strings.Count(between, "\n")
	if newlines != 1 {
		t.Errorf("expected 1 newline between tool segments, got %d\nbetween: %q\nfull output: %q",
			newlines, between, stripped)
	}
}

func TestFlushCompletedNow_ToolToTextAcrossPrintBoundaries_UsesBlankLine(t *testing.T) {
	// Regression test: tool content and subsequent text are often flushed by
	// separate tea.Printf calls. The combined output should still have exactly
	// one blank line between tool and text.
	mockRender := func(s string, w int) string {
		return "\n\n" + strings.TrimSpace(s)
	}

	tracker := NewToolTracker()
	tracker.HandleToolStart("t1", "web_search", "query", nil)
	tracker.HandleToolEnd("t1", true)

	toolFlush := tracker.FlushCompletedNow(80, mockRender)
	if toolFlush.ToPrint == "" {
		t.Fatal("expected tool flush output")
	}

	tracker.AddTextSegment("Latest Claude update.", 80)
	tracker.CompleteTextSegments(func(s string) string {
		return mockRender(s, 80)
	})

	textFlush := tracker.FlushCompletedNow(80, mockRender)
	if textFlush.ToPrint == "" {
		t.Fatal("expected text flush output")
	}

	// Simulate tea.Printf trailing newlines between flush calls.
	output := toolFlush.ToPrint + "\n" + textFlush.ToPrint + "\n"
	stripped := stripAnsiForTest(output)

	toolIdx := strings.Index(stripped, "web_search")
	if toolIdx == -1 {
		t.Fatalf("expected tool name in output, got: %q", stripped)
	}
	textIdx := strings.Index(stripped, "Latest Claude update.")
	if textIdx == -1 {
		t.Fatalf("expected text in output, got: %q", stripped)
	}
	if toolIdx >= textIdx {
		t.Fatalf("expected tool before text, tool=%d text=%d output=%q", toolIdx, textIdx, stripped)
	}

	between := stripped[toolIdx+len("web_search") : textIdx]
	if newlines := strings.Count(between, "\n"); newlines != 2 {
		t.Fatalf("expected exactly 2 newlines between tool and text (blank line), got %d\nbetween: %q\nfull: %q", newlines, between, stripped)
	}
}

func TestFlushStreamingText_ToolToTextCompactsLeadingBlankLinesInRenderedText(t *testing.T) {
	// Regression test: even when rendered text starts with blank lines,
	// tool->text boundaries should remain a single blank line.
	width := 80
	renderMd := func(s string, w int) string {
		return "\n\n" + strings.TrimSpace(s)
	}

	tracker := NewToolTracker()
	tracker.TextMode = true // force fallback renderMd path
	tracker.HandleToolStart("t1", "web_search", "query", nil)
	tracker.HandleToolEnd("t1", true)
	tracker.AddTextSegment("Latest Claude update.\n\n", width)

	res := tracker.FlushStreamingText(0, width, renderMd)
	if res.ToPrint == "" {
		t.Fatal("expected tool+text flush")
	}

	stripped := stripAnsiForTest(res.ToPrint)
	toolIdx := strings.Index(stripped, "web_search")
	if toolIdx == -1 {
		t.Fatalf("expected tool name in output, got: %q", stripped)
	}
	textIdx := strings.Index(stripped, "Latest Claude update.")
	if textIdx == -1 {
		t.Fatalf("expected text in output, got: %q", stripped)
	}
	if toolIdx >= textIdx {
		t.Fatalf("expected tool before text, tool=%d text=%d output=%q", toolIdx, textIdx, stripped)
	}

	between := stripped[toolIdx+len("web_search") : textIdx]
	if newlines := strings.Count(between, "\n"); newlines != 2 {
		t.Fatalf("expected exactly 2 newlines between tool and text (blank line), got %d\nbetween: %q\nfull: %q", newlines, between, stripped)
	}
}

func TestFlushStreamingText_ToolAndTextInOnePrint(t *testing.T) {
	// Regression test: when FlushStreamingText returns both a completed tool
	// and threshold-triggered text in a single ToPrint, the tool→text separator
	// must use the full SegmentSeparator (not FlushLeadingSeparator) since no
	// tea.Printf newline occurs between them within the same ToPrint.
	width := 80
	renderMd := func(s string, w int) string {
		return RenderMarkdown(s, w)
	}

	tracker := NewToolTracker()

	// Add a completed tool segment
	tracker.HandleToolStart("t1", "web_search", "query", nil)
	tracker.HandleToolEnd("t1", true)

	// Add enough text to exceed a 0 threshold
	tracker.AddTextSegment("Hello world.\n\nSecond paragraph.\n\n", width)

	// Single FlushStreamingText call should return both tool and text
	res := tracker.FlushStreamingText(0, width, renderMd)
	if res.ToPrint == "" {
		t.Fatal("expected tool+text flush")
	}

	stripped := stripAnsiForTest(res.ToPrint)

	// Both tool and text should be present
	toolIdx := strings.Index(stripped, "web_search")
	if toolIdx == -1 {
		t.Fatalf("expected tool in output, got: %q", stripped)
	}
	textIdx := strings.Index(stripped, "Hello world")
	if textIdx == -1 {
		t.Fatalf("expected text in output, got: %q", stripped)
	}
	if toolIdx >= textIdx {
		t.Errorf("tool should appear before text, tool at %d, text at %d", toolIdx, textIdx)
	}

	// Check spacing between tool line and text: should be one blank line
	// since SegmentSeparator(SegmentTool, SegmentText) = "\n\n"
	between := stripped[toolIdx:textIdx]
	newlines := strings.Count(between, "\n")
	if newlines != 2 {
		t.Errorf("expected 2 newlines between tool and text (blank line), got %d\nbetween: %q\nfull: %q",
			newlines, between, stripped)
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
	tracker.HandleToolStart("tool-1", "tool", "info", nil)
	tracker.HandleToolEnd("tool-1", true)

	// Flush tool
	res3 := tracker.FlushAllRemaining(80, 0, renderMd)

	// Simulate tea.Printf which adds newline after each print.
	// Our stripping compensates for this.
	var total strings.Builder
	if res1.ToPrint != "" {
		total.WriteString(res1.ToPrint)
		total.WriteString("\n")
	}
	if res2.ToPrint != "" {
		total.WriteString(res2.ToPrint)
		total.WriteString("\n")
	}
	if res3.ToPrint != "" {
		total.WriteString(res3.ToPrint)
		total.WriteString("\n")
	}

	output := total.String()

	// Verify essential content is present
	if !strings.Contains(output, "First") {
		t.Error("Missing 'First' in output")
	}
	if !strings.Contains(output, "Second") {
		t.Error("Missing 'Second' in output")
	}
	if !strings.Contains(output, "●") {
		t.Error("Missing tool indicator in output")
	}

	// Verify there's proper separation between text and tool.
	// Strip ANSI codes first to make checking easier
	stripped := stripAnsiForTest(output)
	toolIdx := strings.Index(stripped, "●")
	if toolIdx > 0 {
		before := stripped[:toolIdx]
		// Count consecutive newlines before tool
		newlineCount := 0
		for i := len(before) - 1; i >= 0; i-- {
			if before[i] == '\n' {
				newlineCount++
			} else {
				break
			}
		}
		// Compact spacing should avoid runaway blank lines while preserving
		// existing text trailing newlines in edge cases.
		if newlineCount < 1 || newlineCount > 2 {
			t.Errorf("Expected 1-2 newlines before tool indicator, got %d\nStripped output before tool: %q", newlineCount, before[max(0, len(before)-50):])
		}
	}
}

func TestRenderUnflushed_ShowsPendingParagraphContent(t *testing.T) {
	tracker := NewToolTracker()
	width := 80

	tracker.AddTextSegment("This paragraph should stream before block completion", width)

	output := stripAnsi(tracker.RenderUnflushed(width, RenderMarkdown, false))
	if !strings.Contains(output, "This paragraph should stream before block completion") {
		t.Fatalf("expected pending paragraph content to be visible, got %q", output)
	}
}

func TestRenderUnflushed_HidesPendingTableContent(t *testing.T) {
	tracker := NewToolTracker()
	width := 80

	tracker.AddTextSegment("| A | B |\n|---|---|\n| 1 |", width)

	output := stripAnsi(tracker.RenderUnflushed(width, RenderMarkdown, false))
	if strings.Contains(output, "| A | B |") || strings.Contains(output, "|---|---|") {
		t.Fatalf("expected pending table lines to stay hidden, got %q", output)
	}
}

func TestRenderUnflushed_HidesPendingListMarkerContent(t *testing.T) {
	tracker := NewToolTracker()
	width := 80

	tracker.AddTextSegment("1. ", width)

	output := stripAnsi(tracker.RenderUnflushed(width, RenderMarkdown, false))
	if strings.Contains(output, "1.") {
		t.Fatalf("expected pending list marker to stay hidden, got %q", output)
	}
}

func TestRenderUnflushed_ShowsPendingAsteriskPrefix(t *testing.T) {
	tracker := NewToolTracker()
	width := 80

	tracker.AddTextSegment("*", width)

	output := stripAnsi(tracker.RenderUnflushed(width, RenderMarkdown, false))
	if !strings.Contains(output, "*") {
		t.Fatalf("expected pending asterisk prefix to remain visible, got %q", output)
	}
}

// stripAnsiForTest removes ANSI escape sequences for testing
func stripAnsiForTest(s string) string {
	result := ""
	inEscape := false
	for _, c := range s {
		if c == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				inEscape = false
			}
			continue
		}
		result += string(c)
	}
	return result
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

func TestFlushStreamingText_FlushesCompletedToolsFirst(t *testing.T) {
	// Test that FlushStreamingText flushes completed tool segments
	// that appear before the current text segment.
	width := 80
	renderMd := func(s string, w int) string {
		return RenderMarkdown(s, w)
	}

	tracker := NewToolTracker()

	// Add a tool segment and mark it complete
	tracker.HandleToolStart("tool-1", "test_tool", "test info", nil)
	tracker.HandleToolEnd("tool-1", true)

	// Add an incomplete text segment with content
	tracker.AddTextSegment("Some streaming text.\n\n", width)

	// FlushStreamingText should flush the tool first, then the text
	res := tracker.FlushStreamingText(0, width, renderMd)

	output := res.ToPrint

	// Verify tool segment content appears in output
	stripped := stripAnsiForTest(output)
	if !strings.Contains(stripped, "●") {
		t.Error("Expected tool indicator (●) in output, tool segment was not flushed")
	}
	if !strings.Contains(stripped, "test_tool") {
		t.Error("Expected 'test_tool' in output")
	}
	if !strings.Contains(stripped, "Some streaming text") {
		t.Error("Expected 'Some streaming text' in output")
	}

	// Verify tool appears before text
	toolIdx := strings.Index(stripped, "●")
	textIdx := strings.Index(stripped, "Some streaming text")
	if toolIdx > textIdx {
		t.Errorf("Tool should appear before text. Tool at %d, text at %d", toolIdx, textIdx)
	}

	// Verify the tool segment is marked as flushed
	if !tracker.Segments[0].Flushed {
		t.Error("Tool segment should be marked as flushed")
	}
}

func TestFlushStreamingText_ReturnsToolsEvenWhenTextBelowThreshold(t *testing.T) {
	// Test that completed tools are returned even when text is below threshold
	width := 80

	tracker := NewToolTracker()

	// Add a tool segment and mark it complete
	tracker.HandleToolStart("tool-1", "test_tool", "test info", nil)
	tracker.HandleToolEnd("tool-1", true)

	// Add an incomplete text segment with minimal content
	tracker.AddTextSegment("Hi", width)

	// Use high threshold so text won't be flushed, but tool should still be
	res := tracker.FlushStreamingText(1000, width, nil)

	output := res.ToPrint
	stripped := stripAnsiForTest(output)

	// Tool should be flushed even though text is below threshold
	if !strings.Contains(stripped, "●") {
		t.Error("Expected tool indicator (●) in output even when text is below threshold")
	}
	if !strings.Contains(stripped, "test_tool") {
		t.Error("Expected 'test_tool' in output")
	}

	// Text should NOT be in output since it's below threshold
	if strings.Contains(stripped, "Hi") {
		t.Error("Text should not be flushed when below threshold")
	}
}

func TestStreamingFlush_CodeBlockThenText(t *testing.T) {
	md := "2. **Enforce changeset URL requirements at the model layer**\n" +
		"    The controller validates `changeset_url`.\n" +
		"    **Fix:** Add a model validation, e.g.:\n" +
		"    ```ruby\n" +
		"    validate :changeset_url_valid_for_fixed\n" +
		"    \n" +
		"    errors.add(:resolution_changeset_url, \"must be a valid URL\") unless\n" +
		"    resolution_changeset_url =~ /\\Ahttps?:\\/\\//\n" +
		"    end\n" +
		"    ```\n" +
		"    This keeps the constraint consistent no matter how the model is updated.\n" +
		"\n" +
		"3. **Ignore changeset URLs when resolving as invalid**\n" +
		"    In `HotOrNotController#resolve` (`app/controllers/hot_or_not_controller.rb:194-206`), a malicious request can still pass a `changeset_url` while resolving as invalid.\n"

	tracker := NewToolTracker()
	width := 80
	chunkSize := 24

	var allPrinted strings.Builder
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
		result := tracker.FlushStreamingText(0, width, RenderMarkdown)
		if result.ToPrint != "" {
			allPrinted.WriteString(result.ToPrint)
			allPrinted.WriteString("\n") // tea.Printf adds newline after each flush
		}
	}

	tracker.CompleteTextSegments(func(text string) string {
		return RenderMarkdown(text, width)
	})
	result := tracker.FlushAllRemaining(width, 0, RenderMarkdown)
	if result.ToPrint != "" {
		allPrinted.WriteString(result.ToPrint)
		allPrinted.WriteString("\n")
	}

	output := stripAnsi(allPrinted.String())
	if !strings.Contains(output, "This keeps the constraint consistent no matter how the model is updated.") {
		t.Fatalf("Expected paragraph after code block to be present in output.\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "changeset_url") {
		t.Fatalf("Expected inline code `changeset_url` to be present in output.\nOutput:\n%s", output)
	}
}

func TestStreamingFlush_BlockquoteThenText(t *testing.T) {
	md := "1. **Has quote**\n" +
		"    > quoted line\n" +
		"    > more quote\n" +
		"    This keeps the list item going after the quote.\n" +
		"\n" +
		"2. **Next item**\n"

	tracker := NewToolTracker()
	width := 80
	chunkSize := 20

	var allPrinted strings.Builder
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
		result := tracker.FlushStreamingText(0, width, RenderMarkdown)
		if result.ToPrint != "" {
			allPrinted.WriteString(result.ToPrint)
			allPrinted.WriteString("\n")
		}
	}

	tracker.CompleteTextSegments(func(text string) string {
		return RenderMarkdown(text, width)
	})
	result := tracker.FlushAllRemaining(width, 0, RenderMarkdown)
	if result.ToPrint != "" {
		allPrinted.WriteString(result.ToPrint)
		allPrinted.WriteString("\n")
	}

	output := stripAnsi(allPrinted.String())
	if !strings.Contains(output, "This keeps the list item going after the quote.") {
		t.Fatalf("Expected paragraph after blockquote to be present in output.\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "Next item") {
		t.Fatalf("Expected following list item to be present in output.\nOutput:\n%s", output)
	}
}

func TestStreamingFlush_TableThenText(t *testing.T) {
	md := "1. **Has table**\n" +
		"    | A | B |\n" +
		"    |---|---|\n" +
		"    | 1 | 2 |\n" +
		"    After the table, more text in the same item.\n" +
		"\n" +
		"2. **Next item**\n"

	tracker := NewToolTracker()
	width := 80
	chunkSize := 20

	var allPrinted strings.Builder
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
		result := tracker.FlushStreamingText(0, width, RenderMarkdown)
		if result.ToPrint != "" {
			allPrinted.WriteString(result.ToPrint)
			allPrinted.WriteString("\n")
		}
	}

	tracker.CompleteTextSegments(func(text string) string {
		return RenderMarkdown(text, width)
	})
	result := tracker.FlushAllRemaining(width, 0, RenderMarkdown)
	if result.ToPrint != "" {
		allPrinted.WriteString(result.ToPrint)
		allPrinted.WriteString("\n")
	}

	output := stripAnsi(allPrinted.String())
	if !strings.Contains(output, "After the table, more text in the same item.") {
		t.Fatalf("Expected paragraph after table to be present in output.\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "Next item") {
		t.Fatalf("Expected following list item to be present in output.\nOutput:\n%s", output)
	}
}

func TestStreamingFlush_OrderedNestedListRemainsTight(t *testing.T) {
	md := "1. First numbered item\n" +
		"2. Second numbered item with ~~strikethrough~~\n" +
		"3. Third numbered item\n" +
		"   1. Nested numbered one\n" +
		"   2. Nested numbered two\n" +
		"4. Fourth numbered item\n"

	tracker := NewToolTracker()
	width := 80
	chunkSize := 18

	var allPrinted strings.Builder
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
		result := tracker.FlushStreamingText(0, width, RenderMarkdown)
		if result.ToPrint != "" {
			allPrinted.WriteString(result.ToPrint)
			allPrinted.WriteString("\n") // tea.Printf appends newline
		}
	}

	tracker.CompleteTextSegments(func(text string) string {
		return RenderMarkdown(text, width)
	})
	result := tracker.FlushAllRemaining(width, 0, RenderMarkdown)
	if result.ToPrint != "" {
		allPrinted.WriteString(result.ToPrint)
		allPrinted.WriteString("\n")
	}

	output := stripAnsi(allPrinted.String())
	lines := strings.Split(output, "\n")
	nestedOne := -1
	nestedTwo := -1
	for i, line := range lines {
		if nestedOne == -1 && strings.Contains(line, "Nested numbered one") {
			nestedOne = i
		}
		if nestedTwo == -1 && strings.Contains(line, "Nested numbered two") {
			nestedTwo = i
		}
	}

	if nestedOne == -1 || nestedTwo == -1 {
		t.Fatalf("expected both nested lines in output:\n%s", output)
	}
	if nestedTwo != nestedOne+1 {
		t.Fatalf("expected tight nested list (adjacent lines), got spacing %d lines apart\nOutput:\n%s", nestedTwo-nestedOne, output)
	}
}

func TestStreamingFlush_UserReproKeepsOrderedItemsAfterNestedList(t *testing.T) {
	md := "1. **Step one**: Initialize the system with `init()`\n" +
		"2. **Step two**: Configure the settings\n" +
		"   - Set `timeout=30`\n" +
		"   - Enable `debug=true`\n" +
		"3. **Step three**: Run the main loop\n" +
		"4. **Step four**: Cleanup and exit\n" +
		"\n" +
		"> **Summary:** This debug output contains headers, code blocks, lists, tables, blockquotes, and various inline formatting elements to thoroughly test markdown rendering performance.\n"

	tracker := NewToolTracker()
	width := 80
	chunkSize := 24

	var allPrinted strings.Builder
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
		result := tracker.FlushStreamingText(0, width, RenderMarkdown)
		if result.ToPrint != "" {
			allPrinted.WriteString(result.ToPrint)
			allPrinted.WriteString("\n")
		}
	}

	tracker.CompleteTextSegments(func(text string) string {
		return RenderMarkdown(text, width)
	})
	result := tracker.FlushAllRemaining(width, 0, RenderMarkdown)
	if result.ToPrint != "" {
		allPrinted.WriteString(result.ToPrint)
		allPrinted.WriteString("\n")
	}

	output := stripAnsi(allPrinted.String())
	if !strings.Contains(output, "Step three") {
		t.Fatalf("expected Step three to remain visible\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "Step four") {
		t.Fatalf("expected Step four to remain visible\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "Summary:") {
		t.Fatalf("expected summary blockquote to remain visible\nOutput:\n%s", output)
	}
}

func TestStreamingFlush_HeadingThenText(t *testing.T) {
	md := "1. **Has heading**\n" +
		"    ### Nested heading\n" +
		"    Text after heading in the same item.\n" +
		"\n" +
		"2. **Next item**\n"

	tracker := NewToolTracker()
	width := 80
	chunkSize := 20

	var allPrinted strings.Builder
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
		result := tracker.FlushStreamingText(0, width, RenderMarkdown)
		if result.ToPrint != "" {
			allPrinted.WriteString(result.ToPrint)
			allPrinted.WriteString("\n")
		}
	}

	tracker.CompleteTextSegments(func(text string) string {
		return RenderMarkdown(text, width)
	})
	result := tracker.FlushAllRemaining(width, 0, RenderMarkdown)
	if result.ToPrint != "" {
		allPrinted.WriteString(result.ToPrint)
		allPrinted.WriteString("\n")
	}

	output := stripAnsi(allPrinted.String())
	if !strings.Contains(output, "Text after heading in the same item.") {
		t.Fatalf("Expected paragraph after heading to be present in output.\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "Next item") {
		t.Fatalf("Expected following list item to be present in output.\nOutput:\n%s", output)
	}
}

func TestStreamingFlush_SuggestionsKeepsAllNumberedItems(t *testing.T) {
	md := "## Suggestions\n\n" +
		"1. Major: down migration does not faithfully restore prior schema constraints\n" +
		"  - Where: db/post_migrate/20260213000001_drop_patch_notes.rb:21-22\n" +
		"  - Issue: patch_note_id is recreated as non-unique index, while original schema created a unique index\n" +
		"  (db/migrate/20260210000002_create_patch_discussion_links.rb:19 from history).\n" +
		"  - Risk: Rollback allows duplicate links per legacy note and diverges from previous behavior.\n" +
		"  - Fix: Recreate the index with unique: true (and match original column type/constraints as closely as possible).\n\n" +
		"2. Major: get_patch response contract changed (notes_summary.unhandled_notes removed)\n" +
		"  - Where: app/controllers/patch_triage_controller.rb:923-926\n" +
		"  - Issue: notes_summary now exposes counts only; previous payload included unhandled_notes.\n" +
		"  - Risk: External clients/scripts relying on the old field will break silently.\n" +
		"  - Fix: Either preserve unhandled_notes for one deprecation window (from discussion links), or explicitly version/document\n" +
		"  the API contract change.\n\n" +
		"3. Major (verify): possible ID field mismatch in automation script when marking handled posts\n" +
		"  - Where: scan/handle_patch_notes:364-372\n" +
		"  - Issue: Script sends post_id=#{post[\"id\"]} to mark_post_handled.\n" +
		"  - Risk: If get_discussion returns a separate post_id field (common in this codebase), this may target wrong records.\n" +
		"  - Fix: Prefer post[\"post_id\"] || post[\"id\"] and fail fast on unsuccessful tool responses.\n"

	chunkSizes := []int{7, 11, 17, 23, 37, 53}
	for _, chunkSize := range chunkSizes {
		t.Run(fmt.Sprintf("chunk_%d", chunkSize), func(t *testing.T) {
			tracker := NewToolTracker()
			width := 96

			var allPrinted strings.Builder
			for i := 0; i < len(md); i += chunkSize {
				end := i + chunkSize
				if end > len(md) {
					end = len(md)
				}
				chunk := md[i:end]
				tracker.AddTextSegment(chunk, width)
				result := tracker.FlushStreamingText(0, width, RenderMarkdown)
				if result.ToPrint != "" {
					allPrinted.WriteString(result.ToPrint)
					allPrinted.WriteString("\n")
				}
			}

			tracker.CompleteTextSegments(func(text string) string {
				return RenderMarkdown(text, width)
			})
			result := tracker.FlushAllRemaining(width, 0, RenderMarkdown)
			if result.ToPrint != "" {
				allPrinted.WriteString(result.ToPrint)
				allPrinted.WriteString("\n")
			}

			output := stripAnsi(allPrinted.String())

			i1 := strings.Index(output, "1. Major: down migration")
			i2 := strings.Index(output, "2. Major: get_patch response contract changed")
			i3 := strings.Index(output, "3. Major (verify): possible ID field mismatch")
			if i1 == -1 || i2 == -1 || i3 == -1 {
				t.Fatalf("expected all numbered findings, got:\n%s", output)
			}
			if !(i1 < i2 && i2 < i3) {
				t.Fatalf("expected numbered findings in order, indexes=%d,%d,%d\nOutput:\n%s", i1, i2, i3, output)
			}
		})
	}
}

func TestStreamingFlush_ThematicBreakThenText(t *testing.T) {
	md := "1. **Has break**\n" +
		"    ---\n" +
		"    Text after break in the same item.\n" +
		"\n" +
		"2. **Next item**\n"

	tracker := NewToolTracker()
	width := 80
	chunkSize := 20

	var allPrinted strings.Builder
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
		result := tracker.FlushStreamingText(0, width, RenderMarkdown)
		if result.ToPrint != "" {
			allPrinted.WriteString(result.ToPrint)
			allPrinted.WriteString("\n")
		}
	}

	tracker.CompleteTextSegments(func(text string) string {
		return RenderMarkdown(text, width)
	})
	result := tracker.FlushAllRemaining(width, 0, RenderMarkdown)
	if result.ToPrint != "" {
		allPrinted.WriteString(result.ToPrint)
		allPrinted.WriteString("\n")
	}

	output := stripAnsi(allPrinted.String())
	if !strings.Contains(output, "Text after break in the same item.") {
		t.Fatalf("Expected paragraph after thematic break to be present in output.\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "Next item") {
		t.Fatalf("Expected following list item to be present in output.\nOutput:\n%s", output)
	}
}
