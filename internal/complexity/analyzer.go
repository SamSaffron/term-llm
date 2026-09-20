// Package complexity provides the repository's versioned complexity analysis.
// Its counting rules deliberately match plans/go-complexity-reduction.md: one
// plus a nesting-weighted increment for every branch, loop, switch and select,
// one for each else or else-if, one for each sequence of logical operators, and
// one for each labeled branch. A switch or select costs the same whether it has
// two cases or twenty, because flat dispatch is read one case at a time. Each
// function literal is measured as its own unit named <enclosing>.funcN, so
// callback-shaped code is attributed where it is actually read.
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
	return Report{CountingRules: CountingRules, Files: files, Functions: out}, nil
}

// CountingRules describes the measure emitted in every report.
const CountingRules = "1 + (1 + nesting depth) per if/for/range/switch/select + 1 per else or else-if + 1 per logical operator sequence + 1 per labeled break/continue/goto; a guard clause that exits is flat and does not deepen its body; switch and select count once regardless of case count; each function literal is measured separately as <immediate parent>.funcN"

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
			out = append(out, measure(fset, file.Name.Name, path, module, receiverName(fset, d.Recv), d.Name.Name, d.Body, fileLines, test)...)
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
					out = append(out, measure(fset, file.Name.Name, path, module, "", name, lit.Body, fileLines, test)...)
				}
			}
		}
	}
	return out, File{Module: module, Package: file.Name.Name, Path: path, Lines: fileLines, Test: test}, nil
}

// measure scores a declaration body and every function literal it contains.
// Literals are separate units because a callback is read on its own terms, not
// as extra branching in the function that happens to pass it.
func measure(fset *token.FileSet, pkg, path, module, receiver, name string, body *ast.BlockStmt, fileLines int, test bool) []Function {
	var out []Function
	pending := []pendingUnit{{name: name, body: body}}
	for len(pending) > 0 {
		unit := pending[0]
		pending = pending[1:]
		w := walker{complexity: 1}
		w.stmts(unit.body.List, 0)
		// Number literals within their own parent so that a literal nested in a
		// literal keeps its identity when a sibling is added or removed.
		for i, lit := range w.literals {
			pending = append(pending, pendingUnit{name: fmt.Sprintf("%s.func%d", unit.name, i+1), body: lit.Body})
		}
		out = append(out, Function{
			Module:     module,
			Package:    pkg,
			Path:       path,
			Receiver:   receiver,
			Name:       unit.name,
			Complexity: w.complexity,
			Lines:      fset.Position(unit.body.End()).Line - fset.Position(unit.body.Pos()).Line + 1,
			FileLines:  fileLines,
			Test:       test,
		})
	}
	return out
}

type pendingUnit struct {
	name string
	body *ast.BlockStmt
}

// walker scores one function body. Nesting is the number of enclosing control
// structures within the same body; function literals are collected rather than
// scored, so each one restarts at nesting zero.
type walker struct {
	complexity int
	literals   []*ast.FuncLit
}

func (w *walker) stmts(list []ast.Stmt, nesting int) {
	for _, stmt := range list {
		w.stmt(stmt, nesting)
	}
}

// block walks a body that the grammar always supplies, without trusting it to
// be present.
func (w *walker) block(body *ast.BlockStmt, nesting int) {
	if body == nil {
		return
	}
	w.stmts(body.List, nesting)
}

func (w *walker) stmt(stmt ast.Stmt, nesting int) {
	switch n := stmt.(type) {
	case nil:
		return
	case *ast.IfStmt:
		w.ifStmt(n, nesting, false)
	case *ast.ForStmt:
		w.complexity += 1 + nesting
		w.stmt(n.Init, nesting)
		w.expr(n.Cond)
		w.stmt(n.Post, nesting)
		w.block(n.Body, nesting+1)
	case *ast.RangeStmt:
		w.complexity += 1 + nesting
		w.expr(n.X)
		w.block(n.Body, nesting+1)
	case *ast.SwitchStmt:
		w.complexity += 1 + nesting
		w.stmt(n.Init, nesting)
		w.expr(n.Tag)
		w.clauses(n.Body, nesting)
	case *ast.TypeSwitchStmt:
		w.complexity += 1 + nesting
		w.stmt(n.Init, nesting)
		w.stmt(n.Assign, nesting)
		w.clauses(n.Body, nesting)
	case *ast.SelectStmt:
		w.complexity += 1 + nesting
		w.clauses(n.Body, nesting)
	case *ast.LabeledStmt:
		w.stmt(n.Stmt, nesting)
	case *ast.BranchStmt:
		if n.Label != nil || n.Tok == token.GOTO {
			w.complexity++ // Non-local control flow is a real jump to follow.
		}
	case *ast.BlockStmt:
		w.stmts(n.List, nesting)
	default:
		w.expr(stmt) // Remaining statements only carry expressions.
	}
}

// clauses scores the bodies of switch, type switch and select cases. The cases
// themselves are free: the statement already paid for the dispatch.
func (w *walker) clauses(body *ast.BlockStmt, nesting int) {
	if body == nil {
		return
	}
	for _, clause := range body.List {
		switch c := clause.(type) {
		case *ast.CaseClause:
			for _, expr := range c.List {
				w.expr(expr)
			}
			w.stmts(c.Body, nesting+1)
		case *ast.CommClause:
			w.stmt(c.Comm, nesting+1)
			w.stmts(c.Body, nesting+1)
		}
	}
}

// ifStmt scores a branch. An else-if is charged flat and keeps the chain's
// nesting level, because a chain reads as one decision, not as growing depth.
// A guard that exits is also charged flat and does not deepen its body: an
// early return discharges a case instead of asking the reader to hold one.
func (w *walker) ifStmt(n *ast.IfStmt, nesting int, elseIf bool) {
	guard := guardClause(n)
	if elseIf || guard {
		w.complexity++
	} else {
		w.complexity += 1 + nesting
	}
	body := nesting + 1
	if guard {
		body = nesting
	}
	w.stmt(n.Init, nesting)
	w.expr(n.Cond)
	w.block(n.Body, body)
	switch other := n.Else.(type) {
	case nil:
	case *ast.IfStmt:
		w.ifStmt(other, nesting, true)
	default:
		w.complexity++
		w.stmt(other, nesting+1)
	}
}

// guardClause reports whether a branch only leaves: no else, and a body whose
// last statement returns, jumps or panics.
func guardClause(n *ast.IfStmt) bool {
	if n.Else != nil || n.Body == nil || len(n.Body.List) == 0 {
		return false
	}
	switch last := n.Body.List[len(n.Body.List)-1].(type) {
	case *ast.ReturnStmt, *ast.BranchStmt:
		return true
	case *ast.ExprStmt:
		call, ok := last.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		fn, ok := call.Fun.(*ast.Ident)
		return ok && fn.Name == "panic"
	default:
		return false
	}
}

// expr walks an expression (or an expression-only statement), collecting
// function literals and charging one per sequence of logical operators.
func (w *walker) expr(node ast.Node) {
	if node == nil {
		return
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			w.literals = append(w.literals, x)
			return false
		case *ast.BinaryExpr:
			if isLogical(x.Op) {
				w.logical(x)
				return false
			}
		}
		return true
	})
}

// logical charges one per run of the same operator, so a multi-clause guard
// costs one and a mixed condition costs one per alternation.
func (w *walker) logical(root *ast.BinaryExpr) {
	last := token.ILLEGAL
	var visit func(ast.Expr)
	visit = func(e ast.Expr) {
		if b, ok := unparen(e).(*ast.BinaryExpr); ok && isLogical(b.Op) {
			visit(b.X)
			if b.Op != last {
				w.complexity++
				last = b.Op
			}
			visit(b.Y)
			return
		}
		w.expr(e)
	}
	visit(root)
}

func isLogical(op token.Token) bool { return op == token.LAND || op == token.LOR }

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
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
