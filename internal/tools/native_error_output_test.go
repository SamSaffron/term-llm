package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
)

func TestNativeToolInvalidArgsAreErrors(t *testing.T) {
	tools := []core.Tool{
		NewReadFileTool(nil, DefaultOutputLimits()), NewWriteFileTool(nil), NewEditFileTool(nil),
		NewGlobTool(nil), NewGrepTool(nil, DefaultOutputLimits()),
		NewViewImageTool(nil), NewShowImageTool(nil), NewShowMediaTool(nil),
	}
	for _, tool := range tools {
		t.Run(tool.Spec().Name, func(t *testing.T) {
			for _, args := range []string{`{}`, `{"path":`, `{"path":42,"file_path":42,"pattern":42}`} {
				out, err := tool.Execute(context.Background(), json.RawMessage(args))
				if err != nil || !out.IsError || !strings.Contains(out.Content, "Error [INVALID_PARAMS]:") {
					t.Fatalf("Execute(%s) = %+v, %v; want invalid-params tool error", args, out, err)
				}
			}
		})
	}
}

func TestNativeToolFailuresAreErrors(t *testing.T) {
	dir := t.TempDir()
	textPath := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(textPath, []byte("original text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(dir, "binary.bin")
	if err := os.WriteFile(binaryPath, []byte{0, 1, 2}, 0600); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(dir, "sample.png")
	writeTestPNG(t, imagePath)
	missing := filepath.Join(dir, "missing.png")
	vision := mockprovider.NewMockProvider("vision").AddError(errors.New("vision unavailable"))
	tests := []struct {
		name string
		tool core.Tool
		args map[string]any
		want string
	}{
		{"read missing", NewReadFileTool(nil, DefaultOutputLimits()), map[string]any{"path": missing}, "FILE_NOT_FOUND"},
		{"read binary", NewReadFileTool(nil, DefaultOutputLimits()), map[string]any{"path": binaryPath}, "BINARY_FILE"},
		{"read range", NewReadFileTool(nil, DefaultOutputLimits()), map[string]any{"path": textPath, "start_line": 100}, "INVALID_PARAMS"},
		{"write directory", NewWriteFileTool(nil), map[string]any{"path": dir, "content": "x"}, "not a regular file"},
		{"write invalid parent", NewWriteFileTool(nil), map[string]any{"path": filepath.Join(textPath, "child"), "content": "x"}, "Error ["},
		{"edit missing", NewEditFileTool(nil), map[string]any{"path": missing, "old_text": "old", "new_text": "new"}, "FILE_NOT_FOUND"},
		{"edit no match", NewEditFileTool(nil), map[string]any{"path": textPath, "old_text": "absent", "new_text": "new"}, "could not find old_text"},
		{"edit no mode", NewEditFileTool(nil), map[string]any{"path": textPath}, "provide either"},
		{"edit mixed modes", NewEditFileTool(nil), map[string]any{"path": textPath, "old_text": "original", "instructions": "replace"}, "cannot mix"},
		{"edit instructions", NewEditFileTool(nil), map[string]any{"path": textPath, "instructions": "replace"}, "instructions mode requires"},
		{"glob missing", NewGlobTool(nil), map[string]any{"path": missing, "pattern": "*"}, "FILE_NOT_FOUND"},
		{"glob invalid pattern", NewGlobTool(nil), map[string]any{"path": dir, "pattern": "["}, "Error ["},
		{"glob traversal", NewGlobTool(nil), map[string]any{"path": dir, "pattern": "../*"}, "invalid pattern"},
		{"grep missing", NewGrepTool(nil, DefaultOutputLimits()), map[string]any{"path": missing, "pattern": "x"}, "FILE_NOT_FOUND"},
		{"grep invalid regex", NewGrepTool(nil, DefaultOutputLimits()), map[string]any{"path": dir, "pattern": "["}, "invalid regex pattern"},
		{"view missing", NewViewImageTool(nil), map[string]any{"file_path": missing}, "FILE_NOT_FOUND"},
		{"view unsupported", NewViewImageTool(nil), map[string]any{"file_path": textPath}, "UNSUPPORTED_FORMAT"},
		{"view directory", NewViewImageTool(nil), map[string]any{"file_path": dir}, "failed to read image"},
		{"view invalid region", NewViewImageTool(nil), map[string]any{"file_path": imagePath, "region": "invalid"}, "unsupported region"},
		{"view vision failure", NewViewImageToolWithVision(nil, vision, "vision-model"), map[string]any{"file_path": imagePath}, "vision analysis failed"},
		{"show image missing", NewShowImageTool(nil), map[string]any{"file_path": missing}, "FILE_NOT_FOUND"},
		{"show image unsupported", NewShowImageTool(nil), map[string]any{"file_path": textPath}, "UNSUPPORTED_FORMAT"},
		{"show media missing", NewShowMediaTool(nil), map[string]any{"path": missing}, "FILE_NOT_FOUND"},
		{"show media directory", NewShowMediaTool(nil), map[string]any{"path": dir}, "regular file"},
		{"show media unsupported", NewShowMediaTool(nil), map[string]any{"path": textPath}, "UNSUPPORTED_FORMAT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			out, err := tt.tool.Execute(context.Background(), raw)
			if err != nil || !out.IsError || !strings.Contains(out.Content, tt.want) {
				t.Fatalf("Execute() = %+v, %v; want IsError and %q", out, err, tt.want)
			}
		})
	}
}

func TestNativeToolWarningsPreserveStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	const content = "Error [EXECUTION_FAILED]: this is file content, not a tool failure"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		tool core.Tool
		args map[string]any
		want string
	}{
		{NewReadFileTool(nil, DefaultOutputLimits()), map[string]any{"path": path}, "1: " + content},
		{NewWriteFileTool(nil), map[string]any{"path": filepath.Join(dir, "new.txt"), "content": content}, "Created new file:"},
		{NewEditFileTool(nil), map[string]any{"path": path, "old_text": "file content", "new_text": "ordinary content"}, "Edited "},
		{NewGlobTool(nil), map[string]any{"path": dir, "pattern": "absent.*"}, "No files matched the pattern."},
		{NewGrepTool(nil, DefaultOutputLimits()), map[string]any{"path": dir, "pattern": "absent"}, "No matches found."},
	}
	for _, tt := range tests {
		t.Run(tt.tool.Spec().Name, func(t *testing.T) {
			tt.args["unknown"] = true
			raw, _ := json.Marshal(tt.args)
			out, err := tt.tool.Execute(context.Background(), raw)
			const warning = "Unknown parameter 'unknown' was ignored\n"
			if err != nil || out.IsError || !strings.HasPrefix(out.Content, warning) || !strings.Contains(out.Content, tt.want) {
				t.Fatalf("successful Execute() = %+v, %v; want warning and %q", out, err, tt.want)
			}
			out, err = tt.tool.Execute(context.Background(), json.RawMessage(`{"unknown":true}`))
			if err != nil || !out.IsError || !strings.HasPrefix(out.Content, warning+"Error [INVALID_PARAMS]:") {
				t.Fatalf("failed Execute() = %+v, %v; want warning and error flag", out, err)
			}
		})
	}
}

func TestGrepHelperFailuresAreErrors(t *testing.T) {
	requireGoFallback(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("needle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, intent := range []string{"", "find relevant matches"} {
		t.Run("intent="+intent, func(t *testing.T) {
			tool, _ := newIntentTool(t, newScoringFake(), nil)
			for _, tt := range []struct {
				pattern string
				path    string
				want    string
			}{
				{"[", dir, "invalid regex pattern"},
				{"needle", filepath.Join(dir, "missing"), "failed to collect files"},
			} {
				out := tool.executeGrep(context.Background(), GrepArgs{Pattern: tt.pattern}, tt.path, intent, 2, 20)
				if !out.IsError || !strings.Contains(out.Content, tt.want) {
					t.Fatalf("executeGrep(%q, %q) = %+v; want failed search", tt.pattern, tt.path, out)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			out := tool.executeGrep(ctx, GrepArgs{Pattern: "needle"}, dir, intent, 2, 20)
			if !out.IsError || !strings.Contains(out.Content, "grep timed out") {
				t.Fatalf("canceled search = %+v; want error", out)
			}
		})
	}
}

func TestGrepIntentExecutePreservesStatus(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, .9, .8)
	tool, _ := newIntentTool(t, failingFake(errors.New("classifier unavailable")), nil)
	for _, tt := range []struct {
		pattern string
		want    string
		isError bool
	}{
		{"[", "invalid regex pattern", true},
		{"absent", "No matches found.", false},
		{"needle", "intent rerank skipped", false},
	} {
		raw, _ := json.Marshal(map[string]any{"path": dir, "pattern": tt.pattern, "intent": "find matches", "unknown": true})
		out, err := tool.Execute(context.Background(), raw)
		if err != nil || out.IsError != tt.isError || !strings.Contains(out.Content, tt.want) || !strings.HasPrefix(out.Content, "Unknown parameter 'unknown' was ignored\n") {
			t.Fatalf("Execute(%q) = %+v, %v; want IsError=%v and %q", tt.pattern, out, err, tt.isError, tt.want)
		}
	}
}

func TestPathApprovalErrorsAlwaysFail(t *testing.T) {
	for _, err := range []error{
		NewToolError(ErrPermissionDenied, "denied"),
		NewToolError(ErrInvalidParams, "invalid path"),
		errors.New("approval failed"),
	} {
		out := pathApprovalErrorOutput("warning\n", err)
		if !out.IsError || !strings.HasPrefix(out.Content, "warning\nError [") {
			t.Fatalf("approval error output = %+v; want warning and error", out)
		}
	}
}
