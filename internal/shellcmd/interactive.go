package shellcmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/procutil"
)

// InteractiveArgs returns the argv needed to start shellPath for an
// interactive /shell session. noRC suppresses shell startup files where the
// shell has a native switch; unsupported shells return an error rather than
// silently running user rc files.
func InteractiveArgs(shellPath string, noRC bool) ([]string, error) {
	if !noRC {
		return nil, nil
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(shellPath)), ".exe")
	switch name {
	case "powershell", "pwsh":
		return []string{"-NoLogo", "-NoProfile"}, nil
	case "cmd":
		return []string{"/d"}, nil
	case "zsh":
		return []string{"-f"}, nil
	case "bash":
		return []string{"--noprofile", "--norc"}, nil
	case "fish":
		return []string{"--no-config"}, nil
	case "csh", "tcsh":
		return []string{"-f"}, nil
	case "nu", "nushell":
		return []string{"--no-config-file"}, nil
	case "sh", "dash", "ash", "ksh", "mksh", "pdksh":
		return nil, nil
	default:
		return nil, fmt.Errorf("/shell --no-rc is not supported for shell %q", shellPath)
	}
}

// ApprovalCommand renders the exact shell invocation for approval evidence.
// The POSIX "-c command" form keeps its lossless preview and approval-cache key;
// other platforms show the real argv.
func ApprovalCommand(shellPath, command string, env map[string]string) string {
	args := Args(shellPath, command)
	if len(args) == 2 && args[0] == "-c" {
		return procutil.ShellEnvironmentCommand(command, shellPath, env)
	}
	return procutil.ShellEnvironmentCommandArgs(shellPath, args, env)
}
