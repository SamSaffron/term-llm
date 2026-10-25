package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	// The isolated shell wrapper shares package discovery with the checker.
	if len(args) > 0 && args[0] == "--go-test" {
		if err := runGoTest(args[1:]); err != nil {
			fmt.Fprintln(stderr, "gotest:", err)
			return 1
		}
		return 0
	}
	fs := flag.NewFlagSet("check-changed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "run every check regardless of the diff")
	base := fs.String("base", "", "base ref; diff is taken from its merge-base with HEAD (default: first of upstream/main, origin/main, main)")
	dry := fs.Bool("dry-run", false, "print the planned steps without running them")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *base == "" {
		*base = defaultBase()
	}
	if err := execute(*all, *base, *dry, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "check-changed:", err)
		return 1
	}
	return 0
}

var baseCandidates = []string{"upstream/main", "origin/main", "main"}

// defaultBase returns the first existing candidate ref, or the first candidate
// so the later merge-base error names a sensible ref.
func defaultBase() string {
	for _, ref := range baseCandidates {
		if _, err := git("rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return ref
		}
	}
	return baseCandidates[0]
}

func git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func splitNUL(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// changedFiles returns files differing from the merge-base, including staged,
// unstaged and untracked files.
func changedFiles(base string) ([]string, error) {
	mb, err := git("merge-base", "HEAD", base)
	if err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(string(mb))
	diff, err := git("diff", "--name-only", "-z", "--no-renames", ref)
	if err != nil {
		return nil, err
	}
	untracked, err := git("ls-files", "-o", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range append(splitNUL(diff), splitNUL(untracked)...) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out, nil
}

func listPackages() ([]Pkg, error) {
	files, err := repositoryGoFiles()
	if err != nil {
		return nil, err
	}
	dirs := repositoryPackageDirs(files)
	if len(dirs) == 0 {
		return nil, nil
	}
	cmd := exec.Command("go", append([]string{"list", "-e", "-json"}, dirs...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	var pkgs []Pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p Pkg
		var raw struct {
			ImportPath                         string
			Dir                                string
			Imports, TestImports, XTestImports []string
			GoFiles, CgoFiles                  []string
			Error                              *struct{ Err string }
		}
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("decode go list: %w", err)
		}
		// Explicit directory listing also reports packages excluded by platform
		// or build tags; like ./..., omit those, but retain actual source errors.
		if raw.Error != nil && strings.Contains(raw.Error.Err, "build constraints exclude all Go files") {
			continue
		}
		rel, err := filepath.Rel(root, raw.Dir)
		if err != nil {
			return nil, err
		}
		p = Pkg{ImportPath: raw.ImportPath, Dir: filepath.ToSlash(rel), GoFiles: raw.GoFiles, CgoFiles: raw.CgoFiles,
			Imports: raw.Imports, TestImports: raw.TestImports, XTestImports: raw.XTestImports}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// nestedModules returns nested module directories and their module paths.
func nestedModules() ([]string, map[string]string, error) {
	out, err := git("ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*go.mod", ":(top,exclude)tmp")
	if err != nil {
		return nil, nil, err
	}
	var mods []string
	paths := map[string]string{}
	for _, f := range splitNUL(out) {
		d := filepath.ToSlash(filepath.Dir(f))
		if d == "." || scratchPath(d) || dependencyPath(d) {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			continue
		}
		mods = append(mods, d)
		cmd := exec.Command("go", "list", "-m")
		cmd.Dir = d
		if mp, err := cmd.Output(); err == nil {
			paths[d] = strings.TrimSpace(string(mp))
		}
	}
	return mods, paths, nil
}

// npmCache returns the real npm cache directory so the isolated HOME does not
// force a cold cache (and network fetches) on every run.
func npmCache() string {
	if c := os.Getenv("npm_config_cache"); c != "" {
		return c
	}
	if out, err := exec.Command("npm", "config", "get", "cache").Output(); err == nil {
		if c := strings.TrimSpace(string(out)); c != "" && c != "undefined" {
			return c
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".npm")
	}
	return ""
}

func isolatedEnv(tmp string) ([]string, error) {
	get := func(k string) (string, error) {
		out, err := exec.Command("go", "env", k).Output()
		return strings.TrimSpace(string(out)), err
	}
	modcache, err := get("GOMODCACHE")
	if err != nil {
		return nil, err
	}
	gocache, err := get("GOCACHE")
	if err != nil {
		return nil, err
	}
	gopath, err := get("GOPATH")
	if err != nil {
		return nil, err
	}
	env := append(os.Environ(),
		"HOME="+filepath.Join(tmp, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(tmp, "config"),
		"XDG_DATA_HOME="+filepath.Join(tmp, "data"),
		"XDG_CACHE_HOME="+filepath.Join(tmp, "cache"),
		"GOMODCACHE="+modcache,
		"GOCACHE="+gocache,
		"GOPATH="+gopath,
	)
	if c := npmCache(); c != "" {
		env = append(env, "npm_config_cache="+c)
	}
	return env, nil
}

func execute(all bool, base string, dry bool, stdout, stderr io.Writer) error {
	var files []string
	if !all {
		var err error
		if files, err = changedFiles(base); err != nil {
			return err
		}
	}
	pkgs, err := listPackages()
	if err != nil {
		return err
	}
	mods, modPaths, err := nestedModules()
	if err != nil {
		return err
	}
	var goFiles []string
	if all {
		goFiles, err = repositoryGoFiles()
		if err != nil {
			return err
		}
	}
	pl := BuildPlan(Input{Files: files, All: all, GoFiles: goFiles, Pkgs: pkgs, NestedMods: mods, NestedModPaths: modPaths,
		ExistsFile: func(f string) bool { _, err := os.Stat(f); return err == nil }})

	fmt.Fprintf(stdout, "base=%s all=%v changed files=%d changed pkgs=%d affected pkgs=%d\n",
		base, all, len(files), len(pl.Changed), len(pl.Affected))
	if len(pl.Steps) == 0 {
		fmt.Fprintln(stdout, "nothing to check")
		return nil
	}
	if dry {
		for _, s := range pl.Steps {
			fmt.Fprintf(stdout, "would run [%s]: %s\n", s.Name, strings.Join(s.Args, " "))
		}
		return nil
	}

	tmp, err := os.MkdirTemp("", "check-changed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	env, err := isolatedEnv(tmp)
	if err != nil {
		return err
	}
	for _, d := range []string{"home", "config", "data", "cache"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			return err
		}
	}

	type result struct {
		name string
		ok   bool
		dur  time.Duration
	}
	var results []result
	for _, s := range pl.Steps {
		fmt.Fprintf(stdout, "==> %s: %s\n", s.Name, strings.Join(s.Args, " "))
		start := time.Now()
		var buf bytes.Buffer
		cmd := exec.Command(s.Args[0], s.Args[1:]...)
		cmd.Dir = s.Dir
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = io.MultiWriter(stdout, &buf), io.MultiWriter(stderr, &buf)
		runErr := cmd.Run()
		ok := runErr == nil
		if ok && s.Name == "gofmt" && strings.TrimSpace(buf.String()) != "" {
			fmt.Fprintln(stderr, "gofmt: files above need formatting")
			ok = false
		}
		results = append(results, result{s.Name, ok, time.Since(start)})
	}
	failed := 0
	fmt.Fprintln(stdout, "\n== summary ==")
	for _, r := range results {
		status := "PASS"
		if !r.ok {
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(stdout, "%s  %s (%s)\n", status, r.name, r.dur.Round(time.Millisecond))
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	fmt.Fprintln(stdout, "all checks passed")
	return nil
}
