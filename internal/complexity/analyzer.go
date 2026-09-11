// Package complexity provides the repository's versioned cyclomatic-complexity
// analysis. Its counting rules deliberately match plans/go-complexity-reduction.md:
// one plus if/for/range/non-default switch or select cases and &&/||. Decisions
// in function literals are attributed to the nearest named declaration (or to a
// package-level function-valued initializer), so the scope is stable over time.
package complexity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Function identifies and measures a function or method.
type Function struct {
	Module     string `json:"module"`
	Package    string `json:"package"`
	Path       string `json:"path"`
	Receiver   string `json:"receiver,omitempty"`
	Name       string `json:"name"`
	Complexity int    `json:"complexity"`
	Lines      int    `json:"lines"`
	FileLines  int    `json:"file_lines"`
	Test       bool   `json:"test,omitempty"`
}

// Report is stable, machine-readable analysis output.
type Report struct {
	CountingRules string     `json:"counting_rules"`
	Files         []File     `json:"files"`
	Functions     []Function `json:"functions"`
}

// File records every analyzed source file, including files without functions.
type File struct {
	Module  string `json:"module"`
	Package string `json:"package"`
	Path    string `json:"path"`
	Lines   int    `json:"lines"`
	Test    bool   `json:"test,omitempty"`
}

// Options controls repository traversal.
type Options struct {
	IncludeTests bool
}

// Analyze walks root, including nested modules and platform-specific source.
func Analyze(root string, opts Options) (Report, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Report{}, err
	}
	modules, err := moduleRoots(root)
	if err != nil {
		return Report{}, err
	}
	var out []Function
	var files []File
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && excludedDir(rel, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || (!opts.IncludeTests && strings.HasSuffix(path, "_test.go")) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if generated(data) {
			return nil
		}
		functions, analyzedFile, err := analyzeFile(data, rel, moduleFor(rel, modules), strings.HasSuffix(path, "_test.go"))
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		files = append(files, analyzedFile)
		out = append(out, functions...)
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Receiver != b.Receiver {
			return a.Receiver < b.Receiver
		}
		return a.Name < b.Name
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return Report{CountingRules: "1 + if + for + range + non-default switch/select cases + && + ||; nested function literals attributed to enclosing declaration", Files: files, Functions: out}, nil
}

func analyzeFile(src []byte, path, module string, test bool) ([]Function, File, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, File{}, err
	}
	fileLines := bytes.Count(src, []byte("\n")) + 1
	var out []Function
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			out = append(out, measure(fset, file.Name.Name, path, module, receiverName(fset, d.Recv), d.Name.Name, d.Body, fileLines, test))
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, value := range vs.Values {
					lit, ok := value.(*ast.FuncLit)
					if !ok {
						continue
					}
					name := "initializer"
					if i < len(vs.Names) {
						name = vs.Names[i].Name
					}
					out = append(out, measure(fset, file.Name.Name, path, module, "", name, lit.Body, fileLines, test))
				}
			}
		}
	}
	return out, File{Module: module, Package: file.Name.Name, Path: path, Lines: fileLines, Test: test}, nil
}

func measure(fset *token.FileSet, pkg, path, module, receiver, name string, body *ast.BlockStmt, fileLines int, test bool) Function {
	complexity := 1
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			complexity++
		case *ast.CaseClause:
			if n.List != nil {
				complexity++
			}
		case *ast.CommClause:
			if n.Comm != nil {
				complexity++
			}
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				complexity++
			}
		}
		return true // Nested literals intentionally count toward their owner.
	})
	return Function{Module: module, Package: pkg, Path: path, Receiver: receiver, Name: name, Complexity: complexity, Lines: fset.Position(body.End()).Line - fset.Position(body.Pos()).Line + 1, FileLines: fileLines, Test: test}
}

func receiverName(fset *token.FileSet, recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, recv.List[0].Type)
	return b.String()
}

func moduleRoots(root string) ([]string, error) {
	var roots []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && excludedDir(rel, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			dir := filepath.ToSlash(filepath.Dir(rel))
			if dir == "." {
				dir = "."
			}
			roots = append(roots, dir)
		}
		return nil
	})
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	return roots, err
}

func moduleFor(path string, roots []string) string {
	for _, root := range roots {
		if root == "." || path == root || strings.HasPrefix(path, root+"/") {
			return root
		}
	}
	return "."
}

func excludedDir(rel, name string) bool {
	if name == ".git" || name == ".cache" || name == "vendor" || name == "node_modules" || name == "tmp" || name == "testdata" {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	return rel == "internal/serveui/static/dist" || strings.HasPrefix(rel, "internal/serveui/static/dist/")
}

func generated(src []byte) bool {
	header := src
	if i := bytes.Index(src, []byte("package ")); i >= 0 {
		header = src[:i]
	}
	return bytes.Contains(header, []byte("Code generated")) && bytes.Contains(header, []byte("DO NOT EDIT."))
}

// Encode writes a reproducibly formatted report.
func Encode(report Report) ([]byte, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Key is the stable baseline identity of a function.
func Key(f Function) string {
	return f.Module + "|" + f.Path + "|" + f.Package + "|" + f.Receiver + "|" + f.Name
}
