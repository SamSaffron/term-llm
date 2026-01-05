package ui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/samsaffron/term-llm/internal/config"
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

// spinnerModel is the bubbletea model for the loading spinner
type spinnerModel struct {
	spinner   spinner.Model
	cancel    context.CancelFunc
	cancelled bool
	result    *spinnerResultMsg
	styles    *Styles
}

func newSpinnerModel(cancel context.CancelFunc, tty *os.File) spinnerModel {
	styles := NewStyles(tty)
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styles.Spinner
	return spinnerModel{
		spinner: s,
		cancel:  cancel,
		styles:  styles,
	}
}

func (m spinnerModel) Init() tea.Cmd {
	return m.spinner.Tick
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
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m spinnerModel) View() string {
	return m.spinner.View() + " Thinking... " + m.styles.Muted.Render("(esc to cancel)")
}

func runWithSpinner(ctx context.Context, debug bool, run func(context.Context) (any, error)) (any, error) {
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
	model := newSpinnerModel(cancel, tty)
	p := tea.NewProgram(model, tea.WithInput(tty), tea.WithOutput(tty), tea.WithoutSignalHandler())

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

// RunWithSpinner shows a spinner while executing the provided function.
func RunWithSpinner(ctx context.Context, debug bool, run func(context.Context) (any, error)) (any, error) {
	return runWithSpinner(ctx, debug, run)
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
}

func newSelectModel(suggestions []llm.CommandSuggestion, tty *os.File) selectModel {
	width := 80
	if tty != nil {
		if w, _, err := term.GetSize(int(tty.Fd())); err == nil && w > 0 {
			width = w
		}
	}
	return selectModel{
		suggestions: suggestions,
		cursor:      0,
		styles:      NewStyles(tty),
		tty:         tty,
		width:       width,
	}
}

func (m selectModel) Init() tea.Cmd {
	return nil
}

func (m selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.suggestions) { // +1 for "something else"
				m.cursor++
			}
		case "enter":
			if m.cursor == len(m.suggestions) {
				m.selected = SomethingElse
			} else {
				m.selected = m.suggestions[m.cursor].Command
				m.done = true
			}
			return m, tea.Quit
		case "h", "H":
			// Only show help for actual commands, not "something else"
			if m.cursor < len(m.suggestions) {
				m.showHelp = true
				return m, tea.Quit
			}
		case "esc", "q", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
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
	b.WriteString(m.styles.Muted.Render("  [h] help"))
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

	// "something else" option (hidden when done)
	if !m.done {
		b.WriteString("\n")
		cursor := "  "
		if m.cursor == len(m.suggestions) {
			cursor = m.styles.Highlighted.Render("> ")
		}
		b.WriteString(cursor)
		b.WriteString(m.styles.Muted.Render("something else..."))
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
// If engine is non-nil and user presses 'h', shows help for the highlighted command.
// allowNonTTY permits a non-interactive fallback when no TTY is available.
func SelectCommand(suggestions []llm.CommandSuggestion, shell string, engine *llm.Engine, allowNonTTY bool) (string, error) {
	for {
		// Get tty for proper rendering
		tty, ttyErr := getTTY()
		if ttyErr != nil {
			if !allowNonTTY {
				return "", fmt.Errorf("no TTY available (set TERM_LLM_ALLOW_NON_TTY=1 to allow non-interactive selection)")
			}
			// Fallback to simple first option if no TTY
			if len(suggestions) > 0 {
				return suggestions[0].Command, nil
			}
			return "", fmt.Errorf("no TTY available")
		}

		model := newSelectModel(suggestions, tty)
		p := tea.NewProgram(model, tea.WithInput(tty), tea.WithOutput(tty), tea.WithoutSignalHandler())

		finalModel, err := p.Run()
		tty.Close()

		if err != nil {
			return "", err
		}

		m := finalModel.(selectModel)

		if m.cancelled {
			return "", fmt.Errorf("cancelled")
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

		return m.selected, nil
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

// RunSetupWizard runs the first-time setup wizard and returns the config
func RunSetupWizard() (*config.Config, error) {
	// Use /dev/tty for output to bypass redirections
	tty, ttyErr := getTTY()
	if ttyErr == nil {
		defer tty.Close()
		fmt.Fprint(tty, "Welcome to term-llm! Let's get you set up.\n\n")
	} else {
		fmt.Fprint(os.Stderr, "Welcome to term-llm! Let's get you set up.\n\n")
	}

	var provider string

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which LLM provider do you want to use?").
				Options(
					huh.NewOption("Anthropic (Claude)", "anthropic"),
					huh.NewOption("OpenAI", "openai"),
					huh.NewOption("OpenRouter", "openrouter"),
				).
				Value(&provider),
		),
	)

	if ttyErr == nil {
		tty2, _ := getTTY() // need fresh handle after form might close it
		defer tty2.Close()
		form = form.WithInput(tty2).WithOutput(tty2)
	}

	if err := form.Run(); err != nil {
		return nil, err
	}

	// Check for env var
	var envVar string
	var apiKey string
	switch provider {
	case "anthropic":
		envVar = "ANTHROPIC_API_KEY"
		apiKey = os.Getenv(envVar)
	case "openai":
		envVar = "OPENAI_API_KEY"
		apiKey = os.Getenv(envVar)
	case "openrouter":
		envVar = "OPENROUTER_API_KEY"
		apiKey = os.Getenv(envVar)
	}

	if apiKey == "" {
		return nil, fmt.Errorf("%s environment variable is not set\n\nPlease set it:\n  export %s=your-api-key", envVar, envVar)
	}

	cfg := &config.Config{
		Provider: provider,
		Anthropic: config.AnthropicConfig{
			Model: "claude-sonnet-4-5",
		},
		OpenAI: config.OpenAIConfig{
			Model: "gpt-5.2",
		},
		OpenRouter: config.OpenRouterConfig{
			Model:    "x-ai/grok-code-fast-1",
			AppURL:   "https://github.com/samsaffron/term-llm",
			AppTitle: "term-llm",
		},
	}

	// Save the config
	if err := config.Save(cfg); err != nil {
		return nil, fmt.Errorf("failed to save config: %w", err)
	}

	path, _ := config.GetConfigPath()
	if tty, err := getTTY(); err == nil {
		fmt.Fprintf(tty, "Config saved to %s\n\n", path)
		tty.Close()
	} else {
		fmt.Fprintf(os.Stderr, "Config saved to %s\n\n", path)
	}

	// Reload to pick up the env var
	return config.Load()
}
