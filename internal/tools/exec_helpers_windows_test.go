//go:build windows

package tools

import (
	"os/exec"
	"testing"
)

const createNewProcessGroup = 0x00000200

func TestPrepareToolCommandDetachesProcessGroup(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	cleanup, err := prepareToolCommand(cmd)
	if err != nil {
		t.Fatalf("prepareToolCommand() error = %v", err)
	}
	defer cleanup()

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatalf("SysProcAttr = %#v, want CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr)
	}
	if cmd.Cancel == nil {
		t.Fatal("expected process-tree cancellation hook")
	}
}
