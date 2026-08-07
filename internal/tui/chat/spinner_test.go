package chat

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	"github.com/samsaffron/term-llm/internal/ui"
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

func TestChatSpinnerTickContinuesWhileSideQuestionRuns(t *testing.T) {
	m := newTestChatModel(true)
	m.sideQuestion.Running = true

	before := m.spinner.View()
	_, cmd := m.Update(spinner.TickMsg{ID: m.spinner.ID()})
	after := m.spinner.View()

	if cmd == nil {
		t.Fatal("expected spinner tick to be re-scheduled while side question runs")
	}
	if after == before {
		t.Fatalf("expected spinner frame to advance while side question runs, still %q", after)
	}
}

func TestChatSpinnerTickContinuesInFooterWhileBranchPathNotesGenerate(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 100
	m.branchPathNotesRequest = &BranchPathNotesRequest{SourceSessionID: "source"}
	m.branchOperationCancel = func() {}
	m.branchOperationStarted = time.Now().Add(-2 * time.Second)

	spinnerText := ui.StripANSI(m.spinner.View())
	plainActivity := ui.StripANSI(m.renderBranchPathNotesActivity())
	if strings.Contains(plainActivity, spinnerText) {
		t.Fatalf("branch activity rendered spinner %q in the stream: %q", spinnerText, plainActivity)
	}
	if !strings.Contains(plainActivity, "○ Path notes from an earlier path") {
		t.Fatalf("branch activity missing running item: %q", plainActivity)
	}
	plainStatus := ui.StripANSI(m.renderStatusLine())
	if spinnerText == "" || !strings.Contains(plainStatus, spinnerText) || !strings.Contains(plainStatus, "Creating path notes") {
		t.Fatalf("footer missing path-note spinner/status %q: %q", spinnerText, plainStatus)
	}

	before := m.spinner.View()
	_, cmd := m.Update(spinner.TickMsg{ID: m.spinner.ID()})
	after := m.spinner.View()
	if cmd == nil {
		t.Fatal("expected spinner tick to be re-scheduled while path notes generate")
	}
	if after == before {
		t.Fatalf("expected branch spinner frame to advance, still %q", after)
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
