package filetrack

import (
	"strconv"
	"strings"
	"testing"
)

func TestBuildHunks(t *testing.T) {
	oldContent := []byte("line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n")
	newContent := []byte("line1\nline2\nCHANGED\nline4\nline5\nline6\nline7\nline8\nline9\nADDED\nline10\n")

	hunks := BuildHunks("test.txt", oldContent, newContent)
	if len(hunks) == 0 {
		t.Fatal("expected hunks for differing content")
	}

	if hunks[0].OldStart < 1 || hunks[0].NewStart < 1 {
		t.Fatalf("hunk starts = %d/%d, want 1-indexed", hunks[0].OldStart, hunks[0].NewStart)
	}

	var adds, dels, ctx int
	for _, h := range hunks {
		for _, l := range h.Lines {
			switch l.T {
			case "add":
				adds++
			case "del":
				dels++
			case "ctx":
				ctx++
			default:
				t.Fatalf("unknown line type %q", l.T)
			}
		}
	}
	if adds != 2 || dels != 1 {
		t.Fatalf("adds/dels = %d/%d, want 2/1", adds, dels)
	}
	if ctx == 0 {
		t.Fatal("expected context lines")
	}
}

func TestBuildHunksIdenticalContent(t *testing.T) {
	content := []byte("same\n")
	if hunks := BuildHunks("f", content, content); hunks != nil {
		t.Fatalf("hunks = %+v, want nil for identical content", hunks)
	}
}

func TestBuildHunksLineText(t *testing.T) {
	hunks := BuildHunks("f", []byte("old line\n"), []byte("new line\n"))
	var foundDel, foundAdd bool
	for _, h := range hunks {
		for _, l := range h.Lines {
			if l.T == "del" && l.S == "old line" {
				foundDel = true
			}
			if l.T == "add" && l.S == "new line" {
				foundAdd = true
			}
		}
	}
	if !foundDel || !foundAdd {
		t.Fatalf("expected prefix-stripped del/add lines, got %+v", hunks)
	}
}

func TestBuildHunksWithContextExpandsAndMerges(t *testing.T) {
	oldLines := make([]string, 60)
	newLines := make([]string, 60)
	for i := range oldLines {
		oldLines[i] = "line" + strconv.Itoa(i+1)
		newLines[i] = oldLines[i]
	}
	newLines[9] = "changed10"
	newLines[49] = "changed50"
	oldContent := []byte(strings.Join(oldLines, "\n") + "\n")
	newContent := []byte(strings.Join(newLines, "\n") + "\n")

	base := BuildHunks("f", oldContent, newContent)
	if len(base) != 2 {
		t.Fatalf("base hunks = %d, want 2", len(base))
	}
	expanded := BuildHunksWithContext("f", oldContent, newContent, 10)
	if len(expanded) != 2 {
		t.Fatalf("expanded hunks = %d, want 2", len(expanded))
	}
	if expanded[0].OldStart != 1 || expanded[1].OldStart != 40 {
		t.Fatalf("expanded starts = %d/%d, want 1/40", expanded[0].OldStart, expanded[1].OldStart)
	}

	full := BuildHunksWithContext("f", oldContent, newContent, 100)
	if len(full) != 1 {
		t.Fatalf("full hunks = %d, want 1", len(full))
	}
	oldCount, newCount := hunkLineCounts(full[0])
	if full[0].OldStart != 1 || full[0].NewStart != 1 || oldCount != 60 || newCount != 60 {
		t.Fatalf("full hunk = start %d/%d count %d/%d, want 1/1 count 60/60", full[0].OldStart, full[0].NewStart, oldCount, newCount)
	}
}

func TestBuildHunksWithContextCreateAndDelete(t *testing.T) {
	for _, tc := range []struct {
		old string
		new string
	}{
		{"", "one\ntwo\n"},
		{"one\ntwo\n", ""},
	} {
		hunks := BuildHunksWithContext("f", []byte(tc.old), []byte(tc.new), 100)
		if len(hunks) != 1 || len(hunks[0].Lines) != 2 {
			t.Fatalf("BuildHunksWithContext(%q, %q) = %+v, want one complete two-line hunk", tc.old, tc.new, hunks)
		}
	}
}

func TestLineCount(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    int
	}{
		{"", 0},
		{"one", 1},
		{"one\n", 1},
		{"one\ntwo\n", 2},
	} {
		if got := LineCount([]byte(tc.content)); got != tc.want {
			t.Errorf("LineCount(%q) = %d, want %d", tc.content, got, tc.want)
		}
	}
}

func TestCountAddsDels(t *testing.T) {
	tests := []struct {
		name       string
		old, new   string
		adds, dels int
	}{
		{"create", "", "a\nb\nc\n", 3, 0},
		{"create without trailing newline", "", "a\nb", 2, 0},
		{"delete", "a\nb\n", "", 0, 2},
		{"modify", "a\nb\nc\n", "a\nX\nc\n", 1, 1},
		{"empty both", "", "", 0, 0},
		{"pure addition", "a\n", "a\nb\n", 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adds, dels := CountAddsDels([]byte(tt.old), []byte(tt.new))
			if adds != tt.adds || dels != tt.dels {
				t.Fatalf("adds/dels = %d/%d, want %d/%d", adds, dels, tt.adds, tt.dels)
			}
		})
	}
}

func TestCountAddsDelsLargeChange(t *testing.T) {
	oldContent := strings.Repeat("shared\n", 50) + "removed1\nremoved2\n"
	newContent := strings.Repeat("shared\n", 50) + "added1\nadded2\nadded3\n"
	adds, dels := CountAddsDels([]byte(oldContent), []byte(newContent))
	if adds != 3 || dels != 2 {
		t.Fatalf("adds/dels = %d/%d, want 3/2", adds, dels)
	}
}
