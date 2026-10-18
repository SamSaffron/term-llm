// Command checkchanged runs the checks relevant to the files changed since the
// merge-base with a base ref (default upstream/main).
package main

import (
	"path"
	"sort"
	"strings"
)

// Pkg is the subset of `go list -json` output needed for dependency mapping.
type Pkg struct {
	ImportPath   string
	Dir          string // path relative to the module root ("." for root)
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// Step is one command to run.
type Step struct {
	Name string
	Dir  string // relative to repo root; "" means root
	Args []string
}

// Input describes a change for planning.
type Input struct {
	Files      []string // changed paths, slash-separated, relative to repo root
	All        bool
	Pkgs       []Pkg    // root-module packages
	NestedMods []string // nested module directories (relative)
	ExistsFile func(string) bool
	ModulePath string
}

// Plan is the resulting set of steps plus the package sets used.
type Plan struct {
	Steps    []Step
	Changed  []string // import paths directly changed
	Affected []string // changed plus reverse dependencies
}

func isUnder(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// ReverseDeps returns changed plus every package that transitively imports one
// (including via test imports), sorted.
func ReverseDeps(pkgs []Pkg, changed []string) []string {
	rev := map[string][]string{}
	for _, p := range pkgs {
		for _, group := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
			for _, imp := range group {
				rev[imp] = append(rev[imp], p.ImportPath)
			}
		}
	}
	seen := map[string]bool{}
	queue := append([]string(nil), changed...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		queue = append(queue, rev[cur]...)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ownerPkg maps a changed file to the root-module package dir owning it.
// Non-Go files (embedded assets, testdata) map to the nearest ancestor package.
func ownerPkg(file string, byDir map[string]Pkg) (Pkg, bool) {
	dir := path.Dir(file)
	if !strings.HasSuffix(file, ".go") {
		for dir != "." {
			if p, ok := byDir[dir]; ok {
				return p, true
			}
			dir = path.Dir(dir)
		}
		return Pkg{}, false
	}
	p, ok := byDir[dir]
	return p, ok
}

// BuildPlan maps changed files to a list of check steps.
func BuildPlan(in Input) Plan {
	var pl Plan
	byDir := map[string]Pkg{}
	for _, p := range in.Pkgs {
		byDir[p.Dir] = p
	}
	var goFiles []string
	var goChanged, sourceChanged, nestedChanged, frontend, plans, ledger, docs, unknownGo bool
	nested := map[string]bool{}
	changedPkgs := map[string]bool{}
	for _, f := range in.Files {
		if isUnder(f, "docs-site") || isUnder(f, "internal/providerspec") || isUnder(f, "internal/tools") || isUnder(f, "internal/config") || isUnder(f, "internal/docsgen") || isUnder(f, "cmd") || f == "Makefile" || isUnder(f, "scripts/checkchanged") {
			docs = true
		}
		inNested := ""
		for _, m := range in.NestedMods {
			if isUnder(f, m) {
				inNested = m
			}
		}
		switch {
		case f == "Makefile" || f == ".github/workflows/ci.yml" || f == "scripts/check-changed.sh":
			// Changes to the check orchestration itself must exercise every area.
			goChanged, unknownGo, nestedChanged, frontend, plans, ledger = true, true, true, true, true, true
		case f == "scripts/lint-plans.sh":
			plans = true
		case f == "scripts/verify_nested_modules.sh":
			nestedChanged = true
		case inNested != "":
			nested[inNested] = true
			nestedChanged = true
			if strings.HasSuffix(f, ".go") {
				sourceChanged = true
				if in.ExistsFile(f) {
					goFiles = append(goFiles, f)
				}
			}
			continue
		case isUnder(f, "frontend"):
			frontend = true
		case isUnder(f, "plans"):
			plans = true
		case isUnder(f, "ledger"):
			ledger = true
		}
		if f == "go.mod" || f == "go.sum" {
			goChanged, unknownGo = true, true
			continue
		}
		if strings.HasSuffix(f, ".go") {
			sourceChanged = true
			if in.ExistsFile(f) {
				goFiles = append(goFiles, f)
			}
		}
		if p, ok := ownerPkg(f, byDir); ok {
			goChanged = true
			changedPkgs[p.ImportPath] = true
		} else if strings.HasSuffix(f, ".go") {
			// Deleted or unlisted package (e.g. build-tagged): be conservative.
			goChanged, unknownGo = true, true
		}
	}
	if frontend {
		// The frontend is embedded by serveui. Rebuild the assets and exercise
		// its reverse dependents, not just JavaScript checks.
		if p, ok := byDir["internal/serveui"]; ok {
			goChanged = true
			changedPkgs[p.ImportPath] = true
		}
	}
	if in.All {
		goChanged, unknownGo, nestedChanged, frontend, plans, ledger, sourceChanged, docs = true, true, true, true, true, true, true, true
		for _, m := range in.NestedMods {
			nested[m] = true
		}
	}

	var targets []string
	if goChanged {
		if unknownGo {
			targets = []string{"./..."}
			for _, p := range in.Pkgs {
				pl.Affected = append(pl.Affected, p.ImportPath)
			}
			sort.Strings(pl.Affected)
		} else {
			for k := range changedPkgs {
				pl.Changed = append(pl.Changed, k)
			}
			sort.Strings(pl.Changed)
			pl.Affected = ReverseDeps(in.Pkgs, pl.Changed)
			targets = pl.Affected
		}
	}
	if in.All {
		// Match CI's repository-wide formatting check, including nested modules.
		goFiles = []string{"."}
	}
	if len(goFiles) > 0 {
		sort.Strings(goFiles)
		pl.Steps = append(pl.Steps, Step{Name: "gofmt", Args: append([]string{"gofmt", "-l"}, goFiles...)})
	}
	if goChanged {
		pl.Steps = append(pl.Steps,
			Step{Name: "make build", Args: []string{"make", "build"}},
			Step{Name: "go build", Args: []string{"go", "build", "./..."}},
			Step{Name: "go vet", Args: append([]string{"go", "vet"}, targets...)},
			Step{Name: "go test", Args: append([]string{"go", "test"}, targets...)},
		)
	}
	if docs {
		pl.Steps = append(pl.Steps, Step{Name: "docs drift", Args: []string{"go", "test", "./cmd/gendocs", "-run", "^TestReferenceDocsUpToDate$"}})
	}
	if nestedChanged {
		pl.Steps = append(pl.Steps, Step{Name: "nested modules", Args: []string{"scripts/verify_nested_modules.sh"}})
	}
	if frontend {
		for _, s := range []string{"format:check", "lint", "typecheck", "test"} {
			pl.Steps = append(pl.Steps, Step{Name: "frontend " + s, Args: []string{"npm", "--prefix", "frontend", "run", s}})
		}
		// `npm test` is a script alias; `run test` works equally.
	}
	if sourceChanged || ledger || in.All {
		pl.Steps = append(pl.Steps, Step{Name: "complexity", Args: []string{"make", "complexity"}})
	}
	if plans {
		pl.Steps = append(pl.Steps, Step{Name: "plans-lint", Args: []string{"make", "plans-lint"}})
	}
	return pl
}
