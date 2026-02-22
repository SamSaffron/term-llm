package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/diagnostics"
	"github.com/samsaffron/term-llm/internal/edit"
	"github.com/samsaffron/term-llm/internal/exitcode"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	editDryRun     bool
	editDebug      bool
	editProvider   string
	editFiles      []string
	editContext    []string
	editDiffFormat string
	editMCP        string
	// Tool flags
	editTools         string
	editReadDirs      []string
	editWriteDirs     []string
	editShellAllow    []string
	editSystemMessage string
	// Yolo mode
	editYolo bool
	// Skills flag
	editSkills string
)

var editCmd = &cobra.Command{
	Use:   "edit <request>",
	Short: "Edit files using AI assistance",
	Long: `Edit files based on natural language instructions.

The AI will make changes to the specified files. Use --dry-run to preview
changes without applying them.

Examples:
  term-llm edit "add error handling" --file main.go
  term-llm edit "refactor to use interfaces" --file "*.go"
  term-llm edit "fix the loop" --file utils.go:45-60
  term-llm edit "use the API client" -f main.go -c api/client.go

Line range syntax:
  main.go       - Edit entire file (no guard)
  main.go:11-22 - Only lines 11-22 can be modified
  main.go:11-   - Lines 11 to end of file
  main.go:-22   - Lines 1-22

Context files:
  Use --context/-c to include read-only reference files that inform the edit
  but won't be modified themselves.`,
	Args:         cobra.MinimumNArgs(1),
	SilenceUsage: true,
	RunE:         runEdit,
}

func init() {
	// Edit-specific flags
	editCmd.Flags().StringArrayVarP(&editFiles, "file", "f", nil, "File(s) to edit (required, supports line ranges like file.go:10-20)")
	editCmd.Flags().StringArrayVarP(&editContext, "context", "c", nil, "File(s) to include as read-only context (supports globs, 'clipboard')")
	editCmd.Flags().BoolVar(&editDryRun, "dry-run", false, "Show what would change without applying")
	editCmd.Flags().StringVar(&editDiffFormat, "diff-format", "", "Force diff format: 'udiff' or 'replace' (default: auto)")

	// Common flags shared across commands
	AddProviderFlag(editCmd, &editProvider)
	AddDebugFlag(editCmd, &editDebug)
	AddMCPFlag(editCmd, &editMCP)
	AddToolFlags(editCmd, &editTools, &editReadDirs, &editWriteDirs, &editShellAllow)
	AddSystemMessageFlag(editCmd, &editSystemMessage)
	AddYoloFlag(editCmd, &editYolo)
	AddSkillsFlag(editCmd, &editSkills)

	if err := editCmd.MarkFlagRequired("file"); err != nil {
		panic(fmt.Sprintf("failed to mark file flag required: %v", err))
	}

	// Additional completions
	if err := editCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register tools completion: %v", err))
	}
	rootCmd.AddCommand(editCmd)
}

func toPromptSpecs(specs []input.FileSpec) []prompt.EditSpec {
	result := make([]prompt.EditSpec, 0, len(specs))
	for _, spec := range specs {
		result = append(result, prompt.EditSpec{
			Path:      spec.Path,
			StartLine: spec.StartLine,
			EndLine:   spec.EndLine,
			HasGuard:  spec.HasRegion,
		})
	}
	return result
}

// absPath converts a path to absolute, returning the original if conversion fails
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func runEdit(cmd *cobra.Command, args []string) error {
	request := strings.Join(args, " ")
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	if err := applyProviderOverrides(cfg, cfg.Edit.Provider, cfg.Edit.Model, editProvider); err != nil {
		return err
	}

	// Override instructions if flag is set
	if editSystemMessage != "" {
		cfg.Edit.Instructions = editSystemMessage
	}

	initThemeFromConfig(cfg)

	// Create provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Initialize local tools if --tools flag is set
	if editTools != "" {
		engine := newEngine(provider, cfg)
		toolConfig := buildToolConfig(editTools, editReadDirs, editWriteDirs, editShellAllow, cfg)
		// For edit command, exclude edit/write tools to avoid conflicts with the command's own editing
		toolConfig.Enabled = filterOutTools(toolConfig.Enabled, "edit_file", "write_file")
		if len(toolConfig.Enabled) == 0 {
			return fmt.Errorf("no tools remaining after excluding edit/write (edit command handles file modifications)")
		}
		if errs := toolConfig.Validate(); len(errs) > 0 {
			return fmt.Errorf("invalid tool config: %v", errs[0])
		}
		toolMgr, err := tools.NewToolManager(&toolConfig, cfg)
		if err != nil {
			return fmt.Errorf("failed to initialize tools: %w", err)
		}
		wireImageRecorder(toolMgr.Registry, "", "")
		// Enable yolo mode if flag is set
		if editYolo {
			toolMgr.ApprovalMgr.SetYoloMode(true)
		}
		// Set up the improved approval UI with git-aware heuristics
		toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
			if isShell {
				return tools.RunShellApprovalUI(path)
			}
			return tools.RunFileApprovalUI(path, isWrite)
		}
		toolMgr.SetupEngine(engine)

		// Wire spawn_agent runner if enabled
		if err := WireSpawnAgentRunner(cfg, toolMgr, editYolo); err != nil {
			return err
		}
	}

	// Initialize MCP servers if --mcp flag is set
	var mcpManager *mcp.Manager
	if editMCP != "" {
		engine := newEngine(provider, cfg)
		mcpOpts := &MCPOptions{
			Provider: provider,
			YoloMode: editYolo,
		}
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			mcpOpts.Model = providerCfg.Model
		}
		mcpManager, err = enableMCPServersWithFeedback(ctx, editMCP, engine, cmd.ErrOrStderr(), mcpOpts)
		if err != nil {
			return err
		}
		if mcpManager != nil {
			defer mcpManager.StopAll()
		}
	}

	// Parse file specs and read files
	var files []input.FileContent
	var specs []input.FileSpec

	for _, f := range editFiles {
		spec, err := input.ParseFileSpec(f)
		if err != nil {
			return err
		}

		if strings.ContainsAny(spec.Path, "*?[") {
			// Glob pattern
			expanded, err := input.ReadFiles([]string{spec.Path})
			if err != nil {
				return fmt.Errorf("failed to read files: %w", err)
			}
			for _, ef := range expanded {
				// Normalize to absolute path for consistent lookups
				ef.Path = absPath(ef.Path)
				files = append(files, ef)
				specs = append(specs, input.FileSpec{Path: ef.Path})
			}
		} else {
			// Normalize to absolute path for consistent lookups
			absFilePath := absPath(spec.Path)
			content, err := os.ReadFile(absFilePath)
			if err != nil {
				return fmt.Errorf("failed to read %s: %w", spec.Path, err)
			}
			files = append(files, input.FileContent{
				Path:    absFilePath,
				Content: string(content),
			})
			spec.Path = absFilePath
			specs = append(specs, spec)
		}
	}

	if len(files) == 0 {
		return fmt.Errorf("no files found")
	}

	// Read context files if provided
	var contextFiles []input.FileContent
	if len(editContext) > 0 {
		contextFiles, err = input.ReadFiles(editContext)
		if err != nil {
			return fmt.Errorf("failed to read context files: %w", err)
		}
	}

	// Read stdin as additional context (useful for piping git diffs, etc.)
	stdinContent, err := input.ReadStdin()
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	return runStreamEdit(ctx, cfg, provider, request, files, specs, contextFiles, stdinContent)
}

// getActiveModel returns the model name for the active provider
func getActiveModel(cfg *config.Config) string {
	if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
		return providerCfg.Model
	}
	return ""
}

// runStreamEdit runs the streaming edit flow (one-shot, no tools)
func runStreamEdit(ctx context.Context, cfg *config.Config, provider llm.Provider, request string, files []input.FileContent, specs []input.FileSpec, contextFiles []input.FileContent, stdinContent string) error {
	// Build file contents map
	fileContents := make(map[string]string)
	for _, f := range files {
		fileContents[f.Path] = f.Content
	}

	// Build guards map
	guards := make(map[string][2]int)
	for _, spec := range specs {
		if spec.HasRegion {
			guards[spec.Path] = [2]int{spec.StartLine, spec.EndLine}
		}
	}

	// Create progress channel for spinner updates
	progressCh := make(chan ui.ProgressUpdate, 10)

	// Get model from config (needed for diagnostics callback)
	model := getActiveModel(cfg)

	// Create executor config
	execConfig := edit.ExecutorConfig{
		FileContents: fileContents,
		Guards:       guards,
		Debug:        editDebug,
		DebugRaw:     debugRaw,
		OnFileStart: func(path string) {
			if editDebug {
				fmt.Printf("Editing: %s\n", path)
			}
			// Send status update to spinner
			select {
			case progressCh <- ui.ProgressUpdate{Status: "editing " + filepath.Base(path)}:
			default:
			}
		},
		OnSearchMatch: func(path string, level edit.MatchLevel) {
			if editDebug {
				fmt.Printf("  Search matched (%s)\n", level)
			}
			// Send milestone to spinner
			select {
			case progressCh <- ui.ProgressUpdate{Milestone: fmt.Sprintf("✓ Found edit for %s", filepath.Base(path))}:
			default:
			}
		},
		OnSearchFail: func(path string, search string, err error) {
			// Only log in debug mode - retries will handle silently
			if editDebug {
				fmt.Printf("  Search failed: %s\n", err)
				lines := strings.Split(search, "\n")
				if len(lines) > 3 {
					fmt.Printf("    First line: %s\n", truncateStr(lines[0], 60))
					fmt.Printf("    Last line:  %s\n", truncateStr(lines[len(lines)-1], 60))
				}
			}
			// Send failure milestone to spinner
			select {
			case progressCh <- ui.ProgressUpdate{Milestone: fmt.Sprintf("✗ Edit failed for %s, retrying...", filepath.Base(path))}:
			default:
			}
		},
		OnProgress: func(msg string) {
			// Send retry progress to spinner
			select {
			case progressCh <- ui.ProgressUpdate{Milestone: "⟳ " + msg}:
			default:
			}
		},
		OnTokens: func(outputTokens int) {
			// Send token update to spinner
			select {
			case progressCh <- ui.ProgressUpdate{OutputTokens: outputTokens}:
			default:
			}
		},
		OnFirstToken: func() {
			// Send phase update to spinner - transition from "Thinking" to "Responding"
			select {
			case progressCh <- ui.ProgressUpdate{Phase: "Responding"}:
			default:
			}
		},
		OnToolStart: func(toolName string) {
			// Send phase update for tool execution
			phase := "Running " + toolName
			if toolName == edit.ReadContextToolName {
				phase = "Reading"
			}
			select {
			case progressCh <- ui.ProgressUpdate{Phase: phase}:
			default:
			}
		},
		OnAPIRetry: func(attempt, maxAttempts int, waitSecs float64) {
			// Send rate limit retry progress to spinner
			msg := fmt.Sprintf("⏳ Rate limited, retrying (%d/%d) in %.0fs...", attempt+1, maxAttempts, waitSecs)
			select {
			case progressCh <- ui.ProgressUpdate{Milestone: msg}:
			default:
			}
		},
		// About text is stored and shown on demand via (i)nfo
	}

	// Add OnRetry callback for diagnostics if enabled
	if cfg.Diagnostics.Enabled {
		execConfig.OnRetry = func(diag edit.RetryDiagnostic) {
			d := &diagnostics.EditRetryDiagnostic{
				Timestamp:     time.Now(),
				Provider:      provider.Name(),
				Model:         model,
				FilePath:      diag.RetryContext.FilePath,
				AttemptNumber: diag.AttemptNumber,
				Reason:        diag.RetryContext.Reason,
				LLMResponse:   diag.RetryContext.PartialOutput,
				FailedSearch:  diag.RetryContext.FailedSearch,
				DiffLines:     diag.RetryContext.DiffLines,
				FileContent:   diag.RetryContext.FileContent,
				SystemPrompt:  diag.SystemPrompt,
				UserPrompt:    diag.UserPrompt,
			}
			dir := cfg.Diagnostics.Dir
			if dir == "" {
				dir = config.GetDiagnosticsDir()
			}
			if err := diagnostics.WriteEditRetry(dir, d); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to write diagnostics: %v\n", err)
			}
		}
	}

	// Create executor
	executor := edit.NewStreamEditExecutor(provider, model, execConfig)

	// Determine diff format: flag > config > auto-detect by model
	diffFormat := editDiffFormat
	if diffFormat == "" {
		diffFormat = cfg.Edit.DiffFormat
	}
	if diffFormat == "" {
		diffFormat = "auto"
	}

	// Build prompts
	promptSpecs := toPromptSpecs(specs)
	useUnifiedDiff := prompt.ShouldUseUnifiedDiff(model, diffFormat)
	useLazyContext := prompt.ShouldUseLazyContext(files, promptSpecs)

	var systemPrompt, userPrompt string
	if useLazyContext {
		// Lazy context mode: only send editable region + padding, LLM can request more
		systemPrompt = prompt.StreamEditSystemPromptLazy(cfg.Edit.Instructions, promptSpecs, model, diffFormat)
		userPrompt = prompt.StreamEditUserPromptLazy(request, files, promptSpecs, contextFiles, stdinContent, useUnifiedDiff)
		execConfig.LazyContext = true
	} else {
		// Full context mode: send entire file
		systemPrompt = prompt.StreamEditSystemPrompt(cfg.Edit.Instructions, promptSpecs, model, diffFormat)
		userPrompt = prompt.StreamEditUserPrompt(request, files, promptSpecs, contextFiles, stdinContent, useUnifiedDiff)
	}

	messages := []llm.Message{
		llm.SystemText(systemPrompt),
		llm.UserText(userPrompt),
	}

	// Execute with spinner (with progress updates)
	debugMode := editDebug
	type execResult struct {
		results   []edit.EditResult
		aboutText string
	}
	result, err := ui.RunWithSpinnerProgress(ctx, debugMode || debugRaw, progressCh, func(ctx context.Context) (any, error) {
		defer close(progressCh)
		results, aboutText, err := executor.Execute(ctx, messages)
		if err != nil {
			return nil, err
		}
		return execResult{results: results, aboutText: aboutText}, nil
	})
	if err != nil {
		if err.Error() == "cancelled" {
			return exitcode.Cancel()
		}
		return fmt.Errorf("edit failed: %w", err)
	}

	execRes, ok := result.(execResult)
	if !ok {
		return fmt.Errorf("unexpected result type")
	}
	results := execRes.results

	if len(results) == 0 {
		fmt.Println("No edits proposed")
		return exitcode.NoEdits("no edits proposed")
	}

	// Consolidate results by file - use final state for each file
	// Results are cumulative, so last result for each file has final content
	fileResults := make(map[string]edit.EditResult)
	var fileOrder []string
	for _, r := range results {
		if r.Error != nil {
			continue
		}
		if _, seen := fileResults[r.Path]; !seen {
			fileOrder = append(fileOrder, r.Path)
			// First result for this file - get original content
			fileResults[r.Path] = edit.EditResult{
				Path:       r.Path,
				OldContent: fileContents[r.Path], // Original from disk
				NewContent: r.NewContent,
			}
		} else {
			// Update with latest content
			existing := fileResults[r.Path]
			existing.NewContent = r.NewContent
			fileResults[r.Path] = existing
		}
	}

	// Filter to only files with actual changes
	var changedResults []edit.EditResult
	for _, path := range fileOrder {
		r := fileResults[path]
		if r.OldContent != r.NewContent {
			changedResults = append(changedResults, r)
		}
	}

	if len(changedResults) == 0 {
		fmt.Println("No edits proposed")
		return exitcode.NoEdits("no changes")
	}

	// Show all diffs first
	for i, r := range changedResults {
		if i > 0 {
			fmt.Println()
		}
		ui.PrintUnifiedDiff(r.Path, r.OldContent, r.NewContent)
	}

	// Dry run - no approval needed
	if editDryRun {
		fmt.Println()
		return nil
	}

	// Batch approval with info option
	hasInfo := execRes.aboutText != ""
	reprompt := false
	for {
		approval := ui.PromptBatchApproval(hasInfo, reprompt)
		switch approval {
		case ui.EditApprovalInfo:
			ui.ShowEditInfo(execRes.aboutText)
			reprompt = true
			continue // loop back to prompt
		case ui.EditApprovalNo:
			fmt.Println()
			return exitcode.Declined("user declined edits")
		case ui.EditApprovalYes:
			// Apply all changes
			var applied int
			for _, r := range changedResults {
				if err := os.WriteFile(r.Path, []byte(r.NewContent), 0644); err != nil {
					fmt.Printf("  error writing %s: %s\n", r.Path, err.Error())
					continue
				}
				applied++
			}
			if len(changedResults) > 1 {
				fmt.Printf("\r%d files updated\n", applied)
			}
			fmt.Println()
			return nil
		}
	}
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
