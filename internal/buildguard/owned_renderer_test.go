package buildguard

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate renderer dependency guard")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func requireFileMatch(t *testing.T, path, pattern string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !regexp.MustCompile(pattern).Match(contents) {
		t.Fatalf("%s does not match required pattern %q", path, pattern)
	}
}

type resolvedModule struct {
	Path    string
	Dir     string
	Replace *struct {
		Path string
		Dir  string
	}
}

func resolveModule(t *testing.T, root, modulePath string) resolvedModule {
	t.Helper()
	cmd := exec.Command("go", "list", "-mod=readonly", "-m", "-json", modulePath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve %s from root go.mod: %v\n%s", modulePath, err, output)
	}
	var module resolvedModule
	if err := json.NewDecoder(bytes.NewReader(output)).Decode(&module); err != nil {
		t.Fatalf("decode resolution for %s: %v", modulePath, err)
	}
	return module
}

func requireLocalModule(t *testing.T, root, modulePath, replacement string) {
	t.Helper()
	module := resolveModule(t, root, modulePath)
	if module.Path != modulePath {
		t.Fatalf("resolved module path = %q, want %q", module.Path, modulePath)
	}
	if module.Replace == nil {
		t.Fatalf("%s resolved without the required in-repository replacement", modulePath)
	}
	if module.Replace.Path != replacement {
		t.Fatalf("%s replacement = %q, want %q", modulePath, module.Replace.Path, replacement)
	}
	wantDir := filepath.Clean(filepath.Join(root, replacement))
	gotDir := filepath.Clean(module.Replace.Dir)
	if gotDir != wantDir {
		t.Fatalf("%s resolved to %q, want in-repository directory %q", modulePath, gotDir, wantDir)
	}
}

func TestReleaseArchivesRetainOwnedModuleLicenses(t *testing.T) {
	root := repositoryRoot(t)
	licensePaths := []string{
		"third_party/bubbletea/LICENSE",
		"third_party/ultraviolet/LICENSE",
		"internal/reflow/LICENSE",
	}
	for _, path := range licensePaths {
		path := path
		t.Run(path, func(t *testing.T) {
			contents, err := os.ReadFile(filepath.Join(root, path))
			if err != nil {
				t.Fatalf("read retained license: %v", err)
			}
			if !bytes.Contains(contents, []byte("MIT License")) || !bytes.Contains(contents, []byte("THE SOFTWARE IS PROVIDED \"AS IS\"")) {
				t.Fatalf("%s is not a complete retained MIT license", path)
			}
			requireFileMatch(t, filepath.Join(root, ".goreleaser.yml"), `(?m)^\s*-\s+`+regexp.QuoteMeta(path)+`\s*$`)
		})
	}
}

func TestOwnedRendererModulesCannotSilentlyResolveUpstream(t *testing.T) {
	root := repositoryRoot(t)
	requireLocalModule(t, root, "charm.land/bubbletea/v2", "./third_party/bubbletea")
	requireLocalModule(t, root, "github.com/charmbracelet/ultraviolet", "./third_party/ultraviolet")

	requireFileMatch(t, filepath.Join(root, "go.mod"), `(?m)^replace\s+charm\.land/bubbletea/v2\s+=>\s+\./third_party/bubbletea\s*$`)
	requireFileMatch(t, filepath.Join(root, "go.mod"), `(?m)^replace\s+github\.com/charmbracelet/ultraviolet\s+=>\s+\./third_party/ultraviolet\s*$`)
	requireFileMatch(t, filepath.Join(root, "third_party", "bubbletea", "go.mod"), `(?m)^module\s+charm\.land/bubbletea/v2\s*$`)
	requireFileMatch(t, filepath.Join(root, "third_party", "bubbletea", "go.mod"), `(?m)^replace\s+github\.com/charmbracelet/ultraviolet\s+=>\s+\.\./ultraviolet\s*$`)
	requireFileMatch(t, filepath.Join(root, "third_party", "ultraviolet", "go.mod"), `(?m)^module\s+github\.com/charmbracelet/ultraviolet\s*$`)
}
