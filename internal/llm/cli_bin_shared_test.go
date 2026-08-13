package llm

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCLIProvidersUseSharedCommandConstructor(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(thisFile), "*_bin.go"))
	if err != nil {
		t.Fatalf("glob CLI provider files: %v", err)
	}
	for _, path := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "CommandContext" {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if ok && pkg.Name == "exec" {
				t.Errorf("%s invokes exec.CommandContext directly; use newCLICommand so Request.WorkingDir is applied", filepath.Base(path))
			}
			return true
		})
	}
}

func TestNewCLICommandAppliesWorkingDirectoryPolicy(t *testing.T) {
	workingDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	relativeDir, err := filepath.Rel(cwd, workingDir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	tests := []struct {
		name       string
		workingDir string
		want       string
	}{
		{name: "directory", workingDir: workingDir, want: workingDir},
		{name: "relative directory", workingDir: relativeDir, want: workingDir},
		{name: "trimmed", workingDir: "  " + workingDir + "  ", want: workingDir},
		{name: "empty inherits process directory"},
		{name: "whitespace inherits process directory", workingDir: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := newCLICommand(context.Background(), "test-binary", nil, tt.workingDir)
			if err != nil {
				t.Fatalf("newCLICommand: %v", err)
			}
			if cmd.Dir != tt.want {
				t.Fatalf("Dir = %q, want %q", cmd.Dir, tt.want)
			}
		})
	}
}

func TestDrainCLIDiagnosticLinesDrainsOversizedLineAndUnterminatedTail(t *testing.T) {
	oversizedBytes := cliDiagnosticLineReadMaxBytes + 2*1024*1024
	input := bytes.NewReader(append(bytes.Repeat([]byte("x"), oversizedBytes), []byte("\nshort\r\nfinal")...))
	var lines []string
	if err := drainCLIDiagnosticLines(input, func(line string) {
		lines = append(lines, line)
	}); err != nil {
		t.Fatalf("drainCLIDiagnosticLines: %v", err)
	}
	if input.Len() != 0 {
		t.Fatalf("reader has %d bytes left; diagnostics were not fully drained", input.Len())
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %#v, want oversized marker plus two lines", lines)
	}
	if !strings.Contains(lines[0], "oversized diagnostic line omitted") || strings.Contains(lines[0], "x") {
		t.Fatalf("oversized line marker = %q", lines[0])
	}
	if lines[1] != "short" || lines[2] != "final" {
		t.Fatalf("remaining lines = %#v, want [short final]", lines[1:])
	}
}

func TestNewCLICommandRejectsInvalidWorkingDirectory(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), filePath} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cmd, err := newCLICommand(context.Background(), "test-binary", nil, path)
			if err == nil {
				t.Fatalf("newCLICommand(%q) = %+v, want error", path, cmd)
			}
			if !strings.Contains(err.Error(), "working directory") {
				t.Fatalf("error = %q, want working directory context", err)
			}
		})
	}
}
