package skills

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	internalSkills "github.com/samsaffron/term-llm/internal/skills"
)

func TestUpdate_SpaceTogglesSelectedSkill(t *testing.T) {
	m := &AddModel{
		phase: PhaseSelectSkills,
		discoveredSkills: []internalSkills.DiscoveredSkill{
			{Name: "first"},
			{Name: "second"},
		},
		skillCursor:   1,
		skillSelected: []bool{true, true},
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	m = updated.(*AddModel)

	if m.skillSelected[1] {
		t.Fatal("expected space to toggle current skill off")
	}
}

func TestUpdate_SpaceTogglesInstallPath(t *testing.T) {
	m := &AddModel{
		phase:         PhaseSelectPaths,
		installCursor: 1,
		installPaths: []InstallPath{
			{Label: "global", Selected: true},
			{Label: "project", Selected: false},
		},
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	m = updated.(*AddModel)

	if !m.installPaths[1].Selected {
		t.Fatal("expected space to toggle current install path on")
	}
}

func TestView_SelectSkillsStaysWithinTerminalHeight(t *testing.T) {
	m := &AddModel{
		width:  80,
		height: 24,
		phase:  PhaseSelectSkills,
		repoRef: internalSkills.GitHubRepoRef{
			Owner: "acme",
			Repo:  "skills",
		},
	}
	for i := 0; i < 20; i++ {
		m.discoveredSkills = append(m.discoveredSkills, internalSkills.DiscoveredSkill{
			Name:        "skill",
			Description: "description",
			FileCount:   1,
		})
		m.skillSelected = append(m.skillSelected, true)
	}

	view := m.View().Content
	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("select-skills view height = %d, want <= %d", got, m.height)
	}
}

func TestView_SelectPathsStaysWithinTerminalHeight(t *testing.T) {
	m := &AddModel{
		width:  80,
		height: 24,
		phase:  PhaseSelectPaths,
	}
	for i := 0; i < 20; i++ {
		m.installPaths = append(m.installPaths, InstallPath{Path: "/tmp/path", Label: "label"})
	}

	view := m.View().Content
	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("select-paths view height = %d, want <= %d", got, m.height)
	}
}
