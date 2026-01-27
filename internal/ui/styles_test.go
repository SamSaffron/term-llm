package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestThemeFromConfig_Default(t *testing.T) {
	// Empty config should return default theme
	cfg := ThemeConfig{}
	theme := ThemeFromConfig(cfg)

	defaultTheme := DefaultTheme()
	if theme.Primary != defaultTheme.Primary {
		t.Errorf("expected Primary=%q, got %q", defaultTheme.Primary, theme.Primary)
	}
	if theme.UserMsgBg != defaultTheme.UserMsgBg {
		t.Errorf("expected UserMsgBg=%q, got %q", defaultTheme.UserMsgBg, theme.UserMsgBg)
	}
}

func TestThemeFromConfig_UserMsgBgOverride(t *testing.T) {
	// UserMsgBg should be overridable via config
	cfg := ThemeConfig{
		UserMsgBg: "#ff0000",
	}
	theme := ThemeFromConfig(cfg)

	expected := lipgloss.Color("#ff0000")
	if theme.UserMsgBg != expected {
		t.Errorf("expected UserMsgBg=%q, got %q", expected, theme.UserMsgBg)
	}
}

func TestThemeFromConfig_AllOverrides(t *testing.T) {
	cfg := ThemeConfig{
		Primary:   "#111111",
		Secondary: "#222222",
		Success:   "#333333",
		Error:     "#444444",
		Warning:   "#555555",
		Muted:     "#666666",
		Text:      "#777777",
		Spinner:   "#888888",
		UserMsgBg: "#999999",
	}
	theme := ThemeFromConfig(cfg)

	tests := []struct {
		name     string
		got      lipgloss.Color
		expected lipgloss.Color
	}{
		{"Primary", theme.Primary, lipgloss.Color("#111111")},
		{"Secondary", theme.Secondary, lipgloss.Color("#222222")},
		{"Success", theme.Success, lipgloss.Color("#333333")},
		{"Error", theme.Error, lipgloss.Color("#444444")},
		{"Warning", theme.Warning, lipgloss.Color("#555555")},
		{"Muted", theme.Muted, lipgloss.Color("#666666")},
		{"Text", theme.Text, lipgloss.Color("#777777")},
		{"Spinner", theme.Spinner, lipgloss.Color("#888888")},
		{"UserMsgBg", theme.UserMsgBg, lipgloss.Color("#999999")},
	}

	for _, tt := range tests {
		if tt.got != tt.expected {
			t.Errorf("%s: expected %q, got %q", tt.name, tt.expected, tt.got)
		}
	}
}

func TestThemeFromConfig_SecondarySetsBorder(t *testing.T) {
	// When Secondary is set, Border should also be set to the same value
	cfg := ThemeConfig{
		Secondary: "#abcdef",
	}
	theme := ThemeFromConfig(cfg)

	expected := lipgloss.Color("#abcdef")
	if theme.Border != expected {
		t.Errorf("expected Border to follow Secondary=%q, got %q", expected, theme.Border)
	}
}

func TestErrorCircle_ContainsRedColor(t *testing.T) {
	// ErrorCircle should return ANSI-styled red circle
	result := ErrorCircle()

	// Should contain the filled circle character
	if !strings.Contains(result, "●") {
		t.Error("expected ErrorCircle to contain filled circle character ●")
	}

	// Should contain ANSI escape for red color (RGB 239,68,68)
	if !strings.Contains(result, "\033[38;2;239;68;68m") {
		t.Error("expected ErrorCircle to contain red ANSI color code")
	}

	// Should contain reset code
	if !strings.Contains(result, "\033[0m") {
		t.Error("expected ErrorCircle to contain ANSI reset code")
	}
}

func TestSuccessCircle_ContainsGreenColor(t *testing.T) {
	result := SuccessCircle()

	if !strings.Contains(result, "●") {
		t.Error("expected SuccessCircle to contain filled circle character ●")
	}

	// Should contain ANSI escape for green color (RGB 79,185,101)
	if !strings.Contains(result, "\033[38;2;79;185;101m") {
		t.Error("expected SuccessCircle to contain green ANSI color code")
	}
}

func TestPendingCircle_ContainsHollowCircle(t *testing.T) {
	result := PendingCircle()

	// Should contain hollow circle character
	if !strings.Contains(result, "○") {
		t.Error("expected PendingCircle to contain hollow circle character ○")
	}
}

func TestWorkingCircle_ContainsOrangeColor(t *testing.T) {
	result := WorkingCircle()

	if !strings.Contains(result, "●") {
		t.Error("expected WorkingCircle to contain filled circle character ●")
	}

	// Should contain ANSI escape for orange color (RGB 255,165,0)
	if !strings.Contains(result, "\033[38;2;255;165;0m") {
		t.Error("expected WorkingCircle to contain orange ANSI color code")
	}
}

func TestDefaultTheme_HasUserMsgBg(t *testing.T) {
	theme := DefaultTheme()

	// UserMsgBg should be set to a non-empty value
	if theme.UserMsgBg == "" {
		t.Error("expected DefaultTheme to have non-empty UserMsgBg")
	}

	// Should be the gruvbox dark gray
	expected := lipgloss.Color("#3c3836")
	if theme.UserMsgBg != expected {
		t.Errorf("expected UserMsgBg=%q, got %q", expected, theme.UserMsgBg)
	}
}
