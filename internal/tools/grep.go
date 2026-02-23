package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/samsaffron/term-llm/internal/llm"
)

// GrepTool implements the grep tool.
type GrepTool struct {
	approval *ApprovalManager
	limits   OutputLimits
}

// NewGrepTool creates a new GrepTool.
func NewGrepTool(approval *ApprovalManager, limits OutputLimits) *GrepTool {
	return &GrepTool{
		approval: approval,
		limits:   limits,
	}
}

// ripgrepAvailable checks if ripgrep (rg) is available.
func ripgrepAvailable() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

// rgMatch represents a ripgrep JSON match.
type rgMatch struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type rgMatchData struct {
	Path struct {
		Text string `json:"text"`
	} `json:"path"`
	Lines struct {
		Text string `json:"text"`
	} `json:"lines"`
	LineNumber     int `json:"line_number"`
	AbsoluteOffset int `json:"absolute_offset"`
}

// executeRipgrep runs ripgrep and returns matches.
func (t *GrepTool) executeRipgrep(ctx context.Context, pattern, searchPath, include, exclude string, contextLines, maxResults int, filesWithMatches bool) ([]GrepMatch, error) {
	// files-with-matches mode: skip JSON parsing entirely, just return filenames
	if filesWithMatches {
		args := []string{"--files-with-matches", "--hidden", "--glob", "!.git"}
		if include != "" {
			args = append(args, "--glob", include)
		}
		if exclude != "" {
			args = append(args, "--glob", "!"+exclude)
		}
		args = append(args, pattern, searchPath)
		cmd := exec.CommandContext(ctx, "rg", args...)
		output, err := cmd.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return nil, nil
			}
			return nil, err
		}
		var matches []GrepMatch
		for _, f := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if f != "" {
				matches = append(matches, GrepMatch{FilePath: f})
			}
		}
		return matches, nil
	}

	args := []string{
		"--json",                                // JSON output for parsing
		"--max-count", strconv.Itoa(maxResults), // Limit per file
		"--context", strconv.Itoa(contextLines), // Context lines
		"--hidden",        // Search hidden files but...
		"--glob", "!.git", // ...exclude .git
	}
	if include != "" {
		args = append(args, "--glob", include)
	}
	if exclude != "" {
		args = append(args, "--glob", "!"+exclude)
	}
	args = append(args, pattern, searchPath)

	cmd := exec.CommandContext(ctx, "rg", args...)
	output, err := cmd.Output()

	// Exit code 1 means no matches, which is not an error
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}

	return parseRipgrepOutput(output, maxResults)
}

// pendingMatch tracks context for building ripgrep results.
type pendingMatch struct {
	filePath   string
	lineNumber int
	matchLine  string
	before     []string
	after      []string
}

// parseRipgrepOutput parses ripgrep JSON output into GrepMatches.
func parseRipgrepOutput(output []byte, maxResults int) ([]GrepMatch, error) {
	var matches []GrepMatch
	var pending *pendingMatch

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}

		var msg rgMatch
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "match":
			// Flush any pending match
			if pending != nil {
				matches = append(matches, buildMatchFromPending(pending))
				if len(matches) >= maxResults {
					return matches, nil
				}
			}

			var data rgMatchData
			if err := json.Unmarshal(msg.Data, &data); err != nil {
				continue
			}

			pending = &pendingMatch{
				filePath:   data.Path.Text,
				lineNumber: data.LineNumber,
				matchLine:  strings.TrimSuffix(data.Lines.Text, "\n"),
			}

		case "context":
			if pending == nil {
				continue
			}
			var data rgMatchData
			if err := json.Unmarshal(msg.Data, &data); err != nil {
				continue
			}

			contextLine := strings.TrimSuffix(data.Lines.Text, "\n")
			if data.LineNumber < pending.lineNumber {
				pending.before = append(pending.before, contextLine)
			} else {
				pending.after = append(pending.after, contextLine)
			}
		}
	}

	// Flush final pending match
	if pending != nil {
		matches = append(matches, buildMatchFromPending(pending))
	}

	return matches, nil
}

func buildMatchFromPending(p *pendingMatch) GrepMatch {
	var sb strings.Builder
	startLine := p.lineNumber - len(p.before)

	for i, line := range p.before {
		sb.WriteString(fmt.Sprintf("  %d: %s\n", startLine+i, line))
	}
	sb.WriteString(fmt.Sprintf("> %d: %s\n", p.lineNumber, p.matchLine))
	for i, line := range p.after {
		sb.WriteString(fmt.Sprintf("  %d: %s\n", p.lineNumber+1+i, line))
	}

	return GrepMatch{
		FilePath:   p.filePath,
		LineNumber: p.lineNumber,
		Match:      p.matchLine,
		Context:    strings.TrimSuffix(sb.String(), "\n"),
	}
}

// GrepArgs are the arguments for grep.
type GrepArgs struct {
	Pattern          string `json:"pattern"`
	Path             string `json:"path,omitempty"`
	Include          string `json:"include,omitempty"` // glob filter e.g., "*.go"
	Exclude          string `json:"exclude,omitempty"` // glob pattern to exclude e.g., "vendor/**"
	MaxResults       int    `json:"max_results,omitempty"`
	ContextLines     int    `json:"context_lines,omitempty"`      // lines of context around match (default 3)
	FilesWithMatches bool   `json:"files_with_matches,omitempty"` // return filenames only
}

// GrepMatch represents a single grep match.
type GrepMatch struct {
	FilePath   string `json:"file_path"`
	LineNumber int    `json:"line_number"`
	Match      string `json:"match"`
	Context    string `json:"context,omitempty"` // 3 lines of context
}

func (t *GrepTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        GrepToolName,
		Description: "Search file contents using regex patterns (RE2 syntax). Returns matches with context.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"description": "Regular expression pattern to search for (RE2 syntax)",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "File or directory to search in (defaults to current directory)",
				},
				"include": map[string]interface{}{
					"type":        "string",
					"description": "Glob filter for files, e.g., '*.go' or '*.{js,ts}'",
				},
				"exclude": map[string]interface{}{
					"type":        "string",
					"description": "Glob pattern for paths to exclude, e.g. 'vendor/**' or '**/*_test.go'",
				},
				"context_lines": map[string]interface{}{
					"type":        "integer",
					"description": "Lines of context around each match (default: 3)",
					"default":     3,
				},
				"files_with_matches": map[string]interface{}{
					"type":        "boolean",
					"description": "Return only filenames containing matches, not the match lines (default: false)",
				},
				"max_results": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of results (default: 100)",
					"default":     100,
				}},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
	}
}

func (t *GrepTool) Preview(args json.RawMessage) string {
	var a GrepArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Pattern == "" {
		return ""
	}
	pattern := a.Pattern
	if len(pattern) > 30 {
		pattern = pattern[:27] + "..."
	}
	result := fmt.Sprintf("/%s/", pattern)
	if a.Path != "" {
		result += " in " + a.Path
	}
	if a.Include != "" {
		result += " (" + a.Include + ")"
	}
	if a.Exclude != "" {
		result += " exclude:" + a.Exclude
	}
	return result
}

func (t *GrepTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	warning := WarnUnknownParams(args, []string{
		"pattern", "path", "include", "exclude",
		"max_results", "context_lines", "files_with_matches",
	})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}

	var a GrepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Pattern == "" {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "pattern is required"))), nil
	}

	// Set defaults
	searchPath := a.Path
	if searchPath == "" {
		var err error
		searchPath, err = os.Getwd()
		if err != nil {
			return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot get working directory: %v", err))), nil
		}
	}

	maxResults := a.MaxResults
	if maxResults <= 0 {
		maxResults = t.limits.MaxResults
	}

	contextLines := a.ContextLines
	if contextLines <= 0 {
		contextLines = 3
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(GrepToolName, searchPath, a.Pattern, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return textOutput(formatToolError(toolErr)), nil
			}
			return textOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return textOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", searchPath))), nil
		}
	}

	// Try ripgrep first (faster)
	if ripgrepAvailable() {
		matches, err := t.executeRipgrep(ctx, a.Pattern, searchPath, a.Include, a.Exclude, contextLines, maxResults, a.FilesWithMatches)
		if err != nil {
			if ctx.Err() != nil {
				return textOutput("grep timed out after 1 minute; try a more specific pattern or path"), nil
			}
			// Fall through to Go implementation on ripgrep error
		} else {
			if len(matches) == 0 {
				return textOutput("No matches found."), nil
			}
			if a.FilesWithMatches {
				return textOutput(formatFilesWithMatches(matches)), nil
			}
			return textOutput(formatGrepResults(matches, len(matches) >= maxResults)), nil
		}
	}

	// Fallback: Go implementation
	// Compile regex
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "invalid regex pattern: %v", err))), nil
	}

	// Collect files to search
	files, err := collectFiles(searchPath, a.Include, a.Exclude)
	if err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to collect files: %v", err))), nil
	}

	// Sort by modification time (newest first)
	sortFilesByMtime(files)

	// Search files
	var matches []GrepMatch
	for _, file := range files {
		if ctx.Err() != nil {
			return textOutput("grep timed out after 1 minute; try a more specific pattern or path"), nil
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
		return textOutput("No matches found."), nil
	}

	if a.FilesWithMatches {
		return textOutput(formatFilesWithMatches(matches)), nil
	}

	// Format results
	return textOutput(formatGrepResults(matches, len(matches) >= maxResults)), nil
}

// collectFiles collects files to search.
func collectFiles(searchPath, include, exclude string) ([]string, error) {
	info, err := os.Stat(searchPath)
	if err != nil {
		return nil, err
	}

	// Single file
	if !info.IsDir() {
		return []string{searchPath}, nil
	}

	// Directory - walk and collect files
	var files []string
	err = filepath.WalkDir(searchPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		relPath, _ := filepath.Rel(searchPath, path)

		// Skip hidden directories
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}

		if d.IsDir() {
			// Skip excluded directories early
			if exclude != "" {
				if matched, _ := doublestar.Match(exclude, relPath); matched {
					return filepath.SkipDir
				}
			}
			return nil
		}

		// Apply include filter against relative path
		if include != "" {
			if matched, _ := doublestar.Match(include, relPath); !matched {
				return nil
			}
		}

		// Apply exclude filter against relative path
		if exclude != "" {
			if matched, _ := doublestar.Match(exclude, relPath); matched {
				return nil
			}
		}

		files = append(files, path)
		return nil
	})

	return files, err
}

// sortFilesByMtime sorts files by modification time (newest first).
func sortFilesByMtime(files []string) {
	type fileInfo struct {
		path  string
		mtime int64
	}

	infos := make([]fileInfo, 0, len(files))
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			infos = append(infos, fileInfo{path: f, mtime: 0})
			continue
		}
		infos = append(infos, fileInfo{path: f, mtime: info.ModTime().Unix()})
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].mtime > infos[j].mtime
	})

	for i, info := range infos {
		files[i] = info.path
	}
}

// searchFile searches a single file for matches.
func searchFile(path string, re *regexp.Regexp, maxMatches, contextLines int) ([]GrepMatch, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Check for binary
	buf := make([]byte, 512)
	n, err := file.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}

	contentType := http.DetectContentType(buf[:n])
	if !strings.HasPrefix(contentType, "text/") &&
		!strings.Contains(contentType, "json") &&
		!strings.Contains(contentType, "xml") {
		return nil, fmt.Errorf("binary file")
	}

	// Reset to beginning
	if _, err := file.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("seek: %w", err)
	}

	// Read all lines for context support
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Search for matches
	var matches []GrepMatch
	for lineNum, line := range lines {
		if re.MatchString(line) {
			match := GrepMatch{
				FilePath:   path,
				LineNumber: lineNum + 1, // 1-indexed
				Match:      line,
				Context:    buildContext(lines, lineNum, contextLines),
			}
			matches = append(matches, match)

			if len(matches) >= maxMatches {
				break
			}
		}
	}

	return matches, nil
}

// buildContext builds context lines around a match.
func buildContext(lines []string, matchIdx, contextLines int) string {
	start := matchIdx - contextLines
	if start < 0 {
		start = 0
	}
	end := matchIdx + contextLines + 1
	if end > len(lines) {
		end = len(lines)
	}

	var sb strings.Builder
	for i := start; i < end; i++ {
		prefix := "  "
		if i == matchIdx {
			prefix = "> "
		}
		sb.WriteString(fmt.Sprintf("%s%d: %s\n", prefix, i+1, lines[i]))
	}

	return strings.TrimSuffix(sb.String(), "\n")
}

// formatFilesWithMatches formats a deduplicated list of filenames containing matches.
func formatFilesWithMatches(matches []GrepMatch) string {
	var sb strings.Builder
	seen := make(map[string]bool)
	for _, m := range matches {
		if !seen[m.FilePath] {
			seen[m.FilePath] = true
			sb.WriteString(m.FilePath + "\n")
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// formatGrepResults formats grep results for the LLM.
func formatGrepResults(matches []GrepMatch, truncated bool) string {
	var sb strings.Builder

	for i, m := range matches {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(fmt.Sprintf("%s:%d\n", m.FilePath, m.LineNumber))
		sb.WriteString(m.Context)
		sb.WriteString("\n")
	}

	if truncated {
		sb.WriteString("\n[Results truncated at limit]")
	}

	return sb.String()
}
