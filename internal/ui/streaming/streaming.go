// Package streaming provides a streaming markdown renderer that wraps glamour's
// TermRenderer. It buffers markdown input and renders complete blocks as they
// become available, making it suitable for rendering markdown from streaming
// sources like LLM outputs.
package streaming

import (
	"bytes"
	"io"
	"regexp"
	"strings"

	"github.com/charmbracelet/glamour"
)

// multiNewlineRe matches 3 or more consecutive newlines.
var multiNewlineRe = regexp.MustCompile(`\n{3,}`)

// normalizeNewlines reduces 3+ consecutive newlines to 2 (one blank line max).
func normalizeNewlines(s []byte) []byte {
	return multiNewlineRe.ReplaceAll(s, []byte("\n\n"))
}

// blockType represents the type of markdown block being processed.
type blockType int

const (
	blockUnknown blockType = iota
	blockParagraph
	blockFencedCode
	blockTable
	blockList
	blockBlockquote
	blockHeading
	blockThematicBreak
)

// state represents the current state of the streaming renderer.
type state int

const (
	stateReady        state = iota // Ready for new block
	stateInParagraph               // Accumulating paragraph
	stateInFencedCode              // Inside ``` ... ```
	stateInTable                   // Inside table rows
	stateInList                    // Inside list
	stateInBlockquote              // Inside > block
)

// StreamRenderer wraps glamour's TermRenderer and provides streaming capabilities.
// It buffers markdown input and renders complete blocks immediately to the output writer.
type StreamRenderer struct {
	tr     *glamour.TermRenderer
	output io.Writer

	// Line buffering - accumulates bytes until we have complete lines
	lineBuf bytes.Buffer

	// All markdown received so far (for re-rendering)
	allMarkdown bytes.Buffer

	// How many bytes of rendered output we've already written
	renderedLen int

	// Current state
	state state

	// Fenced code block state
	fenceChar   rune // '`' or '~'
	fenceLen    int  // number of fence characters
	fenceIndent int  // leading spaces before fence

	// List state
	listIndent int // base indent level of current list

	// Track pending block content (lines that form the current incomplete block)
	pendingLines []string

	// Partial rendering configuration
	partialEnabled bool                // Whether partial block rendering is enabled
	termWidth      int                 // Terminal width for line counting
	termCtrl       *terminalController // Terminal control for cursor movement

	// Track partial block state for re-rendering
	partialState partialState

	// Glamour options for re-creating renderer on resize
	glamourOpts []glamour.TermRendererOption
}

// NewRenderer creates a new streaming markdown renderer.
// Options are passed directly to glamour.NewTermRenderer.
func NewRenderer(w io.Writer, opts ...glamour.TermRendererOption) (*StreamRenderer, error) {
	tr, err := glamour.NewTermRenderer(opts...)
	if err != nil {
		return nil, err
	}

	return &StreamRenderer{
		tr:          tr,
		output:      w,
		state:       stateReady,
		glamourOpts: opts,
	}, nil
}

// NewRendererWithOptions creates a new streaming markdown renderer with
// additional streaming-specific options like partial rendering.
func NewRendererWithOptions(
	w io.Writer,
	streamOpts []StreamRendererOption,
	glamourOpts ...glamour.TermRendererOption,
) (*StreamRenderer, error) {
	tr, err := glamour.NewTermRenderer(glamourOpts...)
	if err != nil {
		return nil, err
	}

	sr := &StreamRenderer{
		tr:          tr,
		output:      w,
		state:       stateReady,
		glamourOpts: glamourOpts,
	}

	// Apply streaming options
	for _, opt := range streamOpts {
		opt(sr)
	}

	// Initialize terminal controller only if partial rendering is enabled
	// AND terminal width was explicitly set (via WithTerminalWidth).
	// Without terminal width, partial rendering uses flowing mode (append-only).
	if sr.partialEnabled && sr.termWidth > 0 {
		sr.termCtrl = newTerminalController(w, sr.termWidth)
	}

	return sr, nil
}

// normalizedMarkdown returns the buffered markdown with tabs normalized to 2 spaces.
// This prevents glamour from expanding tabs to 8 spaces, which causes code blocks
// to overflow terminal width.
func (sr *StreamRenderer) normalizedMarkdown() []byte {
	content := sr.allMarkdown.Bytes()
	return bytes.ReplaceAll(content, []byte("\t"), []byte("  "))
}

// Write accepts markdown chunks and renders complete blocks immediately.
// It implements io.Writer.
func (sr *StreamRenderer) Write(p []byte) (n int, err error) {
	// Add incoming bytes to line buffer
	sr.lineBuf.Write(p)

	// Process complete lines
	for {
		line, err := sr.lineBuf.ReadString('\n')
		if err != nil {
			// No complete line yet, put back what we read
			sr.lineBuf.WriteString(line)
			break
		}
		// Process the complete line (including newline)
		if err := sr.processLine(line); err != nil {
			return len(p), err
		}
	}

	// Trigger partial render if enabled and we have pending content
	if sr.partialEnabled && (len(sr.pendingLines) > 0 || sr.lineBuf.Len() > 0) {
		if err := sr.renderPartialBlock(); err != nil {
			return len(p), err
		}
	}

	return len(p), nil
}

// CommittedMarkdownLen returns the number of raw markdown bytes that have been
// committed as complete blocks. This excludes any pending/incomplete block content.
func (sr *StreamRenderer) CommittedMarkdownLen() int {
	return sr.allMarkdown.Len()
}

// processLine handles a single complete line of input.
func (sr *StreamRenderer) processLine(line string) error {
	// Remove the trailing newline for analysis, but keep track of it
	content := strings.TrimSuffix(line, "\n")
	content = strings.TrimSuffix(content, "\r")

	switch sr.state {
	case stateReady:
		return sr.handleReady(content, line)
	case stateInParagraph:
		return sr.handleParagraph(content, line)
	case stateInFencedCode:
		return sr.handleFencedCode(content, line)
	case stateInTable:
		return sr.handleTable(content, line)
	case stateInList:
		return sr.handleList(content, line)
	case stateInBlockquote:
		return sr.handleBlockquote(content, line)
	}

	return nil
}

// handleReady processes a line when we're ready for a new block.
func (sr *StreamRenderer) handleReady(content, rawLine string) error {
	// Skip blank lines at the start - add to markdown but don't change state
	if isBlankLine(content) {
		sr.allMarkdown.WriteString(rawLine)
		return nil
	}

	blockType := sr.detectBlock(content)

	switch blockType {
	case blockFencedCode:
		sr.state = stateInFencedCode
		sr.fenceChar, sr.fenceLen, sr.fenceIndent = parseFence(content)
		sr.pendingLines = append(sr.pendingLines, rawLine)

	case blockHeading:
		// Headings are complete immediately (single line)
		sr.allMarkdown.WriteString(rawLine)
		return sr.emitRendered()

	case blockThematicBreak:
		// Thematic breaks are complete immediately
		sr.allMarkdown.WriteString(rawLine)
		return sr.emitRendered()

	case blockTable:
		sr.state = stateInTable
		sr.pendingLines = append(sr.pendingLines, rawLine)

	case blockList:
		sr.state = stateInList
		sr.listIndent = countLeadingSpaces(content)
		sr.pendingLines = append(sr.pendingLines, rawLine)

	case blockBlockquote:
		sr.state = stateInBlockquote
		sr.pendingLines = append(sr.pendingLines, rawLine)

	case blockParagraph:
		sr.state = stateInParagraph
		sr.pendingLines = append(sr.pendingLines, rawLine)

	default:
		sr.state = stateInParagraph
		sr.pendingLines = append(sr.pendingLines, rawLine)
	}

	return nil
}

// handleParagraph processes a line while accumulating a paragraph.
func (sr *StreamRenderer) handleParagraph(content, rawLine string) error {
	// Blank line ends paragraph
	if isBlankLine(content) {
		// Commit pending lines to markdown
		for _, l := range sr.pendingLines {
			sr.allMarkdown.WriteString(l)
		}
		sr.pendingLines = nil
		sr.allMarkdown.WriteString(rawLine)
		sr.state = stateReady
		return sr.emitRendered()
	}

	// IMPORTANT: Check for setext heading underline FIRST (=== or ---)
	// This must be checked before thematic break because --- is ambiguous
	if isSetextUnderline(content) && len(sr.pendingLines) > 0 {
		// This converts the paragraph to a heading
		for _, l := range sr.pendingLines {
			sr.allMarkdown.WriteString(l)
		}
		sr.pendingLines = nil
		sr.allMarkdown.WriteString(rawLine)
		sr.state = stateReady
		return sr.emitRendered()
	}

	// Check if this line starts a new block type
	blockType := sr.detectBlock(content)

	switch blockType {
	case blockFencedCode, blockHeading, blockThematicBreak, blockTable, blockList, blockBlockquote:
		// Commit current paragraph first
		for _, l := range sr.pendingLines {
			sr.allMarkdown.WriteString(l)
		}
		sr.pendingLines = nil
		sr.state = stateReady
		if err := sr.emitRendered(); err != nil {
			return err
		}
		// Then process this line as a new block
		return sr.handleReady(content, rawLine)
	}

	// Continue accumulating paragraph
	sr.pendingLines = append(sr.pendingLines, rawLine)
	return nil
}

// handleFencedCode processes a line while inside a fenced code block.
func (sr *StreamRenderer) handleFencedCode(content, rawLine string) error {
	sr.pendingLines = append(sr.pendingLines, rawLine)

	// Check for closing fence
	if isClosingFence(content, sr.fenceChar, sr.fenceLen, sr.fenceIndent) {
		// Commit all pending lines
		for _, l := range sr.pendingLines {
			sr.allMarkdown.WriteString(l)
		}
		sr.pendingLines = nil
		sr.state = stateReady
		sr.fenceChar = 0
		sr.fenceLen = 0
		sr.fenceIndent = 0
		return sr.emitRendered()
	}

	return nil
}

// handleTable processes a line while inside a table.
func (sr *StreamRenderer) handleTable(content, rawLine string) error {
	// Tables continue as long as lines contain |
	if isTableLine(content) {
		sr.pendingLines = append(sr.pendingLines, rawLine)
		return nil
	}

	// Non-table line ends the table
	for _, l := range sr.pendingLines {
		sr.allMarkdown.WriteString(l)
	}
	sr.pendingLines = nil
	sr.state = stateReady
	if err := sr.emitRendered(); err != nil {
		return err
	}

	// Process this line as a new block
	return sr.handleReady(content, rawLine)
}

// handleList processes a line while inside a list.
func (sr *StreamRenderer) handleList(content, rawLine string) error {
	// Blank line might end list or be between items
	if isBlankLine(content) {
		sr.pendingLines = append(sr.pendingLines, rawLine)
		return nil
	}

	indent := countLeadingSpaces(content)
	trimmed := strings.TrimLeft(content, " \t")

	// Check if this is a list marker - always continues the list
	if isListMarker(trimmed) {
		sr.pendingLines = append(sr.pendingLines, rawLine)
		return nil
	}

	// Check if a new block type is starting (not a paragraph)
	blockType := sr.detectBlock(content)
	if blockType != blockParagraph && blockType != blockUnknown {
		// New block type, emit list
		for _, l := range sr.pendingLines {
			sr.allMarkdown.WriteString(l)
		}
		sr.pendingLines = nil
		sr.state = stateReady
		if err := sr.emitRendered(); err != nil {
			return err
		}
		return sr.handleReady(content, rawLine)
	}

	// For non-list-marker content to continue the list, it must be indented
	// more than the base list indent (continuation of list item text)
	if indent > sr.listIndent {
		sr.pendingLines = append(sr.pendingLines, rawLine)
		return nil
	}

	// Non-indented, non-list content ends the list
	for _, l := range sr.pendingLines {
		sr.allMarkdown.WriteString(l)
	}
	sr.pendingLines = nil
	sr.state = stateReady
	if err := sr.emitRendered(); err != nil {
		return err
	}
	return sr.handleReady(content, rawLine)
}

// handleBlockquote processes a line while inside a blockquote.
func (sr *StreamRenderer) handleBlockquote(content, rawLine string) error {
	trimmed := strings.TrimLeft(content, " \t")

	// Blank lines within blockquotes are allowed
	if isBlankLine(content) {
		sr.pendingLines = append(sr.pendingLines, rawLine)
		return nil
	}

	// Lines starting with > continue the blockquote
	if len(trimmed) > 0 && trimmed[0] == '>' {
		sr.pendingLines = append(sr.pendingLines, rawLine)
		return nil
	}

	// Non-blockquote line ends the blockquote
	for _, l := range sr.pendingLines {
		sr.allMarkdown.WriteString(l)
	}
	sr.pendingLines = nil
	sr.state = stateReady
	if err := sr.emitRendered(); err != nil {
		return err
	}
	return sr.handleReady(content, rawLine)
}

// emitRendered renders the full document and outputs only the new portion.
// This maintains exact parity with glamour's direct rendering while avoiding
// redundant output of already-written content.
func (sr *StreamRenderer) emitRendered() error {
	// Clear partial render before emitting complete block
	if sr.partialEnabled {
		if err := sr.clearPartialState(); err != nil {
			return err
		}
	}

	if sr.allMarkdown.Len() == 0 {
		return nil
	}

	// Render the full document to maintain consistent styling
	rendered, err := sr.tr.RenderBytes(sr.normalizedMarkdown())
	if err != nil {
		return err
	}

	// Normalize consecutive newlines to fix inconsistent header spacing
	rendered = normalizeNewlines(rendered)

	// Find the stable length - exclude trailing newlines that may change
	// as more content is added (document margin vs inter-block spacing)
	stableLen := len(rendered)
	for stableLen > 0 && rendered[stableLen-1] == '\n' {
		stableLen--
	}

	// Output only the new portion since last render
	if stableLen > sr.renderedLen {
		if _, err := sr.output.Write(rendered[sr.renderedLen:stableLen]); err != nil {
			return err
		}
		sr.renderedLen = stableLen
	}

	return nil
}

// Flush renders any buffered content, treating incomplete blocks as complete.
func (sr *StreamRenderer) Flush() error {
	// Clear partial render state first
	if sr.partialEnabled {
		if err := sr.clearPartialState(); err != nil {
			return err
		}
	}

	// First, process any remaining partial line
	if sr.lineBuf.Len() > 0 {
		remaining := sr.lineBuf.String()
		sr.lineBuf.Reset()
		sr.pendingLines = append(sr.pendingLines, remaining)
		if !strings.HasSuffix(remaining, "\n") {
			sr.pendingLines[len(sr.pendingLines)-1] += "\n"
		}
	}

	// Commit any pending lines
	for _, l := range sr.pendingLines {
		sr.allMarkdown.WriteString(l)
	}
	sr.pendingLines = nil
	sr.state = stateReady

	if sr.allMarkdown.Len() == 0 {
		return nil
	}

	// Render the full document to maintain consistent styling
	rendered, err := sr.tr.RenderBytes(sr.normalizedMarkdown())
	if err != nil {
		return err
	}

	// Normalize consecutive newlines to fix inconsistent header spacing
	rendered = normalizeNewlines(rendered)

	// Output the remaining portion (including trailing newlines for final flush)
	if len(rendered) > sr.renderedLen {
		if _, err := sr.output.Write(rendered[sr.renderedLen:]); err != nil {
			return err
		}
	}

	return nil
}

// Close flushes any remaining content and cleans up.
func (sr *StreamRenderer) Close() error {
	return sr.Flush()
}

// Resize handles terminal resize events by re-creating the glamour renderer
// with the new width and re-rendering all accumulated content.
// The caller should clear the screen before calling this method.
func (sr *StreamRenderer) Resize(newWidth int) error {
	if newWidth <= 0 {
		return nil
	}

	// Update width
	sr.termWidth = newWidth

	// Update terminal controller
	if sr.termCtrl != nil {
		sr.termCtrl.width = newWidth
	}

	// Re-create glamour renderer with new word wrap width
	newOpts := make([]glamour.TermRendererOption, 0, len(sr.glamourOpts)+1)
	for _, opt := range sr.glamourOpts {
		newOpts = append(newOpts, opt)
	}
	newOpts = append(newOpts, glamour.WithWordWrap(newWidth-1)) // slight margin

	tr, err := glamour.NewTermRenderer(newOpts...)
	if err != nil {
		return err
	}
	sr.tr = tr

	// Clear partial state
	sr.partialState = partialState{}

	// Reset render tracking - we'll re-render everything
	sr.renderedLen = 0

	if sr.allMarkdown.Len() > 0 {
		rendered, err := sr.tr.RenderBytes(sr.normalizedMarkdown())
		if err != nil {
			return err
		}

		// Normalize consecutive newlines to fix inconsistent header spacing
		rendered = normalizeNewlines(rendered)

		// Find stable length (exclude trailing newlines)
		stableLen := len(rendered)
		for stableLen > 0 && rendered[stableLen-1] == '\n' {
			stableLen--
		}

		if stableLen > 0 {
			if _, err := sr.output.Write(rendered[:stableLen]); err != nil {
				return err
			}
			sr.renderedLen = stableLen
		}
	}

	return nil
}

// detectBlock determines the type of block a line starts.
func (sr *StreamRenderer) detectBlock(line string) blockType {
	trimmed := strings.TrimLeft(line, " \t")

	if len(trimmed) == 0 {
		return blockUnknown // blank line
	}

	// Fenced code: ``` or ~~~
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		return blockFencedCode
	}

	// Heading: # (ATX style)
	if trimmed[0] == '#' {
		// Verify it's a valid heading (# followed by space or end of line)
		for i, c := range trimmed {
			if c != '#' {
				if c == ' ' || c == '\t' {
					return blockHeading
				}
				break
			}
			if i >= 6 { // Max 6 # characters
				break
			}
		}
		// Check for empty heading like "##\n"
		allHashes := true
		for _, c := range trimmed {
			if c != '#' {
				allHashes = false
				break
			}
		}
		if allHashes && len(trimmed) <= 6 {
			return blockHeading
		}
	}

	// Thematic break: ---, ***, ___ (with optional spaces)
	if isThematicBreak(trimmed) {
		return blockThematicBreak
	}

	// Blockquote: >
	if trimmed[0] == '>' {
		return blockBlockquote
	}

	// List: -, *, +, or digit followed by . or )
	if isListMarker(trimmed) {
		return blockList
	}

	// Table: contains | (but not at start of line for blockquotes)
	// Check for table structure: line contains | and looks like a table row
	if isTableLine(line) {
		return blockTable
	}

	return blockParagraph
}

// isBlankLine returns true if the line contains only whitespace.
func isBlankLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

// isListMarker returns true if the line starts with a list marker.
func isListMarker(trimmed string) bool {
	if len(trimmed) == 0 {
		return false
	}

	// Unordered list markers: -, *, +
	if (trimmed[0] == '-' || trimmed[0] == '*' || trimmed[0] == '+') &&
		len(trimmed) > 1 && (trimmed[1] == ' ' || trimmed[1] == '\t') {
		return true
	}

	// Ordered list markers: digit(s) followed by . or )
	i := 0
	for i < len(trimmed) && i < 9 && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i > 0 && i < len(trimmed) && (trimmed[i] == '.' || trimmed[i] == ')') {
		if i+1 < len(trimmed) && (trimmed[i+1] == ' ' || trimmed[i+1] == '\t') {
			return true
		}
		// Handle case like "1.\n" (number followed by marker at end)
		if i+1 == len(trimmed) {
			return true
		}
	}

	return false
}

// isThematicBreak returns true if the line is a thematic break (---, ***, ___).
func isThematicBreak(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}

	// Must be at least 3 of the same character (-, *, _) with optional spaces
	char := rune(trimmed[0])
	if char != '-' && char != '*' && char != '_' {
		return false
	}

	count := 0
	for _, c := range trimmed {
		if c == char {
			count++
		} else if c != ' ' && c != '\t' {
			return false
		}
	}

	return count >= 3
}

// isTableLine returns true if the line appears to be part of a table.
func isTableLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}

	// A table line should contain at least one | character
	// But we need to be careful not to confuse with other constructs
	if !strings.Contains(trimmed, "|") {
		return false
	}

	// Simple heuristic: if it has | and doesn't look like something else, it's a table
	return true
}

// isSetextUnderline returns true if the line is a setext heading underline.
func isSetextUnderline(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}

	// Must be all = or all - (with optional trailing spaces already trimmed)
	char := trimmed[0]
	if char != '=' && char != '-' {
		return false
	}

	for _, c := range trimmed {
		if byte(c) != char {
			return false
		}
	}

	return true
}

// parseFence extracts fence info from a fence opening line.
func parseFence(line string) (char rune, length int, indent int) {
	indent = countLeadingSpaces(line)
	trimmed := strings.TrimLeft(line, " \t")

	if len(trimmed) == 0 {
		return 0, 0, 0
	}

	char = rune(trimmed[0])
	length = 0
	for _, c := range trimmed {
		if c == char {
			length++
		} else {
			break
		}
	}

	return char, length, indent
}

// isClosingFence returns true if the line is a valid closing fence.
func isClosingFence(line string, openChar rune, openLen int, openIndent int) bool {
	indent := countLeadingSpaces(line)
	// Closing fence can have up to 3 spaces of indentation
	if indent > 3 && indent > openIndent+3 {
		return false
	}

	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) == 0 {
		return false
	}

	// Must start with same fence character
	if rune(trimmed[0]) != openChar {
		return false
	}

	// Count fence characters
	fenceLen := 0
	for _, c := range trimmed {
		if c == openChar {
			fenceLen++
		} else if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			// Trailing whitespace is OK
			break
		} else {
			// Other characters after fence chars means not a closing fence
			return false
		}
	}

	// Closing fence must have at least as many fence chars as opening
	return fenceLen >= openLen
}

// countLeadingSpaces returns the number of leading space characters.
// Tabs are counted as 1 for simplicity.
func countLeadingSpaces(line string) int {
	count := 0
	for _, c := range line {
		if c == ' ' {
			count++
		} else if c == '\t' {
			count++ // Simplified: treat tab as 1 space for indent comparison
		} else {
			break
		}
	}
	return count
}
