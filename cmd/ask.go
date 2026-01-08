package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
  term-llm ask -f code.go:10-50 "Explain this function"
  term-llm ask -f clipboard "What is this?"
  cat error.log | term-llm ask "What went wrong?"

Line range syntax for files:
  main.go       - Include entire file
  main.go:11-22 - Include only lines 11-22
  main.go:11-   - Include lines 11 to end of file
  main.go:-22   - Include lines 1-22`,
	Args: cobra.MinimumNArgs(1),
	RunE: runAsk,
}

func init() {
	askCmd.Flags().BoolVarP(&askSearch, "search", "s", false, "Enable web search for current information")
	askCmd.Flags().BoolVarP(&askDebug, "debug", "d", false, "Show debug information")
	askCmd.Flags().BoolVarP(&askText, "text", "t", false, "Output plain text instead of rendered markdown")
	askCmd.Flags().StringVar(&askProvider, "provider", "", "Override provider, optionally with model (e.g., openai:gpt-4o)")
	askCmd.Flags().StringArrayVarP(&askFiles, "file", "f", nil, "File(s) to include as context (supports globs, line ranges like file.go:10-20, 'clipboard')")
	if err := askCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register provider completion: %v", err))
	}
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

	debugMode := askDebug
	req := llm.Request{
		Messages:          messages,
		Search:            askSearch,
		ParallelToolCalls: true,
		Debug:             debugMode,
		DebugRaw:          debugRaw,
	}

	// Check if we're in a TTY and can use glamour
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	useGlamour := !askText && isTTY && !debugRaw

	// Create channel for streaming output
	output := make(chan string)

	// Channel for tool events (for phase updates)
	toolEvents := make(chan toolEvent, 10)

	errChan := make(chan error, 1)
	go func() {
		stream, err := engine.Stream(ctx, req)
		if err != nil {
			errChan <- err
			close(output)
			close(toolEvents)
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				close(output)
				close(toolEvents)
				errChan <- nil
				return
			}
			if err != nil {
				errChan <- err
				close(output)
				close(toolEvents)
				return
			}
			if event.Type == llm.EventError && event.Err != nil {
				errChan <- event.Err
				close(output)
				close(toolEvents)
				return
			}
			if event.Type == llm.EventToolExecStart {
				select {
				case toolEvents <- toolEvent{Name: event.ToolName, Info: event.ToolInfo}:
				default:
				}
			}
			if event.Type == llm.EventRetry {
				select {
				case toolEvents <- toolEvent{
					IsRetry:          true,
					RetryAttempt:     event.RetryAttempt,
					RetryMaxAttempts: event.RetryMaxAttempts,
					RetryWaitSecs:    event.RetryWaitSecs,
				}:
				default:
				}
			}
			if event.Type == llm.EventTextDelta && event.Text != "" {
				output <- event.Text
			}
		}
	}()

	if useGlamour {
		err = streamWithGlamour(ctx, output, toolEvents)
	} else {
		err = streamPlainText(ctx, output, toolEvents)
	}

	if err != nil {
		return err
	}

	if err := <-errChan; err != nil {
		return fmt.Errorf("streaming failed: %w", err)
	}

	return nil
}

// toolEvent represents a tool execution event with name and additional info
type toolEvent struct {
	Name string // Tool name (e.g., "web_search", "read_url")
	Info string // Additional info (e.g., URL being fetched)
	// Retry fields (when IsRetry is true)
	IsRetry          bool
	RetryAttempt     int
	RetryMaxAttempts int
	RetryWaitSecs    float64
}

// streamPlainText streams text directly without formatting
func streamPlainText(ctx context.Context, output <-chan string, toolEvents <-chan toolEvent) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-toolEvents:
			// Show retry status in plain text mode
			if ev.IsRetry {
				fmt.Fprintf(os.Stderr, "\rRate limited (%d/%d), waiting %.0fs...\n",
					ev.RetryAttempt, ev.RetryMaxAttempts, ev.RetryWaitSecs)
			}
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

// truncateURL shortens a URL for display, keeping the domain and path start
func truncateURL(url string, maxLen int) string {
	if len(url) <= maxLen {
		return url
	}
	// Remove protocol prefix for cleaner display
	display := strings.TrimPrefix(url, "https://")
	display = strings.TrimPrefix(display, "http://")
	if len(display) <= maxLen {
		return display
	}
	// Truncate with ellipsis
	return display[:maxLen-3] + "..."
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
	phase       string    // Current phase: "Thinking", "Responding"
	toolPhase   string    // Phase during tool execution (after content started), empty when not in tool
	retryStatus string    // Retry status (e.g., "Rate limited (2/5), waiting 5s...")
	startTime   time.Time // For elapsed time display
}

type askContentMsg string
type askDoneMsg struct{}
type askCancelledMsg struct{}
type askTickMsg time.Time
type askToolStartMsg struct {
	Name string // Tool name being executed
	Info string // Additional info (e.g., URL)
}
type askRetryMsg struct {
	Attempt     int
	MaxAttempts int
	WaitSecs    float64
}

func newAskStreamModel() askStreamModel {
	width := getTerminalWidth()
	styles := ui.DefaultStyles()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styles.Spinner

	return askStreamModel{
		spinner:   s,
		styles:    styles,
		content:   &strings.Builder{},
		width:     width,
		loading:   true,
		phase:     "Thinking",
		startTime: time.Now(),
	}
}

func (m askStreamModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.tickEvery())
}

// tickEvery returns a command that sends a tick every second for elapsed time updates.
func (m askStreamModel) tickEvery() tea.Cmd {
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg {
		return askTickMsg(t)
	})
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
		// If we were in tool phase, add newlines to separate
		if m.toolPhase != "" {
			m.content.WriteString("\n\n")
			m.toolPhase = ""
		}
		m.loading = false
		m.phase = "Responding"
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

	case askTickMsg:
		// Continue ticking for elapsed time updates
		if m.loading || m.toolPhase != "" {
			return m, m.tickEvery()
		}

	case askRetryMsg:
		// Rate limit retry - update status
		m.retryStatus = fmt.Sprintf("Rate limited (%d/%d), waiting %.0fs...",
			msg.Attempt, msg.MaxAttempts, msg.WaitSecs)
		return m, m.tickEvery()

	case askToolStartMsg:
		// Clear retry status when tool starts
		m.retryStatus = ""
		// Tool execution starting/ending - update phase
		var newPhase string
		if msg.Name == "" {
			// Empty tool name means back to thinking
			newPhase = "Thinking"
		} else if msg.Name == llm.WebSearchToolName {
			newPhase = "Searching"
		} else if msg.Name == llm.ReadURLToolName {
			if msg.Info != "" {
				// Show truncated URL in dim style
				url := truncateURL(msg.Info, 50)
				newPhase = fmt.Sprintf("Reading \x1b[2m%s\x1b[0m", url)
			} else {
				newPhase = "Reading"
			}
		} else {
			newPhase = "Running " + msg.Name
		}

		// If content has already started streaming, use toolPhase to show spinner at end
		if !m.loading {
			m.toolPhase = newPhase
			// Return a command to keep spinner animating
			return m, m.spinner.Tick
		}
		m.phase = newPhase

	case spinner.TickMsg:
		if m.loading || m.toolPhase != "" {
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
		return ui.StreamingIndicator{
			Spinner:    m.spinner.View(),
			Phase:      m.phase,
			Elapsed:    time.Since(m.startTime),
			Status:     m.retryStatus,
			ShowCancel: true,
		}.Render(m.styles)
	}

	if m.rendered == "" {
		return ""
	}

	// If in tool phase, append spinner at end of content
	if m.toolPhase != "" {
		return m.rendered + "\n" + ui.StreamingIndicator{
			Spinner:    m.spinner.View(),
			Phase:      m.toolPhase,
			Elapsed:    time.Since(m.startTime),
			ShowCancel: false,
		}.Render(m.styles)
	}

	return m.rendered
}

// streamWithGlamour renders markdown beautifully as content streams in
func streamWithGlamour(ctx context.Context, output <-chan string, toolEvents <-chan toolEvent) error {
	model := newAskStreamModel()

	// Create program - use inline mode so output stays in terminal
	p := tea.NewProgram(model,
		tea.WithoutSignalHandler(),
	)

	// Stream content in background, respecting context cancellation
	go func() {
		for {
			select {
			case <-ctx.Done():
				p.Send(askCancelledMsg{})
				return
			case ev, ok := <-toolEvents:
				if ok {
					if ev.IsRetry {
						p.Send(askRetryMsg{
							Attempt:     ev.RetryAttempt,
							MaxAttempts: ev.RetryMaxAttempts,
							WaitSecs:    ev.RetryWaitSecs,
						})
					} else {
						p.Send(askToolStartMsg{Name: ev.Name, Info: ev.Info})
					}
				}
			case chunk, ok := <-output:
				if !ok {
					p.Send(askDoneMsg{})
					return
				}
				p.Send(askContentMsg(chunk))
			}
		}
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
