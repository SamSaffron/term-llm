package sessions

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestUpdate_PasteMsgUpdatesSearchInput(t *testing.T) {
	m := New(nil, 80, 24, nil)
	m.searching = true
	m.searchInput.Focus()

	updated, _ := m.Update(tea.PasteMsg{Content: "release notes"})
	m = updated.(*Model)

	if got := m.searchInput.Value(); got != "release notes" {
		t.Fatalf("expected pasted search query, got %q", got)
	}
}
