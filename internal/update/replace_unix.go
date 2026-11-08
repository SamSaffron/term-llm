//go:build !windows

package update

import "os"

func replaceExecutable(tempPath, exePath string) error {
	if err := os.Chmod(tempPath, 0o755); err != nil {
		return err
	}
	return os.Rename(tempPath, exePath)
}
