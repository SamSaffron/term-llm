package tools

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestFinishGrepRankedNoteCountsRenderedBlocks verifies the "showing N" note
// reports the blocks actually rendered when huge blocks hit the byte cap.
func TestFinishGrepRankedNoteCountsRenderedBlocks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := newIntentTool(t, newScoringFake(), nil)
	huge := "> 1: score=0.9 " + strings.Repeat("x", 12*1024)
	m := make([]GrepMatch, 10)
	for i := range m {
		m[i] = GrepMatch{FilePath: filepath.Join(root, fmt.Sprintf("f%d.go", i)), LineNumber: 1, Context: huge}
	}
	out := finish(t, tool, root, m, 100, false)
	if len(out) > maxOutputBytes {
		t.Fatalf("output %d exceeds %d", len(out), maxOutputBytes)
	}
	if !strings.HasSuffix(out, "[output capped at 50KB — narrow your search]\n") {
		t.Fatal("missing cap notice")
	}
	match := regexp.MustCompile(`showing (\d+) of`).FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("note missing: %.200s", out)
	}
	shown, _ := strconv.Atoi(match[1])
	if got := strings.Count(out, ":\n> 1: score=0.9"); got != shown || shown == 0 || shown >= len(m) {
		t.Fatalf("note says %d, rendered %d", shown, got)
	}
}
