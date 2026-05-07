package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const (
	// maxLineDisplayLen is the maximum rune length of any line written into
	// grep context output.  Lines beyond this are truncated with "…" to keep
	// token counts predictable on minified / generated files.
	maxLineDisplayLen = 120

	// maxMatchesPerFileDisplay is the maximum number of matches shown per file
	// in the grouped output.  When a file has more matches than this, a note
	// is appended so the model knows to narrow its search.
	maxMatchesPerFileDisplay = 10

	// autoEnrichThreshold is the maximum number of match blocks for which
	// auto-enrichment is applied.  Above this, the result set is large enough
	// that adding more context would bloat the response unhelpfully.
	autoEnrichThreshold = 3

	// maxOutputBytes is the total byte budget for a formatted grep response.
	// File groups that would push the output over this limit are omitted and
	// replaced with a truncation note.  Keeps minified / generated files from
	// producing enormous single-block responses even after per-line truncation.
	maxOutputBytes = 50 * 1024 // 50 KB

	// rgHardMaxOutputLines is the hard safety cap for raw ripgrep output.
	// Once exceeded, the subprocess is terminated and the result is marked
	// truncated before parsing / formatting happens.
	rgHardMaxOutputLines = 10000

	// rgMaxBufferedBytes caps the raw ripgrep stdout captured in memory.
	// This is intentionally much higher than the final display budget because
	// JSON output expands lines substantially before we collapse them back down.
	rgMaxBufferedBytes = 8 * 1024 * 1024 // 8 MB
)

// autoEnrichContextLines returns the number of context lines to use when
// auto-enriching a small result set.  The goal is to give the model enough
// surrounding code to understand each match without a follow-up read_file
// call.
//
//   - 1 block  → 30 lines: wide window to capture the full function/method body
//   - 2–3 blocks → 10 lines: moderate window; adjacent blocks will merge via
//     the normal block-merge pipeline, producing a clean single view
//   - >3 blocks → 0 (no enrichment)
func autoEnrichContextLines(blockCount int) int {
	switch {
	case blockCount == 1:
		return 30
	case blockCount >= 2 && blockCount <= autoEnrichThreshold:
		return 10
	default:
		return 0
	}
}

// GrepTool implements the grep tool.
type GrepTool struct {
	approval        *ApprovalManager
	limits          OutputLimits
	rgCaptureLimits ripgrepCaptureLimits
}

// NewGrepTool creates a new GrepTool.
func NewGrepTool(approval *ApprovalManager, limits OutputLimits) *GrepTool {
	return &GrepTool{
		approval:        approval,
		limits:          limits,
		rgCaptureLimits: defaultRipgrepCaptureLimits(),
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

type ripgrepResult struct {
	matches   []GrepMatch
	truncated bool
}

func buildRipgrepArgs(a GrepArgs, searchPath string, contextLines, maxResults int) []string {
	args := []string{
		"--no-config",
		"--color=never",
		"--hidden",
		"--glob", "!.git",
	}

	if a.FilesWithMatches {
		args = append(args, "--files-with-matches")
	} else {
		args = append(args,
			"--json",
			"--max-count", strconv.Itoa(maxResults),
			"--context", strconv.Itoa(contextLines),
		)
	}

	if a.Include != "" {
		args = append(args, "--glob", a.Include)
	}
	if a.Exclude != "" {
		args = append(args, "--glob", "!"+a.Exclude)
	}
	if a.Type != "" {
		args = append(args, "--type", a.Type)
	}
	if a.Multiline {
		args = append(args, "--multiline", "--multiline-dotall")
	}

	args = append(args, a.Pattern, searchPath)
	return args
}

type ripgrepCaptureLimits struct {
	maxOutputLines   int
	maxBufferedBytes int
}

func defaultRipgrepCaptureLimits() ripgrepCaptureLimits {
	return ripgrepCaptureLimits{
		maxOutputLines:   rgHardMaxOutputLines,
		maxBufferedBytes: rgMaxBufferedBytes,
	}
}

func (l ripgrepCaptureLimits) normalize() ripgrepCaptureLimits {
	if l.maxOutputLines <= 0 {
		l.maxOutputLines = rgHardMaxOutputLines
	}
	if l.maxBufferedBytes <= 0 {
		l.maxBufferedBytes = rgMaxBufferedBytes
	}
	return l
}

func runRipgrep(ctx context.Context, args []string, limits ripgrepCaptureLimits) ([]byte, bool, error) {
	limits = limits.normalize()
	cmd := exec.CommandContext(ctx, "rg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}

	stderrCh := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(stderr, 64*1024))
		stderrCh <- data
	}()

	var out bytes.Buffer
	reader := bufio.NewReader(stdout)
	lineCount := 0
	truncated := false

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineCount++
			if lineCount > limits.maxOutputLines || out.Len()+len(line) > limits.maxBufferedBytes {
				truncated = true
				_ = cmd.Process.Kill()
				break
			}
			out.Write(line)
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			_ = cmd.Process.Kill()
			<-stderrCh
			_ = cmd.Wait()
			return nil, false, readErr
		}
	}

	waitErr := cmd.Wait()
	stderrOut := strings.TrimSpace(string(<-stderrCh))

	if waitErr != nil {
		if truncated {
			return out.Bytes(), true, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, false, nil
		}
		if stderrOut != "" {
			return nil, false, fmt.Errorf("ripgrep failed: %s", stderrOut)
		}
		return nil, false, waitErr
	}

	return out.Bytes(), truncated, nil
}

// executeRipgrep runs ripgrep and returns matches.
func (t *GrepTool) executeRipgrep(ctx context.Context, a GrepArgs, searchPath string, contextLines, maxResults int) (ripgrepResult, error) {
	args := buildRipgrepArgs(a, searchPath, contextLines, maxResults)
	output, truncated, err := runRipgrep(ctx, args, t.rgCaptureLimits)
	if err != nil {
		return ripgrepResult{}, err
	}
	if len(output) == 0 {
		return ripgrepResult{}, nil
	}

	// files-with-matches mode: skip JSON parsing entirely, just return filenames.
	if a.FilesWithMatches {
		var matches []GrepMatch
		for _, f := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if f != "" {
				matches = append(matches, GrepMatch{FilePath: f})
			}
		}
		return ripgrepResult{matches: matches, truncated: truncated}, nil
	}

	matches, err := parseRipgrepOutput(output, maxResults, contextLines)
	if err != nil {
		return ripgrepResult{}, err
	}
	return ripgrepResult{matches: matches, truncated: truncated}, nil
}

// pendingMatch tracks context for building ripgrep results.
// before and after carry line numbers so adjacent matches can be merged
// without re-parsing formatted strings.
type pendingMatch struct {
	filePath   string
	lineNumber int
	matchLine  string
	before     []contextEntry
	after      []contextEntry
}

// contextEntry is one line from a ripgrep "context" event.
type contextEntry struct {
	lineNumber int
	text       string
}

// matchBlock is the merged display unit for one contiguous context window.
// A single block may highlight multiple match lines when adjacent matches
// are merged.
type matchBlock struct {
	filePath string
	lines    []blockLine
}

type blockLine struct {
	number  int
	text    string
	isMatch bool
}

// pendingToBlock converts a pendingMatch to a matchBlock.
func pendingToBlock(p *pendingMatch) matchBlock {
	lines := make([]blockLine, 0, len(p.before)+1+len(p.after))
	for _, e := range p.before {
		lines = append(lines, blockLine{e.lineNumber, e.text, false})
	}
	lines = append(lines, blockLine{p.lineNumber, p.matchLine, true})
	for _, e := range p.after {
		lines = append(lines, blockLine{e.lineNumber, e.text, false})
	}
	return matchBlock{p.filePath, lines}
}

// mergeBlocks combines two matchBlocks into one, deduplicating shared lines.
// Lines present in both blocks are shown once; a line that is a match in
// either block is marked as a match in the result.
func mergeBlocks(a, b matchBlock) matchBlock {
	byNum := make(map[int]int, len(a.lines)+len(b.lines)) // lineNum → slice index
	merged := make([]blockLine, 0, len(a.lines)+len(b.lines))
	for _, l := range a.lines {
		byNum[l.number] = len(merged)
		merged = append(merged, l)
	}
	for _, l := range b.lines {
		if idx, ok := byNum[l.number]; ok {
			if l.isMatch {
				merged[idx].isMatch = true
			}
		} else {
			byNum[l.number] = len(merged)
			merged = append(merged, l)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].number < merged[j].number
	})
	return matchBlock{a.filePath, merged}
}

// pendingsToBlocks converts raw parsed matches to display blocks, merging
// adjacent matches whose context windows overlap or are contiguous.
func pendingsToBlocks(pending []pendingMatch) []matchBlock {
	var blocks []matchBlock
	for i := range pending {
		b := pendingToBlock(&pending[i])
		if len(blocks) > 0 && blocks[len(blocks)-1].filePath == b.filePath {
			last := blocks[len(blocks)-1]
			lastLine := last.lines[len(last.lines)-1].number
			firstLine := b.lines[0].number
			if firstLine <= lastLine+1 {
				blocks[len(blocks)-1] = mergeBlocks(last, b)
				continue
			}
		}
		blocks = append(blocks, b)
	}
	return blocks
}

// blockToGrepMatch converts a matchBlock to a GrepMatch for external use.
func blockToGrepMatch(b matchBlock) GrepMatch {
	var sb strings.Builder
	for _, l := range b.lines {
		prefix := "  "
		if l.isMatch {
			prefix = "> "
		}
		sb.WriteString(fmt.Sprintf("%s%d: %s\n", prefix, l.number, truncateLine(l.text)))
	}
	// LineNumber and Match refer to the first highlighted line.
	var firstNum int
	var firstText string
	for _, l := range b.lines {
		if l.isMatch {
			firstNum = l.number
			firstText = l.text
			break
		}
	}
	return GrepMatch{
		FilePath:   b.filePath,
		LineNumber: firstNum,
		Match:      firstText,
		Context:    strings.TrimSuffix(sb.String(), "\n"),
	}
}

// parseToPending parses ripgrep JSON output into raw pendingMatches.
//
// # Stream ordering
//
// rg --json emits events in this order per match group:
//
//	begin → context(before) → match → context(after) → end
//
// Before-context lines arrive *before* the match event, so a naive parser
// that creates pendingMatch on "match" will always drop them.
//
// # Before-context: look-back buffer
//
// We maintain a rolling contextBuf of recent context entries.  When a "match"
// event fires we pull any entries with lineNumber < matchLine as before-context
// and reset the buffer.  This correctly handles:
//   - First match in a group (before lines arrive while pending == nil)
//   - Far-apart matches in separate groups (before lines arrive after "end" flush)
//   - Close matches: after-cap overflow lines buffer for the next match's before
//
// # After-context: capped slice
//
// After-context lines go directly into pending.after, capped at maxAfterContext.
// Lines beyond the cap are buffered (not discarded) so they are available as
// before-context for the next match.
func parseToPending(output []byte, maxResults, maxAfterContext int) ([]pendingMatch, error) {
	var pending *pendingMatch
	var result []pendingMatch
	var contextBuf []contextEntry // look-back buffer for before-context

	bufAppend := func(e contextEntry) {
		contextBuf = append(contextBuf, e)
		if len(contextBuf) > maxAfterContext {
			contextBuf = contextBuf[len(contextBuf)-maxAfterContext:]
		}
	}

	flush := func() bool {
		if pending == nil {
			return false
		}
		result = append(result, *pending)
		pending = nil
		return len(result) >= maxResults
	}

	// Walk the raw buffer line-by-line instead of strings.Split(string(output), "\n").
	// This avoids copying/splitting the entire ripgrep stream up front and lets
	// maxResults stop parsing after the useful prefix.
	for len(output) > 0 {
		line := output
		if i := bytes.IndexByte(output, '\n'); i >= 0 {
			line = output[:i]
			output = output[i+1:]
		} else {
			output = nil
		}
		if len(line) == 0 {
			continue
		}

		var msg rgMatch
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "match":
			if flush() {
				return result, nil
			}

			var data rgMatchData
			if err := json.Unmarshal(msg.Data, &data); err != nil {
				continue
			}

			// Collect before-context from the look-back buffer.
			var before []contextEntry
			for _, e := range contextBuf {
				if e.lineNumber < data.LineNumber {
					before = append(before, e)
				}
			}

			pending = &pendingMatch{
				filePath:   data.Path.Text,
				lineNumber: data.LineNumber,
				matchLine:  strings.TrimSuffix(data.Lines.Text, "\n"),
				before:     before,
			}
			contextBuf = nil // reset; subsequent context lines are after-context

		case "context":
			var data rgMatchData
			if err := json.Unmarshal(msg.Data, &data); err != nil {
				continue
			}

			text := strings.TrimSuffix(data.Lines.Text, "\n")
			if pending != nil && data.LineNumber > pending.lineNumber && len(pending.after) < maxAfterContext {
				// Normal after-context for the current match.
				pending.after = append(pending.after, contextEntry{data.LineNumber, text})
			} else {
				// Everything else goes into the look-back buffer:
				//  - lines before the first match in a group (pending == nil)
				//  - lines after "end" / before the next group's first match
				//  - after-cap overflow that belongs to the next match's before
				bufAppend(contextEntry{data.LineNumber, text})
			}

		case "end":
			// rg emits "end" at the close of each match group.  Flush so the
			// last match in a group gets its after-context before the next
			// group's before-context starts arriving.
			if flush() {
				return result, nil
			}
			contextBuf = nil // each group starts with a clean buffer
		}
	}

	// Flush any remaining match not terminated by an "end" event.
	flush()

	return result, nil
}

// parseRipgrepOutput parses ripgrep JSON output into GrepMatches.
// It runs the full pipeline: raw parse → merge adjacent blocks → format.
func parseRipgrepOutput(output []byte, maxResults, maxAfterContext int) ([]GrepMatch, error) {
	pending, err := parseToPending(output, maxResults, maxAfterContext)
	if err != nil {
		return nil, err
	}
	blocks := pendingsToBlocks(pending)
	matches := make([]GrepMatch, len(blocks))
	for i, b := range blocks {
		matches[i] = blockToGrepMatch(b)
	}
	return matches, nil
}

// enrichGrepMatchesFromFiles expands small ripgrep result sets by reading only
// the matched line windows from the files already found by the first rg pass.
// This preserves auto-enriched context without spawning a second full-tree rg.
func enrichGrepMatchesFromFiles(ctx context.Context, matches []GrepMatch, contextLines int) ([]GrepMatch, error) {
	if len(matches) == 0 || contextLines <= 0 {
		return matches, nil
	}

	groups := groupMatchesByFile(matches)
	enriched := make([]GrepMatch, 0, len(matches))
	for _, g := range groups {
		blocks, err := enrichFileGrepGroup(ctx, g, contextLines)
		if err != nil {
			return nil, err
		}
		if len(blocks) == 0 {
			enriched = append(enriched, g.matches...)
			continue
		}
		for _, b := range blocks {
			enriched = append(enriched, blockToGrepMatch(b))
		}
	}
	return enriched, nil
}

type lineWindow struct {
	start int
	end   int
}

func windowAroundLine(line, contextLines int) lineWindow {
	start := line - contextLines
	if start < 1 {
		start = 1
	}
	return lineWindow{start: start, end: line + contextLines}
}

func enrichFileGrepGroup(ctx context.Context, group fileGroup, contextLines int) ([]matchBlock, error) {
	matchLines := make(map[int]struct{}, len(group.matches))
	var centers []int
	for _, m := range group.matches {
		for _, line := range grepMatchLineNumbers(m) {
			if line <= 0 {
				continue
			}
			if _, seen := matchLines[line]; !seen {
				centers = append(centers, line)
				matchLines[line] = struct{}{}
			}
		}
	}
	if len(centers) == 0 {
		return nil, nil
	}
	sort.Ints(centers)

	windows := make([]lineWindow, len(centers))
	for i, line := range centers {
		windows[i] = windowAroundLine(line, contextLines)
	}

	lines, err := readLineWindows(ctx, group.path, windows)
	if err != nil {
		return nil, err
	}

	blocks := make([]matchBlock, 0, len(centers))
	for _, center := range centers {
		w := windowAroundLine(center, contextLines)
		block := matchBlock{filePath: group.path}
		for line := w.start; line <= w.end; line++ {
			text, ok := lines[line]
			if !ok {
				continue
			}
			_, isMatch := matchLines[line]
			block.lines = append(block.lines, blockLine{number: line, text: text, isMatch: isMatch})
		}
		if len(block.lines) > 0 {
			blocks = append(blocks, block)
		}
	}

	merged := make([]matchBlock, 0, len(blocks))
	for _, b := range blocks {
		if len(merged) > 0 {
			last := merged[len(merged)-1]
			lastLine := last.lines[len(last.lines)-1].number
			firstLine := b.lines[0].number
			if firstLine <= lastLine+1 {
				merged[len(merged)-1] = mergeBlocks(last, b)
				continue
			}
		}
		merged = append(merged, b)
	}
	return merged, nil
}

func grepMatchLineNumbers(m GrepMatch) []int {
	seen := make(map[int]struct{})
	add := func(line int) {
		if line > 0 {
			seen[line] = struct{}{}
		}
	}
	for _, line := range strings.Split(m.Context, "\n") {
		if !strings.HasPrefix(line, "> ") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "> "))
		idx := strings.IndexByte(rest, ':')
		if idx <= 0 {
			continue
		}
		lineNum, err := strconv.Atoi(strings.TrimSpace(rest[:idx]))
		if err == nil {
			add(lineNum)
		}
	}
	if len(seen) == 0 {
		add(m.LineNumber)
	}

	lines := make([]int, 0, len(seen))
	for line := range seen {
		lines = append(lines, line)
	}
	sort.Ints(lines)
	return lines
}

func readLineWindows(ctx context.Context, path string, windows []lineWindow) (map[int]string, error) {
	if len(windows) == 0 {
		return nil, nil
	}

	minStart := windows[0].start
	maxEnd := windows[0].end
	for _, w := range windows[1:] {
		if w.start < minStart {
			minStart = w.start
		}
		if w.end > maxEnd {
			maxEnd = w.end
		}
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	lines := make(map[int]string)
	reader := bufio.NewReader(file)
	lineNum := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			lineNum++
			if lineNum >= minStart && lineInWindows(lineNum, windows) {
				line = strings.TrimSuffix(line, "\n")
				lines[lineNum] = line
			}
			if lineNum >= maxEnd {
				break
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readErr
		}
	}

	return lines, nil
}

func lineInWindows(line int, windows []lineWindow) bool {
	for _, w := range windows {
		if line >= w.start && line <= w.end {
			return true
		}
	}
	return false
}

// truncateLine trims leading/trailing whitespace and caps the line at
// maxLineDisplayLen runes, appending "…" when truncated.  Leading whitespace
// is preserved up to the cap so indentation remains meaningful.
func truncateLine(s string) string {
	s = strings.TrimRight(s, " \t")
	r := []rune(s)
	if len(r) <= maxLineDisplayLen {
		return s
	}
	return string(r[:maxLineDisplayLen-1]) + "…"
}

// GrepArgs are the arguments for grep.
type GrepArgs struct {
	Pattern          string `json:"pattern"`
	Path             string `json:"path,omitempty"`
	Include          string `json:"include,omitempty"` // glob filter e.g., "*.go"
	Exclude          string `json:"exclude,omitempty"` // glob pattern to exclude e.g., "vendor/**"
	Type             string `json:"type,omitempty"`    // rg --type filter, e.g. "go"
	MaxResults       int    `json:"max_results,omitempty"`
	ContextLines     int    `json:"context_lines,omitempty"`      // lines of context around match (default 2)
	FilesWithMatches bool   `json:"files_with_matches,omitempty"` // return filenames only
	Multiline        bool   `json:"multiline,omitempty"`          // allow matches to span line boundaries
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
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Ripgrep file type filter, e.g. 'go', 'ts', or 'rb'",
				},
				"context_lines": map[string]interface{}{
					"type":        "integer",
					"description": "Lines of context around each match (default: 2)",
					"default":     2,
				}, "files_with_matches": map[string]interface{}{
					"type":        "boolean",
					"description": "Return only filenames containing matches, not the match lines (default: false)",
				},
				"multiline": map[string]interface{}{
					"type":        "boolean",
					"description": "Allow regex matches to span line boundaries (default: false)",
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
	if a.Type != "" {
		result += " type:" + a.Type
	}
	if a.Exclude != "" {
		result += " exclude:" + a.Exclude
	}
	if a.Multiline {
		result += " multiline"
	}
	return result
}

func (t *GrepTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	warning := WarnUnknownParams(args, []string{
		"pattern", "path", "include", "exclude", "type",
		"max_results", "context_lines", "files_with_matches", "multiline",
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
		contextLines = 2
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

	resolvedSearchPath, err := resolveToolPath(searchPath, false)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return textOutput(formatToolError(toolErr)), nil
		}
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot resolve path: %v", err))), nil
	}

	// Try ripgrep first (faster)
	if ripgrepAvailable() {
		result, err := t.executeRipgrep(ctx, a, resolvedSearchPath, contextLines, maxResults)
		if err != nil {
			if ctx.Err() != nil {
				return textOutput("grep timed out after 1 minute; try a more specific pattern or path"), nil
			}
			// Fall through to Go implementation on ripgrep error
		} else {
			matches := result.matches
			if len(matches) == 0 {
				return textOutput("No matches found."), nil
			}
			if a.FilesWithMatches {
				return textOutput(formatFilesWithMatches(matches, result.truncated)), nil
			}
			// Auto-enrich: when the result set is small and the caller didn't
			// request explicit context, bump the context window so the model can
			// understand the match without an extra read_file round-trip.  The
			// initial ripgrep pass has already found the matched files and line
			// numbers, so enrich directly from those files instead of spawning and
			// scanning a second full-tree ripgrep process.
			if a.ContextLines <= 0 {
				if enriched := autoEnrichContextLines(len(matches)); enriched > contextLines {
					if a.Multiline {
						// Multiline matches can span several physical lines in a
						// single rg event, so keep rg's own context expansion there.
						if richer, err2 := t.executeRipgrep(ctx, a, resolvedSearchPath, enriched, maxResults); err2 == nil && len(richer.matches) > 0 {
							matches = richer.matches
							result.truncated = richer.truncated
						}
					} else if richer, err2 := enrichGrepMatchesFromFiles(ctx, matches, enriched); err2 == nil && len(richer) > 0 {
						matches = richer
					}
				}
			}
			// Sort so the most recently modified files appear first.
			matches = sortGrepMatchesByMtime(matches)
			return textOutput(formatGrepResults(matches, result.truncated || len(matches) >= maxResults)), nil
		}
	}

	// Fallback: Go implementation
	// Compile regex
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "invalid regex pattern: %v", err))), nil
	}

	// Collect files to search
	files, err := collectFiles(resolvedSearchPath, a.Include, a.Exclude)
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
		return textOutput(formatFilesWithMatches(matches, len(matches) >= maxResults)), nil
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

// sortGrepMatchesByMtime reorders matches so that matches from the
// most-recently-modified file appear first.  Within a file the original
// line order is preserved.  Files that cannot be stat'd are sorted last.
func sortGrepMatchesByMtime(matches []GrepMatch) []GrepMatch {
	groups := groupMatchesByFile(matches)

	type mtimeGroup struct {
		fg    fileGroup
		mtime int64
	}
	mg := make([]mtimeGroup, len(groups))
	for i, g := range groups {
		info, err := os.Stat(g.path)
		if err != nil {
			mg[i] = mtimeGroup{fg: g, mtime: 0}
		} else {
			mg[i] = mtimeGroup{fg: g, mtime: info.ModTime().Unix()}
		}
	}

	sort.SliceStable(mg, func(i, j int) bool {
		return mg[i].mtime > mg[j].mtime
	})

	out := make([]GrepMatch, 0, len(matches))
	for _, m := range mg {
		out = append(out, m.fg.matches...)
	}
	return out
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
		sb.WriteString(fmt.Sprintf("%s%d: %s\n", prefix, i+1, truncateLine(lines[i])))
	}

	return strings.TrimSuffix(sb.String(), "\n")
}

// formatFilesWithMatches formats a deduplicated list of filenames containing matches.
func formatFilesWithMatches(matches []GrepMatch, truncated bool) string {
	var sb strings.Builder
	seen := make(map[string]bool)
	for _, m := range matches {
		if !seen[m.FilePath] {
			seen[m.FilePath] = true
			sb.WriteString(m.FilePath + "\n")
		}
	}
	if truncated {
		sb.WriteString("[Results truncated at limit]\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// fileGroup holds the ordered matches for a single file.
type fileGroup struct {
	path    string
	matches []GrepMatch
}

// groupMatchesByFile groups matches by file path, preserving encounter order.
func groupMatchesByFile(matches []GrepMatch) []fileGroup {
	var groups []fileGroup
	idx := make(map[string]int, len(matches))
	for _, m := range matches {
		if i, ok := idx[m.FilePath]; ok {
			groups[i].matches = append(groups[i].matches, m)
		} else {
			idx[m.FilePath] = len(groups)
			groups = append(groups, fileGroup{path: m.FilePath, matches: []GrepMatch{m}})
		}
	}
	return groups
}

// formatGrepResults formats grep results grouped by file for the LLM.
//
// Output shape:
//
//	N matches in M files
//
//	path/to/file.go (K matches):
//	  3: context line
//	> 4: matching line
//	  5: context line
//
//	path/to/other.go (1 match):
//	...
func formatGrepResults(matches []GrepMatch, truncated bool) string {
	if len(matches) == 0 {
		return ""
	}

	groups := groupMatchesByFile(matches)

	pluralOf := map[string]string{
		"match": "matches",
		"file":  "files",
	}
	plural := func(n int, word string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, word)
		}
		if p, ok := pluralOf[word]; ok {
			return fmt.Sprintf("%d %s", n, p)
		}
		return fmt.Sprintf("%d %ss", n, word)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s in %s\n", plural(len(matches), "match"), plural(len(groups), "file")))

	bytesWritten := sb.Len()
	bytesCapped := false

	for gi, g := range groups {
		// Build this file's block into a temporary buffer so we can measure
		// it before committing to the main output.
		var block strings.Builder
		block.WriteString(fmt.Sprintf("\n%s (%s):\n", g.path, plural(len(g.matches), "match")))

		display := g.matches
		overflow := 0
		if len(g.matches) > maxMatchesPerFileDisplay {
			display = g.matches[:maxMatchesPerFileDisplay]
			overflow = len(g.matches) - maxMatchesPerFileDisplay
		}

		for i, m := range display {
			if i > 0 {
				block.WriteString("  …\n")
			}
			block.WriteString(m.Context)
			block.WriteString("\n")
		}

		if overflow > 0 {
			block.WriteString(fmt.Sprintf("\n  [+%d more — narrow your search]\n", overflow))
		}

		// Enforce the total byte budget.  Stop before writing this block if it
		// would push us over; always write at least the first group so the
		// caller gets something useful.
		if gi > 0 && bytesWritten+block.Len() > maxOutputBytes {
			remaining := len(groups) - gi
			sb.WriteString(fmt.Sprintf("\n[output capped at 50KB — %s not shown; narrow your search]\n",
				plural(remaining, "file")))
			bytesCapped = true
			break
		}

		sb.WriteString(block.String())
		bytesWritten += block.Len()
	}

	if truncated && !bytesCapped {
		sb.WriteString("\n[Results truncated at limit]")
	}

	return sb.String()
}
