package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepDisplayPath(t *testing.T) {
	base := filepath.FromSlash("/work/repo")
	for _, tc := range []struct{ path, want string }{
		{"/work/repo/internal/a.go", filepath.FromSlash("internal/a.go")},
		{"/work/repo/a.go", "a.go"},
		{"/work/repo-other/a.go", filepath.FromSlash("/work/repo-other/a.go")},
		{"/work/a.go", filepath.FromSlash("/work/a.go")},
		{"/elsewhere/a.go", filepath.FromSlash("/elsewhere/a.go")},
		{"/work/repo", filepath.FromSlash("/work/repo")},
		{"relative/a.go", "relative/a.go"},
	} {
		if got := grepDisplayPath(base, filepath.FromSlash(tc.path)); got != tc.want {
			t.Errorf("grepDisplayPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	if got := grepDisplayPath("", filepath.FromSlash("/work/repo/a.go")); got != filepath.FromSlash("/work/repo/a.go") {
		t.Errorf("empty base changed path: %q", got)
	}
}

func grepOutput(t *testing.T, cfg *ToolConfig, args GrepArgs) string {
	t.Helper()
	out, err := NewGrepTool(nil, DefaultOutputLimits(), cfg).Execute(context.Background(), mustToolArgs(t, args))
	if err != nil {
		t.Fatalf("grep Execute: %v", err)
	}
	return out.Content
}

func TestGrepPathsOutsideWorkingDirStayAbsolute(t *testing.T) {
	base := t.TempDir()
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "x.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := grepOutput(t, &ToolConfig{BaseDir: base}, GrepArgs{Pattern: "needle", Path: outside})
	if !strings.Contains(out, filepath.Join(outside, "x.txt")) || strings.Contains(out, "[paths relative to:") {
		t.Fatalf("outside path not absolute, or spurious base marker:\n%s", out)
	}
}

func TestGrepFilesWithMatchesRelative(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "sub", "x.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := grepOutput(t, &ToolConfig{BaseDir: base}, GrepArgs{Pattern: "needle", FilesWithMatches: true})
	canonicalBase, _ := filepath.EvalSymlinks(base)
	if out != "[paths relative to: "+canonicalBase+"]\n"+filepath.Join("sub", "x.txt") {
		t.Fatalf("files_with_matches = %q", out)
	}
}

// Match paths are canonical, so a symlinked working directory must be
// canonicalized before comparing or every path would stay absolute.
func TestGrepRelativeThroughSymlinkedWorkingDir(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "x.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &ToolConfig{BaseDir: link}
	out := grepOutput(t, cfg, GrepArgs{Pattern: "needle"})
	if !strings.HasPrefix(out, "[paths relative to: "+real+"]\n") || !strings.Contains(out, "\nx.txt (1 match):") {
		t.Fatalf("symlinked base not relativized:\n%s", out)
	}
}

func TestGrepRankedOutputRelative(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, 0.6, 0.9)
	cfg := rerankTestConfig(t, withTypesafeKey)
	installRerankFactory(t, newScoringFake())
	tool := NewGrepTool(nil, DefaultOutputLimits(), &ToolConfig{BaseDir: dir}).WithReranker(NewGrepReranker(cfg))
	out := execGrep(t, tool, dir, map[string]any{"intent": "x"})
	if !strings.HasPrefix(out, "[paths relative to: "+dir+"]\n[intent rerank:") || strings.Contains(out, dir+string(filepath.Separator)) {
		t.Fatalf("ranked output not relative:\n%s", out)
	}
	if a, b := strings.Index(out, "\nf2.txt:"), strings.Index(out, "\nf1.txt:"); a < 0 || b < 0 || a > b {
		t.Fatalf("want relative headers in rank order (f2 then f1):\n%s", out)
	}
}
