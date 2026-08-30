package project

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Project resolution canonicalizes symlinks. On macOS, make test temp paths
	// use /private/var rather than the /var alias so stored fixtures match it.
	if tempRoot, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if err := os.Setenv("TMPDIR", filepath.Clean(tempRoot)); err != nil {
			fmt.Fprintf(os.Stderr, "set canonical TMPDIR: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
