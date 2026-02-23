package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepTool_UsesFilePath(t *testing.T) {
	dir := t.TempDir()
	token := "unique_grep_token_1234567890"
	filePath := filepath.Join(dir, "sample.txt")

	if err := os.WriteFile(filePath, []byte("before "+token+" after"), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	tool := NewGrepTool(nil, DefaultOutputLimits())
	args, err := json.Marshal(GrepArgs{
		Pattern:          token,
		Path:             dir,
		FilesWithMatches: true,
	})
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(output.Content, filePath) {
		t.Fatalf("expected output to contain %q, got: %s", filePath, output.Content)
	}
}
