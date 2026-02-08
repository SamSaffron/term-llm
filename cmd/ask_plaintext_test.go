package cmd

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/ui"
)

func captureStreamPlainTextOutput(t *testing.T, events []ui.StreamEvent) string {
	t.Helper()

	ch := make(chan ui.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	os.Stdout = w

	err = streamPlainText(context.Background(), ch)
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("streamPlainText returned error: %v", err)
	}

	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("read stdout: %v", readErr)
	}
	return string(out)
}

func countTrailingNewlines(s string) int {
	count := 0
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '\n' {
			break
		}
		count++
	}
	return count
}

func TestStreamPlainText_DoesNotInsertExtraBlankLineBeforeToolWhenTextAlreadyHasBlankLine(t *testing.T) {
	output := captureStreamPlainTextOutput(t, []ui.StreamEvent{
		ui.TextEvent("Now let me write the test.\n\n"),
		ui.ToolStartEvent("call-1", "shell", "(cd /var/www/discourse && sed -n '93,95p' plugins/discourse-ai/spec/requests/ai_helper/assistant_controller_spec.rb)"),
		ui.ToolEndEvent("call-1", "shell", "(cd /var/www/discourse && sed -n '93,95p' plugins/discourse-ai/spec/requests/ai_helper/assistant_controller_spec.rb)", true),
		ui.DoneEvent(0),
	})

	plain := stripAnsi(output)
	toolIdx := strings.Index(plain, "● shell(cd /var/www/discourse")
	if toolIdx == -1 {
		t.Fatalf("expected shell tool line in output, got: %q", plain)
	}

	beforeTool := plain[:toolIdx]
	if got := countTrailingNewlines(beforeTool); got != 2 {
		t.Fatalf("expected exactly 2 trailing newlines before tool block, got %d\noutput: %q", got, plain)
	}

	if got := countTrailingNewlines(plain); got != 2 {
		t.Fatalf("expected exactly 2 trailing newlines at end of output, got %d\noutput: %q", got, plain)
	}
}

func TestStreamPlainText_TextToToolUsesSingleNewlineWhenTextHasNoTrailingNewline(t *testing.T) {
	output := captureStreamPlainTextOutput(t, []ui.StreamEvent{
		ui.TextEvent("Now let me write the test."),
		ui.ToolStartEvent("call-1", "shell", "(echo hi)"),
		ui.ToolEndEvent("call-1", "shell", "(echo hi)", true),
		ui.DoneEvent(0),
	})

	plain := stripAnsi(output)
	boundary := "test.\n● shell(echo hi)"
	if !strings.Contains(plain, boundary) {
		t.Fatalf("expected single newline text->tool boundary %q, got %q", boundary, plain)
	}
}

func TestStreamPlainText_ToolToTextUsesBlankLine(t *testing.T) {
	output := captureStreamPlainTextOutput(t, []ui.StreamEvent{
		ui.ToolStartEvent("call-1", "shell", "(echo hi)"),
		ui.ToolEndEvent("call-1", "shell", "(echo hi)", true),
		ui.TextEvent("Recent updates."),
		ui.DoneEvent(0),
	})

	plain := stripAnsi(output)
	toolLabel := "shell(echo hi)"
	toolIdx := strings.Index(plain, toolLabel)
	if toolIdx == -1 {
		t.Fatalf("expected tool label %q in output, got %q", toolLabel, plain)
	}
	textIdx := strings.Index(plain, "Recent updates.")
	if textIdx == -1 {
		t.Fatalf("expected text after tool in output, got %q", plain)
	}
	if toolIdx >= textIdx {
		t.Fatalf("expected tool before text, tool index=%d text index=%d output=%q", toolIdx, textIdx, plain)
	}

	between := plain[toolIdx+len(toolLabel) : textIdx]
	if got := strings.Count(between, "\n"); got != 2 {
		t.Fatalf("expected exactly 2 newlines between tool and text (blank line), got %d; between=%q output=%q", got, between, plain)
	}
}

func TestStreamPlainText_ToolToTextTrimsLeadingTextBlankLines(t *testing.T) {
	output := captureStreamPlainTextOutput(t, []ui.StreamEvent{
		ui.ToolStartEvent("call-1", "shell", "(echo hi)"),
		ui.ToolEndEvent("call-1", "shell", "(echo hi)", true),
		ui.TextEvent("\n\nRecent updates."),
		ui.DoneEvent(0),
	})

	plain := stripAnsi(output)
	boundary := "shell(echo hi)\n\nRecent updates."
	if !strings.Contains(plain, boundary) {
		t.Fatalf("expected compact tool->text boundary %q, got %q", boundary, plain)
	}
}
