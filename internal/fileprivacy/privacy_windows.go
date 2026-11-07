//go:build windows

package fileprivacy

import (
	"io/fs"
	"os"
)

// CheckFile documents Windows' profile-ACL assumption. Go's os.FileMode does
// not expose Windows ACLs, so this does not pretend to verify them.
func CheckFile(fs.FileInfo) error { return nil }

// CheckDir documents Windows' profile-ACL assumption for private directories.
func CheckDir(fs.FileInfo) error { return nil }

// ChmodFile applies the writable private-file mode. Windows os.Chmod only
// controls read-only attributes, but errors must still be surfaced.
func ChmodFile(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }

// ChmodDir applies the private-directory mode; errors are surfaced.
func ChmodDir(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }
