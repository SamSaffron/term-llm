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

Agent examples:
  term-llm ask --agent reviewer "Review this code" -f main.go
  term-llm ask -a commit "Write a commit message"
  term-llm ask -a editor "Add error handling to this function" -f utils.go

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
	askCmd.Flags().StringVarP(&askSystemMessage, "system-message", "m", "", "System message/instructions for the LLM (overrides config)")
	askCmd.Flags().StringVarP(&askAgent, "agent", "a", "", "Use an agent (named configuration bundle)")
	if err := askCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register provider completion: %v", err))
	}
	if err := askCmd.RegisterFlagCompletionFunc("mcp", MCPFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register mcp completion: %v", err))
	}
	if err := askCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register tools completion: %v", err))
	}
	if err := askCmd.RegisterFlagCompletionFunc("agent", AgentFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register agent completion: %v", err))
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
		templateCtx := agents.NewTemplateContext().WithFiles(askFiles)

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

	debugMode := askDebug
	req := llm.Request{
		Messages:            messages,
		Search:              askSearch,
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
		if !askSearch {
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
				// Skip tool indicator for ask_user - it has its own UI
				if event.ToolName == tools.AskUserToolName {
					continue
				}
				select {
				case toolEvents <- toolEvent{Name: event.ToolName, Info: event.ToolInfo}:
				default:
				}
			}
			if event.Type == llm.EventToolExecEnd {
				stats.ToolEnd()
				// Skip tool indicator for ask_user - it has its own UI
				if event.ToolName == tools.AskUserToolName {
					continue
				}
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
			if event.Type == llm.EventPhase && event.Text != "" {
				select {
				case toolEvents <- toolEvent{
					IsPhase: true,
					Phase:   event.Text,
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
	// Phase fields (when IsPhase is true)
	IsPhase bool
	Phase   string
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

	// Tool and segment tracking (shared component)
	tracker      *ui.ToolTracker
	printedLines int // Number of lines already printed to scrollback

	// State flags
	thinking bool // True only when waiting for LLM response (not during tools or streaming)

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

// maxViewLines is the maximum number of lines to keep in View().
// Content beyond this is printed to scrollback to prevent scroll issues.
const maxViewLines = 8

// maybeFlushToScrollback checks if content exceeds maxViewLines and prints
// excess to scrollback, keeping View() small to avoid terminal scroll issues.
func (m *askStreamModel) maybeFlushToScrollback() tea.Cmd {
	// Render current completed content
	completed := m.tracker.CompletedSegments()
	content := ui.RenderSegments(completed, m.width, -1, renderMd)
	totalLines := strings.Count(content, "\n")

	// If content exceeds threshold, print excess to scrollback
	if totalLines > maxViewLines+m.printedLines {
		// Find split point - print all but last maxViewLines
		lines := strings.Split(content, "\n")
		splitAt := len(lines) - maxViewLines
		if splitAt > m.printedLines {
			// Print lines from printedLines to splitAt
			toPrint := strings.Join(lines[m.printedLines:splitAt], "\n")
			m.printedLines = splitAt
			return tea.Println(toPrint)
		}
	}
	return nil
}

func (m askStreamModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle tool start messages even while approval form is active
	if toolMsg, ok := msg.(askToolStartMsg); ok && m.approvalForm != nil {
		if m.tracker.HandleToolStart(toolMsg.Name, toolMsg.Info) {
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
				rendered, err := renderMarkdown(m.tracker.Segments[i].Text, m.width)
				if err == nil {
					m.tracker.Segments[i].Rendered = rendered
				}
			}
		}

	case askContentMsg:
		m.thinking = false
		m.tracker.AddTextSegment(string(msg))

		// Flush excess content to scrollback to keep View() small
		if cmd := m.maybeFlushToScrollback(); cmd != nil {
			return m, cmd
		}

	case askDoneMsg:
		m.thinking = false
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
		m.thinking = false
		if m.tracker.HandleToolStart(msg.Name, msg.Info) {
			// New segment added, start wave animation
			return m, m.tracker.StartWave()
		}
		// Already have pending segment for this tool, just restart wave
		return m, m.tracker.StartWave()

	case askToolEndMsg:
		m.tracker.HandleToolEnd(msg.Name, msg.Success)

		// If no more pending tools, go back to thinking
		if !m.tracker.HasPending() {
			m.thinking = true
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
		if m.thinking {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func renderMd(text string, width int) string {
	if text == "" {
		return ""
	}
	rendered, _ := renderMarkdown(text, width)
	return rendered
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

	// Show thinking spinner or active tools
	if m.thinking || len(active) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		phase := m.phase
		if phase == "" {
			if m.thinking {
				phase = "Thinking"
			} else {
				phase = "Working"
			}
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
			if ev.IsPhase {
				p.Send(askPhaseMsg(ev.Phase))
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
	style := ui.GlamourStyle()
	margin := uint(0)
	style.Document.Margin = &margin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = &margin

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
