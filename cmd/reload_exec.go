//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// execReload shares the explicit environment handoff with serving. Session IDs
// and drafts stay in SQLite; argv is unchanged across repeated replacements.
func execReload(id, service string) error {
	exe := chatReloadExecutable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return err
		}
	}
	return syscall.Exec(exe, append([]string(nil), os.Args...), webExecEnviron(id, service))
}
