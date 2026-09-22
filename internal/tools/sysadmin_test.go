package tools

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
)

func TestBuiltinSysadminShellAllowlist(t *testing.T) {
	// Isolate registry discovery so local/user overrides cannot shadow the bundle.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Chdir(home)
	registry, err := agents.NewRegistry(agents.RegistryConfig{UseBuiltin: true})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := registry.Get("sysadmin")
	if err != nil {
		t.Fatal(err)
	}
	perms := NewToolPermissions()
	for _, pattern := range agent.Shell.Allow {
		if err := perms.AddShellPattern(pattern); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		command string
		want    bool
	}{
		{"systemctl status nginx", true},
		{"df -h /", true},
		{"cat /etc/os-release", true},
		{"ps -p 1 -o comm=", true},
		{"sv status /etc/service/telegram", true},
		{"docker logs web --tail 50", true},
		{"apt-cache show nginx", true},
		{"apt-cache policy nginx", true},
		{"apt-cache search nginx", true},
		{"sudo -n true", true},
		{"sudo -n -l", true},
		{"systemctl restart nginx", false},
		{"journalctl --vacuum-size=1M", false},
		{"rg --pre /tmp/x foo /etc", false},
		{"rm -rf /tmp/x", false},
		{"sudo -n cat /etc/shadow", false},
		{"sudo -n journalctl --rotate", false},
		{"sudo -n systemctl status nginx", false},
		{"sudo -n true extra", false},
		{"sudo -n -l cat /etc/shadow", false},
		{"apt-cache gencaches", false},
		{"dd if=/dev/zero of=/tmp/x", false},
		{"file -C -m /tmp/magic", false},
		{"rpm -qa --pipe /tmp/x", false},
		{"dnf list --setopt=pluginpath=/tmp/plugins", false},
		{"dnf info --setopt=logdir=/tmp/logs nginx", false},
		{"pacman -Siy nginx", false},
		{"brew info --github nginx", false},
		{"podman ps --cpu-profile=/tmp/profile", false},
		{"podman logs web --memory-profile=/tmp/profile", false},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			if got := unfilteredConfiguredShellAllowedForTest(perms, tt.command); got != tt.want {
				t.Errorf("shell allowlist match = %v, want %v", got, tt.want)
			}
		})
	}
}
