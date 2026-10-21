// Command genpkgmap regenerates PACKAGES.md, the one-line-per-package map of
// the root module built from package doc comments.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/doc"
	"go/parser"
	"go/token"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mapFile is the generated page, relative to the repository root.
const mapFile = "PACKAGES.md"

// skipDir reports directories that never hold root-module packages.
func skipDir(name string) bool {
	switch name {
	case "node_modules", "testdata", "tmp", "vendor":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

type pkgDoc struct {
	dir, name, doc string
}

// readPackage returns the package name and doc comment of the non-test Go
// files in dir, or ok=false when dir holds no such files.
func readPackage(root, dir string) (pkgDoc, bool, error) {
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return pkgDoc{}, false, err
	}
	p := pkgDoc{dir: filepath.ToSlash(dir)}
	found := false
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || skipDir(n) {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, dir, n), nil, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			return pkgDoc{}, false, err
		}
		if buildIgnored(f) {
			continue
		}
		found = true
		if p.name == "" {
			p.name = f.Name.Name
		}
		if p.doc == "" && f.Doc != nil {
			p.doc = f.Doc.Text()
		}
	}
	return p, found, nil
}

// buildIgnored reports a `//go:build ignore` file, the convention for
// standalone generators that are not part of the directory's package.
func buildIgnored(f *ast.File) bool {
	for _, group := range f.Comments {
		if group.Pos() >= f.Package {
			break
		}
		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			return err == nil && expr.String() == "ignore"
		}
	}
	return false
}

// collect walks the root module and returns its packages in path order.
// Directories with their own go.mod are separate modules and are skipped.
func collect(root string) ([]pkgDoc, error) {
	var pkgs []pkgDoc
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "." {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
		}
		p, ok, err := readPackage(root, rel)
		if err != nil {
			return err
		}
		if ok {
			pkgs = append(pkgs, p)
		}
		return nil
	})
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].dir < pkgs[j].dir })
	return pkgs, err
}

// synopsis returns the first sentence of the doc without the conventional
// "Package name" / "Command name" prefix.
func synopsis(p pkgDoc) string {
	s := new(doc.Package).Synopsis(p.doc)
	if rest, ok := strings.CutPrefix(s, "Package "+p.name+" "); ok {
		return rest
	}
	if p.name == "main" && strings.HasPrefix(s, "Command ") {
		if fields := strings.SplitN(s, " ", 3); len(fields) == 3 {
			return fields[2]
		}
	}
	return s
}

// render builds the generated section, failing for undocumented packages so
// the map never silently omits one.
func render(pkgs []pkgDoc) (string, error) {
	var b strings.Builder
	var missing []string
	for _, p := range pkgs {
		s := synopsis(p)
		if s == "" {
			missing = append(missing, fmt.Sprintf("%s: add %s/doc.go with a `// Package %s ...` comment", p.dir, p.dir, p.name))
			continue
		}
		fmt.Fprintf(&b, "- `%s` — %s\n", p.dir, s)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("packages without a doc comment:\n%s", strings.Join(missing, "\n"))
	}
	return b.String(), nil
}

const (
	beginMarker = "<!-- BEGIN GENERATED: packages -->"
	endMarker   = "<!-- END GENERATED: packages -->"
)

// replaceSection swaps the marked section of original for content, keeping
// the surrounding prose. It does not import internal/docsgen, which links the
// embedded frontend, so this check runs without a frontend build.
func replaceSection(original, content string) (string, error) {
	if strings.Count(original, beginMarker) != 1 || strings.Count(original, endMarker) != 1 {
		return "", fmt.Errorf("%s: expected exactly one pair of packages markers", mapFile)
	}
	start := strings.Index(original, beginMarker) + len(beginMarker)
	stop := strings.Index(original, endMarker)
	if stop < start {
		return "", fmt.Errorf("%s: reversed packages markers", mapFile)
	}
	return original[:start] + "\n" + strings.TrimSpace(content) + "\n" + original[stop:], nil
}

// syncMap checks or rewrites PACKAGES.md under root.
func syncMap(root string, check bool) error {
	pkgs, err := collect(root)
	if err != nil {
		return err
	}
	content, err := render(pkgs)
	if err != nil {
		return err
	}
	path := filepath.Join(root, mapFile)
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	updated, err := replaceSection(string(original), content)
	if err != nil {
		return err
	}
	if updated == string(original) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is out of date; run make pkgmap", path)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func main() {
	check := flag.Bool("check", false, "fail instead of writing when PACKAGES.md is out of date")
	flag.Parse()
	if err := syncMap(".", *check); err != nil {
		log.Fatal(err)
	}
}
