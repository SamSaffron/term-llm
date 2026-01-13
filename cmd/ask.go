package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/glamour/styles"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	askDebug          bool
	askSearch         bool
	askText           bool
	askProvider       string
	askFiles          []string
	askMCP            string
	askMaxTurns       int
	askNativeSearch   bool
	askNoNativeSearch bool
	// Tool flags
	askTools      string
	askReadDirs   []string
	askWriteDirs  []string
	askShellAllow []string
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
	askCmd.Flags().StringVar(&askMCP, "mcp", "", "Enable MCP server(s), comma-separated (e.g., playwright,filesystem)")
	askCmd.Flags().IntVar(&askMaxTurns, "max-turns", 20, "Max agentic turns for tool execution")
	askCmd.Flags().BoolVar(&askNativeSearch, "native-search", false, "Use provider's native search (override config)")
	askCmd.Flags().BoolVar(&askNoNativeSearch, "no-native-search", false, "Use external search tools instead of provider's native search")
	// Tool flags
	askCmd.Flags().StringVar(&askTools, "tools", "", "Enable local tools (comma-separated: read,write,edit,shell,grep,find,view,image)")
	askCmd.Flags().StringArrayVar(&askReadDirs, "read-dir", nil, "Directories for read/grep/find/view tools (repeatable)")
	askCmd.Flags().StringArrayVar(&askWriteDirs, "write-dir", nil, "Directories for write/edit tools (repeatable)")
	askCmd.Flags().StringArrayVar(&askShellAllow, "shell-allow", nil, "Shell command patterns to allow (repeatable, glob syntax)")
	if err := askCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register provider completion: %v", err))
	}
	if err := askCmd.RegisterFlagCompletionFunc("mcp", MCPFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register mcp completion: %v", err))
	}
	if err := askCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register tools completion: %v", err))
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
	engine := llm.NewEngine(provider, defaultToolRegistry(cfg))

	// Initialize local tools if --tools flag is set
	var toolMgr *tools.ToolManager
	if askTools != "" {
		toolConfig := buildToolConfig(askTools, askReadDirs, askWriteDirs, askShellAllow, cfg)
		if errs := toolConfig.Validate(); len(errs) > 0 {
			return fmt.Errorf("invalid tool config: %v", errs[0])
		}
		toolMgr, err = tools.NewToolManager(&toolConfig, cfg)
		if err != nil {
			return fmt.Errorf("failed to initialize tools: %w", err)
		}
		// PromptFunc is set in streamWithGlamour to use bubbletea UI
		toolMgr.SetupEngine(engine)
	}

	// Initialize MCP servers if --mcp flag is set
	var mcpManager *mcp.Manager
	if askMCP != "" {
		mcpManager, err = enableMCPServersWithFeedback(ctx, askMCP, engine, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		if mcpManager != nil {
			defer mcpManager.StopAll()
		}
	}

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
		Messages:            messages,
		Search:              askSearch,
		ForceExternalSearch: resolveForceExternalSearch(cfg, askNativeSearch, askNoNativeSearch),
		ParallelToolCalls:   true,
		MaxTurns:            askMaxTurns,
		Debug:               debugMode,
		DebugRaw:            debugRaw,
	}

	// Add tools to request if any are registered (local or MCP)
	if toolMgr != nil || mcpManager != nil {
		req.Tools = engine.Tools().AllSpecs()
		req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}

	// Check if we're in a TTY and can use glamour
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	useGlamour := !askText && isTTY && !debugRaw

	// Create channel for streaming output
	output := make(chan string)

	// Channel for tool events (for phase updates)
	toolEvents := make(chan toolEvent, 10)

	// Track session stats
	stats := ui.NewSessionStats()

	// For glamour mode, create the tea.Program and set PromptFunc BEFORE starting the stream
	// This avoids a race condition where tool execution starts before PromptFunc is set
	var teaProgram *tea.Program
	if useGlamour && toolMgr != nil {
		model := newAskStreamModel()
		teaProgram = tea.NewProgram(model, tea.WithoutSignalHandler())
		toolMgr.ApprovalMgr.PromptFunc = func(req *tools.ApprovalRequest) (tools.ConfirmOutcome, string) {
			responseCh := make(chan bool, 1)
			teaProgram.Send(askApprovalRequestMsg{
				Description: req.Description,
				ToolName:    req.ToolName,
				ToolInfo:    req.ToolInfo,
				ResponseCh:  responseCh,
			})
			approved := <-responseCh
			if approved {
				return tools.ProceedAlways, req.Path
			}
			return tools.Cancel, ""
		}
	}

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
				if event.ToolName != "" {
					stats.ToolStart()
				} else {
					stats.ToolEnd()
				}
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
			if event.Type == llm.EventUsage && event.Use != nil {
				stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens)
				select {
				case toolEvents <- toolEvent{
					IsUsage:      true,
					InputTokens:  event.Use.InputTokens,
					OutputTokens: event.Use.OutputTokens,
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
		err = streamWithGlamour(ctx, output, toolEvents, teaProgram)
	} else {
		err = streamPlainText(ctx, output, toolEvents)
	}

	if err != nil {
		return err
	}

	if err := <-errChan; err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("streaming failed: %w", err)
	}

	if showStats {
		stats.Finalize()
		fmt.Fprintln(cmd.ErrOrStderr(), stats.Render())
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
	// Usage fields (when IsUsage is true)
	IsUsage      bool
	InputTokens  int
	OutputTokens int
}

// streamPlainText streams text directly without formatting
func streamPlainText(ctx context.Context, output <-chan string, toolEvents <-chan toolEvent) error {
	var pendingTools []string
	printedAny := false
	lastEndedWithNewline := true
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-toolEvents:
			if !ok {
				toolEvents = nil
				continue
			}
			// Show retry status in plain text mode
			if ev.IsRetry {
				fmt.Fprintf(os.Stderr, "\rRate limited (%d/%d), waiting %.0fs...\n",
					ev.RetryAttempt, ev.RetryMaxAttempts, ev.RetryWaitSecs)
				continue
			}
			if ev.IsUsage {
				continue
			}
			if ev.Name == "" {
				if len(pendingTools) > 0 {
					if printedAny && !lastEndedWithNewline {
						fmt.Print("\n")
					}
					if printedAny {
						fmt.Print("\n")
					}
					for _, tool := range pendingTools {
						fmt.Printf("• %s ✓\n", tool)
					}
					fmt.Print("\n")
					pendingTools = nil
					printedAny = true
					lastEndedWithNewline = true
				}
				continue
			}
			_, toolDesc := toolDisplay(ev.Name, ev.Info)
			if toolDesc != "" {
				pendingTools = append(pendingTools, toolDesc)
			}
		case chunk, ok := <-output:
			if !ok {
				if len(pendingTools) > 0 {
					if printedAny && !lastEndedWithNewline {
						fmt.Print("\n")
					}
					if printedAny {
						fmt.Print("\n")
					}
					for _, tool := range pendingTools {
						fmt.Printf("• %s ✓\n", tool)
					}
					fmt.Print("\n")
				}
				fmt.Println()
				return nil
			}
			fmt.Print(chunk)
			printedAny = true
			if len(chunk) > 0 {
				lastEndedWithNewline = strings.HasSuffix(chunk, "\n")
			}
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

func toolDisplay(name, info string) (string, string) {
	phase := ui.FormatToolPhase(name, info)
	return phase.Active, phase.Completed
}

func appendToolLog(content *strings.Builder, tools []string) {
	if len(tools) == 0 {
		return
	}
	if content.Len() > 0 {
		content.WriteString("\n\n")
	}
	for _, tool := range tools {
		content.WriteString("- ")
		content.WriteString(tool)
		content.WriteString(" ✓\n")
	}
	content.WriteString("\n")
}

// askStreamModel is a bubbletea model for streaming ask responses
type askStreamModel struct {
	spinner      spinner.Model
	styles       *ui.Styles
	content      *strings.Builder
	rendered     string
	finalOutput  string // stored for printing after tea exits
	width        int
	loading      bool
	phase        string    // Current phase: "Thinking", "Responding"
	toolPhase    string    // Phase during tool execution (after content started), empty when not in tool
	pendingTools []string  // Tools started in the current batch
	retryStatus  string    // Retry status (e.g., "Rate limited (2/5), waiting 5s...")
	startTime    time.Time // For elapsed time display
	totalTokens  int       // Total tokens (input + output) used

	// Approval prompt state (using huh form)
	approvalForm       *huh.Form
	approvalDesc       string
	approvalToolInfo   string       // Info for the tool that triggered approval (to avoid duplicates)
	approvalResponseCh chan<- bool  // channel to send y/n response back to tool
}

type askContentMsg string
type askDoneMsg struct{}
type askCancelledMsg struct{}
type askUsageMsg struct {
	InputTokens  int
	OutputTokens int
}
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
type askApprovalRequestMsg struct {
	Description string
	ToolName    string
	ToolInfo    string
	ResponseCh  chan<- bool
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
	// Handle tool start messages even while approval form is active
	// This ensures parallel tool executions are tracked in pendingTools
	if toolMsg, ok := msg.(askToolStartMsg); ok && m.approvalForm != nil {
		// Don't process the "tools complete" signal (Name == "") during approval
		// Just accumulate named tools for later logging
		if toolMsg.Name != "" {
			// Skip the tool that triggered approval (already tracked in toolPhase)
			if toolMsg.Info != m.approvalToolInfo {
				_, toolDesc := toolDisplay(toolMsg.Name, toolMsg.Info)
				if toolDesc != "" {
					m.pendingTools = append(m.pendingTools, toolDesc)
				}
			}
		}
		// Don't return - let the message also go to the form (for spinner updates, etc.)
	}

	// If approval form is active, delegate to it
	if m.approvalForm != nil {
		form, cmd := m.approvalForm.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.approvalForm = f
		}

		// Check if form completed
		if m.approvalForm.State == huh.StateCompleted {
			approved := m.approvalForm.GetBool("confirm")
			// Log all pending tools (including the one that triggered approval)
			if m.content.Len() > 0 {
				m.content.WriteString("\n\n")
			}
			if approved {
				// Log the tool that triggered approval first (use toolPhase which has Completed form)
				if m.toolPhase != "" {
					m.content.WriteString("- ")
					m.content.WriteString(m.toolPhase)
					m.content.WriteString(" ✓\n")
				}
				// Log any additional parallel tools that started during approval
				for _, tool := range m.pendingTools {
					m.content.WriteString("- ")
					m.content.WriteString(tool)
					m.content.WriteString(" ✓\n")
				}
			} else {
				m.content.WriteString("- ✗ ")
				m.content.WriteString(m.approvalDesc)
				m.content.WriteString("\n")
			}
			m.pendingTools = nil // Clear pending tools after logging
			m.rendered = m.render()

			// Send response
			if m.approvalResponseCh != nil {
				m.approvalResponseCh <- approved
			}
			m.approvalForm = nil
			m.approvalResponseCh = nil
			m.approvalDesc = ""
			m.approvalToolInfo = ""
			return m, m.spinner.Tick
		}

		// Check if form was aborted (esc/ctrl+c)
		if m.approvalForm.State == huh.StateAborted {
			if m.approvalResponseCh != nil {
				m.approvalResponseCh <- false
			}
			m.approvalForm = nil
			m.approvalResponseCh = nil
			m.approvalDesc = ""
			return m, tea.Quit
		}

		return m, cmd
	}

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
		m.finalOutput = m.rendered
		m.rendered = ""
		return m, tea.Quit

	case askCancelledMsg:
		return m, tea.Quit

	case askUsageMsg:
		m.totalTokens = msg.InputTokens + msg.OutputTokens

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
		if msg.Name == "" {
			if len(m.pendingTools) > 0 {
				appendToolLog(m.content, m.pendingTools)
				m.pendingTools = nil
				m.rendered = m.render()
			}
			m.toolPhase = ""
			m.phase = "Thinking"
			m.loading = true // Show thinking spinner while waiting for LLM response
			return m, m.spinner.Tick
		}

		newPhase, toolDesc := toolDisplay(msg.Name, msg.Info)
		if toolDesc != "" {
			m.pendingTools = append(m.pendingTools, toolDesc)
		}

		// If content has already started streaming, use toolPhase to show spinner at end
		if !m.loading {
			m.toolPhase = newPhase
		} else {
			m.phase = newPhase
		}
		// Return a command to keep spinner animating
		return m, m.spinner.Tick

	case askApprovalRequestMsg:
		m.approvalDesc = msg.Description
		m.approvalResponseCh = msg.ResponseCh
		m.approvalToolInfo = msg.ToolInfo // Store to deduplicate pending tools
		// Store tool phase for after approval completes (use Completed form for consistency)
		if msg.ToolName != "" {
			phase := ui.FormatToolPhase(msg.ToolName, msg.ToolInfo)
			m.toolPhase = phase.Completed
		}
		// Form title is just the approval question - no tool phase
		m.approvalForm = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Key("confirm").
					Title(msg.Description).
					Affirmative("Yes").
					Negative("No").
					WithButtonAlignment(lipgloss.Left),
			),
		).WithShowHelp(false).WithShowErrors(false)
		return m, m.approvalForm.Init()

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
	// If approval form is active, show rendered content + form (no spinner/time during approval)
	if m.approvalForm != nil {
		var view strings.Builder
		if m.rendered != "" {
			view.WriteString(m.rendered)
			view.WriteString("\n\n")
		}
		view.WriteString(m.approvalForm.View())
		return view.String()
	}

	if m.loading {
		indicator := ui.StreamingIndicator{
			Spinner:    m.spinner.View(),
			Phase:      m.phase,
			Elapsed:    time.Since(m.startTime),
			Tokens:     m.totalTokens,
			Status:     m.retryStatus,
			ShowCancel: true,
		}.Render(m.styles)
		if m.rendered != "" {
			return m.rendered + "\n" + indicator
		}
		return indicator
	}

	// If in tool phase, show spinner (even if no content yet)
	if m.toolPhase != "" {
		indicator := ui.StreamingIndicator{
			Spinner:    m.spinner.View(),
			Phase:      m.toolPhase,
			Elapsed:    time.Since(m.startTime),
			Tokens:     m.totalTokens,
			ShowCancel: true,
		}.Render(m.styles)
		if m.rendered != "" {
			return m.rendered + "\n" + indicator
		}
		return indicator
	}

	if m.rendered == "" {
		return ""
	}

	return m.rendered
}

// streamWithGlamour renders markdown beautifully as content streams in
// If p is nil, creates a new tea.Program; otherwise uses the provided one.
func streamWithGlamour(ctx context.Context, output <-chan string, toolEvents <-chan toolEvent, p *tea.Program) error {
	// Create program if not provided (when no tools are used)
	if p == nil {
		model := newAskStreamModel()
		p = tea.NewProgram(model, tea.WithoutSignalHandler())
	}

	programDone := make(chan error, 1)
	var finalModel askStreamModel
	go func() {
		fm, err := p.Run()
		if m, ok := fm.(askStreamModel); ok {
			finalModel = m
		}
		programDone <- err
	}()

	for output != nil || toolEvents != nil {
		select {
		case <-ctx.Done():
			p.Send(askCancelledMsg{})
			output = nil
			toolEvents = nil

		case ev, ok := <-toolEvents:
			if !ok {
				toolEvents = nil
				continue
			}
			if ev.IsRetry {
				p.Send(askRetryMsg{
					Attempt:     ev.RetryAttempt,
					MaxAttempts: ev.RetryMaxAttempts,
					WaitSecs:    ev.RetryWaitSecs,
				})
				continue
			}
			if ev.IsUsage {
				p.Send(askUsageMsg{
					InputTokens:  ev.InputTokens,
					OutputTokens: ev.OutputTokens,
				})
				continue
			}
			if ev.Name == "" {
				p.Send(askToolStartMsg{Name: "", Info: ""})
				continue
			}
			p.Send(askToolStartMsg{Name: ev.Name, Info: ev.Info})

		case chunk, ok := <-output:
			if !ok {
				p.Send(askDoneMsg{})
				output = nil
				continue
			}
			p.Send(askContentMsg(chunk))
		}
	}

	err := <-programDone
	if finalModel.finalOutput != "" {
		fmt.Println(finalModel.finalOutput)
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

// enableMCPServersWithFeedback initializes MCP servers with user feedback.
// Returns the manager (caller must call StopAll) or error if setup failed.
func enableMCPServersWithFeedback(ctx context.Context, mcpFlag string, engine *llm.Engine, errWriter io.Writer) (*mcp.Manager, error) {
	serverNames := parseServerList(mcpFlag)
	if len(serverNames) == 0 {
		return nil, nil
	}

	mcpManager := mcp.NewManager()
	if err := mcpManager.LoadConfig(); err != nil {
		return nil, fmt.Errorf("failed to load MCP config: %w", err)
	}

	// Validate all servers exist before starting any
	available := mcpManager.AvailableServers()
	availableSet := make(map[string]bool)
	for _, s := range available {
		availableSet[s] = true
	}

	var missing []string
	for _, name := range serverNames {
		if !availableSet[name] {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		if len(missing) == 1 {
			return nil, fmt.Errorf("MCP server '%s' not configured. Add it with: term-llm mcp add %s", missing[0], missing[0])
		}
		return nil, fmt.Errorf("MCP servers not configured: %s. Add them with: term-llm mcp add <name>", strings.Join(missing, ", "))
	}

	// Show starting message
	fmt.Fprintf(errWriter, "Starting MCP: %s", strings.Join(serverNames, ", "))

	// Enable all servers (async)
	var enableErrors []string
	for _, server := range serverNames {
		if err := mcpManager.Enable(ctx, server); err != nil {
			enableErrors = append(enableErrors, fmt.Sprintf("%s: %v", server, err))
		}
	}

	if len(enableErrors) > 0 {
		fmt.Fprintf(errWriter, "\n")
		return nil, fmt.Errorf("failed to start MCP servers: %s", strings.Join(enableErrors, "; "))
	}

	// Wait for servers with spinner animation
	spinChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinIdx := 0
	timeout := 10 * time.Second
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		allReady := true
		for _, name := range mcpManager.EnabledServers() {
			status, _ := mcpManager.ServerStatus(name)
			if status == mcp.StatusStarting {
				allReady = false
				break
			}
		}
		if allReady {
			break
		}
		fmt.Fprintf(errWriter, "\r%s Starting MCP: %s", spinChars[spinIdx], strings.Join(serverNames, ", "))
		spinIdx = (spinIdx + 1) % len(spinChars)
		time.Sleep(80 * time.Millisecond)
	}

	// Check for failed servers
	var failedServers []string
	for _, name := range serverNames {
		status, err := mcpManager.ServerStatus(name)
		if status == mcp.StatusFailed {
			errMsg := "unknown error"
			if err != nil {
				errMsg = err.Error()
			}
			failedServers = append(failedServers, fmt.Sprintf("%s (%s)", name, errMsg))
		}
	}

	if len(failedServers) > 0 {
		fmt.Fprintf(errWriter, "\n")
		return nil, fmt.Errorf("MCP servers failed to start: %s", strings.Join(failedServers, "; "))
	}

	// Register MCP tools
	mcp.RegisterMCPTools(mcpManager, engine.Tools())
	tools := mcpManager.AllTools()

	// Show result
	if len(tools) > 0 {
		fmt.Fprintf(errWriter, "\r✓ MCP ready: %d tools from %s\n", len(tools), strings.Join(serverNames, ", "))
	} else {
		fmt.Fprintf(errWriter, "\n")
		return nil, fmt.Errorf("MCP servers started but no tools available from: %s", strings.Join(serverNames, ", "))
	}

	return mcpManager, nil
}

// parseServerList splits comma-separated server names and trims whitespace.
func parseServerList(mcpFlag string) []string {
	var servers []string
	for _, s := range strings.Split(mcpFlag, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			servers = append(servers, s)
		}
	}
	return servers
}

// Ensure ansi package is imported for style config
var _ = ansi.StyleConfig{}
