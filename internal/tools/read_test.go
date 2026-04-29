package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileToolLineRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\ndelta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadFileTool(nil, DefaultOutputLimits())
	args, _ := json.Marshal(ReadFileArgs{Path: path, StartLine: 2, EndLine: 3})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	want := "2: beta\n3: gamma"
	if out.Content != want {
		t.Fatalf("unexpected output:\nwant %q\n got %q", want, out.Content)
	}
}

func TestReadFileToolPreservesTrailingEmptyLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadFileTool(nil, DefaultOutputLimits())
	args, _ := json.Marshal(ReadFileArgs{Path: path})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	want := "1: alpha\n2: "
	if out.Content != want {
		t.Fatalf("unexpected output:\nwant %q\n got %q", want, out.Content)
	}
}

func TestReadFileToolStartLineBeyondEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadFileTool(nil, DefaultOutputLimits())
	args, _ := json.Marshal(ReadFileArgs{Path: path, StartLine: 4})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	want := "Error [INVALID_PARAMS]: start_line 4 exceeds file length 2"
	if out.Content != want {
		t.Fatalf("unexpected output:\nwant %q\n got %q", want, out.Content)
	}
}

func TestReadFileToolTruncatesWithTotalLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour"), 0o644); err != nil {
		t.Fatal(err)
	}

	limits := DefaultOutputLimits()
	limits.MaxLines = 2
	tool := NewReadFileTool(nil, limits)
	args, _ := json.Marshal(ReadFileArgs{Path: path})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	want := "1: one\n2: two\n\n[Output truncated. Total lines: 4. Use start_line/end_line for pagination.]"
	if out.Content != want {
		t.Fatalf("unexpected output:\nwant %q\n got %q", want, out.Content)
	}
}

func TestStreamLineNumberedRangeMatchesLegacyFormatting(t *testing.T) {
	baseLimits := DefaultOutputLimits()
	smallLineLimit := baseLimits
	smallLineLimit.MaxLines = 2
	smallByteLimit := baseLimits
	smallByteLimit.MaxBytes = 10

	tests := []struct {
		name      string
		content   string
		startLine int
		endLine   int
		limits    OutputLimits
	}{
		{name: "empty file", content: "", limits: baseLimits},
		{name: "trailing newline", content: "alpha\n", limits: baseLimits},
		{name: "range", content: "alpha\nbeta\ngamma\ndelta\n", startLine: 2, endLine: 3, limits: baseLimits},
		{name: "end beyond eof", content: "alpha\nbeta", startLine: 1, endLine: 10, limits: baseLimits},
		{name: "start after end", content: "alpha\nbeta\ngamma\ndelta", startLine: 4, endLine: 2, limits: baseLimits},
		{name: "start beyond eof", content: "alpha\nbeta", startLine: 4, limits: baseLimits},
		{name: "line truncated", content: "one\ntwo\nthree\nfour", limits: smallLineLimit},
		{name: "byte truncated", content: "abcdefghij\nklmnop", limits: smallByteLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, wantErr := legacyReadFileFormatting(tt.content, tt.startLine, tt.endLine, tt.limits)
			got, gotErr := streamLineNumberedRange(context.Background(), bufio.NewReader(strings.NewReader(tt.content)), tt.startLine, tt.endLine, tt.limits)
			if (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("error mismatch: got %v want %v", gotErr, wantErr)
			}
			if gotErr != nil && gotErr.Error() != wantErr.Error() {
				t.Fatalf("error mismatch: got %v want %v", gotErr, wantErr)
			}
			if got != want {
				t.Fatalf("output mismatch:\nwant %q\n got %q", want, got)
			}
		})
	}
}

func legacyReadFileFormatting(content string, startLine, endLine int, limits OutputLimits) (string, error) {
	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	start := 0
	if startLine > 0 {
		start = startLine - 1
	}
	if start >= totalLines {
		return "", NewToolErrorf(ErrInvalidParams, "start_line %d exceeds file length %d", startLine, totalLines)
	}

	end := totalLines
	if endLine > 0 && endLine < totalLines {
		end = endLine
	}

	if start >= end {
		return "No content in requested range.", nil
	}

	selectedLines := lines[start:end]

	truncated := false
	if len(selectedLines) > limits.MaxLines {
		selectedLines = selectedLines[:limits.MaxLines]
		truncated = true
	}

	var sb strings.Builder
	for i, line := range selectedLines {
		lineNum := start + i + 1
		sb.WriteString(fmt.Sprintf("%d: %s\n", lineNum, line))
	}

	output := strings.TrimSuffix(sb.String(), "\n")

	if int64(len(output)) > limits.MaxBytes {
		output = output[:limits.MaxBytes]
		truncated = true
	}

	if truncated {
		output += fmt.Sprintf("\n\n[Output truncated. Total lines: %d. Use start_line/end_line for pagination.]", totalLines)
	}

	return output, nil
}

func BenchmarkReadFileToolStartRangeLargeFile(b *testing.B) {
	path := filepath.Join(b.TempDir(), "large.txt")
	file, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	linePayload := strings.Repeat("x", 96)
	for i := 0; i < 200_000; i++ {
		if _, err := fmt.Fprintf(file, "line %06d %s\n", i, linePayload); err != nil {
			b.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}

	tool := NewReadFileTool(nil, DefaultOutputLimits())
	args, _ := json.Marshal(ReadFileArgs{Path: path, StartLine: 1, EndLine: 20})

	b.ReportAllocs()
	b.SetBytes(info.Size())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			b.Fatalf("Execute returned error: %v", err)
		}
		if !strings.Contains(out.Content, "20: line 000019") {
			b.Fatalf("range output missing final line: %q", out.Content)
		}
	}
}
