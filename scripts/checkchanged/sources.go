package main

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// scratchPath is deliberately independent of gitignore: even force-added scratch
// files and modules must never become check targets.
func scratchPath(p string) bool { return isUnder(path.Clean(filepath.ToSlash(p)), "tmp") }

// repositoryGoFiles includes tracked and new, non-ignored source, including real
// nested modules and testdata. Never walk scratch trees or dependency caches.
func repositoryGoFiles() ([]string, error) {
	out, err := git("ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go", ":(top,exclude)tmp")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range splitNUL(out) {
		if scratchPath(f) || dependencyPath(f) {
			continue
		}
		if info, err := os.Lstat(f); err == nil && info.Mode().IsRegular() {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, nil
}

func dependencyPath(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == "node_modules" || part == "vendor" {
			return true
		}
	}
	return false
}

func packageDirectory(dir string) bool {
	for _, part := range strings.Split(dir, "/") {
		if part == "testdata" || (part != "." && (strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_"))) {
			return false
		}
	}
	// A nested module is not part of the root package set, whether or not its
	// go.mod is tracked. Real nested modules retain their separate checks.
	for d := dir; d != "."; d = path.Dir(d) {
		if _, err := os.Stat(path.Join(d, "go.mod")); err == nil {
			return false
		}
	}
	return true
}

func repositoryPackageDirs(files []string) []string {
	seen := map[string]bool{}
	for _, f := range files {
		dir := path.Dir(f)
		base := path.Base(f)
		if strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
			continue
		}
		if !scratchPath(f) && packageDirectory(dir) {
			seen["./"+dir] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// testArgs expands recursive selectors against the same owned package set used
// by the full checker. Explicit scratch paths are errors, not silently skipped.
func testArgs(args []string) ([]string, error) {
	var out []string
	var pkgs []Pkg
	loaded := false
	hasPackage := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-args" || a == "--args" {
			out = append(out, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			if a == "-C" || strings.HasPrefix(a, "-C=") {
				return nil, fmt.Errorf("-C is not supported; run tests from the repository root")
			}
			out = append(out, a)
			if testFlagValue(a) && !strings.Contains(a, "=") {
				if i+1 == len(args) {
					return nil, fmt.Errorf("missing value for %s", a)
				}
				i++
				out = append(out, args[i])
			}
			continue
		}
		hasPackage = true
		if a == "all" {
			return nil, fmt.Errorf("target all includes unowned packages; use ./... instead")
		}
		if err := rejectScratchTarget(a); err != nil {
			return nil, err
		}
		if !strings.Contains(a, "...") {
			out = append(out, a)
			continue
		}
		if !loaded {
			var err error
			pkgs, err = listPackages()
			if err != nil {
				return nil, err
			}
			loaded = true
		}
		matches := matchPackages(a, pkgs)
		if len(matches) == 0 {
			return nil, fmt.Errorf("no repository packages match %q", a)
		}
		out = append(out, matches...)
	}
	if !hasPackage {
		out = append([]string{"."}, out...)
	}
	return out, nil
}

// Keep values of Go's test/build flags opaque, especially -run regexes and
// compiler flags. Unknown flags are left standalone rather than letting a new
// boolean flag hide the following package from scratch-target validation.
func testFlagValue(flag string) bool {
	name := strings.TrimPrefix(strings.SplitN(strings.TrimLeft(flag, "-"), "=", 2)[0], "test.")
	switch name {
	case "asmflags", "bench", "benchtime", "blockprofile", "blockprofilerate", "buildmode", "compiler", "count", "covermode", "coverpkg", "coverprofile", "cpu", "cpuprofile", "exec", "fuzz", "fuzzminimizetime", "fuzztime", "gcflags", "gccgoflags", "installsuffix", "ldflags", "list", "memprofile", "memprofilerate", "mod", "modfile", "mutexprofile", "mutexprofilefraction", "o", "outputdir", "overlay", "p", "parallel", "pgo", "pkgdir", "run", "shuffle", "skip", "tags", "testlogfile", "timeout", "toolexec", "trace", "vet":
		return true
	default:
		return false
	}
}

func rejectScratchTarget(target string) error {
	// Check local paths (including absolute paths and symlinks) and root-module
	// import paths. This happens before go list/test sees an explicit target.
	local := strings.TrimSuffix(target, "/...")
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, candidate := range []string{local, resolvedPath(local)} {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			return err
		}
		for _, base := range []string{root, resolvedPath(root)} {
			rel, err := filepath.Rel(base, abs)
			if err == nil && scratchPath(rel) {
				return fmt.Errorf("scratch target %q is not allowed (repository tmp/)", target)
			}
		}
	}
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		return fmt.Errorf("read root module: %w", err)
	}
	for _, line := range strings.Split(string(mod), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" && isUnder(local, strings.Trim(fields[1], "\"")+"/tmp") {
			return fmt.Errorf("scratch target %q is not allowed (repository tmp/)", target)
		}
	}
	return nil
}

func resolvedPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func matchPackages(pattern string, pkgs []Pkg) []string {
	var matches []string
	prefix, suffix, _ := strings.Cut(pattern, "...")
	for _, p := range pkgs {
		name := p.ImportPath
		if strings.HasPrefix(pattern, "./") {
			name = "./" + p.Dir
		}
		if (strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)) || (suffix == "" && name == strings.TrimSuffix(prefix, "/")) || (pattern == "./..." && p.Dir == ".") {
			matches = append(matches, p.ImportPath)
		}
	}
	return matches
}

func runGoTest(args []string) error {
	targets, err := testArgs(args)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", append([]string{"test"}, targets...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
