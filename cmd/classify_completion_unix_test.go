//go:build unix

package cmd

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestClassifyAnswerCompletionDoesNotBlockOnFIFO pins that shell completion
// never opens a non-regular --questions path. Opening a FIFO blocks until a
// writer appears, which would freeze the user's shell on every <TAB>.
func TestClassifyAnswerCompletionDoesNotBlockOnFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "questions.yaml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	done := make(chan struct{})
	var got []string
	var directive string
	go func() {
		defer close(done)
		got, directive = classifyCompletions(t, "classify", "state", "--questions", fifo, "--answer", "")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("completion blocked on a FIFO --questions path")
	}
	assertClassifyCompletions(t, got, directive, nil)
}
