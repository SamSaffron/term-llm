package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	execPrintOnly bool
	execSearch    bool
	execDebug     bool
	execAutoPick  bool
	execMaxOpts   int
)

var execCmd = &cobra.Command{
	Use:   "exec <request>",
	Short: "Translate natural language to CLI commands",
	Long: `Get command suggestions from AI and execute one.

By default, shows an interactive selection UI. Use --auto-pick to
automatically execute the highest-likelihood suggestion.

Examples:
  term-llm exec "list files by size"
  term-llm exec "find go files" --auto-pick    # auto-execute best
  term-llm exec "install latest node" -s       # with web search
  term-llm exec "compress folder" -n 5         # show max 5 options
  term-llm exec "disk usage" -p                # print only, don't execute`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExec,
}

func init() {
	execCmd.Flags().BoolVarP(&execPrintOnly, "print-only", "p", false, "Print command instead of executing")
	execCmd.Flags().BoolVarP(&execSearch, "search", "s", false, "Enable web search for current information")
	execCmd.Flags().BoolVarP(&execDebug, "debug", "d", false, "Show full LLM request and response")
	execCmd.Flags().BoolVarP(&execAutoPick, "auto-pick", "a", false, "Auto-execute the best suggestion without prompting")
	execCmd.Flags().IntVarP(&execMaxOpts, "max", "n", 0, "Maximum number of options to show (0 = no limit)")
	rootCmd.AddCommand(execCmd)
}

func runExec(cmd *cobra.Command, args []string) error {
	userInput := strings.Join(args, " ")
	ctx := context.Background()

	// Check if setup is needed
	var cfg *config.Config
	var err error

	if config.NeedsSetup() {
		cfg, err = ui.RunSetupWizard()
		if err != nil {
			return fmt.Errorf("setup cancelled: %w", err)
		}
	} else {
		cfg, err = config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
	}

	// Initialize theme from config
	ui.InitTheme(ui.ThemeConfig{
		Primary:   cfg.Theme.Primary,
		Secondary: cfg.Theme.Secondary,
		Success:   cfg.Theme.Success,
		Error:     cfg.Theme.Error,
		Warning:   cfg.Theme.Warning,
		Muted:     cfg.Theme.Muted,
		Text:      cfg.Theme.Text,
		Spinner:   cfg.Theme.Spinner,
	})

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Detect shell
	shell := detectShell()

	// Determine number of suggestions: use -n flag if set, otherwise config default
	numSuggestions := cfg.Exec.Suggestions
	if execMaxOpts > 0 {
		numSuggestions = execMaxOpts
	}

	// Main loop for refinement
	for {
		// Build request
		req := llm.SuggestRequest{
			UserInput:      userInput,
			Shell:          shell,
			Instructions:   cfg.Exec.Instructions,
			NumSuggestions: numSuggestions,
			EnableSearch:   execSearch,
			Debug:          execDebug,
		}

		// Get suggestions from LLM with spinner
		suggestions, err := ui.RunWithSpinner(ctx, provider, req)
		if err != nil {
			if err.Error() == "cancelled" {
				return nil
			}
			return fmt.Errorf("failed to get suggestions: %w", err)
		}

		if len(suggestions) == 0 {
			return fmt.Errorf("no suggestions returned")
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
			if execPrintOnly {
				fmt.Println(command)
				return nil
			}
			return executeCommand(command, shell)
		}

		// Interactive mode: show selection UI (with help support via 'h' key)
		selected, err := ui.SelectCommand(suggestions, shell, provider)
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
