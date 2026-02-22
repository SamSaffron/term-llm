package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
	"github.com/samsaffron/term-llm/internal/tui/plan"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	planDebug          bool
	planProvider       string
	planMaxTurns       int
	planFile           string
	planSearch         bool
	planNoSearch       bool
	planNativeSearch   bool
	planNoNativeSearch bool
)

var planCmd = &cobra.Command{
	Use:   "plan [file]",
	Short: "Collaborative planning TUI",
	Long: `Start a collaborative planning TUI where you and an AI agent
edit a plan document together in real-time.

Examples:
  term-llm plan                      # Start with a new plan
  term-llm plan project.md           # Edit existing plan file
  term-llm plan --no-search          # Disable web search
  term-llm plan --provider chatgpt   # Use specific provider

Keyboard shortcuts:
  Ctrl+P       - Invoke planner agent
  Ctrl+S       - Save document
  Ctrl+C       - Cancel agent / Quit
  Esc          - Exit insert mode (vim)

The planner agent can:
- Add structure (headers, bullets, sections)
- Reorganize content
- Ask clarifying questions
- Make incremental edits

Your edits are preserved - the agent accounts for changes you make
while it's working.`,
	RunE: runPlan,
}

func init() {
	AddProviderFlag(planCmd, &planProvider)
	AddDebugFlag(planCmd, &planDebug)
	AddMaxTurnsFlag(planCmd, &planMaxTurns, 50)
	planCmd.Flags().BoolVarP(&planSearch, "search", "s", true, "Enable web search for current information")
	planCmd.Flags().BoolVar(&planNoSearch, "no-search", false, "Disable web search for this plan session")
	AddNativeSearchFlags(planCmd, &planNativeSearch, &planNoNativeSearch)

	planCmd.Flags().StringVarP(&planFile, "file", "f", "plan.md", "Plan file to edit")

	rootCmd.AddCommand(planCmd)
}

func runPlan(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Determine file path
	filePath := planFile
	if len(args) > 0 {
		filePath = args[0]
	}

	// Make path absolute
	if !filepath.IsAbs(filePath) {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		filePath = filepath.Join(cwd, filePath)
	}

	// Apply provider overrides
	if err := applyProviderOverridesWithAgent(cfg, cfg.Chat.Provider, cfg.Chat.Model, planProvider, "", ""); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	planSearchEnabled := resolvePlanSearch(planSearch, planNoSearch)

	// Create LLM provider and engine
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Create tool registry with investigation tools for the planner
	// The planner can glob, grep, read files, run shell commands to explore,
	// and spawn sub-agents (codebase, researcher) for deeper investigation
	toolConfig := &tools.ToolConfig{
		Enabled: []string{
			tools.AskUserToolName,
			tools.ReadFileToolName,
			tools.GlobToolName,
			tools.GrepToolName,
			tools.ShellToolName,
			tools.SpawnAgentToolName,
		},
	}

	// Get current working directory for permissions
	cwd, _ := os.Getwd()
	if cwd != "" {
		toolConfig.ReadDirs = []string{cwd}
		toolConfig.ShellAllow = []string{"*"} // Allow shell commands for investigation
	}

	// Configure spawn_agent: read-only agents only, conservative limits
	toolConfig.Spawn = tools.SpawnConfig{
		MaxParallel:    2,                                  // Conservative for plan mode
		MaxDepth:       1,                                  // Sub-agents don't spawn further sub-agents
		DefaultTimeout: 120,                                // 2 min per sub-agent task
		AllowedAgents:  []string{"codebase", "researcher"}, // Read-only agents only
	}

	perms, err := toolConfig.BuildPermissions()
	if err != nil {
		return err
	}
	approvalMgr := tools.NewApprovalManager(perms)
	registry, err := tools.NewLocalToolRegistry(toolConfig, cfg, approvalMgr)
	if err != nil {
		return err
	}
	wireImageRecorder(registry, "", "")

	engine := newEngine(provider, cfg)
	registry.RegisterWithEngine(engine)

	// Wire spawn_agent runner for sub-agent delegation
	toolMgr := &tools.ToolManager{Registry: registry, ApprovalMgr: approvalMgr}
	if err := WireSpawnAgentRunner(cfg, toolMgr, true /* yolo for read-only agents */); err != nil {
		return fmt.Errorf("setup spawn_agent for plan: %w", err)
	}

	// Set up debug logger if enabled
	debugLogger, debugLoggerErr := createDebugLogger(cfg)
	if debugLoggerErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", debugLoggerErr)
	}
	if debugLogger != nil {
		engine.SetDebugLogger(debugLogger)
		defer debugLogger.Close()
	}

	// Determine model name
	modelName := getModelName(cfg)

	// Resolve max turns
	maxTurns := planMaxTurns
	if maxTurns == 0 {
		maxTurns = 50
	}

	// Only enable alt-screen when stdout is a terminal
	useAltScreen := term.IsTerminal(int(os.Stdout.Fd()))

	// Resolve force external search setting
	forceExternalSearch := resolveForceExternalSearch(cfg, planNativeSearch, planNoNativeSearch)

	// Create plan model
	model := plan.New(cfg, provider, engine, modelName, maxTurns, filePath, planSearchEnabled, forceExternalSearch)

	// Load existing file if it exists
	if data, err := os.ReadFile(filePath); err == nil {
		model.LoadContent(string(data))
	}

	// Build program options
	var opts []tea.ProgramOption
	if useAltScreen {
		opts = append(opts, tea.WithAltScreen())
	}
	opts = append(opts, tea.WithMouseCellMotion()) // Enable mouse support

	// Run the TUI
	p := tea.NewProgram(model, opts...)

	// Set up program reference for ask_user handling
	model.SetProgram(p)

	// Set up spawn_agent event callback for subagent progress visibility
	if spawnTool := toolMgr.GetSpawnAgentTool(); spawnTool != nil {
		spawnTool.SetEventCallback(func(callID string, event tools.SubagentEvent) {
			go p.Send(plan.SubagentProgressMsg{CallID: callID, Event: event})
		})
	}

	// Set up ask_user handling for inline mode
	if useAltScreen {
		tools.SetAskUserUIFunc(func(questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
			doneCh := make(chan []tools.AskUserAnswer, 1)
			p.Send(plan.AskUserRequestMsg{
				Questions: questions,
				DoneCh:    doneCh,
			})
			select {
			case answers := <-doneCh:
				if answers == nil {
					return nil, fmt.Errorf("cancelled by user")
				}
				return answers, nil
			case <-ctx.Done():
				return nil, fmt.Errorf("cancelled: %w", ctx.Err())
			}
		})
		defer tools.ClearAskUserUIFunc()
	}

	// Wire signal handling to quit gracefully
	go func() {
		<-ctx.Done()
		p.Quit()
	}()

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("failed to run plan TUI: %w", err)
	}

	// Check for handoff to chat
	if fm, ok := finalModel.(*plan.Model); ok && fm.HandedOff() {
		planContent := fm.GetContent()
		agentName := fm.HandoffAgent()
		return runChatFromPlan(cfg, planContent, agentName, modelName, useAltScreen, forceExternalSearch, planSearchEnabled)
	}

	return nil
}

func resolvePlanSearch(searchFlag, noSearchFlag bool) bool {
	return searchFlag && !noSearchFlag
}

// runChatFromPlan launches a chat session with the plan content as system instructions.
func runChatFromPlan(cfg *config.Config, planContent string, agentName string, modelName string, useAltScreen bool, forceExternalSearch bool, searchEnabled bool) error {
	ctx, stop := signal.NotifyContext()
	defer stop()

	// Load agent if specified
	agent, err := LoadAgent(agentName, cfg)
	if err != nil {
		return fmt.Errorf("load agent: %w", err)
	}

	// Use agent's system prompt as-is (plan content goes as first user message)
	if agent != nil && agent.SystemPrompt != "" {
		cfg.Chat.Instructions = agent.SystemPrompt
	} else {
		cfg.Chat.Instructions = "You are a helpful assistant. Follow the user's plan step by step."
	}

	// Build the plan as the first user message
	planMessage := "I have a plan to execute. Here is the plan document:\n\n" + planContent + "\n\nPlease follow this plan step by step."
	autoSendQueue := []string{planMessage}

	// Apply agent provider/model overrides
	agentProvider, agentModel := "", ""
	if agent != nil {
		agentProvider, agentModel = agent.Provider, agent.Model
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Chat.Provider, cfg.Chat.Model, "", agentProvider, agentModel); err != nil {
		return err
	}
	modelName = getModelName(cfg)

	// Create a fresh provider and engine for the chat session
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Use the full chat tool registry (includes search, read_url, etc.)
	engine := newEngine(provider, cfg)

	// Resolve settings using agent configuration
	cliFlags := CLIFlags{
		Tools:  "all",
		Search: searchEnabled,
	}
	settings, err := ResolveSettings(cfg, agent, cliFlags, cfg.Chat.Provider, cfg.Chat.Model, cfg.Chat.Instructions, cfg.Chat.MaxTurns, 200)
	if err != nil {
		return err
	}

	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		return err
	}

	// Set up debug logger
	debugLogger, debugLoggerErr := createDebugLogger(cfg)
	if debugLoggerErr != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", debugLoggerErr)
	}
	if debugLogger != nil {
		engine.SetDebugLogger(debugLogger)
	}

	// Create MCP manager (empty, no servers for handoff)
	mcpManager := mcp.NewManager()

	// Resolve enabled local tools
	enabledLocalTools := tools.ParseToolsFlag(settings.Tools)

	// Create session store
	store, storeCleanup := InitSessionStore(cfg, os.Stderr)
	defer storeCleanup()

	// Create chat model
	model := chat.New(cfg, provider, engine, modelName, mcpManager, settings.MaxTurns, forceExternalSearch, settings.Search, enabledLocalTools, settings.Tools, "", false, "", store, nil, useAltScreen, autoSendQueue, false, false, agentName, false)

	// Build program options
	var opts []tea.ProgramOption
	if useAltScreen {
		opts = append(opts, tea.WithAltScreen())
	}
	opts = append(opts, tea.WithMouseCellMotion())

	p := tea.NewProgram(model, opts...)

	// Set up approval UI
	if toolMgr != nil {
		toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
			if useAltScreen {
				doneCh := make(chan tools.ApprovalResult, 1)
				p.Send(chat.ApprovalRequestMsg{
					Path:    path,
					IsWrite: isWrite,
					IsShell: isShell,
					DoneCh:  doneCh,
				})
				select {
				case result := <-doneCh:
					return result, nil
				case <-ctx.Done():
					return tools.ApprovalResult{Choice: tools.ApprovalChoiceDeny}, fmt.Errorf("cancelled: %w", ctx.Err())
				}
			}
			p.ReleaseTerminal()
			defer func() {
				p.RestoreTerminal()
				p.Send(chat.ResumeFromExternalUIMsg{})
			}()
			if isShell {
				return tools.RunShellApprovalUI(path)
			}
			return tools.RunFileApprovalUI(path, isWrite)
		}
	}

	// Set up ask_user handling
	if useAltScreen {
		tools.SetAskUserUIFunc(func(questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
			doneCh := make(chan []tools.AskUserAnswer, 1)
			p.Send(chat.AskUserRequestMsg{
				Questions: questions,
				DoneCh:    doneCh,
			})
			select {
			case answers := <-doneCh:
				if answers == nil {
					return nil, fmt.Errorf("cancelled by user")
				}
				return answers, nil
			case <-ctx.Done():
				return nil, fmt.Errorf("cancelled: %w", ctx.Err())
			}
		})
		defer tools.ClearAskUserUIFunc()
	}

	// Wire signal handling
	go func() {
		<-ctx.Done()
		p.Quit()
	}()

	_, err = p.Run()

	mcpManager.StopAll()
	if debugLogger != nil {
		debugLogger.Close()
	}

	if err != nil {
		return fmt.Errorf("failed to run chat: %w", err)
	}
	return nil
}
