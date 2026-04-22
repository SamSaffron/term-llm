package tuiutil

import (
	"testing"

	"charm.land/lipgloss/v2"
)

func TestAccentPanelStyleMatchesLegacyStyle(t *testing.T) {
	accent := lipgloss.Color("10")
	legacy := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderLeft(true).
		BorderForeground(accent).
		PaddingLeft(1).
		PaddingRight(2).
		PaddingTop(1).
		PaddingBottom(1)

	got := AccentPanelStyle(accent).Render("body")
	want := legacy.Render("body")
	if got != want {
		t.Fatalf("AccentPanelStyle render mismatch\nwant:\n%q\n\ngot:\n%q", want, got)
	}
}

func TestCompactAccentPanelStyleMatchesLegacyStyle(t *testing.T) {
	accent := lipgloss.Color("10")
	legacy := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderLeft(true).
		BorderForeground(accent).
		PaddingLeft(1).
		PaddingRight(2)

	got := CompactAccentPanelStyle(accent).Render("body")
	want := legacy.Render("body")
	if got != want {
		t.Fatalf("CompactAccentPanelStyle render mismatch\nwant:\n%q\n\ngot:\n%q", want, got)
	}
}

func TestAccentTitleStyleMatchesLegacyStyle(t *testing.T) {
	accent := lipgloss.Color("208")
	legacy := lipgloss.NewStyle().
		Foreground(accent).
		Bold(true).
		MarginBottom(1)

	got := AccentTitleStyle(accent).Render("Title")
	want := legacy.Render("Title")
	if got != want {
		t.Fatalf("AccentTitleStyle render mismatch\nwant:\n%q\n\ngot:\n%q", want, got)
	}
}

func TestMutedHelpStyleMatchesLegacyStyle(t *testing.T) {
	muted := lipgloss.Color("245")
	legacy := lipgloss.NewStyle().
		Foreground(muted).
		MarginTop(1)

	got := MutedHelpStyle(muted).Render("help")
	want := legacy.Render("help")
	if got != want {
		t.Fatalf("MutedHelpStyle render mismatch\nwant:\n%q\n\ngot:\n%q", want, got)
	}
}

func TestNumberedOptionPrefix(t *testing.T) {
	if got := NumberedOptionPrefix(0, true); got != "> 1. " {
		t.Fatalf("selected prefix = %q", got)
	}
	if got := NumberedOptionPrefix(2, false); got != "  3. " {
		t.Fatalf("unselected prefix = %q", got)
	}
}

func TestQuickSelectHelpText(t *testing.T) {
	if got := QuickSelectHelpText(2); got != "↑↓ select  1-2 quick  enter confirm  esc cancel" {
		t.Fatalf("QuickSelectHelpText(2) = %q", got)
	}
	if got := QuickSelectHelpText(5); got != "↑↓ select  1-5 quick  enter confirm  esc cancel" {
		t.Fatalf("QuickSelectHelpText(5) = %q", got)
	}
}
