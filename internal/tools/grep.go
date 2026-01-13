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
	LineNumber   int `json:"line_number"`
	AbsoluteOffset int `json:"absolute_offset"`
}

// executeRipgrep runs ripgrep and returns matches.
func (t *GrepTool) executeRipgrep(ctx context.Context, pattern, searchPath, include string, maxResults int) ([]GrepMatch, error) {
	args := []string{
		"--json",           // JSON output for parsing
		"--max-count", strconv.Itoa(maxResults), // Limit per file
		"--context", "3",   // Context lines
		"--hidden",         // Search hidden files but...
		"--glob", "!.git",  // ...exclude .git
	}

	if include != "" {
		args = append(args, "--glob", include)
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
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Include    string `json:"include,omitempty"` // glob filter e.g., "*.go"
	MaxResults int    `json:"max_results,omitempty"`
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
				"max_results": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of results (default: 100)",
					"default":     100,
				},
			},
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
	return result
}

func (t *GrepTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a GrepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return formatToolError(NewToolError(ErrInvalidParams, err.Error())), nil
	}

	if a.Pattern == "" {
		return formatToolError(NewToolError(ErrInvalidParams, "pattern is required")), nil
	}

	// Set defaults
	searchPath := a.Path
	if searchPath == "" {
		var err error
		searchPath, err = os.Getwd()
		if err != nil {
			return formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot get working directory: %v", err)), nil
		}
	}

	maxResults := a.MaxResults
	if maxResults <= 0 {
		maxResults = t.limits.MaxResults
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(GrepToolName, searchPath, a.Pattern, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return formatToolError(toolErr), nil
			}
			return formatToolError(NewToolError(ErrPermissionDenied, err.Error())), nil
		}
		if outcome == Cancel {
			return formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", searchPath)), nil
		}
	}

	// Try ripgrep first (faster)
	if ripgrepAvailable() {
		matches, err := t.executeRipgrep(ctx, a.Pattern, searchPath, a.Include, maxResults)
		if err == nil {
			if len(matches) == 0 {
				return "No matches found.", nil
			}
			return formatGrepResults(matches, len(matches) >= maxResults), nil
		}
		// Fall through to Go implementation on ripgrep error
	}

	// Fallback: Go implementation
	// Compile regex
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return formatToolError(NewToolErrorf(ErrInvalidParams, "invalid regex pattern: %v", err)), nil
	}

	// Collect files to search
	files, err := collectFiles(searchPath, a.Include)
	if err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to collect files: %v", err)), nil
	}

	// Sort by modification time (newest first)
	sortFilesByMtime(files)

	// Search files
	var matches []GrepMatch
	for _, file := range files {
		if len(matches) >= maxResults {
			break
		}

		fileMatches, err := searchFile(file, re, maxResults-len(matches))
		if err != nil {
			continue // Skip files that can't be read
		}
		matches = append(matches, fileMatches...)
	}

	if len(matches) == 0 {
		return "No matches found.", nil
	}

	// Format results
	return formatGrepResults(matches, len(matches) >= maxResults), nil
}

// collectFiles collects files to search.
func collectFiles(searchPath, include string) ([]string, error) {
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

		// Skip hidden directories
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}

		if d.IsDir() {
			return nil
		}

		// Apply include filter
		if include != "" {
			match, err := doublestar.Match(include, d.Name())
			if err != nil || !match {
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
func searchFile(path string, re *regexp.Regexp, maxMatches int) ([]GrepMatch, error) {
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
	file.Seek(0, 0)

	// Read all lines for context support
	var lines []string
	scanner := bufio.NewScanner(file)
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
				Context:    buildContext(lines, lineNum, 3),
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
