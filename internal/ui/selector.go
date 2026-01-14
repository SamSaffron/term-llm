package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/llm"
	"golang.org/x/term"
)

const SomethingElse = "__something_else__"

// getTTY opens /dev/tty for direct terminal access (bypasses redirections)
func getTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

type spinnerResultMsg struct {
	value any
	err   error
}

// progressUpdateMsg carries a progress update to the spinner.
type progressUpdateMsg ProgressUpdate

// tickMsg triggers a refresh of the elapsed time display.
type tickMsg time.Time

// spinnerPauseMsg pauses the spinner (e.g., during approval prompts).
type spinnerPauseMsg struct{}

// spinnerResumeMsg resumes the spinner after pause.
type spinnerResumeMsg struct{}

// spinnerModel is the bubbletea model for the loading spinner
type spinnerModel struct {
	spinner   spinner.Model
	cancel    context.CancelFunc
	cancelled bool
	result    *spinnerResultMsg
	styles    *Styles

	// Progress tracking
	startTime    time.Time
	pausedAt     time.Time     // When pause started (for elapsed time adjustment)
	pausedTotal  time.Duration // Total time spent paused
	paused       bool          // Whether spinner is paused (e.g., during approval)
	outputTokens int
	status       string
	phase        string // Current phase: "Thinking", "Responding", etc.
	progress     <-chan ProgressUpdate
	milestones   []string
}

func newSpinnerModel(cancel context.CancelFunc, tty *os.File, progress <-chan ProgressUpdate) spinnerModel {
	styles := NewStyles(tty)
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styles.Spinner
	return spinnerModel{
		spinner:   s,
		cancel:    cancel,
		styles:    styles,
		startTime: time.Now(),
		phase:     "Thinking",
		progress:  progress,
	}
}

func (m spinnerModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spinner.Tick, m.tickEvery()}
	if m.progress != nil {
		cmds = append(cmds, m.listenProgress())
	}
	return tea.Batch(cmds...)
}

// tickEvery returns a command that sends a tick every second for elapsed time updates.
func (m spinnerModel) tickEvery() tea.Cmd {
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// listenProgress returns a command that waits for the next progress update.
func (m spinnerModel) listenProgress() tea.Cmd {
	return func() tea.Msg {
		if m.progress == nil {
			return nil
		}
		update, ok := <-m.progress
		if !ok {
			return nil // channel closed
		}
		return progressUpdateMsg(update)
	}
}

func (m spinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyEscape || msg.String() == "ctrl+c" {
			m.cancelled = true
			m.cancel()
			return m, tea.Quit
		}
	case spinnerResultMsg:
		m.result = &msg
		return m, tea.Quit
	case spinnerPauseMsg:
		if !m.paused {
			m.paused = true
			m.pausedAt = time.Now()
		}
		return m, nil
	case spinnerResumeMsg:
		if m.paused {
			m.pausedTotal += time.Since(m.pausedAt)
			m.paused = false
		}
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tickMsg:
		// Refresh for elapsed time - continue ticking
		return m, m.tickEvery()
	case progressUpdateMsg:
		// Update tokens/status/phase
		if msg.OutputTokens > 0 {
			m.outputTokens = msg.OutputTokens
		}
		if msg.Status != "" {
			m.status = msg.Status
		}
		if msg.Milestone != "" {
			m.milestones = append(m.milestones, msg.Milestone)
		}
		if msg.Phase != "" {
			m.phase = msg.Phase
		}
		// Continue listening for more progress
		return m, m.listenProgress()
	}
	return m, nil
}

func (m spinnerModel) View() string {
	// When paused (e.g., during approval prompt), hide the spinner
	if m.paused {
		return ""
	}

	var b strings.Builder

	// Print completed milestones above spinner
	for _, ms := range m.milestones {
		b.WriteString(ms)
		b.WriteString("\n")
	}

	// Calculate elapsed time, excluding paused duration
	elapsed := time.Since(m.startTime) - m.pausedTotal

	// Streaming indicator
	b.WriteString(StreamingIndicator{
		Spinner:    m.spinner.View(),
		Phase:      m.phase,
		Elapsed:    elapsed,
		Tokens:     m.outputTokens,
		Status:     m.status,
		ShowCancel: true,
	}.Render(m.styles))

	return b.String()
}

func runWithSpinnerInternal(ctx context.Context, debug bool, progress <-chan ProgressUpdate, run func(context.Context) (any, error)) (any, error) {
	return runWithSpinnerInternalWithHooks(ctx, debug, progress, run, nil)
}

// ApprovalHookSetup is called with functions to pause/resume the spinner.
// It should set up approval hooks that call these functions.
type ApprovalHookSetup func(pause, resume func())

func runWithSpinnerInternalWithHooks(ctx context.Context, debug bool, progress <-chan ProgressUpdate, run func(context.Context) (any, error), setupHooks ApprovalHookSetup) (any, error) {
	// Create cancellable context
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Get tty for proper rendering
	tty, ttyErr := getTTY()
	if ttyErr != nil {
		// Fallback: no spinner, just run directly
		return run(ctx)
	}
	defer tty.Close()

	// In debug mode, skip spinner so output isn't garbled
	if debug {
		return run(ctx)
	}

	// Create and run spinner
	model := newSpinnerModel(cancel, tty, progress)
	p := tea.NewProgram(model, tea.WithInput(tty), tea.WithOutput(tty), tea.WithoutSignalHandler())

	// Set up approval hooks if provided
	if setupHooks != nil {
		setupHooks(
			func() {
				p.Send(spinnerPauseMsg{})
				p.ReleaseTerminal() // Let huh form take over terminal
			},
			func() {
				p.RestoreTerminal() // Take back terminal control
				p.Send(spinnerResumeMsg{})
			},
		)
	}

	// Start request in background and send result to program
	go func() {
		value, err := run(ctx)
		p.Send(spinnerResultMsg{value: value, err: err})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return nil, err
	}

	m := finalModel.(spinnerModel)
	if m.cancelled {
		return nil, fmt.Errorf("cancelled")
	}

	if m.result == nil {
		return nil, fmt.Errorf("no result received")
	}

	return m.result.value, m.result.err
}

func runWithSpinner(ctx context.Context, debug bool, run func(context.Context) (any, error)) (any, error) {
	return runWithSpinnerInternal(ctx, debug, nil, run)
}

// RunWithSpinner shows a spinner while executing the provided function.
func RunWithSpinner(ctx context.Context, debug bool, run func(context.Context) (any, error)) (any, error) {
	return runWithSpinner(ctx, debug, run)
}

// RunWithSpinnerProgress shows a spinner with progress updates while executing the provided function.
// The progress channel can receive updates with token counts, status messages, and milestones.
func RunWithSpinnerProgress(ctx context.Context, debug bool, progress <-chan ProgressUpdate, run func(context.Context) (any, error)) (any, error) {
	return runWithSpinnerInternal(ctx, debug, progress, run)
}

// RunWithSpinnerProgressAndHooks is like RunWithSpinnerProgress but also sets up approval hooks.
// The setupHooks function is called with pause/resume functions that should be used to set up
// tools.SetApprovalHooks() so the spinner pauses during tool approval prompts.
func RunWithSpinnerProgressAndHooks(ctx context.Context, debug bool, progress <-chan ProgressUpdate, run func(context.Context) (any, error), setupHooks ApprovalHookSetup) (any, error) {
	return runWithSpinnerInternalWithHooks(ctx, debug, progress, run, setupHooks)
}

// selectModel is a bubbletea model for command selection with help support
type selectModel struct {
	suggestions []llm.CommandSuggestion
	cursor      int
	selected    string
	cancelled   bool
	showHelp    bool // signal to show help for current selection
	done        bool // command was selected (not "something else")
	styles      *Styles
	tty         *os.File
	width       int
	textInput   textinput.Model
	refinement  string // user's refinement text when "something else" is used
}

func newSelectModel(suggestions []llm.CommandSuggestion, tty *os.File) selectModel {
	width := 80
	if tty != nil {
		if w, _, err := term.GetSize(int(tty.Fd())); err == nil && w > 0 {
			width = w
		}
	}

	styles := NewStyles(tty)

	// Create a renderer bound to the TTY for proper color output
	renderer := lipgloss.NewRenderer(tty)

	// Colors matching the theme
	mutedColor := styles.theme.Muted

	ti := textinput.New()
	ti.Placeholder = "What else should I know?"
	ti.CharLimit = 500
	ti.Width = min(60, width-10)
	ti.Prompt = ""
	// Use renderer-bound styles so colors work correctly with the TTY
	ti.PromptStyle = renderer.NewStyle()
	ti.TextStyle = renderer.NewStyle() // Use terminal's default text color
	ti.PlaceholderStyle = renderer.NewStyle().Foreground(mutedColor)
	ti.Cursor.Style = renderer.NewStyle() // Use terminal's default cursor
	ti.Cursor.SetMode(cursor.CursorBlink)

	return selectModel{
		suggestions: suggestions,
		cursor:      0,
		styles:      styles,
		tty:         tty,
		width:       width,
		textInput:   ti,
	}
}

func (m selectModel) Init() tea.Cmd {
	return nil
}

func (m selectModel) isOnSomethingElse() bool {
	return m.cursor == len(m.suggestions)
}

func (m selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textInput.Width = min(60, m.width-10)
	case tea.KeyMsg:
		// When on "something else", handle text input
		if m.isOnSomethingElse() {
			switch msg.String() {
			case "enter":
				text := strings.TrimSpace(m.textInput.Value())
				if text != "" {
					m.refinement = text
					m.selected = SomethingElse
					return m, tea.Quit
				}
				return m, nil
			case "up", "k":
				// Move up from "something else"
				if m.cursor > 0 {
					m.cursor--
					m.textInput.Blur()
				}
				return m, nil
			case "esc", "ctrl+c":
				m.cancelled = true
				return m, tea.Quit
			}
			// Pass other keys to text input
			var cmd tea.Cmd
			m.textInput, cmd = m.textInput.Update(msg)
			return m, cmd
		}

		// Normal navigation when not on "something else"
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.suggestions) {
				m.cursor++
				// Focus text input when moving to "something else"
				if m.isOnSomethingElse() {
					cmd := m.textInput.Focus()
					// Ensure we return the focus command (which includes blink)
					if cmd != nil {
						return m, cmd
					}
					// Fallback: explicitly start blink if Focus() returned nil
					return m, textinput.Blink
				}
			}
		case "enter":
			m.selected = m.suggestions[m.cursor].Command
			m.done = true
			return m, tea.Quit
		case "i", "I":
			m.showHelp = true
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	default:
		// Pass cursor blink and other messages to the textinput when focused
		if m.isOnSomethingElse() {
			var cmd tea.Cmd
			m.textInput, cmd = m.textInput.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

// wrapText wraps text to fit within maxWidth, with indent on each line
func wrapText(text string, maxWidth int, indent string) string {
	availWidth := maxWidth - len(indent)
	if availWidth <= 10 {
		availWidth = 10
	}

	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}

	var lines []string
	currentLine := words[0]
	for _, word := range words[1:] {
		if len(currentLine)+1+len(word) <= availWidth {
			currentLine += " " + word
		} else {
			lines = append(lines, indent+currentLine)
			currentLine = word
		}
	}
	lines = append(lines, indent+currentLine)

	return strings.Join(lines, "\n")
}

func (m selectModel) View() string {
	var b strings.Builder

	b.WriteString(m.styles.Bold.Render("Select a command to run"))
	b.WriteString(m.styles.Muted.Render("  [i] info"))
	b.WriteString("\n\n")

	for i, s := range m.suggestions {
		cursor := "  "
		if m.cursor == i {
			cursor = m.styles.Highlighted.Render("> ")
		}

		b.WriteString(cursor)
		wrappedCmd := wrapText(s.Command, m.width, "  ")
		// First line already has cursor, so trim the indent
		wrappedCmd = strings.TrimPrefix(wrappedCmd, "  ")
		b.WriteString(m.styles.Command.Render(wrappedCmd))
		b.WriteString("\n")
		wrappedDesc := wrapText(s.Explanation, m.width, "  ")
		b.WriteString(m.styles.Muted.Render(wrappedDesc))
		b.WriteString("\n")
		if i < len(m.suggestions)-1 {
			b.WriteString("\n")
		}
	}

	// "something else" option with inline text input (hidden when done)
	if !m.done {
		b.WriteString("\n")
		if m.isOnSomethingElse() {
			b.WriteString("  ")
			b.WriteString(m.textInput.View())
		} else {
			b.WriteString("  ")
			b.WriteString(m.styles.Muted.Render("something else..."))
		}
		b.WriteString("\n")
	}

	// Show command being executed when done
	if m.done {
		b.WriteString("\n$ ")
		b.WriteString(m.selected)
		b.WriteString("\n")
	}

	return b.String()
}

// SelectCommand presents the user with a list of command suggestions and returns the selected one.
// Returns the selected command or SomethingElse if user wants to refine their request.
// When SomethingElse is returned, the second return value contains the user's refinement text.
// If engine is non-nil and user presses 'i', shows help for the highlighted command.
// allowNonTTY permits a non-interactive fallback when no TTY is available.
func SelectCommand(suggestions []llm.CommandSuggestion, shell string, engine *llm.Engine, allowNonTTY bool) (selected string, refinement string, err error) {
	for {
		// Get tty for proper rendering
		tty, ttyErr := getTTY()
		if ttyErr != nil {
			if !allowNonTTY {
				return "", "", fmt.Errorf("no TTY available (set TERM_LLM_ALLOW_NON_TTY=1 to allow non-interactive selection)")
			}
			// Fallback to simple first option if no TTY
			if len(suggestions) > 0 {
				return suggestions[0].Command, "", nil
			}
			return "", "", fmt.Errorf("no TTY available")
		}

		model := newSelectModel(suggestions, tty)
		p := tea.NewProgram(model, tea.WithInput(tty), tea.WithOutput(tty), tea.WithoutSignalHandler())

		finalModel, err := p.Run()
		tty.Close()

		if err != nil {
			return "", "", err
		}

		m := finalModel.(selectModel)

		if m.cancelled {
			return "", "", fmt.Errorf("cancelled")
		}

		if m.showHelp && engine != nil {
			// Show help for the selected command
			cmd := m.suggestions[m.cursor].Command
			if err := ShowCommandHelp(cmd, shell, engine); err != nil {
				// Log error but continue with selection
				ShowError(fmt.Sprintf("help failed: %v", err))
			}
			// Loop back to selection after help
			continue
		}

		return m.selected, m.refinement, nil
	}
}

// GetRefinement prompts the user for additional guidance
func GetRefinement() (string, error) {
	var refinement string

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("What else should I know?").
				Placeholder("e.g., use ripgrep instead of grep").
				Value(&refinement),
		),
	)

	// Use /dev/tty directly to bypass shell redirections
	if tty, err := getTTY(); err == nil {
		defer tty.Close()
		form = form.WithInput(tty).WithOutput(tty)
	}

	err := form.Run()
	if err != nil {
		return "", err
	}

	return refinement, nil
}

// ShowError displays an error message
func ShowError(msg string) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
}

// ShowCommand displays the command that will be executed (to stderr, keeping stdout clean)
func ShowCommand(cmd string) {
	fmt.Fprintln(os.Stderr, cmd)
}
