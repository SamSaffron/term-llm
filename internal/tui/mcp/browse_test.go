package mcp

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestUpdate_PasteMsgUpdatesFilterInput(t *testing.T) {
	m := New("")

	updated, cmd := m.Update(tea.PasteMsg{Content: "filesystem"})
	m = updated.(*Model)

	if got := m.input.Value(); got != "filesystem" {
		t.Fatalf("expected pasted filter input, got %q", got)
	}
	if got := m.filterText; got != "filesystem" {
		t.Fatalf("expected pasted filter text, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected debounce command after pasted filter input")
	}
}
