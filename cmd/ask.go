package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	askDebug    bool
	askSearch   bool
	askText     bool
	askProvider string
	askFiles    []string
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
  term-llm ask "List 5 programming languages" --text
  term-llm ask -f code.go "Explain this code"
  term-llm ask -f clipboard "What is this?"
  cat error.log | term-llm ask "What went wrong?"`,
	Args: cobra.MinimumNArgs(1),
	RunE: runAsk,
}

func init() {
	askCmd.Flags().BoolVarP(&askSearch, "search", "s", false, "Enable web search for current information")
	askCmd.Flags().BoolVarP(&askDebug, "debug", "d", false, "Show debug information")
	askCmd.Flags().BoolVarP(&askText, "text", "t", false, "Output plain text instead of rendered markdown")
	askCmd.Flags().StringVar(&askProvider, "provider", "", "Override provider, optionally with model (e.g., openai:gpt-4o)")
	askCmd.Flags().StringArrayVarP(&askFiles, "file", "f", nil, "File(s) to include as context (supports globs, 'clipboard')")
	askCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion)
	rootCmd.AddCommand(askCmd)
}

func runAsk(cmd *cobra.Command, args []string) error {
	question := strings.Join(args, " ")
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	if err := applyProviderOverrides(cfg, cfg.Ask.Provider, cfg.Ask.Model, askProvider); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := llm.NewEngine(provider, defaultToolRegistry())

	// Read files if provided
	var files []input.FileContent
	if len(askFiles) > 0 {
		files, err = input.ReadFiles(askFiles)
		if err != nil {
			return fmt.Errorf("failed to read files: %w", err)
		}
	}

	// Read stdin if available
	stdinContent, err := input.ReadStdin()
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	userPrompt := prompt.AskUserPrompt(question, files, stdinContent)
	messages := []llm.Message{}
	if cfg.Ask.Instructions != "" {
		messages = append(messages, llm.SystemText(cfg.Ask.Instructions))
	}
	messages = append(messages, llm.UserText(userPrompt))

	debugMode := askDebug || debugRaw
	req := llm.Request{
		Messages: messages,
		Search:   askSearch,
		Debug:    debugMode,
		DebugRaw: debugRaw,
	}

	// Check if we're in a TTY and can use glamour
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	useGlamour := !askText && isTTY && !debugRaw

	// Create channel for streaming output
	output := make(chan string)

	errChan := make(chan error, 1)
	go func() {
		stream, err := engine.Stream(ctx, req)
		if err != nil {
			errChan <- err
			close(output)
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				close(output)
				errChan <- nil
				return
			}
			if err != nil {
				errChan <- err
				close(output)
				return
			}
			if event.Type == llm.EventError && event.Err != nil {
				errChan <- event.Err
				close(output)
				return
			}
			if event.Type == llm.EventTextDelta && event.Text != "" {
				output <- event.Text
			}
		}
	}()

	if useGlamour {
		err = streamWithGlamour(ctx, output)
	} else {
		err = streamPlainText(ctx, output)
	}

	if err != nil {
		return err
	}

	if err := <-errChan; err != nil {
		return fmt.Errorf("streaming failed: %w", err)
	}

	return nil
}

// streamPlainText streams text directly without formatting
func streamPlainText(ctx context.Context, output <-chan string) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case chunk, ok := <-output:
			if !ok {
				fmt.Println()
				return nil
			}
			fmt.Print(chunk)
		}
	}
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
type askCancelledMsg struct{}

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

	case askCancelledMsg:
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
func streamWithGlamour(ctx context.Context, output <-chan string) error {
	model := newAskStreamModel()

	// Create program - use inline mode so output stays in terminal
	p := tea.NewProgram(model,
		tea.WithoutSignalHandler(),
	)

	// Watch for context cancellation and quit immediately
	go func() {
		<-ctx.Done()
		p.Send(askCancelledMsg{})
	}()

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
