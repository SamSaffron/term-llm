package mentions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildWalkIncludesHiddenSynthesizesDirectoriesAndSkipsVCS(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(".hidden/config")
	mustWrite("internal/llm/types.go")
	mustWrite(".hg/store/secret")

	s, err := Build(context.Background(), root, DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Kind{}
	for _, c := range s.Candidates {
		got[c.Path] = c.Kind
	}
	for _, want := range []string{".hidden", ".hidden/config", "internal", "internal/llm", "internal/llm/types.go"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q in %#v", want, got)
		}
	}
	if _, ok := got[".hg/store/secret"]; ok {
		t.Fatal("indexed VCS metadata")
	}
}

func TestBuildGitExcludesIgnoredAndDeletedTracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".gitignore", "tracked.txt")
	run("commit", "-qm", "base")
	run("update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(gitOutput(t, root, "rev-parse", "HEAD"))+",vendor/sub")
	if err := os.MkdirAll(filepath.Join(root, "vendor", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "sub", "inside.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Build(context.Background(), root, DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range s.Candidates {
		got[c.Path] = true
	}
	if got["tracked.txt"] {
		t.Fatal("deleted tracked path was indexed")
	}
	if got["ignored.txt"] {
		t.Fatal("ignored path was indexed")
	}
	if !got["visible.txt"] {
		t.Fatal("untracked visible path missing")
	}
	if !got["vendor/sub"] || got["vendor/sub/inside.go"] {
		t.Fatalf("submodule should be directory-only, got %#v", got)
	}
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func BenchmarkBuildWalk10K(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 10_000; i++ {
		path := filepath.Join(root, "src", "pkg", string(rune('a'+i%26)), "file-"+fmtInt(i)+".go")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Build(context.Background(), root, DefaultBuildOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

func fmtInt(v int) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	buf := [20]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%10]
		v /= 10
	}
	return string(buf[i:])
}

func TestBuildGitFailureDoesNotFallBackToIgnoredWalk(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	discoverGitForBuild = func(context.Context, string, *candidateCollector) error {
		return errors.New("forced git discovery failure")
	}
	t.Cleanup(func() { discoverGitForBuild = discoverGit })

	if snapshot, err := Build(context.Background(), root, DefaultBuildOptions()); err == nil {
		for _, candidate := range snapshot.Candidates {
			if candidate.Path == ".env" {
				t.Fatal("gitignored secret was exposed by walk fallback")
			}
		}
		t.Fatal("broken git worktree unexpectedly downgraded to walk discovery")
	}
}

func TestBuildWithoutGitFailsClosedOnlyWhenGitMetadataExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH executable semantics differ on Windows")
	}
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "visible.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Build(context.Background(), plain, DefaultBuildOptions())
	if err != nil {
		t.Fatalf("plain directory without git did not use walk: %v", err)
	}
	if snapshot.Source != "walk" {
		t.Fatalf("plain source = %q, want walk", snapshot.Source)
	}

	worktree := t.TempDir()
	if err := os.Mkdir(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), worktree, DefaultBuildOptions()); err == nil {
		t.Fatal("Git worktree without git executable fell back to an unfiltered walk")
	}
}

func TestBuildEnforcesLimitsAndWalkSafety(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(root, fmtInt(i)+".txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "bad\nname"), nil, 0o644); err != nil && runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "0.txt"), filepath.Join(root, "linked-file")); err != nil && !errors.Is(err, os.ErrPermission) {
		// Symlink support is platform/filesystem dependent; direct safety checks
		// below still cover rejection when links cannot be created.
		t.Logf("symlink unavailable: %v", err)
	}

	snapshot, err := Build(context.Background(), root, BuildOptions{MaxCandidates: 3, MaxPathBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Truncated || len(snapshot.Candidates) > 3 {
		t.Fatalf("candidate limit not enforced: truncated=%v len=%d", snapshot.Truncated, len(snapshot.Candidates))
	}
	byteLimited, err := Build(context.Background(), root, BuildOptions{MaxCandidates: 100, MaxPathBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !byteLimited.Truncated || byteLimited.PathBytes > 100 {
		t.Fatalf("path-byte limit not enforced: truncated=%v bytes=%d", byteLimited.Truncated, byteLimited.PathBytes)
	}
	for _, candidate := range snapshot.Candidates {
		if strings.Contains(candidate.Path, "\n") || candidate.Path == "linked-file" {
			t.Fatalf("unsafe walk candidate indexed: %q", candidate.Path)
		}
	}

	for _, path := range []string{"a/../b.txt", "../secret", "bad\x00name", "bad\nname", string([]byte{'b', 'a', 'd', 0xff})} {
		clean, ok := cleanSafeRelativePath(path)
		if path == "a/../b.txt" {
			if !ok || clean != "b.txt" {
				t.Fatalf("safe cleaned path = %q, %v", clean, ok)
			}
			continue
		}
		if ok {
			t.Fatalf("unsafe path accepted: %q -> %q", path, clean)
		}
	}
}

func TestBuildCancellationDuringGitProbeReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Build(ctx, t.TempDir(), DefaultBuildOptions())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Build error = %v, want context.Canceled", err)
	}
}

func TestBuildWalkSkipsSymlinkedFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "linked-file")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Build(context.Background(), root, DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range snapshot.Candidates {
		if strings.HasPrefix(candidate.Path, "linked-") {
			t.Fatalf("symlink candidate indexed: %q", candidate.Path)
		}
	}
}
