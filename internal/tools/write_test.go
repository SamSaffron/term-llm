package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/diff"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestWriteFileTool_NewSmallFileEmitsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.go")
	expectedPath, err := resolveToolPath(path, true)
	if err != nil {
		t.Fatalf("failed to resolve expected path: %v", err)
	}
	content := "package main\n\nfunc main() {}\n"

	tool := NewWriteFileTool(nil)
	args, err := json.Marshal(WriteFileArgs{Path: path, Content: content})
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(written) != content {
		t.Fatalf("written content = %q, want %q", string(written), content)
	}

	if len(output.Diffs) != 1 {
		t.Fatalf("expected one diff, got %d: %#v", len(output.Diffs), output.Diffs)
	}
	d := output.Diffs[0]
	if d.File != expectedPath {
		t.Errorf("diff file = %q, want %q", d.File, expectedPath)
	}
	if d.Old != "" {
		t.Errorf("diff old = %q, want empty", d.Old)
	}
	if d.New != content {
		t.Errorf("diff new = %q, want %q", d.New, content)
	}
	if d.Line != 1 {
		t.Errorf("diff line = %d, want 1", d.Line)
	}
	if d.Operation != core.DiffOperationCreate {
		t.Errorf("diff operation = %q, want %q", d.Operation, core.DiffOperationCreate)
	}
}

func TestWriteFileTool_NewLargeFileSkipsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	content := strings.Repeat("x", diff.MaxDiffSize+1)

	tool := NewWriteFileTool(nil)
	args, err := json.Marshal(WriteFileArgs{Path: path, Content: content})
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(written) != content {
		t.Fatalf("written content length = %d, want %d", len(written), len(content))
	}
	if len(output.Diffs) != 0 {
		t.Fatalf("expected no diffs for large new file, got %d: %#v", len(output.Diffs), output.Diffs)
	}
}

func TestWriteFileTool_ExistingSmallOverwriteEmitsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	oldContent := "old\ncontent\n"
	newContent := "new\ncontent\n"
	if err := os.WriteFile(path, []byte(oldContent), 0644); err != nil {
		t.Fatalf("failed to create existing file: %v", err)
	}
	expectedPath, err := resolveToolPath(path, true)
	if err != nil {
		t.Fatalf("failed to resolve expected path: %v", err)
	}

	tool := NewWriteFileTool(nil)
	args, err := json.Marshal(WriteFileArgs{Path: path, Content: newContent})
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if len(output.Diffs) != 1 {
		t.Fatalf("expected one diff, got %d: %#v", len(output.Diffs), output.Diffs)
	}
	d := output.Diffs[0]
	if d.File != expectedPath {
		t.Errorf("diff file = %q, want %q", d.File, expectedPath)
	}
	if d.Old != oldContent {
		t.Errorf("diff old = %q, want %q", d.Old, oldContent)
	}
	if d.New != newContent {
		t.Errorf("diff new = %q, want %q", d.New, newContent)
	}
	if d.Line != 1 {
		t.Errorf("diff line = %d, want 1", d.Line)
	}
	if d.Operation != "" {
		t.Errorf("diff operation = %q, want empty", d.Operation)
	}
}
