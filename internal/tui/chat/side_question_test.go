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
		wantPanelRows    int
	}{
		{name: "demo terminal", terminalWidth: 120, terminalHeight: 36, wantWidth: 112, wantResponseRows: 22, wantPanelRows: 29},
		{name: "tall terminal grows", terminalWidth: 120, terminalHeight: 48, wantWidth: 112, wantResponseRows: 34, wantPanelRows: 41},
		{name: "maximum", terminalWidth: 200, terminalHeight: 100, wantWidth: 120, wantResponseRows: 40, wantPanelRows: 47},
		{name: "small terminal", terminalWidth: 28, terminalHeight: 10, wantWidth: 28, wantResponseRows: 1, wantPanelRows: 10},
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
			if got := lipgloss.Height(panel); got != tc.wantPanelRows {
				t.Fatalf("panel height = %d, want %d", got, tc.wantPanelRows)
			}
			if got := lipgloss.Height(panel); got > tc.terminalHeight {
				t.Fatalf("panel height = %d, terminal height = %d", got, tc.terminalHeight)
			}
		})
	}
}

func TestSideQuestionPanelShowsLiveMainStatus(t *testing.T) {
	m := newTestChatModel(true)
	m.width, m.height = 120, 36
	m.sideQuestion.Running = true

	m.streaming = true
	m.phase = "Responding"
	responding := ui.StripANSI(m.renderSideQuestionPanel())
	if !strings.Contains(responding, "Side question · answering · main responding") {
		t.Fatalf("responding header missing live main status: %q", responding)
	}

	m.phase = "Thinking"
	running := ui.StripANSI(m.renderSideQuestionPanel())
	if !strings.Contains(running, "Side question · answering · main running") {
		t.Fatalf("running header missing live main status: %q", running)
	}

	m.sideQuestion.Running = false
	mainStillRunning := ui.StripANSI(m.renderSideQuestionPanel())
	if !strings.Contains(mainStillRunning, "Side question · ready · main running") {
		t.Fatalf("idle side header missing live main status: %q", mainStillRunning)
	}

	m.streaming = false
	done := ui.StripANSI(m.renderSideQuestionPanel())
	if strings.Contains(done, "main responding") || strings.Contains(done, "main running") {
		t.Fatalf("completed main status remained in side header: %q", done)
	}
	if !strings.Contains(done, "Side question · ready") {
		t.Fatalf("side status changed when main completed: %q", done)
	}
}

func TestSideQuestionPanelRendersMarkdown(t *testing.T) {
	m := newTestChatModel(true)
	m.width, m.height = 100, 30
	m.sideQuestion.Question = "markdown?"
	m.sideQuestion.Running = true
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
	m.sideQuestion.Question = "list?"
	m.sideQuestion.Running = true
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
	if !m.sideQuestion.Visible || !m.sideQuestion.Composer.Focused() {
		t.Fatalf("side composer was not reopened and focused: %#v", m.sideQuestion)
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
	_, _ = m.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	if !m.sideQuestion.ConfirmClear || len(m.sideQuestion.History) != 1 {
		t.Fatal("first ctrl+x should only confirm")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	if len(m.sideQuestion.History) != 0 || !m.sideQuestion.Visible {
		t.Fatal("second ctrl+x did not clear history in place")
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

func TestSideTranscriptIsChronologicalAndComposerPreservesMainDraft(t *testing.T) {
	m := newTestChatModel(true)
	m.width, m.height = 100, 24
	m.sideQuestion.Visible = true
	m.sideQuestion.History = []sidequestion.Entry{
		{Question: "first question", Response: "first answer"},
		{Question: "second question", Response: "second answer"},
	}
	m.setTextareaValue("unfinished main draft")
	m.focusSideComposer()
	m.sideQuestion.Composer.SetValue("follow-up")
	provider := llm.NewMockProvider("mock").AddTextResponse("third answer")
	m.SetSideQuestionProviderFactory(func(_, _ string) (llm.Provider, error) { return provider, nil })

	plain := ui.StripANSI(m.renderSideQuestionPanel())
	positions := []int{
		strings.Index(plain, "first question"), strings.Index(plain, "first answer"),
		strings.Index(plain, "second question"), strings.Index(plain, "second answer"),
	}
	for i, position := range positions {
		if position < 0 || (i > 0 && position <= positions[i-1]) {
			t.Fatalf("transcript is not chronological: %q", plain)
		}
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if !m.sideQuestion.Running || cmd == nil {
		t.Fatal("Enter did not send the side composer")
	}
	if got := m.textarea.Value(); got != "unfinished main draft" {
		t.Fatalf("side composer changed main draft: %q", got)
	}
}

func TestSideEscapeClearsDraftThenClosesAndCancelKeepsOpen(t *testing.T) {
	m := newTestChatModel(true)
	m.sideQuestion.Visible = true
	m.focusSideComposer()
	m.sideQuestion.Composer.SetValue("draft")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(*Model)
	if !m.sideQuestion.Visible || m.sideQuestion.Composer.Value() != "" {
		t.Fatalf("first Esc state = visible %v draft %q", m.sideQuestion.Visible, m.sideQuestion.Composer.Value())
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(*Model)
	if m.sideQuestion.Visible {
		t.Fatal("second Esc did not close empty idle overlay")
	}

	sideCtx, sideCancel := context.WithCancel(context.Background())
	m.sideQuestion.Visible = true
	m.sideQuestion.Running = true
	m.sideQuestion.Cancel = sideCancel
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(*Model)
	if sideCtx.Err() == nil || !m.sideQuestion.Visible || m.sideQuestion.Running || !m.sideQuestion.Composer.Focused() {
		t.Fatalf("running Esc did not cancel in place: %#v", m.sideQuestion)
	}
}

func TestSideTranscriptScrollKeysDoNotStealCursorKeys(t *testing.T) {
	m := newTestChatModel(true)
	m.width, m.height = 80, 20
	m.sideQuestion.Visible = true
	m.sideQuestion.History = []sidequestion.Entry{{Question: "q", Response: strings.Repeat("line\n", 50)}}
	m.focusSideComposer()
	m.sideQuestion.Composer.SetValue("editing")

	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.sideQuestion.Scroll != 0 {
		t.Fatalf("ordinary Up scrolled transcript while editing: %d", m.sideQuestion.Scroll)
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.sideQuestion.Scroll == 0 {
		t.Fatal("PageUp did not scroll transcript")
	}
	before := m.sideQuestion.Scroll
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	if m.sideQuestion.Scroll >= before {
		t.Fatalf("Ctrl+Down did not scroll toward transcript bottom: %d >= %d", m.sideQuestion.Scroll, before)
	}
}

func TestSideCommandWhileRunningOnlyRefocusesLiveOverlay(t *testing.T) {
	m := newTestChatModel(true)
	m.sideQuestion.Visible = true
	m.sideQuestion.Running = true
	m.sideQuestion.Question = "live question"
	m.sideQuestion.Response.WriteString("partial answer")
	m.sideQuestion.History = []sidequestion.Entry{{Question: "old", Response: "old answer"}}
	m.setTextareaValue("/side")

	updated, cmd := m.ExecuteCommand("/side")
	m = updated.(*Model)
	if cmd != nil || m.sideQuestion.Question != "live question" || m.sideQuestion.Response.String() != "partial answer" {
		t.Fatalf("/side replaced active exchange: %#v", m.sideQuestion)
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
