package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The failure guard must be registered after the HTTP dependencies' defers.
// Checking the registration order covers every post-Start return, including
// Hub registration and reload binding, without introducing lifecycle test hooks.
func TestServeFailedStartupStopsHTTPBeforeDependencies(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "serve_start.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stop, start token.Pos
	dependencies := map[string]token.Pos{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.DeferStmt:
			ast.Inspect(n.Call, func(child ast.Node) bool {
				call, ok := child.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == "stopWidgets" || fun.Name == "closeLiveDecisionStore" || fun.Name == "storeCleanup" {
						dependencies[fun.Name] = n.Pos()
					}
				case *ast.SelectorExpr:
					if fun.Sel.Name == "CloseContext" {
						dependencies["session manager"] = n.Pos()
					}
					if fun.Sel.Name == "Stop" {
						stop = n.Pos()
					}
				}
				return true
			})
		case *ast.CallExpr:
			if fun, ok := n.Fun.(*ast.SelectorExpr); ok && fun.Sel.Name == "Start" {
				start = n.Pos()
			}
		}
		return true
	})
	if len(dependencies) != 4 || stop == token.NoPos || start == token.NoPos {
		t.Fatalf("missing lifecycle registrations: dependencies=%v stop=%v start=%v", dependencies, stop, start)
	}
	for name, pos := range dependencies {
		if pos >= stop || stop >= start {
			t.Errorf("HTTP stop defer must follow %s cleanup and precede Start", name)
		}
	}
}
