//go:build !windows

package fileprivacy

import (
	"fmt"
	"io/fs"
	"os"
)

// CheckFile enforces the Unix owner-only file policy. Modes such as 0400 and
// 0600 pass; any group/other permission bits fail.
func CheckFile(info fs.FileInfo) error {
	return checkOwnerOnly("file", info)
}

// CheckDir enforces the Unix owner-only directory policy.
func CheckDir(info fs.FileInfo) error {
	return checkOwnerOnly("directory", info)
}

func checkOwnerOnly(kind string, info fs.FileInfo) error {
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s permissions %04o are accessible by group or others; require owner-only", kind, perm)
	}
	return nil
}

// ChmodFile applies an owner-only file mode.
func ChmodFile(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }

// ChmodDir applies an owner-only directory mode.
func ChmodDir(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }
