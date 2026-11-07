//go:build !js && !windows

package tools

import (
	"os/exec"
	"testing"
)

func TestPrepareToolCommandDetachesControllingTerminal(t *testing.T) {
	cmd := exec.Command("sh", "-c", "true")
	cleanup, err := prepareToolCommand(cmd)
	if err != nil {
		t.Fatalf("prepareToolCommand() error = %v", err)
	}
	defer cleanup()

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr = %#v, want Setsid enabled", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid must not be combined with Setsid")
	}
}
