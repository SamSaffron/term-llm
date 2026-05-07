package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestBuildRipgrepArgs_AddsDeterministicAndUsefulFlags(t *testing.T) {
	a := GrepArgs{
		Pattern:   "foo\\nbar",
		Include:   "*.go",
		Exclude:   "vendor/**",
		Type:      "go",
		Multiline: true,
	}

	args := buildRipgrepArgs(a, "/repo", 2, 100)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--no-config",
		"--color=never",
		"--hidden",
		"--json",
		"--max-count 100",
		"--context 2",
		"--glob !.git",
		"--glob *.go",
		"--glob !vendor/**",
		"--type go",
		"--multiline",
		"--multiline-dotall",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected args to contain %q, got: %s", want, joined)
		}
	}
}

func TestBuildRipgrepArgs_FilesWithMatchesOmitsJSONFlags(t *testing.T) {
	a := GrepArgs{Pattern: "needle", FilesWithMatches: true}
	args := buildRipgrepArgs(a, "/repo", 2, 100)
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--files-with-matches") {
		t.Fatalf("expected files-with-matches flag, got: %s", joined)
	}
	for _, unwanted := range []string{"--json", "--max-count", "--context"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("did not expect %q in args: %s", unwanted, joined)
		}
	}
}

func TestFormatFilesWithMatches_Truncated(t *testing.T) {
	matches := []GrepMatch{{FilePath: "a.go"}, {FilePath: "a.go"}, {FilePath: "b.go"}}
	result := formatFilesWithMatches(matches, true)

	if strings.Count(result, "a.go") != 1 || strings.Count(result, "b.go") != 1 {
		t.Fatalf("expected deduped file list, got:\n%s", result)
	}
	if !strings.Contains(result, "[Results truncated at limit]") {
		t.Fatalf("expected truncation notice, got:\n%s", result)
	}
}

func TestFormatGrepResults_Truncated(t *testing.T) {
	matches := []GrepMatch{{FilePath: "x.go", LineNumber: 1, Match: "foo", Context: "> 1: foo"}}
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

// TestPendingsToBlocks_MergeAdjacent verifies that two matches on consecutive
// lines (e.g. lines 41 and 42) are merged into a single block with two ">"
// markers rather than displayed as two separate overlapping context windows.
func TestPendingsToBlocks_MergeAdjacent(t *testing.T) {
	pending := []pendingMatch{
		{
			filePath:   "f.go",
			lineNumber: 41,
			matchLine:  "matchA",
			before:     []contextEntry{{39, "ctx39"}, {40, "ctx40"}},
			after:      []contextEntry{},
		},
		{
			filePath:   "f.go",
			lineNumber: 42,
			matchLine:  "matchB",
			before:     []contextEntry{},
			after:      []contextEntry{{43, "ctx43"}, {44, "ctx44"}},
		},
	}

	blocks := pendingsToBlocks(pending)

	if len(blocks) != 1 {
		t.Fatalf("expected 1 merged block, got %d", len(blocks))
	}

	b := blocks[0]
	// Should have: 39(ctx), 40(ctx), 41(match), 42(match), 43(ctx), 44(ctx)
	if len(b.lines) != 6 {
		t.Errorf("expected 6 lines in merged block, got %d: %+v", len(b.lines), b.lines)
	}

	matchCount := 0
	for _, l := range b.lines {
		if l.isMatch {
			matchCount++
		}
	}
	if matchCount != 2 {
		t.Errorf("expected 2 match lines in merged block, got %d", matchCount)
	}

	m := blockToGrepMatch(b)
	if !strings.Contains(m.Context, "> 41:") || !strings.Contains(m.Context, "> 42:") {
		t.Errorf("merged context missing match markers:\n%s", m.Context)
	}
	// No duplicate context lines (39,40 should appear once each)
	if strings.Count(m.Context, "ctx39") != 1 || strings.Count(m.Context, "ctx40") != 1 {
		t.Errorf("context lines duplicated in merged block:\n%s", m.Context)
	}
}

// TestPendingsToBlocks_NoMergeFarApart verifies that matches with a large gap
// between them are kept as separate blocks.
func TestPendingsToBlocks_NoMergeFarApart(t *testing.T) {
	pending := []pendingMatch{
		{
			filePath:   "f.go",
			lineNumber: 41,
			matchLine:  "matchA",
			before:     []contextEntry{{39, "ctx39"}, {40, "ctx40"}},
			after:      []contextEntry{{43, "ctx43"}, {44, "ctx44"}},
		},
		{
			filePath:   "f.go",
			lineNumber: 339,
			matchLine:  "matchB",
			before:     []contextEntry{{337, "ctx337"}, {338, "ctx338"}},
			after:      []contextEntry{{340, "ctx340"}, {341, "ctx341"}},
		},
	}

	blocks := pendingsToBlocks(pending)

	if len(blocks) != 2 {
		t.Fatalf("expected 2 separate blocks, got %d", len(blocks))
	}
	if blocks[0].lines[0].number != 39 {
		t.Errorf("block 0 should start at line 39, got %d", blocks[0].lines[0].number)
	}
	if blocks[1].lines[0].number != 337 {
		t.Errorf("block 1 should start at line 337, got %d", blocks[1].lines[0].number)
	}
}

// TestPendingsToBlocks_DifferentFiles verifies that matches in different files
// are never merged even if line numbers overlap.
func TestPendingsToBlocks_DifferentFiles(t *testing.T) {
	pending := []pendingMatch{
		{filePath: "a.go", lineNumber: 10, matchLine: "matchA"},
		{filePath: "b.go", lineNumber: 10, matchLine: "matchB"},
	}

	blocks := pendingsToBlocks(pending)

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks for different files, got %d", len(blocks))
	}
	if blocks[0].filePath != "a.go" || blocks[1].filePath != "b.go" {
		t.Errorf("unexpected file order: %s, %s", blocks[0].filePath, blocks[1].filePath)
	}
}

// TestFormatGrepResults_EllipsisSeparator verifies that non-adjacent blocks
// within the same file are separated by "  …" rather than a blank line.
func TestFormatGrepResults_EllipsisSeparator(t *testing.T) {
	matches := []GrepMatch{
		{FilePath: "f.go", LineNumber: 10, Match: "matchA", Context: "> 10: matchA"},
		{FilePath: "f.go", LineNumber: 300, Match: "matchB", Context: "> 300: matchB"},
	}

	result := formatGrepResults(matches, false)

	if !strings.Contains(result, "  …") {
		t.Errorf("expected ellipsis separator between non-adjacent blocks:\n%s", result)
	}
}

// TestParseRipgrepOutput_MergesAdjacent is an end-to-end test verifying that
// adjacent matches produce a single merged GrepMatch with both ">" lines.
func TestParseRipgrepOutput_MergesAdjacent(t *testing.T) {
	makeMatch := func(lineNum int, text string) string {
		return fmt.Sprintf(`{"type":"match","data":{"path":{"text":"f.go"},"lines":{"text":%q},"line_number":%d,"absolute_offset":0,"submatches":[]}}`,
			text+"\n", lineNum)
	}
	makeContext := func(lineNum int, text string) string {
		return fmt.Sprintf(`{"type":"context","data":{"path":{"text":"f.go"},"lines":{"text":%q},"line_number":%d,"absolute_offset":0,"submatches":[]}}`,
			text+"\n", lineNum)
	}
	makeEnd := func() string {
		return `{"type":"end","data":{"path":{"text":"f.go"},"binary_offset":null,"stats":{"elapsed":{"secs":0,"nanos":0},"searches":1,"searches_with_match":1,"bytes_searched":0,"bytes_printed":0,"matched_lines":2,"matches":2}}}`
	}

	// Two adjacent matches at lines 41 and 42, context=2.
	// rg emits before-context for 41, then match@41, then match@42 (no context
	// between), then after-context for 42, then end.
	lines := []string{
		makeContext(39, "ctx39"),
		makeContext(40, "ctx40"),
		makeMatch(41, "matchA"),
		makeMatch(42, "matchB"),
		makeContext(43, "ctx43"),
		makeContext(44, "ctx44"),
		makeEnd(),
	}
	input := []byte(strings.Join(lines, "\n"))

	matches, err := parseRipgrepOutput(input, 100, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 merged match, got %d", len(matches))
	}

	ctx := matches[0].Context
	if !strings.Contains(ctx, "> 41:") || !strings.Contains(ctx, "> 42:") {
		t.Errorf("merged context missing match markers:\n%s", ctx)
	}
	if strings.Count(ctx, "ctx39") != 1 || strings.Count(ctx, "ctx44") != 1 {
		t.Errorf("context lines should appear exactly once:\n%s", ctx)
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

// TestParseRipgrepOutput_NoContextBleed verifies that:
//   - After-context is capped at maxAfterContext (no bleed into matchA)
//   - Cap-overflow lines are buffered and recovered as matchB's before-context
//
// rg emits before-context for match B while match A is still pending (those
// lines have higher line numbers than A).  Old behaviour: they bled into A's
// after-slice.  New behaviour: cap at maxAfterContext, buffer overflow, hand
// off to B's before on the next "match" event.
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

	// Matches at lines 41 and 339, context=2.  Real rg stream order:
	//   match@41
	//   context@43,44      (after for 41; fills cap)
	//   context@337,338    (before for 339; cap exceeded → buffer)
	//   match@339
	//   context@340,341    (after for 339)
	//   end
	lines := []string{
		makeMatch("f.go", 41, "matchA"),
		makeContext("f.go", 43, "after43"),
		makeContext("f.go", 44, "after44"),
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

	// matchA: after-context must be exactly [43,44] — overflow lines must NOT appear.
	ctxA := matches[0].Context
	if strings.Contains(ctxA, "before337") || strings.Contains(ctxA, "before338") {
		t.Errorf("context bleed: matchA contains matchB's before-context:\n%s", ctxA)
	}
	if !strings.Contains(ctxA, "after43") || !strings.Contains(ctxA, "after44") {
		t.Errorf("matchA missing after-context:\n%s", ctxA)
	}

	// matchB: overflow lines must be recovered as before-context.
	ctxB := matches[1].Context
	if !strings.Contains(ctxB, "before337") || !strings.Contains(ctxB, "before338") {
		t.Errorf("matchB missing before-context recovered from overflow:\n%s", ctxB)
	}
	if !strings.Contains(ctxB, "after340") || !strings.Contains(ctxB, "after341") {
		t.Errorf("matchB missing after-context:\n%s", ctxB)
	}
}

// TestParseRipgrepOutput_BeforeContext verifies that before-context lines are
// correctly captured even though rg emits them before the "match" event.
func TestParseRipgrepOutput_BeforeContext(t *testing.T) {
	makeContext := func(lineNum int, text string) string {
		return fmt.Sprintf(`{"type":"context","data":{"path":{"text":"f.go"},"lines":{"text":%q},"line_number":%d,"absolute_offset":0,"submatches":[]}}`,
			text+"\n", lineNum)
	}
	makeMatch := func(lineNum int, text string) string {
		return fmt.Sprintf(`{"type":"match","data":{"path":{"text":"f.go"},"lines":{"text":%q},"line_number":%d,"absolute_offset":0,"submatches":[]}}`,
			text+"\n", lineNum)
	}
	makeEnd := func() string {
		return `{"type":"end","data":{"path":{"text":"f.go"},"binary_offset":null,"stats":{"elapsed":{"secs":0,"nanos":0},"searches":1,"searches_with_match":1,"bytes_searched":0,"bytes_printed":0,"matched_lines":1,"matches":1}}}`
	}

	// Single match at line 10, context=2.  rg emits:
	//   context@8  (before, arrives before the match event — pending==nil)
	//   context@9  (before)
	//   match@10
	//   context@11 (after)
	//   context@12 (after)
	//   end
	lines := []string{
		makeContext(8, "before8"),
		makeContext(9, "before9"),
		makeMatch(10, "theMatch"),
		makeContext(11, "after11"),
		makeContext(12, "after12"),
		makeEnd(),
	}
	input := []byte(strings.Join(lines, "\n"))

	matches, err := parseRipgrepOutput(input, 100, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	ctx := matches[0].Context
	if !strings.Contains(ctx, "before8") || !strings.Contains(ctx, "before9") {
		t.Errorf("before-context not captured:\n%s", ctx)
	}
	if !strings.Contains(ctx, "after11") || !strings.Contains(ctx, "after12") {
		t.Errorf("after-context missing:\n%s", ctx)
	}
	// Verify ordering: before8 appears before the match line, after11 after.
	iB := strings.Index(ctx, "before8")
	iM := strings.Index(ctx, "theMatch")
	iA := strings.Index(ctx, "after11")
	if !(iB < iM && iM < iA) {
		t.Errorf("context lines out of order: before=%d match=%d after=%d\n%s", iB, iM, iA, ctx)
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

func TestGrepTool_TypeFilter_Integration(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("ripgrep not available")
	}

	dir := t.TempDir()
	token := "TYPE_FILTER_TOKEN"
	if err := os.WriteFile(filepath.Join(dir, "match.go"), []byte("package x\n// "+token+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "match.txt"), []byte(token+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewGrepTool(nil, DefaultOutputLimits())
	args, _ := json.Marshal(GrepArgs{Pattern: token, Path: dir, Type: "go", ContextLines: 0})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.Content, "match.go") {
		t.Fatalf("expected go file in output:\n%s", output.Content)
	}
	if strings.Contains(output.Content, "match.txt") {
		t.Fatalf("did not expect txt file in output:\n%s", output.Content)
	}
}

func TestGrepTool_Multiline_Integration(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("ripgrep not available")
	}

	dir := t.TempDir()
	content := "first line\nSECOND_LINE\nthird line\n"
	if err := os.WriteFile(filepath.Join(dir, "multi.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewGrepTool(nil, DefaultOutputLimits())

	withoutArgs, _ := json.Marshal(GrepArgs{Pattern: "first line\\nSECOND_LINE", Path: dir, ContextLines: 0})
	withoutOutput, err := tool.Execute(context.Background(), withoutArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withoutOutput.Content, "No matches found") {
		t.Fatalf("expected multiline-disabled search to miss, got:\n%s", withoutOutput.Content)
	}

	withArgs, _ := json.Marshal(GrepArgs{Pattern: "first line\\nSECOND_LINE", Path: dir, ContextLines: 0, Multiline: true})
	withOutput, err := tool.Execute(context.Background(), withArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withOutput.Content, "multi.txt") {
		t.Fatalf("expected multiline-enabled search to hit file, got:\n%s", withOutput.Content)
	}
}

func TestGrepTool_RawRipgrepOutputCap_Integration(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("ripgrep not available")
	}

	const testRawLineCap = 128

	dir := t.TempDir()
	path := filepath.Join(dir, "many.txt")
	var content strings.Builder
	for i := 0; i < testRawLineCap+50; i++ {
		content.WriteString("HARD_CAP_TOKEN\n")
	}
	if err := os.WriteFile(path, []byte(content.String()), 0644); err != nil {
		t.Fatal(err)
	}

	// Exercise the same Execute path as production, but inject a small raw-output
	// cap so this fixture proves truncation without forcing rg to emit 10k JSON
	// records on every test run.
	tool := NewGrepTool(nil, DefaultOutputLimits())
	tool.rgCaptureLimits.maxOutputLines = testRawLineCap
	args, _ := json.Marshal(GrepArgs{Pattern: "HARD_CAP_TOKEN", Path: dir, MaxResults: testRawLineCap + 50, ContextLines: 0})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.Content, "[Results truncated at limit]") {
		t.Fatalf("expected raw ripgrep truncation notice, got:\n%s", output.Content)
	}
	highlightedLines := strings.Count(output.Content, "> ")
	if highlightedLines < testRawLineCap/2 {
		t.Fatalf("expected a large truncated match block, got only %d highlighted lines", highlightedLines)
	}
}

// TestAutoEnrichContextLines checks the block-count → context-lines mapping.
func TestAutoEnrichContextLines(t *testing.T) {
	cases := []struct {
		blocks int
		want   int
	}{
		{0, 0},
		{1, 30},
		{2, 10},
		{3, 10},
		{4, 0},
		{100, 0},
	}
	for _, c := range cases {
		got := autoEnrichContextLines(c.blocks)
		if got != c.want {
			t.Errorf("autoEnrichContextLines(%d) = %d, want %d", c.blocks, got, c.want)
		}
	}
}

// TestAutoEnrich_SingleMatch verifies that a single-match result is enriched
// with additional context when the caller does not request explicit context.
func TestAutoEnrich_SingleMatch(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("ripgrep not available")
	}

	dir := t.TempDir()

	// Write a file with padding lines so we can verify context expansion.
	var content strings.Builder
	for i := 1; i <= 40; i++ {
		if i == 20 {
			content.WriteString("UNIQUE_AUTOENRICH_TOKEN\n")
		} else {
			content.WriteString(fmt.Sprintf("padding line %d\n", i))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "target.go"), []byte(content.String()), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewGrepTool(nil, DefaultOutputLimits())

	// With explicit ContextLines=0 (default), auto-enrich should kick in.
	args, _ := json.Marshal(GrepArgs{
		Pattern:      "UNIQUE_AUTOENRICH_TOKEN",
		Path:         dir,
		ContextLines: 0,
	})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	// Auto-enrich bumps to 30 lines for a single block.
	// The file has 39 padding lines + 1 match.  With 30 lines of context
	// the result should contain significantly more than the default 2 lines.
	// We verify by counting context lines (prefixed with "  ") in the output.
	contextLineCount := 0
	for _, line := range strings.Split(output.Content, "\n") {
		if strings.HasPrefix(line, "  ") && strings.Contains(line, "padding line") {
			contextLineCount++
		}
	}
	if contextLineCount < 20 {
		t.Errorf("expected ≥20 context lines with auto-enrich, got %d\noutput:\n%s", contextLineCount, output.Content)
	}

	// Explicit context override must suppress auto-enrichment.
	args2, _ := json.Marshal(GrepArgs{
		Pattern:      "UNIQUE_AUTOENRICH_TOKEN",
		Path:         dir,
		ContextLines: 1,
	})
	output2, err := tool.Execute(context.Background(), args2)
	if err != nil {
		t.Fatal(err)
	}
	contextLineCount2 := 0
	for _, line := range strings.Split(output2.Content, "\n") {
		if strings.HasPrefix(line, "  ") && strings.Contains(line, "padding line") {
			contextLineCount2++
		}
	}
	if contextLineCount2 > 2 {
		t.Errorf("explicit ContextLines=1 should not auto-enrich; got %d context lines\noutput:\n%s", contextLineCount2, output2.Content)
	}
}

func TestEnrichGrepMatchesFromFilesExpandsAndMerges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.go")

	var content strings.Builder
	for i := 1; i <= 50; i++ {
		switch i {
		case 20:
			content.WriteString("first TOKEN\n")
		case 32:
			content.WriteString("second TOKEN\n")
		default:
			content.WriteString(fmt.Sprintf("padding line %d\n", i))
		}
	}
	if err := os.WriteFile(path, []byte(content.String()), 0644); err != nil {
		t.Fatal(err)
	}

	matches := []GrepMatch{
		{FilePath: path, LineNumber: 20, Match: "first TOKEN", Context: "> 20: first TOKEN"},
		{FilePath: path, LineNumber: 32, Match: "second TOKEN", Context: "> 32: second TOKEN"},
	}
	enriched, err := enrichGrepMatchesFromFiles(context.Background(), matches, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(enriched) != 1 {
		t.Fatalf("expected overlapping enriched windows to merge into 1 block, got %d: %+v", len(enriched), enriched)
	}

	ctx := enriched[0].Context
	for _, want := range []string{
		"  10: padding line 10",
		"> 20: first TOKEN",
		"> 32: second TOKEN",
		"  42: padding line 42",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("missing %q in enriched context:\n%s", want, ctx)
		}
	}
}

// TestSortGrepMatchesByMtime verifies that matches from the most recently
// modified file appear first in the output.
func TestSortGrepMatchesByMtime(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("ripgrep not available")
	}

	dir := t.TempDir()

	// Write two files and then force distinct mtimes so ordering is stable.
	older := filepath.Join(dir, "older.go")
	newer := filepath.Join(dir, "newer.go")

	token := "MTIME_SORT_TOKEN"
	if err := os.WriteFile(older, []byte("package x\n// "+token+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("package x\n// "+token+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	if err := os.Chtimes(older, base.Add(-2*time.Hour), base.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, base, base); err != nil {
		t.Fatal(err)
	}

	tool := NewGrepTool(nil, DefaultOutputLimits())
	args, _ := json.Marshal(GrepArgs{Pattern: token, Path: dir, ContextLines: 0})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	newerPos := strings.Index(output.Content, "newer.go")
	olderPos := strings.Index(output.Content, "older.go")
	if newerPos < 0 || olderPos < 0 {
		t.Fatalf("expected both files in output:\n%s", output.Content)
	}
	if newerPos > olderPos {
		t.Errorf("expected newer.go before older.go in output:\n%s", output.Content)
	}
}

// TestFormatGrepResults_ByteCap verifies that output is capped at maxOutputBytes
// and a descriptive note is appended.
func TestFormatGrepResults_ByteCap(t *testing.T) {
	// Each block needs to be large enough to push us over 50KB well within
	// the 50-file set.  Use a ~1500-char context string (simulating many
	// context lines) so the cap fires around file 35.
	bigContext := strings.Repeat("> 1: "+strings.Repeat("x", 110)+"\n", 12) // ~1500 bytes/block

	var matches []GrepMatch
	for i := 0; i < 50; i++ {
		matches = append(matches, GrepMatch{
			FilePath:   fmt.Sprintf("/tmp/file%02d.go", i),
			LineNumber: 1,
			Match:      "x",
			Context:    bigContext,
		})
	}

	result := formatGrepResults(matches, false)

	// Must contain the cap notice.
	if !strings.Contains(result, "output capped at 50KB") {
		t.Errorf("expected byte-cap notice in output (total output %d bytes):\n%.500s", len(result), result)
	}

	// Must not exceed two blocks past the limit.
	if len(result) > maxOutputBytes+2*len(bigContext) {
		t.Errorf("output far exceeds byte cap: got %d bytes", len(result))
	}

	// First file must always be present.
	if !strings.Contains(result, "file00.go") {
		t.Errorf("first file missing from capped output:\n%s", result)
	}
}

func BenchmarkParseRipgrepOutputStopsAtMaxResults(b *testing.B) {
	output := buildSyntheticRipgrepJSON(5000)

	b.ReportAllocs()
	b.SetBytes(int64(len(output)))
	for i := 0; i < b.N; i++ {
		matches, err := parseRipgrepOutput(output, 100, 0)
		if err != nil {
			b.Fatal(err)
		}
		if len(matches) != 100 {
			b.Fatalf("expected 100 matches, got %d", len(matches))
		}
	}
}

func buildSyntheticRipgrepJSON(matchCount int) []byte {
	var sb strings.Builder
	sb.Grow(matchCount * 220)
	for i := 0; i < matchCount; i++ {
		path := fmt.Sprintf("f%05d.go", i)
		line := fmt.Sprintf("needle %05d\n", i)
		fmt.Fprintf(&sb, `{"type":"match","data":{"path":{"text":%q},"lines":{"text":%q},"line_number":1,"absolute_offset":0,"submatches":[]}}`+"\n", path, line)
		fmt.Fprintf(&sb, `{"type":"end","data":{"path":{"text":%q}}}`+"\n", path)
	}
	return []byte(sb.String())
}

func BenchmarkGrepToolAutoEnrichSingleMatchLargeTree(b *testing.B) {
	if !ripgrepAvailable() {
		b.Skip("ripgrep not available")
	}

	dir := b.TempDir()
	const token = "BENCH_AUTOENRICH_TOKEN"
	for i := 0; i < 800; i++ {
		var content strings.Builder
		for line := 1; line <= 80; line++ {
			if i == 799 && line == 40 {
				content.WriteString(token + "\n")
			} else {
				fmt.Fprintf(&content, "file %03d filler line %02d\n", i, line)
			}
		}
		path := filepath.Join(dir, fmt.Sprintf("file%04d.go", i))
		if err := os.WriteFile(path, []byte(content.String()), 0644); err != nil {
			b.Fatal(err)
		}
	}

	tool := NewGrepTool(nil, DefaultOutputLimits())
	args, _ := json.Marshal(GrepArgs{Pattern: token, Path: dir, ContextLines: 0})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			b.Fatalf("Execute returned error: %v", err)
		}
		if !strings.Contains(out.Content, token) || !strings.Contains(out.Content, "filler line 10") {
			b.Fatalf("auto-enriched output missing expected content:\n%s", out.Content)
		}
	}
}
