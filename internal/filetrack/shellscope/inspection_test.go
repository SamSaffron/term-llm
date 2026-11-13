package shellscope

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestExpandPatternsPreservesLiteralEvidenceAndHidesGit(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	writeFixture(t, filepath.Join(root, "src", "main.go"), "package main")
	writeFixture(t, filepath.Join(root, ".git", "private.go"), "administrative")
	writeFixture(t, filepath.Join(root, ".github", "workflow.go"), "visible")
	got := ExpandPatterns(root, []string{"", " missing.go ", "**/*.go", ".GIT/config"})
	want := []Candidate{
		{Path: filepath.Join(root, "missing.go"), Literal: true},
		{Path: filepath.Join(root, ".github", "workflow.go")},
		{Path: filepath.Join(root, "src", "main.go")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %+v, want %+v", got, want)
	}
	if got := NormalizePatterns([]string{"  ", " x ", " y"}); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Fatalf("normalized patterns = %v", got)
	}
}

func TestRepoResolverRetainsScopedOwnershipBoundaries(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	writeFixture(t, filepath.Join(root, ".git"), "gitdir: metadata")
	work := filepath.Join(root, "workspace")
	nested := filepath.Join(work, "nested")
	writeFixture(t, filepath.Join(nested, ".git"), "gitdir: nested-metadata")
	resolver := NewRepoResolver()
	if got := resolver.OwningRepo(work); got != root {
		t.Fatalf("workspace repository = %q, want %q", got, root)
	}
	if got := resolver.OwningRepoForCandidate(filepath.Join(nested, "new.txt"), work, root); got != nested {
		t.Fatalf("nested repository = %q, want %q", got, nested)
	}
	if got := resolver.OwningRepoForCandidate(filepath.Join(root, "unrelated", "new.txt"), work, root); got != "" {
		t.Fatalf("unrelated path inherited common-ancestor repository %q", got)
	}
	if PathWithinRoot(root+"-sibling", root) || !PathWithinRoot(filepath.Join(root, "child"), root) {
		t.Fatal("path containment lost separator boundary")
	}
}

func fixtureGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func TestGitInspectionRetainsBoundsAndIndexBaseline(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	fixtureGit(t, root, "init", "-q")
	big := filepath.Join(root, "big.txt")
	small := filepath.Join(root, "small.txt")
	ignored := filepath.Join(root, "ignored.txt")
	writeFixture(t, big, strings.Repeat("x", 100))
	writeFixture(t, small, "index")
	writeFixture(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	fixtureGit(t, root, "add", ".")
	writeFixture(t, small, "worktree")
	writeFixture(t, ignored, "not a candidate")
	ctx := context.Background()
	got := GitShowIndexBatch(ctx, root, []string{big, small}, 16, 2, 16)
	if len(got) != 1 || string(got[small]) != "index" {
		t.Fatalf("bounded index content = %v", got)
	}
	if ignoredPaths := GitIgnoredCandidates(ctx, root, []string{small, ignored}); !ignoredPaths[ignored] || ignoredPaths[small] {
		t.Fatalf("ignored paths = %v", ignoredPaths)
	}
	if status := GitStatusPorcelain(ctx, root); status == nil || !strings.HasSuffix(status[small], "M") {
		t.Fatalf("working tree status = %v", status)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := GitStatusPorcelain(cancelled, root); got != nil {
		t.Fatalf("cancelled probe returned evidence: %v", got)
	}
}

func TestDrainGitBatchObjectRequiresExactSeparator(t *testing.T) {
	for _, tc := range []struct {
		input string
		valid bool
	}{
		{"blob\nnext", true},
		{"blob!next", false},
		{"blo", false},
	} {
		if got := drainGitBatchObject(bufio.NewReader(strings.NewReader(tc.input)), 4); got != tc.valid {
			t.Errorf("drain %q = %v, want %v", tc.input, got, tc.valid)
		}
	}
}
