package chat

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// The status line is the last row of the alt-screen frame. Bubble Tea v2 clips
// anything beyond the terminal height, so if the viewport render ever exceeds
// its allotted height the footer's last row silently disappears. The wave
// animation path (renderAltScreenViewportLines) must therefore never produce
// more lines than the viewport height, even when content contains an over-wide
// line (e.g. a spawn_agent progress line with a long [provider:model] suffix).
func TestRenderAltScreenViewportLinesClampsOverwideLines(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 40
	m.height = 12
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	vpH := m.viewport.Height()
	if vpH <= 0 {
		t.Fatalf("viewport height = %d, want > 0", vpH)
	}

	lines := make([]string, vpH)
	for i := range lines {
		lines[i] = "line"
	}
	lines[1] = "@agent  7 calls · 4.7k tokens · 25s  [chatgpt:" + strings.Repeat("x", 60) + "]"

	out := m.renderAltScreenViewportLines(lines)
	if got := lipgloss.Height(out); got != vpH {
		t.Fatalf("viewport render height = %d, want %d (over-wide line must be truncated, not wrapped)", got, vpH)
	}
}

// Full-frame invariant: the composed alt-screen frame must never be taller
// than the terminal, or the bottom row (status line) gets clipped.
func TestViewAltScreenFrameNeverExceedsTerminalHeight(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 40
	m.height = 14
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)

	wide := "@agent  7 calls · 4.7k tokens · 25s  [chatgpt:" + strings.Repeat("y", 60) + "]"
	m.viewCache.historyLines = []string{wide, "after"}
	m.viewCache.historyContent = wide + "\nafter"
	m.viewCache.historyValid = true
	m.viewCache.historyWidth = m.width

	// Drive the wave-only animation path: streaming with a pending tool. The
	// first render locks in the tracker version via the normal viewport path;
	// advancing WavePos alone then forces the custom line-render path that
	// historically wrapped over-wide lines.
	m.streaming = true
	m.tracker.HandleToolStart("call1", "spawn_agent", "@agent", nil)

	first := m.viewAltScreen()
	if got := lipgloss.Height(first); got > m.height {
		t.Fatalf("alt-screen frame height = %d exceeds terminal height %d on viewport path", got, m.height)
	}

	m.tracker.WavePos++
	second := m.viewAltScreen()
	if got := lipgloss.Height(second); got > m.height {
		t.Fatalf("alt-screen frame height = %d exceeds terminal height %d on wave path; status line would be clipped", got, m.height)
	}
}
