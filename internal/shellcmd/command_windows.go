//go:build windows

package shellcmd

import (
	"os/exec"
	"strings"
	"syscall"
)

func configureCommand(cmd *exec.Cmd, shellPath string, args []string) {
	if !isCmd(shellPath) {
		return
	}
	commandAt := -1
	for i, arg := range args {
		if arg == "/c" && i+1 < len(args) {
			commandAt = i + 1
			break
		}
	}
	if commandAt < 0 {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	commandLine := syscall.EscapeArg(shellPath)
	for _, arg := range args[:commandAt] {
		commandLine += " " + syscall.EscapeArg(arg)
	}
	// cmd.exe does not parse the CommandLineToArgvW grammar used by Go's
	// EscapeArg. Wrap the whole /c command and quote the executable explicitly;
	// /s then strips only the outer pair and leaves the command's own quoting
	// intact.
	cmd.SysProcAttr.CmdLine = commandLine + ` "` + args[commandAt] + `"`
}

func isCmd(shellPath string) bool {
	name := strings.ToLower(shellPath)
	name = strings.TrimSuffix(name, ".exe")
	name = name[strings.LastIndexAny(name, `\/`)+1:]
	return name == "cmd"
}
