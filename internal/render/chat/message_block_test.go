package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestMessageBlockRenderer_UserMessageBackground_UsesANSI256Fallback(t *testing.T) {
	prevProfile := lipgloss.ColorProfile()
	prevDarkBg := lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.ANSI256)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(prevProfile)
		lipgloss.SetHasDarkBackground(prevDarkBg)
	})

	renderer := NewMessageBlockRenderer(80, nil, false)
	msg := &session.Message{
		ID:          1,
		Role:        llm.RoleUser,
		TextContent: "contrast check",
	}

	rendered := renderer.renderUserMessage(msg)
	if !strings.Contains(rendered, "\x1b[48;5;235m") {
		t.Fatalf("expected ANSI-256 user message background 235, got %q", rendered)
	}
}

func TestMessageBlockRenderer_UserMessageBackground_UsesThemeColorForTrueColor(t *testing.T) {
	prevProfile := lipgloss.ColorProfile()
	prevDarkBg := lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(prevProfile)
		lipgloss.SetHasDarkBackground(prevDarkBg)
	})

	renderer := NewMessageBlockRenderer(80, nil, false)
	msg := &session.Message{
		ID:          1,
		Role:        llm.RoleUser,
		TextContent: "contrast check",
	}

	rendered := renderer.renderUserMessage(msg)
	if !strings.Contains(rendered, "\x1b[48;2;60;56;54m") {
		t.Fatalf("expected truecolor user message background #3c3836, got %q", rendered)
	}
}
