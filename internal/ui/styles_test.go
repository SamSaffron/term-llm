package ui

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func colorsEqual(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == b
	}
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

func TestThemeFromConfig_Default(t *testing.T) {
	cfg := ThemeConfig{}
	theme := ThemeFromConfig(cfg)

	defaultTheme := DefaultTheme()
	if !colorsEqual(theme.Primary, defaultTheme.Primary) {
		t.Errorf("expected Primary=%v, got %v", defaultTheme.Primary, theme.Primary)
	}
	if !colorsEqual(theme.UserMsgBg, defaultTheme.UserMsgBg) {
		t.Errorf("expected UserMsgBg=%v, got %v", defaultTheme.UserMsgBg, theme.UserMsgBg)
	}
}

func TestThemeFromConfig_UserMsgBgOverride(t *testing.T) {
	cfg := ThemeConfig{
		UserMsgBg: "#ff0000",
	}
	theme := ThemeFromConfig(cfg)

	expected := lipgloss.Color("#ff0000")
	if !colorsEqual(theme.UserMsgBg, expected) {
		t.Errorf("expected UserMsgBg=%v, got %v", expected, theme.UserMsgBg)
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
		got      color.Color
		expected color.Color
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
		if !colorsEqual(tt.got, tt.expected) {
			t.Errorf("%s: expected %v, got %v", tt.name, tt.expected, tt.got)
		}
	}
}

func TestThemeFromConfig_SecondarySetsBorder(t *testing.T) {
	cfg := ThemeConfig{
		Secondary: "#abcdef",
	}
	theme := ThemeFromConfig(cfg)

	expected := lipgloss.Color("#abcdef")
	if !colorsEqual(theme.Border, expected) {
		t.Errorf("expected Border to follow Secondary=%v, got %v", expected, theme.Border)
	}
}

func TestErrorCircle_ContainsRedColor(t *testing.T) {
	result := ErrorCircle()

	if !strings.Contains(result, "●") {
		t.Error("expected ErrorCircle to contain filled circle character ●")
	}

	if !strings.Contains(result, "\033[38;2;239;68;68m") {
		t.Error("expected ErrorCircle to contain red ANSI color code")
	}

	if !strings.Contains(result, "\033[0m") {
		t.Error("expected ErrorCircle to contain ANSI reset code")
	}
}

func TestSuccessCircle_ContainsGreenColor(t *testing.T) {
	result := SuccessCircle()

	if !strings.Contains(result, "●") {
		t.Error("expected SuccessCircle to contain filled circle character ●")
	}

	if !strings.Contains(result, "\033[38;2;79;185;101m") {
		t.Error("expected SuccessCircle to contain green ANSI color code")
	}
}

func TestPendingCircle_ContainsHollowCircle(t *testing.T) {
	result := PendingCircle()

	if !strings.Contains(result, "○") {
		t.Error("expected PendingCircle to contain hollow circle character ○")
	}
}

func TestWorkingCircle_ContainsOrangeColor(t *testing.T) {
	result := WorkingCircle()

	if !strings.Contains(result, "●") {
		t.Error("expected WorkingCircle to contain filled circle character ●")
	}

	if !strings.Contains(result, "\033[38;2;255;165;0m") {
		t.Error("expected WorkingCircle to contain orange ANSI color code")
	}
}

func TestDefaultTheme_HasUserMsgBg(t *testing.T) {
	theme := DefaultTheme()

	if theme.UserMsgBg == nil {
		t.Error("expected DefaultTheme to have non-nil UserMsgBg")
	}

	expected := lipgloss.Color("#3c3836")
	if !colorsEqual(theme.UserMsgBg, expected) {
		t.Errorf("expected UserMsgBg=%v, got %v", expected, theme.UserMsgBg)
	}
}
