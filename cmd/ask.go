package cmd

import (
	"os"
	"time"

	"github.com/samsaffron/term-llm/internal/app/ask"
	"github.com/samsaffron/term-llm/internal/config"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
	"github.com/spf13/cobra"
)

var (
	askDebug           bool
	askSearch          bool
	askNoSearch        bool
	askText            bool
	askPorcelain       bool
	askJSON            bool
	askProgressive     bool
	askProvider        string
	askFiles           []string
	askMCP             string
	askMaxTurns        int
	askMaxOutputTokens int
	askInlineMaxBytes  string
	askTimeout         time.Duration
	askStopWhen        string
	askContinueWith    string
	askNativeSearch    bool
	askNoNativeSearch  bool
	askNoWebFetch      bool
	// Tool flags
	askTools         string
	askReadDirs      []string
	askWriteDirs     []string
	askShellAllow    []string
	askSystemMessage string
	// Agent flag
	askAgent string
	// Approval modes
	askApproval string
	askYolo     bool
	askAuto     bool
	// Fast provider flag
	askFast bool
	// Skills flag
	askSkills string
	// Session resume flag
	askResume string
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
  term-llm ask "Explain git rebase" --json | jq -c .
  term-llm ask -f code.go "Explain this code"
  term-llm ask -f code.go:10-50 "Explain this function"
  term-llm ask -f clipboard "What is this?"
  cat error.log | term-llm ask "What went wrong?"

Agent examples (use @agent shortcut or --agent flag):
  term-llm ask @reviewer "Review this code" -f main.go
  term-llm ask @commit-message              (uses default prompt)
  term-llm ask @commit-message "focus on the bug fix"
  term-llm ask @editor "Add error handling" -f utils.go
  term-llm ask --agent web-researcher "Find info about Go 1.22"

Line range syntax for files:
  main.go       - Include entire file
  main.go:11-22 - Include only lines 11-22
  main.go:11-   - Include lines 11 to end of file
  main.go:-22   - Include lines 1-22`,
	Args:              cobra.MinimumNArgs(0),
	RunE:              runAsk,
	ValidArgsFunction: AtAgentCompletion,
}

func init() {
	AddCommonFlags(askCmd,
		CommonCoreFlags|CommonSearchFlags|CommonMaxTurns|CommonMaxOutputTokens|CommonFiles|CommonAgent|CommonSkills,
		CommonFlagBindings{
			Provider:         &askProvider,
			Debug:            &askDebug,
			Search:           &askSearch,
			NoSearch:         &askNoSearch,
			NativeSearch:     &askNativeSearch,
			NoNativeSearch:   &askNoNativeSearch,
			NoWebFetch:       &askNoWebFetch,
			MCP:              &askMCP,
			MaxTurns:         &askMaxTurns,
			MaxTurnsDefault:  50,
			MaxOutputTokens:  &askMaxOutputTokens,
			Tools:            &askTools,
			ReadDirs:         &askReadDirs,
			WriteDirs:        &askWriteDirs,
			ShellAllow:       &askShellAllow,
			SystemMessage:    &askSystemMessage,
			Files:            &askFiles,
			FilesDescription: "File(s) to include as context (supports globs, line ranges like file.go:10-20, 'clipboard')",
			Agent:            &askAgent,
			Approval:         &askApproval,
			Yolo:             &askYolo,
			Auto:             &askAuto,
			Skills:           &askSkills,
		})

	// Ask-specific flags
	askCmd.Flags().BoolVarP(&askText, "text", "t", false, "Output plain text instead of rendered markdown")
	askCmd.Flags().BoolVar(&askPorcelain, "porcelain", false, "Output plain text without tool status lines (implies --text)")
	askCmd.Flags().BoolVar(&askJSON, "json", false, "Emit JSONL event stream on stdout (one event per line, implies --text)")
	askCmd.Flags().BoolVar(&askProgressive, "progressive", false, "Enable progressive execution with persisted best-so-far progress")
	askCmd.Flags().DurationVar(&askTimeout, "timeout", 0, "Set a hard deadline for the run (used by progressive execution for finalization budget)")
	askCmd.Flags().StringVar(&askStopWhen, "stop-when", "", "Progressive stop condition: done or timeout (defaults to done in progressive mode)")
	askCmd.Flags().StringVar(&askContinueWith, "continue-with", "", "Custom continuation prompt for progressive timeout mode")
	askCmd.Flags().BoolVar(&askFast, "fast", false, "Use the configured fast provider/model instead of the default")
	askCmd.Flags().StringVar(&askInlineMaxBytes, "inline-max-bytes", "", "Embed stdin/-f text up to this size inline instead of staging it for tools (e.g. 512K, 2M; overrides ask.stdin_inline_max_bytes)")

	// Session resume flag - NoOptDefVal allows --resume without a value
	askCmd.Flags().StringVarP(&askResume, "resume", "r", "", "Continue a session (empty for most recent, or session ID)")
	askCmd.Flags().Lookup("resume").NoOptDefVal = " " // space means "flag was passed without value"

	rootCmd.AddCommand(askCmd)
}

func runAsk(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext()
	defer stop()
	opts, deps := captureAskInvocation(cmd)
	return ask.Run(ctx, args, opts, deps)
}

func captureAskInvocation(cmd *cobra.Command) (ask.Options, ask.Dependencies) {
	opts := ask.Options{
		Debug:           askDebug,
		Search:          askSearch,
		NoSearch:        askNoSearch,
		Text:            askText,
		Porcelain:       askPorcelain,
		JSON:            askJSON,
		Progressive:     askProgressive,
		Provider:        askProvider,
		Files:           append([]string(nil), askFiles...),
		MCP:             askMCP,
		MaxTurns:        askMaxTurns,
		MaxOutputTokens: askMaxOutputTokens,
		InlineMaxBytes:  askInlineMaxBytes,
		Timeout:         askTimeout,
		StopWhen:        askStopWhen,
		ContinueWith:    askContinueWith,
		NativeSearch:    askNativeSearch,
		NoNativeSearch:  askNoNativeSearch,
		NoWebFetch:      askNoWebFetch,
		Tools:           askTools,
		ReadDirs:        append([]string(nil), askReadDirs...),
		WriteDirs:       append([]string(nil), askWriteDirs...),
		ShellAllow:      append([]string(nil), askShellAllow...),
		SystemMessage:   askSystemMessage,
		Agent:           askAgent,
		Approval:        askApproval,
		Yolo:            askYolo,
		Auto:            askAuto,
		Fast:            askFast,
		Skills:          askSkills,
		Resume:          askResume,
		DebugRaw:        debugRaw, ShowStats: showStats,
		Changed: ask.FlagState{
			Resume: cmd.Flags().Changed("resume"), Tools: cmd.Flags().Changed("tools"),
			System: cmd.Flags().Changed("system"), MaxTurns: cmd.Flags().Changed("max-turns"),
			Search: cmd.Flags().Changed("search"), MCP: cmd.Flags().Changed("mcp"),
		},
	}
	stdin := cmd.InOrStdin()
	hasStdin := ask.ReaderHasStdin(stdin)
	// Capture CLI arbitration now, but defer errors/config resolution to startup.
	cli, cliErr := approvalModeFromCommand(cmd, opts.Approval, opts.Auto, opts.Yolo)
	deps := ask.Dependencies{
		Stdin: stdin, HasStdin: hasStdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
		Interactive:  func() bool { return terminalpolicy.Interactive(os.Stdin, os.Stdout) },
		SessionStore: sessionStoreOverrides(), MemoryPaths: memoryPaths(),
		LoadConfig: loadConfigWithSetup, NewProvider: ask.NewProvider, CreateDebugLogger: createDebugLogger,
		RunnerCleanupTimeout: runpkg.DefaultRunnerCleanupTimeout,
		ResolveApproval: func(cfg *config.Config) (resolvedApprovalMode, error) {
			if cliErr != nil {
				return resolvedApprovalMode{}, cliErr
			}
			return resolveApprovalMode(approvalModeResolutionInput{Surface: approvalSurfaceAsk, Config: cfg, CLI: cli})
		},
	}
	return opts, deps
}
