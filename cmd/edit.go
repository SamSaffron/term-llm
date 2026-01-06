package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/edit"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	editDryRun    bool
	editDebug     bool
	editProvider  string
	editFiles     []string
	editDiffFormat string
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

Line range syntax:
  main.go       - Edit entire file (no guard)
  main.go:11-22 - Only lines 11-22 can be modified
  main.go:11-   - Lines 11 to end of file
  main.go:-22   - Lines 1-22`,
	Args: cobra.MinimumNArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringArrayVarP(&editFiles, "file", "f", nil, "File(s) to edit (required, supports line ranges like file.go:10-20)")
	editCmd.Flags().BoolVar(&editDryRun, "dry-run", false, "Show what would change without applying")
	editCmd.Flags().StringVar(&editProvider, "provider", "", "Override provider, optionally with model (e.g., openai:gpt-4o)")
	editCmd.Flags().BoolVarP(&editDebug, "debug", "d", false, "Show debug information")
	editCmd.Flags().StringVar(&editDiffFormat, "diff-format", "", "Force diff format: 'udiff' or 'replace' (default: auto)")
	if err := editCmd.MarkFlagRequired("file"); err != nil {
		panic(fmt.Sprintf("failed to mark file flag required: %v", err))
	}
	if err := editCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register provider completion: %v", err))
	}
	rootCmd.AddCommand(editCmd)
}

// FileSpec represents a file with optional line range guard
type FileSpec struct {
	Path      string
	StartLine int // 1-indexed, 0 means from beginning
	EndLine   int // 1-indexed, 0 means to end
	HasGuard  bool
}

type diffEntry struct {
	path              string
	writePath         string
	oldContent        string
	newContent        string
	skipReasons       []string
	countSkipIfNoDiff bool
	onApplied         func(path, newContent string)
}

type diffApplyOptions struct {
	separatorOnAnyOutput bool
}

func toPromptSpecs(specs []FileSpec) []prompt.EditSpec {
	result := make([]prompt.EditSpec, 0, len(specs))
	for _, spec := range specs {
		result = append(result, prompt.EditSpec{
			Path:      spec.Path,
			StartLine: spec.StartLine,
			EndLine:   spec.EndLine,
			HasGuard:  spec.HasGuard,
		})
	}
	return result
}

func applyDiffEntries(entries []diffEntry, dryRun bool, opts diffApplyOptions) (int, int) {
	var applied, skipped int
	firstDiff := true
	printed := false

	for _, entry := range entries {
		for _, reason := range entry.skipReasons {
			ui.ShowEditSkipped(entry.path, reason)
		}
		if len(entry.skipReasons) > 0 {
			printed = true
		}

		if entry.oldContent == entry.newContent {
			if entry.countSkipIfNoDiff && len(entry.skipReasons) > 0 {
				skipped++
			}
			continue
		}

		if opts.separatorOnAnyOutput {
			if printed {
				fmt.Println()
			}
		} else if !firstDiff {
			fmt.Println()
		}
		firstDiff = false

		ui.PrintUnifiedDiff(entry.path, entry.oldContent, entry.newContent)
		printed = true

		if dryRun {
			continue
		}

		if !ui.PromptApplyEdit() {
			skipped++
			continue
		}

		writePath := entry.writePath
		if writePath == "" {
			writePath = entry.path
		}

		if err := os.WriteFile(writePath, []byte(entry.newContent), 0644); err != nil {
			fmt.Printf("  error: %s\n", err.Error())
			skipped++
			continue
		}

		if entry.onApplied != nil {
			entry.onApplied(writePath, entry.newContent)
		}

		applied++
	}

	return applied, skipped
}

// absPath converts a path to absolute, returning the original if conversion fails
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// parseFileSpec parses a file specification like "main.go:11-22"
func parseFileSpec(spec string) (FileSpec, error) {
	re := regexp.MustCompile(`^(.+?)(?::(\d*)-(\d*))?$`)
	matches := re.FindStringSubmatch(spec)
	if matches == nil {
		return FileSpec{}, fmt.Errorf("invalid file spec: %s", spec)
	}

	fs := FileSpec{Path: matches[1]}

	if strings.Contains(spec, ":") && len(matches) > 1 {
		fs.HasGuard = true
		if matches[2] != "" {
			start, err := strconv.Atoi(matches[2])
			if err != nil {
				return FileSpec{}, fmt.Errorf("invalid start line: %s", matches[2])
			}
			fs.StartLine = start
		}
		if matches[3] != "" {
			end, err := strconv.Atoi(matches[3])
			if err != nil {
				return FileSpec{}, fmt.Errorf("invalid end line: %s", matches[3])
			}
			fs.EndLine = end
		}
	}

	return fs, nil
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

	initThemeFromConfig(cfg)

	// Create provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Parse file specs and read files
	var files []input.FileContent
	var specs []FileSpec

	for _, f := range editFiles {
		spec, err := parseFileSpec(f)
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
				specs = append(specs, FileSpec{Path: ef.Path})
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

	return runStreamEdit(ctx, cfg, provider, request, files, specs)
}

// getActiveModel returns the model name for the active provider
func getActiveModel(cfg *config.Config) string {
	switch cfg.Provider {
	case "anthropic":
		return cfg.Anthropic.Model
	case "openai":
		return cfg.OpenAI.Model
	case "openrouter":
		return cfg.OpenRouter.Model
	case "gemini":
		return cfg.Gemini.Model
	case "zen":
		return cfg.Zen.Model
	default:
		return ""
	}
}

// runStreamEdit runs the streaming edit flow (one-shot, no tools)
func runStreamEdit(ctx context.Context, cfg *config.Config, provider llm.Provider, request string, files []input.FileContent, specs []FileSpec) error {
	// Build file contents map
	fileContents := make(map[string]string)
	for _, f := range files {
		fileContents[f.Path] = f.Content
	}

	// Build guards map
	guards := make(map[string][2]int)
	for _, spec := range specs {
		if spec.HasGuard {
			guards[spec.Path] = [2]int{spec.StartLine, spec.EndLine}
		}
	}

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
		},
		OnSearchMatch: func(path string, level edit.MatchLevel) {
			if editDebug {
				fmt.Printf("  Search matched (%s)\n", level)
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
		},
		// About text is stored and shown on demand via (i)nfo
	}

	// Get model from config
	model := getActiveModel(cfg)

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
	systemPrompt := prompt.StreamEditSystemPrompt(cfg.Edit.Instructions, promptSpecs, model, diffFormat)
	userPrompt := prompt.StreamEditUserPrompt(request, files, promptSpecs, useUnifiedDiff)

	messages := []llm.Message{
		llm.SystemText(systemPrompt),
		llm.UserText(userPrompt),
	}

	// Execute with spinner
	debugMode := editDebug
	type execResult struct {
		results   []edit.EditResult
		aboutText string
	}
	result, err := ui.RunWithSpinner(ctx, debugMode || debugRaw, func(ctx context.Context) (any, error) {
		results, aboutText, err := executor.Execute(ctx, messages)
		if err != nil {
			return nil, err
		}
		return execResult{results: results, aboutText: aboutText}, nil
	})
	if err != nil {
		if err.Error() == "cancelled" {
			return nil
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
		return nil
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
		return nil
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
			return nil
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
				fmt.Printf("%d files updated\n", applied)
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
