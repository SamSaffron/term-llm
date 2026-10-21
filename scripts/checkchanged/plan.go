// Command checkchanged runs the checks relevant to the files changed since the
// merge-base with a base ref (default upstream/main, origin/main, then main).
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
	// NestedModPaths maps a nested module directory to its module path, so
	// root packages importing it can be treated as changed.
	NestedModPaths map[string]string
	ExistsFile     func(string) bool
	ModulePath     string
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

// importsModule reports whether p directly imports a package of module mod.
func importsModule(p Pkg, mod string) bool {
	for _, group := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
		for _, imp := range group {
			if isUnder(imp, mod) {
				return true
			}
		}
	}
	return false
}

// state accumulates what the changed files touch.
type state struct {
	goFiles                                                                []string
	goChanged, sourceChanged, nestedChanged, frontend, plans, ledger, docs bool
	unknownGo, apiTypes, pkgMap                                            bool
	nested                                                                 map[string]bool
	changedPkgs                                                            map[string]bool
}

func (st *state) addGoFile(f string, in Input) {
	st.sourceChanged = true
	if in.ExistsFile(f) {
		st.goFiles = append(st.goFiles, f)
	}
}

func nestedOwner(f string, mods []string) string {
	owner := ""
	for _, m := range mods {
		if isUnder(f, m) {
			owner = m
		}
	}
	return owner
}

// apiTypeSources are the Go trees whose JSON contracts internal/tsgen reads.
// tsgen parses source rather than importing these packages, so reverse
// dependencies alone do not select its drift test.
var apiTypeSources = []string{"internal/app/serve", "internal/serve", "internal/session", "internal/share", "internal/hub", "internal/llm/core", "internal/plan", "internal/tools"}

func affectsAPITypes(f string) bool {
	if isUnder(f, "frontend/src/api") || isUnder(f, "internal/tsgen") || isUnder(f, "cmd/gentsapi") {
		return true
	}
	if !strings.HasSuffix(f, ".go") {
		return false
	}
	if isUnder(f, "cmd") && strings.HasPrefix(path.Base(f), "serve") {
		return true
	}
	for _, dir := range apiTypeSources {
		if isUnder(f, dir) {
			return true
		}
	}
	return false
}

// classify records the effect of one changed file.
func (st *state) classify(f string, in Input, byDir map[string]Pkg) {
	if isUnder(f, "docs-site") {
		st.docs = true
	}
	if affectsAPITypes(f) {
		st.apiTypes = true
	}
	if path.Base(f) == "go.mod" {
		// Adding or removing a nested module changes the root package set.
		st.pkgMap = true
	}
	if m := nestedOwner(f, in.NestedMods); m != "" {
		st.nested[m] = true
		st.nestedChanged = true
		if strings.HasSuffix(f, ".go") {
			st.addGoFile(f, in)
		}
		return
	}
	switch {
	case f == "Makefile" || f == ".github/workflows/ci.yml" || f == "scripts/check-changed.sh":
		// Changes to the check orchestration itself must exercise every area.
		st.goChanged, st.unknownGo, st.nestedChanged, st.frontend, st.plans, st.ledger = true, true, true, true, true, true
	case f == "scripts/lint-plans.sh":
		st.plans = true
	case f == "scripts/verify_nested_modules.sh":
		st.nestedChanged = true
	case isUnder(f, "frontend"):
		st.frontend = true
	case isUnder(f, "plans"):
		st.plans = true
	case isUnder(f, "ledger"):
		st.ledger = true
	}
	if f == "go.mod" || f == "go.sum" {
		st.goChanged, st.unknownGo = true, true
		return
	}
	if f == "PACKAGES.md" {
		st.pkgMap = true
	}
	if strings.HasSuffix(f, ".go") {
		// Any root-module Go file may add, remove or reword a package doc.
		st.pkgMap = true
		st.addGoFile(f, in)
	}
	if p, ok := ownerPkg(f, byDir); ok {
		st.goChanged = true
		st.changedPkgs[p.ImportPath] = true
	} else if strings.HasSuffix(f, ".go") {
		// Deleted or unlisted package (e.g. build-tagged): be conservative.
		st.goChanged, st.unknownGo = true, true
	}
}

// addEmbeddedAndNested marks root packages affected through the embedded
// frontend assets and through nested modules they import.
func (st *state) addEmbeddedAndNested(in Input, byDir map[string]Pkg) {
	if st.frontend {
		// The frontend is embedded by serveui. Rebuild the assets and exercise
		// its reverse dependents, not just JavaScript checks.
		if p, ok := byDir["internal/serveui"]; ok {
			st.goChanged = true
			st.changedPkgs[p.ImportPath] = true
		}
	}
	for m := range st.nested {
		mod := in.NestedModPaths[m]
		if mod == "" {
			continue
		}
		for _, p := range in.Pkgs {
			if importsModule(p, mod) {
				st.goChanged = true
				st.changedPkgs[p.ImportPath] = true
			}
		}
	}
}

func (st *state) applyAll(in Input) {
	st.goChanged, st.unknownGo, st.nestedChanged, st.frontend, st.plans, st.ledger, st.sourceChanged, st.docs = true, true, true, true, true, true, true, true
	for _, m := range in.NestedMods {
		st.nested[m] = true
	}
	// Match CI's repository-wide formatting check, including nested modules.
	st.goFiles = []string{"."}
}

// resolveTargets fills pl.Changed/pl.Affected and returns the go package arguments.
func (st *state) resolveTargets(in Input, pl *Plan) []string {
	if !st.goChanged {
		return nil
	}
	if st.unknownGo {
		for _, p := range in.Pkgs {
			pl.Changed = append(pl.Changed, p.ImportPath)
		}
		sort.Strings(pl.Changed)
		pl.Affected = append([]string(nil), pl.Changed...)
		return []string{"./..."}
	}
	for k := range st.changedPkgs {
		pl.Changed = append(pl.Changed, k)
	}
	sort.Strings(pl.Changed)
	pl.Affected = ReverseDeps(in.Pkgs, pl.Changed)
	return pl.Affected
}

// hasTarget reports whether go test already covers the named command package.
func hasTarget(targets []string, name string) bool {
	for _, t := range targets {
		if t == "./..." || path.Base(t) == name {
			return true
		}
	}
	return false
}

func goSteps(targets []string) []Step {
	return []Step{
		{Name: "make build", Args: []string{"make", "build"}},
		{Name: "go build", Args: []string{"go", "build", "./..."}},
		{Name: "go vet", Args: append([]string{"go", "vet"}, targets...)},
		{Name: "go test", Args: append([]string{"go", "test"}, targets...)},
	}
}

func frontendSteps() []Step {
	var steps []Step
	for _, s := range []string{"format:check", "lint", "typecheck", "test"} {
		steps = append(steps, Step{Name: "frontend " + s, Args: []string{"npm", "--prefix", "frontend", "run", s}})
	}
	return steps
}

// BuildPlan maps changed files to a list of check steps.
func BuildPlan(in Input) Plan {
	var pl Plan
	byDir := map[string]Pkg{}
	for _, p := range in.Pkgs {
		byDir[p.Dir] = p
	}
	st := &state{nested: map[string]bool{}, changedPkgs: map[string]bool{}}
	for _, f := range in.Files {
		st.classify(f, in, byDir)
	}
	st.addEmbeddedAndNested(in, byDir)
	if in.All {
		st.applyAll(in)
	}
	targets := st.resolveTargets(in, &pl)

	if len(st.goFiles) > 0 {
		sort.Strings(st.goFiles)
		pl.Steps = append(pl.Steps, Step{Name: "gofmt", Args: append([]string{"gofmt", "-l"}, st.goFiles...)})
	}
	if st.goChanged {
		pl.Steps = append(pl.Steps, goSteps(targets)...)
	}
	if st.nestedChanged {
		pl.Steps = append(pl.Steps, Step{Name: "nested modules", Args: []string{"scripts/verify_nested_modules.sh"}})
	}
	// Frontend checks precede the docs drift check, which compiles packages
	// that embed the built frontend assets.
	if st.frontend {
		pl.Steps = append(pl.Steps, frontendSteps()...)
	}
	// Docs drift is a gendocs test: go test already runs it when gendocs is a
	// target, so only add it for docs-site changes that miss it.
	if st.apiTypes && !hasTarget(targets, "gentsapi") {
		pl.Steps = append(pl.Steps, Step{Name: "api types drift", Args: []string{"go", "test", "./cmd/gentsapi", "-run", "^TestAPITypesUpToDate$"}})
	}
	if st.docs && !hasTarget(targets, "gendocs") {
		if !st.goChanged {
			pl.Steps = append(pl.Steps, Step{Name: "make frontend", Args: []string{"make", "frontend"}})
		}
		pl.Steps = append(pl.Steps, Step{Name: "docs drift", Args: []string{"go", "test", "./cmd/gendocs", "-run", "^TestReferenceDocsUpToDate$"}})
	}
	if st.pkgMap && !hasTarget(targets, "genpkgmap") {
		pl.Steps = append(pl.Steps, Step{Name: "package map drift", Args: []string{"go", "test", "./cmd/genpkgmap", "-run", "^TestPackageMapUpToDate$"}})
	}
	if st.sourceChanged || st.ledger || in.All {
		pl.Steps = append(pl.Steps, Step{Name: "complexity", Args: []string{"make", "complexity"}})
	}
	if st.plans {
		pl.Steps = append(pl.Steps, Step{Name: "plans-lint", Args: []string{"make", "plans-lint"}})
	}
	return pl
}
