package ui

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestTextInputRendering(t *testing.T) {
	// Test without renderer (default behavior - no colors in non-TTY)
	t.Run("without_renderer", func(t *testing.T) {
		textColor := lipgloss.Color("15")
		mutedColor := lipgloss.Color("245")

		ti := textinput.New()
		ti.Placeholder = "Test placeholder"
		ti.CharLimit = 500
		ti.Width = 50
		ti.Prompt = ""
		ti.PromptStyle = lipgloss.NewStyle()
		ti.TextStyle = lipgloss.NewStyle().Foreground(textColor)
		ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(mutedColor)
		ti.Cursor.Style = lipgloss.NewStyle().Foreground(textColor)
		ti.Cursor.SetMode(cursor.CursorBlink)

		ti.Focus()
		view := ti.View()
		plain := testutil.StripANSI(view)

		t.Logf("Without renderer - raw: %q", view)
		t.Logf("Without renderer - plain: %q", plain)

		// Without a TTY-bound renderer, raw and plain should be same (no ANSI codes)
		// This verifies the issue we're fixing
		if view != plain {
			t.Logf("Colors ARE being applied without renderer (unexpected in non-TTY)")
		} else {
			t.Logf("Colors NOT being applied without renderer (expected in non-TTY)")
		}
	})

	// Test with TTY-bound renderer (simulated)
	t.Run("with_tty_renderer", func(t *testing.T) {
		// Open /dev/tty to simulate actual terminal
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			t.Skip("No TTY available for testing")
		}
		defer tty.Close()

		renderer := lipgloss.NewRenderer(tty)
		textColor := lipgloss.Color("15")
		mutedColor := lipgloss.Color("245")

		ti := textinput.New()
		ti.Placeholder = "Test placeholder"
		ti.CharLimit = 500
		ti.Width = 50
		ti.Prompt = ""
		ti.PromptStyle = renderer.NewStyle()
		ti.TextStyle = renderer.NewStyle().Foreground(textColor)
		ti.PlaceholderStyle = renderer.NewStyle().Foreground(mutedColor)
		ti.Cursor.Style = renderer.NewStyle().Foreground(textColor)
		ti.Cursor.SetMode(cursor.CursorBlink)

		ti.Focus()
		view := ti.View()
		plain := testutil.StripANSI(view)

		t.Logf("With TTY renderer - raw: %q", view)
		t.Logf("With TTY renderer - plain: %q", plain)

		// With TTY-bound renderer, raw should have ANSI codes
		if view != plain {
			t.Logf("Colors ARE being applied with TTY renderer (expected)")
		} else {
			t.Errorf("Colors NOT being applied with TTY renderer - this is the bug!")
		}

		// Check for ANSI escape sequences
		if !strings.Contains(view, "\x1b[") {
			t.Errorf("Expected ANSI codes in output with TTY renderer, got: %q", view)
		}
	})
}
