//go:build windows

package tools

import (
	"context"
	"os/exec"
	"testing"
)

const createNewProcessGroup = 0x00000200

func TestPrepareToolCommandContextConfiguresProcessTreeCancellation(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	owned, err := prepareToolCommand(cmd)
	if err != nil {
		t.Fatalf("prepareToolCommand() error = %v", err)
	}
	defer owned.Close()

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatalf("SysProcAttr = %#v, want CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr)
	}
	if cmd.Cancel == nil {
		t.Fatal("expected process-tree cancellation hook for CommandContext")
	}
	if err := owned.Run(); err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
}

func TestPrepareToolCommandPlainCommandStartsWithoutCancelHook(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	owned, err := prepareToolCommand(cmd)
	if err != nil {
		t.Fatalf("prepareToolCommand() error = %v", err)
	}
	defer owned.Close()

	if cmd.Cancel != nil {
		t.Fatal("plain exec.Command must not receive a Cancel hook")
	}
	if err := owned.Run(); err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
}
