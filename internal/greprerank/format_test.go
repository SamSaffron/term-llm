package greprerank

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

const maxOutputBytes = 50 * 1024

func rankedTestNote(n int) string {
	return fmt.Sprintf("[showing %d ranked blocks]\n", n)
}

func TestFormatRankedGrepResults_NoteCountsRenderedBlocks(t *testing.T) {
	ctx := "> 1: " + strings.Repeat("x", 9000)
	var matches []Item
	for i := 0; i < 20; i++ {
		matches = append(matches, Item{Path: fmt.Sprintf("f%d.go", i%2), Block: ctx})
	}
	out := FormatRanked(matches, rankedTestNote, maxOutputBytes)
	if len(out) > maxOutputBytes {
		t.Fatalf("output %d exceeds cap", len(out))
	}
	got := strings.Count(out, ":\n> 1: ")
	if got == 0 || got >= len(matches) {
		t.Fatalf("expected partial render, got %d", got)
	}
	if !strings.HasPrefix(out, rankedTestNote(got)) {
		t.Fatalf("note does not match rendered count %d: %q", got, out[:40])
	}
	if !strings.HasSuffix(out, rankedCapNote) {
		t.Fatal("missing cap note")
	}
	if !strings.Contains(out, "f0.go:") || !strings.Contains(out, "f1.go:") {
		t.Fatal("repeated headers/rank order lost")
	}
}

func TestFormatRankedGrepResults_AllFitNoCapNote(t *testing.T) {
	m := []Item{{Path: "a.go", Block: "> 1: a"}, {Path: "a.go", Block: "> 9: b"}}
	out := FormatRanked(m, rankedTestNote, maxOutputBytes)
	want := rankedTestNote(2) + "\na.go:\n> 1: a\n\na.go:\n> 9: b\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestFormatRankedGrepResults_OversizedFirstBlockKeepsMatch(t *testing.T) {
	before := strings.Repeat("  1: 世界世界世界世界\n", 8000)
	ctx := before + "> 8001: needle 世界 line\n  8002: after"
	m := []Item{
		{Path: "dir/ünï.go", Block: ctx},
		{Path: "other.go", Block: "> 1: second"},
	}
	out := FormatRanked(m, rankedTestNote, maxOutputBytes)
	if len(out) > maxOutputBytes {
		t.Fatalf("output %d exceeds cap", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatal("invalid UTF-8")
	}
	if !strings.HasPrefix(out, rankedTestNote(1)) {
		t.Fatalf("note should report 1 block: %q", out[:40])
	}
	if !strings.Contains(out, "dir/ünï.go:\n") || !strings.Contains(out, "> 8001: needle 世界 line") {
		t.Fatal("header or matched line lost")
	}
	if strings.Contains(out, "other.go") || !strings.HasSuffix(out, rankedCapNote) {
		t.Fatal("unexpected second block or missing cap note")
	}
}

func TestFormatRankedGrepResults_OversizedMatchLineTruncatedSafely(t *testing.T) {
	ctx := "  1: ctx\n> 2: " + strings.Repeat("世", maxOutputBytes)
	out := FormatRanked([]Item{{Path: "a.go", Block: ctx}}, rankedTestNote, maxOutputBytes)
	if len(out) > maxOutputBytes || !utf8.ValidString(out) {
		t.Fatalf("bad output len=%d valid=%v", len(out), utf8.ValidString(out))
	}
	if !strings.Contains(out, "a.go:\n> 2: 世") {
		t.Fatal("matched line prefix lost")
	}
}
