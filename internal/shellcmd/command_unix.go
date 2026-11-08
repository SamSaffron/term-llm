//go:build !windows

package shellcmd

import "os/exec"

func configureCommand(*exec.Cmd, string, []string) {}
