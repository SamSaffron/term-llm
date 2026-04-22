package skills

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	internalSkills "github.com/samsaffron/term-llm/internal/skills"
)

func TestUpdate_PasteMsgUpdatesBrowseFilterInput(t *testing.T) {
	m := New("", false)

	updated, cmd := m.Update(tea.PasteMsg{Content: "agents"})
	m = updated.(*Model)

	if got := m.input.Value(); got != "agents" {
		t.Fatalf("expected pasted browse filter input, got %q", got)
	}
	if got := m.filterText; got != "agents" {
		t.Fatalf("expected pasted browse filter text, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected debounce command after pasted browse filter input")
	}
}

func TestUpdate_PasteMsgUpdatesInstallNameInput(t *testing.T) {
	m := New("", false)
	m.viewMode = ViewInstall
	m.installName.Focus()

	updated, _ := m.Update(tea.PasteMsg{Content: "custom-skill"})
	m = updated.(*Model)

	if got := m.installName.Value(); got != "custom-skill" {
		t.Fatalf("expected pasted install name, got %q", got)
	}
}

func TestUpdate_SpaceTogglesInstallPathSelection(t *testing.T) {
	m := New("", false)
	m.filteredList = []internalSkills.RemoteSkill{{Name: "demo"}}
	m.enterInstallView(m.filteredList[0])
	m.installName.Blur()
	m.installCursor = 1
	m.installPaths[1].Selected = false

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	m = updated.(*Model)

	if !m.installPaths[1].Selected {
		t.Fatal("expected space to toggle install path selection on")
	}
}

func TestView_InstallStaysWithinTerminalHeight(t *testing.T) {
	m := New("", false)
	m.width = 80
	m.height = 24
	m.viewMode = ViewInstall
	m.filteredList = []internalSkills.RemoteSkill{{Name: "demo"}}
	m.cursor = 0
	m.installName.SetValue("demo")
	m.installPaths = nil
	for i := 0; i < 20; i++ {
		m.installPaths = append(m.installPaths, InstallPath{Path: fmt.Sprintf("/tmp/path-%d", i), Label: "label"})
	}

	view := m.View().Content
	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("install view height = %d, want <= %d", got, m.height)
	}
}

func TestView_DeleteStaysWithinTerminalHeight(t *testing.T) {
	m := New("", false)
	m.width = 80
	m.height = 24
	m.viewMode = ViewDelete
	m.deleteSkill = internalSkills.RemoteSkill{Name: "demo"}
	m.installed = map[string][]string{"demo": {}}
	for i := 0; i < 20; i++ {
		m.installed["demo"] = append(m.installed["demo"], fmt.Sprintf("/tmp/path-%d", i))
	}

	view := m.View().Content
	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("delete view height = %d, want <= %d", got, m.height)
	}
}
