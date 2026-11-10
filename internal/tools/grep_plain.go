package tools

import (
	"context"
	"regexp"

	"github.com/samsaffron/term-llm/internal/grepsearch"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// executePlainGrep is the unchanged ordinary search path, including enrichment.
func (t *GrepTool) executePlainGrep(ctx context.Context, a GrepArgs, searchPath string, contextLines, maxResults int) core.ToolOutput {
	// Try ripgrep first (faster)
	if ripgrepAvailable() {
		result, err := t.executeRipgrep(ctx, a, searchPath, contextLines, maxResults)
		if err != nil {
			if ctx.Err() != nil {
				return grepTimeoutOutput()
			}
			// Fall through to Go implementation on ripgrep error
		} else {
			matches := result.matches
			if len(matches) == 0 {
				return core.TextOutput("No matches found.")
			}
			if a.FilesWithMatches {
				return core.TextOutput(formatGrepDisplay(grepDisplayBaseFrom(ctx), matches, func(m []GrepMatch, _ int) string { return formatFilesWithMatches(m, result.truncated) }))
			}
			return t.finishGrep(ctx, a, searchPath, "", result, contextLines, maxResults, true)
		}
	}

	// Fallback: Go implementation
	// Compile regex
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return toolErrorOutput("", NewToolErrorf(ErrInvalidParams, "invalid regex pattern: %v", err))
	}

	// Collect files to search
	files, err := grepsearch.CollectFiles(searchPath, a.Include, a.Exclude)
	if err != nil {
		return toolErrorOutput("", NewToolErrorf(ErrExecutionFailed, "failed to collect files: %v", err))
	}

	// Sort by modification time (newest first)
	grepsearch.SortFilesByMtime(files)

	// Search files
	var matches []GrepMatch
	for _, file := range files {
		if ctx.Err() != nil {
			return grepTimeoutOutput()
		}

		if len(matches) >= maxResults {
			break
		}

		fileMatches, err := searchFile(file, re, maxResults-len(matches), contextLines)
		if err != nil {
			continue // Skip files that can't be read
		}
		matches = append(matches, fileMatches...)
	}

	if len(matches) == 0 {
		return core.TextOutput("No matches found.")
	}

	if a.FilesWithMatches {
		truncated := len(matches) >= maxResults
		return core.TextOutput(formatGrepDisplay(grepDisplayBaseFrom(ctx), matches, func(m []GrepMatch, _ int) string { return formatFilesWithMatches(m, truncated) }))
	}

	truncated := len(matches) >= maxResults
	return core.TextOutput(formatGrepDisplay(grepDisplayBaseFrom(ctx), matches, func(m []GrepMatch, budget int) string { return formatGrepResultsWithBudget(m, truncated, budget) }))
}

func (t *GrepTool) executeGrep(ctx context.Context, a GrepArgs, root, intent string, contextLines, maxResults int) core.ToolOutput {
	if intent != "" {
		return t.executeIntentGrep(ctx, a, root, intent, contextLines, maxResults)
	}
	return t.executePlainGrep(ctx, a, root, contextLines, maxResults)
}

func grepTimeoutOutput() core.ToolOutput {
	return core.ToolOutput{Content: "grep timed out after 1 minute; try a more specific pattern or path", IsError: true}
}
