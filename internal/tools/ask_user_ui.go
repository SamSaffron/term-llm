package tools

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	askContainerStyle = lipgloss.NewStyle().
				BorderStyle(lipgloss.NormalBorder()).
				BorderLeft(true).
				BorderForeground(askAccentColor).
				PaddingLeft(1).
				PaddingRight(2).
				PaddingTop(1).
				PaddingBottom(1)

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
	askHelpStyle = lipgloss.NewStyle().
			Foreground(askMutedColor).
			MarginTop(1)

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
	m.textInput.Width = min(50, width-10)
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

// NewEmbeddedAskUserModel creates an ask_user model for embedding in a parent TUI.
func NewEmbeddedAskUserModel(questions []AskUserQuestion, width int) *AskUserModel {
	ti := textinput.New()
	ti.Placeholder = "Type your own answer"
	ti.CharLimit = 500
	ti.Width = min(50, width-10)
	ti.PromptStyle = lipgloss.NewStyle()
	ti.TextStyle = lipgloss.NewStyle().Foreground(askTextColor)
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(askMutedColor)
	ti.Prompt = ""

	m := &AskUserModel{
		questions:  questions,
		answers:    make([]askUIAnswer, len(questions)),
		currentTab: 0,
		cursor:     0,
		textInput:  ti,
		width:      width,
	}

	// Initialize answer indices
	for i := range m.answers {
		m.answers[i].questionIndex = i
	}

	return m
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
	ti := textinput.New()
	ti.Placeholder = "Type your own answer"
	ti.CharLimit = 500
	ti.Width = 50
	ti.PromptStyle = lipgloss.NewStyle()
	ti.TextStyle = lipgloss.NewStyle().Foreground(askTextColor)
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(askMutedColor)
	ti.Prompt = ""

	m := &AskUserModel{
		questions:  questions,
		answers:    make([]askUIAnswer, len(questions)),
		currentTab: 0,
		cursor:     0,
		textInput:  ti,
		width:      80,
	}

	// Initialize answer indices
	for i := range m.answers {
		m.answers[i].questionIndex = i
	}

	return m
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

// Update handles messages for standalone tea.Program use (calls tea.Quit on completion).
func (m *AskUserModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textInput.Width = min(50, m.width-10)

	case tea.KeyMsg:
		// Handle cancel
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			m.Cancelled = true
			return m, tea.Quit
		}

		// When on custom input option, handle text input
		if m.isOnCustomOption() {
			switch msg.String() {
			case "enter":
				text := strings.TrimSpace(m.textInput.Value())
				if text != "" {
					m.answers[m.currentTab].text = text
					m.answers[m.currentTab].isCustom = true
					m.textInput.Blur()
					return m.advanceToNext()
				}
				return m, nil
			case "up":
				// Move up from custom option (only arrow key, not 'k' which should type)
				q := m.questions[m.currentTab]
				m.cursor = len(q.Options) - 1
				m.textInput.Blur()
				return m, nil
			case "tab":
				// Tab to next question
				newTab := (m.currentTab + 1) % m.totalTabs()
				return m.switchTab(newTab)
			case "shift+tab":
				newTab := (m.currentTab - 1 + m.totalTabs()) % m.totalTabs()
				return m.switchTab(newTab)
			}
			// Pass other keys to text input
			var cmd tea.Cmd
			m.textInput, cmd = m.textInput.Update(msg)
			return m, cmd
		}

		// Tab navigation (when not on custom option)
		switch msg.String() {
		case "left", "h":
			newTab := (m.currentTab - 1 + m.totalTabs()) % m.totalTabs()
			return m.switchTab(newTab)
		case "right", "l":
			newTab := (m.currentTab + 1) % m.totalTabs()
			return m.switchTab(newTab)
		case "tab":
			newTab := (m.currentTab + 1) % m.totalTabs()
			return m.switchTab(newTab)
		case "shift+tab":
			newTab := (m.currentTab - 1 + m.totalTabs()) % m.totalTabs()
			return m.switchTab(newTab)
		}

		// On confirm tab
		if m.isOnConfirmTab() {
			if msg.String() == "enter" {
				if m.allAnswered() {
					m.Done = true
					return m, tea.Quit
				}
			}
			return m, nil
		}

		// Question navigation
		q := m.questions[m.currentTab]
		// Multi-select questions don't have "Type your own" option
		totalOptions := len(q.Options)
		if !q.MultiSelect {
			totalOptions++ // +1 for "Type your own"
		}

		switch msg.String() {
		case "up", "k":
			m.cursor = (m.cursor - 1 + totalOptions) % totalOptions
			// If moved to custom option (single-select only), focus the input
			if !q.MultiSelect && m.cursor == len(q.Options) {
				m.textInput.Focus()
				return m, textinput.Blink
			}
		case "down", "j":
			m.cursor = (m.cursor + 1) % totalOptions
			// If moved to custom option (single-select only), focus the input
			if !q.MultiSelect && m.cursor == len(q.Options) {
				m.textInput.Focus()
				return m, textinput.Blink
			}
		case "enter", " ":
			if q.MultiSelect {
				// Multi-select: toggle the current option
				m.toggleOption(m.cursor)
				return m, nil
			}
			// Single-select: select and advance
			m.answers[m.currentTab].text = q.Options[m.cursor].Label
			m.answers[m.currentTab].isCustom = false
			return m.advanceToNext()
		}
	}

	return m, nil
}

// UpdateEmbedded handles messages for embedded use (does not call tea.Quit).
// Returns a tea.Cmd if one is needed (e.g., for text input blinking).
func (m *AskUserModel) UpdateEmbedded(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textInput.Width = min(50, m.width-10)

	case tea.KeyMsg:
		// Handle cancel
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			m.Cancelled = true
			return nil
		}

		// When on custom input option, handle text input
		if m.isOnCustomOption() {
			switch msg.String() {
			case "enter":
				text := strings.TrimSpace(m.textInput.Value())
				if text != "" {
					m.answers[m.currentTab].text = text
					m.answers[m.currentTab].isCustom = true
					m.textInput.Blur()
					return m.advanceToNextEmbedded()
				}
				return nil
			case "up":
				// Move up from custom option (only arrow key, not 'k' which should type)
				q := m.questions[m.currentTab]
				m.cursor = len(q.Options) - 1
				m.textInput.Blur()
				return nil
			case "tab":
				// Tab to next question
				newTab := (m.currentTab + 1) % m.totalTabs()
				m.switchTabEmbedded(newTab)
				return nil
			case "shift+tab":
				newTab := (m.currentTab - 1 + m.totalTabs()) % m.totalTabs()
				m.switchTabEmbedded(newTab)
				return nil
			}
			// Pass other keys to text input
			var cmd tea.Cmd
			m.textInput, cmd = m.textInput.Update(msg)
			return cmd
		}

		// Tab navigation (when not on custom option)
		switch msg.String() {
		case "left", "h":
			newTab := (m.currentTab - 1 + m.totalTabs()) % m.totalTabs()
			m.switchTabEmbedded(newTab)
			return nil
		case "right", "l":
			newTab := (m.currentTab + 1) % m.totalTabs()
			m.switchTabEmbedded(newTab)
			return nil
		case "tab":
			newTab := (m.currentTab + 1) % m.totalTabs()
			m.switchTabEmbedded(newTab)
			return nil
		case "shift+tab":
			newTab := (m.currentTab - 1 + m.totalTabs()) % m.totalTabs()
			m.switchTabEmbedded(newTab)
			return nil
		}

		// On confirm tab
		if m.isOnConfirmTab() {
			if msg.String() == "enter" {
				if m.allAnswered() {
					m.Done = true
				}
			}
			return nil
		}

		// Question navigation
		q := m.questions[m.currentTab]
		// Multi-select questions don't have "Type your own" option
		totalOptions := len(q.Options)
		if !q.MultiSelect {
			totalOptions++ // +1 for "Type your own"
		}

		switch msg.String() {
		case "up", "k":
			m.cursor = (m.cursor - 1 + totalOptions) % totalOptions
			// If moved to custom option (single-select only), focus the input
			if !q.MultiSelect && m.cursor == len(q.Options) {
				m.textInput.Focus()
				return textinput.Blink
			}
		case "down", "j":
			m.cursor = (m.cursor + 1) % totalOptions
			// If moved to custom option (single-select only), focus the input
			if !q.MultiSelect && m.cursor == len(q.Options) {
				m.textInput.Focus()
				return textinput.Blink
			}
		case "enter", " ":
			if q.MultiSelect {
				// Multi-select: toggle the current option
				m.toggleOption(m.cursor)
				return nil
			}
			// Single-select: select and advance
			m.answers[m.currentTab].text = q.Options[m.cursor].Label
			m.answers[m.currentTab].isCustom = false
			return m.advanceToNextEmbedded()
		}
	}

	return nil
}

func (m *AskUserModel) switchTab(newTab int) (tea.Model, tea.Cmd) {
	m.currentTab = newTab
	m.cursor = 0
	m.textInput.Blur()
	m.textInput.SetValue("")
	return m, nil
}

func (m *AskUserModel) switchTabEmbedded(newTab int) {
	m.currentTab = newTab
	m.cursor = 0
	m.textInput.Blur()
	m.textInput.SetValue("")
}

func (m *AskUserModel) advanceToNext() (tea.Model, tea.Cmd) {
	// Single question - submit immediately
	if m.isSingleQuestion() {
		m.Done = true
		return m, tea.Quit
	}

	// Find next unanswered question
	for i := m.currentTab + 1; i < len(m.questions); i++ {
		if !m.isAnswered(i) {
			return m.switchTab(i)
		}
	}
	// Check from beginning
	for i := 0; i < m.currentTab; i++ {
		if !m.isAnswered(i) {
			return m.switchTab(i)
		}
	}
	// All answered - go to confirm tab
	return m.switchTab(len(m.questions))
}

func (m *AskUserModel) advanceToNextEmbedded() tea.Cmd {
	// Single question - submit immediately
	if m.isSingleQuestion() {
		m.Done = true
		return nil
	}

	// Find next unanswered question
	for i := m.currentTab + 1; i < len(m.questions); i++ {
		if !m.isAnswered(i) {
			m.switchTabEmbedded(i)
			return nil
		}
	}
	// Check from beginning
	for i := 0; i < m.currentTab; i++ {
		if !m.isAnswered(i) {
			m.switchTabEmbedded(i)
			return nil
		}
	}
	// All answered - go to confirm tab
	m.switchTabEmbedded(len(m.questions))
	return nil
}

func (m *AskUserModel) View() string {
	if m.Done {
		// Summary is printed separately after program quits to persist through TUI redraw
		return ""
	}

	var b strings.Builder

	// Tabs (only show if multiple questions)
	if !m.isSingleQuestion() {
		b.WriteString(m.renderTabs())
		b.WriteString("\n\n")
	}

	// Content
	if m.isOnConfirmTab() {
		b.WriteString(m.renderConfirm())
	} else {
		b.WriteString(m.renderQuestion())
	}

	// Help bar
	b.WriteString("\n")
	b.WriteString(m.renderHelp())

	return askContainerStyle.Render(b.String())
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

	q := m.questions[m.currentTab]

	// Question text
	b.WriteString(askQuestionStyle.Render(q.Question))
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
			optLine.WriteString(style.Render(fmt.Sprintf("%d. %s %s", i+1, checkbox, opt.Label)))
		} else {
			// Single-select: show checkmark when picked
			isPicked := m.isAnswered(m.currentTab) && m.answers[m.currentTab].text == opt.Label && !m.answers[m.currentTab].isCustom
			optLine.WriteString(style.Render(fmt.Sprintf("%d. %s", i+1, opt.Label)))
			if isPicked {
				optLine.WriteString(" ")
				optLine.WriteString(askCheckStyle.Render("✓"))
			}
		}
		b.WriteString(optLine.String())
		b.WriteString("\n")

		// Description
		if opt.Description != "" {
			b.WriteString(askDescriptionStyle.Render(opt.Description))
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

	// Convert internal answers to external format
	answers := make([]AskUserAnswer, len(result.answers))
	for i, a := range result.answers {
		q := questions[i]
		if q.MultiSelect {
			// Build list of selected option labels
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

	return answers, nil
}
