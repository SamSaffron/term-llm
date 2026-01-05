package input

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFiles(t *testing.T) {
	// Create a temp directory for test files
	tempDir, err := os.MkdirTemp("", "term-llm-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create test files
	file1 := filepath.Join(tempDir, "test1.txt")
	file2 := filepath.Join(tempDir, "test2.txt")
	os.WriteFile(file1, []byte("content1"), 0644)
	os.WriteFile(file2, []byte("content2"), 0644)

	t.Run("single file", func(t *testing.T) {
		files, err := ReadFiles([]string{file1})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d", len(files))
		}
		if files[0].Content != "content1" {
			t.Errorf("expected content1, got %s", files[0].Content)
		}
	})

	t.Run("multiple files", func(t *testing.T) {
		files, err := ReadFiles([]string{file1, file2})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 2 {
			t.Fatalf("expected 2 files, got %d", len(files))
		}
	})

	t.Run("glob pattern", func(t *testing.T) {
		pattern := filepath.Join(tempDir, "*.txt")
		files, err := ReadFiles([]string{pattern})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 2 {
			t.Fatalf("expected 2 files from glob, got %d", len(files))
		}
	})

	t.Run("non-existent file", func(t *testing.T) {
		_, err := ReadFiles([]string{"/nonexistent/file.txt"})
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})

	t.Run("empty path list", func(t *testing.T) {
		files, err := ReadFiles([]string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 0 {
			t.Errorf("expected 0 files, got %d", len(files))
		}
	})
}

func TestFormatFilesXML(t *testing.T) {
	t.Run("single file", func(t *testing.T) {
		files := []FileContent{
			{Path: "test.txt", Content: "hello world"},
		}
		result := FormatFilesXML(files, "")
		expected := `<<<<< FILE: test.txt >>>>>
hello world
<<<<< END FILE >>>>>`
		if result != expected {
			t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
		}
	})

	t.Run("multiple files", func(t *testing.T) {
		files := []FileContent{
			{Path: "a.txt", Content: "aaa"},
			{Path: "b.txt", Content: "bbb"},
		}
		result := FormatFilesXML(files, "")
		if result == "" {
			t.Error("expected non-empty result")
		}
		// Check both files are present
		if !contains(result, "<<<<< FILE: a.txt >>>>>") || !contains(result, "<<<<< FILE: b.txt >>>>>") {
			t.Errorf("result missing file markers: %s", result)
		}
	})

	t.Run("with stdin", func(t *testing.T) {
		result := FormatFilesXML(nil, "stdin content")
		expected := `<<<<< STDIN >>>>>
stdin content
<<<<< END STDIN >>>>>`
		if result != expected {
			t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
		}
	})

	t.Run("files and stdin", func(t *testing.T) {
		files := []FileContent{
			{Path: "test.txt", Content: "file content"},
		}
		result := FormatFilesXML(files, "stdin content")
		if !contains(result, "<<<<< FILE: test.txt >>>>>") {
			t.Error("missing file marker")
		}
		if !contains(result, "<<<<< STDIN >>>>>") {
			t.Error("missing stdin marker")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		result := FormatFilesXML(nil, "")
		if result != "" {
			t.Errorf("expected empty result, got: %s", result)
		}
	})
}

func TestHasStdin(t *testing.T) {
	// In test environment, stdin is usually not a pipe
	// This test just ensures the function doesn't panic
	result := HasStdin()
	// We can't really test the true case in unit tests without mocking
	// Just verify it returns a boolean without error
	_ = result
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
