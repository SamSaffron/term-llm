package edit

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/cmd/udiff"
	"github.com/samsaffron/term-llm/internal/llm"
)

// MaxRetryAttempts is the maximum number of retry attempts for failed edits.
const MaxRetryAttempts = 3

// EditResult represents the result of applying an edit to a file.
type EditResult struct {
	Path        string
	OldContent  string
	NewContent  string
	Format      Format
	MatchLevel  MatchLevel // Only for search/replace
	Error       error
	DiffWarning string // For unified diff with partial failures
}

// ExecutorConfig configures the stream edit executor.
type ExecutorConfig struct {
	// FileContents maps file paths to their current content.
	FileContents map[string]string

	// Guards maps file paths to their allowed line ranges (1-indexed).
	// If a file has a guard, edits are only allowed within that range.
	Guards map[string][2]int // [startLine, endLine]

	// OnProgress is called with progress updates during execution.
	OnProgress func(msg string)

	// OnFileStart is called when a file edit begins.
	OnFileStart func(path string)

	// OnSearchMatch is called when a search matches successfully.
	OnSearchMatch func(path string, level MatchLevel)

	// OnSearchFail is called when a search fails to match.
	OnSearchFail func(path string, search string, err error)

	// OnEditApplied is called when an edit is successfully applied.
	OnEditApplied func(path string, oldContent, newContent string)

	// OnAbout is called when the about section is received.
	OnAbout func(content string)

	// Debug enables debug output.
	Debug bool

	// DebugRaw enables raw request/response output.
	DebugRaw bool
}

// StreamEditExecutor executes streaming edits with validation and retry.
type StreamEditExecutor struct {
	config    ExecutorConfig
	provider  llm.Provider
	model     string
	parser    *StreamParser
	results   []EditResult
	aboutText string

	// For retry handling
	retryContext *RetryContext
	accumulated  strings.Builder // Full LLM output accumulated
}

// NewStreamEditExecutor creates a new executor.
func NewStreamEditExecutor(provider llm.Provider, model string, config ExecutorConfig) *StreamEditExecutor {
	return &StreamEditExecutor{
		config:   config,
		provider: provider,
		model:    model,
	}
}

// Execute runs the streaming edit with the given messages.
// Returns the results and about text, or an error.
func (e *StreamEditExecutor) Execute(ctx context.Context, messages []llm.Message) ([]EditResult, string, error) {
	for attempt := 0; attempt < MaxRetryAttempts; attempt++ {
		results, aboutText, retryCtx, err := e.executeOnce(ctx, messages)
		if err == nil {
			return results, aboutText, nil
		}

		if retryCtx == nil {
			// Not a retriable error
			return nil, "", err
		}

		// Build retry prompt and add to messages
		retryPrompt := BuildRetryPrompt(*retryCtx)
		if e.config.OnProgress != nil {
			e.config.OnProgress(fmt.Sprintf("Retry attempt %d/%d", attempt+1, MaxRetryAttempts))
		}

		// Add the partial assistant response and error feedback
		if e.accumulated.Len() > 0 {
			messages = append(messages, llm.AssistantText(e.accumulated.String()))
		}
		messages = append(messages, llm.UserText(retryPrompt))
	}

	return nil, "", fmt.Errorf("edit failed after %d attempts", MaxRetryAttempts)
}

// executeOnce runs a single attempt at streaming edits.
func (e *StreamEditExecutor) executeOnce(ctx context.Context, messages []llm.Message) ([]EditResult, string, *RetryContext, error) {
	e.results = nil
	e.aboutText = ""
	e.retryContext = nil
	e.accumulated.Reset()

	// Working copy of file contents
	workingContents := make(map[string]string)
	for path, content := range e.config.FileContents {
		workingContents[path] = content
	}

	// Set up parser callbacks
	callbacks := ParserCallbacks{
		OnFileStart: func(path string) {
			if e.config.OnFileStart != nil {
				e.config.OnFileStart(path)
			}
		},

		OnSearchReady: func(path, search string) error {
			if e.config.Debug {
				searchPreview := search
				if len(searchPreview) > 100 {
					searchPreview = searchPreview[:100] + "..."
				}
				fmt.Fprintf(os.Stderr, "[DEBUG] Search block for %s: %q\n", path, searchPreview)
			}

			content, ok := workingContents[path]
			if !ok {
				if e.config.Debug {
					fmt.Fprintf(os.Stderr, "[DEBUG] File not found: %s\n", path)
				}
				err := fmt.Errorf("file not found: %s", path)
				e.retryContext = &RetryContext{
					FilePath:     path,
					FailedSearch: search,
					Reason:       err.Error(),
				}
				return err
			}

			// Check for guard
			var match MatchResult
			var err error
			if guard, hasGuard := e.config.Guards[path]; hasGuard {
				match, err = FindMatchWithGuard(content, search, guard[0], guard[1])
			} else {
				match, err = FindMatch(content, search)
			}

			if err != nil {
				if e.config.Debug {
					fmt.Fprintf(os.Stderr, "[DEBUG] Search failed: %v\n", err)
				}
				if e.config.OnSearchFail != nil {
					e.config.OnSearchFail(path, search, err)
				}
				e.retryContext = &RetryContext{
					FilePath:      path,
					FailedSearch:  search,
					FileContent:   content,
					Reason:        err.Error(),
					PartialOutput: e.accumulated.String(),
				}
				return err
			}

			if e.config.Debug {
				fmt.Fprintf(os.Stderr, "[DEBUG] Search matched at level: %s\n", match.Level)
			}

			if e.config.OnSearchMatch != nil {
				e.config.OnSearchMatch(path, match.Level)
			}

			return nil
		},

		OnReplaceReady: func(path, search, replace string) {
			content := workingContents[path]

			// Find match again (should succeed since OnSearchReady passed)
			var match MatchResult
			if guard, hasGuard := e.config.Guards[path]; hasGuard {
				match, _ = FindMatchWithGuard(content, search, guard[0], guard[1])
			} else {
				match, _ = FindMatch(content, search)
			}

			// Apply the edit
			oldContent := content
			newContent := ApplyMatch(content, match, replace)
			workingContents[path] = newContent

			e.results = append(e.results, EditResult{
				Path:       path,
				OldContent: oldContent,
				NewContent: newContent,
				Format:     FormatSearchReplace,
				MatchLevel: match.Level,
			})

			if e.config.OnEditApplied != nil {
				e.config.OnEditApplied(path, oldContent, newContent)
			}
		},

		OnDiffReady: func(path string, diffLines []string) error {
			// Filter out spurious empty lines from streaming (LLM often adds blank lines)
			filteredLines := filterDiffEmptyLines(diffLines)

			if e.config.Debug {
				fmt.Fprintf(os.Stderr, "[DEBUG] Processing diff for: %s\n", path)
				fmt.Fprintf(os.Stderr, "[DEBUG] Diff lines (%d total):\n", len(filteredLines))
				for i, line := range filteredLines {
					if i < 20 || i >= len(filteredLines)-5 { // Show first 20 and last 5
						fmt.Fprintf(os.Stderr, "  %3d: %s\n", i, line)
					} else if i == 20 {
						fmt.Fprintf(os.Stderr, "  ... (%d lines omitted) ...\n", len(filteredLines)-25)
					}
				}
			}

			// Try to find the file - handle both absolute and relative paths
			content, resolvedPath, ok := findWorkingContent(workingContents, path)
			if !ok {
				if e.config.Debug {
					fmt.Fprintf(os.Stderr, "[DEBUG] File not found: %s\n", path)
				}
				err := fmt.Errorf("file not found: %s", path)
				e.retryContext = &RetryContext{
					FilePath: path,
					Reason:   err.Error(),
				}
				return err
			}
			path = resolvedPath // Use the resolved path for updates

			if e.config.Debug {
				fmt.Fprintf(os.Stderr, "[DEBUG] Resolved path: %s\n", resolvedPath)
			}

			// Parse and apply the unified diff
			diffText := strings.Join(filteredLines, "\n")
			diffs, err := udiff.Parse(diffText)
			if err != nil {
				if e.config.Debug {
					fmt.Fprintf(os.Stderr, "[DEBUG] Failed to parse diff: %v\n", err)
				}
				e.retryContext = &RetryContext{
					FilePath:      path,
					DiffLines:     filteredLines,
					FileContent:   content,
					Reason:        fmt.Sprintf("failed to parse diff: %v", err),
					PartialOutput: e.accumulated.String(),
				}
				return err
			}

			if e.config.Debug {
				fmt.Fprintf(os.Stderr, "[DEBUG] Parsed %d file diff(s)\n", len(diffs))
				for i, fd := range diffs {
					fmt.Fprintf(os.Stderr, "[DEBUG]   File %d: %s with %d hunk(s)\n", i, fd.Path, len(fd.Hunks))
				}
			}

			// Apply the diffs
			if len(diffs) == 0 {
				if e.config.Debug {
					fmt.Fprintf(os.Stderr, "[DEBUG] No diffs to apply\n")
				}
				return nil
			}

			// Apply hunks - collect all warnings
			oldContent := content
			currentContent := content
			var allWarnings []string

			for _, fileDiff := range diffs {
				result := udiff.ApplyWithWarnings(currentContent, fileDiff.Hunks)
				currentContent = result.Content
				allWarnings = append(allWarnings, result.Warnings...)

				if e.config.Debug {
					fmt.Fprintf(os.Stderr, "[DEBUG] Applied %d hunk(s), %d warning(s)\n",
						len(fileDiff.Hunks), len(result.Warnings))
					for _, w := range result.Warnings {
						fmt.Fprintf(os.Stderr, "[DEBUG]   Warning: %s\n", w)
					}
				}
			}

			// If ANY hunks failed, trigger retry instead of partial success
			if len(allWarnings) > 0 {
				if e.config.Debug {
					fmt.Fprintf(os.Stderr, "[DEBUG] Hunks failed - triggering retry\n")
				}
				warning := strings.Join(allWarnings, "; ")
				e.retryContext = &RetryContext{
					FilePath:      path,
					DiffLines:     filteredLines,
					FileContent:   content,
					Reason:        fmt.Sprintf("some hunks failed to apply: %s", warning),
					PartialOutput: e.accumulated.String(),
				}
				return fmt.Errorf("hunk application failed: %s", warning)
			}

			if e.config.Debug {
				fmt.Fprintf(os.Stderr, "[DEBUG] All hunks applied successfully\n")
			}

			e.results = append(e.results, EditResult{
				Path:       path,
				OldContent: oldContent,
				NewContent: currentContent,
				Format:     FormatUnifiedDiff,
			})

			workingContents[path] = currentContent

			if e.config.OnEditApplied != nil {
				e.config.OnEditApplied(path, oldContent, currentContent)
			}

			return nil
		},

		OnFileComplete: func(edit FileEdit) {
			// Already handled in OnReplaceReady/OnDiffReady
		},

		OnAboutComplete: func(content string) {
			e.aboutText = content
			if e.config.OnAbout != nil {
				e.config.OnAbout(content)
			}
		},
	}

	e.parser = NewStreamParser(callbacks)

	// Create stream request (no tools)
	req := llm.Request{
		Model:    e.model,
		Messages: messages,
		Debug:    e.config.Debug,
		DebugRaw: e.config.DebugRaw,
	}

	// Debug output before streaming
	if e.config.DebugRaw {
		llm.DebugRawRequest(true, e.provider.Name(), e.provider.Credential(), req, "Request")
	}

	// Create cancellable context for halting
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	rawStream, err := e.provider.Stream(streamCtx, req)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to start stream: %w", err)
	}
	defer rawStream.Close()

	// Wrap stream for debug output
	stream := llm.WrapDebugStream(e.config.DebugRaw, rawStream)

	// Process stream events
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Check if we halted intentionally
			if e.parser.IsHalted() {
				return nil, "", e.retryContext, e.parser.HaltError()
			}
			// Check for context cancellation (from our halt)
			if ctx.Err() != nil {
				return nil, "", e.retryContext, e.parser.HaltError()
			}
			return nil, "", nil, fmt.Errorf("stream error: %w", err)
		}

		switch event.Type {
		case llm.EventTextDelta:
			e.accumulated.WriteString(event.Text)
			if err := e.parser.Feed(event.Text); err != nil {
				cancel() // Halt the stream
				return nil, "", e.retryContext, err
			}

		case llm.EventError:
			return nil, "", nil, event.Err

		case llm.EventDone:
			// Stream complete
		}
	}

	// Finish parsing any remaining content
	if err := e.parser.Finish(); err != nil {
		return nil, "", e.retryContext, err
	}

	return e.results, e.aboutText, nil, nil
}

// Results returns the edit results from the last execution.
func (e *StreamEditExecutor) Results() []EditResult {
	return e.results
}

// AboutText returns the about section text from the last execution.
func (e *StreamEditExecutor) AboutText() string {
	return e.aboutText
}

// AccumulatedOutput returns the full LLM output accumulated during execution.
func (e *StreamEditExecutor) AccumulatedOutput() string {
	return e.accumulated.String()
}

// findWorkingContent finds content for a path, handling both absolute and relative paths.
// Returns (content, resolvedPath, found).
func findWorkingContent(contents map[string]string, path string) (string, string, bool) {
	// Direct match
	if content, ok := contents[path]; ok {
		return content, path, true
	}

	// Try matching by basename (LLM often outputs relative paths)
	basename := filepath.Base(path)
	for fullPath, content := range contents {
		if filepath.Base(fullPath) == basename {
			return content, fullPath, true
		}
	}

	// Try suffix match (e.g., "foo/bar.go" matching "/full/path/foo/bar.go")
	for fullPath, content := range contents {
		if strings.HasSuffix(fullPath, "/"+path) || strings.HasSuffix(fullPath, "\\"+path) {
			return content, fullPath, true
		}
	}

	return "", "", false
}

// filterDiffEmptyLines removes spurious empty lines that LLMs add between diff lines.
// In unified diff format, empty lines are only valid as context lines within hunks
// (where they represent blank lines in the source file and start with a space).
// This function removes:
// - Empty lines in the header section (before first @@)
// - Standalone empty lines between diff lines (not starting with space/+/-)
func filterDiffEmptyLines(lines []string) []string {
	result := make([]string, 0, len(lines))
	inHunk := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Check if this starts a hunk
		if strings.HasPrefix(trimmed, "@@") {
			inHunk = true
			result = append(result, line)
			continue
		}

		// In header section: skip empty lines
		if !inHunk {
			if trimmed == "" {
				continue
			}
			result = append(result, line)
			continue
		}

		// In hunk: empty line is only valid if it's a context line (starts with space)
		// or if it's truly empty (represents a blank line in source)
		if line == "" {
			// Skip empty lines - they're artifacts from streaming
			// Real blank context lines would be " " (space prefix)
			continue
		}

		result = append(result, line)
	}

	return result
}
