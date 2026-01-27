package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/plan"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	planDebug    bool
	planProvider string
	planMaxTurns int
	planFile     string
)

var planCmd = &cobra.Command{
	Use:   "plan [file]",
	Short: "Collaborative planning TUI",
	Long: `Start a collaborative planning TUI where you and an AI agent
edit a plan document together in real-time.

Examples:
  term-llm plan                      # Start with a new plan
  term-llm plan project.md           # Edit existing plan file
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

	// Create LLM provider and engine
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Create tool registry with investigation tools for the planner
	// The planner can glob, grep, read files, and run shell commands to explore
	toolConfig := &tools.ToolConfig{
		Enabled: []string{
			tools.AskUserToolName,
			tools.ReadFileToolName,
			tools.GlobToolName,
			tools.GrepToolName,
			tools.ShellToolName,
		},
	}

	// Get current working directory for permissions
	cwd, _ := os.Getwd()
	if cwd != "" {
		toolConfig.ReadDirs = []string{cwd}
		toolConfig.ShellAllow = []string{"*"} // Allow shell commands for investigation
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

	engine := llm.NewEngine(provider, llm.NewToolRegistry())
	registry.RegisterWithEngine(engine)

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

	// Create plan model
	model := plan.New(cfg, provider, engine, modelName, maxTurns, filePath)

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

	_, err = p.Run()
	if err != nil {
		return fmt.Errorf("failed to run plan TUI: %w", err)
	}

	return nil
}
