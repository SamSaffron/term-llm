package llm

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
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
	tests := []struct {
		name       string
		workingDir string
		want       string
	}{
		{name: "directory", workingDir: "/tmp/project", want: "/tmp/project"},
		{name: "trimmed", workingDir: "  /tmp/project  ", want: "/tmp/project"},
		{name: "empty inherits process directory"},
		{name: "whitespace inherits process directory", workingDir: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newCLICommand(context.Background(), "test-binary", nil, tt.workingDir)
			if cmd.Dir != tt.want {
				t.Fatalf("Dir = %q, want %q", cmd.Dir, tt.want)
			}
		})
	}
}
