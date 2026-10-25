package main

import (
	"reflect"
	"strings"
	"testing"
)

var testPkgs = []Pkg{
	{ImportPath: "m", GoFiles: []string{"pkg.go"}, Dir: ".", Imports: []string{"m/cmd"}},
	{ImportPath: "m/cmd", GoFiles: []string{"pkg.go"}, Dir: "cmd", Imports: []string{"m/internal/a"}},
	{ImportPath: "m/internal/a", GoFiles: []string{"pkg.go"}, Dir: "internal/a", Imports: []string{"m/internal/b"}},
	{ImportPath: "m/internal/b", GoFiles: []string{"pkg.go"}, Dir: "internal/b"},
	{ImportPath: "m/internal/c", GoFiles: []string{"pkg.go"}, Dir: "internal/c", TestImports: []string{"m/internal/b"}},
	{ImportPath: "m/internal/d", GoFiles: []string{"pkg.go"}, Dir: "internal/d"},
	{ImportPath: "m/internal/serveui", GoFiles: []string{"pkg.go"}, Dir: "internal/serveui"},
	{ImportPath: "m/cmd/genpkgmap", GoFiles: []string{"pkg.go"}, Dir: "cmd/genpkgmap"},
	{ImportPath: "m/cmd/gendocs", GoFiles: []string{"pkg.go"}, Dir: "cmd/gendocs", Imports: []string{"m/cmd"}},
	{ImportPath: "m/internal/e", GoFiles: []string{"pkg.go"}, Dir: "internal/e", Imports: []string{"other.example/reflow/sub"}},
}

func plan(all bool, files ...string) Plan {
	return BuildPlan(Input{Files: files, All: all, Pkgs: testPkgs,
		GoFiles:    []string{"main.go", "internal/reflow/a.go"},
		NestedMods: []string{"internal/reflow"}, NestedModPaths: map[string]string{"internal/reflow": "other.example/reflow"},
		ExistsFile: func(string) bool { return true }})
}

func names(p Plan) string {
	var n []string
	for _, s := range p.Steps {
		n = append(n, s.Name)
	}
	return strings.Join(n, ",")
}

func TestReverseDeps(t *testing.T) {
	got := ReverseDeps(testPkgs, []string{"m/internal/b"})
	want := []string{"m", "m/cmd", "m/cmd/gendocs", "m/internal/a", "m/internal/b", "m/internal/c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestGoFileMapsToPackageAndReverseDeps(t *testing.T) {
	p := plan(false, "internal/a/x.go")
	if !reflect.DeepEqual(p.Changed, []string{"m/internal/a"}) {
		t.Fatalf("changed %v", p.Changed)
	}
	if !reflect.DeepEqual(p.Affected, []string{"m", "m/cmd", "m/cmd/gendocs", "m/internal/a"}) {
		t.Fatalf("affected %v", p.Affected)
	}
	if got := names(p); got != "gofmt,make build,go build,go vet,go test,package map drift,complexity" {
		t.Fatalf("steps %s", got)
	}
}

func TestPackageMapDriftSelection(t *testing.T) {
	if got := names(plan(false, "PACKAGES.md")); got != "package map drift" {
		t.Fatalf("PACKAGES.md: %s", got)
	}
	if got := names(plan(false, "internal/reflow/go.mod")); !strings.Contains(got, "package map drift") {
		t.Errorf("nested go.mod: missing package map drift: %s", got)
	}
	for _, file := range []string{"internal/reflow/a.go", "docs-site/a.md", "frontend/src/a.ts", "go.mod"} {
		if got := names(plan(false, file)); strings.Contains(got, "package map drift") {
			t.Errorf("%s: unexpected package map drift: %s", file, got)
		}
	}
}

func TestEmbeddedAssetMapsToNearestPackage(t *testing.T) {
	p := plan(false, "internal/d/static/app.js")
	if !reflect.DeepEqual(p.Changed, []string{"m/internal/d"}) {
		t.Fatalf("changed %v", p.Changed)
	}
}

func TestDocsOnlyChangeRunsDriftCheck(t *testing.T) {
	if got := names(plan(false, "README.md")); got != "" {
		t.Fatalf("steps %s", got)
	}
	if got := names(plan(false, "docs-site/content/x.md")); got != "make frontend,docs drift" {
		t.Fatalf("steps %s", got)
	}
}

func TestGendocsRunsOnceViaGoTest(t *testing.T) {
	for _, file := range []string{"cmd/root.go", "internal/b/x.go", "Makefile"} {
		if got := names(plan(false, file)); strings.Contains(got, "docs drift") {
			t.Errorf("%s: duplicate gendocs test: %s", file, got)
		}
	}
	if got := names(plan(false, "docs-site/a.md", "internal/b/x.go")); strings.Contains(got, "docs drift") {
		t.Errorf("duplicate gendocs test: %s", got)
	}
	if got := names(plan(false, "internal/d/x.go", "docs-site/a.md")); !strings.Contains(got, "docs drift") {
		t.Errorf("missing docs drift: %s", got)
	}
}

func TestNestedModuleChangeTestsRootImporters(t *testing.T) {
	p := plan(false, "internal/reflow/a.go")
	if !reflect.DeepEqual(p.Changed, []string{"m/internal/e"}) {
		t.Fatalf("changed %v", p.Changed)
	}
	if got := names(p); got != "gofmt,make build,go build,go vet,go test,nested modules,complexity" {
		t.Fatal(got)
	}
}

func TestUnknownGoCountsAllPackages(t *testing.T) {
	if p := plan(true); len(p.Changed) != len(testPkgs) {
		t.Fatalf("changed %d want %d", len(p.Changed), len(testPkgs))
	}
}

func TestFrontendPrecedesDocs(t *testing.T) {
	got := names(plan(false, "frontend/src/a.ts", "docs-site/a.md", "internal/d/x.go"))
	if i, j := strings.Index(got, "frontend test"), strings.Index(got, "docs drift"); i < 0 || j < i {
		t.Fatal(got)
	}
}

func TestBuildPlanBranches(t *testing.T) {
	if got := names(plan(false)); got != "" {
		t.Fatal(got)
	}
}

func TestSpecialAreas(t *testing.T) {
	if got := names(plan(false, "frontend/src/a.ts")); got != "make build,go build,go vet,go test,frontend format:check,frontend lint,frontend typecheck,frontend test" {
		t.Fatal(got)
	}
	if got := names(plan(false, "plans/x.md")); got != "plans-lint" {
		t.Fatal(got)
	}
	if got := names(plan(false, "internal/reflow/a.go")); got != "gofmt,make build,go build,go vet,go test,nested modules,complexity" {
		t.Fatal(got)
	}
	if got := names(plan(false, "ledger/go-size-baseline.json")); got != "complexity" {
		t.Fatal(got)
	}
}

func TestInfrastructureChanges(t *testing.T) {
	if got := names(plan(false, "Makefile")); got != "make build,go build,go vet,go test,nested modules,frontend format:check,frontend lint,frontend typecheck,frontend test,complexity,plans-lint" {
		t.Fatal(got)
	}
	if got := names(plan(false, "scripts/lint-plans.sh")); got != "plans-lint" {
		t.Fatal(got)
	}
	if got := names(plan(false, "scripts/verify_nested_modules.sh")); got != "nested modules" {
		t.Fatal(got)
	}
}

func TestGoModAndAll(t *testing.T) {
	p := plan(false, "go.mod")
	if len(p.Affected) != len(testPkgs) {
		t.Fatalf("go.mod should affect all: %v", p.Affected)
	}
	all := plan(true)
	want := "gofmt,make build,go build,go vet,go test,nested modules,frontend format:check,frontend lint,frontend typecheck,frontend test,complexity,plans-lint"
	if got := names(all); got != want {
		t.Fatal(got)
	}
}

func TestDeletedPackageIsConservative(t *testing.T) {
	p := BuildPlan(Input{Files: []string{"internal/gone/x.go"}, Pkgs: testPkgs,
		ExistsFile: func(string) bool { return false }})
	if got := p.Steps[3].Args[2:]; !reflect.DeepEqual(got, p.Affected) || len(got) != len(testPkgs) {
		t.Fatalf("expected every owned package, got %v", got)
	}
}

func TestAPITypesDriftSelection(t *testing.T) {
	covered := func(p Plan) bool {
		for _, s := range p.Steps {
			if s.Name == "api types drift" {
				return true
			}
			if s.Name == "go test" && hasTarget(s.Args[2:], "gentsapi") {
				return true
			}
		}
		return false
	}
	for _, file := range []string{"cmd/serve_handlers.go", "internal/app/serve/jobs/schema.go", "frontend/src/api/endpoints.ts", "internal/tsgen/generate.go", "internal/share/share.go", "internal/session/rush.go", "internal/hub/probe.go"} {
		if p := plan(false, file); !covered(p) {
			t.Errorf("%s: %s", file, names(p))
		}
	}
	for _, file := range []string{"cmd/chat.go", "frontend/src/hub/app.tsx"} {
		if p := plan(false, file); strings.Contains(names(p), "api types drift") {
			t.Errorf("%s: %s", file, names(p))
		}
	}
}

func TestScratchChangesDoNotSelectChecks(t *testing.T) {
	if got := names(plan(false, "tmp/bench/seed.go", "tmp/nested/go.mod", "tmp/module/main_test.go")); got != "" {
		t.Fatalf("scratch changes selected checks: %s", got)
	}
}

func TestFullAndFallbackPlansUseExplicitOwnedTargets(t *testing.T) {
	for _, p := range []Plan{plan(true), plan(false, "go.mod"), plan(false, "internal/gone/x.go")} {
		for _, s := range p.Steps {
			for _, arg := range s.Args {
				if arg == "./..." || (s.Name == "gofmt" && arg == ".") || scratchPath(arg) {
					t.Fatalf("recursive or scratch target in %s: %v", s.Name, s.Args)
				}
			}
			if s.Name == "go build" || s.Name == "go vet" || s.Name == "go test" {
				if len(s.Args[2:]) != len(testPkgs) {
					t.Fatalf("%s lost package coverage: %v", s.Name, s.Args)
				}
			}
		}
	}
	for _, s := range plan(true).Steps {
		if s.Name == "gofmt" && !reflect.DeepEqual(s.Args, []string{"gofmt", "-l", "internal/reflow/a.go", "main.go"}) {
			t.Fatalf("formatting lost nested module files: %v", s.Args)
		}
	}
}

func TestBuildTargetsUseGoAndCgoSources(t *testing.T) {
	pkgs := []Pkg{
		{ImportPath: "m/go", Dir: "go", GoFiles: []string{"source.go"}},
		{ImportPath: "m/cgo", Dir: "cgo", CgoFiles: []string{"source.go"}},
		{ImportPath: "m/testonly", Dir: "testonly"},
	}
	p := BuildPlan(Input{All: true, Pkgs: pkgs})
	for _, step := range p.Steps {
		if step.Name == "go build" {
			want := []string{"go", "build", "m/cgo", "m/go"}
			if !reflect.DeepEqual(step.Args, want) {
				t.Fatalf("build targets %v; want %v", step.Args, want)
			}
		}
		if step.Name == "go vet" || step.Name == "go test" {
			if !reflect.DeepEqual(step.Args[2:], []string{"m/cgo", "m/go", "m/testonly"}) {
				t.Fatalf("%s lost test-only coverage: %v", step.Name, step.Args)
			}
		}
	}
	for _, step := range goSteps([]string{"m/testonly"}, pkgs[2:]) {
		if step.Name == "go build" {
			t.Fatalf("empty build target list would implicitly build the working directory: %v", step.Args)
		}
	}
}
