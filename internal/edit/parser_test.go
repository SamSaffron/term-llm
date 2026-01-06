package edit

import (
	"errors"
	"testing"
)

func TestStreamParser_SearchReplace(t *testing.T) {
	var fileStartPath string
	var searchContent string
	var replaceContent string
	var aboutContent string
	var fileComplete bool

	callbacks := ParserCallbacks{
		OnFileStart: func(path string) {
			fileStartPath = path
		},
		OnSearchReady: func(path, search string) error {
			searchContent = search
			return nil
		},
		OnReplaceReady: func(path, search, replace string) {
			replaceContent = replace
		},
		OnFileComplete: func(edit FileEdit) {
			fileComplete = true
		},
		OnAboutComplete: func(content string) {
			aboutContent = content
		},
	}

	parser := NewStreamParser(callbacks)

	// Feed in chunks to simulate streaming
	chunks := []string{
		"[FILE: test.go]\n",
		"<<<<<<< SEARCH\n",
		"old code\n",
		"=======\n",
		"new code\n",
		">>>>>>> REPLACE\n",
		"[/FILE]\n\n",
		"[ABOUT]\n",
		"Fixed the code\n",
		"[/ABOUT]\n",
	}

	for _, chunk := range chunks {
		if err := parser.Feed(chunk); err != nil {
			t.Fatalf("Feed error: %v", err)
		}
	}

	if err := parser.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}

	if fileStartPath != "test.go" {
		t.Errorf("expected file path 'test.go', got %q", fileStartPath)
	}

	if searchContent != "old code" {
		t.Errorf("expected search 'old code', got %q", searchContent)
	}

	if replaceContent != "new code" {
		t.Errorf("expected replace 'new code', got %q", replaceContent)
	}

	if !fileComplete {
		t.Error("expected OnFileComplete to be called")
	}

	if aboutContent != "Fixed the code" {
		t.Errorf("expected about 'Fixed the code', got %q", aboutContent)
	}
}

func TestStreamParser_MultipleEdits(t *testing.T) {
	input := `[FILE: test.go]
<<<<<<< SEARCH
first old
=======
first new
>>>>>>> REPLACE
<<<<<<< SEARCH
second old
=======
second new
>>>>>>> REPLACE
[/FILE]
`

	var edits []struct {
		search  string
		replace string
	}

	callbacks := ParserCallbacks{
		OnSearchReady: func(path, search string) error {
			return nil
		},
		OnReplaceReady: func(path, search, replace string) {
			edits = append(edits, struct {
				search  string
				replace string
			}{search, replace})
		},
	}

	parser := NewStreamParser(callbacks)
	if err := parser.Feed(input); err != nil {
		t.Fatalf("Feed error: %v", err)
	}
	if err := parser.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}

	if len(edits) != 2 {
		t.Fatalf("expected 2 edits, got %d", len(edits))
	}

	if edits[0].search != "first old" {
		t.Errorf("first search: expected 'first old', got %q", edits[0].search)
	}
	if edits[0].replace != "first new" {
		t.Errorf("first replace: expected 'first new', got %q", edits[0].replace)
	}
	if edits[1].search != "second old" {
		t.Errorf("second search: expected 'second old', got %q", edits[1].search)
	}
	if edits[1].replace != "second new" {
		t.Errorf("second replace: expected 'second new', got %q", edits[1].replace)
	}
}

func TestStreamParser_UnifiedDiff(t *testing.T) {
	input := `[FILE: test.go]
--- test.go
+++ test.go
@@ func main @@
 func main() {
-    old()
+    new()
 }
[/FILE]
`

	var diffLines []string
	var fileComplete bool

	callbacks := ParserCallbacks{
		OnDiffReady: func(path string, lines []string) error {
			diffLines = lines
			return nil
		},
		OnFileComplete: func(edit FileEdit) {
			fileComplete = true
			if edit.Format != FormatUnifiedDiff {
				t.Errorf("expected FormatUnifiedDiff, got %v", edit.Format)
			}
		},
	}

	parser := NewStreamParser(callbacks)
	if err := parser.Feed(input); err != nil {
		t.Fatalf("Feed error: %v", err)
	}
	if err := parser.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}

	if !fileComplete {
		t.Error("expected OnFileComplete to be called")
	}

	if len(diffLines) == 0 {
		t.Error("expected diff lines to be collected")
	}
}

func TestStreamParser_SearchValidationHalt(t *testing.T) {
	input := `[FILE: test.go]
<<<<<<< SEARCH
bad search
=======
replacement
>>>>>>> REPLACE
[/FILE]
`

	validationErr := errors.New("search not found")

	callbacks := ParserCallbacks{
		OnSearchReady: func(path, search string) error {
			return validationErr
		},
	}

	parser := NewStreamParser(callbacks)
	err := parser.Feed(input)

	if err == nil {
		t.Error("expected error from search validation")
	}

	if !parser.IsHalted() {
		t.Error("expected parser to be halted")
	}

	if parser.HaltError() != validationErr {
		t.Errorf("expected halt error %v, got %v", validationErr, parser.HaltError())
	}
}

func TestStreamParser_MultipleFiles(t *testing.T) {
	input := `[FILE: a.go]
<<<<<<< SEARCH
code a
=======
new a
>>>>>>> REPLACE
[/FILE]

[FILE: b.go]
<<<<<<< SEARCH
code b
=======
new b
>>>>>>> REPLACE
[/FILE]
`

	var files []string

	callbacks := ParserCallbacks{
		OnFileStart: func(path string) {
			files = append(files, path)
		},
		OnSearchReady: func(path, search string) error {
			return nil
		},
	}

	parser := NewStreamParser(callbacks)
	if err := parser.Feed(input); err != nil {
		t.Fatalf("Feed error: %v", err)
	}
	if err := parser.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	if files[0] != "a.go" {
		t.Errorf("expected first file 'a.go', got %q", files[0])
	}
	if files[1] != "b.go" {
		t.Errorf("expected second file 'b.go', got %q", files[1])
	}
}

func TestStreamParser_ChunkedInput(t *testing.T) {
	// Test that partial line buffering works correctly
	input := "[FILE: test.go]\n<<<<<<< SEARCH\nold\n=======\nnew\n>>>>>>> REPLACE\n[/FILE]"

	var searchContent string

	callbacks := ParserCallbacks{
		OnSearchReady: func(path, search string) error {
			searchContent = search
			return nil
		},
	}

	parser := NewStreamParser(callbacks)

	// Feed one character at a time
	for _, ch := range input {
		if err := parser.Feed(string(ch)); err != nil {
			t.Fatalf("Feed error: %v", err)
		}
	}

	if err := parser.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}

	if searchContent != "old" {
		t.Errorf("expected search 'old', got %q", searchContent)
	}
}

func TestStreamParser_Reset(t *testing.T) {
	parser := NewStreamParser(ParserCallbacks{})

	// Parse something first
	parser.Feed("[FILE: test.go]\n")
	parser.Feed("<<<<<<< SEARCH\n")

	if parser.State() != StateInSearch {
		t.Errorf("expected StateInSearch before reset, got %v", parser.State())
	}

	parser.Reset()

	if parser.State() != StateIdle {
		t.Errorf("expected StateIdle after reset, got %v", parser.State())
	}

	if parser.CurrentFile() != "" {
		t.Errorf("expected empty current file after reset, got %q", parser.CurrentFile())
	}
}

func TestFormat_String(t *testing.T) {
	tests := []struct {
		format Format
		want   string
	}{
		{FormatUnknown, "unknown"},
		{FormatSearchReplace, "search-replace"},
		{FormatUnifiedDiff, "unified-diff"},
	}

	for _, tt := range tests {
		if got := tt.format.String(); got != tt.want {
			t.Errorf("Format(%d).String() = %q, want %q", tt.format, got, tt.want)
		}
	}
}

func TestParserState_String(t *testing.T) {
	tests := []struct {
		state ParserState
		want  string
	}{
		{StateIdle, "idle"},
		{StateInFile, "in-file"},
		{StateInSearch, "in-search"},
		{StateInReplace, "in-replace"},
		{StateInDiff, "in-diff"},
		{StateInAbout, "in-about"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("ParserState(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestStreamParser_IncompleteAbout(t *testing.T) {
	// Test that ABOUT content is flushed even without [/ABOUT] closing tag
	// This happens when LLM doesn't output the closing tag
	var aboutContent string

	callbacks := ParserCallbacks{
		OnSearchReady: func(path, search string) error {
			return nil
		},
		OnAboutComplete: func(content string) {
			aboutContent = content
		},
	}

	parser := NewStreamParser(callbacks)

	// Feed input WITHOUT closing [/ABOUT]
	input := `[FILE: test.go]
<<<<<<< SEARCH
old
=======
new
>>>>>>> REPLACE
[/FILE]

[ABOUT]
This is the about text
that spans multiple lines
`

	if err := parser.Feed(input); err != nil {
		t.Fatalf("Feed error: %v", err)
	}

	// Before Finish, about content should be empty (not yet flushed)
	if aboutContent != "" {
		t.Errorf("expected empty about content before Finish, got %q", aboutContent)
	}

	if err := parser.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}

	// After Finish, incomplete about should be flushed
	expected := "This is the about text\nthat spans multiple lines"
	if aboutContent != expected {
		t.Errorf("expected about %q, got %q", expected, aboutContent)
	}
}
