package testutil

import (
	"os"
	"testing"
)

func TestProcessHasExited(t *testing.T) {
	if ProcessHasExited(os.Getpid()) {
		t.Fatal("current process reported as exited")
	}
	if !ProcessHasExited(1 << 30) {
		t.Fatal("nonexistent process reported as running")
	}
}

func TestProcStatState(t *testing.T) {
	AssertProcStatState(t, procStatState)
}
