package streaming

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/glamour"
)

// testRenderer creates a StreamRenderer for testing with consistent options.
func testRenderer(t *testing.T, w *bytes.Buffer) *StreamRenderer {
	t.Helper()
	sr, err := NewRenderer(w, glamour.WithStandardStyle("dark"))
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}
	return sr
}

// renderFull renders markdown in one pass.
func renderFull(t *testing.T, input string) string {
	t.Helper()
	var buf bytes.Buffer
	sr, err := NewRenderer(&buf, glamour.WithStandardStyle("dark"))
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}
	sr.Write([]byte(input))
	sr.Close()
	return buf.String()
}

// renderChunked renders markdown byte-by-byte.
func renderChunked(t *testing.T, input string) string {
	t.Helper()
	var buf bytes.Buffer
	sr, err := NewRenderer(&buf, glamour.WithStandardStyle("dark"))
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}
	for i := 0; i < len(input); i++ {
		sr.Write([]byte{input[i]})
	}
	sr.Close()
	return buf.String()
}

// renderRandomChunks renders markdown with random chunk sizes.
func renderRandomChunks(t *testing.T, input string, maxChunkSize int) string {
	t.Helper()
	var buf bytes.Buffer
	sr, err := NewRenderer(&buf, glamour.WithStandardStyle("dark"))
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}
	pos := 0
	for pos < len(input) {
		chunkSize := rand.Intn(maxChunkSize) + 1
		if pos+chunkSize > len(input) {
			chunkSize = len(input) - pos
		}
		sr.Write([]byte(input[pos : pos+chunkSize]))
		pos += chunkSize
	}
	sr.Close()
	return buf.String()
}

var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func stripANSIForStreamingTest(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}

type nonResettableWriter struct {
	builder strings.Builder
}

func (w *nonResettableWriter) Write(p []byte) (int, error) {
	return w.builder.Write(p)
}

func (w *nonResettableWriter) String() string {
	return w.builder.String()
}

// assertChunkingInvariant verifies that chunked output matches full output.
func assertChunkingInvariant(t *testing.T, name, input string) {
	t.Helper()

	full := renderFull(t, input)
	chunked := renderChunked(t, input)

	if full != chunked {
		t.Errorf("%s: chunking invariant FAILED\nInput:\n%s\n\nFull output (%d bytes):\n%q\n\nChunked output (%d bytes):\n%q",
			name, input, len(full), full, len(chunked), chunked)
	}
}

//
// ============================================================================
// CHUNKING INVARIANT TESTS
// These tests verify that output is identical regardless of how input is chunked
// ============================================================================
//

func TestChunkingInvariant_Heading(t *testing.T) {
	assertChunkingInvariant(t, "ATX Heading", "# Hello World\n")
	assertChunkingInvariant(t, "ATX Heading H2", "## Subheading\n")
	assertChunkingInvariant(t, "ATX Heading H6", "###### Deep heading\n")
}

func TestChunkingInvariant_SetextHeading(t *testing.T) {
	assertChunkingInvariant(t, "Setext H1", "Heading\n=======\n")
	assertChunkingInvariant(t, "Setext H2", "Heading\n-------\n")
}

func TestChunkingInvariant_Paragraph(t *testing.T) {
	assertChunkingInvariant(t, "Simple paragraph", "This is a paragraph.\n\n")
	assertChunkingInvariant(t, "Multi-line paragraph", "Line one.\nLine two.\nLine three.\n\n")
}

func TestChunkingInvariant_FencedCode(t *testing.T) {
	assertChunkingInvariant(t, "Fenced code backticks", "```\ncode here\n```\n")
	assertChunkingInvariant(t, "Fenced code with lang", "```go\nfmt.Println(\"hello\")\n```\n")
	assertChunkingInvariant(t, "Fenced code tildes", "~~~\ncode here\n~~~\n")
	assertChunkingInvariant(t, "Fenced code 4 backticks", "````\n```\nnested\n```\n````\n")
}

func TestChunkingInvariant_List(t *testing.T) {
	assertChunkingInvariant(t, "Unordered list dash", "- Item 1\n- Item 2\n- Item 3\n\nAfter list.\n")
	assertChunkingInvariant(t, "Unordered list asterisk", "* Item 1\n* Item 2\n\nAfter.\n")
	assertChunkingInvariant(t, "Ordered list", "1. First\n2. Second\n3. Third\n\nAfter.\n")
}

func TestChunkingInvariant_NestedList(t *testing.T) {
	input := `- Item 1
  - Nested A
  - Nested B
- Item 2

After.
`
	assertChunkingInvariant(t, "Nested list", input)
}

func TestChunkingInvariant_Blockquote(t *testing.T) {
	assertChunkingInvariant(t, "Simple blockquote", "> This is a quote\n\nAfter.\n")
	assertChunkingInvariant(t, "Multi-line blockquote", "> Line 1\n> Line 2\n\nAfter.\n")
}

func TestChunkingInvariant_ThematicBreak(t *testing.T) {
	assertChunkingInvariant(t, "HR dashes", "---\n")
	assertChunkingInvariant(t, "HR asterisks", "***\n")
	assertChunkingInvariant(t, "HR underscores", "___\n")
}

func TestChunkingInvariant_Table(t *testing.T) {
	input := `| A | B |
|---|---|
| 1 | 2 |

After.
`
	assertChunkingInvariant(t, "Simple table", input)
}

func TestChunkingInvariant_MixedContent(t *testing.T) {
	input := `# Heading

This is a paragraph with **bold** and *italic*.

- List item 1
- List item 2

` + "```go\ncode block\n```\n" + `

> A blockquote

---

Final paragraph.
`
	assertChunkingInvariant(t, "Mixed content", input)
}

func TestChunkingInvariant_ComplexDocument(t *testing.T) {
	input := `# Welcome

This document tests the streaming markdown renderer.

## Features

The renderer supports:

- **Headings** (ATX and Setext)
- *Paragraphs* with inline formatting
- Lists (ordered and unordered)
- Code blocks

### Code Example

` + "```go\npackage main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, World!\")\n}\n```\n" + `

## Tables

| Feature | Supported |
|---------|-----------|
| Headers | Yes |
| Lists | Yes |
| Code | Yes |

> Note: This is a blockquote to test that feature.

---

*The end.*
`
	assertChunkingInvariant(t, "Complex document", input)
}

func TestChunkingInvariant_RandomChunks(t *testing.T) {
	input := `# Test

Paragraph here.

- Item 1
- Item 2

` + "```\ncode\n```\n"

	full := renderFull(t, input)

	// Test with various random chunk sizes
	for trial := 0; trial < 20; trial++ {
		chunked := renderRandomChunks(t, input, 10)
		if full != chunked {
			t.Errorf("Random chunk trial %d failed:\nFull:\n%q\nChunked:\n%q", trial, full, chunked)
		}
	}
}

func TestChunkingInvariant_SingleByteChunks(t *testing.T) {
	testCases := []string{
		"# H\n",
		"Para\n\n",
		"- A\n- B\n\nX\n",
		"```\nx\n```\n",
		"> Q\n\nX\n",
		"---\n",
	}

	for i, input := range testCases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			full := renderFull(t, input)
			chunked := renderChunked(t, input)
			if full != chunked {
				t.Errorf("Byte-by-byte chunking failed for input %q\nFull: %q\nChunked: %q",
					input, full, chunked)
			}
		})
	}
}

//
// ============================================================================
// UNIT TESTS FOR INDIVIDUAL BLOCK TYPES
// ============================================================================
//

func TestNewRenderer(t *testing.T) {
	var buf bytes.Buffer
	sr, err := NewRenderer(&buf, glamour.WithStandardStyle("dark"))
	if err != nil {
		t.Fatalf("NewRenderer failed: %v", err)
	}
	if sr == nil {
		t.Fatal("NewRenderer returned nil")
	}
	if sr.tr == nil {
		t.Fatal("StreamRenderer has nil TermRenderer")
	}
	if sr.output != &buf {
		t.Fatal("StreamRenderer has wrong output writer")
	}
}

func TestHeadingImmediateEmit(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("# Hello World\n"))

	if buf.Len() == 0 {
		t.Error("heading should emit immediately")
	}

	output := buf.String()
	if !strings.Contains(output, "Hello") || !strings.Contains(output, "World") {
		t.Errorf("output should contain heading text, got: %q", output)
	}

	sr.Close()
}

func TestParagraphEmitOnBlankLine(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("This is a paragraph.\n"))
	initialLen := buf.Len()

	sr.Write([]byte("\n"))

	if buf.Len() == initialLen {
		t.Error("paragraph should emit after blank line")
	}

	sr.Close()
}

func TestFencedCodeBlock(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("```go\n"))
	if buf.Len() > 0 {
		t.Error("should not emit before closing fence")
	}

	sr.Write([]byte("fmt.Println(\"hello\")\n"))
	if buf.Len() > 0 {
		t.Error("should not emit code content before closing fence")
	}

	sr.Write([]byte("```\n"))
	if buf.Len() == 0 {
		t.Error("should emit after closing fence")
	}

	sr.Close()
}

func TestFencedCodeBlockTilde(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("~~~python\n"))
	sr.Write([]byte("print('hello')\n"))
	sr.Write([]byte("~~~\n"))

	if buf.Len() == 0 {
		t.Error("tilde fence should work")
	}

	sr.Close()
}

func TestFencedCodeBlockNestedBackticks(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("````\n"))
	sr.Write([]byte("```\n"))
	sr.Write([]byte("nested\n"))
	sr.Write([]byte("```\n"))

	if buf.Len() > 0 {
		t.Error("shorter fence inside should not close block")
	}

	sr.Write([]byte("````\n"))
	if buf.Len() == 0 {
		t.Error("matching fence should close block")
	}

	sr.Close()
}

func TestTable(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("| Name | Age |\n"))
	sr.Write([]byte("|------|-----|\n"))
	sr.Write([]byte("| Alice | 30 |\n"))
	sr.Write([]byte("\n"))

	if buf.Len() == 0 {
		t.Error("table should emit after blank line")
	}

	sr.Close()
}

func TestList(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("- Item 1\n"))
	sr.Write([]byte("- Item 2\n"))
	sr.Write([]byte("- Item 3\n"))
	sr.Write([]byte("\n"))
	sr.Write([]byte("Regular paragraph\n"))

	output := buf.String()
	if !strings.Contains(output, "Item") {
		t.Errorf("list output missing items: %q", output)
	}

	sr.Close()
}

func TestOrderedList(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("1. First\n"))
	sr.Write([]byte("2. Second\n"))
	sr.Write([]byte("3. Third\n"))
	sr.Write([]byte("\n"))
	sr.Write([]byte("Regular paragraph\n"))

	output := buf.String()
	if !strings.Contains(output, "First") ||
		!strings.Contains(output, "Second") ||
		!strings.Contains(output, "Third") {
		t.Errorf("ordered list missing items: %q", output)
	}

	sr.Close()
}

func TestOrderedList_EmitsCompletedItemsIncrementally(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	// First item should stay pending until a following list marker arrives.
	sr.Write([]byte("1. First\n"))
	if strings.Contains(stripANSIForStreamingTest(buf.String()), "First") {
		t.Fatalf("first item should not render before a following item marker, got: %q", stripANSIForStreamingTest(buf.String()))
	}

	// Second marker closes item 1, so item 1 should now appear.
	sr.Write([]byte("2. Second\n"))
	output := stripANSIForStreamingTest(buf.String())
	if !strings.Contains(output, "First") {
		t.Fatalf("expected first item to render after second marker, got: %q", output)
	}
	if strings.Contains(output, "Second") {
		t.Fatalf("second item should remain pending until another boundary, got: %q", output)
	}

	// Third marker closes item 2, so item 2 should now appear.
	sr.Write([]byte("3. Third\n"))
	output = stripANSIForStreamingTest(buf.String())
	if !strings.Contains(output, "Second") {
		t.Fatalf("expected second item to render after third marker, got: %q", output)
	}
	if strings.Contains(output, "Third") {
		t.Fatalf("third item should remain pending until list end, got: %q", output)
	}

	// Final flush should include item 3.
	sr.Close()
	output = stripANSIForStreamingTest(buf.String())
	if !strings.Contains(output, "Third") {
		t.Fatalf("expected third item after close, got: %q", output)
	}
}

func TestNestedList_EmitsNestedItemsIncrementally(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("1. Parent\n"))
	sr.Write([]byte("   - Child A\n"))
	if strings.Contains(stripANSIForStreamingTest(buf.String()), "Child A") {
		t.Fatalf("first nested item should remain pending before sibling marker, got: %q", stripANSIForStreamingTest(buf.String()))
	}

	// Sibling nested marker closes Child A and should emit it.
	sr.Write([]byte("   - Child B\n"))
	output := stripANSIForStreamingTest(buf.String())
	if strings.Contains(output, "Child A") || strings.Contains(output, "Child B") {
		t.Fatalf("nested items should remain pending until nested list closes, got: %q", output)
	}

	// Top-level sibling marker closes nested list and emits both nested items.
	sr.Write([]byte("2. Next\n"))
	output = stripANSIForStreamingTest(buf.String())
	if !strings.Contains(output, "Child A") {
		t.Fatalf("expected first nested item after nested list closes, got: %q", output)
	}
	if !strings.Contains(output, "Child B") {
		t.Fatalf("expected second nested item after nested list closes, got: %q", output)
	}
	if strings.Contains(output, "Next") {
		t.Fatalf("next top-level item should remain pending until list end, got: %q", output)
	}

	sr.Close()
	output = stripANSIForStreamingTest(buf.String())
	if !strings.Contains(output, "Next") {
		t.Fatalf("expected final top-level item after close, got: %q", output)
	}
}

func TestApplyRenderedSnapshot_NonResettableWriterErrorsOnPrefixChange(t *testing.T) {
	writer := &nonResettableWriter{}
	sr, err := NewRenderer(writer, glamour.WithStandardStyle("dark"))
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}

	sr.lastRendered = []byte("prefix-value")

	err = sr.applyRenderedSnapshot([]byte("prefix-updated"), false)
	if err == nil {
		t.Fatal("expected non-resettable writer error for changed prefix")
	}
	if !strings.Contains(err.Error(), "non-resettable writer") {
		t.Fatalf("expected non-resettable writer error, got: %v", err)
	}

	err = sr.applyRenderedSnapshot([]byte("prefix-updated"), true)
	if err == nil {
		t.Fatal("expected non-resettable writer error for changed prefix with rewrite enabled")
	}
	if !strings.Contains(err.Error(), "non-resettable writer") {
		t.Fatalf("expected non-resettable writer error, got: %v", err)
	}
}

func TestApplyRenderedSnapshot_BestEffortShorterSnapshotKeepsExistingOutput(t *testing.T) {
	var buf bytes.Buffer
	sr, err := NewRenderer(&buf, glamour.WithStandardStyle("dark"))
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}

	original := []byte("abcdef")
	sr.lastRendered = append(sr.lastRendered[:0], original...)
	sr.renderedLen = len(original)
	if _, err := buf.Write(original); err != nil {
		t.Fatalf("failed to seed output: %v", err)
	}

	err = sr.applyRenderedSnapshot([]byte("ab"), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if buf.String() != "abcdef" {
		t.Fatalf("expected append-only output unchanged, got %q", buf.String())
	}
	if string(sr.lastRendered) != "abcdef" {
		t.Fatalf("expected lastRendered unchanged, got %q", string(sr.lastRendered))
	}
	if sr.renderedLen != len("abcdef") {
		t.Fatalf("expected renderedLen unchanged, got %d", sr.renderedLen)
	}
}

func TestBlockquote(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("> This is a quote\n"))
	sr.Write([]byte("> Second line\n"))
	sr.Write([]byte("Regular paragraph\n"))

	if buf.Len() == 0 {
		t.Error("blockquote should emit when followed by non-quote line")
	}

	sr.Close()
}

func TestThematicBreak(t *testing.T) {
	tests := []string{"---\n", "***\n", "___\n", "- - -\n", "* * *\n"}

	for _, test := range tests {
		var buf bytes.Buffer
		sr := testRenderer(t, &buf)

		sr.Write([]byte(test))

		if buf.Len() == 0 {
			t.Errorf("thematic break %q should emit immediately", test)
		}

		sr.Close()
	}
}

func TestSetextHeading(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("Heading\n"))
	sr.Write([]byte("=======\n"))

	if buf.Len() == 0 {
		t.Error("setext heading should emit after underline")
	}

	sr.Close()
}

func TestFlush(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("Incomplete paragraph"))

	if buf.Len() > 0 {
		t.Error("should not emit incomplete line")
	}

	sr.Flush()

	if buf.Len() == 0 {
		t.Error("Flush should emit buffered content")
	}
}

func TestClose(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("Some content"))
	sr.Close()

	if buf.Len() == 0 {
		t.Error("Close should flush remaining content")
	}
}

func TestWriteImplementsWriter(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	var _ io.Writer = sr

	n, err := sr.Write([]byte("test\n"))
	if err != nil {
		t.Errorf("Write returned error: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned %d, want 5", n)
	}

	sr.Close()
}

func TestEmptyInput(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte(""))
	sr.Close()

	// Should not panic
}

func TestOnlyWhitespace(t *testing.T) {
	var buf bytes.Buffer
	sr := testRenderer(t, &buf)

	sr.Write([]byte("   \n\n   \n"))
	sr.Close()

	// Should not panic
}

//
// ============================================================================
// BLOCK DETECTION TESTS
// ============================================================================
//

func TestBlockDetection(t *testing.T) {
	sr := &StreamRenderer{}

	tests := []struct {
		line     string
		expected blockType
	}{
		{"# Heading", blockHeading},
		{"## Heading 2", blockHeading},
		{"###### Heading 6", blockHeading},
		{"```", blockFencedCode},
		{"```go", blockFencedCode},
		{"~~~", blockFencedCode},
		{"---", blockThematicBreak},
		{"***", blockThematicBreak},
		{"___", blockThematicBreak},
		{"- - -", blockThematicBreak},
		{"> quote", blockBlockquote},
		{"- list item", blockList},
		{"* list item", blockList},
		{"+ list item", blockList},
		{"1. ordered", blockList},
		{"10. ordered", blockList},
		{"| table |", blockTable},
		{"regular text", blockParagraph},
		{"#hashtag", blockParagraph}, // Not a heading (no space after #)
	}

	for _, tt := range tests {
		got := sr.detectBlock(tt.line)
		if got != tt.expected {
			t.Errorf("detectBlock(%q) = %v, want %v", tt.line, got, tt.expected)
		}
	}
}

func TestIsListMarker(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"- item", true},
		{"* item", true},
		{"+ item", true},
		{"1. item", true},
		{"10. item", true},
		{"1) item", true},
		{"-item", false},
		{"1.item", false},
		{"text", false},
		{"", false},
	}

	for _, tt := range tests {
		got := isListMarker(tt.input)
		if got != tt.expected {
			t.Errorf("isListMarker(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

func TestIsOrderedListMarkerPrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"1.", true},
		{"1)", true},
		{"1. ", false},
		{"1) ", false},
		{"1", false},
		{"", false},
		{"123456789.", true},
		{"123456789)", true},
		{"1234567890.", false}, // >9 digits
		{"a.", false},
	}

	for _, tt := range tests {
		got := isOrderedListMarkerPrefix(tt.input)
		if got != tt.expected {
			t.Errorf("isOrderedListMarkerPrefix(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

func TestIsThematicBreak(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"---", true},
		{"***", true},
		{"___", true},
		{"- - -", true},
		{"* * *", true},
		{"----", true},
		{"--", false},
		{"-", false},
		{"- -", false},
		{"abc", false},
	}

	for _, tt := range tests {
		got := isThematicBreak(tt.input)
		if got != tt.expected {
			t.Errorf("isThematicBreak(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

func TestParseFence(t *testing.T) {
	tests := []struct {
		input      string
		wantChar   rune
		wantLen    int
		wantIndent int
	}{
		{"```", '`', 3, 0},
		{"````", '`', 4, 0},
		{"~~~", '~', 3, 0},
		{"  ```", '`', 3, 2},
		{"```go", '`', 3, 0},
	}

	for _, tt := range tests {
		char, length, indent := parseFence(tt.input)
		if char != tt.wantChar || length != tt.wantLen || indent != tt.wantIndent {
			t.Errorf("parseFence(%q) = (%c, %d, %d), want (%c, %d, %d)",
				tt.input, char, length, indent, tt.wantChar, tt.wantLen, tt.wantIndent)
		}
	}
}

func TestIsClosingFence(t *testing.T) {
	tests := []struct {
		line       string
		openChar   rune
		openLen    int
		openIndent int
		expected   bool
	}{
		{"```", '`', 3, 0, true},
		{"````", '`', 3, 0, true},
		{"``", '`', 3, 0, false},
		{"~~~", '~', 3, 0, true},
		{"```", '~', 3, 0, false},
		{"~~~", '`', 3, 0, false},
		{"  ```", '`', 3, 0, true},
		{"```x", '`', 3, 0, false},
	}

	for _, tt := range tests {
		got := isClosingFence(tt.line, tt.openChar, tt.openLen, tt.openIndent)
		if got != tt.expected {
			t.Errorf("isClosingFence(%q, %c, %d, %d) = %v, want %v",
				tt.line, tt.openChar, tt.openLen, tt.openIndent, got, tt.expected)
		}
	}
}

//
// ============================================================================
// BENCHMARKS
// ============================================================================
//

func BenchmarkStreamingFull(b *testing.B) {
	input := `# Heading

Paragraph text here.

- Item 1
- Item 2

` + "```go\ncode\n```\n"

	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		sr, _ := NewRenderer(&buf, glamour.WithStandardStyle("dark"))
		sr.Write([]byte(input))
		sr.Close()
	}
}

func BenchmarkStreamingChunked(b *testing.B) {
	input := `# Heading

Paragraph text here.

- Item 1
- Item 2

` + "```go\ncode\n```\n"

	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		sr, _ := NewRenderer(&buf, glamour.WithStandardStyle("dark"))
		for j := 0; j < len(input); j++ {
			sr.Write([]byte{input[j]})
		}
		sr.Close()
	}
}
