package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/llm"
	renderchat "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/sidequestion"
	"github.com/samsaffron/term-llm/internal/ui"
)

type SideQuestionState struct {
	Visible      bool
	Running      bool
	Question     string
	Response     strings.Builder
	Synthetic    bool
	History      []sidequestion.Entry
	Composer     textarea.Model
	ComposerInit bool
	Cancel       context.CancelFunc
	Done         chan struct{}
	Generation   uint64
	Usage        llm.Usage
	Err          error
	Scroll       int
	ConfirmClear bool
	events       chan sideQuestionEventMsg

	selectionLines []string
	panelY         int
	panelContentX  int
	panelHeight    int
}

type sideQuestionEventMsg struct {
	generation uint64
	event      llm.Event
	result     *sidequestion.Result
	err        error
}

func (m *Model) SetSideQuestionProviderFactory(factory func(providerKey, model string) (llm.Provider, error)) {
	m.sideProviderFactory = factory
}

func (m *Model) sideSnapshot() []llm.Message {
	// The streaming context tracks the exact completed provider boundary as the
	// main turn advances through assistant/tool cycles. Fork from that boundary
	// so side questions see tool results produced during the active user turn,
	// while excluding only the assistant response that is still in progress.
	m.contextEstimateMu.Lock()
	if m.streaming && len(m.streamingContextMessages) > 0 {
		messages := sidequestion.CloneMessages(m.streamingContextMessages)
		if m.streamingContextPendingAssistant && len(messages) > 0 && messages[len(messages)-1].Role == llm.RoleAssistant {
			messages = messages[:len(messages)-1]
		}
		m.contextEstimateMu.Unlock()
		return messages
	}
	m.contextEstimateMu.Unlock()
	return m.buildMessages()
}

func (m *Model) ensureSideComposer() {
	if m.sideQuestion.ComposerInit {
		return
	}
	composer := textarea.New()
	composerPrompt := "❯ "
	composer.Placeholder = "Ask a follow-up…"
	composer.Prompt = composerPrompt
	composer.ShowLineNumbers = false
	composer.CharLimit = 0
	composer.SetHeight(1)
	composer.SetVirtualCursor(true)
	composer.SetPromptFunc(lipgloss.Width(composerPrompt), func(textarea.PromptInfo) string {
		return composerPrompt
	})
	theme := m.styles.Theme()
	composerStyles := composer.Styles()
	composerStyles.Focused.CursorLine = lipgloss.NewStyle()
	composerStyles.Focused.Base = lipgloss.NewStyle().Foreground(theme.Text)
	composerStyles.Focused.Placeholder = lipgloss.NewStyle().Foreground(theme.Muted)
	composerStyles.Focused.EndOfBuffer = lipgloss.NewStyle()
	composerStyles.Focused.Prompt = lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
	composerStyles.Blurred = composerStyles.Focused
	composer.SetStyles(composerStyles)
	composer.Focus()
	m.sideQuestion.Composer = composer
	m.sideQuestion.ComposerInit = true
	m.resizeSideComposer()
}

func (m *Model) resizeSideComposer() {
	if !m.sideQuestion.ComposerInit {
		return
	}
	m.sideQuestion.Composer.SetWidth(m.sideQuestionPanelGeometry().bodyWidth)
	m.sideQuestion.Composer.SetHeight(1)
}

func (m *Model) clearSubmittedSideCommand(question string) {
	value := strings.TrimSpace(m.textarea.Value())
	if len(value) < len("/side") || !strings.EqualFold(value[:len("/side")], "/side") {
		return
	}
	if strings.TrimSpace(value[len("/side"):]) == question {
		m.setTextareaValue("")
		if m.completions != nil {
			m.completions.Hide()
		}
	}
}

func (m *Model) focusSideComposer() {
	m.ensureSideComposer()
	m.resizeSideComposer()
	m.sideQuestion.Composer.Focus()
}

func (m *Model) cmdSide(question string) (tea.Model, tea.Cmd) {
	question = strings.TrimSpace(question)
	m.clearSubmittedSideCommand(question)
	m.sideQuestion.Visible = true
	m.sideQuestion.ConfirmClear = false
	if question == "" {
		m.focusSideComposer()
		return m, nil
	}
	if m.sideQuestion.Running {
		m.sideQuestion.Err = errors.New("A side question is already running")
		return m, nil
	}
	if m.sideQuestion.Done != nil {
		select {
		case <-m.sideQuestion.Done:
			m.sideQuestion.Done = nil
		default:
			m.sideQuestion.Err = errors.New("The previous side question is still stopping")
			return m, nil
		}
	}
	if m.sideProviderFactory == nil {
		return m.showSystemMessage("Side questions are unavailable for this runtime")
	}
	provider, err := m.sideProviderFactory(m.providerKey, m.modelName)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Unable to start side question: %v", err))
	}
	history := append([]sidequestion.Entry(nil), m.sideQuestion.History...)
	inputLimit := 0
	if m.engine != nil {
		inputLimit = m.engine.InputLimit()
	}
	messages, err := sidequestion.BuildMessages(m.sideSnapshot(), history, question, m.providerKey, m.modelName, inputLimit)
	if err != nil {
		if cleaner, ok := provider.(llm.ProviderCleaner); ok {
			cleaner.CleanupMCP()
		}
		return m.showSystemMessage(fmt.Sprintf("Unable to start side question: %v", err))
	}
	m.ensureSideComposer()
	m.sideQuestion.Composer.SetValue("")
	m.sideQuestion.Composer.Focus()

	m.sideQuestion.Generation++
	generation := m.sideQuestion.Generation
	ctx, cancel := context.WithCancel(m.rootCtx)
	m.sideQuestion.Visible = true
	m.sideQuestion.Running = true
	m.sideQuestion.Question = question
	m.sideQuestion.Response.Reset()
	m.sideQuestion.Synthetic = false
	m.sideQuestion.Cancel = cancel
	m.sideQuestion.Err = nil
	m.sideQuestion.Scroll = 0
	m.sideQuestion.ConfirmClear = false
	m.sideQuestion.events = make(chan sideQuestionEventMsg, 64)
	done := make(chan struct{})
	m.sideQuestion.Done = done
	events := m.sideQuestion.events
	reasoningEffort := ""
	reasoningMode := ""
	if m.sess != nil {
		reasoningEffort = strings.TrimSpace(m.sess.ReasoningEffort)
		reasoningMode = strings.TrimSpace(m.sess.ReasoningMode)
	}
	req := llm.Request{
		Model:           m.modelName,
		Messages:        messages,
		ReasoningEffort: reasoningEffort,
		Responses:       &llm.ResponsesOptions{ReasoningMode: reasoningMode},
	}
	go func() {
		defer close(done)
		defer close(events)
		defer func() {
			if cleaner, ok := provider.(llm.ProviderCleaner); ok {
				cleaner.CleanupMCP()
			}
		}()
		result, runErr := sidequestion.Run(ctx, provider, req, func(event llm.Event) {
			if len(events) >= cap(events)-1 {
				return
			}
			select {
			case events <- sideQuestionEventMsg{generation: generation, event: event}:
			default:
			}
		})
		select {
		case events <- sideQuestionEventMsg{generation: generation, result: &result, err: runErr}:
		case <-ctx.Done():
		}
	}()
	commands := []tea.Cmd{m.listenSideQuestion(events)}
	if !m.streaming {
		// Reuse the chat spinner's existing tick loop when the main response is
		// active; only seed it here when the side request is the sole activity.
		commands = append(commands, m.spinner.Tick)
	}
	return m, tea.Batch(commands...)
}

func (m *Model) listenSideQuestion(events <-chan sideQuestionEventMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-events
		if !ok {
			return nil
		}
		return msg
	}
}

func (m *Model) updateSideQuestion(msg sideQuestionEventMsg) tea.Cmd {
	if msg.generation != m.sideQuestion.Generation {
		return nil
	}
	if msg.result != nil || msg.err != nil {
		m.sideQuestion.Running = false
		m.sideQuestion.Cancel = nil
		if msg.result != nil {
			m.sideQuestion.Usage = msg.result.Usage
			if m.stats != nil {
				m.stats.AddSideQuestionUsageForModel(m.modelName, msg.result.Usage.InputTokens, msg.result.Usage.OutputTokens, msg.result.Usage.CachedInputTokens, msg.result.Usage.CacheWriteTokens)
			}
		}
		if errors.Is(msg.err, context.Canceled) {
			m.sideQuestion.Question = ""
			m.sideQuestion.Response.Reset()
			m.focusSideComposer()
			return nil
		}
		if msg.err != nil {
			m.sideQuestion.Err = msg.err
			m.focusSideComposer()
			return nil
		}
		m.sideQuestion.Response.Reset()
		m.sideQuestion.Response.WriteString(msg.result.Response)
		m.sideQuestion.Synthetic = msg.result.Synthetic
		if !msg.result.Synthetic && strings.TrimSpace(msg.result.Response) != "" {
			m.sideQuestion.History = sidequestion.AppendHistory(m.sideQuestion.History, sidequestion.Entry{
				Question: m.sideQuestion.Question, Response: msg.result.Response,
				CreatedAt: time.Now(), Usage: msg.result.Usage,
			})
		}
		m.focusSideComposer()
		return nil
	}
	switch msg.event.Type {
	case llm.EventTextDelta:
		m.sideQuestion.Response.WriteString(msg.event.Text)
	case llm.EventAttemptDiscard:
		m.sideQuestion.Response.Reset()
	case llm.EventUsage:
		if msg.event.Use != nil {
			m.sideQuestion.Usage.Add(*msg.event.Use)
		}
	case llm.EventRetry:
		m.sideQuestion.Err = fmt.Errorf("retrying side question (attempt %d)", msg.event.RetryAttempt)
	}
	return m.listenSideQuestion(m.sideQuestion.events)
}

func (m *Model) cancelSideQuestion() {
	cancel, done := m.sideQuestion.Cancel, m.sideQuestion.Done
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
			m.sideQuestion.Done = nil
		default:
		}
	}
	m.sideQuestion.Generation++
	m.sideQuestion.Running = false
	m.sideQuestion.Cancel = nil
	m.sideQuestion.Question = ""
	m.sideQuestion.Response.Reset()
	m.sideQuestion.Err = nil
	m.focusSideComposer()
}

func (m *Model) clearSideQuestionHistory() {
	if m.sideQuestion.Running || m.sideQuestion.Done != nil {
		m.cancelSideQuestion()
	}
	m.sideQuestion.History = nil
	m.sideQuestion.Question = ""
	m.sideQuestion.Response.Reset()
	m.sideQuestion.Synthetic = false
	m.sideQuestion.Usage = llm.Usage{}
	m.sideQuestion.Err = nil
	m.sideQuestion.Scroll = 0
	m.sideQuestion.Visible = false
	m.sideQuestion.ConfirmClear = false
}

func (m *Model) handleSideQuestionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	keyName := strings.ToLower(msg.String())
	if m.selection.SideQuestion && keyName != "ctrl+c" && keyName != "ctrl+y" {
		m.selection = Selection{}
	}
	if m.sideQuestion.ConfirmClear && keyName != "ctrl+x" {
		m.sideQuestion.ConfirmClear = false
	}
	switch keyName {
	case "esc":
		if m.sideQuestion.Running {
			m.cancelSideQuestion()
			return m, nil
		}
		m.focusSideComposer()
		if m.sideQuestion.Composer.Value() != "" {
			m.sideQuestion.Composer.SetValue("")
			return m, nil
		}
		m.sideQuestion.Visible = false
	case "enter":
		if m.sideQuestion.Running {
			return m, nil
		}
		m.focusSideComposer()
		question := strings.TrimSpace(m.sideQuestion.Composer.Value())
		if question == "" {
			return m, nil
		}
		return m.cmdSide(question)
	case "pgup", "ctrl+up":
		m.sideQuestion.Scroll += max(1, m.sideQuestionPanelGeometry().responseRows-1)
	case "pgdown", "ctrl+down":
		m.sideQuestion.Scroll = max(0, m.sideQuestion.Scroll-max(1, m.sideQuestionPanelGeometry().responseRows-1))
	case "ctrl+c":
		if m.selection.Active {
			cmd := m.copySelectionToClipboard()
			m.selection = Selection{}
			return m, cmd
		}
		m.sideQuestion.Visible = false
		m.sideQuestion.ConfirmClear = false
		m.selection = Selection{}
	case "ctrl+y":
		if m.selection.Active {
			cmd := m.copySelectionToClipboard()
			m.selection = Selection{}
			return m, cmd
		}
		return m.cmdCopy(nil)
	case "ctrl+x":
		if !m.sideQuestion.Running {
			if m.sideQuestion.ConfirmClear {
				m.clearSideQuestionHistory()
				m.sideQuestion.Visible = true
				m.focusSideComposer()
			} else if len(m.sideQuestion.History) > 0 {
				m.sideQuestion.ConfirmClear = true
			}
		}
	default:
		m.focusSideComposer()
		var cmd tea.Cmd
		m.sideQuestion.Composer, cmd = m.sideQuestion.Composer.Update(msg)
		return m, cmd
	}
	return m, nil
}

type sideQuestionPanelSize struct {
	width        int
	bodyWidth    int
	responseRows int
}

func (m *Model) sideQuestionSelectionPosition(x, y int) ContentPos {
	geometry := m.sideQuestionPanelGeometry()
	line := min(max(1, y-m.sideQuestion.panelY), max(1, m.sideQuestion.panelHeight-2))
	col := min(max(0, x-m.sideQuestion.panelContentX), geometry.bodyWidth)
	return ContentPos{Line: line, Col: col}
}

func (m *Model) handleSideQuestionMouseWheel(msg tea.MouseMsg) bool {
	wheel, ok := msg.(tea.MouseWheelMsg)
	if !ok {
		return false
	}
	switch wheel.Mouse().Button {
	case tea.MouseWheelUp:
		m.sideQuestion.Scroll += 3
	case tea.MouseWheelDown:
		m.sideQuestion.Scroll = max(0, m.sideQuestion.Scroll-3)
	case tea.MouseWheelLeft, tea.MouseWheelRight:
		// The side transcript has no horizontal scroll, but the modal still owns
		// the gesture so the conversation underneath cannot move.
	}
	return true
}

func (m *Model) handleSideQuestionSelectionMouse(msg tea.MouseMsg) bool {
	mouse := msg.Mouse()
	switch msg.(type) {
	case tea.MouseClickMsg:
		if mouse.Button != tea.MouseLeft {
			return false
		}
		geometry := m.sideQuestionPanelGeometry()
		inside := mouse.X >= m.sideQuestion.panelContentX && mouse.X <= m.sideQuestion.panelContentX+geometry.bodyWidth &&
			mouse.Y > m.sideQuestion.panelY && mouse.Y < m.sideQuestion.panelY+m.sideQuestion.panelHeight-1
		if !inside {
			if m.selection.SideQuestion {
				m.selection = Selection{}
			}
			return true
		}
		pos := m.sideQuestionSelectionPosition(mouse.X, mouse.Y)
		m.beginSelection(pos, true)
		return true
	case tea.MouseMotionMsg:
		if !m.selection.SideQuestion {
			return false
		}
		return m.moveSelection(m.sideQuestionSelectionPosition(mouse.X, mouse.Y))
	case tea.MouseReleaseMsg:
		if !m.selection.SideQuestion {
			return false
		}
		return m.finishSelection(m.sideQuestionSelectionPosition(mouse.X, mouse.Y))
	}
	return false
}

func (m *Model) sideQuestionPanelGeometry() sideQuestionPanelSize {
	// Leave several rows of the underlying conversation visible around the panel
	// while making the response a useful reading surface on normal terminals.
	// The clamps keep very large terminals comfortable and let small terminals
	// use every cell safely.
	width := min(120, max(64, m.width-8))
	width = min(width, max(1, m.width))
	bodyWidth := max(1, width-4) // border plus one cell of padding on each side
	responseRows := min(40, max(1, m.height-14))
	return sideQuestionPanelSize{width: width, bodyWidth: bodyWidth, responseRows: responseRows}
}

func (m *Model) renderSideQuestionTranscript(width int) []string {
	theme := m.styles.Theme()
	errorStyle := lipgloss.NewStyle().Foreground(theme.Error)
	var transcript strings.Builder
	appendExchange := func(question, response string) {
		if transcript.Len() > 0 {
			transcript.WriteString("\n\n")
		}
		transcript.WriteString(renderchat.RenderUserTextBlock(strings.TrimSpace(question), width, theme))
		transcript.WriteString("\n\n")
		if strings.TrimSpace(response) != "" {
			transcript.WriteString(ui.RenderMarkdownWithOptions(response, width, ui.MarkdownRenderOptions{
				WrapOffset:        0,
				NormalizeTabs:     true,
				NormalizeNewlines: false,
			}))
		} else if m.sideQuestion.Running {
			transcript.WriteString(m.spinner.View())
		}
	}
	for _, entry := range m.sideQuestion.History {
		appendExchange(entry.Question, entry.Response)
	}
	if m.sideQuestion.Running || m.sideQuestion.Synthetic || m.sideQuestion.Err != nil {
		appendExchange(m.sideQuestion.Question, m.sideQuestion.Response.String())
	}
	if m.sideQuestion.Err != nil {
		if transcript.Len() > 0 {
			transcript.WriteString("\n\n")
		}
		transcript.WriteString(errorStyle.Render(wordwrap.String(m.sideQuestion.Err.Error(), width)))
	}
	return strings.Split(strings.Trim(transcript.String(), "\n"), "\n")
}

func sideQuestionFooter(running, confirmClear bool, width int) string {
	footer := "Esc cancel · Ctrl+C close"
	if !running {
		switch {
		case width >= 64:
			footer = "Enter send · Esc clear/close · PgUp/PgDn scroll · Ctrl+C close · Ctrl+X clear"
		case width >= 36:
			footer = "Enter send · Esc clear/close · Ctrl+C close"
		default:
			footer = "Enter send · Ctrl+C close"
		}
	}
	if confirmClear {
		footer = "Press Ctrl+X again to clear side history"
	}
	return ansi.Truncate(footer, width, "…")
}

func (m *Model) renderSideQuestionPanel() string {
	geometry := m.sideQuestionPanelGeometry()
	lines := m.renderSideQuestionTranscript(geometry.bodyWidth)
	maxScroll := max(0, len(lines)-geometry.responseRows)
	scroll := min(maxScroll, max(0, m.sideQuestion.Scroll))
	start := maxScroll - scroll
	end := min(len(lines), start+geometry.responseRows)
	visibleLines := append([]string(nil), lines[start:end]...)
	for len(visibleLines) < geometry.responseRows {
		visibleLines = append(visibleLines, "")
	}
	visible := strings.Join(visibleLines, "\n")
	status := "ready"
	if m.sideQuestion.Running {
		status = "answering"
	}
	mainStatus := ""
	if m.streaming {
		mainStatus = " · main running"
		if strings.EqualFold(strings.TrimSpace(m.phase), "responding") {
			mainStatus = " · main responding"
		}
	}
	attention := ""
	if m.approvalModel != nil || m.approvalDoneCh != nil {
		attention = " · main needs approval"
	} else if m.askUserModel != nil || m.askUserDoneCh != nil {
		attention = " · main needs input"
	}
	header := ansi.Truncate("Side question · "+status+mainStatus+attention, geometry.bodyWidth, "…")
	footer := sideQuestionFooter(m.sideQuestion.Running, m.sideQuestion.ConfirmClear, geometry.bodyWidth)
	theme := m.styles.Theme()
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Primary)
	footerStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	content := fmt.Sprintf("%s\n\n%s", headerStyle.Render(header), visible)
	m.ensureSideComposer()
	content += "\n\n" + m.sideQuestion.Composer.View()
	content += "\n\n" + footerStyle.Render(footer)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Border).
		Width(geometry.width).
		Padding(0, 1).
		Render(content)
}

func (m *Model) sideQuestionSelectableLines(panelLines []string, contentOffset, contentWidth int) []string {
	lines := make([]string, len(panelLines))
	for i, line := range panelLines {
		if i == 0 || i == len(panelLines)-1 {
			continue
		}
		lines[i] = ansi.Cut(line, contentOffset, contentOffset+contentWidth)
	}
	return lines
}

func (m *Model) applySideQuestionSelection(panel string, contentOffset, contentWidth int) string {
	if !m.selection.SideQuestion {
		return panel
	}
	return applySelectionHighlight(panel, m.selection, 0, contentOffset, contentWidth)
}

func (m *Model) renderSideQuestionOverlay(background string) string {
	panel := m.renderSideQuestionPanel()
	contentOffset := 2 // rounded border plus one cell of horizontal padding
	geometry := m.sideQuestionPanelGeometry()
	panelLines := strings.Split(panel, "\n")
	m.sideQuestion.selectionLines = m.sideQuestionSelectableLines(panelLines, contentOffset, geometry.bodyWidth)
	if m.selection.Active && m.selection.SideQuestion {
		panel = m.applySideQuestionSelection(panel, contentOffset, geometry.bodyWidth)
		panelLines = strings.Split(panel, "\n")
	}
	lines := strings.Split(background, "\n")
	for len(lines) < m.height {
		lines = append(lines, strings.Repeat(" ", max(1, m.width)))
	}
	x := max(0, (m.width-lipgloss.Width(panel))/2)
	y := max(0, (m.height-len(panelLines))/2)
	m.sideQuestion.panelY = y
	m.sideQuestion.panelContentX = x + contentOffset
	m.sideQuestion.panelHeight = len(panelLines)
	for i, panelLine := range panelLines {
		row := y + i
		if row >= len(lines) || x >= m.width {
			break
		}
		left := ansi.Cut(lines[row], 0, x)
		overlay := ansi.Cut(panelLine, 0, m.width-x)
		rightStart := x + lipgloss.Width(overlay)
		right := ansi.Cut(lines[row], rightStart, m.width)
		lines[row] = left + overlay + right
	}
	return strings.Join(lines, "\n")
}
