package markdown

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	gtext "github.com/yuin/goldmark/text"
)

// Renderer renders complete markdown content for a terminal target.
type Renderer interface {
	Render(source []byte) ([]byte, error)
	Resize(width int)
}

// Palette describes the semantic colours used by the terminal renderer.
type Palette struct {
	Primary   string
	Secondary string
	Success   string
	Warning   string
	Muted     string
	Text      string
}

// Config controls how markdown is rendered for a specific caller.
type Config struct {
	Palette Palette

	// Width is the caller-visible width before any wrap offset is applied.
	Width int
	// WrapOffset preserves legacy call-site behaviour where some renderers use a
	// slightly smaller wrap width than the viewport width.
	WrapOffset int

	NormalizeTabs      bool
	NormalizeNewlines  bool
	TrimSpace          bool
	EnsureTrailingLine bool
}

// ANSI is the terminal markdown renderer implementation.
type ANSI struct {
	config Config
}

// NewANSI creates a new terminal markdown renderer.
func NewANSI(config Config) *ANSI {
	return &ANSI{config: config}
}

// Resize updates the renderer width for future renders.
func (r *ANSI) Resize(width int) {
	r.config.Width = width
}

var markdownParser = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.DefinitionList,
	),
)

// Render renders markdown to ANSI bytes.
func (r *ANSI) Render(source []byte) ([]byte, error) {
	content := string(source)
	if r.config.NormalizeTabs {
		// Deliberately 2 spaces, not the CommonMark-spec 4.
		// Keeps terminal output compact while remaining readable.
		content = strings.ReplaceAll(content, "\t", "  ")
	}
	if strings.TrimSpace(content) == "" {
		return []byte(""), nil
	}

	doc := markdownParser.Parser().Parse(gtext.NewReader([]byte(content)))
	styles := newANSIStyles(r.config.Palette)
	rendered, err := r.renderBlockChildren(doc, []byte(content), r.wrapWidth(), styles, "\n\n")
	if err != nil {
		return nil, err
	}

	if r.config.NormalizeNewlines {
		rendered = normalizeNewlines(rendered)
	}
	if r.config.TrimSpace {
		rendered = strings.TrimSpace(rendered)
	}
	if r.config.EnsureTrailingLine && rendered != "" && !strings.HasSuffix(rendered, "\n") {
		rendered += "\n"
	}

	return []byte(rendered), nil
}

func (r *ANSI) wrapWidth() int {
	width := r.config.Width - r.config.WrapOffset
	if width < 1 {
		return 1
	}
	return width
}

// RenderString is a convenience helper for one-shot rendering.
func RenderString(content string, config Config) (string, error) {
	rendered, err := NewANSI(config).Render([]byte(content))
	if err != nil {
		return "", err
	}
	return string(rendered), nil
}

type ansiStyle struct {
	prefix string
}

func (s ansiStyle) Render(text string) string {
	if text == "" || s.prefix == "" {
		return text
	}
	return s.prefix + text + "\x1b[0m"
}

type ansiStyles struct {
	text          ansiStyle
	heading       ansiStyle
	blockquote    ansiStyle
	strong        ansiStyle
	emphasis      ansiStyle
	strikethrough ansiStyle
	code          ansiStyle
	link          ansiStyle
	image         ansiStyle
	rule          ansiStyle
	definition    ansiStyle
	tableHeader   ansiStyle
	tableBorder   ansiStyle
}

func newANSIStyle(color string, attrs ...string) ansiStyle {
	codes := make([]string, 0, len(attrs)+1)
	for _, attr := range attrs {
		if attr != "" {
			codes = append(codes, attr)
		}
	}
	if fg := colorCode(color); fg != "" {
		codes = append(codes, fg)
	}
	if len(codes) == 0 {
		return ansiStyle{}
	}
	return ansiStyle{prefix: "\x1b[" + strings.Join(codes, ";") + "m"}
}

func colorCode(color string) string {
	color = strings.TrimSpace(color)
	if color == "" {
		return ""
	}
	if strings.HasPrefix(color, "#") && len(color) == 7 {
		r, errR := strconv.ParseInt(color[1:3], 16, 64)
		g, errG := strconv.ParseInt(color[3:5], 16, 64)
		b, errB := strconv.ParseInt(color[5:7], 16, 64)
		if errR == nil && errG == nil && errB == nil {
			return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
		}
	}
	if n, err := strconv.Atoi(color); err == nil {
		return fmt.Sprintf("38;5;%d", n)
	}
	return ""
}

func newANSIStyles(p Palette) ansiStyles {
	return ansiStyles{
		text:          newANSIStyle(p.Text),
		heading:       newANSIStyle(p.Secondary, "1"),
		blockquote:    newANSIStyle(p.Warning, "3"),
		strong:        newANSIStyle(p.Primary, "1"),
		emphasis:      newANSIStyle(p.Warning, "3"),
		strikethrough: newANSIStyle("", "9"),
		code:          newANSIStyle(p.Primary),
		link:          newANSIStyle(p.Secondary, "4"),
		image:         newANSIStyle(p.Muted),
		rule:          newANSIStyle(p.Muted),
		definition:    newANSIStyle(p.Secondary),
		tableHeader:   newANSIStyle(p.Secondary, "1"),
		tableBorder:   newANSIStyle(p.Muted, "2"),
	}
}

func (r *ANSI) renderBlockChildren(parent gast.Node, source []byte, width int, styles ansiStyles, separator string) (string, error) {
	var blocks []string
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		rendered, err := r.renderBlock(child, source, width, styles)
		if err != nil {
			return "", err
		}
		rendered = strings.Trim(rendered, "\n")
		if rendered != "" {
			blocks = append(blocks, rendered)
		}
	}
	return strings.Join(blocks, separator), nil
}

func (r *ANSI) renderBlock(node gast.Node, source []byte, width int, styles ansiStyles) (string, error) {
	switch n := node.(type) {
	case *gast.Heading:
		inline, err := r.renderInlineChildren(n, source, styles)
		if err != nil {
			return "", err
		}
		label := styles.heading.Render(strings.Repeat("#", max(n.Level, 1)))
		if strings.TrimSpace(stripANSI(inline)) == "" {
			return wrapANSI(label, width), nil
		}
		return wrapANSI(label+" "+inline, width), nil
	case *gast.Paragraph:
		inline, err := r.renderInlineChildren(n, source, styles)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(stripANSI(inline)) == "" {
			return styles.text.Render(" "), nil
		}
		return wrapANSI(inline, width), nil
	case *gast.TextBlock:
		inline, err := r.renderInlineChildren(n, source, styles)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(stripANSI(inline)) == "" {
			return styles.text.Render(" "), nil
		}
		return wrapANSI(inline, width), nil
	case *gast.Blockquote:
		inner, err := r.renderBlockChildren(n, source, max(width-2, 1), styles, "\n")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(stripANSI(inner)) == "" {
			return styles.blockquote.Render(" "), nil
		}
		return prefixLinesWithStyle(inner, "  ", "  ", styles.blockquote), nil
	case *gast.List:
		return r.renderList(n, source, width, styles)
	case *gast.FencedCodeBlock:
		return r.renderCodeBlock(string(n.Text(source)), string(n.Language(source)), width, styles), nil
	case *gast.CodeBlock:
		return r.renderCodeBlock(string(n.Text(source)), "", width, styles), nil
	case *extast.Table:
		return r.renderTable(n, source, width, styles)
	case *gast.ThematicBreak:
		return styles.rule.Render("--------"), nil
	case *extast.DefinitionList:
		return r.renderBlockChildren(n, source, width, styles, "\n")
	case *extast.DefinitionTerm:
		text := strings.TrimSpace(r.plainInlineChildren(n, source, true))
		if text == "" {
			return "", nil
		}
		return wrapANSI(styles.strong.Render(text), width), nil
	case *extast.DefinitionDescription:
		inner, err := r.renderBlockChildren(n, source, max(width-2, 1), styles, "\n")
		if err != nil {
			return "", err
		}
		return prefixLinesWithStyle(inner, "🠶 ", "  ", styles.definition), nil
	case *gast.HTMLBlock:
		return wrapANSI(styles.text.Render(string(bytesOrEmpty(node.Text(source)))), width), nil
	default:
		if node.FirstChild() != nil {
			return r.renderBlockChildren(node, source, width, styles, "\n")
		}
		return "", nil
	}
}

func (r *ANSI) renderList(list *gast.List, source []byte, width int, styles ansiStyles) (string, error) {
	var items []string
	number := list.Start
	if number <= 0 {
		number = 1
	}
	for itemNode := list.FirstChild(); itemNode != nil; itemNode = itemNode.NextSibling() {
		item, ok := itemNode.(*gast.ListItem)
		if !ok {
			continue
		}

		prefix := "• "
		if list.IsOrdered() {
			prefix = fmt.Sprintf("%d. ", number)
			number++
		}
		innerWidth := max(width-visibleWidth(prefix), 1)
		body, err := r.renderBlockChildren(item, source, innerWidth, styles, "\n")
		if err != nil {
			return "", err
		}
		body = strings.Trim(body, "\n")
		if body == "" {
			items = append(items, prefix)
			continue
		}
		items = append(items, prefixExistingLines(body, prefix, strings.Repeat(" ", visibleWidth(prefix))))
	}
	return strings.Join(items, "\n"), nil
}

func (r *ANSI) renderTable(table *extast.Table, source []byte, width int, styles ansiStyles) (string, error) {
	var header []string
	var bodyRows [][]string
	var aligns []extast.Alignment
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch row := child.(type) {
		case *extast.TableHeader:
			header = r.collectTableRow(row, source, styles, true)
			if len(row.Alignments) > 0 {
				aligns = row.Alignments
			}
		case *extast.TableRow:
			bodyRows = append(bodyRows, r.collectTableRow(row, source, styles, false))
			if len(aligns) == 0 && len(row.Alignments) > 0 {
				aligns = row.Alignments
			}
		}
	}
	if len(header) == 0 && len(bodyRows) == 0 {
		return "", nil
	}

	colCount := len(header)
	for _, row := range bodyRows {
		if len(row) > colCount {
			colCount = len(row)
		}
	}
	if colCount == 0 {
		return "", nil
	}

	naturalWidths := make([]int, colCount)
	measureRow := func(row []string) {
		for i, cell := range row {
			cellWidth := visibleWidth(cell)
			if cellWidth > naturalWidths[i] {
				naturalWidths[i] = cellWidth
			}
		}
	}
	measureRow(header)
	for _, row := range bodyRows {
		measureRow(row)
	}

	gridWidth := max(width-4, 1)
	widths := append([]int(nil), naturalWidths...)
	fitColumnWidths(widths, gridWidth, colCount)

	if len(bodyRows) > 0 && shouldRenderTableAsRecords(naturalWidths, widths) {
		return r.renderTableRecords(header, bodyRows, width, styles), nil
	}
	return r.renderTableGrid(header, bodyRows, aligns, widths, styles), nil
}

func (r *ANSI) renderTableGrid(header []string, bodyRows [][]string, aligns []extast.Alignment, widths []int, styles ansiStyles) string {
	rows := make([][]string, 0, len(bodyRows)+1)
	if len(header) > 0 {
		rows = append(rows, header)
	}
	rows = append(rows, bodyRows...)
	if len(rows) == 0 {
		return ""
	}

	topBorder := renderTableRule(widths, "┌", "┬", "┐", styles)
	middleBorder := renderTableRule(widths, "├", "┼", "┤", styles)
	bottomBorder := renderTableRule(widths, "└", "┴", "┘", styles)

	var lines []string
	lines = append(lines, topBorder)
	for rowIndex, row := range rows {
		wrappedCells := make([][]string, len(widths))
		maxCellLines := 1
		for i := range widths {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			cellLines := wrapCellLines(cell, widths[i])
			wrappedCells[i] = cellLines
			if len(cellLines) > maxCellLines {
				maxCellLines = len(cellLines)
			}
		}

		for lineIdx := 0; lineIdx < maxCellLines; lineIdx++ {
			var line strings.Builder
			line.WriteString(styles.tableBorder.Render("│"))
			line.WriteString(" ")
			for i := range widths {
				cellText := ""
				if lineIdx < len(wrappedCells[i]) {
					cellText = wrappedCells[i][lineIdx]
				}
				line.WriteString(padCell(cellText, widths[i], alignmentAt(aligns, i)))
				line.WriteString(" ")
				line.WriteString(styles.tableBorder.Render("│"))
				if i < len(widths)-1 {
					line.WriteString(" ")
				}
			}
			lines = append(lines, line.String())
		}

		if rowIndex < len(rows)-1 {
			lines = append(lines, middleBorder)
		}
	}
	lines = append(lines, bottomBorder)
	return strings.Join(lines, "\n")
}

func (r *ANSI) renderTableRecords(header []string, bodyRows [][]string, width int, styles ansiStyles) string {
	if width < 1 {
		width = 1
	}

	labels := make([]string, len(header))
	copy(labels, header)
	for _, row := range bodyRows {
		if len(row) > len(labels) {
			extra := make([]string, len(row)-len(labels))
			labels = append(labels, extra...)
		}
	}
	for i := range labels {
		if strings.TrimSpace(stripANSI(labels[i])) == "" {
			labels[i] = styles.tableHeader.Render(fmt.Sprintf("Column %d", i+1))
		}
	}

	recordDivider := styles.tableBorder.Render(strings.Repeat("─", width))
	var blocks []string
	for _, row := range bodyRows {
		var blockLines []string
		for i := range labels {
			label := labels[i]
			value := ""
			if i < len(row) {
				value = row[i]
			}
			blockLines = append(blockLines, renderTableRecordField(label, value, width)...)
		}
		blocks = append(blocks, strings.Join(blockLines, "\n"))
	}
	return strings.Join(blocks, "\n"+recordDivider+"\n")
}

func renderTableRecordField(label, value string, width int) []string {
	const minInlineValueWidth = 8

	if width < 1 {
		width = 1
	}

	prefix := label + ": "
	prefixWidth := visibleWidth(prefix)
	if prefixWidth < width && width-prefixWidth >= minInlineValueWidth {
		valueLines := wrapCellLines(value, width-prefixWidth)
		lines := make([]string, 0, len(valueLines))
		for i, line := range valueLines {
			if i == 0 {
				lines = append(lines, prefix+line)
				continue
			}
			lines = append(lines, strings.Repeat(" ", prefixWidth)+line)
		}
		return lines
	}

	lines := wrapCellLines(label+":", width)
	valueWidth := max(width-2, 1)
	for _, line := range wrapCellLines(value, valueWidth) {
		lines = append(lines, "  "+line)
	}
	return lines
}

func renderTableRule(widths []int, left, middle, right string, styles ansiStyles) string {
	parts := make([]string, len(widths))
	for i, w := range widths {
		if w < 1 {
			w = 1
		}
		parts[i] = strings.Repeat("─", w+2)
	}
	return styles.tableBorder.Render(left + strings.Join(parts, middle) + right)
}

func shouldRenderTableAsRecords(naturalWidths, fittedWidths []int) bool {
	const minReadableColumnWidth = 8

	for i := range fittedWidths {
		natural := 1
		if i < len(naturalWidths) && naturalWidths[i] > 0 {
			natural = naturalWidths[i]
		}
		if fittedWidths[i] < minReadableColumnWidth && fittedWidths[i] < natural {
			return true
		}
	}
	return false
}

func (r *ANSI) collectTableRow(node gast.Node, source []byte, styles ansiStyles, header bool) []string {
	var row []string
	for cellNode := node.FirstChild(); cellNode != nil; cellNode = cellNode.NextSibling() {
		cell, ok := cellNode.(*extast.TableCell)
		if !ok {
			continue
		}
		text, err := r.renderInlineChildren(cell, source, styles)
		if err != nil {
			text = r.plainInlineChildren(cell, source, true)
		}
		text = strings.ReplaceAll(text, "\n", " ")
		text = strings.TrimSpace(text)
		if header {
			text = styles.tableHeader.Render(text)
		}
		row = append(row, text)
	}
	return row
}

// codeBgEsc is a dim grey background used to visually distinguish code blocks.
const codeBgEsc = "\x1b[48;5;236m"

func (r *ANSI) renderCodeBlock(code, language string, width int, styles ansiStyles) string {
	code = strings.TrimRight(code, "\n")
	if code == "" {
		return styles.text.Render(" ")
	}
	lines := strings.Split(code, "\n")
	highlighter := getCodeHighlighter(language)
	var result []string
	for _, line := range lines {
		var styled string
		if highlighter != nil {
			styled = highlighter.highlightLine(line)
		} else {
			styled = styles.text.Render(line)
		}
		wrapped := wrapANSI(styled, width)
		for _, sub := range strings.Split(wrapped, "\n") {
			result = append(result, codeLineBg(sub, width))
		}
	}
	return strings.Join(result, "\n")
}

// codeLineBg wraps a single rendered code line with a background colour,
// re-applying it after every SGR reset so that syntax-highlight tokens
// don't punch holes in the background. The line is right-padded to width
// so the background fills the full block.
func codeLineBg(line string, width int) string {
	inner := strings.ReplaceAll(line, "\x1b[0m", "\x1b[0m"+codeBgEsc)
	pad := width - visibleWidth(line)
	if pad < 0 {
		pad = 0
	}
	return codeBgEsc + inner + strings.Repeat(" ", pad) + "\x1b[0m"
}

func (r *ANSI) renderInlineChildren(parent gast.Node, source []byte, styles ansiStyles) (string, error) {
	var b strings.Builder
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		rendered, err := r.renderInline(child, source, styles)
		if err != nil {
			return "", err
		}
		b.WriteString(rendered)
	}
	return b.String(), nil
}

func (r *ANSI) renderInline(node gast.Node, source []byte, styles ansiStyles) (string, error) {
	switch n := node.(type) {
	case *gast.Text:
		text := decodeInlineText(string(n.Value(source)))
		rendered := styles.text.Render(text)
		if n.HardLineBreak() {
			rendered += "\n"
		} else if n.SoftLineBreak() {
			rendered += " "
		}
		return rendered, nil
	case *gast.String:
		text := string(n.Value)
		if n.IsCode() {
			return styles.code.Render(text), nil
		}
		return styles.text.Render(text), nil
	case *gast.Emphasis:
		inner, err := r.renderInlineChildren(n, source, styles)
		if err != nil {
			return "", err
		}
		if n.Level >= 2 {
			return styles.strong.Render(inner), nil
		}
		return styles.emphasis.Render(inner), nil
	case *extast.Strikethrough:
		inner, err := r.renderInlineChildren(n, source, styles)
		if err != nil {
			return "", err
		}
		return styles.strikethrough.Render(inner), nil
	case *gast.CodeSpan:
		return styles.code.Render(r.plainInlineChildren(n, source, false)), nil
	case *gast.Link:
		label, err := r.renderInlineChildren(n, source, styles)
		if err != nil {
			return "", err
		}
		url := string(n.Destination)
		label = strings.TrimSpace(label)
		if label == "" {
			return styles.link.Render(url), nil
		}
		if strings.TrimSpace(stripANSI(label)) == url {
			return styles.link.Render(url), nil
		}
		return label + " " + styles.link.Render(url), nil
	case *gast.AutoLink:
		return styles.link.Render(string(n.URL(source))), nil
	case *gast.Image:
		alt := strings.TrimSpace(r.plainInlineChildren(n, source, true))
		if alt == "" {
			alt = "Image"
		}
		url := string(n.Destination)
		text := "Image: " + alt
		if url != "" {
			text += " → " + url
		}
		return styles.image.Render(text), nil
	case *extast.TaskCheckBox:
		if n.IsChecked {
			return styles.text.Render("[✓] "), nil
		}
		return styles.text.Render("[ ] "), nil
	case *gast.RawHTML:
		return styles.text.Render(string(bytesOrEmpty(node.Text(source)))), nil
	default:
		if node.FirstChild() != nil {
			return r.renderInlineChildren(node, source, styles)
		}
		return "", nil
	}
}

func (r *ANSI) plainInlineChildren(parent gast.Node, source []byte, decode bool) string {
	var b strings.Builder
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		b.WriteString(r.plainInline(child, source, decode))
	}
	return b.String()
}

func (r *ANSI) plainInline(node gast.Node, source []byte, decode bool) string {
	switch n := node.(type) {
	case *gast.Text:
		text := string(n.Value(source))
		if decode {
			text = decodeInlineText(text)
		}
		if n.HardLineBreak() {
			return text + "\n"
		}
		if n.SoftLineBreak() {
			return text + " "
		}
		return text
	case *gast.String:
		return string(n.Value)
	case *gast.CodeSpan:
		return r.plainInlineChildren(n, source, false)
	case *gast.Link:
		label := strings.TrimSpace(r.plainInlineChildren(n, source, decode))
		url := string(n.Destination)
		if label == "" {
			return url
		}
		if label == url {
			return url
		}
		return label + " " + url
	case *gast.AutoLink:
		return string(n.URL(source))
	case *gast.Image:
		alt := strings.TrimSpace(r.plainInlineChildren(n, source, decode))
		if alt == "" {
			alt = "Image"
		}
		url := string(n.Destination)
		if url == "" {
			return alt
		}
		return fmt.Sprintf("Image: %s → %s", alt, url)
	case *extast.TaskCheckBox:
		if n.IsChecked {
			return "[✓] "
		}
		return "[ ] "
	default:
		if node.FirstChild() != nil {
			return r.plainInlineChildren(node, source, decode)
		}
		if text := bytesOrEmpty(node.Text(source)); len(text) > 0 {
			return string(text)
		}
		return ""
	}
}

// wrapCellLines wraps text to fit within width using word-wrap, with a
// hard-break fallback for words wider than the column.
func wrapCellLines(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	if visibleWidth(text) <= width {
		return []string{text}
	}
	wrapped := wrapANSI(text, width)
	var result []string
	for _, line := range strings.Split(wrapped, "\n") {
		if visibleWidth(line) <= width {
			result = append(result, line)
			continue
		}
		// Hard break: strip ANSI and split by visible width.
		plain := stripANSI(line)
		runes := []rune(plain)
		for len(runes) > 0 {
			w := 0
			pos := 0
			for pos < len(runes) {
				rw := xansi.StringWidth(string(runes[pos : pos+1]))
				if w+rw > width {
					break
				}
				w += rw
				pos++
			}
			if pos == 0 {
				pos = 1
			}
			result = append(result, string(runes[:pos]))
			runes = runes[pos:]
		}
	}
	return result
}

func wrapANSI(text string, width int) string {
	if text == "" {
		return ""
	}
	if width < 1 {
		width = 1
	}
	return wordwrap.String(text, width)
}

func prefixLinesWithStyle(text, firstPrefix, otherPrefix string, style ansiStyle) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		prefix := otherPrefix
		if i == 0 {
			prefix = firstPrefix
		}
		if line == "" {
			lines[i] = prefix
			continue
		}
		lines[i] = prefix + style.Render(line)
	}
	return strings.Join(lines, "\n")
}

func prefixExistingLines(text, firstPrefix, otherPrefix string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = firstPrefix + line
		} else {
			lines[i] = otherPrefix + line
		}
	}
	return strings.Join(lines, "\n")
}

func fitColumnWidths(widths []int, totalWidth, colCount int) {
	if colCount == 0 {
		return
	}
	if totalWidth < colCount {
		for i := range widths {
			widths[i] = 1
		}
		return
	}
	separatorWidth := 3 * (colCount - 1)
	available := totalWidth - separatorWidth
	if available < colCount {
		available = colCount
	}
	total := 0
	for i := range widths {
		if widths[i] < 1 {
			widths[i] = 1
		}
		total += widths[i]
	}
	if total <= available {
		return
	}
	// Fair-share allocation: start with equal shares, then release unused
	// space from columns that fit within their share.
	natural := make([]int, len(widths))
	copy(natural, widths)
	settled := make([]bool, len(widths))

	remaining := available
	unsettledCount := len(widths)

	for unsettledCount > 0 {
		share := remaining / unsettledCount
		if share < 1 {
			share = 1
		}
		changed := false
		for i := range widths {
			if settled[i] {
				continue
			}
			if natural[i] <= share {
				widths[i] = natural[i]
				remaining -= natural[i]
				unsettledCount--
				settled[i] = true
				changed = true
			}
		}
		if !changed {
			// No columns settled — distribute remaining evenly among unsettled.
			share = remaining / unsettledCount
			extra := remaining - share*unsettledCount
			for i := range widths {
				if settled[i] {
					continue
				}
				widths[i] = share
				if extra > 0 {
					widths[i]++
					extra--
				}
			}
			break
		}
	}
	// Distribute any leftover space to the widest column(s).
	allocated := 0
	for _, w := range widths {
		allocated += w
	}
	for allocated < available {
		bestIdx := 0
		for i := 1; i < len(widths); i++ {
			if widths[i] > widths[bestIdx] {
				bestIdx = i
			}
		}
		widths[bestIdx]++
		allocated++
	}
}

func alignmentAt(aligns []extast.Alignment, i int) extast.Alignment {
	if i < len(aligns) {
		return aligns[i]
	}
	return extast.AlignNone
}

func padCell(text string, width int, align extast.Alignment) string {
	if width < 1 {
		width = 1
	}
	visible := visibleWidth(text)
	if visible >= width {
		return text
	}
	padding := width - visible
	switch align {
	case extast.AlignRight:
		return strings.Repeat(" ", padding) + text
	case extast.AlignCenter:
		left := padding / 2
		right := padding - left
		return strings.Repeat(" ", left) + text + strings.Repeat(" ", right)
	default:
		return text + strings.Repeat(" ", padding)
	}
}

func visibleWidth(text string) int {
	return xansi.StringWidth(text)
}

var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(text string) string {
	return ansiEscapeRe.ReplaceAllString(text, "")
}

func bytesOrEmpty(v []byte) []byte {
	if v == nil {
		return []byte{}
	}
	return v
}

// multiNewlineRe matches 3 or more consecutive newlines.
var multiNewlineRe = regexp.MustCompile(`\n{3,}`)

func normalizeNewlines(s string) string {
	return multiNewlineRe.ReplaceAllString(s, "\n\n")
}

// backslashEscapeRe matches a backslash followed by an ASCII punctuation character.
var backslashEscapeRe = regexp.MustCompile(`\\([!"#$%&'()*+,\-./:;<=>?@\[\\\]^_{|}~` + "`])")

// decodeInlineText processes raw goldmark source text for terminal display:
// 1. Strips CommonMark backslash escapes (\* → *)
// 2. Decodes HTML entities (&amp; → &, &ouml; → ö)
func decodeInlineText(s string) string {
	s = backslashEscapeRe.ReplaceAllString(s, "$1")
	return html.UnescapeString(s)
}

type codeHighlighter struct {
	lexer chroma.Lexer
	style *chroma.Style
}

var codeHighlighterCache sync.Map // map[string]*codeHighlighter

func getCodeHighlighter(language string) *codeHighlighter {
	language = strings.TrimSpace(strings.ToLower(language))
	if language == "" {
		return nil
	}
	if cached, ok := codeHighlighterCache.Load(language); ok {
		if cached == nil {
			return nil
		}
		return cached.(*codeHighlighter)
	}

	lexer := lexers.Get(language)
	if lexer == nil {
		lexer = lexers.Match("file." + language)
	}
	if lexer == nil {
		codeHighlighterCache.Store(language, nil)
		return nil
	}
	lexer = chroma.Coalesce(lexer)
	style := chromastyles.Get("monokai")
	if style == nil {
		style = chromastyles.Fallback
	}
	h := &codeHighlighter{lexer: lexer, style: style}
	codeHighlighterCache.Store(language, h)
	return h
}

func (h *codeHighlighter) highlightLine(line string) string {
	if h == nil {
		return line
	}
	iterator, err := h.lexer.Tokenise(nil, line)
	if err != nil {
		return line
	}
	var b strings.Builder
	for token := iterator(); token != chroma.EOF; token = iterator() {
		value := strings.TrimRight(token.Value, "\n")
		if value == "" {
			continue
		}
		entry := h.style.Get(token.Type)
		var codes []string
		if entry.Colour.IsSet() {
			codes = append(codes, fmt.Sprintf("38;2;%d;%d;%d", entry.Colour.Red(), entry.Colour.Green(), entry.Colour.Blue()))
		}
		if entry.Bold == chroma.Yes {
			codes = append(codes, "1")
		}
		if entry.Italic == chroma.Yes {
			codes = append(codes, "3")
		}
		if entry.Underline == chroma.Yes {
			codes = append(codes, "4")
		}
		if len(codes) > 0 {
			fmt.Fprintf(&b, "\x1b[%sm%s\x1b[0m", strings.Join(codes, ";"), value)
		} else {
			b.WriteString(value)
		}
	}
	return b.String()
}
