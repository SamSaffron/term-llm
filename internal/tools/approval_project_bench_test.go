package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkApprovalProjectWarm includes fresh identity revalidation, not just a
// data-cache map hit. Git is removed from PATH before measurement, so a cached
// approval must remain usable without any successful Git subprocesses.
func BenchmarkApprovalProjectWarm(b *testing.B) {
	b.Setenv("XDG_CONFIG_HOME", b.TempDir())
	root := b.TempDir()
	git := func(args ...string) {
		b.Helper()
		if out, err := runGitTestCommandAllowError(root, args...); err != nil {
			b.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "--allow-empty", "-q", "-m", "init")
	linked := filepath.Join(b.TempDir(), "linked")
	git("worktree", "add", "--detach", linked, "HEAD")
	nested := filepath.Join(root, "one", "two", "three")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		b.Fatal(err)
	}
	pa, err := LoadProjectApprovals(root)
	if err != nil {
		b.Fatal(err)
	}
	if err := pa.ApproveShellPattern("make *"); err != nil {
		b.Fatal(err)
	}
	mgr := NewApprovalManager(NewToolPermissions())
	paths := []struct{ name, path string }{
		{"root", root},
		{"nested", nested},
		{"missing", filepath.Join(nested, "new", "file")},
		{"worktree", linked},
	}
	for _, tc := range paths {
		if pa := mgr.getProjectApprovals(tc.path); pa == nil || len(pa.shellPatternsSnapshot()) != 1 {
			b.Fatalf("warming %s failed", tc.name)
		}
	}
	b.Setenv("PATH", b.TempDir())
	for _, tc := range paths {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if mgr.getProjectApprovals(tc.path) == nil {
					b.Fatal("warm approval lost")
				}
			}
		})
	}
}
