package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sourceFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	fixtureGit(t, "init", "-q")
	for name, content := range map[string]string{
		".gitignore":                      "tmp/\nnode_modules/\n",
		"go.mod":                          "module example.test/repo\n\ngo 1.24\n",
		"main.go":                         "package main\nfunc main() {}\n",
		"internal/a/a.go":                 "package a\n",
		"internal/testonly/local_test.go": "package testonly\n",
		"internal/new/new_test.go":        "package new_test\n",
		"internal/tagged/tagged.go":       "//go:build never_enabled\n\npackage tagged\n",
		"internal/testdata/broken.go":     "not valid Go",
		"internal/_ignored/broken.go":     "not valid Go",
		"internal/reflow/go.mod":          "module example.test/reflow\n\ngo 1.24\n",
		"internal/reflow/reflow.go":       "package reflow\n",
		"tmp/bench/seed.go":               "not valid Go: scratch must not be parsed",
		"tmp/untracked/broken.go":         "not valid Go",
		"tmp/cache/module/go.mod":         "not a valid module file",
		"tmp/cache/module/broken.go":      "not valid Go",
		"node_modules/cache/broken.go":    "not valid Go",
		"node_modules/cache/go.mod":       "not a valid module file",
	} {
		writeSource(t, name, content)
	}
	fixtureGit(t, "add", "go.mod", "main.go", "internal/a", ".gitignore")
	// Exclusion must survive both .gitignore changes and force-added scratch.
	fixtureGit(t, "add", "-f", "tmp/bench/seed.go", "tmp/cache/module/go.mod", "node_modules/cache/go.mod")
	return dir
}

func writeSource(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func fixtureGit(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestDiscoveryExcludesScratchBeforeGoList(t *testing.T) {
	sourceFixture(t)
	pkgs, err := listPackages()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range pkgs {
		got = append(got, p.Dir)
	}
	if want := []string{".", "internal/a", "internal/new", "internal/testonly"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("packages %v; want %v", got, want)
	}
	mods, paths, err := nestedModules()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mods, []string{"internal/reflow"}) || paths["internal/reflow"] != "example.test/reflow" {
		t.Fatalf("nested modules %v %v", mods, paths)
	}
	files, err := repositoryGoFiles()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(files, "internal/reflow/reflow.go") || !contains(files, "internal/new/new_test.go") {
		t.Fatalf("missing owned files: %v", files)
	}
	for _, f := range files {
		if scratchPath(f) || dependencyPath(f) {
			t.Fatalf("unowned formatting target: %s", f)
		}
	}
	// Exercise production full-mode discovery without running build/test/format
	// against any fixture source (especially the deliberately malformed scratch).
	var stdout, stderr bytes.Buffer
	if err := execute(true, "unused", true, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"tmp/", "node_modules/", "./...", "gofmt -l .\n"} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("full plan includes %q:\n%s", forbidden, &stdout)
		}
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestTestArgsExpandOwnedPackagesAndPreserveFlags(t *testing.T) {
	sourceFixture(t)
	for _, tc := range []struct {
		name       string
		args, want []string
	}{
		{"default", nil, []string{"."}},
		{"recursive", []string{"-run", "^TestO'Brien$", "-count=1", "./..."}, []string{"-run", "^TestO'Brien$", "-count=1", "example.test/repo", "example.test/repo/internal/a", "example.test/repo/internal/new", "example.test/repo/internal/testonly"}},
		{"subtree", []string{"./internal/...", "-race", "-short", "-benchmem"}, []string{"example.test/repo/internal/a", "example.test/repo/internal/new", "example.test/repo/internal/testonly", "-race", "-short", "-benchmem"}},
		{"import subtree", []string{"example.test/repo/internal/..."}, []string{"example.test/repo/internal/a", "example.test/repo/internal/new", "example.test/repo/internal/testonly"}},
		{"profiles", []string{"-coverprofile", "tmp/coverage.out", "-test.paniconexit0", "./internal/a"}, []string{"-coverprofile", "tmp/coverage.out", "-test.paniconexit0", "./internal/a"}},
		{"focused", []string{"./internal/a", "-run", "tmp/marker", "-args", "tmp/opaque"}, []string{"./internal/a", "-run", "tmp/marker", "-args", "tmp/opaque"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := testArgs(tc.args)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("testArgs(%v) = %v, %v; want %v", tc.args, got, err, tc.want)
			}
		})
	}
}

func TestTestArgsRejectExplicitScratch(t *testing.T) {
	dir := sourceFixture(t)
	for _, target := range []string{
		"tmp", "./tmp", "./tmp/...", "tmp/bench/seed.go", "./internal/../tmp/bench",
		filepath.Join(dir, "tmp/bench"), "example.test/repo/tmp", "example.test/repo/tmp/...",
	} {
		t.Run(target, func(t *testing.T) {
			if _, err := testArgs([]string{"-run", "^$", target}); err == nil || !strings.Contains(err.Error(), "scratch target") {
				t.Fatalf("scratch target %q: %v", target, err)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(dir, "tmp"), "scratch-link"); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		for _, target := range []string{"./scratch-link", "./scratch-link/bench/..."} {
			if _, err := testArgs([]string{target}); err == nil || !strings.Contains(err.Error(), "scratch target") {
				t.Fatalf("scratch symlink %q: %v", target, err)
			}
		}
	})
	for _, args := range [][]string{{"-test.paniconexit0", "./tmp/bench"}, {"all"}, {"-C", "tmp", "./..."}, {"-C=tmp", "./..."}, {"./missing/..."}} {
		if _, err := testArgs(args); err == nil {
			t.Fatalf("unsafe or empty selector accepted: %v", args)
		}
	}
}

func TestDiscoveredTestOnlyPackagesRemainCheckedButNotBuilt(t *testing.T) {
	sourceFixture(t)
	pkgs, err := listPackages()
	if err != nil {
		t.Fatal(err)
	}
	files, err := repositoryGoFiles()
	if err != nil {
		t.Fatal(err)
	}
	plan := BuildPlan(Input{All: true, Pkgs: pkgs, GoFiles: files})
	for _, step := range plan.Steps {
		switch step.Name {
		case "go build":
			want := []string{"go", "build", "example.test/repo", "example.test/repo/internal/a"}
			if !reflect.DeepEqual(step.Args, want) {
				t.Fatalf("build targets %v; want %v", step.Args, want)
			}
		case "go vet", "go test":
			for _, pkg := range []string{"example.test/repo/internal/new", "example.test/repo/internal/testonly"} {
				if !contains(step.Args, pkg) {
					t.Errorf("%s omitted test-only package %s", step.Name, pkg)
				}
			}
		case "gofmt":
			for _, file := range []string{"internal/new/new_test.go", "internal/testonly/local_test.go"} {
				if !contains(step.Args, file) {
					t.Errorf("gofmt omitted test-only source %s", file)
				}
			}
		}
	}
}
