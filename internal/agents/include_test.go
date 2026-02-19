package agents

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestExpandFileIncludes_BasicRelative(t *testing.T) {
	tmp := t.TempDir()
	partPath := filepath.Join(tmp, "part.md")
	writeTestFile(t, partPath, "included content")

	out, err := ExpandFileIncludes("before\n{{file:part.md}}\nafter", IncludeOptions{
		BaseDir:       tmp,
		MaxDepth:      10,
		AllowAbsolute: true,
	})
	if err != nil {
		t.Fatalf("ExpandFileIncludes() error = %v", err)
	}

	want := "before\nincluded content\nafter"
	if out != want {
		t.Fatalf("ExpandFileIncludes() = %q, want %q", out, want)
	}
}

func TestExpandFileIncludes_NestedAndWhitespace(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "a.md"), "A->{{file: b.md }}")
	writeTestFile(t, filepath.Join(tmp, "b.md"), "B")

	out, err := ExpandFileIncludes("{{file: a.md}}", IncludeOptions{
		BaseDir:       tmp,
		MaxDepth:      10,
		AllowAbsolute: true,
	})
	if err != nil {
		t.Fatalf("ExpandFileIncludes() error = %v", err)
	}
	if out != "A->B" {
		t.Fatalf("ExpandFileIncludes() = %q, want %q", out, "A->B")
	}
}

func TestExpandFileIncludes_AbsolutePathAllowed(t *testing.T) {
	tmp := t.TempDir()
	abs := filepath.Join(tmp, "abs.md")
	writeTestFile(t, abs, "abs")

	out, err := ExpandFileIncludes("{{file:"+abs+"}}", IncludeOptions{
		BaseDir:       tmp,
		MaxDepth:      10,
		AllowAbsolute: true,
	})
	if err != nil {
		t.Fatalf("ExpandFileIncludes() error = %v", err)
	}
	if out != "abs" {
		t.Fatalf("ExpandFileIncludes() = %q, want %q", out, "abs")
	}
}

func TestExpandFileIncludes_MissingFileFails(t *testing.T) {
	tmp := t.TempDir()
	_, err := ExpandFileIncludes("{{file:nope.md}}", IncludeOptions{
		BaseDir:       tmp,
		MaxDepth:      10,
		AllowAbsolute: true,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "nope.md") {
		t.Fatalf("error %q should mention missing file", err.Error())
	}
}

func TestExpandFileIncludes_DetectsCycle(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "a.md"), "{{file:b.md}}")
	writeTestFile(t, filepath.Join(tmp, "b.md"), "{{file:a.md}}")

	_, err := ExpandFileIncludes("{{file:a.md}}", IncludeOptions{
		BaseDir:       tmp,
		MaxDepth:      10,
		AllowAbsolute: true,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "cycle") {
		t.Fatalf("error %q should mention cycle", err.Error())
	}
}

func TestExpandFileIncludes_MaxDepth(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i <= 10; i++ {
		name := filepath.Join(tmp, "f"+strconv.Itoa(i)+".md")
		next := "done"
		if i < 10 {
			next = "{{file:f" + strconv.Itoa(i+1) + ".md}}"
		}
		writeTestFile(t, name, next)
	}

	_, err := ExpandFileIncludes("{{file:f0.md}}", IncludeOptions{
		BaseDir:       tmp,
		MaxDepth:      5,
		AllowAbsolute: true,
	})
	if err == nil {
		t.Fatal("expected max depth error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "depth") {
		t.Fatalf("error %q should mention depth", err.Error())
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
