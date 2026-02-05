package ui

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUnifiedDiffLineNumbers(t *testing.T) {
	tests := []struct {
		name       string
		oldContent string
		newContent string
		wantLines  []string // Expected line number prefixes in order
	}{
		{
			name:       "replacement with fewer lines",
			oldContent: "line1\nold2\nold3\nold4\nline5\n",
			newContent: "line1\nnew2\nline5\n",
			// Context line1 (1), delete old2-old4 (virtual 2,3,4), add new2 (2), context line5 (3)
			wantLines: []string{"1 ", "2-", "3-", "4-", "2+", "3 "},
		},
		{
			name:       "replacement with more lines",
			oldContent: "line1\nold2\nline3\n",
			newContent: "line1\nnew2\nnew3\nnew4\nline3\n",
			// Context line1 (1), delete old2 (2), add new2-new4 (2-4), context line3 (5)
			wantLines: []string{"1 ", "2-", "2+", "3+", "4+", "5 "},
		},
		{
			name:       "pure deletion",
			oldContent: "line1\ndelete_me\nline3\n",
			newContent: "line1\nline3\n",
			// Context line1 (1), delete delete_me (2), context line3 (2)
			wantLines: []string{"1 ", "2-", "2 "},
		},
		{
			name:       "pure addition",
			oldContent: "line1\nline2\n",
			newContent: "line1\nnew_line\nline2\n",
			// Context line1 (1), add new_line (2), context line2 (3)
			wantLines: []string{"1 ", "2+", "3 "},
		},
	}

	// Regex to extract line numbers from ANSI-colored output
	// Matches patterns like "  1  " (context), "  2- " (deletion), "  3+ " (addition)
	lineNumRe := regexp.MustCompile(`(\d+)([-+ ]) `)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture stdout
			old := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			PrintUnifiedDiff("test.txt", tt.oldContent, tt.newContent)

			w.Close()
			os.Stdout = old

			var buf bytes.Buffer
			io.Copy(&buf, r)
			output := buf.String()

			// Extract all line number prefixes
			matches := lineNumRe.FindAllStringSubmatch(output, -1)

			var gotLines []string
			for _, m := range matches {
				gotLines = append(gotLines, m[1]+m[2])
			}

			// Compare
			if len(gotLines) != len(tt.wantLines) {
				t.Errorf("got %d line prefixes, want %d\ngot:  %v\nwant: %v",
					len(gotLines), len(tt.wantLines), gotLines, tt.wantLines)
				return
			}

			for i := range tt.wantLines {
				if gotLines[i] != tt.wantLines[i] {
					t.Errorf("line %d: got %q, want %q\nfull got:  %v\nfull want: %v",
						i, gotLines[i], tt.wantLines[i], gotLines, tt.wantLines)
				}
			}
		})
	}
}

func TestSplitIntoTokens(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect []string
	}{
		{
			name:   "simple words",
			input:  "hello world",
			expect: []string{"hello", " ", "world"},
		},
		{
			name:   "multiple spaces",
			input:  "hello  world",
			expect: []string{"hello", "  ", "world"},
		},
		{
			name:   "tabs and spaces",
			input:  "\tindented code",
			expect: []string{"\t", "indented", " ", "code"},
		},
		{
			name:   "code with parens",
			input:  "func(x int)",
			expect: []string{"func(x", " ", "int)"},
		},
		{
			name:   "empty string",
			input:  "",
			expect: nil,
		},
		{
			name:   "only whitespace",
			input:  "   ",
			expect: []string{"   "},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitIntoTokens(tt.input)
			if len(got) != len(tt.expect) {
				t.Errorf("got %d tokens, want %d\ngot:  %q\nwant: %q",
					len(got), len(tt.expect), got, tt.expect)
				return
			}
			for i := range tt.expect {
				if got[i] != tt.expect[i] {
					t.Errorf("token %d: got %q, want %q", i, got[i], tt.expect[i])
				}
			}
		})
	}
}

func TestWordDiff(t *testing.T) {
	tests := []struct {
		name            string
		oldLine         string
		newLine         string
		expectOldChange []bool // which segments are changed in old
		expectNewChange []bool // which segments are changed in new
	}{
		{
			name:            "single word change",
			oldLine:         "func doSomething(x int) {",
			newLine:         "func doSomething(y int) {",
			expectOldChange: []bool{false, false, true, false, false, false, false}, // x is changed
			expectNewChange: []bool{false, false, true, false, false, false, false}, // y is changed
		},
		{
			name:    "word addition at end",
			oldLine: "return value",
			newLine: "return value // comment",
			// old: "return", " ", "value" (3 tokens)
			// new: "return", " ", "value", " ", "//", " ", "comment" (7 tokens)
			// LCS: "return", " ", "value"
			expectOldChange: []bool{false, false, false},
			expectNewChange: []bool{false, false, false, true, true, true, true}, // added: " ", "//", " ", "comment"
		},
		{
			name:    "completely different",
			oldLine: "old line",
			newLine: "new text",
			// old: "old", " ", "line" (3 tokens)
			// new: "new", " ", "text" (3 tokens)
			// LCS: " " (single space is common)
			expectOldChange: []bool{true, false, true}, // "old" and "line" changed, space unchanged
			expectNewChange: []bool{true, false, true}, // "new" and "text" changed, space unchanged
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldSegs, newSegs := wordDiff(tt.oldLine, tt.newLine)

			// Check old segments
			if len(oldSegs) != len(tt.expectOldChange) {
				t.Errorf("old segments: got %d, want %d", len(oldSegs), len(tt.expectOldChange))
			} else {
				for i, seg := range oldSegs {
					if seg.isChanged != tt.expectOldChange[i] {
						t.Errorf("old segment %d (%q): isChanged=%v, want %v",
							i, seg.text, seg.isChanged, tt.expectOldChange[i])
					}
				}
			}

			// Check new segments
			if len(newSegs) != len(tt.expectNewChange) {
				t.Errorf("new segments: got %d, want %d", len(newSegs), len(tt.expectNewChange))
			} else {
				for i, seg := range newSegs {
					if seg.isChanged != tt.expectNewChange[i] {
						t.Errorf("new segment %d (%q): isChanged=%v, want %v",
							i, seg.text, seg.isChanged, tt.expectNewChange[i])
					}
				}
			}
		})
	}
}

func TestShouldUseWordDiff(t *testing.T) {
	tests := []struct {
		name    string
		oldLine string
		newLine string
		expect  bool
	}{
		{
			name:    "similar lines",
			oldLine: "func doSomething(x int) {",
			newLine: "func doSomething(y int) {",
			expect:  true, // most words are the same
		},
		{
			name:    "completely different",
			oldLine: "func old() {",
			newLine: "type NewStruct struct {",
			expect:  false, // too different
		},
		{
			name:    "same line",
			oldLine: "return nil",
			newLine: "return nil",
			expect:  true,
		},
		{
			name:    "empty lines",
			oldLine: "",
			newLine: "",
			expect:  false, // empty lines
		},
		{
			name:    "small change in comment",
			oldLine: "// This is a comment about foo",
			newLine: "// This is a comment about bar",
			expect:  true, // most words match
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldUseWordDiff(tt.oldLine, tt.newLine)
			if got != tt.expect {
				t.Errorf("shouldUseWordDiff(%q, %q) = %v, want %v",
					tt.oldLine, tt.newLine, got, tt.expect)
			}
		})
	}
}

func TestRenderDiffSegmentWordDiff(t *testing.T) {
	// Test that word-level diff applies stronger backgrounds to changed words
	oldContent := "func doSomething(x int) {"
	newContent := "func doSomething(y int) {"

	result := RenderDiffSegment("test.go", oldContent, newContent, 120, 1)

	// The result should contain both normal and strong background colors
	// Strong backgrounds may be combined with foreground colors in the same ANSI sequence
	// e.g., \x1b[48;2;90;40;40;38;2;r;g;bm
	strongRedBgPattern := "48;2;90;40;40"
	strongGreenBgPattern := "48;2;40;90;40"

	if !strings.Contains(result, strongRedBgPattern) {
		t.Errorf("expected strong red background (90;40;40) for changed word, not found in output")
	}
	if !strings.Contains(result, strongGreenBgPattern) {
		t.Errorf("expected strong green background (40;90;40) for changed word, not found in output")
	}
}

func TestRenderDiffSegmentNoWordDiff(t *testing.T) {
	// Test that completely different lines don't get word-level diff
	oldContent := "func oldFunction() {"
	newContent := "type NewStruct struct {"

	result := RenderDiffSegment("test.go", oldContent, newContent, 120, 1)

	// Strong backgrounds should NOT appear for completely different lines
	strongRedBg := "\x1b[48;2;90;40;40m"
	strongGreenBg := "\x1b[48;2;40;90;40m"

	if strings.Contains(result, strongRedBg) {
		t.Errorf("strong red background should not appear for completely different lines")
	}
	if strings.Contains(result, strongGreenBg) {
		t.Errorf("strong green background should not appear for completely different lines")
	}
}

func TestWrapDiffLine_DoesNotOverWrapWideRunes(t *testing.T) {
	// Three emoji are 6 terminal cells total (each emoji is width 2).
	// With contentWidth=6, this should stay on a single wrapped line.
	got := wrapDiffLine(2, 1, '+', "", "🙂🙂🙂", 6, nil)

	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 wrapped line, got %d: %q", len(lines), lines)
	}
}

func TestWrapWordDiffLine_DoesNotOverWrapWideRunes(t *testing.T) {
	highlighted := "\x1b[48;2;40;90;40m🙂🙂🙂\x1b[0m"
	got := wrapWordDiffLine(2, 1, '+', "", highlighted, 6, [3]int{30, 60, 30})

	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 wrapped line, got %d: %q", len(lines), lines)
	}
}

func TestSplitAtVisibleLength_WideRunes_NoRuneSplit(t *testing.T) {
	const plain = "🙂🙂🙂🙂"
	input := "\x1b[31m" + plain + "\x1b[0m"

	before, after := splitAtVisibleLength(input, 5)
	beforePlain := stripAnsi(before)
	afterPlain := stripAnsi(after)

	if !utf8.ValidString(beforePlain) {
		t.Fatalf("before split must be valid UTF-8, got %q", beforePlain)
	}
	if !utf8.ValidString(afterPlain) {
		t.Fatalf("after split must be valid UTF-8, got %q", afterPlain)
	}
	if beforePlain+afterPlain != plain {
		t.Fatalf("split must preserve content: got before=%q after=%q", beforePlain, afterPlain)
	}
	if got := ansiDisplayWidth(before, 0); got > 5 {
		t.Fatalf("before part exceeds target width: got=%d target=5", got)
	}
}

func TestWrapDiffLine_WideRunes_WhenWrapping_ValidUTF8(t *testing.T) {
	got := wrapDiffLine(2, 1, '+', "", "🙂🙂🙂🙂", 5, nil)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")

	if len(lines) != 2 {
		t.Fatalf("expected 2 wrapped lines, got %d: %q", len(lines), lines)
	}
	for i, line := range lines {
		if !utf8.ValidString(stripAnsi(line)) {
			t.Fatalf("line %d contains invalid UTF-8 after wrapping: %q", i, line)
		}
	}
}

func TestWrapWordDiffLine_WideRunes_WhenWrapping_ValidUTF8(t *testing.T) {
	highlighted := "\x1b[48;2;40;90;40m🙂🙂🙂🙂\x1b[0m"
	got := wrapWordDiffLine(2, 1, '+', "", highlighted, 5, [3]int{30, 60, 30})
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")

	if len(lines) != 2 {
		t.Fatalf("expected 2 wrapped lines, got %d: %q", len(lines), lines)
	}
	for i, line := range lines {
		if !utf8.ValidString(stripAnsi(line)) {
			t.Fatalf("line %d contains invalid UTF-8 after wrapping: %q", i, line)
		}
	}
}
