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
	askCmd.Flags().StringVar(&askTools, "tools", "", "Enable local tools (comma-separated, or 'all' for everything: read,write,edit,shell,grep,find,view,image)")
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
				stats.ToolStart()
				select {
				case toolEvents <- toolEvent{Name: event.ToolName, Info: event.ToolInfo}:
				default:
				}
			}
			if event.Type == llm.EventToolExecEnd {
				stats.ToolEnd()
				select {
				case toolEvents <- toolEvent{
					Name:        event.ToolName,
					Info:        event.ToolInfo,
					IsToolEnd:   true,
					ToolSuccess: event.ToolSuccess,
				}:
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
	// Tool end fields (when IsToolEnd is true)
	IsToolEnd   bool
	ToolSuccess bool
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
	// Track pending tools with their status
	type toolEntry struct {
		name    string
		info    string
		success bool
		done    bool
	}
	var tools []toolEntry
	printedAny := false
	lastEndedWithNewline := true

	printTools := func() {
		if len(tools) == 0 {
			return
		}
		if printedAny && !lastEndedWithNewline {
			fmt.Print("\n")
		}
		if printedAny {
			fmt.Print("\n")
		}
		for _, t := range tools {
			phase := ui.FormatToolPhase(t.name, t.info)
			if t.success {
				fmt.Printf("%s %s\n", ui.SuccessCircle(), phase.Completed)
			} else {
				fmt.Printf("%s %s\n", ui.ErrorCircle(), phase.Completed)
			}
		}
		fmt.Print("\n")
		tools = nil
		printedAny = true
		lastEndedWithNewline = true
	}

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
			if ev.IsToolEnd {
				// Find and update the tool entry
				for i := range tools {
					if tools[i].name == ev.Name && !tools[i].done {
						tools[i].success = ev.ToolSuccess
						tools[i].done = true
						break
					}
				}
				// Check if all tools are done
				allDone := true
				for _, t := range tools {
					if !t.done {
						allDone = false
						break
					}
				}
				if allDone && len(tools) > 0 {
					printTools()
				}
				continue
			}
			// Tool start - add to pending
			tools = append(tools, toolEntry{name: ev.Name, info: ev.Info})
		case chunk, ok := <-output:
			if !ok {
				if len(tools) > 0 {
					printTools()
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

// askStreamModel is a bubbletea model for streaming ask responses
type askStreamModel struct {
	spinner spinner.Model
	styles  *ui.Styles
	width   int

	// Segment-based content model
	segments []ui.Segment // All segments in the stream (text + tools)

	// State flags
	thinking bool // True only when waiting for LLM response (not during tools or streaming)

	// Wave animation for pending tools
	wavePos    int
	wavePaused bool

	// Status display
	retryStatus string    // Retry status (e.g., "Rate limited (2/5), waiting 5s...")
	startTime   time.Time // For elapsed time display
	totalTokens int       // Total tokens (input + output) used

	// Approval prompt state (using huh form)
	approvalForm       *huh.Form
	approvalDesc       string
	approvalToolInfo   string      // Info for the tool that triggered approval (to avoid duplicates)
	approvalResponseCh chan<- bool // channel to send y/n response back to tool
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
type askToolEndMsg struct {
	Name    string // Tool name that completed
	Info    string // Additional info
	Success bool   // Whether the tool succeeded
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
type askWaveTickMsg struct{}
type askWavePauseMsg struct{}

func newAskStreamModel() askStreamModel {
	width := getTerminalWidth()
	styles := ui.DefaultStyles()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styles.Spinner

	return askStreamModel{
		spinner:   s,
		styles:    styles,
		width:     width,
		thinking:  true,
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
	if toolMsg, ok := msg.(askToolStartMsg); ok && m.approvalForm != nil {
		// Add tool segment as pending during approval
		if toolMsg.Info != m.approvalToolInfo {
			m.segments = append(m.segments, ui.Segment{
				Type:       ui.SegmentTool,
				ToolName:   toolMsg.Name,
				ToolInfo:   toolMsg.Info,
				ToolStatus: ui.ToolPending,
			})
		}
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
			// Update the tool segment that triggered approval
			for i := len(m.segments) - 1; i >= 0; i-- {
				if m.segments[i].Type == ui.SegmentTool &&
					m.segments[i].ToolStatus == ui.ToolPending &&
					m.segments[i].ToolInfo == m.approvalToolInfo {
					if approved {
						m.segments[i].ToolStatus = ui.ToolSuccess
					} else {
						m.segments[i].ToolStatus = ui.ToolError
					}
					break
				}
			}

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
		// Re-render text segments with new width
		for i := range m.segments {
			if m.segments[i].Type == ui.SegmentText && m.segments[i].Complete {
				rendered, err := renderMarkdown(m.segments[i].Text, m.width)
				if err == nil {
					m.segments[i].Rendered = rendered
				}
			}
		}

	case askContentMsg:
		m.thinking = false
		text := string(msg)

		// Find or create current text segment
		var currentSeg *ui.Segment
		if len(m.segments) > 0 && m.segments[len(m.segments)-1].Type == ui.SegmentText && !m.segments[len(m.segments)-1].Complete {
			currentSeg = &m.segments[len(m.segments)-1]
		} else {
			m.segments = append(m.segments, ui.Segment{Type: ui.SegmentText})
			currentSeg = &m.segments[len(m.segments)-1]
		}
		currentSeg.Text += text

	case askDoneMsg:
		m.thinking = false
		// Mark all text segments as complete and render
		for i := range m.segments {
			if m.segments[i].Type == ui.SegmentText && !m.segments[i].Complete {
				m.segments[i].Complete = true
				if m.segments[i].Text != "" {
					rendered, err := renderMarkdown(m.segments[i].Text, m.width)
					if err == nil {
						m.segments[i].Rendered = rendered
					}
				}
			}
		}
		return m, tea.Quit

	case askCancelledMsg:
		return m, tea.Quit

	case askUsageMsg:
		m.totalTokens = msg.InputTokens + msg.OutputTokens

	case askTickMsg:
		// Continue ticking for elapsed time updates during thinking
		if m.thinking {
			return m, m.tickEvery()
		}

	case askRetryMsg:
		m.retryStatus = fmt.Sprintf("Rate limited (%d/%d), waiting %.0fs...",
			msg.Attempt, msg.MaxAttempts, msg.WaitSecs)
		return m, m.tickEvery()

	case askToolStartMsg:
		m.retryStatus = ""
		m.thinking = false
		// Check if we already have a pending segment for this tool+info (avoid duplicates)
		alreadyPending := false
		for i := len(m.segments) - 1; i >= 0; i-- {
			seg := m.segments[i]
			if seg.Type == ui.SegmentTool &&
				seg.ToolStatus == ui.ToolPending &&
				seg.ToolName == msg.Name &&
				seg.ToolInfo == msg.Info {
				alreadyPending = true
				break
			}
		}
		if !alreadyPending {
			// Add new tool segment as pending
			m.segments = append(m.segments, ui.Segment{
				Type:       ui.SegmentTool,
				ToolName:   msg.Name,
				ToolInfo:   msg.Info,
				ToolStatus: ui.ToolPending,
			})
		}
		// Start wave animation
		m.wavePos = 0
		m.wavePaused = false
		return m, tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg {
			return askWaveTickMsg{}
		})

	case askToolEndMsg:
		// Update the matching pending tool to success/error
		// Match on both name AND info to handle parallel calls to same tool
		for i := len(m.segments) - 1; i >= 0; i-- {
			if m.segments[i].Type == ui.SegmentTool &&
				m.segments[i].ToolStatus == ui.ToolPending &&
				m.segments[i].ToolName == msg.Name &&
				m.segments[i].ToolInfo == msg.Info {
				if msg.Success {
					m.segments[i].ToolStatus = ui.ToolSuccess
				} else {
					m.segments[i].ToolStatus = ui.ToolError
				}
				break
			}
		}
		// If no more pending tools, go back to thinking
		if !ui.HasPendingTool(m.segments) {
			m.thinking = true
			return m, m.spinner.Tick
		}

	case askWaveTickMsg:
		// Update wave animation for pending tools
		if ui.HasPendingTool(m.segments) && !m.wavePaused {
			toolTextLen := ui.GetPendingToolTextLen(m.segments)
			m.wavePos++
			if m.wavePos >= toolTextLen {
				// Wave complete, start pause
				m.wavePaused = true
				m.wavePos = -1
				return m, tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
					return askWavePauseMsg{}
				})
			}
			return m, tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg {
				return askWaveTickMsg{}
			})
		}

	case askWavePauseMsg:
		// Pause complete, restart wave
		if ui.HasPendingTool(m.segments) {
			m.wavePaused = false
			m.wavePos = 0
			return m, tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg {
				return askWaveTickMsg{}
			})
		}

	case askApprovalRequestMsg:
		m.approvalDesc = msg.Description
		m.approvalResponseCh = msg.ResponseCh
		m.approvalToolInfo = msg.ToolInfo

		// Add tool segment as pending
		m.segments = append(m.segments, ui.Segment{
			Type:       ui.SegmentTool,
			ToolName:   msg.ToolName,
			ToolInfo:   msg.ToolInfo,
			ToolStatus: ui.ToolPending,
		})

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
		if m.thinking {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

// renderSegments renders all segments with proper spacing
func (m askStreamModel) renderSegments() string {
	renderMd := func(text string, width int) string {
		rendered, err := renderMarkdown(text, width)
		if err != nil {
			return text
		}
		return rendered
	}
	return ui.RenderSegments(m.segments, m.width, m.wavePos, renderMd)
}

func (m askStreamModel) View() string {
	var b strings.Builder

	// Render all segments
	content := m.renderSegments()
	if content != "" {
		b.WriteString(content)
	}

	// If approval form is active, show it after content (no spinner during approval)
	if m.approvalForm != nil {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(m.approvalForm.View())
		return b.String()
	}

	// Show thinking spinner (only when waiting for LLM response)
	if m.thinking {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		indicator := ui.StreamingIndicator{
			Spinner:    m.spinner.View(),
			Phase:      "Thinking",
			Elapsed:    time.Since(m.startTime),
			Tokens:     m.totalTokens,
			Status:     m.retryStatus,
			ShowCancel: true,
		}.Render(m.styles)
		b.WriteString(indicator)
	}

	// Ensure trailing newline so final line isn't cut off
	if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}

	return b.String()
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
	go func() {
		_, err := p.Run()
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
			if ev.IsToolEnd {
				p.Send(askToolEndMsg{
					Name:    ev.Name,
					Info:    ev.Info,
					Success: ev.ToolSuccess,
				})
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
	// Note: Don't print finalOutput here - bubbletea's final View() already persists on screen
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
		fmt.Fprintf(errWriter, "\r✓ MCP ready: %d tools from %s\n\n", len(tools), strings.Join(serverNames, ", "))
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
