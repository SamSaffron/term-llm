package chat

import (
	"net/url"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// terminalWorkingDirectoryCmd reports the session's effective directory using
// OSC 7. Terminal emulators use this shell-integration sequence to update their
// working-directory metadata even though term-llm deliberately keeps its
// process working directory unchanged.
func terminalWorkingDirectoryCmd(dir string) tea.Cmd {
	sequence := terminalWorkingDirectorySequence(dir)
	if sequence == "" {
		return nil
	}
	return tea.Raw(sequence)
}

func terminalWorkingDirectorySequence(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}

	path := filepath.ToSlash(filepath.Clean(dir))
	// Windows drive paths need a leading slash in a file URI.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	return "\x1b]7;" + uri + "\x1b\\"
}

func (m *Model) takeTerminalWorkingDirectoryCmd() tea.Cmd {
	if m == nil {
		return nil
	}
	dir := m.pendingTerminalDirectory
	m.pendingTerminalDirectory = ""
	return terminalWorkingDirectoryCmd(dir)
}
