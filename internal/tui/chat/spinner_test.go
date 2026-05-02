package chat

import (
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
)

func TestChatSpinnerUsesReducedDefaultFPS(t *testing.T) {
	m := newTestChatModel(false)

	const want = 250 * time.Millisecond
	if m.spinner.Spinner.FPS != want {
		t.Fatalf("spinner FPS = %v, want %v", m.spinner.Spinner.FPS, want)
	}
}

func TestChatSpinnerFPSFromEnv(t *testing.T) {
	t.Setenv(chatSpinnerIntervalEnv, "120")
	if got := chatSpinnerFPSFromEnv(); got != 120*time.Millisecond {
		t.Fatalf("chatSpinnerFPSFromEnv() = %v, want 120ms", got)
	}

	t.Setenv(chatSpinnerIntervalEnv, "0")
	if got := chatSpinnerFPSFromEnv(); got != 250*time.Millisecond {
		t.Fatalf("chatSpinnerFPSFromEnv() with invalid value = %v, want 250ms", got)
	}
}

func TestChatSpinnerTickIgnoredWhilePausedForExternalUI(t *testing.T) {
	m := newTestChatModel(true)
	m.streaming = true
	m.pausedForExternalUI = true

	before := m.spinner.View()
	_, cmd := m.Update(spinner.TickMsg{ID: m.spinner.ID()})
	after := m.spinner.View()

	if cmd != nil {
		t.Fatal("expected no follow-up spinner tick while paused for external UI")
	}
	if after != before {
		t.Fatalf("spinner frame advanced while paused: before=%q after=%q", before, after)
	}
}

func TestChatSpinnerTickContinuesWhileInspectorModeActive(t *testing.T) {
	m := newTestChatModel(true)
	m.streaming = true
	m.inspectorMode = true
	m.inspectorModel = inspector.New(nil, m.width, m.height, m.styles)

	before := m.spinner.View()
	_, cmd := m.Update(spinner.TickMsg{ID: m.spinner.ID()})
	after := m.spinner.View()

	if cmd == nil {
		t.Fatal("expected spinner tick to be re-scheduled while inspector is active")
	}
	if after == before {
		t.Fatalf("expected spinner frame to advance while inspector is active, still %q", after)
	}
}
