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
	// Container with left border accent
	askContainerStyle = lipgloss.NewStyle().
				BorderStyle(lipgloss.NormalBorder()).
				BorderLeft(true).
				BorderForeground(askAccentColor).
				PaddingLeft(1).
				PaddingRight(2).
				PaddingTop(1).
				PaddingBottom(1)

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
	isCustom      bool
}

// askModel is the bubbletea model for the ask_user UI
type askModel struct {
	questions  []AskUserQuestion
	answers    []askUIAnswer
	currentTab int
	cursor     int // Current selection within options
	textInput  textinput.Model
	width      int
	done       bool
	cancelled  bool
}

// isOnCustomOption returns true if cursor is on "Type your own answer"
func (m askModel) isOnCustomOption() bool {
	if m.isOnConfirmTab() {
		return false
	}
	q := m.questions[m.currentTab]
	return m.cursor == len(q.Options)
}

func newAskModel(questions []AskUserQuestion) askModel {
	ti := textinput.New()
	ti.Placeholder = "Type your own answer"
	ti.CharLimit = 500
	ti.Width = 50
	ti.PromptStyle = lipgloss.NewStyle()
	ti.TextStyle = lipgloss.NewStyle().Foreground(askTextColor)
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(askMutedColor)
	ti.Prompt = ""

	return askModel{
		questions:  questions,
		answers:    make([]askUIAnswer, len(questions)),
		currentTab: 0,
		cursor:     0,
		textInput:  ti,
		width:      80,
	}
}

func (m askModel) Init() tea.Cmd {
	// Initialize answers
	for i := range m.answers {
		m.answers[i].questionIndex = i
	}
	return nil
}

func (m askModel) isAnswered(idx int) bool {
	return m.answers[idx].text != ""
}

func (m askModel) allAnswered() bool {
	for i := range m.answers {
		if !m.isAnswered(i) {
			return false
		}
	}
	return true
}

func (m askModel) isSingleQuestion() bool {
	return len(m.questions) == 1
}

func (m askModel) isOnConfirmTab() bool {
	return !m.isSingleQuestion() && m.currentTab == len(m.questions)
}

func (m askModel) totalTabs() int {
	if m.isSingleQuestion() {
		return 1
	}
	return len(m.questions) + 1 // questions + confirm tab
}

func (m askModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textInput.Width = min(50, m.width-10)

	case tea.KeyMsg:
		// Handle cancel
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			m.cancelled = true
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
			case "up", "k":
				// Move up from custom option
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
					m.done = true
					return m, tea.Quit
				}
			}
			return m, nil
		}

		// Question navigation
		q := m.questions[m.currentTab]
		totalOptions := len(q.Options) + 1 // +1 for "Type your own"

		switch msg.String() {
		case "up", "k":
			m.cursor = (m.cursor - 1 + totalOptions) % totalOptions
			// If moved to custom option, focus the input
			if m.cursor == len(q.Options) {
				m.textInput.Focus()
				return m, textinput.Blink
			}
		case "down", "j":
			m.cursor = (m.cursor + 1) % totalOptions
			// If moved to custom option, focus the input
			if m.cursor == len(q.Options) {
				m.textInput.Focus()
				return m, textinput.Blink
			}
		case "enter", " ":
			// Regular option selected
			m.answers[m.currentTab].text = q.Options[m.cursor].Label
			m.answers[m.currentTab].isCustom = false
			return m.advanceToNext()
		}
	}

	return m, nil
}

func (m askModel) switchTab(newTab int) (tea.Model, tea.Cmd) {
	m.currentTab = newTab
	m.cursor = 0
	m.textInput.Blur()
	m.textInput.SetValue("")
	return m, nil
}

func (m askModel) advanceToNext() (tea.Model, tea.Cmd) {
	// Single question - submit immediately
	if m.isSingleQuestion() {
		m.done = true
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

func (m askModel) View() string {
	if m.done {
		return m.renderSummary()
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

func (m askModel) renderTabs() string {
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

func (m askModel) renderQuestion() string {
	var b strings.Builder

	q := m.questions[m.currentTab]

	// Question text
	b.WriteString(askQuestionStyle.Render(q.Question))
	b.WriteString("\n")

	// Options
	for i, opt := range q.Options {
		isSelected := m.cursor == i
		isPicked := m.isAnswered(m.currentTab) && m.answers[m.currentTab].text == opt.Label && !m.answers[m.currentTab].isCustom

		// Option line: "1. Label  ✓"
		var optLine strings.Builder
		style := askOptionStyle
		if isSelected {
			style = askSelectedOptionStyle
		}
		optLine.WriteString(style.Render(fmt.Sprintf("%d. %s", i+1, opt.Label)))
		if isPicked {
			optLine.WriteString(" ")
			optLine.WriteString(askCheckStyle.Render("✓"))
		}
		b.WriteString(optLine.String())
		b.WriteString("\n")

		// Description
		if opt.Description != "" {
			b.WriteString(askDescriptionStyle.Render(opt.Description))
			b.WriteString("\n")
		}
	}

	// "Type your own answer" option - show input inline
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

func (m askModel) renderConfirm() string {
	var b strings.Builder

	b.WriteString(askQuestionStyle.Render("Review your answers"))
	b.WriteString("\n")

	for i, q := range m.questions {
		answered := m.isAnswered(i)

		b.WriteString(askReviewLabelStyle.Render(q.Header + ": "))
		if answered {
			b.WriteString(askReviewValueStyle.Render(m.answers[i].text))
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

func (m askModel) renderHelp() string {
	var parts []string

	if !m.isSingleQuestion() {
		parts = append(parts, "tab switch")
	}

	if !m.isOnConfirmTab() {
		parts = append(parts, "↑↓ select")
	}

	if m.isOnConfirmTab() {
		parts = append(parts, "enter submit")
	} else if m.isOnCustomOption() {
		parts = append(parts, "enter confirm")
	} else {
		parts = append(parts, "enter select")
	}

	parts = append(parts, "esc dismiss")

	return askHelpStyle.Render(strings.Join(parts, "  "))
}

func (m askModel) renderSummary() string {
	var b strings.Builder

	for i, q := range m.questions {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(askCheckStyle.Render("✓ "))
		b.WriteString(askReviewLabelStyle.Render(q.Question + " "))
		b.WriteString(askReviewValueStyle.Render(m.answers[i].text))
	}

	// Newlines outside container so they're not affected by container styling
	return "\n" + askContainerStyle.Render(b.String()) + "\n"
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

	result := finalModel.(askModel)
	if result.cancelled {
		return nil, fmt.Errorf("cancelled by user")
	}

	// Convert internal answers to external format
	answers := make([]AskUserAnswer, len(result.answers))
	for i, a := range result.answers {
		answers[i] = AskUserAnswer{
			QuestionIndex: a.questionIndex,
			Header:        questions[i].Header,
			Selected:      a.text,
			IsCustom:      a.isCustom,
		}
	}

	return answers, nil
}
