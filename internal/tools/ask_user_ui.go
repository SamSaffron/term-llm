package tools

import (
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/tuiutil"
	"golang.org/x/term"
)

// Theme colors (matching term-llm's existing theme)
var (
	askAccentColor = lipgloss.Color("10")  // green - matches term-llm primary
	askTextColor   = lipgloss.Color("15")  // white
	askMutedColor  = lipgloss.Color("245") // gray
	askBgColor     = lipgloss.Color("236") // dark gray for active tab
)

// Styles
var (
	// Container with left border accent (for interactive UI)
	askContainerStyle = tuiutil.AccentPanelStyle(askAccentColor)

	// Compact container for summary display (no vertical padding)
	askSummaryStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderLeft(true).
			BorderForeground(askAccentColor).
			PaddingLeft(1)

	// Tab styles - simple horizontal tabs
	askActiveTabStyle = lipgloss.NewStyle().
				Background(askAccentColor).
				Foreground(lipgloss.Color("0")). // black text on accent
				Padding(0, 1)

	askInactiveTabStyle = lipgloss.NewStyle().
				Background(askBgColor).
				Foreground(askMutedColor).
				Padding(0, 1)

	askAnsweredTabStyle = lipgloss.NewStyle().
				Background(askBgColor).
				Foreground(askTextColor).
				Padding(0, 1)

	// Question text
	askQuestionStyle = lipgloss.NewStyle().
				Foreground(askTextColor).
				MarginBottom(1)

	// Option styles
	askOptionStyle = lipgloss.NewStyle().
			Foreground(askTextColor)

	askSelectedOptionStyle = lipgloss.NewStyle().
				Foreground(askAccentColor)

	askDescriptionStyle = lipgloss.NewStyle().
				Foreground(askMutedColor).
				PaddingLeft(3)

	// Checkmark
	askCheckStyle = lipgloss.NewStyle().
			Foreground(askAccentColor)

	// Help bar
	askHelpStyle = tuiutil.MutedHelpStyle(askMutedColor)

	// Review/confirm styles
	askReviewLabelStyle = lipgloss.NewStyle().
				Foreground(askMutedColor)

	askReviewValueStyle = lipgloss.NewStyle().
				Foreground(askTextColor)

	askNotAnsweredStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("9")) // red
)

// askUIAnswer is the internal answer structure for the UI
type askUIAnswer struct {
	questionIndex int
	text          string
	selected      []int // indices of selected options for multi-select
	isCustom      bool
}

// AskUserModel is the bubbletea model for the ask_user UI.
// It can be embedded in a parent TUI for inline rendering.
type AskUserModel struct {
	questions  []AskUserQuestion
	answers    []askUIAnswer
	currentTab int
	cursor     int // Current selection within options
	textInput  textinput.Model
	width      int
	Done       bool
	Cancelled  bool
}

// askInputWidth returns the width used for the custom-answer text input.
func askInputWidth(width int) int {
	return max(10, min(50, width-10))
}

// IsDone returns true if the user has completed all questions.
func (m *AskUserModel) IsDone() bool {
	return m.Done
}

// IsCancelled returns true if the user cancelled the dialog.
func (m *AskUserModel) IsCancelled() bool {
	return m.Cancelled
}

// SetWidth updates the width for rendering.
func (m *AskUserModel) SetWidth(width int) {
	m.width = width
	m.textInput.SetWidth(askInputWidth(width))
}

// Answers returns the collected answers after the user completes the dialog.
func (m *AskUserModel) Answers() []AskUserAnswer {
	if !m.Done || m.Cancelled {
		return nil
	}

	answers := make([]AskUserAnswer, len(m.answers))
	for i, a := range m.answers {
		q := m.questions[i]
		if q.MultiSelect {
			var labels []string
			for _, idx := range a.selected {
				if idx >= 0 && idx < len(q.Options) {
					labels = append(labels, q.Options[idx].Label)
				}
			}
			answers[i] = AskUserAnswer{
				QuestionIndex: a.questionIndex,
				Header:        q.Header,
				Selected:      strings.Join(labels, ", "),
				SelectedList:  labels,
				IsCustom:      false,
				IsMultiSelect: true,
			}
		} else {
			answers[i] = AskUserAnswer{
				QuestionIndex: a.questionIndex,
				Header:        q.Header,
				Selected:      a.text,
				IsCustom:      a.isCustom,
				IsMultiSelect: false,
			}
		}
	}
	return answers
}

// applyAskInputStyles applies the ask-user textinput styles for both focus states.
func applyAskInputStyles(ti *textinput.Model) {
	ts := ti.Styles()
	ts.Focused.Prompt = lipgloss.NewStyle()
	ts.Focused.Text = lipgloss.NewStyle().Foreground(askTextColor)
	ts.Focused.Placeholder = lipgloss.NewStyle().Foreground(askMutedColor)
	ts.Blurred = ts.Focused
	ti.SetStyles(ts)
}

func newAskUserModelWithWidth(questions []AskUserQuestion, width int) *AskUserModel {
	ti := textinput.New()
	ti.Placeholder = "Type your own answer"
	ti.CharLimit = 500
	ti.SetWidth(askInputWidth(width))
	ti.Prompt = ""
	applyAskInputStyles(&ti)

	m := &AskUserModel{
		questions:  questions,
		answers:    make([]askUIAnswer, len(questions)),
		currentTab: 0,
		cursor:     0,
		textInput:  ti,
		width:      width,
	}

	for i := range m.answers {
		m.answers[i].questionIndex = i
	}

	return m
}

// NewEmbeddedAskUserModel creates an ask_user model for embedding in a parent TUI.
func NewEmbeddedAskUserModel(questions []AskUserQuestion, width int) *AskUserModel {
	return newAskUserModelWithWidth(questions, width)
}

// isOnCustomOption returns true if cursor is on "Type your own answer"
// Multi-select questions don't have a custom option
func (m *AskUserModel) isOnCustomOption() bool {
	if m.isOnConfirmTab() {
		return false
	}
	q := m.questions[m.currentTab]
	if q.MultiSelect {
		return false
	}
	return m.cursor == len(q.Options)
}

// isMultiSelect returns true if the current question allows multiple selections
func (m *AskUserModel) isMultiSelect() bool {
	if m.isOnConfirmTab() {
		return false
	}
	return m.questions[m.currentTab].MultiSelect
}

// isOptionSelected returns true if the option at idx is selected in multi-select mode
func (m *AskUserModel) isOptionSelected(idx int) bool {
	for _, sel := range m.answers[m.currentTab].selected {
		if sel == idx {
			return true
		}
	}
	return false
}

// toggleOption toggles the selection of the option at idx for multi-select
func (m *AskUserModel) toggleOption(idx int) {
	ans := &m.answers[m.currentTab]
	for i, sel := range ans.selected {
		if sel == idx {
			// Remove from selection
			ans.selected = append(ans.selected[:i], ans.selected[i+1:]...)
			return
		}
	}
	// Add to selection
	ans.selected = append(ans.selected, idx)
}

func newAskModel(questions []AskUserQuestion) *AskUserModel {
	return newAskUserModelWithWidth(questions, 80)
}

func (m *AskUserModel) Init() tea.Cmd {
	return nil
}

func (m *AskUserModel) isAnswered(idx int) bool {
	if m.questions[idx].MultiSelect {
		return len(m.answers[idx].selected) > 0
	}
	return m.answers[idx].text != ""
}

func (m *AskUserModel) allAnswered() bool {
	for i := range m.answers {
		if !m.isAnswered(i) {
			return false
		}
	}
	return true
}

func (m *AskUserModel) isSingleQuestion() bool {
	return len(m.questions) == 1
}

func (m *AskUserModel) isOnConfirmTab() bool {
	return !m.isSingleQuestion() && m.currentTab == len(m.questions)
}

func (m *AskUserModel) totalTabs() int {
	if m.isSingleQuestion() {
		return 1
	}
	return len(m.questions) + 1 // questions + confirm tab
}

type askUpdateMode int

const (
	askUpdateStandalone askUpdateMode = iota
	askUpdateEmbedded
)

func (m *AskUserModel) nextUnansweredTab() int {
	for i := m.currentTab + 1; i < len(m.questions); i++ {
		if !m.isAnswered(i) {
			return i
		}
	}
	for i := 0; i < m.currentTab; i++ {
		if !m.isAnswered(i) {
			return i
		}
	}
	return len(m.questions)
}

func (m *AskUserModel) switchTabTo(newTab int) {
	m.currentTab = newTab
	m.cursor = 0
	m.textInput.Blur()
	m.textInput.SetValue("")
}

func (m *AskUserModel) advanceToNext(mode askUpdateMode) (tea.Cmd, bool) {
	if m.isSingleQuestion() {
		m.Done = true
		return nil, true
	}
	m.switchTabTo(m.nextUnansweredTab())
	return nil, false
}

func (m *AskUserModel) moveQuestionCursor(delta int, totalOptions int) tea.Cmd {
	m.cursor = (m.cursor + delta + totalOptions) % totalOptions
	if !m.isMultiSelect() && m.cursor == len(m.questions[m.currentTab].Options) {
		m.textInput.Focus()
		return textinput.Blink
	}
	return nil
}

func (m *AskUserModel) handleCustomInputMessage(msg tea.KeyPressMsg, mode askUpdateMode) (tea.Cmd, bool) {
	switch msg.String() {
	case "enter":
		text := strings.TrimSpace(m.textInput.Value())
		if text == "" {
			return nil, false
		}
		m.answers[m.currentTab].text = text
		m.answers[m.currentTab].isCustom = true
		m.textInput.Blur()
		return m.advanceToNext(mode)
	case "up":
		q := m.questions[m.currentTab]
		m.cursor = len(q.Options) - 1
		m.textInput.Blur()
		return nil, false
	case "tab":
		m.switchTabTo((m.currentTab + 1) % m.totalTabs())
		return nil, false
	case "shift+tab":
		m.switchTabTo((m.currentTab - 1 + m.totalTabs()) % m.totalTabs())
		return nil, false
	default:
		var cmd tea.Cmd
		m.textInput, cmd = m.textInput.Update(msg)
		return cmd, false
	}
}

func (m *AskUserModel) handleQuestionMessage(msg tea.KeyPressMsg, mode askUpdateMode) (tea.Cmd, bool) {
	q := m.questions[m.currentTab]
	totalOptions := len(q.Options)
	if !q.MultiSelect {
		totalOptions++
	}

	switch msg.String() {
	case "up", "k":
		return m.moveQuestionCursor(-1, totalOptions), false
	case "down", "j":
		return m.moveQuestionCursor(1, totalOptions), false
	case " ", "space":
		if q.MultiSelect {
			m.toggleOption(m.cursor)
			return nil, false
		}
		m.answers[m.currentTab].text = q.Options[m.cursor].Label
		m.answers[m.currentTab].isCustom = false
		return m.advanceToNext(mode)
	case "enter":
		if q.MultiSelect {
			if m.isSingleQuestion() && m.isAnswered(m.currentTab) {
				m.Done = true
				return nil, true
			}
			m.toggleOption(m.cursor)
			return nil, false
		}
		m.answers[m.currentTab].text = q.Options[m.cursor].Label
		m.answers[m.currentTab].isCustom = false
		return m.advanceToNext(mode)
	}

	return nil, false
}

func (m *AskUserModel) applyMessage(msg tea.Msg, mode askUpdateMode) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetWidth(msg.Width)
		return nil, false

	case tea.PasteMsg:
		if !m.isOnCustomOption() {
			return nil, false
		}
		var cmd tea.Cmd
		m.textInput, cmd = m.textInput.Update(msg)
		return cmd, false

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			m.Cancelled = true
			return nil, true
		}

		if m.isOnCustomOption() {
			return m.handleCustomInputMessage(msg, mode)
		}

		switch msg.String() {
		case "left", "h":
			m.switchTabTo((m.currentTab - 1 + m.totalTabs()) % m.totalTabs())
			return nil, false
		case "right", "l", "tab":
			m.switchTabTo((m.currentTab + 1) % m.totalTabs())
			return nil, false
		case "shift+tab":
			m.switchTabTo((m.currentTab - 1 + m.totalTabs()) % m.totalTabs())
			return nil, false
		}

		if m.isOnConfirmTab() {
			if msg.String() == "enter" && m.allAnswered() {
				m.Done = true
				return nil, true
			}
			return nil, false
		}

		return m.handleQuestionMessage(msg, mode)
	}

	return nil, false
}

// Update handles messages for standalone tea.Program use (calls tea.Quit on completion).
func (m *AskUserModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd, done := m.applyMessage(msg, askUpdateStandalone)
	if done {
		return m, tea.Quit
	}
	return m, cmd
}

// UpdateEmbedded handles messages for embedded use (does not call tea.Quit).
// Returns a tea.Cmd if one is needed (e.g., for text input blinking).
func (m *AskUserModel) UpdateEmbedded(msg tea.Msg) tea.Cmd {
	cmd, _ := m.applyMessage(msg, askUpdateEmbedded)
	return cmd
}

func (m *AskUserModel) View() tea.View {
	if m.Done {
		return tea.NewView("")
	}

	var b strings.Builder

	if !m.isSingleQuestion() {
		b.WriteString(m.renderTabs())
		b.WriteString("\n\n")
	}

	if m.isOnConfirmTab() {
		b.WriteString(m.renderConfirm())
	} else {
		b.WriteString(m.renderQuestion())
	}

	b.WriteString("\n")
	b.WriteString(m.renderHelp())

	style := askContainerStyle.Width(m.width)
	return tea.NewView(style.Render(b.String()))
}

func (m *AskUserModel) renderTabs() string {
	var tabs []string

	for i, q := range m.questions {
		isActive := i == m.currentTab
		isAnswered := m.isAnswered(i)

		var style lipgloss.Style
		if isActive {
			style = askActiveTabStyle
		} else if isAnswered {
			style = askAnsweredTabStyle
		} else {
			style = askInactiveTabStyle
		}

		tabs = append(tabs, style.Render(q.Header))
	}

	// Confirm tab
	isActive := m.isOnConfirmTab()
	var style lipgloss.Style
	if isActive {
		style = askActiveTabStyle
	} else {
		style = askInactiveTabStyle
	}
	tabs = append(tabs, style.Render("Confirm"))

	return strings.Join(tabs, " ")
}

func (m *AskUserModel) renderQuestion() string {
	var b strings.Builder

	// innerWidth accounts for border (1) + paddingLeft (1) + paddingRight (2) = 4
	innerWidth := m.width - 4
	if innerWidth < 20 {
		innerWidth = 20
	}

	q := m.questions[m.currentTab]

	// Question text
	b.WriteString(askQuestionStyle.Render(wordwrap.String(q.Question, innerWidth)))
	b.WriteString("\n")

	// Options
	for i, opt := range q.Options {
		isSelected := m.cursor == i

		var optLine strings.Builder
		style := askOptionStyle
		if isSelected {
			style = askSelectedOptionStyle
		}

		if q.MultiSelect {
			// Multi-select: show checkbox
			isChecked := m.isOptionSelected(i)
			checkbox := "[ ]"
			if isChecked {
				checkbox = "[✓]"
			}
			optLine.WriteString(style.Render(wordwrap.String(fmt.Sprintf("%d. %s %s", i+1, checkbox, opt.Label), innerWidth)))
		} else {
			// Single-select: show checkmark when picked
			isPicked := m.isAnswered(m.currentTab) && m.answers[m.currentTab].text == opt.Label && !m.answers[m.currentTab].isCustom
			optLine.WriteString(style.Render(wordwrap.String(fmt.Sprintf("%d. %s", i+1, opt.Label), innerWidth)))
			if isPicked {
				optLine.WriteString(" ")
				optLine.WriteString(askCheckStyle.Render("✓"))
			}
		}
		b.WriteString(optLine.String())
		b.WriteString("\n")

		// Description (askDescriptionStyle has PaddingLeft(3), so reduce wrap width)
		if opt.Description != "" {
			b.WriteString(askDescriptionStyle.Render(wordwrap.String(opt.Description, innerWidth-3)))
			b.WriteString("\n")
		}
	}

	// Multi-select: no custom option
	if q.MultiSelect {
		return b.String()
	}

	// "Type your own answer" option - show input inline (single-select only)
	isOnCustom := m.isOnCustomOption()
	isPicked := m.isAnswered(m.currentTab) && m.answers[m.currentTab].isCustom

	if isOnCustom {
		// Show the text input with prompt
		b.WriteString(askSelectedOptionStyle.Render(fmt.Sprintf("%d. ", len(q.Options)+1)))
		b.WriteString(m.textInput.View())
		if isPicked {
			b.WriteString(" ")
			b.WriteString(askCheckStyle.Render("✓"))
		}
		b.WriteString("\n")
	} else {
		// Show as regular option
		style := askOptionStyle
		var optLine strings.Builder
		optLine.WriteString(style.Render(fmt.Sprintf("%d. Type your own answer", len(q.Options)+1)))
		if isPicked {
			optLine.WriteString(" ")
			optLine.WriteString(askCheckStyle.Render("✓"))
		}
		b.WriteString(optLine.String())
		b.WriteString("\n")
		// Show previously entered custom answer below
		if isPicked {
			b.WriteString(askDescriptionStyle.Render(m.answers[m.currentTab].text))
			b.WriteString("\n")
		}
	}

	return b.String()
}

func (m *AskUserModel) renderConfirm() string {
	var b strings.Builder

	b.WriteString(askQuestionStyle.Render("Review your answers"))
	b.WriteString("\n")

	for i, q := range m.questions {
		answered := m.isAnswered(i)

		b.WriteString(askReviewLabelStyle.Render(q.Header + ": "))
		if answered {
			if q.MultiSelect {
				// Build comma-separated list of selected options
				var labels []string
				for _, idx := range m.answers[i].selected {
					if idx >= 0 && idx < len(q.Options) {
						labels = append(labels, q.Options[idx].Label)
					}
				}
				b.WriteString(askReviewValueStyle.Render(strings.Join(labels, ", ")))
			} else {
				b.WriteString(askReviewValueStyle.Render(m.answers[i].text))
			}
		} else {
			b.WriteString(askNotAnsweredStyle.Render("(not answered)"))
		}
		b.WriteString("\n")
	}

	if m.allAnswered() {
		b.WriteString("\n")
		b.WriteString(askOptionStyle.Render("Press Enter to submit"))
	} else {
		b.WriteString("\n")
		b.WriteString(askNotAnsweredStyle.Render("Answer all questions to submit"))
	}

	return b.String()
}

func (m *AskUserModel) renderHelp() string {
	var parts []string

	if !m.isSingleQuestion() {
		parts = append(parts, "tab next")
	}

	if !m.isOnConfirmTab() {
		parts = append(parts, "↑↓ select")
	}

	if m.isOnConfirmTab() {
		parts = append(parts, "enter submit")
	} else if m.isMultiSelect() {
		parts = append(parts, "space toggle")
		if m.isSingleQuestion() {
			parts = append(parts, "enter submit")
		}
	} else if m.isOnCustomOption() {
		parts = append(parts, "enter confirm")
	} else {
		parts = append(parts, "enter select")
	}

	parts = append(parts, "esc dismiss")

	return askHelpStyle.Render(strings.Join(parts, "  "))
}

// RenderSummary returns a styled summary of the user's choices.
func (m *AskUserModel) RenderSummary() string {
	var b strings.Builder

	for i, q := range m.questions {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(askCheckStyle.Render("✓ "))
		b.WriteString(askReviewLabelStyle.Render(q.Question + " "))
		if q.MultiSelect {
			// Build comma-separated list of selected options
			var labels []string
			for _, idx := range m.answers[i].selected {
				if idx >= 0 && idx < len(q.Options) {
					labels = append(labels, q.Options[idx].Label)
				}
			}
			b.WriteString(askReviewValueStyle.Render(strings.Join(labels, ", ")))
		} else {
			b.WriteString(askReviewValueStyle.Render(m.answers[i].text))
		}
	}

	// Return styled container with trailing newlines for separation
	return askSummaryStyle.Render(b.String()) + "\n\n"
}

// RenderPlainSummary returns a plain text summary (styling applied at display time).
func (m *AskUserModel) RenderPlainSummary() string {
	var parts []string
	for i, q := range m.questions {
		answer := m.answers[i].text
		if q.MultiSelect {
			var labels []string
			for _, idx := range m.answers[i].selected {
				if idx >= 0 && idx < len(q.Options) {
					labels = append(labels, q.Options[idx].Label)
				}
			}
			answer = strings.Join(labels, ", ")
		}
		parts = append(parts, fmt.Sprintf("%s: %s", q.Header, answer))
	}
	return strings.Join(parts, " | ")
}

// getTTY opens /dev/tty for direct terminal access
func getAskUserTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// RunAskUser presents the questions to the user and returns their answers.
func RunAskUser(questions []AskUserQuestion) ([]AskUserAnswer, error) {
	tty, err := getAskUserTTY()
	if err != nil {
		return nil, fmt.Errorf("no TTY available: %w", err)
	}
	defer tty.Close()

	// Get terminal width
	width := 80
	if w, _, err := term.GetSize(int(tty.Fd())); err == nil && w > 0 {
		width = w
	}

	m := newAskModel(questions)
	m.width = width

	p := tea.NewProgram(m, tea.WithInput(tty), tea.WithOutput(tty))

	finalModel, err := p.Run()
	if err != nil {
		return nil, err
	}

	result := finalModel.(*AskUserModel)
	if result.Cancelled {
		return nil, fmt.Errorf("cancelled by user")
	}

	// Store the summary for the TUI to display via the segment system.
	// This replaces direct TTY printing which gets overwritten when the main TUI redraws.
	// Use plain text summary - styling is applied at render time to avoid ANSI corruption.
	SetLastAskUserResult(result.RenderPlainSummary())

	answers := result.Answers()
	if answers == nil {
		return nil, fmt.Errorf("ask_user finished without answers")
	}
	return answers, nil
}
