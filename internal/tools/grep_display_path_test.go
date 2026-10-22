package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepDisplayPath(t *testing.T) {
	// filepath.Abs gives a volume on Windows, so these stay absolute there.
	abs := func(p string) string {
		a, err := filepath.Abs(filepath.FromSlash(p))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	base := abs("/work/repo")
	for _, tc := range []struct{ path, want string }{
		{abs("/work/repo/internal/a.go"), filepath.FromSlash("internal/a.go")},
		{abs("/work/repo/a.go"), "a.go"},
		{abs("/work/repo-other/a.go"), abs("/work/repo-other/a.go")},
		{abs("/work/a.go"), abs("/work/a.go")},
		{abs("/elsewhere/a.go"), abs("/elsewhere/a.go")},
		{base, base},
		{"relative/a.go", "relative/a.go"},
	} {
		if got := grepDisplayPath(base, tc.path); got != tc.want {
			t.Errorf("grepDisplayPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	if got := grepDisplayPath("", abs("/work/repo/a.go")); got != abs("/work/repo/a.go") {
		t.Errorf("empty base changed path: %q", got)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// A root base would only save one byte and is easy to misresolve.
func TestGrepRootWorkingDirStaysAbsolute(t *testing.T) {
	root, err := filepath.Abs(string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	tool := NewGrepTool(nil, DefaultOutputLimits(), &ToolConfig{BaseDir: root})
	if base := tool.grepDisplayBase(); base != "" {
		t.Fatalf("root working dir display base = %q, want none", base)
	}
}

// The marker's bytes come out of the budget handed to the formatter, so the
// total stays within maxOutputBytes (FormatRanked's cap is hard).
func TestFormatGrepDisplayChargesMarkerToBudget(t *testing.T) {
	base := canonicalTempDir(t)
	matches := []GrepMatch{{FilePath: filepath.Join(base, "a.go")}}
	marker := "[paths relative to: " + base + "]\n"
	var gotBudget int
	var gotPath string
	out := formatGrepDisplay(base, matches, func(m []GrepMatch, budget int) string {
		gotBudget, gotPath = budget, m[0].FilePath
		return "body"
	})
	if out != marker+"body" || gotBudget != maxOutputBytes-len(marker) || gotPath != "a.go" {
		t.Fatalf("out=%q budget=%d path=%q", out, gotBudget, gotPath)
	}
	if matches[0].FilePath != filepath.Join(base, "a.go") {
		t.Fatalf("caller's match mutated: %q", matches[0].FilePath)
	}
	// Nothing shortened: full budget, no marker.
	out = formatGrepDisplay(base, []GrepMatch{{FilePath: filepath.Join(canonicalTempDir(t), "b.go")}}, func(_ []GrepMatch, budget int) string {
		gotBudget = budget
		return "body"
	})
	if out != "body" || gotBudget != maxOutputBytes {
		t.Fatalf("outside: out=%q budget=%d", out, gotBudget)
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
	outside := canonicalTempDir(t)
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
	canonicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	if out != "[paths relative to: "+canonicalBase+"]\n"+filepath.Join("sub", "x.txt") {
		t.Fatalf("files_with_matches = %q", out)
	}
}

// Match paths are canonical, so a symlinked working directory must be
// canonicalized before comparing or every path would stay absolute.
func TestGrepRelativeThroughSymlinkedWorkingDir(t *testing.T) {
	real := canonicalTempDir(t)
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
