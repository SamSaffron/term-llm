package depguard

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Prefixes are repository-relative package paths, matched on package boundaries.
// exact applies only to forbidden imports: serve leaves may import one another,
// but must not import the host root. The source scan includes tests and all tags.
type rule struct {
	pkgPrefix     string
	excludePrefix string
	forbidden     []string
	exact         bool
}

var layeringRules = []rule{
	{pkgPrefix: "internal", forbidden: []string{"cmd"}},
	{pkgPrefix: "internal/app/runtime", forbidden: []string{"internal/app/serve", "internal/app/ask", "internal/app/chat"}},
	{pkgPrefix: "internal/app/serve/hub", forbidden: []string{"internal/app/serve"}, exact: true},
	{pkgPrefix: "internal/app/serve/shell", forbidden: []string{"internal/app/serve"}, exact: true},
	{pkgPrefix: "internal/app/serve/protocol", forbidden: []string{"internal/app/serve"}, exact: true},
	{pkgPrefix: "internal/app/serve/server", forbidden: []string{"internal/app/serve"}, exact: true},
	{pkgPrefix: "internal/app/serve/jobs", forbidden: []string{"internal/app/serve"}, exact: true},
	{pkgPrefix: "internal", excludePrefix: "internal/app", forbidden: []string{"internal/app"}},
	{pkgPrefix: "internal/app/serve", forbidden: []string{"github.com/spf13/cobra", "github.com/spf13/pflag", "internal/app/ask", "internal/app/chat"}},
	{pkgPrefix: "internal/app/chat", forbidden: []string{"github.com/spf13/cobra", "github.com/spf13/pflag", "internal/app/ask"}},
	{pkgPrefix: "internal/app/ask", forbidden: []string{"github.com/spf13/cobra", "github.com/spf13/pflag"}},
}

func hasPackagePrefix(pkg, prefix string) bool {
	return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
}

func TestInternalPackageLayering(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	modulePath, err := readModulePath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	violations, err := checkLayering(repoRoot, modulePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("internal package layering violations:\n%s", strings.Join(violations, "\n"))
	}
}

func checkLayering(repoRoot, modulePath string) ([]string, error) {
	internalDir := filepath.Join(repoRoot, "internal")
	fset := token.NewFileSet()
	var violations []string

	err := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		relPath, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return fmt.Errorf("make %s relative to repo root: %w", path, err)
		}
		relPath = filepath.ToSlash(relPath)
		pkg := filepath.ToSlash(filepath.Dir(relPath))
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse imports in %s: %w", path, err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("unquote import path in %s: %w", path, err)
			}
			importPkg := strings.TrimPrefix(importPath, modulePath+"/")
			for _, r := range layeringRules {
				if !hasPackagePrefix(pkg, r.pkgPrefix) || (r.excludePrefix != "" && hasPackagePrefix(pkg, r.excludePrefix)) {
					continue
				}
				for _, forbidden := range r.forbidden {
					if importPkg == forbidden || (!r.exact && hasPackagePrefix(importPkg, forbidden)) {
						violations = append(violations, fmt.Sprintf("%s imports %s", relPath, importPath))
					}
				}
			}
		}
		return nil
	})
	return violations, err
}

func TestLayeringGuardSelfTest(t *testing.T) {
	const modulePath = "example.com/fixture"
	cases := []struct {
		file, imported string
		forbidden      bool
	}{
		{"internal/app/serve/probe.go", "github.com/spf13/cobra", true},
		{"internal/app/serve/probe_test.go", "internal/app/chat", true},
		{"internal/app/serve/probe_test.go", "internal/app/ask", true},
		{"internal/app/chat/probe_test.go", "github.com/spf13/cobra", true},
		{"internal/app/chat/probe_test.go", "internal/app/ask", true},
		{"internal/app/serve/sub/probe_test.go", "github.com/spf13/pflag", true},
		{"internal/app/serve-extra/probe.go", "github.com/spf13/cobra", false},
		{"internal/app/ask/probe.go", "github.com/spf13/cobra", true},
		{"internal/core/probe.go", "cmd", true},
		{"internal/core/probe_test.go", "cmd/sub", true},
		{"internal/app/runtime/probe.go", "internal/app/serve", true},
		{"internal/app/runtime/sub/probe.go", "internal/app/serve/hub", true},
		{"internal/app/runtime/probe.go", "internal/app/ask", true},
		{"internal/app/runtime/sub/probe.go", "internal/app/ask/sub", true},
		{"internal/app/runtime/probe.go", "internal/app/chat", true},
		{"internal/app/runtime/sub/probe.go", "internal/app/chat/sub", true},
		{"internal/app/serve/hub/probe.go", "internal/app/serve", true},
		{"internal/app/serve/shell/sub/probe_test.go", "internal/app/serve", true},
		{"internal/app/serve/protocol/probe.go", "internal/app/serve", true},
		{"internal/app/serve/server/probe.go", "internal/app/serve", true},
		{"internal/app/serve/jobs/probe.go", "internal/app/serve", true},
		{"internal/core/probe.go", "internal/app", true},
		{"internal/core/sub/probe_test.go", "internal/app/runtime", true},
		{"internal/app/ask/probe.go", "internal/app/serve", false},
		{"internal/app/chat/probe.go", "internal/app/serve", false},
		{"internal/app/serve/probe.go", "internal/app/runtime", false},
		{"internal/app/serve/shell/probe.go", "internal/app/serve/protocol", false},
		{"internal/app/serve/hub/probe.go", "internal/app/serve-extra", false},
		{"internal/app/runtime/probe.go", "internal/app/serve-extra", false},
		{"internal/app/runtime-extra/probe.go", "internal/app/serve", false},
		{"internal/core/probe.go", "internal/application", false},
		{"internal/core/probe.go", "cmd-extra", false},
		{"internal/core/probe.go", "internal/session", false},
	}
	for _, tc := range cases {
		t.Run(tc.file+"->"+tc.imported, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, filepath.FromSlash(tc.file))
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			// An unsatisfied tag proves direct guards scan every source file.
			importPath := modulePath + "/" + tc.imported
			if strings.HasPrefix(tc.imported, "github.com/") {
				importPath = tc.imported
			}
			source := "//go:build depguard_synthetic\n\npackage probe\nimport _ " + strconv.Quote(importPath) + "\n"
			if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			violations, err := checkLayering(root, modulePath)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.forbidden {
				want = 1
			}
			if len(violations) != want {
				t.Fatalf("got violations %v, want %d", violations, want)
			}
			if want == 1 && violations[0] != tc.file+" imports "+importPath {
				t.Fatalf("unexpected diagnostic: %s", violations[0])
			}
		})
	}
}

func TestRuntimeDoesNotTransitivelyImportServe(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	modulePath, err := readModulePath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Unlike the source scan, go list resolves indirect edges and respects the
	// default build's platform and build tags. Do not use -e: load errors fail.
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./internal/app/runtime")
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list runtime dependencies: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if hasPackagePrefix(dependency, modulePath+"/internal/app/serve") {
			t.Errorf("internal/app/runtime transitively imports %s", dependency)
		}
	}
}

func findRepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("locate depguard test file")
	}

	dir := filepath.Dir(file)
	for {
		goModPath := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(goModPath); err == nil {
			return dir, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %w", goModPath, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", file)
		}
		dir = parent
	}
}

func readModulePath(repoRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}

	return "", fmt.Errorf("module declaration not found in go.mod")
}

func TestServeDoesNotTransitivelyImportCLI(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "-test", "-f", "{{.ImportPath}}", "./internal/app/serve/...")
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list serve dependencies: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if hasPackagePrefix(dependency, "github.com/spf13/cobra") || hasPackagePrefix(dependency, "github.com/spf13/pflag") || strings.Contains(dependency, "/internal/app/ask") || strings.Contains(dependency, "/internal/app/chat") {
			t.Errorf("serve transitively imports CLI dependency %s", dependency)
		}
	}
}
