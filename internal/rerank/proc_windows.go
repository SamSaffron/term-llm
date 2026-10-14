//go:build windows

package rerank

import "os/exec"

func setProcAttrs(cmd *exec.Cmd) {}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
