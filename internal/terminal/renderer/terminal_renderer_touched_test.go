package uv

import (
	"fmt"
	"io"
	"testing"
)

// TestRenderReusesTouchedLineData pins the steady-state per-frame allocation
// cost of [TerminalRenderer.Render]. Render used to allocate a fresh
// [LineData] for every line of both the new and retained buffers on every
// frame, which dominated the garbage produced by a TUI redrawing at frame
// rate. The records belong exclusively to their slot, so they are reused.
func TestRenderReusesTouchedLineData(t *testing.T) {
	const width, height = 80, 24

	r := NewTerminalRenderer(io.Discard, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	r.Resize(width, height)

	lines := make([]string, height)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %02d holds unchanging content", i)
	}
	buf := NewRenderBuffer(width, height)
	setTestFrame(buf, lines, width, height)
	r.Render(buf)
	if err := r.Flush(); err != nil {
		t.Fatalf("initial flush: %v", err)
	}

	allocs := testing.AllocsPerRun(200, func() {
		buf.TouchLine(0, height/2, width-1)
		r.Render(buf)
		if err := r.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
	})

	// One record per line of each buffer would be 2*height allocations before
	// any real render work. Allow generous headroom for the write path while
	// still failing if per-line records return.
	if limit := float64(height) / 2; allocs > limit {
		t.Fatalf("Render allocated %.1f objects/frame, want <= %.1f", allocs, limit)
	}
}

// TestRenderTouchedSlotsAreDistinct guards the reuse above: every retained
// record must stay owned by exactly one line of one buffer. [RenderBuffer.TouchLine]
// mutates records in place, so a shared record would smear one line's damage
// across others and corrupt the diff.
func TestRenderTouchedSlotsAreDistinct(t *testing.T) {
	const width, height = 40, 8

	r := NewTerminalRenderer(io.Discard, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	r.Resize(width, height)

	buf := NewRenderBuffer(width, height)
	lines := make([]string, height)
	for frame := range 4 {
		for i := range lines {
			lines[i] = fmt.Sprintf("frame %d row %d", frame, i)
		}
		setTestFrame(buf, lines, width, height)
		r.Render(buf)
		if err := r.Flush(); err != nil {
			t.Fatalf("frame %d flush: %v", frame, err)
		}

		seen := make(map[*LineData]string, len(buf.Touched)+len(r.curbuf.Touched))
		for i, d := range buf.Touched {
			if d == nil {
				continue
			}
			if where, ok := seen[d]; ok {
				t.Fatalf("frame %d: new buffer line %d shares a record with %s", frame, i, where)
			}
			seen[d] = fmt.Sprintf("new buffer line %d", i)
		}
		for i, d := range r.curbuf.Touched {
			if d == nil {
				continue
			}
			if where, ok := seen[d]; ok {
				t.Fatalf("frame %d: retained buffer line %d shares a record with %s", frame, i, where)
			}
			seen[d] = fmt.Sprintf("retained buffer line %d", i)
		}
	}
}

// TestRenderResetsTouchedRecords asserts the post-render state every caller
// depends on: each line of both buffers carries a clean record, exactly as a
// freshly allocated one would.
func TestRenderResetsTouchedRecords(t *testing.T) {
	const width, height = 40, 6

	r := NewTerminalRenderer(io.Discard, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	r.Resize(width, height)

	buf := NewRenderBuffer(width, height)
	lines := make([]string, height)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	setTestFrame(buf, lines, width, height)
	buf.TouchLine(0, 2, width-1)
	r.Render(buf)
	if err := r.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if len(buf.Touched) != height {
		t.Fatalf("new buffer has %d touched records, want %d", len(buf.Touched), height)
	}
	for i, d := range buf.Touched {
		if d == nil {
			t.Fatalf("new buffer line %d has no record after render", i)
		}
		if *d != (LineData{FirstCell: -1, LastCell: -1}) {
			t.Fatalf("new buffer line %d record = %+v, want a clean record", i, *d)
		}
	}
	for i, d := range r.curbuf.Touched {
		if d == nil {
			t.Fatalf("retained buffer line %d has no record after render", i)
		}
		if *d != (LineData{FirstCell: -1, LastCell: -1}) {
			t.Fatalf("retained buffer line %d record = %+v, want a clean record", i, *d)
		}
	}
	if got := buf.TouchedLines(); got != height {
		t.Fatalf("TouchedLines() = %d, want %d", got, height)
	}
}
