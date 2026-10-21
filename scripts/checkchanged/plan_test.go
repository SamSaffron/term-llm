package main

import (
	"reflect"
	"strings"
	"testing"
)

var testPkgs = []Pkg{
	{ImportPath: "m", Dir: ".", Imports: []string{"m/cmd"}},
	{ImportPath: "m/cmd", Dir: "cmd", Imports: []string{"m/internal/a"}},
	{ImportPath: "m/internal/a", Dir: "internal/a", Imports: []string{"m/internal/b"}},
	{ImportPath: "m/internal/b", Dir: "internal/b"},
	{ImportPath: "m/internal/c", Dir: "internal/c", TestImports: []string{"m/internal/b"}},
	{ImportPath: "m/internal/d", Dir: "internal/d"},
	{ImportPath: "m/internal/serveui", Dir: "internal/serveui"},
	{ImportPath: "m/cmd/gendocs", Dir: "cmd/gendocs", Imports: []string{"m/cmd"}},
	{ImportPath: "m/internal/e", Dir: "internal/e", Imports: []string{"other.example/reflow/sub"}},
}

func plan(all bool, files ...string) Plan {
	return BuildPlan(Input{Files: files, All: all, Pkgs: testPkgs,
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
	if last := p.Steps[2].Args[len(p.Steps[2].Args)-1]; last != "./..." && len(p.Affected) != len(testPkgs) {
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
	if got := p.Steps[3].Args; got[len(got)-1] != "./..." {
		t.Fatalf("expected ./..., got %v", got)
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
