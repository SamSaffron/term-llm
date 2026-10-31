package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/ui"
)

func TestFooterNoticeFlattensShortMultilineMessages(t *testing.T) {
	for _, tt := range []struct {
		content string
		want    string
		ok      bool
	}{
		{"Unknown command: /x\nType /help for available commands.", "Unknown command: /x · Type /help for available commands.", true},
		{"No approved directories.\n\nUse `/dirs add <path>` to approve a directory,\nor attach a file to be prompted for approval.",
			"No approved directories. · Use /dirs add <path> to approve a directory, or attach a file to be prompted for approval.", true},
		{"Attached 2 files (3 KB):\n- a.go\n- b.go", "Attached 2 files (3 KB): a.go · b.go", true},
		{"## Approved Directories\n\n- `/tmp`", "", false},
		{"one\ntwo\nthree\nfour\nfive", "", false},
		{"Usage:\n" + strings.Repeat("long words ", 30), "", false},
	} {
		got, ok := footerNotice(tt.content)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("footerNotice(%q) = %q, %v; want %q, %v", tt.content, got, ok, tt.want, tt.ok)
		}
	}
}

// tea.Println output is dropped on the alternate screen, so multi-line command
// output must reach the footer or a content dialog instead.
func TestMultilineSystemMessagesAreVisibleOnAltScreen(t *testing.T) {
	m := newTestChatModel(true)
	m.approvedDirs = &ApprovedDirs{}
	updated, _ := m.ExecuteCommand("/dirs")
	m = updated.(*Model)
	if m.dialog.IsOpen() || !strings.Contains(m.footerMessage, "No approved directories") || !strings.Contains(m.footerMessage, "/dirs add <path>") {
		t.Fatalf("short notice: dialog=%v footer=%q", m.dialog.IsOpen(), m.footerMessage)
	}

	m.approvedDirs = &ApprovedDirs{Directories: []string{"/srv/project"}}
	updated, _ = m.ExecuteCommand("/dirs")
	m = updated.(*Model)
	if m.dialog.Type() != DialogContent || m.dialog.title != "Approved Directories" {
		t.Fatalf("listing dialog = %v %q", m.dialog.Type(), m.dialog.title)
	}
	if content := ui.StripANSI(m.dialog.Content()); !strings.Contains(content, "/srv/project") || !strings.Contains(content, "/dirs remove <path>") {
		t.Fatalf("listing dialog content = %q", content)
	}

	// Inline mode keeps printing above the program.
	inline := newTestChatModel(false)
	inline.approvedDirs = &ApprovedDirs{Directories: []string{"/srv/project"}}
	updated, cmd := inline.ExecuteCommand("/dirs")
	if inline = updated.(*Model); cmd == nil || inline.dialog.IsOpen() {
		t.Fatalf("inline listing: cmd=%v dialog=%v", cmd != nil, inline.dialog.IsOpen())
	}
}
