package udiff

import (
	"strings"
	"testing"
)

// === Basic Operations ===

func TestApplySimpleReplace(t *testing.T) {
	content := `func Add(a, b int) int {
    return a + b
}`
	hunks := []Hunk{{
		Context: "func Add",
		Lines: []Line{
			{Type: Context, Content: "func Add(a, b int) int {"},
			{Type: Remove, Content: "    return a + b"},
			{Type: Add, Content: "    return a + b + 1"},
			{Type: Context, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	expected := `func Add(a, b int) int {
    return a + b + 1
}`
	if result != expected {
		t.Errorf("unexpected result:\n%s\n\nexpected:\n%s", result, expected)
	}
}

func TestApplyAddLines(t *testing.T) {
	content := `func Foo() {
    doSomething()
}`
	hunks := []Hunk{{
		Context: "func Foo",
		Lines: []Line{
			{Type: Context, Content: "func Foo() {"},
			{Type: Context, Content: "    doSomething()"},
			{Type: Add, Content: "    doSomethingElse()"},
			{Type: Context, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	expected := `func Foo() {
    doSomething()
    doSomethingElse()
}`
	if result != expected {
		t.Errorf("unexpected result:\n%s\n\nexpected:\n%s", result, expected)
	}
}

func TestApplyRemoveLines(t *testing.T) {
	content := `func Foo() {
    line1()
    line2()
    line3()
}`
	hunks := []Hunk{{
		Context: "func Foo",
		Lines: []Line{
			{Type: Context, Content: "func Foo() {"},
			{Type: Context, Content: "    line1()"},
			{Type: Remove, Content: "    line2()"},
			{Type: Context, Content: "    line3()"},
			{Type: Context, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	expected := `func Foo() {
    line1()
    line3()
}`
	if result != expected {
		t.Errorf("unexpected result:\n%s\n\nexpected:\n%s", result, expected)
	}
}

// === Elision Tests ===

func TestApplyElisionEntireFunction(t *testing.T) {
	content := `package main

func OldFunc() {
    line1()
    line2()
    line3()
}

func Other() {
    keep()
}`
	hunks := []Hunk{{
		Context: "func OldFunc",
		Lines: []Line{
			{Type: Remove, Content: "func OldFunc() {"},
			{Type: Elision, Content: ""},
			{Type: Remove, Content: "}"},
			{Type: Add, Content: "func OldFunc() {"},
			{Type: Add, Content: "    newImpl()"},
			{Type: Add, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	if !strings.Contains(result, "newImpl()") {
		t.Error("expected result to contain newImpl()")
	}
	if !strings.Contains(result, "func Other()") {
		t.Error("expected result to still contain func Other()")
	}
	if strings.Contains(result, "line1()") {
		t.Error("expected old function body to be removed")
	}
}

func TestApplyElisionFunctionBody(t *testing.T) {
	content := `func Process(items []Item) error {
    // validation
    if len(items) == 0 {
        return errors.New("empty")
    }

    // processing
    for _, item := range items {
        process(item)
    }

    return nil
}`
	hunks := []Hunk{{
		Context: "func Process",
		Lines: []Line{
			{Type: Remove, Content: "func Process(items []Item) error {"},
			{Type: Elision, Content: ""},
			{Type: Remove, Content: "}"},
			{Type: Add, Content: "func Process(items []Item) error {"},
			{Type: Add, Content: "    return newProcess(items)"},
			{Type: Add, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	expected := `func Process(items []Item) error {
    return newProcess(items)
}`
	if result != expected {
		t.Errorf("unexpected result:\n%s\n\nexpected:\n%s", result, expected)
	}
}

func TestApplyElisionMultipleFunctions(t *testing.T) {
	content := `func First() {
    old1()
}

func Second() {
    old2()
}`
	// Replace both functions
	hunks := []Hunk{
		{
			Context: "func First",
			Lines: []Line{
				{Type: Remove, Content: "func First() {"},
				{Type: Elision, Content: ""},
				{Type: Remove, Content: "}"},
				{Type: Add, Content: "func First() {"},
				{Type: Add, Content: "    new1()"},
				{Type: Add, Content: "}"},
			},
		},
		{
			Context: "func Second",
			Lines: []Line{
				{Type: Remove, Content: "func Second() {"},
				{Type: Elision, Content: ""},
				{Type: Remove, Content: "}"},
				{Type: Add, Content: "func Second() {"},
				{Type: Add, Content: "    new2()"},
				{Type: Add, Content: "}"},
			},
		},
	}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	if !strings.Contains(result, "new1()") {
		t.Error("expected result to contain new1()")
	}
	if !strings.Contains(result, "new2()") {
		t.Error("expected result to contain new2()")
	}
	if strings.Contains(result, "old1()") || strings.Contains(result, "old2()") {
		t.Error("expected old function bodies to be removed")
	}
}

// === Ambiguity Tests (CRITICAL) ===

func TestApplyAmbiguousClosingBrace(t *testing.T) {
	// Multiple } on different lines - should match the right one
	content := `func Outer() {
    if true {
        inner()
    }
    more()
}

func Other() {
    keep()
}`
	hunks := []Hunk{{
		Context: "func Outer",
		Lines: []Line{
			{Type: Remove, Content: "func Outer() {"},
			{Type: Elision, Content: ""},
			{Type: Remove, Content: "}"},
			{Type: Add, Content: "func Outer() {"},
			{Type: Add, Content: "    simplified()"},
			{Type: Add, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// func Other should still exist
	if !strings.Contains(result, "func Other()") {
		t.Error("expected func Other to remain")
	}
	if !strings.Contains(result, "keep()") {
		t.Error("expected keep() to remain in func Other")
	}
	if strings.Contains(result, "inner()") || strings.Contains(result, "more()") {
		t.Error("expected func Outer body to be replaced")
	}
}

func TestApplyNestedBraces(t *testing.T) {
	content := `func Deep() {
    if a {
        if b {
            if c {
                deep()
            }
        }
    }
}

func After() {}`
	hunks := []Hunk{{
		Context: "func Deep",
		Lines: []Line{
			{Type: Remove, Content: "func Deep() {"},
			{Type: Elision, Content: ""},
			{Type: Remove, Content: "}"},
			{Type: Add, Content: "func Deep() {"},
			{Type: Add, Content: "    shallow()"},
			{Type: Add, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	if !strings.Contains(result, "shallow()") {
		t.Error("expected shallow() in result")
	}
	if !strings.Contains(result, "func After()") {
		t.Error("expected func After to remain")
	}
	if strings.Contains(result, "deep()") {
		t.Error("expected deep() to be removed")
	}
}

func TestApplyBraceInString(t *testing.T) {
	content := `func WithString() {
    fmt.Println("}")
    fmt.Println("{")
    fmt.Println("}{")
    real()
}

func Next() {}`
	hunks := []Hunk{{
		Context: "func WithString",
		Lines: []Line{
			{Type: Remove, Content: "func WithString() {"},
			{Type: Elision, Content: ""},
			{Type: Remove, Content: "}"},
			{Type: Add, Content: "func WithString() {"},
			{Type: Add, Content: "    replaced()"},
			{Type: Add, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	if !strings.Contains(result, "replaced()") {
		t.Error("expected replaced() in result")
	}
	if !strings.Contains(result, "func Next()") {
		t.Error("expected func Next to remain")
	}
	if strings.Contains(result, "real()") {
		t.Error("expected real() to be removed")
	}
}

func TestApplyBraceInComment(t *testing.T) {
	content := `func WithComment() {
    // } this brace is in a comment
    /* } this too */
    actual()
}

func Keep() {}`
	hunks := []Hunk{{
		Context: "func WithComment",
		Lines: []Line{
			{Type: Remove, Content: "func WithComment() {"},
			{Type: Elision, Content: ""},
			{Type: Remove, Content: "}"},
			{Type: Add, Content: "func WithComment() {"},
			{Type: Add, Content: "    new()"},
			{Type: Add, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	if !strings.Contains(result, "new()") {
		t.Error("expected new() in result")
	}
	if !strings.Contains(result, "func Keep()") {
		t.Error("expected func Keep to remain")
	}
	if strings.Contains(result, "actual()") {
		t.Error("expected actual() to be removed")
	}
}

func TestApplyMultipleMatchingFunctions(t *testing.T) {
	// Two functions with similar names - context should disambiguate
	content := `func Process() {
    first()
}

func ProcessItems() {
    second()
}`
	hunks := []Hunk{{
		Context: "func ProcessItems",
		Lines: []Line{
			{Type: Remove, Content: "func ProcessItems() {"},
			{Type: Elision, Content: ""},
			{Type: Remove, Content: "}"},
			{Type: Add, Content: "func ProcessItems() {"},
			{Type: Add, Content: "    newSecond()"},
			{Type: Add, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// First function should remain unchanged
	if !strings.Contains(result, "first()") {
		t.Error("expected first() to remain in func Process")
	}
	// Second function should be replaced
	if !strings.Contains(result, "newSecond()") {
		t.Error("expected newSecond() in func ProcessItems")
	}
	if strings.Contains(result, "second()") {
		t.Error("expected second() to be removed")
	}
}

// === Context Matching Tests ===

func TestApplyContextNotFound(t *testing.T) {
	content := `func Foo() {
    bar()
}`
	hunks := []Hunk{{
		Context: "func NonExistent",
		Lines: []Line{
			{Type: Remove, Content: "func NonExistent() {"},
			{Type: Add, Content: "func NonExistent() {"},
		},
	}}

	_, err := Apply(content, hunks)
	if err == nil {
		t.Error("expected error when context not found")
	}
}

func TestApplyContextMultipleMatches(t *testing.T) {
	// When context matches multiple places, should use the first
	content := `func Do() {
    first()
}

func Do() {
    second()
}`
	hunks := []Hunk{{
		Context: "func Do",
		Lines: []Line{
			{Type: Context, Content: "func Do() {"},
			{Type: Remove, Content: "    first()"},
			{Type: Add, Content: "    replaced()"},
			{Type: Context, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// First match should be replaced
	if !strings.Contains(result, "replaced()") {
		t.Error("expected replaced() in result")
	}
	// Second should remain (unless also matched)
	if !strings.Contains(result, "second()") {
		t.Error("expected second() to remain")
	}
}

func TestApplyWhitespaceTolerance(t *testing.T) {
	// Diff might have slightly different whitespace
	content := `func Foo() {
    bar()
}`
	hunks := []Hunk{{
		Context: "func Foo",
		Lines: []Line{
			{Type: Context, Content: "func Foo() {"},
			{Type: Remove, Content: "  bar()"},   // 2 spaces instead of 4
			{Type: Add, Content: "    baz()"},
			{Type: Context, Content: "}"},
		},
	}}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	if !strings.Contains(result, "baz()") {
		t.Error("expected baz() in result with whitespace tolerance")
	}
}

// === Multi-hunk Tests ===

func TestApplyMultipleHunksSameFile(t *testing.T) {
	content := `func First() {
    a()
}

func Second() {
    b()
}

func Third() {
    c()
}`
	hunks := []Hunk{
		{
			Context: "func First",
			Lines: []Line{
				{Type: Context, Content: "func First() {"},
				{Type: Remove, Content: "    a()"},
				{Type: Add, Content: "    newA()"},
				{Type: Context, Content: "}"},
			},
		},
		{
			Context: "func Third",
			Lines: []Line{
				{Type: Context, Content: "func Third() {"},
				{Type: Remove, Content: "    c()"},
				{Type: Add, Content: "    newC()"},
				{Type: Context, Content: "}"},
			},
		},
	}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	if !strings.Contains(result, "newA()") {
		t.Error("expected newA() in result")
	}
	if !strings.Contains(result, "b()") {
		t.Error("expected b() to remain unchanged")
	}
	if !strings.Contains(result, "newC()") {
		t.Error("expected newC() in result")
	}
}

func TestApplyHunksPreserveOrder(t *testing.T) {
	content := `line1
line2
line3`
	hunks := []Hunk{
		{
			Lines: []Line{
				{Type: Context, Content: "line1"},
				{Type: Remove, Content: "line2"},
				{Type: Add, Content: "LINE2"},
				{Type: Context, Content: "line3"},
			},
		},
	}

	result, err := Apply(content, hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	expected := `line1
LINE2
line3`
	if result != expected {
		t.Errorf("unexpected result:\n%s\n\nexpected:\n%s", result, expected)
	}
}

// === ApplyFileDiffs Tests ===

func TestApplyFileDiffs(t *testing.T) {
	files := map[string]string{
		"a.go": "func A() { old() }",
		"b.go": "func B() { keep() }",
	}

	diffs := []FileDiff{{
		Path: "a.go",
		Hunks: []Hunk{{
			Context: "func A",
			Lines: []Line{
				{Type: Remove, Content: "func A() { old() }"},
				{Type: Add, Content: "func A() { new() }"},
			},
		}},
	}}

	result, err := ApplyFileDiffs(files, diffs)
	if err != nil {
		t.Fatalf("ApplyFileDiffs failed: %v", err)
	}

	if result["a.go"] != "func A() { new() }" {
		t.Errorf("a.go not updated: %s", result["a.go"])
	}
	if result["b.go"] != "func B() { keep() }" {
		t.Errorf("b.go should be unchanged: %s", result["b.go"])
	}
}

func TestApplyFileDiffsFileNotFound(t *testing.T) {
	files := map[string]string{
		"a.go": "content",
	}

	diffs := []FileDiff{{
		Path:  "nonexistent.go",
		Hunks: []Hunk{},
	}}

	_, err := ApplyFileDiffs(files, diffs)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// === Brace Depth Tracking Tests ===

func TestUpdateBraceDepth(t *testing.T) {
	tests := []struct {
		name     string
		depth    int
		line     string
		expected int
	}{
		{"open brace", 0, "{", 1},
		{"close brace", 1, "}", 0},
		{"nested", 1, "if { }", 1},
		{"brace in string", 0, `fmt.Println("}")`, 0},
		{"brace in single quote", 0, `'}'`, 0},
		{"brace in raw string", 0, "`}`", 0},
		{"brace in line comment", 0, "// }", 0},
		{"brace in block comment", 0, "/* } */", 0},
		{"mixed", 0, `{ "}" } // }`, 0}, // { opens, "}" is in string (ignored), } closes = depth 0
		{"empty", 0, "", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := updateBraceDepth(tc.depth, tc.line)
			if result != tc.expected {
				t.Errorf("updateBraceDepth(%d, %q) = %d, expected %d",
					tc.depth, tc.line, result, tc.expected)
			}
		})
	}
}

// === Integration Test ===

func TestFullParseAndApply(t *testing.T) {
	diff := `--- main.go
+++ main.go
@@ func Calculate @@
-func Calculate(x int) int {
-...
-}
+func Calculate(x int) int {
+    return x * 2
+}
`
	content := `package main

func Calculate(x int) int {
    // complex calculation
    result := x
    for i := 0; i < 10; i++ {
        result += i
    }
    return result
}

func main() {
    fmt.Println(Calculate(5))
}`

	files, err := Parse(diff)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	result, err := Apply(content, files[0].Hunks)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	if !strings.Contains(result, "return x * 2") {
		t.Error("expected new implementation")
	}
	if !strings.Contains(result, "func main()") {
		t.Error("expected main function to remain")
	}
	if strings.Contains(result, "complex calculation") {
		t.Error("expected old implementation to be removed")
	}
}
