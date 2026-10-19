//go:build !windows

package reload

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExecutableCapturedBeforeSymlinkReplacement(t *testing.T) {
	const probe = "TERM_LLM_TEST_RELOAD_EXECUTABLE"
	if os.Getenv(probe) != "" {
		invoked := os.Args[0]
		if Executable() != invoked {
			t.Fatalf("did not preserve invocation symlink: %q != %q", Executable(), invoked)
		}
		if err := os.Remove(invoked); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(filepath.Dir(invoked), "new-version"), invoked); err != nil {
			t.Fatal(err)
		}
		if Executable() != invoked {
			t.Fatalf("recomputed executable after replacement: %q", Executable())
		}
		return
	}
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "installed")
	if err := os.Symlink(current, link); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(link, "-test.run=^TestExecutableCapturedBeforeSymlinkReplacement$")
	child.Env = append(os.Environ(), probe+"=1")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("symlink capture: %v\n%s", err, output)
	}
}
