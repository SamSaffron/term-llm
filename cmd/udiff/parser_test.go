package udiff

import (
	"testing"
)

func TestParseBasic(t *testing.T) {
	diff := `--- main.go
+++ main.go
@@ func Add @@
 func Add(a, b int) int {
-    return a + b
+    return a + b + 1
 }
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	file := files[0]
	if file.Path != "main.go" {
		t.Errorf("expected path 'main.go', got %q", file.Path)
	}

	if len(file.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(file.Hunks))
	}

	hunk := file.Hunks[0]
	if hunk.Context != "func Add" {
		t.Errorf("expected context 'func Add', got %q", hunk.Context)
	}

	// Should have: context, remove, add, context
	if len(hunk.Lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(hunk.Lines))
	}

	expected := []struct {
		typ     LineType
		content string
	}{
		{Context, "func Add(a, b int) int {"},
		{Remove, "    return a + b"},
		{Add, "    return a + b + 1"},
		{Context, "}"},
	}

	for i, exp := range expected {
		if hunk.Lines[i].Type != exp.typ {
			t.Errorf("line %d: expected type %v, got %v", i, exp.typ, hunk.Lines[i].Type)
		}
		if hunk.Lines[i].Content != exp.content {
			t.Errorf("line %d: expected content %q, got %q", i, exp.content, hunk.Lines[i].Content)
		}
	}
}

func TestParseMultipleFiles(t *testing.T) {
	diff := `--- file1.go
+++ file1.go
@@ func One @@
-old1
+new1

--- file2.go
+++ file2.go
@@ func Two @@
-old2
+new2
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	if files[0].Path != "file1.go" {
		t.Errorf("expected first file 'file1.go', got %q", files[0].Path)
	}
	if files[1].Path != "file2.go" {
		t.Errorf("expected second file 'file2.go', got %q", files[1].Path)
	}

	if files[0].Hunks[0].Context != "func One" {
		t.Errorf("expected first hunk context 'func One', got %q", files[0].Hunks[0].Context)
	}
	if files[1].Hunks[0].Context != "func Two" {
		t.Errorf("expected second hunk context 'func Two', got %q", files[1].Hunks[0].Context)
	}
}

func TestParseMultipleHunks(t *testing.T) {
	diff := `--- main.go
+++ main.go
@@ func First @@
-old1
+new1
@@ func Second @@
-old2
+new2
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	if len(files[0].Hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d", len(files[0].Hunks))
	}

	if files[0].Hunks[0].Context != "func First" {
		t.Errorf("expected first hunk context 'func First', got %q", files[0].Hunks[0].Context)
	}
	if files[0].Hunks[1].Context != "func Second" {
		t.Errorf("expected second hunk context 'func Second', got %q", files[0].Hunks[1].Context)
	}
}

func TestParseElision(t *testing.T) {
	diff := `--- main.go
+++ main.go
@@ func BigFunc @@
-func BigFunc() {
-...
-}
+func BigFunc() {
+    simplified()
+}
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	hunk := files[0].Hunks[0]

	// Should have: remove, elision, remove, add, add, add
	if len(hunk.Lines) != 6 {
		t.Fatalf("expected 6 lines, got %d", len(hunk.Lines))
	}

	// Check elision marker
	if hunk.Lines[1].Type != Elision {
		t.Errorf("expected line 1 to be Elision, got %v", hunk.Lines[1].Type)
	}

	// Check structure
	expected := []LineType{Remove, Elision, Remove, Add, Add, Add}
	for i, exp := range expected {
		if hunk.Lines[i].Type != exp {
			t.Errorf("line %d: expected type %v, got %v", i, exp, hunk.Lines[i].Type)
		}
	}
}

func TestParseElisionWithWhitespace(t *testing.T) {
	// Elision marker might have trailing whitespace
	diff := `--- main.go
+++ main.go
@@
-start
-...
-end
+replaced
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	hunk := files[0].Hunks[0]
	if hunk.Lines[1].Type != Elision {
		t.Errorf("expected line 1 to be Elision (with trailing whitespace), got %v", hunk.Lines[1].Type)
	}
}

func TestParseEmptyDiff(t *testing.T) {
	files, err := Parse("")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(files) != 0 {
		t.Errorf("expected 0 files for empty diff, got %d", len(files))
	}
}

func TestParseNoHunkHeader(t *testing.T) {
	// Some diffs don't have @@ headers
	diff := `--- main.go
+++ main.go
-old line
+new line
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	if len(files[0].Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(files[0].Hunks))
	}

	// Hunk should have no context (empty string)
	if files[0].Hunks[0].Context != "" {
		t.Errorf("expected empty context, got %q", files[0].Hunks[0].Context)
	}
}

func TestParseGitPrefixes(t *testing.T) {
	// Git diffs have a/ and b/ prefixes
	diff := `--- a/path/to/file.go
+++ b/path/to/file.go
@@
-old
+new
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if files[0].Path != "path/to/file.go" {
		t.Errorf("expected path without git prefix, got %q", files[0].Path)
	}
}

func TestParseHunkHeaderFormats(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected string
	}{
		{"with trailing @@", "@@ func Foo @@", "func Foo"},
		{"without trailing @@", "@@ func Foo", "func Foo"},
		{"empty context", "@@", ""},
		{"just spaces", "@@  @@", ""},
		{"extra whitespace", "@@   func Foo   @@", "func Foo"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseHunkHeader(tc.header)
			if result != tc.expected {
				t.Errorf("parseHunkHeader(%q) = %q, expected %q", tc.header, result, tc.expected)
			}
		})
	}
}

func TestParseEmptyLines(t *testing.T) {
	// Empty lines in diff should be treated as empty context
	diff := `--- main.go
+++ main.go
@@
 line1

 line3
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	hunk := files[0].Hunks[0]
	if len(hunk.Lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(hunk.Lines))
	}

	// Second line should be empty context
	if hunk.Lines[1].Type != Context {
		t.Errorf("expected empty line to be Context, got %v", hunk.Lines[1].Type)
	}
	if hunk.Lines[1].Content != "" {
		t.Errorf("expected empty content, got %q", hunk.Lines[1].Content)
	}
}

func TestParseDiffLineTypes(t *testing.T) {
	tests := []struct {
		line        string
		expectType  LineType
		expectCont  string
		expectError bool
	}{
		{" context", Context, "context", false},
		{"-remove", Remove, "remove", false},
		{"+add", Add, "add", false},
		{"-...", Elision, "", false},
		{"-...  ", Elision, "", false},
		{"", Context, "", false},  // Empty line
		{" ", Context, "", false}, // Just space
		{"x", Context, "", true},  // Invalid prefix
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			result, err := parseDiffLine(tc.line)
			if tc.expectError {
				if err == nil {
					t.Errorf("expected error for line %q", tc.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Type != tc.expectType {
				t.Errorf("expected type %v, got %v", tc.expectType, result.Type)
			}
			if result.Content != tc.expectCont {
				t.Errorf("expected content %q, got %q", tc.expectCont, result.Content)
			}
		})
	}
}

func TestParseComplexDiff(t *testing.T) {
	// A more realistic complex diff
	diff := `--- pkg/handler.go
+++ pkg/handler.go
@@ func HandleRequest @@
 func HandleRequest(w http.ResponseWriter, r *http.Request) {
-    // old validation
-    if r.Method != "POST" {
-        http.Error(w, "method not allowed", 405)
-        return
-    }
+    // new validation with more methods
+    if r.Method != "POST" && r.Method != "PUT" {
+        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
+        return
+    }

     // rest of handler
 }
@@ func ValidateInput @@
-func ValidateInput(data []byte) error {
-...
-}
+func ValidateInput(data []byte) (bool, error) {
+    if len(data) == 0 {
+        return false, errors.New("empty input")
+    }
+    return true, nil
+}

--- pkg/utils.go
+++ pkg/utils.go
@@ func Helper @@
-func Helper() {}
+func Helper() error { return nil }
`
	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// First file should have 2 hunks
	if len(files[0].Hunks) != 2 {
		t.Fatalf("expected 2 hunks in first file, got %d", len(files[0].Hunks))
	}

	// Second hunk should contain elision
	hunk2 := files[0].Hunks[1]
	hasElision := false
	for _, line := range hunk2.Lines {
		if line.Type == Elision {
			hasElision = true
			break
		}
	}
	if !hasElision {
		t.Error("expected second hunk to contain elision marker")
	}

	// Second file should have 1 hunk
	if len(files[1].Hunks) != 1 {
		t.Fatalf("expected 1 hunk in second file, got %d", len(files[1].Hunks))
	}
}
