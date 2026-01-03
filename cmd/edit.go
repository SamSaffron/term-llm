package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/cmd/udiff"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
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
	ctx := context.Background()

	// Load config
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

	// Apply per-command config overrides
	cfg.ApplyOverrides(cfg.Edit.Provider, cfg.Edit.Model)

	// CLI flag takes precedence (supports provider:model syntax)
	if editProvider != "" {
		provider, model, err := llm.ParseProviderModel(editProvider)
		if err != nil {
			return err
		}
		cfg.ApplyOverrides(provider, model)
	}

	// Initialize theme
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

	// Create provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
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

	// Build prompts based on diff format
	userPrompt := buildEditUserPrompt(request, files, specs)

	if useUnifiedDiff {
		// Check if provider supports unified diff
		udiffProv, ok := provider.(llm.UnifiedDiffProvider)
		if !ok {
			return fmt.Errorf("provider %s does not support unified diff format", provider.Name())
		}

		systemPrompt := buildUnifiedDiffSystemPrompt(cfg.Edit.Instructions, specs)
		return processUnifiedDiff(ctx, udiffProv, systemPrompt, userPrompt, fileContents, specs)
	}

	// Use replace (multi-edit) format
	editProv, ok := provider.(llm.EditToolProvider)
	if !ok {
		return fmt.Errorf("provider %s does not support edit tool", provider.Name())
	}

	systemPrompt := buildEditSystemPrompt(cfg.Edit.Instructions, specs)

	// Get all edits from LLM with spinner
	edits, err := ui.RunEditWithSpinner(ctx, editProv, systemPrompt, userPrompt, editDebug)
	if err != nil {
		if err.Error() == "cancelled" {
			return nil
		}
		return fmt.Errorf("edit failed: %w", err)
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

	// Calculate global max width
	globalWidth := 0
	for _, cf := range consolidated {
		if cf.editCount > 0 {
			w := ui.CalcDiffWidth(cf.oldContent, cf.newContent)
			if w > globalWidth {
				globalWidth = w
			}
		}
	}

	// Display and apply
	var applied, skipped int
	first := true
	for _, cf := range consolidated {
		// Show any skip reasons
		for _, reason := range cf.skipReasons {
			ui.ShowEditSkipped(cf.path, reason)
		}

		// Skip if no edits or content unchanged
		if cf.editCount == 0 || cf.oldContent == cf.newContent {
			continue
		}

		if !first {
			fmt.Println()
		}
		first = false

		// Show consolidated diff
		ui.PrintCompactDiff(cf.path, cf.oldContent, cf.newContent, globalWidth)

		if editDryRun {
			continue
		}

		if !ui.PromptApplyEdit() {
			skipped++
			continue
		}

		if err := os.WriteFile(cf.path, []byte(cf.newContent), 0644); err != nil {
			fmt.Printf("  error: %s\n", err.Error())
			skipped++
			continue
		}

		applied++
	}

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

	globalWidth := 0
	for _, pe := range processed {
		if !pe.skip {
			w := ui.CalcDiffWidth(pe.oldContent, pe.newContent)
			if w > globalWidth {
				globalWidth = w
			}
		}
	}

	var applied, skipped int
	for i, pe := range processed {
		if pe.skip {
			ui.ShowEditSkipped(pe.edit.FilePath, pe.skipReason)
			skipped++
			continue
		}

		if i > 0 {
			fmt.Println()
		}

		ui.PrintCompactDiff(pe.edit.FilePath, pe.oldContent, pe.newContent, globalWidth)

		if editDryRun {
			continue
		}

		if !ui.PromptApplyEdit() {
			skipped++
			continue
		}

		writePath := absPath(pe.edit.FilePath)
		if err := os.WriteFile(writePath, []byte(pe.newContent), 0644); err != nil {
			fmt.Printf("  error: %s\n", err.Error())
			skipped++
			continue
		}

		fileContents[writePath] = pe.newContent
		applied++
	}

	if !editDryRun && applied+skipped > 5 {
		fmt.Printf("\n%d applied, %d skipped\n", applied, skipped)
	}

	fmt.Println()
	return nil
}

func buildEditSystemPrompt(instructions string, specs []FileSpec) string {
	cwd, _ := os.Getwd()
	base := fmt.Sprintf(`You are an expert code editor. Use the edit tool to make changes to files.

Context:
- Operating System: %s
- Architecture: %s
- Current Directory: %s`, runtime.GOOS, runtime.GOARCH, cwd)

	if instructions != "" {
		base += fmt.Sprintf("\n- User Context: %s", instructions)
	}

	base += fmt.Sprintf(`

Rules:
1. Make minimal, focused changes
2. Preserve existing code style
3. Use the edit tool for each change - you can call it multiple times
4. The edit tool does find/replace: old_string must match exactly
5. You may include the literal token %s in old_string to match any sequence of characters (including newlines)
6. Include enough context in old_string (especially around %s) to be unique`, editWildcardToken, editWildcardToken)

	// Add guard info
	hasGuards := false
	for _, spec := range specs {
		if spec.HasGuard {
			hasGuards = true
			base += fmt.Sprintf("\n\nIMPORTANT: For %s, only modify lines %d-%d. The <editable-region> block shows the exact content you may edit with line numbers.",
				spec.Path, spec.StartLine, spec.EndLine)
		}
	}
	if hasGuards {
		base += "\n\nYour old_string MUST match text within the editable region. Use the line numbers in <editable-region> to ensure your edit is within bounds."
	}

	return base
}

func buildEditUserPrompt(request string, files []input.FileContent, specs []FileSpec) string {
	var sb strings.Builder

	sb.WriteString("Files:\n\n")
	for _, f := range files {
		sb.WriteString(fmt.Sprintf("<file path=\"%s\">\n", f.Path))
		sb.WriteString(f.Content)
		if !strings.HasSuffix(f.Content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("</file>\n\n")
	}

	// Add editable region blocks for guarded files
	for _, spec := range specs {
		if spec.HasGuard {
			// Find the matching file content
			for _, f := range files {
				if f.Path == spec.Path {
					excerpt := extractLineRange(f.Content, spec.StartLine, spec.EndLine)
					sb.WriteString(fmt.Sprintf("<editable-region path=\"%s\" lines=\"%d-%d\">\n",
						spec.Path, spec.StartLine, spec.EndLine))
					sb.WriteString(excerpt)
					if !strings.HasSuffix(excerpt, "\n") {
						sb.WriteString("\n")
					}
					sb.WriteString("</editable-region>\n\n")
					break
				}
			}
		}
	}

	sb.WriteString(fmt.Sprintf("Request: %s", request))
	return sb.String()
}

// extractLineRange extracts lines startLine to endLine (1-indexed, inclusive) from content
func extractLineRange(content string, startLine, endLine int) string {
	lines := strings.Split(content, "\n")

	// Adjust for 0-based indexing
	start := startLine - 1
	if start < 0 {
		start = 0
	}
	end := endLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start >= len(lines) {
		return ""
	}

	// Build output with line numbers
	var sb strings.Builder
	for i := start; i < end; i++ {
		sb.WriteString(fmt.Sprintf("%d: %s\n", i+1, lines[i]))
	}
	return strings.TrimSuffix(sb.String(), "\n")
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
	case "gemini":
		return cfg.Gemini.Model
	case "zen":
		return cfg.Zen.Model
	default:
		return ""
	}
}

// buildUnifiedDiffSystemPrompt builds a system prompt for unified diff format
func buildUnifiedDiffSystemPrompt(instructions string, specs []FileSpec) string {
	cwd, _ := os.Getwd()
	base := fmt.Sprintf(`You are an expert code editor. Use the unified_diff tool to make changes to files.

Context:
- Operating System: %s
- Architecture: %s
- Current Directory: %s`, runtime.GOOS, runtime.GOARCH, cwd)

	if instructions != "" {
		base += fmt.Sprintf("\n- User Context: %s", instructions)
	}

	base += `

UNIFIED DIFF FORMAT:

--- path/to/file
+++ path/to/file
@@ context to locate (e.g., func ProcessData) @@
 context line (space prefix = unchanged, used to find location)
-line being removed
+line being added

LINE PREFIXES:
- Space " " = context line (unchanged, anchors position)
- Minus "-" = line being removed from original
- Plus "+"  = line being added in replacement

ELISION (-...) FOR LARGE REPLACEMENTS:
When replacing 10+ lines, use -... instead of listing every removed line:

--- file.go
+++ file.go
@@ func BigFunction @@
-func BigFunction() error {
-...
-}
+func BigFunction() error {
+    return simplifiedImpl()
+}

CRITICAL: After -... you MUST have an end anchor (the -} above) so we know where elision stops.
The -... matches everything between -func BigFunction()... and -}.

SMALL CHANGES - LIST ALL LINES:
For changes under 10 lines, list each line explicitly:

--- file.go
+++ file.go
@@ func SmallFunc @@
 func SmallFunc() {
-    oldLine1()
-    oldLine2()
+    newLine1()
+    newLine2()
 }

ADDING NEW CODE (no - lines needed):

--- file.go
+++ file.go
@@ func Existing @@
 func Existing() {
     keepThis()
+    addedLine()
 }

MULTIPLE FILES: Use separate --- +++ blocks for each file.`

	// Add guard info
	for _, spec := range specs {
		if spec.HasGuard {
			base += fmt.Sprintf("\n\nIMPORTANT: For %s, only modify lines %d-%d.",
				spec.Path, spec.StartLine, spec.EndLine)
		}
	}

	return base
}

// processUnifiedDiff handles the unified diff flow
func processUnifiedDiff(ctx context.Context, provider llm.UnifiedDiffProvider, systemPrompt, userPrompt string, fileContents map[string]string, specs []FileSpec) error {
	// Get unified diff from LLM with spinner
	diffStr, err := ui.RunUnifiedDiffWithSpinner(ctx, provider, systemPrompt, userPrompt, editDebug)
	if err != nil {
		if err.Error() == "cancelled" {
			return nil
		}
		return fmt.Errorf("edit failed: %w", err)
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

// processUnifiedDiffResults applies parsed unified diffs and prompts for approval
func processUnifiedDiffResults(fileDiffs []udiff.FileDiff, fileContents map[string]string, specs []FileSpec) error {
	type fileResult struct {
		path       string
		oldContent string
		newContent string
		err        error
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

		newContent, err := udiff.Apply(oldContent, fd.Hunks)
		if err != nil {
			results = append(results, fileResult{
				path: normalizedPath,
				err:  err,
			})
			continue
		}

		results = append(results, fileResult{
			path:       normalizedPath,
			oldContent: oldContent,
			newContent: newContent,
		})
	}

	// Calculate global max width for consistent diff display
	globalWidth := 0
	for _, r := range results {
		if r.err == nil && r.oldContent != r.newContent {
			w := ui.CalcDiffWidth(r.oldContent, r.newContent)
			if w > globalWidth {
				globalWidth = w
			}
		}
	}

	// Display and apply
	var applied, skipped int
	first := true

	for _, r := range results {
		if r.err != nil {
			ui.ShowEditSkipped(r.path, r.err.Error())
			skipped++
			continue
		}

		if r.oldContent == r.newContent {
			continue
		}

		if !first {
			fmt.Println()
		}
		first = false

		// Show diff
		ui.PrintCompactDiff(r.path, r.oldContent, r.newContent, globalWidth)

		if editDryRun {
			continue
		}

		if !ui.PromptApplyEdit() {
			skipped++
			continue
		}

		if err := os.WriteFile(r.path, []byte(r.newContent), 0644); err != nil {
			fmt.Printf("  error: %s\n", err.Error())
			skipped++
			continue
		}

		applied++
	}

	if !editDryRun && applied+skipped > 3 {
		fmt.Printf("\n%d files updated, %d skipped\n", applied, skipped)
	}

	fmt.Println()
	return nil
}
