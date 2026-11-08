//go:build js && wasm

package procutil

import (
	"os/exec"
	"syscall"
)

type jsPlatformCommand struct{ cmd *exec.Cmd }

func configurePlatformCommand(cmd *exec.Cmd, _ commandMode, _ *Command) (platformCommand, error) {
	return &jsPlatformCommand{cmd: cmd}, nil
}

func (p *jsPlatformCommand) start(*Command) error { return p.cmd.Start() }

func (p *jsPlatformCommand) signal(syscall.Signal) error { return nil }

func (p *jsPlatformCommand) cancel(*Command) error { return nil }

func (p *jsPlatformCommand) close(*Command) error { return nil }
