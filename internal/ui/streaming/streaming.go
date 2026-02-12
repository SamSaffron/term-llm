// Package streaming provides a streaming markdown renderer that wraps glamour's
// TermRenderer. It buffers markdown input and renders complete blocks as they
// become available, making it suitable for rendering markdown from streaming
// sources like LLM outputs.
package streaming

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/samsaffron/term-llm/internal/ui/ansisafe"
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
	// Last rendered snapshot that has been written to output.
	// Used to append deltas safely and recover from non-prefix renders.
	lastRendered []byte

	// Current state
	state state

	// Fenced code block state
	fenceChar   rune // '`' or '~'
	fenceLen    int  // number of fence characters
	fenceIndent int  // leading spaces before fence

	// List state
	listIndent           int  // base indent level of current list
	lastListMarkerIndent int  // indent of the most recent list marker line
	listHasMarker        bool // whether we've seen at least one marker in current list block

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

	// Resume state when a nested block (like fenced code) ends.
	// Used to keep list context stable across nested blocks.
	resumeState state
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

// PendingMarkdown returns the current incomplete block markdown.
// This includes pending complete lines plus any partial line in the buffer.
func (sr *StreamRenderer) PendingMarkdown() string {
	return sr.currentBlockContent()
}

// PendingIsTable reports whether the current incomplete block should be treated
// as a table for preview purposes.
func (sr *StreamRenderer) PendingIsTable() bool {
	if sr.state == stateInTable {
		return true
	}

	trimmed := sr.firstPendingLine()
	if trimmed == "" {
		return false
	}

	// Prefer deterministic table starts (pipe-first rows), which avoids
	// suppressing preview for normal prose that merely contains a pipe character.
	return strings.HasPrefix(trimmed, "|")
}

// PendingIsList reports whether the current incomplete block should be treated
// as a list for preview purposes.
func (sr *StreamRenderer) PendingIsList() bool {
	if sr.state == stateInList {
		return true
	}

	trimmed := sr.firstPendingLine()
	if trimmed == "" {
		return false
	}

	if isListMarker(trimmed) {
		return true
	}

	// Handle marker-only partials while tokens are still arriving.
	// Keep this conservative to avoid suppressing normal prose previews
	// (for example a lone "*" while emphasis syntax is still streaming).
	return isOrderedListMarkerPrefix(trimmed) || trimmed == "-" || trimmed == "+"
}

// firstPendingLine returns the first incomplete line from current block content,
// left-trimmed of indentation and without trailing carriage return.
func (sr *StreamRenderer) firstPendingLine() string {
	content := sr.currentBlockContent()
	if content == "" {
		return ""
	}

	firstLine := content
	if idx := strings.Index(firstLine, "\n"); idx >= 0 {
		firstLine = firstLine[:idx]
	}
	firstLine = strings.TrimSuffix(firstLine, "\r")
	return strings.TrimLeft(firstLine, " \t")
}

// beginList initializes list-tracking state when a new list block starts.
func (sr *StreamRenderer) beginList(indent int) {
	sr.state = stateInList
	sr.listIndent = indent
	sr.lastListMarkerIndent = indent
	sr.listHasMarker = true
}

// resetListState clears list-tracking metadata when list processing ends.
func (sr *StreamRenderer) resetListState() {
	sr.listIndent = 0
	sr.lastListMarkerIndent = 0
	sr.listHasMarker = false
}

// commitPendingLines appends pending block lines into allMarkdown and emits
// the incremental rendered delta for the updated full document.
func (sr *StreamRenderer) commitPendingLines() error {
	if len(sr.pendingLines) == 0 {
		return nil
	}
	for _, l := range sr.pendingLines {
		sr.allMarkdown.WriteString(l)
	}
	sr.pendingLines = nil
	return sr.emitRendered()
}

// applyRenderedSnapshot writes the next rendered snapshot using one of:
//  1. append-only delta when snapshot extends previous output
//  2. append-only best-effort delta (ANSI-safe) when prefix changes and output supports Reset()
//  3. explicit error for non-resettable outputs when prefix changes
//
// When allowRewrite is true, case (2) rewrites the full resettable output.
func (sr *StreamRenderer) applyRenderedSnapshot(snapshot []byte, allowRewrite bool) error {
	if bytes.Equal(snapshot, sr.lastRendered) {
		return nil
	}

	// Fast path: append-only when new render extends previous render.
	if bytes.HasPrefix(snapshot, sr.lastRendered) {
		if len(snapshot) > len(sr.lastRendered) {
			if _, err := sr.output.Write(snapshot[len(sr.lastRendered):]); err != nil {
				return err
			}
		}
		sr.lastRendered = append(sr.lastRendered[:0], snapshot...)
		sr.renderedLen = len(sr.lastRendered)
		return nil
	}

	// Prefix changed.
	resetter, resettable := sr.output.(interface{ Reset() })
	if !resettable {
		// Non-resettable writer and changed prefix means we cannot emit a safe
		// incremental delta without terminal cursor control.
		return fmt.Errorf("streaming renderer cannot update changed prefix with non-resettable writer")
	}

	if allowRewrite {
		resetter.Reset()
		if len(snapshot) > 0 {
			if _, err := sr.output.Write(snapshot); err != nil {
				return err
			}
		}
		sr.lastRendered = append(sr.lastRendered[:0], snapshot...)
		sr.renderedLen = len(sr.lastRendered)
		return nil
	}

	// Best-effort append-only delta for resettable writers: emit only new tail
	// bytes beyond the previous rendered length, but avoid slicing mid-ANSI.
	//
	// If the new snapshot is shorter, append-only mode cannot safely remove stale
	// trailing bytes that were already emitted. In that case we keep the previous
	// authoritative snapshot to stay consistent with append-only output.
	prevLen := len(sr.lastRendered)
	if len(snapshot) <= prevLen {
		return nil
	}
	delta := ansisafe.SuffixBytes(snapshot, prevLen)
	if len(delta) > 0 {
		if _, err := sr.output.Write(delta); err != nil {
			return err
		}
	}
	sr.lastRendered = append(sr.lastRendered[:0], snapshot...)
	sr.renderedLen = len(sr.lastRendered)
	return nil
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
		sr.beginList(countLeadingSpaces(content))
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
		if sr.resumeState == stateInList {
			// Return to list context without emitting yet.
			sr.state = sr.resumeState
			sr.resumeState = stateReady
			sr.fenceChar = 0
			sr.fenceLen = 0
			sr.fenceIndent = 0
			return nil
		}
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
	if sr.resumeState == stateInList {
		sr.state = stateInList
		sr.resumeState = stateReady
		return sr.handleList(content, rawLine)
	}
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
		// Stream top-level items as soon as a sibling marker arrives.
		// For nested lists, defer sibling flushes until the nested list closes
		// (outdent or return to base indent) to avoid loose-list rewrites.
		shouldFlushAtMarker := sr.listHasMarker &&
			(indent <= sr.listIndent || indent < sr.lastListMarkerIndent)
		if shouldFlushAtMarker {
			if err := sr.commitPendingLines(); err != nil {
				return err
			}
		}
		sr.pendingLines = append(sr.pendingLines, rawLine)
		if indent < sr.listIndent {
			sr.listIndent = indent
		}
		sr.lastListMarkerIndent = indent
		sr.listHasMarker = true
		return nil
	}

	// Check if a new block type is starting (not a paragraph)
	blockType := sr.detectBlock(content)
	if indent > sr.listIndent {
		switch blockType {
		case blockFencedCode:
			// Nested fenced code block inside list: stay in list context.
			sr.state = stateInFencedCode
			sr.resumeState = stateInList
			sr.fenceChar, sr.fenceLen, sr.fenceIndent = parseFence(content)
			sr.pendingLines = append(sr.pendingLines, rawLine)
			return nil
		case blockBlockquote:
			// Nested blockquote inside list: stay in list context.
			sr.state = stateInBlockquote
			sr.resumeState = stateInList
			sr.pendingLines = append(sr.pendingLines, rawLine)
			return nil
		case blockTable:
			// Nested table inside list: stay in list context.
			sr.state = stateInTable
			sr.resumeState = stateInList
			sr.pendingLines = append(sr.pendingLines, rawLine)
			return nil
		case blockHeading, blockThematicBreak:
			// Nested single-line block inside list: treat as list continuation.
			sr.pendingLines = append(sr.pendingLines, rawLine)
			return nil
		}
	}
	if blockType != blockParagraph && blockType != blockUnknown {
		// New block type, emit list
		sr.state = stateReady
		sr.resetListState()
		if err := sr.commitPendingLines(); err != nil {
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
	sr.state = stateReady
	sr.resetListState()
	if err := sr.commitPendingLines(); err != nil {
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
	if sr.resumeState == stateInList {
		sr.state = stateInList
		sr.resumeState = stateReady
		return sr.handleList(content, rawLine)
	}
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

	return sr.applyRenderedSnapshot(rendered[:stableLen], false)
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

	// Output final render including trailing newlines.
	return sr.applyRenderedSnapshot(rendered, true)
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
	sr.lastRendered = nil

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
			if err := sr.applyRenderedSnapshot(rendered[:stableLen], true); err != nil {
				return err
			}
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

// isOrderedListMarkerPrefix reports whether trimmed is a marker-only ordered
// list prefix like "1." or "2)" (without trailing content yet).
func isOrderedListMarkerPrefix(trimmed string) bool {
	if trimmed == "" {
		return false
	}

	i := 0
	for i < len(trimmed) && i < 9 && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(trimmed) {
		return false
	}
	if trimmed[i] != '.' && trimmed[i] != ')' {
		return false
	}
	return i+1 == len(trimmed)
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
