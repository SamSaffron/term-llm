package tools

import (
	"context"
	"regexp"
)

// executePlainGrep is the unchanged ordinary search path, including enrichment.
func (t *GrepTool) executePlainGrep(ctx context.Context, a GrepArgs, searchPath string, contextLines, maxResults int) string {
	// Try ripgrep first (faster)
	if ripgrepAvailable() {
		result, err := t.executeRipgrep(ctx, a, searchPath, contextLines, maxResults)
		if err != nil {
			if ctx.Err() != nil {
				return "grep timed out after 1 minute; try a more specific pattern or path"
			}
			// Fall through to Go implementation on ripgrep error
		} else {
			matches := result.matches
			if len(matches) == 0 {
				return "No matches found."
			}
			if a.FilesWithMatches {
				return formatGrepDisplay(grepDisplayBaseFrom(ctx), matches, func(m []GrepMatch, _ int) string { return formatFilesWithMatches(m, result.truncated) })
			}
			return t.finishGrep(ctx, a, searchPath, "", result, contextLines, maxResults, true)
		}
	}

	// Fallback: Go implementation
	// Compile regex
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return formatToolError(NewToolErrorf(ErrInvalidParams, "invalid regex pattern: %v", err))
	}

	// Collect files to search
	files, err := collectFiles(searchPath, a.Include, a.Exclude)
	if err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to collect files: %v", err))
	}

	// Sort by modification time (newest first)
	sortFilesByMtime(files)

	// Search files
	var matches []GrepMatch
	for _, file := range files {
		if ctx.Err() != nil {
			return "grep timed out after 1 minute; try a more specific pattern or path"
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
		return "No matches found."
	}

	if a.FilesWithMatches {
		truncated := len(matches) >= maxResults
		return formatGrepDisplay(grepDisplayBaseFrom(ctx), matches, func(m []GrepMatch, _ int) string { return formatFilesWithMatches(m, truncated) })
	}

	truncated := len(matches) >= maxResults
	return formatGrepDisplay(grepDisplayBaseFrom(ctx), matches, func(m []GrepMatch, budget int) string { return formatGrepResultsWithBudget(m, truncated, budget) })
}
