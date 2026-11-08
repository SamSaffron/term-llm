//go:build windows

package update

import "os"

// replaceExecutable swaps a running term-llm.exe. Windows locks a running
// image against deletion but allows renaming it, so move the current binary
// aside, install the new one, then best-effort delete the old image. The .old
// file may remain until a later cleanup; no automatic deletion is scheduled.
func replaceExecutable(tempPath, exePath string) error {
	if err := os.Rename(tempPath, exePath); err == nil {
		return nil
	}
	backup := exePath + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(exePath, backup); err != nil {
		return err
	}
	if err := os.Rename(tempPath, exePath); err != nil {
		_ = os.Rename(backup, exePath)
		return err
	}
	_ = os.Remove(backup)
	return nil
}
