// Package streaming provides a streaming markdown renderer for incremental
// terminal markdown output. It buffers markdown input and renders complete
// blocks as they become available, making it suitable for streaming sources
// like LLM outputs.
package streaming

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"

	rendermarkdown "github.com/samsaffron/term-llm/internal/render/markdown"
)

// multiNewlineRe matches 3 or more consecutive newlines.
var multiNewlineRe = regexp.MustCompile(`\n{3,}`)

// normalizeNewlines reduces 3+ consecutive newlines to 2 (one blank line max).
func normalizeNewlines(s []byte) []byte {
	if !hasThreeConsecutiveNewlines(s) {
		return s
	}
	return multiNewlineRe.ReplaceAll(s, []byte("\n\n"))
}

func hasThreeConsecutiveNewlines(s []byte) bool {
	run := 0
	for _, b := range s {
		if b != '\n' {
			run = 0
			continue
		}
		run++
		if run >= 3 {
			return true
		}
	}
	return false
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

// StreamRenderer wraps a terminal markdown renderer and provides streaming capabilities.
// It buffers markdown input and renders complete blocks immediately to the output writer.
type StreamRenderer struct {
	renderer rendermarkdown.Renderer
	output   io.Writer

	// Line buffering - accumulates bytes until we have complete lines
	lineBuf bytes.Buffer

	// All markdown received so far (for re-rendering)
	allMarkdown bytes.Buffer
	hasTabs     bool // True once input contains a tab, enabling tab normalization only when needed

	// How many bytes of rendered output we've already written
	renderedLen int
	// Last rendered snapshot that has been written to output.
	// Used to append deltas safely and recover from non-prefix renders.
	lastRendered []byte
	// Last committed rendered snapshot (without any partial preview).
	// Used by callers that need a stable scrollback-safe view while partial
	// previews are visible in the active buffer.
	lastCommittedRendered []byte

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

	// Resume state when a nested block (like fenced code) ends.
	// Used to keep list context stable across nested blocks.
	resumeState state

	// Incremental rendering is a conservative fast path for complete blocks whose
	// rendered form is independent of future markdown. The full-document render
	// remains the correctness fallback for globally-sensitive constructs.
	incrementalUnsafe bool
}

// NewRenderer creates a new streaming markdown renderer.
func NewRenderer(w io.Writer, renderer rendermarkdown.Renderer) (*StreamRenderer, error) {
	if renderer == nil {
		return nil, fmt.Errorf("streaming renderer requires a markdown renderer")
	}

	return &StreamRenderer{
		renderer: renderer,
		output:   w,
		state:    stateReady,
	}, nil
}

// NewRendererWithOptions creates a new streaming markdown renderer with
// additional streaming-specific options like partial rendering.
func NewRendererWithOptions(
	w io.Writer,
	renderer rendermarkdown.Renderer,
	streamOpts []StreamRendererOption,
) (*StreamRenderer, error) {
	sr, err := NewRenderer(w, renderer)
	if err != nil {
		return nil, err
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
// This preserves legacy terminal rendering behaviour for code blocks.
func (sr *StreamRenderer) normalizedMarkdown() []byte {
	content := sr.allMarkdown.Bytes()
	if !sr.hasTabs {
		return content
	}
	return bytes.ReplaceAll(content, []byte("\t"), []byte("  "))
}

// Write accepts markdown chunks and renders complete blocks immediately.
// It implements io.Writer.
func (sr *StreamRenderer) Write(p []byte) (n int, err error) {
	if !sr.hasTabs && bytes.IndexByte(p, '\t') >= 0 {
		sr.hasTabs = true
	}

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

// CommittedRendered returns the latest rendered snapshot that contains only
// committed markdown blocks and excludes any active partial preview.
func (sr *StreamRenderer) CommittedRendered() string {
	return string(sr.lastCommittedRendered)
}

// RenderedSnapshot returns the latest rendered bytes. The returned slice is
// read-only and remains stable across future writes.
func (sr *StreamRenderer) RenderedSnapshot() []byte {
	return sr.lastRendered
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
	return firstPendingLine(sr.currentBlockContent())
}

func firstPendingLine(content string) string {
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
	markdown := sr.takePendingLines()
	if len(markdown) == 0 {
		return nil
	}
	return sr.emitCommittedFull(markdown)
}

func (sr *StreamRenderer) commitPendingLinesIncremental() error {
	markdown := sr.takePendingLines()
	if len(markdown) == 0 {
		return nil
	}
	return sr.emitCommittedBlock(markdown)
}

func (sr *StreamRenderer) takePendingLines() []byte {
	if len(sr.pendingLines) == 0 {
		return nil
	}
	var markdown bytes.Buffer
	for _, l := range sr.pendingLines {
		markdown.WriteString(l)
		sr.allMarkdown.WriteString(l)
	}
	sr.pendingLines = nil
	return markdown.Bytes()
}

func (sr *StreamRenderer) emitCommittedFull(markdown []byte) error {
	if markdownNeedsFullDocumentRender(markdown) || markdownHasGlobalMarkdownSemantics(markdown) {
		sr.incrementalUnsafe = true
	}
	return sr.emitRendered()
}

// emitCommittedBlock renders a newly committed top-level block. It first tries
// a conservative append-only render of just that block; globally-sensitive
// markdown falls back to the legacy full-document render so correctness remains
// anchored to the complete parser output.
func (sr *StreamRenderer) emitCommittedBlock(markdown []byte) error {
	if sr.canAppendRenderedBlock(markdown) {
		if err := sr.emitRenderedBlock(markdown); err == nil {
			return nil
		}
	}
	return sr.emitRendered()
}

func (sr *StreamRenderer) commitRawBlock(markdown string, allowIncremental bool) error {
	sr.allMarkdown.WriteString(markdown)
	if !allowIncremental {
		return sr.emitCommittedFull([]byte(markdown))
	}
	return sr.emitCommittedBlock([]byte(markdown))
}

func (sr *StreamRenderer) commitPendingLinesWith(rawLine string, allowIncremental bool) error {
	markdown := sr.takePendingLines()
	if rawLine != "" {
		markdown = append(markdown, rawLine...)
		sr.allMarkdown.WriteString(rawLine)
	}
	if len(markdown) == 0 {
		return nil
	}
	if !allowIncremental {
		return sr.emitCommittedFull(markdown)
	}
	return sr.emitCommittedBlock(markdown)
}

func (sr *StreamRenderer) canAppendRenderedBlock(markdown []byte) bool {
	if sr.incrementalUnsafe {
		return false
	}
	if markdownNeedsFullDocumentRender(markdown) {
		sr.incrementalUnsafe = true
		return false
	}
	if markdownHasGlobalMarkdownSemantics(markdown) {
		sr.incrementalUnsafe = true
		return false
	}
	return true
}

func markdownNeedsFullDocumentRender(markdown []byte) bool {
	lines := bytes.Split(markdown, []byte("\n"))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lineString := string(line)
		trimmed := strings.TrimLeft(lineString, " \t")
		// Setext heading underlines can rewrite the previous paragraph into a
		// heading. Thematic breaks are harmless but share this syntax, so use the
		// full document render as the safe oracle for either case.
		if isSetextUnderline(trimmed) {
			return true
		}
		// Blockquotes can merge with neighbouring blockquote lines and can contain
		// constructs (for example setext underlines) that rewrite earlier content.
		if strings.HasPrefix(trimmed, ">") {
			return true
		}
		// Any indentation can indicate continuation content for a previous block
		// such as a list item. Rendering it standalone would insert a top-level
		// separator that is not present in the full document render.
		if countLeadingSpaces(lineString) > 0 {
			return true
		}
		// Adjacent list chunks can be one list in the full document but separate
		// lists when rendered block-by-block, changing separators and tightness.
		if isListMarker(trimmed) || isListMarkerOnly(trimmed) {
			return true
		}
	}
	return false
}

func markdownHasGlobalMarkdownSemantics(markdown []byte) bool {
	if isFencedCodeBlockMarkdown(markdown) {
		return false
	}
	lines := bytes.Split(markdown, []byte("\n"))
	for _, line := range lines {
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) == 0 {
			continue
		}
		// Link reference definitions can affect links anywhere earlier in the
		// document, so once one appears the safe append-only model is invalid.
		if lineHasReferenceDefinition(trimmed) {
			return true
		}
		// Reference-style and shortcut reference links may be resolved by future
		// definitions. Inline links ([label](url)) are local and safe.
		if lineHasPotentialReferenceLink(trimmed) {
			return true
		}
		// Raw HTML block boundaries and inline/raw rendering can be affected by
		// neighbouring lines. Keep the fast path boring rather than clever.
		if trimmed[0] == '<' {
			return true
		}
	}
	return false
}

func isFencedCodeBlockMarkdown(markdown []byte) bool {
	firstLineEnd := bytes.IndexByte(markdown, '\n')
	if firstLineEnd < 0 {
		return false
	}
	firstLine := string(markdown[:firstLineEnd+1])
	fenceChar, fenceLen, fenceIndent := parseFence(firstLine)
	if fenceIndent != 0 || fenceLen < 3 || (fenceChar != '`' && fenceChar != '~') {
		return false
	}

	remaining := markdown[firstLineEnd+1:]
	for len(remaining) > 0 {
		lineEnd := bytes.IndexByte(remaining, '\n')
		if lineEnd < 0 {
			return isClosingFence(string(remaining), fenceChar, fenceLen, fenceIndent)
		}
		line := remaining[:lineEnd+1]
		if isClosingFence(string(line), fenceChar, fenceLen, fenceIndent) {
			return true
		}
		remaining = remaining[lineEnd+1:]
	}
	return false
}

func lineHasReferenceDefinition(line []byte) bool {
	return len(line) > 1 && line[0] == '[' && bytes.Contains(line, []byte("]:"))
}

func lineHasPotentialReferenceLink(line []byte) bool {
	for i := 0; i < len(line); i++ {
		if line[i] != '[' {
			continue
		}
		closeIdx := bytes.IndexByte(line[i+1:], ']')
		if closeIdx < 0 {
			continue
		}
		after := i + 1 + closeIdx + 1
		if after < len(line) && line[after] == '(' {
			continue
		}
		return true
	}
	return false
}

func trimRenderedBlock(rendered []byte) []byte {
	return bytes.Trim(rendered, "\n")
}

func (sr *StreamRenderer) emitRenderedBlock(markdown []byte) error {
	if sr.partialEnabled {
		if err := sr.clearPartialState(); err != nil {
			return err
		}
	}

	blockMarkdown := markdown
	if sr.hasTabs {
		blockMarkdown = bytes.ReplaceAll(markdown, []byte("\t"), []byte("  "))
	}

	rendered, err := sr.renderer.Render(blockMarkdown)
	if err != nil {
		return err
	}
	rendered = normalizeNewlines(rendered)
	rendered = trimRenderedBlock(rendered)
	if len(rendered) == 0 {
		return nil
	}

	// Match the ANSI renderer's top-level block separator. Grow the committed
	// snapshot in place so appending a block does not copy the entire rendered
	// response on every commit. If internal/render/markdown changes
	// renderBlockChildren's document separator, committed-boundary parity tests
	// should fail before this leaks to scrollback.
	if len(sr.lastCommittedRendered) > 0 {
		sr.lastCommittedRendered = append(sr.lastCommittedRendered, '\n', '\n')
	}
	sr.lastCommittedRendered = append(sr.lastCommittedRendered, rendered...)
	return sr.applyRenderedSnapshot(sr.lastCommittedRendered, false)
}

// applyRenderedSnapshot writes the next rendered snapshot using one of:
//  1. append-only delta when snapshot extends previous output
//  2. full rewrite when prefix changes and output supports Reset()
//  3. explicit error for non-resettable outputs when prefix changes
//
// Rewriting changed-prefix snapshots guarantees deterministic output and
// prevents dropped lines when markdown reflows modify earlier bytes.
func (sr *StreamRenderer) applyRenderedSnapshot(snapshot []byte, _ bool) error {
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
		sr.lastRendered = snapshot
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

	// Changed prefix on a resettable writer: rewrite the full snapshot.
	// Append-only deltas are not safe because bytes before the previous length
	// may have changed.
	sr.lastRendered = snapshot
	sr.renderedLen = len(sr.lastRendered)
	resetter.Reset()
	if len(snapshot) > 0 {
		if _, err := sr.output.Write(snapshot); err != nil {
			return err
		}
	}
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
		// Headings are complete immediately (single line). Only use the
		// block-local fast path when indentation cannot reinterpret the line as
		// an indented code continuation in CommonMark.
		return sr.commitRawBlock(rawLine, countLeadingSpaces(content) < 4)

	case blockThematicBreak:
		// Thematic breaks are complete immediately. Keep the fast path limited to
		// CommonMark top-level indentation.
		return sr.commitRawBlock(rawLine, countLeadingSpaces(content) < 4)

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
		// Commit pending paragraph plus the terminating blank line.
		sr.state = stateReady
		return sr.commitPendingLinesWith(rawLine, true)
	}

	// IMPORTANT: Check for setext heading underline FIRST (=== or ---)
	// This must be checked before thematic break because --- is ambiguous
	if isSetextUnderline(content) && len(sr.pendingLines) > 0 {
		// This converts the paragraph to a heading
		sr.state = stateReady
		return sr.commitPendingLinesWith(rawLine, true)
	}

	// Check if this line starts a new block type
	blockType := sr.detectBlock(content)

	switch blockType {
	case blockFencedCode, blockHeading, blockThematicBreak, blockTable, blockList, blockBlockquote:
		// Commit current paragraph first
		sr.state = stateReady
		if err := sr.commitPendingLines(); err != nil {
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
		allowIncremental := sr.fenceIndent < 4
		sr.state = stateReady
		sr.fenceChar = 0
		sr.fenceLen = 0
		sr.fenceIndent = 0
		if allowIncremental {
			return sr.commitPendingLinesIncremental()
		}
		return sr.commitPendingLines()
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
	sr.state = stateReady
	if err := sr.commitPendingLinesIncremental(); err != nil {
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
	sr.state = stateReady
	if err := sr.commitPendingLines(); err != nil {
		return err
	}
	return sr.handleReady(content, rawLine)
}

// emitRendered renders the full document and outputs only the new portion.
// This maintains direct-render parity while avoiding redundant output of
// already-written content.
func (sr *StreamRenderer) emitRendered() error {
	// Clear partial render before emitting complete block
	if sr.partialEnabled {
		if err := sr.clearPartialState(); err != nil {
			return err
		}
	}

	if sr.allMarkdown.Len() == 0 {
		sr.lastCommittedRendered = nil
		return nil
	}

	// Render the full document to maintain consistent styling
	rendered, err := sr.renderer.Render(sr.normalizedMarkdown())
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

	sr.lastCommittedRendered = bytes.Clone(rendered[:stableLen])
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
		sr.lastCommittedRendered = nil
		return nil
	}

	// Render the full document to maintain consistent styling
	rendered, err := sr.renderer.Render(sr.normalizedMarkdown())
	if err != nil {
		return err
	}

	// Normalize consecutive newlines to fix inconsistent header spacing
	rendered = normalizeNewlines(rendered)
	sr.lastCommittedRendered = bytes.Clone(rendered)

	// Output final render including trailing newlines.
	return sr.applyRenderedSnapshot(rendered, true)
}

// Close flushes any remaining content and cleans up.
func (sr *StreamRenderer) Close() error {
	return sr.Flush()
}

// Resize handles terminal resize events by resizing the markdown renderer
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

	sr.renderer.Resize(newWidth)

	// Clear partial state
	sr.partialState = partialState{}

	// Reset render tracking - we'll re-render everything
	sr.renderedLen = 0
	sr.lastRendered = nil

	if sr.allMarkdown.Len() > 0 {
		rendered, err := sr.renderer.Render(sr.normalizedMarkdown())
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

		sr.lastCommittedRendered = bytes.Clone(rendered[:stableLen])
		if stableLen > 0 {
			if err := sr.applyRenderedSnapshot(rendered[:stableLen], true); err != nil {
				return err
			}
		}
	} else {
		sr.lastCommittedRendered = nil
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

// isListMarkerOnly reports whether trimmed contains just a list marker and
// optional trailing whitespace, with no item content yet.
func isListMarkerOnly(trimmed string) bool {
	if trimmed == "" {
		return false
	}

	if trimmed == "-" || trimmed == "+" || trimmed == "*" {
		return true
	}

	if (trimmed[0] == '-' || trimmed[0] == '+' || trimmed[0] == '*') && len(trimmed) > 1 {
		if trimmed[1] == ' ' || trimmed[1] == '\t' {
			return strings.TrimSpace(trimmed[2:]) == ""
		}
	}

	i := 0
	for i < len(trimmed) && i < 9 && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(trimmed) || (trimmed[i] != '.' && trimmed[i] != ')') {
		return false
	}

	rest := strings.TrimSpace(trimmed[i+1:])
	return rest == ""
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
