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
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/agents"
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
	askTools         string
	askReadDirs      []string
	askWriteDirs     []string
	askShellAllow    []string
	askSystemMessage string
	// Agent flag
	askAgent string
)

var askCmd = &cobra.Command{
	Use:   "ask [@agent] <question>",
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

Agent examples (use @agent shortcut or --agent flag):
  term-llm ask @reviewer "Review this code" -f main.go
  term-llm ask @commit "Write a commit message"
  term-llm ask @editor "Add error handling" -f utils.go
  term-llm ask --agent researcher "Find info about Go 1.22"

Line range syntax for files:
  main.go       - Include entire file
  main.go:11-22 - Include only lines 11-22
  main.go:11-   - Include lines 11 to end of file
  main.go:-22   - Include lines 1-22`,
	Args:              cobra.MinimumNArgs(1),
	RunE:              runAsk,
	ValidArgsFunction: AtAgentCompletion,
}

func init() {
	// Common flags shared across commands
	AddProviderFlag(askCmd, &askProvider)
	AddDebugFlag(askCmd, &askDebug)
	AddSearchFlag(askCmd, &askSearch)
	AddNativeSearchFlags(askCmd, &askNativeSearch, &askNoNativeSearch)
	AddMCPFlag(askCmd, &askMCP)
	AddMaxTurnsFlag(askCmd, &askMaxTurns, 20)
	AddToolFlags(askCmd, &askTools, &askReadDirs, &askWriteDirs, &askShellAllow)
	AddSystemMessageFlag(askCmd, &askSystemMessage)
	AddFileFlag(askCmd, &askFiles, "File(s) to include as context (supports globs, line ranges like file.go:10-20, 'clipboard')")
	AddAgentFlag(askCmd, &askAgent)

	// Ask-specific flags
	askCmd.Flags().BoolVarP(&askText, "text", "t", false, "Output plain text instead of rendered markdown")

	// Additional completions
	if err := askCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register agent completion: %v", err))
	}
	rootCmd.AddCommand(askCmd)
}

func runAsk(cmd *cobra.Command, args []string) error {
	// Extract @agent from args if present
	atAgent, filteredArgs := ExtractAgentFromArgs(args)
	if atAgent != "" && askAgent == "" {
		askAgent = atAgent
	}

	question := strings.Join(filteredArgs, " ")
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Load agent if specified
	var agent *agents.Agent
	if askAgent != "" {
		registry, err := agents.NewRegistry(agents.RegistryConfig{
			UseBuiltin:  cfg.Agents.UseBuiltin,
			SearchPaths: cfg.Agents.SearchPaths,
		})
		if err != nil {
			return fmt.Errorf("create agent registry: %w", err)
		}

		agent, err = registry.Get(askAgent)
		if err != nil {
			return fmt.Errorf("load agent: %w", err)
		}

		if err := agent.Validate(); err != nil {
			return fmt.Errorf("invalid agent: %w", err)
		}
	}

	// Apply provider overrides: CLI > agent > config
	agentProvider := ""
	agentModel := ""
	if agent != nil {
		agentProvider = agent.Provider
		agentModel = agent.Model
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, askProvider, agentProvider, agentModel); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := llm.NewEngine(provider, defaultToolRegistry(cfg))

	// Set up debug logger if enabled
	debugLogger, err := createDebugLogger(cfg)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
	}
	if debugLogger != nil {
		engine.SetDebugLogger(debugLogger)
		defer debugLogger.Close()
	}

	// Determine tool settings: CLI > agent > none
	effectiveTools := askTools
	effectiveReadDirs := askReadDirs
	effectiveWriteDirs := askWriteDirs
	effectiveShellAllow := askShellAllow
	shellAutoRun := false
	var scriptCommands []string

	if agent != nil && effectiveTools == "" {
		// Use agent tool settings
		if agent.HasEnabledList() {
			effectiveTools = strings.Join(agent.Tools.Enabled, ",")
		} else if agent.HasDisabledList() {
			// Get all tools and exclude disabled ones
			allTools := tools.AllToolNames()
			enabledTools := agent.GetEnabledTools(allTools)
			effectiveTools = strings.Join(enabledTools, ",")
		}

		// Agent-specific tool settings
		if len(agent.Read.Dirs) > 0 {
			effectiveReadDirs = agent.Read.Dirs
		}
		if len(agent.Shell.Allow) > 0 {
			effectiveShellAllow = agent.Shell.Allow
		}
		shellAutoRun = agent.Shell.AutoRun

		// Extract script commands from agent
		if len(agent.Shell.Scripts) > 0 {
			for _, script := range agent.Shell.Scripts {
				scriptCommands = append(scriptCommands, script)
			}
		}
	}

	// Initialize local tools if we have any
	var toolMgr *tools.ToolManager
	if effectiveTools != "" {
		toolConfig := buildToolConfig(effectiveTools, effectiveReadDirs, effectiveWriteDirs, effectiveShellAllow, cfg)
		if shellAutoRun {
			toolConfig.ShellAutoRun = true
		}
		if len(scriptCommands) > 0 {
			toolConfig.ScriptCommands = append(toolConfig.ScriptCommands, scriptCommands...)
		}
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

	// Determine MCP servers: CLI > agent
	effectiveMCP := askMCP
	if agent != nil && effectiveMCP == "" {
		mcpServers := agent.GetMCPServerNames()
		if len(mcpServers) > 0 {
			effectiveMCP = strings.Join(mcpServers, ",")
		}
	}

	// Initialize MCP servers if any
	var mcpManager *mcp.Manager
	if effectiveMCP != "" {
		mcpManager, err = enableMCPServersWithFeedback(ctx, effectiveMCP, engine, cmd.ErrOrStderr())
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

	// Determine system instructions: CLI > agent > config
	instructions := cfg.Ask.Instructions
	if agent != nil && agent.SystemPrompt != "" {
		// Expand template variables in agent system prompt
		// Use NewTemplateContextForTemplate to avoid expensive git operations
		// when the template doesn't use them
		templateCtx := agents.NewTemplateContextForTemplate(agent.SystemPrompt).WithFiles(askFiles)

		// Extract resources for builtin agents and set resource_dir
		if agents.IsBuiltinAgent(agent.Name) {
			if resourceDir, err := agents.ExtractBuiltinResources(agent.Name); err == nil {
				templateCtx = templateCtx.WithResourceDir(resourceDir)
			}
		}

		instructions = agents.ExpandTemplate(agent.SystemPrompt, templateCtx)
	}
	if askSystemMessage != "" {
		instructions = askSystemMessage
	}
	if instructions != "" {
		messages = append(messages, llm.SystemText(instructions))
	}
	messages = append(messages, llm.UserText(userPrompt))

	// Determine max turns: CLI default check > agent > CLI default
	effectiveMaxTurns := askMaxTurns
	if agent != nil && agent.MaxTurns > 0 && !cmd.Flags().Changed("max-turns") {
		effectiveMaxTurns = agent.MaxTurns
	}

	// Determine effective search: CLI flag or agent setting
	effectiveSearch := askSearch
	if agent != nil && agent.Search {
		effectiveSearch = true
	}

	debugMode := askDebug
	req := llm.Request{
		Messages:            messages,
		Search:              effectiveSearch,
		ForceExternalSearch: resolveForceExternalSearch(cfg, askNativeSearch, askNoNativeSearch),
		ParallelToolCalls:   true,
		MaxTurns:            effectiveMaxTurns,
		Debug:               debugMode,
		DebugRaw:            debugRaw,
	}

	// Add tools to request if any are registered (local or MCP)
	if toolMgr != nil || mcpManager != nil {
		allSpecs := engine.Tools().AllSpecs()
		// Filter out search tools unless search is enabled
		// (Engine adds them automatically when req.Search is true)
		if !effectiveSearch {
			var filtered []llm.ToolSpec
			for _, spec := range allSpecs {
				if spec.Name != llm.WebSearchToolName && spec.Name != llm.ReadURLToolName {
					filtered = append(filtered, spec)
				}
			}
			req.Tools = filtered
		} else {
			req.Tools = allSpecs
		}
		req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}

	// Check if we're in a TTY and can use glamour
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	useGlamour := !askText && isTTY && !debugRaw

	// Create stream adapter for unified event handling with proper buffering
	adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
	adapter.ToolNameFilter = tools.AskUserToolName // Skip ask_user tool (has its own UI)

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
		// Set up ask_user hooks to pause/resume the TUI during the interactive UI
		tools.SetAskUserHooks(
			func() {
				// Flush content to scrollback before releasing terminal
				done := make(chan struct{})
				teaProgram.Send(askFlushBeforeAskUserMsg{Done: done})
				<-done // Wait for flush to complete
				teaProgram.ReleaseTerminal()
			},
			func() { teaProgram.RestoreTerminal() },
		)
	}

	errChan := make(chan error, 1)
	go func() {
		stream, err := engine.Stream(ctx, req)
		if err != nil {
			errChan <- err
			return
		}
		defer stream.Close()
		// ProcessStream handles all events and closes the channel when done
		adapter.ProcessStream(ctx, stream)
		errChan <- nil
	}()

	if useGlamour {
		err = streamWithGlamour(ctx, adapter.Events(), teaProgram)
	} else {
		err = streamPlainText(ctx, adapter.Events())
	}
	tools.ClearAskUserHooks() // Safe to call even if hooks weren't set

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
		adapter.Stats().Finalize()
		fmt.Fprintln(cmd.ErrOrStderr(), adapter.Stats().Render())
	}

	return nil
}

// streamPlainText streams text directly without formatting
func streamPlainText(ctx context.Context, events <-chan ui.StreamEvent) error {
	// Track pending tools with their status
	type toolEntry struct {
		callID  string
		name    string
		info    string
		success bool
		done    bool
	}
	var pendingTools []toolEntry
	printedAny := false
	lastEndedWithNewline := true

	printTools := func() {
		if len(pendingTools) == 0 {
			return
		}
		if printedAny && !lastEndedWithNewline {
			fmt.Print("\n")
		}
		if printedAny {
			fmt.Print("\n")
		}
		for _, t := range pendingTools {
			phase := ui.FormatToolPhase(t.name, t.info)
			if t.success {
				fmt.Printf("%s %s\n", ui.SuccessCircle(), phase.Completed)
			} else {
				fmt.Printf("%s %s\n", ui.ErrorCircle(), phase.Completed)
			}
		}
		fmt.Print("\n")
		pendingTools = nil
		printedAny = true
		lastEndedWithNewline = true
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				if len(pendingTools) > 0 {
					printTools()
				}
				fmt.Println()
				return nil
			}

			switch ev.Type {
			case ui.StreamEventRetry:
				fmt.Fprintf(os.Stderr, "\rRate limited (%d/%d), waiting %.0fs...\n",
					ev.RetryAttempt, ev.RetryMax, ev.RetryWait)

			case ui.StreamEventUsage:
				// Skip usage events in plain text mode
				continue

			case ui.StreamEventPhase:
				// Skip phase events in plain text mode
				continue

			case ui.StreamEventToolEnd:
				// Find and update the tool entry by callID
				for i := range pendingTools {
					if pendingTools[i].callID == ev.ToolCallID && !pendingTools[i].done {
						pendingTools[i].success = ev.ToolSuccess
						pendingTools[i].done = true
						break
					}
				}
				// Check if all tools are done
				allDone := true
				for _, t := range pendingTools {
					if !t.done {
						allDone = false
						break
					}
				}
				if allDone && len(pendingTools) > 0 {
					printTools()
				}

			case ui.StreamEventToolStart:
				pendingTools = append(pendingTools, toolEntry{
					callID: ev.ToolCallID,
					name:   ev.ToolName,
					info:   ev.ToolInfo,
				})

			case ui.StreamEventText:
				fmt.Print(ev.Text)
				printedAny = true
				if len(ev.Text) > 0 {
					lastEndedWithNewline = strings.HasSuffix(ev.Text, "\n")
				}

			case ui.StreamEventDone:
				if len(pendingTools) > 0 {
					printTools()
				}
				fmt.Println()
				return nil

			case ui.StreamEventError:
				return ev.Err
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

	// Tool and segment tracking (shared component)
	tracker      *ui.ToolTracker
	printedLines int // Number of lines already printed to scrollback

	// State flags
	done bool // True when streaming is complete (prevents spinner from showing)

	// Status display
	retryStatus string    // Retry status (e.g., "Rate limited (2/5), waiting 5s...")
	startTime   time.Time // For elapsed time display
	totalTokens int       // Total tokens (input + output) used
	phase       string    // Current engine phase (Thinking, Searching, etc.)

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
	CallID string // Unique ID for this tool invocation
	Name   string // Tool name being executed
	Info   string // Additional info (e.g., URL)
}
type askToolEndMsg struct {
	CallID  string // Unique ID for this tool invocation
	Success bool   // Whether the tool succeeded
}
type askRetryMsg struct {
	Attempt     int
	MaxAttempts int
	WaitSecs    float64
}
type askPhaseMsg string
type askFlushBeforeAskUserMsg struct {
	Done chan<- struct{} // Signal when flush is complete
}
type askApprovalRequestMsg struct {
	Description string
	ToolName    string
	ToolInfo    string
	ResponseCh  chan<- bool
}

// Use ui.WaveTickMsg and ui.WavePauseMsg from the shared ToolTracker

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
		tracker:   ui.NewToolTracker(),
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

// maxViewLines is the maximum number of lines to keep in View().
// Content beyond this is printed to scrollback to prevent scroll issues.
const maxViewLines = 8

// maybeFlushToScrollback checks if content exceeds maxViewLines and prints
// excess to scrollback, keeping View() small to avoid terminal scroll issues.
func (m *askStreamModel) maybeFlushToScrollback() tea.Cmd {
	result := m.tracker.FlushToScrollback(m.width, m.printedLines, maxViewLines, renderMd)
	if result.ToPrint != "" {
		m.printedLines = result.NewPrintedLines
		return tea.Println(result.ToPrint)
	}
	m.printedLines = result.NewPrintedLines
	return nil
}

func (m askStreamModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle tool start messages even while approval form is active
	if toolMsg, ok := msg.(askToolStartMsg); ok && m.approvalForm != nil {
		if m.tracker.HandleToolStart(toolMsg.CallID, toolMsg.Name, toolMsg.Info) {
			// New segment added, but don't start wave yet (approval form is active)
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
			// Send response - the tool segment will be updated by askToolEndMsg
			// when the tool actually completes (not when approval is granted)
			if m.approvalResponseCh != nil {
				m.approvalResponseCh <- approved
			}
			m.approvalForm = nil
			m.approvalResponseCh = nil
			m.approvalDesc = ""
			m.approvalToolInfo = ""
			// If there are pending tools, restart wave animation
			if m.tracker.HasPending() {
				return m, m.tracker.StartWave()
			}
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
		for i := range m.tracker.Segments {
			if m.tracker.Segments[i].Type == ui.SegmentText && m.tracker.Segments[i].Complete {
				m.tracker.Segments[i].Rendered = ui.RenderMarkdown(m.tracker.Segments[i].Text, m.width)
			}
		}

	case askContentMsg:
		m.tracker.AddTextSegment(string(msg))

		// Flush excess content to scrollback to keep View() small
		if cmd := m.maybeFlushToScrollback(); cmd != nil {
			return m, cmd
		}

	case askDoneMsg:
		m.done = true // Prevent spinner from showing in final View()

		// Mark all text segments as complete but don't pre-render.
		// This ensures RenderSegments uses the same renderMarkdown path
		// as streaming, keeping line counts consistent for scrollback tracking.
		m.tracker.CompleteTextSegments(nil)

		// Print any remaining content to scrollback before quitting
		completed := m.tracker.CompletedSegments()
		content := ui.RenderSegments(completed, m.width, -1, renderMd)

		if content != "" {
			lines := strings.Split(content, "\n")
			if m.printedLines < len(lines) {
				remaining := strings.Join(lines[m.printedLines:], "\n")
				// Mark all lines as printed so View() returns empty
				m.printedLines = len(lines)
				return m, tea.Sequence(tea.Println(remaining), tea.Quit)
			}
		}
		return m, tea.Quit

	case askCancelledMsg:
		m.done = true
		return m, tea.Quit

	case askUsageMsg:
		m.totalTokens = msg.InputTokens + msg.OutputTokens

	case askTickMsg:
		// Stop ticking when done
		if m.done {
			return m, nil
		}
		// Continue ticking for elapsed time and idle detection updates
		return m, m.tickEvery()

	case askRetryMsg:
		m.retryStatus = fmt.Sprintf("Rate limited (%d/%d), waiting %.0fs...",
			msg.Attempt, msg.MaxAttempts, msg.WaitSecs)
		return m, m.tickEvery()

	case askPhaseMsg:
		m.phase = string(msg)
		return m, nil

	case askFlushBeforeAskUserMsg:
		// Flush all completed content to scrollback before ask_user takes over terminal
		completed := m.tracker.CompletedSegments()
		content := ui.RenderSegments(completed, m.width, -1, renderMd)

		var cmds []tea.Cmd
		if content != "" {
			lines := strings.Split(content, "\n")
			if m.printedLines < len(lines) {
				toPrint := strings.Join(lines[m.printedLines:], "\n")
				m.printedLines = len(lines)
				cmds = append(cmds, tea.Println(toPrint))
			}
		}

		// Signal that flush is complete (use a command to ensure tea.Println finishes first)
		cmds = append(cmds, func() tea.Msg {
			close(msg.Done)
			return nil
		})
		return m, tea.Sequence(cmds...)

	case askToolStartMsg:
		m.retryStatus = ""
		if m.tracker.HandleToolStart(msg.CallID, msg.Name, msg.Info) {
			// New segment added, start wave animation
			return m, m.tracker.StartWave()
		}
		// Already have pending segment for this call, just restart wave
		return m, m.tracker.StartWave()

	case askToolEndMsg:
		m.tracker.HandleToolEnd(msg.CallID, msg.Success)

		// If no more pending tools, start spinner for idle state
		if !m.tracker.HasPending() {
			return m, m.spinner.Tick
		}

	case ui.WaveTickMsg:
		if cmd := m.tracker.HandleWaveTick(); cmd != nil {
			return m, cmd
		}

	case ui.WavePauseMsg:
		if cmd := m.tracker.HandleWavePause(); cmd != nil {
			return m, cmd
		}

	case askApprovalRequestMsg:
		m.approvalDesc = msg.Description
		m.approvalResponseCh = msg.ResponseCh
		m.approvalToolInfo = msg.ToolInfo

		// Don't add a new segment - the tool already has a pending segment from askToolStartMsg.
		// The approval is part of that tool's execution, not a separate operation.

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
		// Always maintain the tick chain to keep spinner animating
		// The idle check in View() controls visibility, not animation
		// Don't update spinner when done
		if !m.done {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func renderMd(text string, width int) string {
	return ui.RenderMarkdown(text, width)
}

func (m askStreamModel) View() string {
	var b strings.Builder

	// Get segments from tracker
	completed := m.tracker.CompletedSegments()
	active := m.tracker.ActiveSegments()

	// Render completed segments
	content := ui.RenderSegments(completed, m.width, -1, renderMd)

	// Only show content after what's been printed to scrollback
	if m.printedLines > 0 && content != "" {
		lines := strings.Split(content, "\n")
		if m.printedLines < len(lines) {
			content = strings.Join(lines[m.printedLines:], "\n")
		} else {
			content = ""
		}
	}

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

	// Show spinner when idle (no activity for >1s) or when tools are active
	// Don't show spinner when done - we're about to quit
	if !m.done && (len(active) > 0 || m.tracker.IsIdle(time.Second)) {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		phase := m.phase
		if phase == "" {
			phase = "Thinking"
		}

		indicator := ui.StreamingIndicator{
			Spinner:        m.spinner.View(),
			Phase:          phase,
			Elapsed:        time.Since(m.startTime),
			Tokens:         m.totalTokens,
			Status:         m.retryStatus,
			ShowCancel:     true,
			Segments:       active,
			WavePos:        m.tracker.WavePos,
			Width:          m.width,
			RenderMarkdown: renderMd,
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
func streamWithGlamour(ctx context.Context, events <-chan ui.StreamEvent, p *tea.Program) error {
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

	var streamErr error
	for events != nil {
		select {
		case <-ctx.Done():
			p.Send(askCancelledMsg{})
			events = nil

		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}

			switch ev.Type {
			case ui.StreamEventRetry:
				p.Send(askRetryMsg{
					Attempt:     ev.RetryAttempt,
					MaxAttempts: ev.RetryMax,
					WaitSecs:    ev.RetryWait,
				})

			case ui.StreamEventUsage:
				p.Send(askUsageMsg{
					InputTokens:  ev.InputTokens,
					OutputTokens: ev.OutputTokens,
				})

			case ui.StreamEventPhase:
				p.Send(askPhaseMsg(ev.Phase))

			case ui.StreamEventToolEnd:
				p.Send(askToolEndMsg{
					CallID:  ev.ToolCallID,
					Success: ev.ToolSuccess,
				})

			case ui.StreamEventToolStart:
				p.Send(askToolStartMsg{
					CallID: ev.ToolCallID,
					Name:   ev.ToolName,
					Info:   ev.ToolInfo,
				})

			case ui.StreamEventText:
				p.Send(askContentMsg(ev.Text))

			case ui.StreamEventDone:
				p.Send(askDoneMsg{})
				events = nil

			case ui.StreamEventError:
				if ev.Err != nil {
					streamErr = ev.Err
					p.Send(askCancelledMsg{})
					events = nil
				}
			}
		}
	}

	err := <-programDone
	// Note: Don't print finalOutput here - bubbletea's final View() already persists on screen
	if streamErr != nil {
		return streamErr
	}
	return err
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
	for s := range strings.SplitSeq(mcpFlag, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			servers = append(servers, s)
		}
	}
	return servers
}
