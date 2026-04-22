package ui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestTextInputRendering(t *testing.T) {
	textColor := lipgloss.Color("15")
	mutedColor := lipgloss.Color("245")

	ti := textinput.New()
	ti.Placeholder = "Test placeholder"
	ti.CharLimit = 500
	ti.SetWidth(50)
	ti.Prompt = ""

	styles := textinput.DefaultStyles(false)
	styles.Focused.Prompt = lipgloss.NewStyle()
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	styles.Blurred = styles.Focused
	ti.SetStyles(styles)

	ti.Focus()
	view := ti.View()
	plain := testutil.StripANSI(view)

	t.Logf("raw: %q", view)
	t.Logf("plain: %q", plain)

	if !strings.Contains(plain, "Test placeholder") {
		t.Errorf("expected placeholder text in plain output, got: %q", plain)
	}
}
