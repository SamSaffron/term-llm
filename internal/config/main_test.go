package config

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// TestMain keeps $(...) resolution tests on a POSIX shell. The product default
// on Windows is PowerShell, but these tests exercise the magic-value parser
// with POSIX command strings; Git for Windows provides bash on CI and dev
// machines.
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		if bash, err := exec.LookPath("bash"); err == nil {
			_ = os.Setenv("SHELL", bash)
		}
	}
	os.Exit(m.Run())
}
