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
	askDebug  bool
	askSearch bool
	askText   bool
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

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Build request
	req := llm.AskRequest{
		Question:     question,
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
		err = streamWithBubbleTea(output)
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

// askModel is the bubbletea model for streaming with glamour
type askModel struct {
	spinner    spinner.Model
	content    *strings.Builder
	output     <-chan string
	done       bool
	finalView  string
	hasContent bool
}

// chunkMsg carries a streaming chunk
type chunkMsg string

// doneMsg signals streaming is complete
type doneMsg struct{}

func newAskModel(output <-chan string) askModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	return askModel{
		spinner: s,
		content: &strings.Builder{},
		output:  output,
	}
}

func (m askModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, waitForChunk(m.output))
}

// waitForChunk reads from the channel and sends chunks as messages
func waitForChunk(output <-chan string) tea.Cmd {
	return func() tea.Msg {
		chunk, ok := <-output
		if !ok {
			return doneMsg{}
		}
		return chunkMsg(chunk)
	}
}

func (m askModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			return m, tea.Quit
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case chunkMsg:
		m.content.WriteString(string(msg))
		m.hasContent = true
		// Continue reading chunks
		return m, waitForChunk(m.output)

	case doneMsg:
		m.done = true
		// Render final content
		if m.content.Len() > 0 {
			rendered, err := renderMarkdown(m.content.String())
			if err != nil {
				m.finalView = m.content.String()
			} else {
				m.finalView = rendered
			}
		}
		return m, tea.Quit
	}

	return m, nil
}

func (m askModel) View() string {
	if m.done {
		return m.finalView
	}

	if !m.hasContent {
		return m.spinner.View() + " Thinking..."
	}

	// Render current content with glamour
	rendered, err := renderMarkdown(m.content.String())
	if err != nil {
		return m.content.String()
	}
	return rendered
}

// streamWithBubbleTea uses bubbletea for proper terminal handling
func streamWithBubbleTea(output <-chan string) error {
	// Open TTY for input
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// Fallback to simple streaming if no TTY
		return streamPlainTextFromChan(output)
	}
	defer tty.Close()

	model := newAskModel(output)
	p := tea.NewProgram(model, tea.WithInput(tty), tea.WithOutput(os.Stdout))

	_, err = p.Run()
	return err
}

// streamPlainTextFromChan is a fallback for non-TTY
func streamPlainTextFromChan(output <-chan string) error {
	for chunk := range output {
		fmt.Print(chunk)
	}
	fmt.Println()
	return nil
}

// renderMarkdown renders markdown content using glamour with no padding
func renderMarkdown(content string) (string, error) {
	// Start with Dracula style (good dark theme) and remove margins
	style := styles.DraculaStyleConfig
	style.Document.Margin = uintPtr(0)
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = uintPtr(0)

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(0),
	)
	if err != nil {
		return "", err
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return "", err
	}

	// Trim leading/trailing whitespace that glamour adds
	return strings.TrimSpace(rendered) + "\n", nil
}

func uintPtr(v uint) *uint {
	return &v
}

// Ensure ansi package is imported for style config
var _ = ansi.StyleConfig{}
