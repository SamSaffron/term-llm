package tea

import (
	"bytes"
	"testing"
)

func TestCursedRendererRepaintsSameViewAfterClearScreen(t *testing.T) {
	const (
		content = "same view"
		width   = 20
		height  = 4
	)
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, width, height)
	view := NewView(content)

	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatalf("initial flush: %v", err)
	}

	// Control: the same view at the same geometry is dropped entirely by the
	// unchanged-view shortcut. If this wrote anything, the assertion after the
	// clear would pass for the wrong reason.
	start := len(output.snapshot())
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatalf("control flush: %v", err)
	}
	if got := len(output.snapshot()); got != start {
		t.Fatalf("unchanged view wrote %d writes before the clear", got-start)
	}

	r.clearScreen()
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatalf("flush after clear: %v", err)
	}

	repaint := bytes.Join(output.snapshot()[start:], nil)
	if len(repaint) == 0 {
		t.Fatal("clearScreen followed by the same view wrote no repaint")
	}
	if !bytes.Contains(repaint, []byte(content)) {
		t.Fatalf("clearScreen repaint did not redraw the view content: %q", repaint)
	}
}

// TestCursedRendererRepaintsSameViewAfterResize covers the resize half of the
// pendingErase contract: resize() erases the terminal, so the flush that
// follows must paint again even though neither the view nor its content
// changed. The unchanged-view shortcut is exercised first as a control, so a
// repaint after the resize can only be attributed to the erase.
func TestCursedRendererRepaintsSameViewAfterResize(t *testing.T) {
	const (
		content = "same view"
		width   = 20
		height  = 4
	)
	tests := []struct {
		name      string
		altScreen bool
		resizeW   int
		resizeH   int
		// retainedGeometryChanges marks the one resize that leaves the retained
		// frame a different shape: flush repaints through its geometry path
		// there even without pendingErase, so such a case cannot isolate the
		// erase flag. Every other case keeps the retained geometry and can only
		// repaint because the erase defeated the unchanged-view shortcut.
		retainedGeometryChanges bool
	}{
		{
			name:    "same size inline resize",
			resizeW: width,
			resizeH: height,
		},
		{
			name:    "height only inline resize",
			resizeW: width,
			resizeH: height + 4,
		},
		{
			name:      "same size alternate screen resize",
			altScreen: true,
			resizeW:   width,
			resizeH:   height,
		},
		{
			name:                    "height change alternate screen resize",
			altScreen:               true,
			resizeW:                 width,
			resizeH:                 height + 4,
			retainedGeometryChanges: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := &boundaryWriter{}
			r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, width, height)
			view := NewView(content)
			view.AltScreen = tt.altScreen

			r.render(view)
			if err := r.flush(false); err != nil {
				t.Fatalf("initial flush: %v", err)
			}

			// Control: the same view at the same geometry is dropped entirely
			// by the unchanged-view shortcut. If this wrote anything, the
			// assertion after the resize would pass for the wrong reason.
			start := len(output.snapshot())
			r.render(view)
			if err := r.flush(false); err != nil {
				t.Fatalf("control flush: %v", err)
			}
			if got := len(output.snapshot()); got != start {
				t.Fatalf("unchanged view wrote %d writes before the resize", got-start)
			}

			before := r.cellbuf.Bounds()
			r.resize(tt.resizeW, tt.resizeH)
			r.render(view)
			if err := r.flush(false); err != nil {
				t.Fatalf("flush after resize: %v", err)
			}

			if changed := r.cellbuf.Bounds() != before; changed != tt.retainedGeometryChanges {
				t.Fatalf("retained geometry %v -> %v (changed=%v), want changed=%v: the case does not match the path it claims to cover",
					before, r.cellbuf.Bounds(), changed, tt.retainedGeometryChanges)
			}

			repaint := bytes.Join(output.snapshot()[start:], nil)
			if len(repaint) == 0 {
				t.Fatal("resize followed by the same view wrote no repaint")
			}
			if !bytes.Contains(repaint, []byte(content)) {
				t.Fatalf("resize repaint did not redraw the view content: %q", repaint)
			}
		})
	}
}
