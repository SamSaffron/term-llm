package ansisafe

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestApplyReverseVideo(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"plain text", "hello world"},
		{"empty line", ""},
		{"ansi styled", "\033[31mred\033[0m text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyReverseVideo(tt.line)
			// Must contain selection background
			if !strings.Contains(got, selBg) {
				t.Errorf("missing selBg in %q", got)
			}
			// Must end with background reset
			if !strings.HasSuffix(got, selBgOff) {
				t.Errorf("missing selBgOff suffix in %q", got)
			}
		})
	}
}

func TestHighlightSurvivesResets(t *testing.T) {
	// Table lines may contain embedded \033[0m resets.
	// Verify selection background is re-asserted after each reset.
	line := "\033[0m  Claim  \033[0m│\033[0m  Source  \033[0m│\033[0m  Date  \033[0m"
	got := ApplyReverseVideo(line)

	resetCount := strings.Count(line, "\033[0m")
	bgCount := strings.Count(got, selBg)
	// We expect at least resetCount + 1 (initial + re-assert after each reset)
	if bgCount < resetCount+1 {
		t.Errorf("selBg count = %d, want >= %d (resets=%d)", bgCount, resetCount+1, resetCount)
	}
}

func TestApplyPartialReverseVideo(t *testing.T) {
	tests := []struct {
		name            string
		line            string
		startCol        int
		endCol          int
		wantHasSelBg    bool
		wantVisualWidth int
	}{
		{
			name:            "middle selection",
			line:            "hello world",
			startCol:        2,
			endCol:          7,
			wantHasSelBg:    true,
			wantVisualWidth: 11,
		},
		{
			name:            "full line via endCol=-1",
			line:            "hello",
			startCol:        0,
			endCol:          -1,
			wantHasSelBg:    true,
			wantVisualWidth: 5,
		},
		{
			name:            "zero width selection",
			line:            "hello",
			startCol:        3,
			endCol:          3,
			wantHasSelBg:    false,
			wantVisualWidth: 5,
		},
		{
			name:            "empty line",
			line:            "",
			startCol:        0,
			endCol:          5,
			wantHasSelBg:    false,
			wantVisualWidth: 0,
		},
		{
			name:            "ansi styled partial",
			line:            "\033[31mred\033[0m blue",
			startCol:        0,
			endCol:          3,
			wantHasSelBg:    true,
			wantVisualWidth: 8,
		},
		{
			name:            "wide char CJK",
			line:            "a你好b",
			startCol:        1,
			endCol:          5,
			wantHasSelBg:    true,
			wantVisualWidth: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyPartialReverseVideo(tt.line, tt.startCol, tt.endCol)

			gotWidth := ansi.StringWidth(got)
			if gotWidth != tt.wantVisualWidth {
				t.Errorf("visual width = %d, want %d (got %q)", gotWidth, tt.wantVisualWidth, got)
			}

			hasSelBg := strings.Contains(got, selBg)
			if hasSelBg != tt.wantHasSelBg {
				t.Errorf("contains selBg = %v, want %v", hasSelBg, tt.wantHasSelBg)
			}
		})
	}
}

func TestInjectSelectionBg(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"plain", "hello"},
		{"with color", "\033[31mred\033[0m"},
		{"multiple resets", "\033[1mbold\033[0m normal \033[32mgreen\033[0m"},
		{"256 color", "\033[38;5;200mpink\033[0m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectSelectionBg(tt.input)
			if !strings.HasPrefix(got, selBg) {
				t.Errorf("missing leading selBg: %q", got)
			}
			if !strings.HasSuffix(got, selBgOff) {
				t.Errorf("missing trailing selBgOff: %q", got)
			}
			// Every \033[0m must be followed by selBg
			parts := strings.Split(got, "\033[0m")
			for i := 0; i < len(parts)-1; i++ {
				next := parts[i+1]
				if !strings.HasPrefix(next, selBg) {
					t.Errorf("\\033[0m at segment %d not followed by selBg", i)
				}
			}
		})
	}
}
