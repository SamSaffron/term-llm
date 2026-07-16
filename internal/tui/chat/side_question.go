package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/sidequestion"
	"github.com/samsaffron/term-llm/internal/ui"
)

const sideQuestionStopTimeout = 250 * time.Millisecond

type SideQuestionState struct {
	Visible      bool
	Running      bool
	Question     string
	Response     strings.Builder
	Synthetic    bool
	Selected     int
	History      []sidequestion.Entry
	Cancel       context.CancelFunc
	Done         chan struct{}
	Generation   uint64
	Usage        llm.Usage
	Err          error
	Scroll       int
	ConfirmClear bool
	events       chan sideQuestionEventMsg
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
	messages := m.buildMessages()
	// While the main stream is active, buildMessages contains the submitted user
	// turn but not a completed assistant response. A side question sees the last
	// completed main boundary, so exclude that entire active turn.
	if m.streaming {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == llm.RoleUser {
				messages = messages[:i]
				break
			}
		}
	}
	return sidequestion.PrepareContextSnapshot(messages)
}

func (m *Model) clearSideSubmittedCommand() {
	m.setTextareaValue("")
	if m.completions != nil {
		m.completions.Hide()
	}
}

func (m *Model) cmdSide(question string) (tea.Model, tea.Cmd) {
	question = strings.TrimSpace(question)
	if question == "" {
		if len(m.sideQuestion.History) == 0 {
			return m.showSystemMessage("Usage: /side <question>")
		}
		m.sideQuestion.Visible = true
		m.sideQuestion.Selected = len(m.sideQuestion.History) - 1
		m.loadSelectedSideEntry()
		m.clearSideSubmittedCommand()
		return m, nil
	}
	if m.sideQuestion.Running {
		m.sideQuestion.Visible = true
		m.sideQuestion.Err = errors.New("A side question is already running")
		return m, nil
	}
	if m.sideQuestion.Done != nil {
		select {
		case <-m.sideQuestion.Done:
			m.sideQuestion.Done = nil
		default:
			m.sideQuestion.Visible = true
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
	m.clearSideSubmittedCommand()

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
	m.sideQuestion.Selected = len(m.sideQuestion.History)
	m.sideQuestion.events = make(chan sideQuestionEventMsg, 64)
	done := make(chan struct{})
	m.sideQuestion.Done = done
	events := m.sideQuestion.events
	history := append([]sidequestion.Entry(nil), m.sideQuestion.History...)
	reasoningEffort := ""
	reasoningMode := ""
	if m.sess != nil {
		reasoningEffort = strings.TrimSpace(m.sess.ReasoningEffort)
		reasoningMode = strings.TrimSpace(m.sess.ReasoningMode)
	}
	req := llm.Request{
		Model:           m.modelName,
		Messages:        sidequestion.BuildMessages(m.sideSnapshot(), history, question),
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
			if len(events) < cap(events)-1 {
				events <- sideQuestionEventMsg{generation: generation, event: event}
			}
		})
		select {
		case events <- sideQuestionEventMsg{generation: generation, result: &result, err: runErr}:
		case <-ctx.Done():
		}
	}()
	return m, m.listenSideQuestion(events)
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
		if errors.Is(msg.err, context.Canceled) {
			m.sideQuestion.Response.Reset()
			m.sideQuestion.Visible = false
			return nil
		}
		if msg.err != nil {
			m.sideQuestion.Err = msg.err
			return nil
		}
		m.sideQuestion.Response.Reset()
		m.sideQuestion.Response.WriteString(msg.result.Response)
		m.sideQuestion.Synthetic = msg.result.Synthetic
		m.sideQuestion.Usage = msg.result.Usage
		if m.stats != nil {
			m.stats.AddUsage(msg.result.Usage.InputTokens, msg.result.Usage.OutputTokens, msg.result.Usage.CachedInputTokens, msg.result.Usage.CacheWriteTokens)
		}
		if !msg.result.Synthetic && strings.TrimSpace(msg.result.Response) != "" {
			m.sideQuestion.History = sidequestion.AppendHistory(m.sideQuestion.History, sidequestion.Entry{
				Question: m.sideQuestion.Question, Response: msg.result.Response,
				CreatedAt: time.Now(), Usage: msg.result.Usage,
			})
			m.sideQuestion.Selected = len(m.sideQuestion.History) - 1
		}
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
		timer := time.NewTimer(sideQuestionStopTimeout)
		select {
		case <-done:
			timer.Stop()
			m.sideQuestion.Done = nil
		case <-timer.C:
		}
	}
	m.sideQuestion.Generation++
	m.sideQuestion.Running = false
	m.sideQuestion.Cancel = nil
	m.sideQuestion.Response.Reset()
	m.sideQuestion.Visible = false
	m.sideQuestion.Err = nil
}

func (m *Model) clearSideQuestionHistory() {
	if m.sideQuestion.Running || m.sideQuestion.Done != nil {
		m.cancelSideQuestion()
	}
	m.sideQuestion.History = nil
	m.sideQuestion.Selected = 0
	m.sideQuestion.Question = ""
	m.sideQuestion.Response.Reset()
	m.sideQuestion.Synthetic = false
	m.sideQuestion.Usage = llm.Usage{}
	m.sideQuestion.Err = nil
	m.sideQuestion.Scroll = 0
	m.sideQuestion.Visible = false
	m.sideQuestion.ConfirmClear = false
}

func (m *Model) loadSelectedSideEntry() {
	if m.sideQuestion.Selected < 0 || m.sideQuestion.Selected >= len(m.sideQuestion.History) {
		return
	}
	entry := m.sideQuestion.History[m.sideQuestion.Selected]
	m.sideQuestion.Question = entry.Question
	m.sideQuestion.Response.Reset()
	m.sideQuestion.Response.WriteString(entry.Response)
	m.sideQuestion.Usage = entry.Usage
	m.sideQuestion.Err = nil
	m.sideQuestion.Scroll = 0
}

func (m *Model) handleSideQuestionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	keyName := strings.ToLower(msg.String())
	if m.sideQuestion.ConfirmClear && keyName != "x" {
		m.sideQuestion.ConfirmClear = false
	}
	switch keyName {
	case "esc":
		if m.sideQuestion.Running {
			m.cancelSideQuestion()
		} else {
			m.sideQuestion.Visible = false
		}
	case "enter", " ", "space":
		if !m.sideQuestion.Running {
			m.sideQuestion.Visible = false
		}
	case "left":
		if !m.sideQuestion.Running && m.sideQuestion.Selected > 0 {
			m.sideQuestion.Selected--
			m.loadSelectedSideEntry()
		}
	case "right":
		if !m.sideQuestion.Running && m.sideQuestion.Selected+1 < len(m.sideQuestion.History) {
			m.sideQuestion.Selected++
			m.loadSelectedSideEntry()
		}
	case "up":
		m.sideQuestion.Scroll++
	case "down":
		if m.sideQuestion.Scroll > 0 {
			m.sideQuestion.Scroll--
		}
	case "c":
		if !m.sideQuestion.Running {
			_ = clipboard.CopyTextOSC52(m.sideQuestion.Response.String())
		}
	case "x":
		if !m.sideQuestion.Running {
			if m.sideQuestion.ConfirmClear {
				m.clearSideQuestionHistory()
			} else {
				m.sideQuestion.ConfirmClear = true
			}
		}
	}
	return m, nil
}

type sideQuestionPanelSize struct {
	width        int
	bodyWidth    int
	responseRows int
}

func (m *Model) sideQuestionPanelGeometry() sideQuestionPanelSize {
	// Leave the underlying conversation visible around the panel while making
	// the response a useful reading surface on normal terminals. The clamps keep
	// very large terminals comfortable and let small terminals use every cell.
	width := min(120, max(64, m.width-8))
	width = min(width, max(1, m.width))
	bodyWidth := max(1, width-4) // border plus one cell of padding on each side
	responseRows := min(40, max(1, m.height-10))
	return sideQuestionPanelSize{width: width, bodyWidth: bodyWidth, responseRows: responseRows}
}

func (m *Model) renderSideQuestionResponse(width int) []string {
	response := m.sideQuestion.Response.String()
	if response == "" && m.sideQuestion.Running {
		response = "Thinking…"
	}
	var rendered string
	if response != "" {
		rendered = ui.RenderMarkdownWithOptions(response, width, ui.MarkdownRenderOptions{
			WrapOffset:        0,
			NormalizeTabs:     true,
			NormalizeNewlines: false,
		})
	}
	if m.sideQuestion.Err != nil {
		if rendered != "" {
			rendered += "\n\n"
		}
		rendered += wordwrap.String(m.sideQuestion.Err.Error(), width)
	}
	return strings.Split(strings.Trim(rendered, "\n"), "\n")
}

func sideQuestionFooter(running, confirmClear bool, width int) string {
	footer := "Esc cancel"
	if !running {
		switch {
		case width >= 64:
			footer = "Esc/Enter close · ←/→ history · ↑/↓ scroll · c copy · x clear"
		case width >= 36:
			footer = "Esc close · ←/→ history · ↑/↓ scroll"
		default:
			footer = "Esc close · ↑/↓ scroll"
		}
	}
	if confirmClear {
		footer = "Press x again to clear side history"
	}
	return ansi.Truncate(footer, width, "…")
}

func (m *Model) renderSideQuestionPanel() string {
	geometry := m.sideQuestionPanelGeometry()
	lines := m.renderSideQuestionResponse(geometry.bodyWidth)
	maxScroll := max(0, len(lines)-geometry.responseRows)
	scroll := min(maxScroll, max(0, m.sideQuestion.Scroll))
	start := maxScroll - scroll
	end := min(len(lines), start+geometry.responseRows)
	visibleLines := append([]string(nil), lines[start:end]...)
	for len(visibleLines) < geometry.responseRows {
		visibleLines = append(visibleLines, "")
	}
	visible := strings.Join(visibleLines, "\n")
	status := "done"
	if m.sideQuestion.Running {
		status = "answering"
	}
	attention := ""
	if m.approvalModel != nil || m.approvalDoneCh != nil {
		attention = " · main needs approval"
	} else if m.askUserModel != nil || m.askUserDoneCh != nil {
		attention = " · main needs input"
	}
	position := ""
	if len(m.sideQuestion.History) > 1 && !m.sideQuestion.Running {
		position = fmt.Sprintf(" · %d/%d", m.sideQuestion.Selected+1, len(m.sideQuestion.History))
	}
	header := ansi.Truncate("Side question · "+status+position+attention, geometry.bodyWidth, "…")
	footer := sideQuestionFooter(m.sideQuestion.Running, m.sideQuestion.ConfirmClear, geometry.bodyWidth)
	content := fmt.Sprintf("%s\n\n%s\n\n%s", header, visible, footer)
	return m.styles.TableBorder.Border(lipgloss.RoundedBorder()).Width(geometry.bodyWidth).Padding(0, 1).Render(content)
}

func (m *Model) renderSideQuestionOverlay(background string) string {
	panel := m.renderSideQuestionPanel()
	lines := strings.Split(background, "\n")
	for len(lines) < m.height {
		lines = append(lines, strings.Repeat(" ", max(1, m.width)))
	}
	panelLines := strings.Split(panel, "\n")
	x := max(0, (m.width-lipgloss.Width(panel))/2)
	y := max(0, (m.height-len(panelLines))/2)
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
