package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/runtimeoutput"
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

func TestTUIWarningWriterDeduplicatesOnlyWhileAttached(t *testing.T) {
	var fallback bytes.Buffer
	writer := newTUIWarningWriter(&fallback)
	var notices []string
	writer.attachNotifier(func(text string) { notices = append(notices, text) })
	for _, text := range []string{"disk full\n", "disk full\n", "save failed\n", "disk full\n"} {
		if _, err := writer.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(notices, "|"); got != "disk full|save failed|disk full" {
		t.Fatalf("notices = %q", got)
	}
	if fallback.Len() != 0 {
		t.Fatalf("stderr fallback during UI: %q", fallback.String())
	}
	writer.detach()
	writer.attachNotifier(func(text string) { notices = append(notices, text) })
	_, _ = writer.Write([]byte("disk full\n"))
	if len(notices) != 4 {
		t.Fatalf("notices after reattach = %v", notices)
	}
}

func TestChatSessionWarningsAcrossSwitchDetachAndClose(t *testing.T) {
	var command bytes.Buffer
	first, finishFirst := newChatSessionWarningWriter(&command, nil)
	finishFirst()
	var notices []string
	first.attachNotifier(func(text string) { notices = append(notices, "first: "+text) })

	dir := t.TempDir()
	closeLog, err := runtimeoutput.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()

	second, finishSecond := newChatSessionWarningWriter(&command, first)
	_, _ = second.Write([]byte("building second\n"))
	finishSecond()
	first.detach()
	second.attachNotifier(func(text string) { notices = append(notices, "second: "+text) })

	third, finishThird := newChatSessionWarningWriter(&command, second)
	_, _ = third.Write([]byte("building third\n"))
	finishThird()
	second.detach()
	third.attachNotifier(func(text string) { notices = append(notices, "third: "+text) })

	// Both retired runtimes may still have adopted work while a new UI owns
	// the terminal. Their warnings belong in the log, not the visible footer.
	_, _ = first.Write([]byte("old first run\n"))
	_, _ = second.Write([]byte("old second run\n"))
	_, _ = third.Write([]byte("visible third run\n"))
	if got := strings.Join(notices, "|"); got != "first: building second|second: building third|third: visible third run" {
		t.Fatalf("notices: %q", got)
	}
	third.detach()
	closeLog()
	for _, writer := range []*tuiWarningWriter{first, second, third} {
		if _, err := writer.Write([]byte("after close\n")); err != nil {
			t.Fatal(err)
		}
	}
	if got := command.String(); got != "after close\nafter close\nafter close\n" {
		t.Fatalf("durable command fallback: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tui.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "old first run") || !strings.Contains(got, "old second run") || strings.Contains(got, "after close") {
		t.Fatalf("detached warnings log: %q", got)
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
