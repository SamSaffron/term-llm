package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Bubble Tea v2 models should switch on tea.KeyPressMsg for normal keyboard
// handling. In the upstream implementation tea.KeyMsg is the shared interface
// for both key press and key release events, and the bubbles/v2 key package
// examples use tea.KeyPressMsg for matching user input.
func TestBubbleTeaV2UsesKeyPressMsgForAppKeyHandling(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to locate repository root")
	}
	repoRoot := filepath.Dir(filename)

	fset := token.NewFileSet()
	var offenders []string

	walkErr := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		relPath, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		if relPath != "main.go" && !strings.HasPrefix(relPath, "cmd/") && !strings.HasPrefix(relPath, "internal/") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "tea" || sel.Sel.Name != "KeyMsg" {
				return true
			}

			pos := fset.Position(sel.Pos())
			offenders = append(offenders, fmt.Sprintf("%s:%d", relPath, pos.Line))
			return true
		})

		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repository: %v", walkErr)
	}

	if len(offenders) > 0 {
		t.Fatalf("use tea.KeyPressMsg for press-only handlers; Bubble Tea v2 defines tea.KeyMsg as the shared interface for press and release events:\n%s", strings.Join(offenders, "\n"))
	}
}
