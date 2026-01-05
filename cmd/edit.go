package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/cmd/udiff"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	editDryRun   bool
	editDebug    bool
	editProvider string
	editFiles    []string
	editPerEdit  bool
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
	editCmd.Flags().BoolVar(&editPerEdit, "per-edit", false, "Prompt for each edit separately instead of consolidating per file")
	editCmd.MarkFlagRequired("file")
	editCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion)
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
	globalWidth := 0
	for _, entry := range entries {
		if entry.oldContent != entry.newContent {
			w := ui.CalcDiffWidth(entry.oldContent, entry.newContent)
			if w > globalWidth {
				globalWidth = w
			}
		}
	}

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

		ui.PrintCompactDiff(entry.path, entry.oldContent, entry.newContent, globalWidth)
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
	engine := llm.NewEngine(provider, defaultToolRegistry())

	if !provider.Capabilities().ToolCalls {
		return fmt.Errorf("provider %s does not support tool calls", provider.Name())
	}

	// Determine if we should use unified diff format
	useUnifiedDiff := shouldUseUnifiedDiff(cfg)

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

	// Track file contents for applying edits
	fileContents := make(map[string]string)
	for _, f := range files {
		fileContents[f.Path] = f.Content
	}

	promptSpecs := toPromptSpecs(specs)

	// Build prompts based on diff format
	userPrompt := prompt.EditUserPrompt(request, files, promptSpecs)

	if useUnifiedDiff {
		systemPrompt := prompt.UnifiedDiffSystemPrompt(cfg.Edit.Instructions, promptSpecs)
		return processUnifiedDiff(ctx, engine, systemPrompt, userPrompt, fileContents, specs)
	}

	// Use replace (multi-edit) format
	systemPrompt := prompt.EditSystemPrompt(cfg.Edit.Instructions, promptSpecs, editWildcardToken)

	// Get all edits from LLM with spinner
	debugMode := editDebug || debugRaw
	req := llm.Request{
		Messages: []llm.Message{
			llm.SystemText(systemPrompt),
			llm.UserText(userPrompt),
		},
		Tools: []llm.ToolSpec{
			llm.EditToolSpec(),
		},
		ToolChoice: llm.ToolChoice{
			Mode: llm.ToolChoiceName,
			Name: llm.EditToolName,
		},
		ParallelToolCalls: true,
		Debug:             debugMode,
		DebugRaw:          debugRaw,
	}
	result, err := ui.RunWithSpinner(ctx, debugMode, func(ctx context.Context) (any, error) {
		return collectEdits(ctx, engine, req)
	})
	if err != nil {
		if err.Error() == "cancelled" {
			return nil
		}
		return fmt.Errorf("edit failed: %w", err)
	}

	edits, ok := result.([]llm.EditToolCall)
	if !ok {
		return fmt.Errorf("unexpected edits result")
	}
	if len(edits) == 0 {
		fmt.Println("No edits proposed")
		return nil
	}

	if editPerEdit {
		return processEditsIndividually(edits, fileContents, specs)
	}
	return processEditsConsolidated(edits, fileContents, specs)
}

// processEditsConsolidated groups all edits by file and shows one diff per file
func processEditsConsolidated(edits []llm.EditToolCall, fileContents map[string]string, specs []FileSpec) error {
	// Group edits by file, preserving order
	type fileEdits struct {
		path   string
		edits  []llm.EditToolCall
		errors []string
	}
	fileOrder := []string{}
	editsByFile := make(map[string]*fileEdits)

	for _, edit := range edits {
		// Normalize LLM's path to absolute for consistent lookup
		normalizedPath := absPath(edit.FilePath)
		if _, exists := editsByFile[normalizedPath]; !exists {
			fileOrder = append(fileOrder, normalizedPath)
			editsByFile[normalizedPath] = &fileEdits{path: normalizedPath}
		}
		editsByFile[normalizedPath].edits = append(editsByFile[normalizedPath].edits, edit)
	}

	// Process each file: apply all edits sequentially to compute final content
	type consolidatedFile struct {
		path        string
		oldContent  string
		newContent  string
		editCount   int
		skipReasons []string
	}
	consolidated := make([]consolidatedFile, 0, len(fileOrder))

	for _, path := range fileOrder {
		fe := editsByFile[path]
		cf := consolidatedFile{path: path}

		originalContent, ok := fileContents[path]
		if !ok {
			cf.skipReasons = append(cf.skipReasons, "file not in context")
			consolidated = append(consolidated, cf)
			continue
		}

		cf.oldContent = originalContent
		currentContent := originalContent

		for _, edit := range fe.edits {
			// Skip no-op edits
			if edit.OldString == edit.NewString {
				continue
			}

			// Find the matching spec to check for guards
			var matchSpec *FileSpec
			for i := range specs {
				if specs[i].Path == path {
					matchSpec = &specs[i]
					break
				}
			}

			var match editMatch
			var err error
			if matchSpec != nil && matchSpec.HasGuard {
				// Use guard-scoped matching
				match, err = findEditMatchWithGuard(currentContent, edit.OldString, matchSpec.StartLine, matchSpec.EndLine)
			} else {
				// No guard, search full content
				match, err = findEditMatch(currentContent, edit.OldString)
			}
			if err != nil {
				cf.skipReasons = append(cf.skipReasons, err.Error())
				continue
			}

			currentContent = applyEditMatch(currentContent, match, edit.NewString)
			cf.editCount++
		}

		cf.newContent = currentContent
		consolidated = append(consolidated, cf)
	}

	entries := make([]diffEntry, 0, len(consolidated))
	for _, cf := range consolidated {
		entries = append(entries, diffEntry{
			path:        cf.path,
			oldContent:  cf.oldContent,
			newContent:  cf.newContent,
			skipReasons: cf.skipReasons,
		})
	}

	applied, skipped := applyDiffEntries(entries, editDryRun, diffApplyOptions{})

	if !editDryRun && applied+skipped > 3 {
		fmt.Printf("\n%d files updated, %d skipped\n", applied, skipped)
	}

	fmt.Println()
	return nil
}

// processEditsIndividually handles each edit separately (legacy behavior)
func processEditsIndividually(edits []llm.EditToolCall, fileContents map[string]string, specs []FileSpec) error {
	type processedEdit struct {
		edit       llm.EditToolCall
		oldContent string
		newContent string
		skip       bool
		skipReason string
	}
	processed := make([]processedEdit, 0, len(edits))

	for _, editCall := range edits {
		pe := processedEdit{edit: editCall}
		// Normalize LLM's path to absolute for consistent lookup
		normalizedPath := absPath(editCall.FilePath)

		// Skip no-op edits
		if editCall.OldString == editCall.NewString {
			pe.skip = true
			pe.skipReason = "no change"
			processed = append(processed, pe)
			continue
		}

		content, ok := fileContents[normalizedPath]
		if !ok {
			pe.skip = true
			pe.skipReason = "file not in context"
			processed = append(processed, pe)
			continue
		}

		// Find the matching spec to check for guards
		var matchSpec *FileSpec
		for i := range specs {
			if specs[i].Path == normalizedPath {
				matchSpec = &specs[i]
				break
			}
		}

		var match editMatch
		var err error
		if matchSpec != nil && matchSpec.HasGuard {
			// Use guard-scoped matching
			match, err = findEditMatchWithGuard(content, editCall.OldString, matchSpec.StartLine, matchSpec.EndLine)
		} else {
			// No guard, search full content
			match, err = findEditMatch(content, editCall.OldString)
		}
		if err != nil {
			pe.skip = true
			pe.skipReason = err.Error()
			processed = append(processed, pe)
			continue
		}

		pe.oldContent = content
		pe.newContent = applyEditMatch(content, match, editCall.NewString)
		processed = append(processed, pe)
	}

	entries := make([]diffEntry, 0, len(processed))
	for _, pe := range processed {
		if pe.skip {
			entries = append(entries, diffEntry{
				path:              pe.edit.FilePath,
				skipReasons:       []string{pe.skipReason},
				countSkipIfNoDiff: true,
			})
			continue
		}

		writePath := absPath(pe.edit.FilePath)
		entries = append(entries, diffEntry{
			path:       pe.edit.FilePath,
			writePath:  writePath,
			oldContent: pe.oldContent,
			newContent: pe.newContent,
			onApplied: func(path, newContent string) {
				fileContents[path] = newContent
			},
		})
	}

	applied, skipped := applyDiffEntries(entries, editDryRun, diffApplyOptions{separatorOnAnyOutput: true})

	if !editDryRun && applied+skipped > 5 {
		fmt.Printf("\n%d applied, %d skipped\n", applied, skipped)
	}

	fmt.Println()
	return nil
}

func collectEdits(ctx context.Context, engine *llm.Engine, req llm.Request) ([]llm.EditToolCall, error) {
	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	var edits []llm.EditToolCall
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
		if event.Type == llm.EventToolCall && event.Tool != nil && event.Tool.Name == llm.EditToolName {
			llm.DebugToolCall(req.Debug, *event.Tool)
			edit, err := llm.ParseEditToolCall(*event.Tool)
			if err != nil {
				return nil, err
			}
			edits = append(edits, edit)
		}
	}

	if len(edits) == 0 {
		return nil, fmt.Errorf("no edits returned")
	}
	return edits, nil
}

func validateGuardForReplace(content string, match editMatch, spec FileSpec) error {
	// Count lines before
	lineNum := strings.Count(content[:match.start], "\n") + 1
	endLineNum := lineNum + strings.Count(match.text, "\n")

	// Check if within guard
	if spec.StartLine > 0 && lineNum < spec.StartLine {
		return fmt.Errorf("edit starts at line %d, but guard starts at %d", lineNum, spec.StartLine)
	}
	if spec.EndLine > 0 && endLineNum > spec.EndLine {
		return fmt.Errorf("edit ends at line %d, but guard ends at %d", endLineNum, spec.EndLine)
	}

	return nil
}

// shouldUseUnifiedDiff determines if unified diff format should be used based on config
func shouldUseUnifiedDiff(cfg *config.Config) bool {
	switch cfg.Edit.DiffFormat {
	case "udiff":
		return true
	case "replace":
		return false
	default: // "auto" or empty
		// Use unified diff for Codex models
		model := getActiveModel(cfg)
		return llm.IsCodexModel(model)
	}
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

// processUnifiedDiff handles the unified diff flow
func processUnifiedDiff(ctx context.Context, engine *llm.Engine, systemPrompt, userPrompt string, fileContents map[string]string, specs []FileSpec) error {
	debugMode := editDebug || debugRaw
	req := llm.Request{
		Messages: []llm.Message{
			llm.SystemText(systemPrompt),
			llm.UserText(userPrompt),
		},
		Tools: []llm.ToolSpec{
			llm.UnifiedDiffToolSpec(),
		},
		ToolChoice: llm.ToolChoice{
			Mode: llm.ToolChoiceName,
			Name: llm.UnifiedDiffToolName,
		},
		ParallelToolCalls: false,
		Debug:             debugMode,
		DebugRaw:          debugRaw,
	}
	result, err := ui.RunWithSpinner(ctx, debugMode, func(ctx context.Context) (any, error) {
		return collectUnifiedDiff(ctx, engine, req)
	})
	if err != nil {
		if err.Error() == "cancelled" {
			return nil
		}
		return fmt.Errorf("edit failed: %w", err)
	}

	diffStr, ok := result.(string)
	if !ok {
		return fmt.Errorf("unexpected unified diff result")
	}
	if diffStr == "" {
		fmt.Println("No edits proposed")
		return nil
	}

	// Parse the unified diff
	fileDiffs, err := udiff.Parse(diffStr)
	if err != nil {
		return fmt.Errorf("failed to parse diff: %w", err)
	}

	if len(fileDiffs) == 0 {
		fmt.Println("No edits proposed")
		return nil
	}

	// Apply diffs and show results
	return processUnifiedDiffResults(fileDiffs, fileContents, specs)
}

func collectUnifiedDiff(ctx context.Context, engine *llm.Engine, req llm.Request) (string, error) {
	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var diffs []string
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if event.Type == llm.EventError && event.Err != nil {
			return "", event.Err
		}
		if event.Type == llm.EventToolCall && event.Tool != nil && event.Tool.Name == llm.UnifiedDiffToolName {
			llm.DebugToolCall(req.Debug, *event.Tool)
			diff, err := llm.ParseUnifiedDiff(*event.Tool)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(diff) != "" {
				diffs = append(diffs, diff)
			}
		}
	}

	if len(diffs) == 0 {
		return "", fmt.Errorf("no unified diff returned")
	}
	return strings.Join(diffs, "\n"), nil
}

// processUnifiedDiffResults applies parsed unified diffs and prompts for approval
func processUnifiedDiffResults(fileDiffs []udiff.FileDiff, fileContents map[string]string, specs []FileSpec) error {
	type fileResult struct {
		path       string
		oldContent string
		newContent string
		warnings   []string // Warnings for hunks that failed to apply
	}

	results := make([]fileResult, 0, len(fileDiffs))

	for _, fd := range fileDiffs {
		// Normalize path to absolute
		normalizedPath := absPath(fd.Path)

		oldContent, ok := fileContents[normalizedPath]
		if !ok {
			// Try without normalization (in case LLM used relative path)
			for path, content := range fileContents {
				if strings.HasSuffix(path, "/"+fd.Path) || path == fd.Path {
					normalizedPath = path
					oldContent = content
					ok = true
					break
				}
			}
		}

		if !ok {
			ui.ShowEditSkipped(fd.Path, "file not in context")
			continue
		}

		// Use ApplyWithWarnings to skip failed hunks gracefully
		applyResult := udiff.ApplyWithWarnings(oldContent, fd.Hunks)

		results = append(results, fileResult{
			path:       normalizedPath,
			oldContent: oldContent,
			newContent: applyResult.Content,
			warnings:   applyResult.Warnings,
		})
	}

	entries := make([]diffEntry, 0, len(results))
	for _, r := range results {
		entries = append(entries, diffEntry{
			path:              r.path,
			oldContent:        r.oldContent,
			newContent:        r.newContent,
			skipReasons:       r.warnings,
			countSkipIfNoDiff: len(r.warnings) > 0,
		})
	}

	applied, skipped := applyDiffEntries(entries, editDryRun, diffApplyOptions{})

	if !editDryRun && applied+skipped > 3 {
		fmt.Printf("\n%d files updated, %d skipped\n", applied, skipped)
	}

	fmt.Println()
	return nil
}
