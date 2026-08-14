package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestTUIWarningWriterRoutesThroughProgramWhileRendering pins the rule that a
// background warning must not reach stderr while Bubble Tea owns the alt
// screen. A direct write lands wherever the cursor sits — in practice across
// the prompt line — and stays there until the next full repaint.
func TestTUIWarningWriterRoutesThroughProgramWhileRendering(t *testing.T) {
	var fallback bytes.Buffer
	writer := newTUIWarningWriter(&fallback)

	if _, err := writer.Write([]byte("warning: before the TUI starts\n")); err != nil {
		t.Fatalf("write before attach: %v", err)
	}
	if got := fallback.String(); !strings.Contains(got, "before the TUI starts") {
		t.Fatalf("pre-attach fallback = %q, want the startup warning", got)
	}

	var notices []string
	writer.attachNotifier(func(text string) { notices = append(notices, text) })

	fallback.Reset()
	if _, err := writer.Write([]byte("warning: session AddMessage failed: boom\n")); err != nil {
		t.Fatalf("write while attached: %v", err)
	}
	if fallback.Len() != 0 {
		t.Fatalf("warning reached the fallback writer while rendering: %q", fallback.String())
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "session AddMessage failed") {
		t.Fatalf("notices = %v, want the store warning routed to the TUI", notices)
	}
	if strings.ContainsAny(notices[0], "\n\r") {
		t.Fatalf("notice %q kept line breaks; footer notices are single-line", notices[0])
	}

	writer.detach()
	if _, err := writer.Write([]byte("warning: after the TUI exits\n")); err != nil {
		t.Fatalf("write after detach: %v", err)
	}
	if got := fallback.String(); !strings.Contains(got, "after the TUI exits") {
		t.Fatalf("post-detach fallback = %q, want shutdown warnings visible again", got)
	}
	if len(notices) != 1 {
		t.Fatalf("notices after detach = %v, want no further sends", notices)
	}
}

// TestTUIWarningWriterDropsBlankWrites keeps padding writes from flashing an
// empty footer notice.
func TestTUIWarningWriterDropsBlankWrites(t *testing.T) {
	writer := newTUIWarningWriter(nil)
	var notices []string
	writer.attachNotifier(func(text string) { notices = append(notices, text) })

	n, err := writer.Write([]byte("  \n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 3 {
		t.Fatalf("n = %d, want the full write consumed", n)
	}
	if len(notices) != 0 {
		t.Fatalf("notices = %v, want none for a blank write", notices)
	}
}
