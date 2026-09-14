package uv

import (
	"bytes"
	"io"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestRenderLineFlushesTrailingEmptyCells covers the renderLine trailing cell
// flush (upstream d9e819d). renderLine buffers plain spaces for empty cells
// instead of writing them immediately; if the buffer is never flushed when the
// line ends in empty cells, those trailing spaces are dropped and the terminal
// keeps whatever content was there before.
//
// [Line.String] deliberately strips trailing spaces, so the assertions go
// through [Line.Render].
func TestRenderLineFlushesTrailingEmptyCells(t *testing.T) {
	tests := []struct {
		name string
		line Line
		want string
	}{
		{
			name: "trailing empty cells after text",
			line: Line{
				{Content: "a", Width: 1},
				EmptyCell,
				EmptyCell,
			},
			want: "a  ",
		},
		{
			name: "line of only empty cells",
			line: Line{EmptyCell, EmptyCell, EmptyCell},
			want: "   ",
		},
		{
			name: "trailing empty cells after a styled cell",
			line: Line{
				{Content: "a", Width: 1, Style: Style{Attrs: AttrBold}},
				EmptyCell,
				EmptyCell,
			},
			want: (&Style{Attrs: AttrBold}).String() + "a" + ansi.ResetStyle + "  ",
		},
		{
			name: "trailing empty cell after a hyperlink",
			line: Line{
				{Content: "a", Width: 1, Link: Link{URL: "https://example.com"}},
				EmptyCell,
			},
			want: ansi.SetHyperlink("https://example.com") + "a" + ansi.ResetHyperlink() + " ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.line.Render()
			if got != tt.want {
				t.Errorf("Line.Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRelativeCursorMoveOverwriteSkipsZeroWidthCells covers the phantom space
// in the overwrite optimization of [relativeCursorMove] (upstream 9b00276).
//
// The overwrite path moves the cursor to the target column by re-writing the
// cells that already sit under the path. A column that holds no cell - a nil
// cell past the buffer edge or a zero-width placeholder belonging to the wide
// cell to its left - has nothing to rewrite, so it must not contribute a
// character to the sequence: a fabricated space paints over a column that was
// never written (splitting the wide glyph next to the placeholder) and shifts
// the rest of the overwrite.
func TestRelativeCursorMoveOverwriteSkipsZeroWidthCells(t *testing.T) {
	tests := []struct {
		name  string
		cells []Cell // cells at columns 0..len(cells)-1 of the rendered line
		fx    int    // current cursor column
		tx    int    // target cursor column
		want  string // only the real cells under [fx, tx) may be written
	}{
		{
			name:  "zero-width placeholder at the start of the path",
			cells: []Cell{{Content: "ab", Width: 2}, {}, {Content: "b", Width: 1}},
			fx:    1,
			tx:    3,
			want:  "b",
		},
		{
			name:  "path covers only the zero-width placeholder",
			cells: []Cell{{Content: "ab", Width: 2}, {}, {Content: "b", Width: 1}},
			fx:    1,
			tx:    2,
			want:  "",
		},
		{
			name:  "cell past the end of the line",
			cells: []Cell{{Content: "b", Width: 1}},
			fx:    0,
			tx:    2,
			want:  "b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The line is exactly as wide as the cells above: the path may run
			// past its end, where [RenderBuffer.CellAt] reports a nil cell.
			newbuf := NewRenderBuffer(len(tt.cells), 1)
			for x := range tt.cells {
				cell := tt.cells[x]
				newbuf.SetCell(x, 0, &cell)
			}

			s := NewTerminalRenderer(io.Discard, []string{"TERM=xterm-256color", "COLORTERM=truecolor"})
			s.curbuf = NewRenderBuffer(newbuf.Width(), 1)
			s.cur = cursor{Cell: EmptyCell, Position: Pos(tt.fx, 0)}

			got := relativeCursorMove(s, newbuf, tt.fx, 0, tt.tx, 0, true, false, false)
			if got != tt.want {
				t.Errorf("relativeCursorMove(%d -> %d) = %q, want %q; a column with no cell must not emit a space",
					tt.fx, tt.tx, got, tt.want)
			}
		})
	}
}

// TestRendererFirstFrameHasNoPhantomSpace checks the same phantom space through
// the public render path.
//
// The renderer models its starting cursor as column -1, so the very first move
// to column 0 walks one column to the right over a cell that does not exist.
// Fabricating a space for that column emits an extra character, pushing the
// first frame's content one column to the right.
func TestRendererFirstFrameHasNoPhantomSpace(t *testing.T) {
	const w, h = 6, 2
	var buf bytes.Buffer
	r := NewTerminalRenderer(&buf, []string{"TERM=xterm-256color", "COLORTERM=truecolor"})
	r.SetFullscreen(true)
	r.SetRelativeCursor(true)
	r.Resize(w, h)

	scr := NewScreenBuffer(w, h)
	scr.SetCell(0, 0, &Cell{Content: "a", Width: 1})
	scr.SetCell(1, 0, &Cell{Content: "b", Width: 1})

	r.Render(scr.RenderBuffer)
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	const want = "\nab" // Down to the first row, then the content at column 0.
	if got := buf.String(); got != want {
		t.Errorf("first frame = %q, want %q; the move to column 0 must not paint a column that holds no cell", got, want)
	}
}
