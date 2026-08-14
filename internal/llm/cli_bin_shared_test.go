package llm

import (
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

func TestAppendInlineFlushNotice(t *testing.T) {
	var state cliToolBridgeState
	if got := state.appendInlineFlushNotice("tool ok"); got != "tool ok" {
		t.Fatalf("unmarked output = %q", got)
	}
	state.requestInlineFlush()
	got := state.appendInlineFlushNotice("tool ok")
	if !strings.HasPrefix(got, "tool ok") || !strings.Contains(got, strings.TrimSpace(inlineFlushToolNotice)) {
		t.Fatalf("flushed output = %q", got)
	}
	if state.inlineFlushRequested() {
		t.Fatal("formatting the boundary did not consume the flush request")
	}
	if got := state.appendInlineFlushNotice("next tool"); got != "next tool" {
		t.Fatalf("second output retained one-shot flush notice: %q", got)
	}
	state.requestInlineFlush()
	state.clearInlineFlush()
	if got := state.appendInlineFlushNotice("tool ok"); got != "tool ok" {
		t.Fatalf("cleared output = %q", got)
	}
}

// TestInlineLoopCLIProvidersSupportInlineFlush pins the interjection contract
// for every CLI provider that owns its tool loop. Without a flush hook the
// engine cannot end the running prompt, so a user message queued mid-run waits
// for the whole agentic loop to finish instead of landing in the next turn.
func TestInlineLoopCLIProvidersSupportInlineFlush(t *testing.T) {
	claude := NewClaudeBinProvider("sonnet", nil)
	grok := NewGrokBinProvider("grok-4.5", nil)
	cursor := NewCursorBinProvider("auto-smart", nil)
	agy := NewAgyBinProvider("", nil)

	cases := []struct {
		name         string
		provider     Provider
		state        *cliToolBridgeState
		formatOutput func(ToolOutput) string
	}{
		{"claude_bin", claude, &claude.cliToolBridgeState, claude.formatToolOutputForClaude},
		{"grok_bin", grok, &grok.cliToolBridgeState, grok.formatToolOutput},
		{"cursor_bin", cursor, &cursor.cliToolBridgeState, cursor.formatToolOutput},
		{"agy_bin", agy, &agy.cliToolBridgeState, agy.formatToolOutput},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.provider.Capabilities().InlineToolLoop {
				t.Fatalf("%s no longer runs an inline tool loop; update this test", tc.name)
			}
			wrapped := WrapWithRetry(tc.provider, DefaultRetryConfig())
			flusher, ok := wrapped.(InlineFlusher)
			if !ok {
				t.Fatalf("%s is not reachable as an InlineFlusher through the retry wrapper", tc.name)
			}
			if !flusher.SupportsInlineFlush() {
				t.Fatalf("%s does not advertise inline flush support", tc.name)
			}
			if got := tc.formatOutput(TextOutput("tool ok")); strings.Contains(got, strings.TrimSpace(inlineFlushToolNotice)) {
				t.Fatalf("%s tool output = %q, want no flush notice before a request", tc.name, got)
			}

			flusher.RequestInlineFlush()
			if !tc.state.inlineFlushRequested() {
				t.Fatalf("%s did not mark the tool-result boundary", tc.name)
			}
			got := tc.formatOutput(TextOutput("tool ok"))
			if !strings.Contains(got, strings.TrimSpace(inlineFlushToolNotice)) {
				t.Fatalf("%s tool output = %q, want flush notice", tc.name, got)
			}

			tc.state.clearInlineFlush()
			if got := tc.formatOutput(TextOutput("tool ok")); strings.Contains(got, strings.TrimSpace(inlineFlushToolNotice)) {
				t.Fatalf("%s tool output = %q, want notice cleared for the next stream", tc.name, got)
			}
		})
	}
}

// TestInlineToolLoopProvidersDeclareInlineFlusher is the structural guard behind
// the table above. Any provider that sets InlineToolLoop owns its whole tool
// loop inside one Stream call, so it must also declare the flush hooks or a
// mid-run interjection cannot reach the model until the loop finishes on its
// own. Scanning the source catches a newly added provider that the hand-written
// table would silently miss.
func TestInlineToolLoopProvidersDeclareInlineFlusher(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(thisFile), "*.go"))
	if err != nil {
		t.Fatalf("glob provider files: %v", err)
	}

	methods := map[string]map[string]bool{}
	inlineLoopTypes := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			recv := receiverTypeName(fn.Recv.List[0].Type)
			if recv == "" {
				continue
			}
			if methods[recv] == nil {
				methods[recv] = map[string]bool{}
			}
			methods[recv][fn.Name.Name] = true
			if fn.Name.Name != "Capabilities" || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				kv, ok := node.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "InlineToolLoop" {
					return true
				}
				if value, ok := kv.Value.(*ast.Ident); ok && value.Name == "true" {
					inlineLoopTypes[recv] = true
				}
				return true
			})
		}
	}

	if len(inlineLoopTypes) == 0 {
		t.Fatal("found no InlineToolLoop providers; the source scan is no longer matching")
	}
	for typeName := range inlineLoopTypes {
		for _, method := range []string{"RequestInlineFlush", "SupportsInlineFlush"} {
			if !methods[typeName][method] {
				t.Errorf("%s sets InlineToolLoop but does not declare %s; queued interjections would stall until its inline loop ends", typeName, method)
			}
		}
	}
}

func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

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
