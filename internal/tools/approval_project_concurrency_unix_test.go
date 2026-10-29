//go:build unix

package tools

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO gives an explicit barrier inside the cold approval data read, without
// production test hooks, subprocess timing assumptions, or sleeps.
func TestApprovalProjectBlockedColdLoadDoesNotBlockWarmLookup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	warm, cold := t.TempDir(), t.TempDir()
	for _, root := range []string{warm, cold} {
		runGitTestCommand(t, root, "init", "-q")
	}
	persistMakeApproval(t, warm)
	mgr := NewApprovalManager(NewToolPermissions())
	requireMakeApproval(t, mgr, warm, ProceedAlways)
	approvalFile, err := projectApprovalsFilePath(cold)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(approvalFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(approvalFile, 0o600); err != nil {
		t.Fatal(err)
	}
	coldDone := make(chan struct{})
	go func() {
		defer close(coldDone)
		mgr.getProjectApprovals(cold)
	}()
	// Opening the writer returns only when the loader has opened the reader.
	writer, err := os.OpenFile(approvalFile, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = writer.Close() // EOF releases the cold read, including on failure.
		<-coldDone
	}()
	warmDone := make(chan *ProjectApprovals, 1)
	go func() { warmDone <- mgr.getProjectApprovals(warm) }()
	select {
	case pa := <-warmDone:
		if pa == nil || len(pa.shellPatternsSnapshot()) != 1 {
			t.Fatal("warm approvals lost during unrelated cold load")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked cold approval load blocked unrelated cached approval")
	}
}
