package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestTerminalWorkingDirectorySequence(t *testing.T) {
	dir := filepath.Join(string(filepath.Separator), "tmp", "term llm", "worktree#1")
	got := terminalWorkingDirectorySequence(dir)
	want := "\x1b]7;file:///tmp/term%20llm/worktree%231\x1b\\"
	if got != want {
		t.Fatalf("terminalWorkingDirectorySequence() = %q, want %q", got, want)
	}

	for _, invalid := range []string{"", "relative/path"} {
		if got := terminalWorkingDirectorySequence(invalid); got != "" {
			t.Errorf("terminalWorkingDirectorySequence(%q) = %q, want empty", invalid, got)
		}
	}
}

func TestPendingWorkingDirectoryReportIsEmittedAfterUpdate(t *testing.T) {
	m := newTestChatModel(false)
	dir := filepath.Join(string(filepath.Separator), "tmp", "switched-worktree")
	m.pendingTerminalDirectory = dir

	_, cmd := m.Update(struct{}{})
	raw := strings.Join(rawStringsFromCmd(cmd), "")
	want := terminalWorkingDirectorySequence(dir)
	if !strings.Contains(raw, want) {
		t.Fatalf("Update raw output = %q, want OSC 7 report %q", raw, want)
	}
	if m.pendingTerminalDirectory != "" {
		t.Fatalf("pending terminal directory was not cleared: %q", m.pendingTerminalDirectory)
	}
}

func TestNewSessionReportsProcessWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		run  func(*Model)
	}{
		{name: "clear", run: func(m *Model) { _, _ = m.cmdClear() }},
		{name: "new", run: func(m *Model) { _, _ = m.cmdNew() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{CWD: "/stale/worktree", WorktreeDir: "/stale/worktree"}
			tt.run(m)
			if m.pendingTerminalDirectory != cwd {
				t.Fatalf("pending terminal directory = %q, want process cwd %q", m.pendingTerminalDirectory, cwd)
			}
		})
	}
}

func TestInitReportsResumedSessionWorkingDirectory(t *testing.T) {
	m := newTestChatModel(false)
	dir := filepath.Join(string(filepath.Separator), "tmp", "resumed worktree")
	m.sess = &session.Session{CWD: dir, WorktreeDir: dir}

	raw := strings.Join(rawStringsFromCmd(m.Init()), "")
	want := terminalWorkingDirectorySequence(dir)
	if !strings.Contains(raw, want) {
		t.Fatalf("Init raw output = %q, want OSC 7 report %q", raw, want)
	}
}
