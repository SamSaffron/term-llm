package cmd

import (
	"strings"
	"testing"

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

	updated, _ := model.Update(askContentMsg("**bold**"))
	model = updated.(askStreamModel)

	updated, _ = model.Update(askDoneMsg{})
	model = updated.(askStreamModel)

	view := model.View()
	if strings.Contains(view, "**") {
		t.Fatalf("expected markdown to be rendered on completion, got raw view: %q", view)
	}
	if !strings.Contains(view, "bold") {
		t.Fatalf("expected rendered view to contain content, got: %q", view)
	}
}

func TestAskDoneDoesNotFlushSegments(t *testing.T) {
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
	if model.tracker.Segments[0].Flushed {
		t.Fatalf("expected segments to remain unflushed on done to avoid duplicate scrollback output")
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
