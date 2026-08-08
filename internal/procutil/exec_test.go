package procutil

import (
	"os/exec"
	"testing"
)

func TestConfigureDetachedCommandDetachesControllingTerminal(t *testing.T) {
	cmd := exec.Command("sh", "-c", "true")
	ConfigureDetachedCommand(cmd)

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr = %#v, want Setsid enabled", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid must not be combined with Setsid")
	}
}

func TestConfigureCommandProcessGroupPreservesControllingTerminal(t *testing.T) {
	cmd := exec.Command("sh", "-c", "true")
	ConfigureCommandProcessGroup(cmd)

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %#v, want Setpgid enabled", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Setsid {
		t.Fatal("credential-capable process group must not detach its terminal")
	}
}
