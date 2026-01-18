package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	loopDebug          bool
	loopSearch         bool
	loopProvider       string
	loopMCP            string
	loopMaxTurns       int
	loopNativeSearch   bool
	loopNoNativeSearch bool
	// Tool flags
	loopTools         string
	loopReadDirs      []string
	loopWriteDirs     []string
	loopShellAllow    []string
	loopSystemMessage string
	// Agent flag
	loopAgent string
	// Yolo mode
	loopYolo bool
	// Loop-specific flags
	loopDone     string // Command that returns 0 when done
	loopDoneFile string // FILE:TEXT - done when file contains text
	loopMax      int    // Maximum iterations
	loopHistory  int    // Number of iteration summaries to inject
)

var loopCmd = &cobra.Command{
	Use:   "loop [@agent] <prompt>",
	Short: "Run an autonomous agent loop until completion",
	Long: `Run an agent in a loop until a completion condition is met.

The agent runs repeatedly with fresh context each iteration. State persists
in the filesystem - the agent reads/writes files to track progress.

Completion conditions:
  --done "cmd"           Exit when command returns 0
  --done-file FILE:TEXT  Exit when file contains TEXT

File expansion:
  Use {{file.md}} in the prompt to inline file contents. Files are
  re-read each iteration, so agents can update them for inter-iteration state.

History:
  Use --history N to inject summaries of the last N iterations into the prompt.
  This helps the agent avoid repeating failed approaches.

Examples:
  # Run until tests pass
  term-llm loop --done "go test ./..." "fix the failing tests"

  # Run until file contains completion marker
  term-llm loop --done-file TODO.md:COMPLETE \
    "Implement features in {{TODO.md}}. Mark COMPLETE when done."

  # With tools and iteration history
  term-llm loop --done "npm test" --tools all --history 3 \
    "Fix the tests. Don't repeat failed approaches."

  # Using an agent
  term-llm loop @coder --done "make build" --max 20 \
    "Implement the feature described in {{SPEC.md}}"`,
	Args:              cobra.MinimumNArgs(1),
	RunE:              runLoop,
	ValidArgsFunction: AtAgentCompletion,
}

func init() {
	// Common flags shared across commands
	AddProviderFlag(loopCmd, &loopProvider)
	AddDebugFlag(loopCmd, &loopDebug)
	AddSearchFlag(loopCmd, &loopSearch)
	AddNativeSearchFlags(loopCmd, &loopNativeSearch, &loopNoNativeSearch)
	AddMCPFlag(loopCmd, &loopMCP)
	AddMaxTurnsFlag(loopCmd, &loopMaxTurns, 100) // Higher default for loop
	AddToolFlags(loopCmd, &loopTools, &loopReadDirs, &loopWriteDirs, &loopShellAllow)
	AddSystemMessageFlag(loopCmd, &loopSystemMessage)
	AddAgentFlag(loopCmd, &loopAgent)
	AddYoloFlag(loopCmd, &loopYolo)

	// Loop-specific flags
	loopCmd.Flags().StringVar(&loopDone, "done", "", "Command that signals completion when it returns 0")
	loopCmd.Flags().StringVar(&loopDoneFile, "done-file", "", "FILE:TEXT - complete when file contains text")
	loopCmd.Flags().IntVar(&loopMax, "max", 0, "Maximum number of iterations (0 = unlimited)")
	loopCmd.Flags().IntVar(&loopHistory, "history", 0, "Number of iteration summaries to inject into prompt")

	// Additional completions
	if err := loopCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register tools completion: %v", err))
	}
	rootCmd.AddCommand(loopCmd)
}

// iterationHistory tracks summaries of past iterations for --history
type iterationHistory struct {
	maxSize  int
	entries  []iterationEntry
	capacity int
}

type iterationEntry struct {
	iteration int
	summary   string
	duration  time.Duration
}

func newIterationHistory(size int) *iterationHistory {
	if size <= 0 {
		return nil
	}
	return &iterationHistory{
		maxSize:  size,
		entries:  make([]iterationEntry, 0, size),
		capacity: size,
	}
}

func (h *iterationHistory) add(iteration int, summary string, duration time.Duration) {
	if h == nil {
		return
	}
	entry := iterationEntry{
		iteration: iteration,
		summary:   summary,
		duration:  duration,
	}
	if len(h.entries) >= h.capacity {
		// Shift entries left, dropping oldest
		copy(h.entries, h.entries[1:])
		h.entries[len(h.entries)-1] = entry
	} else {
		h.entries = append(h.entries, entry)
	}
}

func (h *iterationHistory) render() string {
	if h == nil || len(h.entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Previous Iterations\n\n")
	for _, e := range h.entries {
		b.WriteString(fmt.Sprintf("### Iteration %d (%.0fs)\n%s\n\n", e.iteration, e.duration.Seconds(), e.summary))
	}
	return b.String()
}

// expandPrompt replaces {{file}} patterns with file contents
func expandPrompt(prompt string) string {
	re := regexp.MustCompile(`\{\{([^}]+)\}\}`)
	return re.ReplaceAllStringFunc(prompt, func(match string) string {
		path := strings.TrimSpace(match[2 : len(match)-2])
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("[ERROR: cannot read %s: %v]", path, err)
		}
		return string(content)
	})
}

// checkDoneCommand runs the done command and returns true if it exits 0
func checkDoneCommand(ctx context.Context, doneCmd string) bool {
	if doneCmd == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", doneCmd)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return err == nil
}

// checkDoneFile checks if file contains the specified text
func checkDoneFile(doneFileSpec string) bool {
	if doneFileSpec == "" {
		return false
	}
	parts := strings.SplitN(doneFileSpec, ":", 2)
	if len(parts) != 2 {
		return false
	}
	filePath := parts[0]
	searchText := parts[1]

	content, err := os.ReadFile(filePath)
	if err != nil {
		return false
	}
	return strings.Contains(string(content), searchText)
}

func runLoop(cmd *cobra.Command, args []string) error {
	// Extract @agent from args if present
	atAgent, filteredArgs := ExtractAgentFromArgs(args)
	if atAgent != "" && loopAgent == "" {
		loopAgent = atAgent
	}

	basePrompt := strings.Join(filteredArgs, " ")
	ctx, stop := signal.NotifyContext()
	defer stop()

	// Validate we have at least one completion condition
	if loopDone == "" && loopDoneFile == "" {
		return fmt.Errorf("at least one completion condition required: --done or --done-file")
	}

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Load agent if specified
	agent, err := LoadAgent(loopAgent, cfg)
	if err != nil {
		return err
	}

	// Resolve all settings: CLI > agent > config
	settings := ResolveSettings(cfg, agent, CLIFlags{
		Provider:      loopProvider,
		Tools:         loopTools,
		ReadDirs:      loopReadDirs,
		WriteDirs:     loopWriteDirs,
		ShellAllow:    loopShellAllow,
		MCP:           loopMCP,
		SystemMessage: loopSystemMessage,
		MaxTurns:      loopMaxTurns,
		MaxTurnsSet:   cmd.Flags().Changed("max-turns"),
		Search:        loopSearch,
	}, cfg.Ask.Provider, cfg.Ask.Model, cfg.Ask.Instructions, cfg.Ask.MaxTurns, 100)

	// Apply provider overrides
	agentProvider, agentModel := "", ""
	if agent != nil {
		agentProvider, agentModel = agent.Provider, agent.Model
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, loopProvider, agentProvider, agentModel); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider and engine
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := llm.NewEngine(provider, defaultToolRegistry(cfg))

	// Set up debug logger if enabled
	debugLogger, debugLoggerErr := createDebugLogger(cfg)
	if debugLoggerErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", debugLoggerErr)
	}
	if debugLogger != nil {
		engine.SetDebugLogger(debugLogger)
		defer debugLogger.Close()
	}

	// Initialize tools
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		return err
	}
	if toolMgr != nil {
		// In loop mode, we use yolo (auto-approve) by default for headless operation
		if loopYolo {
			toolMgr.ApprovalMgr.SetYoloMode(true)
		} else {
			// Non-yolo mode: use huh approval prompts
			toolMgr.ApprovalMgr.PromptFunc = tools.HuhApprovalPrompt
		}

		// Wire spawn_agent runner if enabled
		if err := WireSpawnAgentRunner(cfg, toolMgr, loopYolo); err != nil {
			return err
		}
	}

	// Initialize MCP servers
	var mcpManager *mcp.Manager
	if settings.MCP != "" {
		mcpManager, err = enableMCPServersWithFeedback(ctx, settings.MCP, engine, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		if mcpManager != nil {
			defer mcpManager.StopAll()
		}
	}

	// Get tool specs
	var toolSpecs []llm.ToolSpec
	if toolMgr != nil || mcpManager != nil {
		toolSpecs = engine.Tools().AllSpecs()
	}

	// Force external search setting
	forceExternalSearch := resolveForceExternalSearch(cfg, loopNativeSearch, loopNoNativeSearch)

	// Initialize history tracker
	history := newIterationHistory(loopHistory)

	// Main loop
	iteration := 0
	for {
		iteration++

		// Check completion conditions FIRST (fast exit)
		if checkDoneCommand(ctx, loopDone) {
			fmt.Fprintf(cmd.ErrOrStderr(), "\n[loop] Done condition met after %d iterations\n", iteration-1)
			return nil
		}
		if checkDoneFile(loopDoneFile) {
			fmt.Fprintf(cmd.ErrOrStderr(), "\n[loop] Done condition met after %d iterations\n", iteration-1)
			return nil
		}

		// Check max iterations
		if loopMax > 0 && iteration > loopMax {
			fmt.Fprintf(cmd.ErrOrStderr(), "\n[loop] Max iterations (%d) reached\n", loopMax)
			return nil
		}

		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Expand prompt with current file contents
		expandedPrompt := expandPrompt(basePrompt)

		// Add history if enabled
		if history != nil {
			expandedPrompt += history.render()
		}

		// Build messages
		messages := []llm.Message{}
		if settings.SystemPrompt != "" {
			messages = append(messages, llm.SystemText(settings.SystemPrompt))
		}
		messages = append(messages, llm.UserText(expandedPrompt))

		// Build request
		req := llm.Request{
			Messages:            messages,
			Tools:               toolSpecs,
			ToolChoice:          llm.ToolChoice{Mode: llm.ToolChoiceAuto},
			ParallelToolCalls:   true,
			Search:              settings.Search,
			ForceExternalSearch: forceExternalSearch,
			MaxTurns:            settings.MaxTurns,
			Debug:               loopDebug,
			DebugRaw:            debugRaw,
		}

		// Print iteration header
		fmt.Fprintf(cmd.ErrOrStderr(), "\n[loop] Iteration %d", iteration)
		if loopMax > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "/%d", loopMax)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "\n")

		// Run agent iteration
		iterStart := time.Now()
		summary, err := runLoopIteration(ctx, engine, req, cmd.OutOrStdout(), cmd.ErrOrStderr())
		iterDuration := time.Since(iterStart)

		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "[loop] Iteration %d failed: %v\n", iteration, err)
			// Add failure to history
			if history != nil {
				history.add(iteration, fmt.Sprintf("FAILED: %v", err), iterDuration)
			}
			continue
		}

		// Add to history
		if history != nil {
			history.add(iteration, summary, iterDuration)
		}

		fmt.Fprintf(cmd.ErrOrStderr(), "[loop] Iteration %d completed in %.1fs\n", iteration, iterDuration.Seconds())
	}
}

// runLoopIteration runs a single iteration and returns a summary for history
func runLoopIteration(ctx context.Context, engine *llm.Engine, req llm.Request, stdout, stderr io.Writer) (string, error) {
	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var textContent strings.Builder
	var toolsUsed []string

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch event.Type {
		case llm.EventTextDelta:
			fmt.Fprint(stdout, event.Text)
			textContent.WriteString(event.Text)

		case llm.EventToolExecStart:
			if event.ToolName != "" {
				// Track tool usage for summary
				toolsUsed = append(toolsUsed, event.ToolName)
				phase := ui.FormatToolPhase(event.ToolName, event.ToolInfo)
				fmt.Fprintf(stderr, "  > %s\n", phase.Active)
			}

		case llm.EventToolExecEnd:
			// Tool completed
			if event.ToolName != "" {
				phase := ui.FormatToolPhase(event.ToolName, event.ToolInfo)
				if event.ToolSuccess {
					fmt.Fprintf(stderr, "  %s %s\n", ui.SuccessCircle(), phase.Completed)
				} else {
					fmt.Fprintf(stderr, "  %s %s\n", ui.ErrorCircle(), phase.Completed)
				}
			}

		case llm.EventRetry:
			fmt.Fprintf(stderr, "  Rate limited (%d/%d), waiting %.0fs...\n",
				event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs)

		case llm.EventError:
			if event.Err != nil {
				return "", event.Err
			}
		}
	}

	// Build summary for history
	summary := buildIterationSummary(textContent.String(), toolsUsed)
	return summary, nil
}

// buildIterationSummary creates a concise summary for history injection
func buildIterationSummary(text string, toolsUsed []string) string {
	var b strings.Builder

	// Add tools used
	if len(toolsUsed) > 0 {
		// Deduplicate tools
		seen := make(map[string]bool)
		var unique []string
		for _, t := range toolsUsed {
			if !seen[t] {
				seen[t] = true
				unique = append(unique, t)
			}
		}
		b.WriteString("Tools: ")
		b.WriteString(strings.Join(unique, ", "))
		b.WriteString("\n")
	}

	// Add first ~500 chars of text as summary
	text = strings.TrimSpace(text)
	if len(text) > 500 {
		text = text[:500] + "..."
	}
	if text != "" {
		b.WriteString("Output: ")
		b.WriteString(text)
	}

	return b.String()
}
