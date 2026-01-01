package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	askDebug    bool
	askSearch   bool
	askText     bool
	askProvider string
)

var askCmd = &cobra.Command{
	Use:   "ask <question>",
	Short: "Ask a question and stream the answer",
	Long: `Ask the LLM a question and receive a streaming response.

Examples:
  term-llm ask "What is the capital of France?"
  term-llm ask "How do I reverse a string in Go?"
  term-llm ask "What is the latest version of Node.js?" -s
  term-llm ask "Explain the difference between TCP and UDP" -d
  term-llm ask "List 5 programming languages" --text`,
	Args: cobra.MinimumNArgs(1),
	RunE: runAsk,
}

func init() {
	askCmd.Flags().BoolVarP(&askSearch, "search", "s", false, "Enable web search for current information")
	askCmd.Flags().BoolVarP(&askDebug, "debug", "d", false, "Show debug information")
	askCmd.Flags().BoolVarP(&askText, "text", "t", false, "Output plain text instead of rendered markdown")
	askCmd.Flags().StringVar(&askProvider, "provider", "", "Override provider (anthropic, openai, gemini, zen)")
	askCmd.RegisterFlagCompletionFunc("provider", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"anthropic", "openai", "gemini", "zen"}, cobra.ShellCompDirectiveNoFileComp
	})
	rootCmd.AddCommand(askCmd)
}

func runAsk(cmd *cobra.Command, args []string) error {
	question := strings.Join(args, " ")
	ctx := context.Background()

	// Load or setup config
	var cfg *config.Config
	var err error

	if config.NeedsSetup() {
		cfg, err = ui.RunSetupWizard()
		if err != nil {
			return fmt.Errorf("setup cancelled: %w", err)
		}
	} else {
		cfg, err = config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
	}

	// Override provider if flag is set
	if askProvider != "" {
		cfg.Provider = askProvider
	}

	// Initialize theme from config
	ui.InitTheme(ui.ThemeConfig{
		Primary:   cfg.Theme.Primary,
		Secondary: cfg.Theme.Secondary,
		Success:   cfg.Theme.Success,
		Error:     cfg.Theme.Error,
		Warning:   cfg.Theme.Warning,
		Muted:     cfg.Theme.Muted,
		Text:      cfg.Theme.Text,
		Spinner:   cfg.Theme.Spinner,
	})

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Build request
	req := llm.AskRequest{
		Question:     question,
		Instructions: cfg.Ask.Instructions,
		EnableSearch: askSearch,
		Debug:        askDebug,
	}

	// Check if we're in a TTY and can use glamour
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	useGlamour := !askText && isTTY

	// Create channel for streaming output
	output := make(chan string)

	// Start streaming in goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- provider.StreamResponse(ctx, req, output)
	}()

	if useGlamour {
		err = streamWithGlamour(output)
	} else {
		err = streamPlainText(output)
	}

	if err != nil {
		return err
	}

	// Check for streaming errors
	if err := <-errChan; err != nil {
		return fmt.Errorf("streaming failed: %w", err)
	}

	return nil
}

// streamPlainText streams text directly without formatting
func streamPlainText(output <-chan string) error {
	for chunk := range output {
		fmt.Print(chunk)
	}
	fmt.Println()
	return nil
}

// getTerminalWidth returns the terminal width or a default
func getTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return 80 // default
	}
	return width
}

// askStreamModel is a bubbletea model for streaming ask responses
type askStreamModel struct {
	spinner     spinner.Model
	styles      *ui.Styles
	content     *strings.Builder
	rendered    string
	finalOutput string // stored for printing after tea exits
	width       int
	loading     bool
}

type askContentMsg string
type askDoneMsg struct{}

func newAskStreamModel() askStreamModel {
	width := getTerminalWidth()
	styles := ui.DefaultStyles()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styles.Spinner

	return askStreamModel{
		spinner: s,
		styles:  styles,
		content: &strings.Builder{},
		width:   width,
		loading: true,
	}
}

func (m askStreamModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m askStreamModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		if m.content.Len() > 0 {
			m.rendered = m.render()
		}

	case askContentMsg:
		m.loading = false
		m.content.WriteString(string(msg))
		m.rendered = m.render()

	case askDoneMsg:
		m.loading = false
		// Store final output for printing after tea exits, clear view
		m.finalOutput = m.rendered
		m.rendered = ""
		return m, tea.Quit

	case spinner.TickMsg:
		if m.loading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m askStreamModel) render() string {
	content := m.content.String()
	if content == "" {
		return ""
	}

	rendered, err := renderMarkdown(content, m.width)
	if err != nil {
		return content
	}
	return rendered
}

func (m askStreamModel) View() string {
	if m.loading {
		return m.spinner.View() + " Thinking... " + m.styles.Muted.Render("(esc to cancel)")
	}

	if m.rendered == "" {
		return ""
	}

	return m.rendered
}

// streamWithGlamour renders markdown beautifully as content streams in
func streamWithGlamour(output <-chan string) error {
	model := newAskStreamModel()

	// Create program - use inline mode so output stays in terminal
	p := tea.NewProgram(model,
		tea.WithoutSignalHandler(),
	)

	// Stream content in background
	go func() {
		for chunk := range output {
			p.Send(askContentMsg(chunk))
		}
		p.Send(askDoneMsg{})
	}()

	finalModel, err := p.Run()

	// Print final output after tea cleanup to ensure it persists
	if m, ok := finalModel.(askStreamModel); ok && m.finalOutput != "" {
		fmt.Println(m.finalOutput)
	}

	return err
}

// renderMarkdown renders markdown content using glamour
func renderMarkdown(content string, width int) (string, error) {
	style := styles.DraculaStyleConfig
	style.Document.Margin = uintPtr(0)
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = uintPtr(0)

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return "", err
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return "", err
	}

	// Don't apply wordwrap - glamour already handles wrapping,
	// and wordwrap breaks ANSI escape codes
	return strings.TrimSpace(rendered), nil
}

func uintPtr(v uint) *uint {
	return &v
}

// Ensure ansi package is imported for style config
var _ = ansi.StyleConfig{}
