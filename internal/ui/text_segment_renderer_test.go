package ui

import (
	"regexp"
	"strings"
	"testing"
)

// stripANSI removes ANSI escape codes from a string for test comparisons
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

func TestTextSegmentRenderer_BasicBold(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	err = renderer.Write("**bold**\n\n")
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	err = renderer.Flush()
	if err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}

	output := renderer.Rendered()

	// Check that ** markers are gone (bold was rendered)
	if strings.Contains(output, "**") {
		t.Errorf("Expected ** markers to be removed, got: %q", output)
	}
	if !strings.Contains(output, "bold") {
		t.Errorf("Expected 'bold' in output, got: %q", output)
	}
}

func TestTextSegmentRenderer_Streaming(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	// Simulate streaming chunks
	chunks := []string{"# Head", "ing\n\n", "Some ", "text.\n\n"}
	for _, chunk := range chunks {
		err = renderer.Write(chunk)
		if err != nil {
			t.Fatalf("Failed to write chunk %q: %v", chunk, err)
		}
	}

	err = renderer.Flush()
	if err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}

	output := renderer.Rendered()
	plainOutput := stripANSI(output)

	// Should contain the heading content
	if !strings.Contains(plainOutput, "Heading") {
		t.Errorf("Expected 'Heading' in output, got: %q", plainOutput)
	}
	// Should contain the text
	if !strings.Contains(plainOutput, "Some text") {
		t.Errorf("Expected 'Some text' in output, got: %q", plainOutput)
	}
}

func TestTextSegmentRenderer_ShowsPartialPlainTextWithoutNewline(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	if err := renderer.Write("Hello"); err != nil {
		t.Fatalf("Failed to write partial text: %v", err)
	}

	got := stripANSI(renderer.RenderedUnflushed())
	if got != "Hello" {
		t.Fatalf("RenderedUnflushed() = %q, want %q", got, "Hello")
	}
}

func TestTextSegmentRenderer_PartialMarkdownStaysVisibleAcrossSyntaxCompletion(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	if err := renderer.Write("Hello **wor"); err != nil {
		t.Fatalf("Failed to write incomplete markdown: %v", err)
	}
	if got := stripANSI(renderer.RenderedUnflushed()); got != "Hello" {
		t.Fatalf("RenderedUnflushed() after incomplete markdown = %q, want %q", got, "Hello")
	}

	if err := renderer.Write("ld**"); err != nil {
		t.Fatalf("Failed to complete markdown: %v", err)
	}
	if got := stripANSI(renderer.RenderedUnflushed()); got != "Hello world" {
		t.Fatalf("RenderedUnflushed() after markdown completion = %q, want %q", got, "Hello world")
	}
}

func TestTextSegmentRenderer_IncompleteDecorationDoesNotLeakRawPreview(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	if err := renderer.Write("stream **abblkjslksjlkj"); err != nil {
		t.Fatalf("Failed to write incomplete markdown: %v", err)
	}
	got := stripANSI(renderer.RenderedUnflushed())
	if got != "stream" {
		t.Fatalf("RenderedUnflushed() with incomplete bold = %q, want %q", got, "stream")
	}
	if committed := stripANSI(renderer.RenderedCommitted()); committed != "" {
		t.Fatalf("RenderedCommitted() with incomplete bold = %q, want empty", committed)
	}

	if err := renderer.Write("**"); err != nil {
		t.Fatalf("Failed to close markdown: %v", err)
	}
	got = stripANSI(renderer.RenderedUnflushed())
	if got != "stream abblkjslksjlkj" {
		t.Fatalf("RenderedUnflushed() after closing bold = %q, want %q", got, "stream abblkjslksjlkj")
	}
	if strings.Contains(renderer.RenderedUnflushed(), "**") {
		t.Fatalf("expected markdown markers to be rendered away after close, got %q", renderer.RenderedUnflushed())
	}
}

func TestTextSegmentRenderer_UnclosedDecorationIsNotFlushedToScrollback(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	if err := renderer.Write("prefix **unstable"); err != nil {
		t.Fatalf("Failed to write incomplete markdown: %v", err)
	}
	renderer.MarkFlushed()
	if got := stripANSI(renderer.RenderedUnflushed()); got != "prefix" {
		t.Fatalf("RenderedUnflushed() after MarkFlushed on uncommitted preview = %q, want %q", got, "prefix")
	}

	if err := renderer.Write("**\n\n"); err != nil {
		t.Fatalf("Failed to close markdown: %v", err)
	}
	if got := stripANSI(renderer.RenderedUnflushed()); !strings.Contains(got, "prefix unstable") {
		t.Fatalf("expected completed decorated text to remain unflushed, got %q", got)
	}
}

func TestTextSegmentRenderer_ChunkedListMarkerClearsStalePreview(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	if err := renderer.Write("1"); err != nil {
		t.Fatalf("Failed to write partial marker: %v", err)
	}
	if got := stripANSI(renderer.RenderedUnflushed()); got != "1" {
		t.Fatalf("RenderedUnflushed() after partial marker = %q, want %q", got, "1")
	}
	if err := renderer.Write(". "); err != nil {
		t.Fatalf("Failed to complete marker: %v", err)
	}
	if got := stripANSI(renderer.RenderedUnflushed()); got != "" {
		t.Fatalf("RenderedUnflushed() after marker-only list = %q, want empty", got)
	}
}

func TestTextSegmentRenderer_UnderscoreIdentifierStreamsAsPlainText(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	if err := renderer.Write("use snake_case_variable and snake__case here"); err != nil {
		t.Fatalf("Failed to write identifier: %v", err)
	}
	got := stripANSI(renderer.RenderedUnflushed())
	want := "use snake_case_variable and snake__case here"
	if got != want {
		t.Fatalf("RenderedUnflushed() = %q, want %q", got, want)
	}
}

func TestTextSegmentRenderer_Width(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	if renderer.Width() != 80 {
		t.Errorf("Expected width 80, got %d", renderer.Width())
	}
}

func TestTextSegmentRenderer_ResizePreservesFlushedPos(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	// Write and flush some content
	err = renderer.Write("Hello world\n\n")
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}
	err = renderer.Flush()
	if err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}

	// Mark content as flushed
	renderer.MarkFlushed()
	flushedBefore := renderer.FlushedRenderedPos()
	if flushedBefore == 0 {
		t.Fatalf("Expected flushedRenderedPos > 0 after MarkFlushed")
	}

	// RenderedUnflushed should be empty after marking flushed
	if renderer.RenderedUnflushed() != "" {
		t.Errorf("Expected empty RenderedUnflushed after MarkFlushed, got: %q", renderer.RenderedUnflushed())
	}

	// Resize to a new width — "Hello world" is short enough that the
	// re-rendered output should be identical, so the flushed boundary
	// must be preserved via common-prefix logic.
	err = renderer.Resize(100)
	if err != nil {
		t.Fatalf("Failed to resize: %v", err)
	}

	// The flushed position must still cover the unchanged prefix.
	if renderer.FlushedRenderedPos() == 0 {
		t.Errorf("Resize must not zero flushedRenderedPos when the re-rendered output matches the flushed prefix")
	}

	// RenderedUnflushed must not re-expose the already-flushed content.
	unflushed := stripANSI(renderer.RenderedUnflushed())
	if strings.Contains(unflushed, "Hello world") {
		t.Errorf("RenderedUnflushed after Resize must not duplicate already-flushed content, got: %q", unflushed)
	}
}

func TestTextSegmentRenderer_RenderedUnflushedIsANSISafeForMidEscapeOffset(t *testing.T) {
	renderer, err := NewTextSegmentRenderer(80)
	if err != nil {
		t.Fatalf("Failed to create renderer: %v", err)
	}

	if err := renderer.Write("1. Item with **bold** text\n2. Second item\n"); err != nil {
		t.Fatalf("Failed to write markdown: %v", err)
	}
	if err := renderer.Flush(); err != nil {
		t.Fatalf("Failed to flush renderer: %v", err)
	}

	output := renderer.RenderedAll()
	escapeIndex := strings.Index(output, "\x1b[")
	if escapeIndex == -1 {
		t.Fatalf("Expected ANSI sequence in rendered output, got: %q", output)
	}

	// Intentionally place the flushed offset inside an ANSI sequence parameter list.
	renderer.flushedRenderedPos = escapeIndex + 2

	unflushed := renderer.RenderedUnflushed()
	if unflushed == "" {
		t.Fatal("Expected non-empty unflushed output")
	}

	// Mid-escape slicing tends to leak fragments like "178m" on line starts.
	badFragment := regexp.MustCompile(`(^|\n)[0-9;]+m`)
	if badFragment.MatchString(unflushed) {
		t.Fatalf("Detected ANSI fragment leakage in unflushed output: %q", unflushed)
	}
}
