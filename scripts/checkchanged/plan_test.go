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
}

func plan(all bool, files ...string) Plan {
	return BuildPlan(Input{Files: files, All: all, Pkgs: testPkgs,
		NestedMods: []string{"internal/reflow"}, ExistsFile: func(string) bool { return true }})
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
	want := []string{"m", "m/cmd", "m/internal/a", "m/internal/b", "m/internal/c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestGoFileMapsToPackageAndReverseDeps(t *testing.T) {
	p := plan(false, "internal/a/x.go")
	if !reflect.DeepEqual(p.Changed, []string{"m/internal/a"}) {
		t.Fatalf("changed %v", p.Changed)
	}
	if !reflect.DeepEqual(p.Affected, []string{"m", "m/cmd", "m/internal/a"}) {
		t.Fatalf("affected %v", p.Affected)
	}
	if got := names(p); got != "gofmt,make build,go build,go vet,go test,complexity" {
		t.Fatalf("steps %s", got)
	}
}

func TestEmbeddedAssetMapsToNearestPackage(t *testing.T) {
	p := plan(false, "internal/d/static/app.js")
	if !reflect.DeepEqual(p.Changed, []string{"m/internal/d"}) {
		t.Fatalf("changed %v", p.Changed)
	}
}

func TestDocsOnlyChangeRunsDriftCheck(t *testing.T) {
	if got := names(plan(false, "README.md", "docs-site/content/x.md")); got != "docs drift" {
		t.Fatalf("steps %s", got)
	}
	for _, file := range []string{"internal/providerspec/spec.go", "internal/tools/descriptors.go", "internal/config/schema.go", "cmd/root.go", "internal/docsgen/docsgen.go"} {
		if got := names(plan(false, file)); !strings.Contains(got, "docs drift") {
			t.Errorf("%s: steps %s", file, got)
		}
	}
}

func TestSpecialAreas(t *testing.T) {
	if got := names(plan(false, "frontend/src/a.ts")); got != "make build,go build,go vet,go test,frontend format:check,frontend lint,frontend typecheck,frontend test" {
		t.Fatal(got)
	}
	if got := names(plan(false, "plans/x.md")); got != "plans-lint" {
		t.Fatal(got)
	}
	if got := names(plan(false, "internal/reflow/a.go")); got != "gofmt,nested modules,complexity" {
		t.Fatal(got)
	}
	if got := names(plan(false, "ledger/go-size-baseline.json")); got != "complexity" {
		t.Fatal(got)
	}
}

func TestInfrastructureChanges(t *testing.T) {
	if got := names(plan(false, "Makefile")); got != "make build,go build,go vet,go test,docs drift,nested modules,frontend format:check,frontend lint,frontend typecheck,frontend test,complexity,plans-lint" {
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
	want := "gofmt,make build,go build,go vet,go test,docs drift,nested modules,frontend format:check,frontend lint,frontend typecheck,frontend test,complexity,plans-lint"
	if got := names(all); got != want {
		t.Fatal(got)
	}
}

func TestDeletedPackageIsConservative(t *testing.T) {
	p := BuildPlan(Input{Files: []string{"internal/gone/x.go"}, Pkgs: testPkgs,
		ExistsFile: func(string) bool { return false }})
	if got := p.Steps[len(p.Steps)-2].Args; got[len(got)-1] != "./..." {
		t.Fatalf("expected ./..., got %v", got)
	}
}
