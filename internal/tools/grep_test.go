package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepTool_UsesFilePath(t *testing.T) {
	dir := t.TempDir()
	token := "unique_grep_token_1234567890"
	filePath := filepath.Join(dir, "sample.txt")

	if err := os.WriteFile(filePath, []byte("before "+token+" after"), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	tool := NewGrepTool(nil, DefaultOutputLimits())
	args, err := json.Marshal(GrepArgs{
		Pattern:          token,
		Path:             dir,
		FilesWithMatches: true,
	})
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(output.Content, filePath) {
		t.Fatalf("expected output to contain %q, got: %s", filePath, output.Content)
	}
}

func TestFormatGrepResults_SingleFile(t *testing.T) {
	matches := []GrepMatch{
		{FilePath: "pkg/foo.go", LineNumber: 10, Match: "func Foo()", Context: "  9: // Foo does things\n> 10: func Foo()\n  11: {"},
		{FilePath: "pkg/foo.go", LineNumber: 20, Match: "func Bar()", Context: "> 20: func Bar()"},
	}

	result := formatGrepResults(matches, false)

	// Summary line
	if !strings.Contains(result, "2 matches in 1 file") {
		t.Errorf("expected summary '2 matches in 1 file', got:\n%s", result)
	}

	// File header appears once
	count := strings.Count(result, "pkg/foo.go")
	if count != 1 {
		t.Errorf("expected file path to appear once, got %d times", count)
	}

	// File header shows match count
	if !strings.Contains(result, "pkg/foo.go (2 matches):") {
		t.Errorf("expected file header with count, got:\n%s", result)
	}

	// Context present
	if !strings.Contains(result, "func Foo()") || !strings.Contains(result, "func Bar()") {
		t.Errorf("expected both match lines in output, got:\n%s", result)
	}
}

func TestFormatGrepResults_MultipleFiles(t *testing.T) {
	matches := []GrepMatch{
		{FilePath: "a/one.go", LineNumber: 1, Match: "match1", Context: "> 1: match1"},
		{FilePath: "b/two.go", LineNumber: 5, Match: "match2", Context: "> 5: match2"},
		{FilePath: "b/two.go", LineNumber: 9, Match: "match3", Context: "> 9: match3"},
	}

	result := formatGrepResults(matches, false)

	if !strings.Contains(result, "3 matches in 2 files") {
		t.Errorf("expected summary '3 matches in 2 files', got:\n%s", result)
	}

	if !strings.Contains(result, "a/one.go (1 match):") {
		t.Errorf("expected singular 'match' for single result, got:\n%s", result)
	}

	if !strings.Contains(result, "b/two.go (2 matches):") {
		t.Errorf("expected '2 matches' for two.go, got:\n%s", result)
	}

	// File path for b/two.go appears exactly once (as header, not per-match)
	count := strings.Count(result, "b/two.go")
	if count != 1 {
		t.Errorf("expected b/two.go to appear once, got %d times", count)
	}
}

func TestFormatGrepResults_Truncated(t *testing.T) {
	matches := []GrepMatch{
		{FilePath: "x.go", LineNumber: 1, Match: "foo", Context: "> 1: foo"},
	}

	result := formatGrepResults(matches, true)

	if !strings.Contains(result, "[Results truncated at limit]") {
		t.Errorf("expected truncation notice, got:\n%s", result)
	}
}

func TestFormatGrepResults_Empty(t *testing.T) {
	result := formatGrepResults(nil, false)
	if result != "" {
		t.Errorf("expected empty string for nil matches, got %q", result)
	}
}

func TestGroupMatchesByFile_PreservesOrder(t *testing.T) {
	matches := []GrepMatch{
		{FilePath: "c.go", LineNumber: 1},
		{FilePath: "a.go", LineNumber: 1},
		{FilePath: "c.go", LineNumber: 2},
		{FilePath: "b.go", LineNumber: 1},
		{FilePath: "a.go", LineNumber: 2},
	}

	groups := groupMatchesByFile(matches)

	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
	// Encounter order: c.go first, then a.go, then b.go
	if groups[0].path != "c.go" || groups[1].path != "a.go" || groups[2].path != "b.go" {
		t.Errorf("unexpected group order: %v, %v, %v", groups[0].path, groups[1].path, groups[2].path)
	}
	if len(groups[0].matches) != 2 || len(groups[1].matches) != 2 || len(groups[2].matches) != 1 {
		t.Errorf("unexpected match counts: %d, %d, %d", len(groups[0].matches), len(groups[1].matches), len(groups[2].matches))
	}
}

// TestParseRipgrepOutput_NoContextBleed verifies that after-context is capped
// at maxAfterContext lines.
//
// The bleed bug: rg emits before-context lines for match B as "context"
// events while match A is still pending (because those lines have higher line
// numbers than A, they went into A's after-slice without bound).  Example:
//
//	match@41  → pending
//	context@43,44      → correct after for 41
//	context@337,338    → before-context for match@339, but lineNumber>41
//	                     so without a cap they bleed into 41's after-slice
//	match@339 → flush 41 (would show 4 after-lines instead of 2)
//
// With maxAfterContext=2, context@337,338 are dropped and 41 flushes cleanly.
func TestParseRipgrepOutput_NoContextBleed(t *testing.T) {
	makeContext := func(path string, lineNum int, text string) string {
		return fmt.Sprintf(`{"type":"context","data":{"path":{"text":%q},"lines":{"text":%q},"line_number":%d,"absolute_offset":0,"submatches":[]}}`,
			path, text+"\n", lineNum)
	}
	makeMatch := func(path string, lineNum int, text string) string {
		return fmt.Sprintf(`{"type":"match","data":{"path":{"text":%q},"lines":{"text":%q},"line_number":%d,"absolute_offset":0,"submatches":[]}}`,
			path, text+"\n", lineNum)
	}
	makeEnd := func() string {
		return `{"type":"end","data":{"path":{"text":"f.go"},"binary_offset":null,"stats":{"elapsed":{"secs":0,"nanos":0},"searches":1,"searches_with_match":1,"bytes_searched":0,"bytes_printed":0,"matched_lines":2,"matches":2}}}`
	}

	// Matches at lines 41 and 339, context=2.
	// rg stream order (observed from real rg --json --context 2 output):
	//   match@41
	//   context@43,44          (after-context for 41)
	//   context@337,338        (before-context for 339, arrives while 41 is pending)
	//   match@339
	//   context@340,341        (after-context for 339)
	//   end
	lines := []string{
		makeMatch("f.go", 41, "matchA"),
		makeContext("f.go", 43, "after43"),
		makeContext("f.go", 44, "after44"),
		// These are before-context for matchB but arrive while matchA is pending.
		// Without the cap they bleed into matchA's after-slice.
		makeContext("f.go", 337, "before337"),
		makeContext("f.go", 338, "before338"),
		makeMatch("f.go", 339, "matchB"),
		makeContext("f.go", 340, "after340"),
		makeContext("f.go", 341, "after341"),
		makeEnd(),
	}
	input := []byte(strings.Join(lines, "\n"))

	matches, err := parseRipgrepOutput(input, 100, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}

	// matchA: after should be exactly [43,44] — no bleed from 337,338.
	ctxA := matches[0].Context
	if strings.Contains(ctxA, "before337") || strings.Contains(ctxA, "before338") {
		t.Errorf("context bleed: matchA context contains matchB's before-context:\n%s", ctxA)
	}
	if !strings.Contains(ctxA, "after43") || !strings.Contains(ctxA, "after44") {
		t.Errorf("matchA missing correct after-context:\n%s", ctxA)
	}

	// matchB: after should be [340,341].
	ctxB := matches[1].Context
	if !strings.Contains(ctxB, "after340") || !strings.Contains(ctxB, "after341") {
		t.Errorf("matchB missing after-context:\n%s", ctxB)
	}
}

// TestParseRipgrepOutput_EndEventFlush verifies that an "end" event flushes
// the pending match, so the last match in each group is correctly emitted.
func TestParseRipgrepOutput_EndEventFlush(t *testing.T) {
	makeMatch := func(lineNum int, text string) string {
		return fmt.Sprintf(`{"type":"match","data":{"path":{"text":"f.go"},"lines":{"text":%q},"line_number":%d,"absolute_offset":0,"submatches":[]}}`,
			text+"\n", lineNum)
	}
	makeEnd := func() string {
		return `{"type":"end","data":{"path":{"text":"f.go"},"binary_offset":null,"stats":{"elapsed":{"secs":0,"nanos":0},"searches":1,"searches_with_match":1,"bytes_searched":0,"bytes_printed":0,"matched_lines":1,"matches":1}}}`
	}

	lines := []string{
		makeMatch(10, "matchA"),
		makeEnd(),
		makeMatch(200, "matchB"),
		makeEnd(),
	}
	input := []byte(strings.Join(lines, "\n"))

	matches, err := parseRipgrepOutput(input, 100, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if matches[0].Match != "matchA" || matches[1].Match != "matchB" {
		t.Errorf("unexpected match content: %v %v", matches[0].Match, matches[1].Match)
	}
}

func TestTruncateLine(t *testing.T) {
	short := "hello world"
	if truncateLine(short) != short {
		t.Errorf("short line should be unchanged, got %q", truncateLine(short))
	}

	long := strings.Repeat("x", maxLineDisplayLen+10)
	result := truncateLine(long)
	r := []rune(result)
	if len(r) > maxLineDisplayLen {
		t.Errorf("truncated line too long: %d runes (max %d)", len(r), maxLineDisplayLen)
	}
	if !strings.HasSuffix(result, "…") {
		t.Errorf("truncated line should end with ellipsis, got %q", result[len(result)-4:])
	}

	// Trailing whitespace stripped even if short
	withTrail := "foo   "
	if truncateLine(withTrail) != "foo" {
		t.Errorf("trailing whitespace not stripped, got %q", truncateLine(withTrail))
	}
}

func TestFormatGrepResults_PerFileOverflow(t *testing.T) {
	// Build more than maxMatchesPerFileDisplay matches for one file.
	var matches []GrepMatch
	for i := 1; i <= maxMatchesPerFileDisplay+3; i++ {
		matches = append(matches, GrepMatch{
			FilePath:   "big.go",
			LineNumber: i,
			Match:      "match",
			Context:    fmt.Sprintf("> %d: match", i),
		})
	}

	result := formatGrepResults(matches, false)

	// File header should show true total.
	expected := fmt.Sprintf("big.go (%d matches):", maxMatchesPerFileDisplay+3)
	if !strings.Contains(result, expected) {
		t.Errorf("expected header %q in:\n%s", expected, result)
	}

	// Overflow note should be present.
	if !strings.Contains(result, "[+3 more") {
		t.Errorf("expected overflow note '[+3 more' in:\n%s", result)
	}

	// Only maxMatchesPerFileDisplay match lines should appear.
	displayed := strings.Count(result, "> ")
	if displayed != maxMatchesPerFileDisplay {
		t.Errorf("expected %d displayed matches, got %d", maxMatchesPerFileDisplay, displayed)
	}
}

func TestGrepTool_GroupedOutput(t *testing.T) {
	dir := t.TempDir()

	// Write two files, both containing the token
	if err := os.WriteFile(filepath.Join(dir, "alpha.go"), []byte("line1\nmatch_here\nline3"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beta.go"), []byte("other\nmatch_here\nend"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewGrepTool(nil, DefaultOutputLimits())
	args, _ := json.Marshal(GrepArgs{
		Pattern:      "match_here",
		Path:         dir,
		ContextLines: 0,
	})

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	// Should have summary
	if !strings.Contains(output.Content, "2 matches in 2 files") {
		t.Errorf("expected grouped summary, got:\n%s", output.Content)
	}

	// Each file path should appear exactly once
	alphaPath := filepath.Join(dir, "alpha.go")
	betaPath := filepath.Join(dir, "beta.go")
	if strings.Count(output.Content, alphaPath) != 1 {
		t.Errorf("alpha.go should appear once, got:\n%s", output.Content)
	}
	if strings.Count(output.Content, betaPath) != 1 {
		t.Errorf("beta.go should appear once, got:\n%s", output.Content)
	}
}
