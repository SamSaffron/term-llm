package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	execPrintOnly bool
	execSearch    bool
	execDebug     bool
	execAutoPick  bool
	execMaxOpts   int
	execProvider  string
	execFiles     []string
)

const (
	allowAutoRunEnv = "TERM_LLM_ALLOW_AUTORUN"
	allowNonTTYEnv  = "TERM_LLM_ALLOW_NON_TTY"
)

var execCmd = &cobra.Command{
	Use:   "exec <request>",
	Short: "Translate natural language to CLI commands",
	Long: `Get command suggestions from AI and execute one.

By default, shows an interactive selection UI. Use --auto-pick to
automatically execute the highest-likelihood suggestion.

Safety: auto-pick execution requires TERM_LLM_ALLOW_AUTORUN=1, and
non-TTY selection requires TERM_LLM_ALLOW_NON_TTY=1 (unless --print-only).

Examples:
  term-llm exec "list files by size"
  term-llm exec "find go files" --auto-pick    # auto-execute best
  term-llm exec "install latest node" -s       # with web search
  term-llm exec "compress folder" -n 5         # show max 5 options
  term-llm exec "disk usage" -p                # print only, don't execute
  term-llm exec -f log.txt "find errors"       # with file context
  term-llm exec -f clipboard "explain this"    # from clipboard
  git diff | term-llm exec "commit message"    # from stdin`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExec,
}

func init() {
	execCmd.Flags().BoolVarP(&execPrintOnly, "print-only", "p", false, "Print command instead of executing")
	execCmd.Flags().BoolVarP(&execSearch, "search", "s", false, "Enable web search for current information")
	execCmd.Flags().BoolVarP(&execDebug, "debug", "d", false, "Show full LLM request and response")
	execCmd.Flags().BoolVarP(&execAutoPick, "auto-pick", "a", false, "Auto-execute the best suggestion without prompting")
	execCmd.Flags().IntVarP(&execMaxOpts, "max", "n", 0, "Maximum number of options to show (0 = no limit)")
	execCmd.Flags().StringVar(&execProvider, "provider", "", "Override provider, optionally with model (e.g., openai:gpt-4o)")
	execCmd.Flags().StringArrayVarP(&execFiles, "file", "f", nil, "File(s) to include as context (supports globs, 'clipboard')")
	execCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion)
	rootCmd.AddCommand(execCmd)
}

func runExec(cmd *cobra.Command, args []string) error {
	userInput := strings.Join(args, " ")
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	if err := applyProviderOverrides(cfg, cfg.Exec.Provider, cfg.Exec.Model, execProvider); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := llm.NewEngine(provider, defaultToolRegistry())

	// Detect shell
	shell := detectShell()

	// Read files if provided
	var files []input.FileContent
	if len(execFiles) > 0 {
		var err error
		files, err = input.ReadFiles(execFiles)
		if err != nil {
			return fmt.Errorf("failed to read files: %w", err)
		}
	}

	// Read stdin if available
	stdinContent, err := input.ReadStdin()
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	// Determine number of suggestions: use -n flag if set, otherwise config default
	numSuggestions := cfg.Exec.Suggestions
	if execMaxOpts > 0 {
		numSuggestions = execMaxOpts
	}

	debugMode := execDebug || debugRaw

	// Main loop for refinement
	for {
		systemPrompt := prompt.SuggestSystemPrompt(shell, cfg.Exec.Instructions, numSuggestions, execSearch)
		userPrompt := prompt.SuggestUserPrompt(userInput, files, stdinContent)
		req := llm.Request{
			Messages: []llm.Message{
				llm.SystemText(systemPrompt),
				llm.UserText(userPrompt),
			},
			Tools: []llm.ToolSpec{
				llm.SuggestCommandsToolSpec(numSuggestions),
			},
			ToolChoice: llm.ToolChoice{
				Mode: llm.ToolChoiceName,
				Name: llm.SuggestCommandsToolName,
			},
			ParallelToolCalls: true,
			Search:            execSearch,
			Debug:             debugMode,
			DebugRaw:          debugRaw,
		}

		result, err := ui.RunWithSpinner(ctx, debugMode, func(ctx context.Context) (any, error) {
			return collectSuggestions(ctx, engine, req)
		})
		if err != nil {
			if err.Error() == "cancelled" {
				return nil
			}
			return fmt.Errorf("failed to get suggestions: %w", err)
		}

		suggestions, ok := result.([]llm.CommandSuggestion)
		if !ok {
			return fmt.Errorf("unexpected suggestions result")
		}

		// Sort by likelihood (highest first)
		sort.Slice(suggestions, func(i, j int) bool {
			return suggestions[i].Likelihood > suggestions[j].Likelihood
		})

		// Limit options if --max is set
		if execMaxOpts > 0 && len(suggestions) > execMaxOpts {
			suggestions = suggestions[:execMaxOpts]
		}

		// Auto-pick mode: execute the best suggestion immediately
		if execAutoPick {
			command := suggestions[0].Command
			if !execPrintOnly && !envEnabled(allowAutoRunEnv) {
				return fmt.Errorf("auto-pick requires %s=1 to execute; use --print-only or set %s=1", allowAutoRunEnv, allowAutoRunEnv)
			}
			if execPrintOnly {
				fmt.Println(command)
				return nil
			}
			return executeCommand(command, shell)
		}

		// Interactive mode: show selection UI (with help support via 'h' key)
		allowNonTTY := execPrintOnly || envEnabled(allowNonTTYEnv)
		selected, err := ui.SelectCommand(suggestions, shell, engine, allowNonTTY)
		if err != nil {
			if err.Error() == "cancelled" {
				return nil
			}
			return fmt.Errorf("selection cancelled: %w", err)
		}

		// Handle "something else" option
		if selected == ui.SomethingElse {
			refinement, err := ui.GetRefinement()
			if err != nil {
				return fmt.Errorf("refinement cancelled: %w", err)
			}
			if refinement == "" {
				continue
			}
			// Append refinement to original request
			userInput = fmt.Sprintf("%s (%s)", userInput, refinement)
			continue
		}

		// Print-only mode: just output the command for shell integration
		if execPrintOnly {
			if selected != "" {
				fmt.Println(selected)
			}
			return nil
		}

		// Execute the selected command
		return executeCommand(selected, shell)
	}
}

func collectSuggestions(ctx context.Context, engine *llm.Engine, req llm.Request) ([]llm.CommandSuggestion, error) {
	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	var suggestions []llm.CommandSuggestion
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if event.Type == llm.EventError && event.Err != nil {
			return nil, event.Err
		}
		if event.Type == llm.EventToolCall && event.Tool != nil && event.Tool.Name == llm.SuggestCommandsToolName {
			llm.DebugToolCall(req.Debug, *event.Tool)
			parsed, err := llm.ParseCommandSuggestions(*event.Tool)
			if err != nil {
				return nil, err
			}
			suggestions = append(suggestions, parsed...)
		}
	}

	if len(suggestions) == 0 {
		return nil, fmt.Errorf("no suggestions returned")
	}
	return suggestions, nil
}

func envEnabled(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch value {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}
