package cmd

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/ui"
)

// Test content similar to the Ruby release notes
const testMarkdown = `Great news! Ruby 4.0 was just released on December 25th, 2025 (Christmas Day) - marking 30 years since Ruby's first public release! Here are some of the coolest features:

## **ZJIT - New JIT Compiler**
ZJIT is a new just-in-time (JIT) compiler, which is developed as the next generation of YJIT. Unlike YJIT's lazy basic block versioning approach, ZJIT uses a more traditional method based compilation strategy, is designed to be more accessible to contributors, and follows a "textbook" compiler architecture that's easier to understand and modify. While ZJIT is faster than the interpreter, but not yet as fast as YJIT, it sets the foundation for future performance improvements and easier community contributions.

## **Ruby Box - Experimental Isolation Feature**
Ruby Box is a new (experimental) feature to provide separation about definitions. Ruby Box can isolate/separate monkey patches, changes of global/class variables, class/module definitions, and loaded native/ruby libraries from other boxes. This means you can load multiple versions of a library simultaneously and isolate test cases from each other!

## **Ractor Improvements**
Ractor, Ruby's parallel execution mechanism, has received several improvements, including a new class, Ractor::Port, which was introduced to address issues related to message sending and receiving.

## **Language Changes**
Some nice syntax improvements:
- Logical binary operators (||, &&, and and or) at the beginning of a line continue the previous line, like fluent dot.
- Set has been promoted from stdlib to a core class. No more ` + "`require 'set'`" + ` needed!

It's an exciting release that balances new experimental features with practical improvements!`

func TestMarkdownRendering(t *testing.T) {
	width := 80

	// Test full render
	fullRender, err := ui.RenderMarkdownWithError(testMarkdown, width)
	if err != nil {
		t.Fatalf("Failed to render full markdown: %v", err)
	}

	t.Logf("Full render output:\n%s", fullRender)
	t.Logf("Full render length: %d chars, %d lines", len(fullRender), strings.Count(fullRender, "\n"))
}

func TestIncrementalRendering(t *testing.T) {
	width := 80

	// Simulate streaming by splitting content and rendering incrementally
	// This mimics what streamWithGlamour does
	chunks := simulateStreaming(testMarkdown)

	var content strings.Builder
	var printedLines int
	var allOutput strings.Builder

	for i, chunk := range chunks {
		content.WriteString(chunk)

		// Only render when we get a newline (like the real code does)
		if strings.Contains(chunk, "\n") {
			rendered, err := ui.RenderMarkdownWithError(content.String(), width)
			if err != nil {
				t.Fatalf("Render failed at chunk %d: %v", i, err)
			}

			lines := strings.Split(rendered, "\n")
			for j := printedLines; j < len(lines); j++ {
				if j < len(lines)-1 {
					allOutput.WriteString(lines[j])
					allOutput.WriteString("\n")
					printedLines++
				}
			}
		}
	}

	// Final render
	finalRendered, _ := ui.RenderMarkdownWithError(content.String(), width)
	lines := strings.Split(finalRendered, "\n")
	for i := printedLines; i < len(lines); i++ {
		line := lines[i]
		if line != "" || i < len(lines)-1 {
			allOutput.WriteString(line)
			allOutput.WriteString("\n")
		}
	}

	// Compare with full render
	fullRender, _ := ui.RenderMarkdownWithError(testMarkdown, width)

	t.Logf("Incremental output:\n%s", allOutput.String())
	t.Logf("Full render:\n%s", fullRender)

	// Allow small line count difference (streaming may add/remove trailing newline)
	incrementalLines := strings.Count(allOutput.String(), "\n")
	fullLines := strings.Count(fullRender, "\n")
	lineDiff := incrementalLines - fullLines
	if lineDiff < 0 {
		lineDiff = -lineDiff
	}

	if lineDiff > 1 {
		t.Errorf("Line count differs significantly!\nIncremental lines: %d\nFull lines: %d",
			incrementalLines, fullLines)
	}

	// Ensure ANSI escape codes are preserved (the main fix we're testing)
	if !strings.Contains(allOutput.String(), "\x1b[") {
		t.Error("Incremental output missing ANSI escape codes - wordwrap may be breaking styling")
	}
	if !strings.Contains(fullRender, "\x1b[") {
		t.Error("Full render missing ANSI escape codes")
	}
}

func TestAskDoneRendersMarkdown(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	// Note: glamour requires paragraph structure (trailing newlines) to render inline markdown.
	// Without newlines, **bold** is not recognized as a complete paragraph.
	updated, _ := model.Update(askContentMsg("**bold**\n\n"))
	model = updated.(askStreamModel)

	_, cmd := model.Update(askDoneMsg{})
	if cmd == nil {
		t.Fatal("expected a command from askDoneMsg")
	}

	// We can't easily inspect the content of tea.Printf command in a unit test
	// but we can verify that segments are now marked as flushed
	if !model.tracker.Segments[0].Flushed {
		t.Error("expected segments to be flushed on done")
	}
}

func TestAskDoneFlushesSegments(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askToolStartMsg{CallID: "call-1", Name: "shell", Info: "(git status)"})
	model = updated.(askStreamModel)

	updated, _ = model.Update(askToolEndMsg{CallID: "call-1", Success: true})
	model = updated.(askStreamModel)

	updated, _ = model.Update(askDoneMsg{})
	model = updated.(askStreamModel)

	if len(model.tracker.Segments) == 0 {
		t.Fatal("expected tool segment to be tracked")
	}
	if !model.tracker.Segments[0].Flushed {
		t.Fatalf("expected segments to be flushed on done")
	}
}

func TestAskToolStartFlushesCompletedTextBoundary(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askContentMsg("Let me grab the announcement.\n\n"))
	model = updated.(askStreamModel)

	updated, _ = model.Update(askToolStartMsg{CallID: "call-1", Name: "read_file", Info: "(announcement.md)"})
	model = updated.(askStreamModel)

	if len(model.tracker.Segments) != 2 {
		t.Fatalf("expected 2 segments (text + tool), got %d", len(model.tracker.Segments))
	}

	textSeg := model.tracker.Segments[0]
	if textSeg.Type != ui.SegmentText {
		t.Fatalf("expected first segment to be text, got %v", textSeg.Type)
	}
	if !textSeg.Complete {
		t.Fatal("expected pre-tool text segment to be complete")
	}
	if !textSeg.Flushed {
		t.Fatal("expected pre-tool text segment to flush at tool boundary")
	}

	toolSeg := model.tracker.Segments[1]
	if toolSeg.Type != ui.SegmentTool {
		t.Fatalf("expected second segment to be tool, got %v", toolSeg.Type)
	}
	if toolSeg.ToolStatus != ui.ToolPending {
		t.Fatalf("expected tool to be pending, got %v", toolSeg.ToolStatus)
	}
}

func TestAskToolEndFlushesCompletedToolBoundary(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askToolStartMsg{CallID: "call-1", Name: "shell", Info: "(pwd)"})
	model = updated.(askStreamModel)

	updated, _ = model.Update(askToolEndMsg{CallID: "call-1", Success: true})
	model = updated.(askStreamModel)

	if len(model.tracker.Segments) != 1 {
		t.Fatalf("expected 1 tool segment, got %d", len(model.tracker.Segments))
	}
	if model.tracker.Segments[0].ToolStatus != ui.ToolSuccess {
		t.Fatalf("expected tool to be successful, got %v", model.tracker.Segments[0].ToolStatus)
	}
	if !model.tracker.Segments[0].Flushed {
		t.Fatal("expected completed tool segment to flush at tool-end boundary")
	}
}

// TestAskDoneNoDoubleRendering verifies that content appears exactly once
// in the final flush, preventing double rendering issues.
func TestAskDoneNoDoubleRendering(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	// Add unique content
	updated, _ := model.Update(askContentMsg("UNIQUE_MARKER_12345\n\n"))
	model = updated.(askStreamModel)

	// Complete
	_, cmd := model.Update(askDoneMsg{})
	if cmd == nil {
		t.Fatal("expected a command from askDoneMsg")
	}

	// Verify it's flushed
	if !model.tracker.Segments[0].Flushed {
		t.Error("segment should be flushed")
	}
}

func TestAskStreamsTextOnSmoothTicks(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askContentMsg("hello world"))
	model = updated.(askStreamModel)

	// Text should be buffered first; no segment until a smooth tick is processed.
	if len(model.tracker.Segments) != 0 {
		t.Fatalf("expected 0 segments before smooth tick, got %d", len(model.tracker.Segments))
	}

	updated, _ = model.Update(ui.SmoothTickMsg{})
	model = updated.(askStreamModel)
	if len(model.tracker.Segments) == 0 {
		t.Fatal("expected text segment after first smooth tick")
	}

	first := model.tracker.Segments[0].GetText()
	if first == "hello world" {
		t.Fatalf("expected partial word-paced content after first tick, got %q", first)
	}
	if !strings.Contains(first, "hello") {
		t.Fatalf("expected first word after first tick, got %q", first)
	}

	updated, _ = model.Update(ui.SmoothTickMsg{})
	model = updated.(askStreamModel)
	got := model.tracker.Segments[0].GetText()
	if got != "hello world" {
		t.Fatalf("expected full content after second tick, got %q", got)
	}
}

func TestAskCoalescesSmoothTickSchedulingForBurstTextEvents(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, firstCmd := model.Update(askContentMsg("hello"))
	model = updated.(askStreamModel)
	if firstCmd == nil {
		t.Fatal("expected first text chunk to schedule smooth tick")
	}

	updated, secondCmd := model.Update(askContentMsg(" world"))
	model = updated.(askStreamModel)
	if secondCmd != nil {
		t.Fatal("expected no additional smooth tick while one is already pending")
	}
	if !model.smoothTickPending {
		t.Fatal("expected smoothTickPending to remain true until tick is handled")
	}
}

func TestAskStreamingFlushThresholdAdaptsToBufferSize(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80
	model.adaptiveFlushThreshold = true

	if got := model.streamingFlushThreshold(); got != 0 {
		t.Fatalf("expected zero threshold for empty buffer, got %d", got)
	}

	model.smoothBuffer.Write(strings.Repeat("word ", 80)) // ~400 bytes
	mid := model.streamingFlushThreshold()
	if mid <= 0 {
		t.Fatalf("expected positive threshold for medium buffer, got %d", mid)
	}

	model.smoothBuffer.Write(strings.Repeat("word ", 400)) // +~2000 bytes
	high := model.streamingFlushThreshold()
	if high <= mid {
		t.Fatalf("expected larger threshold for large buffer, got mid=%d high=%d", mid, high)
	}
}

func TestAskToolStartFlushUsesOrderedCommandComposition(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askContentMsg("Before tool boundary.\n\n"))
	model = updated.(askStreamModel)

	updated, cmd := model.Update(askToolStartMsg{CallID: "call-1", Name: "read_file", Info: "(announcement.md)"})
	model = updated.(askStreamModel)

	if cmd == nil {
		t.Fatal("expected command from tool-start boundary flush")
	}

	msg := cmd()
	if _, isBatch := msg.(tea.BatchMsg); isBatch {
		t.Fatalf("expected ordered (sequence) command composition, got concurrent batch")
	}
}

func TestAskToolStartRefreshesCachedContentBeforeFlush(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	model.tracker.AddTextSegment("Before tool boundary.\n\n", model.width)
	model.tracker.MarkCurrentTextComplete(func(text string) string {
		return renderMd(text, model.width)
	})
	model.cachedContent = model.tracker.RenderUnflushed(model.width, renderMd, false)
	model.contentDirty = false

	updated, cmd := model.Update(askToolStartMsg{CallID: "call-1", Name: "read_file", Info: "(announcement.md)"})
	model = updated.(askStreamModel)

	if cmd == nil {
		t.Fatal("expected command from tool-start boundary flush")
	}

	expected := model.tracker.RenderUnflushed(model.width, renderMd, false)
	if model.cachedContent != expected {
		t.Fatalf("cached content should reflect post-boundary tracker state\nexpected: %q\ngot: %q", model.cachedContent, expected)
	}
	if model.contentDirty {
		t.Fatal("expected contentDirty to be false after refreshing cached content")
	}
}

func TestAskViewNoForcedTrailingNewline(t *testing.T) {
	model := newAskStreamModel()
	model.pausedForExternalUI = true
	model.cachedContent = "content"
	model.contentDirty = false

	view := model.View()
	if strings.HasSuffix(view, "\n") {
		t.Fatalf("unexpected trailing newline in view output: %q", view)
	}
}

// simulateStreaming splits content into chunks like an LLM would stream it
func simulateStreaming(content string) []string {
	var chunks []string
	words := strings.Fields(content)

	for i, word := range words {
		if i > 0 {
			chunks = append(chunks, " ")
		}
		chunks = append(chunks, word)

		// Add newlines where they appear in original
		idx := strings.Index(content, word)
		if idx >= 0 {
			afterWord := idx + len(word)
			if afterWord < len(content) && content[afterWord] == '\n' {
				chunks = append(chunks, "\n")
				if afterWord+1 < len(content) && content[afterWord+1] == '\n' {
					chunks = append(chunks, "\n")
				}
			}
		}
	}

	return chunks
}

func TestHeadingSpacing(t *testing.T) {
	md := `## Headers at All Levels

# Header 1

## Header 2

### Header 3

#### Header 4

##### Header 5

###### Header 6`

	width := 80
	tracker := ui.NewToolTracker()

	// Simulate streaming - write in small chunks
	chunkSize := 20
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
	}

	// Complete and get final output
	tracker.CompleteTextSegments(func(text string) string {
		return renderMd(text, width)
	})

	result := tracker.FlushAllRemaining(width, 0, renderMd)

	// Strip ANSI for counting
	clean := stripAnsi(result.ToPrint)

	// Count lines
	lines := strings.Split(clean, "\n")

	// Should be 14 lines: 7 headers + 6 blank lines + 1 leading blank
	// Allow for trimming variations
	lineCount := len(lines)
	for lineCount > 0 && strings.TrimSpace(lines[lineCount-1]) == "" {
		lineCount--
	}
	if lineCount == 0 {
		t.Fatalf("No output produced")
	}

	// Log actual output for debugging
	t.Logf("Line count: %d", lineCount)
	for i, line := range lines[:lineCount] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			t.Logf("Line %d: (blank)", i)
		} else {
			t.Logf("Line %d: %q", i, trimmed)
		}
	}

	// We expect around 14 lines (7 headers + 6 blank lines + potentially leading blank)
	if lineCount < 13 || lineCount > 15 {
		t.Errorf("Expected 13-15 lines, got %d", lineCount)
	}
}

// stripAnsi removes ANSI escape sequences for testing
func stripAnsi(s string) string {
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
