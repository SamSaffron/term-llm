package chat

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sidequestion"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestSideQuestionPanelResponsiveReadingSurface(t *testing.T) {
	tests := []struct {
		name             string
		terminalWidth    int
		terminalHeight   int
		wantWidth        int
		wantResponseRows int
	}{
		{name: "demo terminal", terminalWidth: 120, terminalHeight: 36, wantWidth: 112, wantResponseRows: 26},
		{name: "maximum", terminalWidth: 200, terminalHeight: 100, wantWidth: 120, wantResponseRows: 40},
		{name: "small terminal", terminalWidth: 28, terminalHeight: 10, wantWidth: 28, wantResponseRows: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestChatModel(true)
			m.width, m.height = tc.terminalWidth, tc.terminalHeight
			geometry := m.sideQuestionPanelGeometry()
			if geometry.width != tc.wantWidth || geometry.responseRows != tc.wantResponseRows {
				t.Fatalf("geometry = %+v, want width %d response rows %d", geometry, tc.wantWidth, tc.wantResponseRows)
			}
			panel := m.renderSideQuestionPanel()
			if got := lipgloss.Width(panel); got > tc.terminalWidth {
				t.Fatalf("panel width = %d, terminal width = %d", got, tc.terminalWidth)
			}
			if got := lipgloss.Height(panel); got > tc.terminalHeight {
				t.Fatalf("panel height = %d, terminal height = %d", got, tc.terminalHeight)
			}
		})
	}
}

func TestSideQuestionPanelRendersMarkdown(t *testing.T) {
	m := newTestChatModel(true)
	m.width, m.height = 100, 30
	m.sideQuestion.Response.WriteString("## Result\n\n- **bold item**\n- `code`")

	panel := m.renderSideQuestionPanel()
	plain := ui.StripANSI(panel)
	if strings.Contains(plain, "**bold item**") || strings.Contains(plain, "`code`") {
		t.Fatalf("panel retained raw markdown: %q", plain)
	}
	for _, want := range []string{"Result", "bold item", "code"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("panel missing rendered markdown text %q: %q", want, plain)
		}
	}
	if !strings.Contains(panel, "\x1b[") {
		t.Fatalf("panel response did not use ANSI markdown styles: %q", panel)
	}
}

func TestSideQuestionPanelLongAnswerScrollsRenderedLines(t *testing.T) {
	m := newTestChatModel(true)
	m.width, m.height = 80, 20
	for i := 1; i <= 30; i++ {
		m.sideQuestion.Response.WriteString(fmt.Sprintf("- item %02d\n", i))
	}

	bottom := ui.StripANSI(m.renderSideQuestionPanel())
	if !strings.Contains(bottom, "item 30") || strings.Contains(bottom, "item 01") {
		t.Fatalf("default viewport should show answer tail: %q", bottom)
	}
	m.sideQuestion.Scroll = 1000
	top := ui.StripANSI(m.renderSideQuestionPanel())
	if !strings.Contains(top, "item 01") || strings.Contains(top, "item 30") {
		t.Fatalf("scrolled viewport should clamp at answer start: %q", top)
	}
	for _, line := range strings.Split(m.renderSideQuestionPanel(), "\n") {
		if got := ansi.StringWidth(line); got > m.width {
			t.Fatalf("rendered line width = %d, terminal width = %d: %q", got, m.width, line)
		}
	}
}

func TestSideCommandOpensOverlayAndClearsSubmittedCommand(t *testing.T) {
	m := newTestChatModel(true)
	m.sess = &session.Session{ID: "main-session"}
	m.messages = []session.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "main fact"}}}}
	m.scrollOffset = 3
	m.setTextareaValue("/side what does that mean?")
	m.completions.Show()
	provider := llm.NewMockProvider("mock").AddTextResponse("side answer")
	m.SetSideQuestionProviderFactory(func(_, _ string) (llm.Provider, error) { return provider, nil })

	updated, cmd := m.ExecuteCommand("/side what does that mean?")
	m = updated.(*Model)
	if !m.sideQuestion.Visible || !m.sideQuestion.Running || m.sess.ID != "main-session" || cmd == nil {
		t.Fatalf("side state = visible %v running %v session %q cmd %v", m.sideQuestion.Visible, m.sideQuestion.Running, m.sess.ID, cmd != nil)
	}
	if m.scrollOffset != 3 {
		t.Fatalf("overlay changed main scroll: %d", m.scrollOffset)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want submitted command cleared", got)
	}
	if m.completions.IsVisible() {
		t.Fatal("expected command completions to be hidden")
	}
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			break
		}
		updated, cmd = m.Update(msg)
		m = updated.(*Model)
	}
	if m.sideQuestion.Running || len(m.sideQuestion.History) != 1 || m.sideQuestion.History[0].Response != "side answer" {
		t.Fatalf("completed side state = %#v", m.sideQuestion)
	}
	if len(m.messages) != 1 {
		t.Fatalf("side content entered transcript: %#v", m.messages)
	}
}

func TestSideCommandReopensOverlayAndClearsSubmittedCommand(t *testing.T) {
	m := newTestChatModel(true)
	m.sideQuestion.History = []sidequestion.Entry{{Question: "earlier", Response: "answer"}}
	m.setTextareaValue("/side")
	m.completions.Show()

	updated, cmd := m.ExecuteCommand("/side")
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("reopening side history unexpectedly returned a command")
	}
	if !m.sideQuestion.Visible || m.sideQuestion.Selected != 0 {
		t.Fatalf("side history was not reopened: %#v", m.sideQuestion)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want submitted command cleared", got)
	}
	if m.completions.IsVisible() {
		t.Fatal("expected command completions to be hidden")
	}
}

func TestSideCommandStartupErrorUsesSlashCommandClearing(t *testing.T) {
	m := newTestChatModel(true)
	m.setTextareaValue("/side question")
	m.completions.Show()

	updated, _ := m.ExecuteCommand("/side question")
	m = updated.(*Model)
	if m.sideQuestion.Running {
		t.Fatal("side question unexpectedly started without a provider factory")
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want established slash-command error clearing", got)
	}
}

func TestSideSnapshotExcludesActiveMainTurn(t *testing.T) {
	m := newTestChatModel(true)
	m.messages = []session.Message{
		{Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "system"}}},
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "completed question"}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "completed answer"}}},
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "active question"}}},
	}
	m.streaming = true
	got := m.sideSnapshot()
	if len(got) != 3 {
		t.Fatalf("snapshot len = %d, want 3: %#v", len(got), got)
	}
}

func TestSideCancellationDoesNotCancelMain(t *testing.T) {
	m := newTestChatModel(true)
	mainCtx, mainCancel := context.WithCancel(context.Background())
	defer mainCancel()
	sideCtx, sideCancel := context.WithCancel(context.Background())
	m.streamCancelFunc = mainCancel
	m.sideQuestion = SideQuestionState{Visible: true, Running: true, Cancel: sideCancel, Generation: 1}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(*Model)
	if sideCtx.Err() == nil {
		t.Fatal("side context was not cancelled")
	}
	if mainCtx.Err() != nil {
		t.Fatal("side cancellation cancelled main")
	}
}

func TestLateSideGenerationIgnoredAndClearConfirmed(t *testing.T) {
	m := newTestChatModel(true)
	m.sideQuestion = SideQuestionState{Visible: true, Generation: 2, History: []sidequestion.Entry{{Question: "q", Response: "a"}}}
	_, _ = m.Update(sideQuestionEventMsg{generation: 1, event: llm.Event{Type: llm.EventTextDelta, Text: "late"}})
	if m.sideQuestion.Response.Len() != 0 {
		t.Fatal("late side event was applied")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'x'})
	if !m.sideQuestion.ConfirmClear || len(m.sideQuestion.History) != 1 {
		t.Fatal("first x should only confirm")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'x'})
	if len(m.sideQuestion.History) != 0 || m.sideQuestion.Visible {
		t.Fatal("second x did not clear history")
	}
}

func TestSideQuestionMirrorsMainReasoningRequestNotDisplayMode(t *testing.T) {
	m := newTestChatModel(true)
	m.modelName = "reasoning-model"
	m.reasoningModeOverride = "expanded"
	m.sess = &session.Session{ID: "main", ReasoningEffort: "high", ReasoningMode: "pro"}
	provider := llm.NewMockProvider("mock").AddTextResponse("answer")
	m.SetSideQuestionProviderFactory(func(_, _ string) (llm.Provider, error) { return provider, nil })
	_, cmd := m.cmdSide("question")
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			break
		}
		cmd = m.updateSideQuestion(msg.(sideQuestionEventMsg))
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("requests = %d", len(provider.Requests))
	}
	req := provider.Requests[0]
	if req.ReasoningEffort != "high" || req.ReasoningEffort == m.reasoningModeOverride || req.Responses == nil || req.Responses.ReasoningMode != "pro" {
		t.Fatalf("side reasoning config = effort %q responses %#v", req.ReasoningEffort, req.Responses)
	}
}

type stubbornTUISideProvider struct {
	release chan struct{}
	mu      sync.Mutex
	starts  int
}

func (p *stubbornTUISideProvider) Name() string                   { return "stubborn" }
func (p *stubbornTUISideProvider) Credential() string             { return "test" }
func (p *stubbornTUISideProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *stubbornTUISideProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	return &stubbornTUISideStream{release: p.release}, nil
}

type stubbornTUISideStream struct{ release <-chan struct{} }

func (s *stubbornTUISideStream) Recv() (llm.Event, error) {
	<-s.release
	return llm.Event{}, io.EOF
}
func (*stubbornTUISideStream) Close() error { return nil }

func TestSideCleanupIsBoundedAndPreventsStubbornRestartOverlap(t *testing.T) {
	m := newTestChatModel(true)
	provider := &stubbornTUISideProvider{release: make(chan struct{})}
	m.SetSideQuestionProviderFactory(func(_, _ string) (llm.Provider, error) { return provider, nil })
	_, _ = m.cmdSide("first")
	deadline := time.Now().Add(time.Second)
	for {
		provider.mu.Lock()
		started := provider.starts
		provider.mu.Unlock()
		if started == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider did not start")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	m.clearSideQuestionHistory()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cleanup took %v", elapsed)
	}
	_, cmd := m.cmdSide("second")
	if cmd != nil || m.sideQuestion.Err == nil || !strings.Contains(m.sideQuestion.Err.Error(), "still stopping") {
		t.Fatalf("restart state: cmd=%v err=%v", cmd != nil, m.sideQuestion.Err)
	}
	provider.mu.Lock()
	starts := provider.starts
	provider.mu.Unlock()
	if starts != 1 {
		t.Fatalf("overlapping provider starts = %d", starts)
	}
	close(provider.release)
}

func TestSyntheticSideUsageSurvivesHistoryCleanupInSessionStats(t *testing.T) {
	m := newTestChatModel(true)
	m.sideQuestion.Generation = 1
	m.sideQuestion.Running = true
	m.updateSideQuestion(sideQuestionEventMsg{
		generation: 1,
		result:     &sidequestion.Result{Response: sidequestion.ToolAttemptResponse, Synthetic: true, Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}},
	})
	m.clearSideQuestionHistory()
	if m.stats == nil || m.stats.InputTokens != 7 || m.stats.OutputTokens != 3 {
		t.Fatalf("side usage lost after cleanup: %#v", m.stats)
	}
}

func TestSideAndOnlySideIsStreamingLocalCommand(t *testing.T) {
	if !isStreamingLocalSlashCommand("/side question") {
		t.Fatal("/side should be available while main streams")
	}
	if isStreamingLocalSlashCommand("/main") || isSlashCommandLike("/main") {
		t.Fatal("/main should not exist")
	}
}
