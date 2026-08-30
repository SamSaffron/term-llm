package gitdiff

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// macOS exposes /var as a symlink to /private/var, while Git reports its
	// canonical worktree path. Keep temporary fixtures in that same namespace.
	if tempRoot, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if err := os.Setenv("TMPDIR", filepath.Clean(tempRoot)); err != nil {
			fmt.Fprintf(os.Stderr, "set canonical TMPDIR: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
