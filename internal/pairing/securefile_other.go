//go:build !unix

package pairing

import (
	"io/fs"
	"os"
)

// checkOwner is a no-op where Unix ownership is unavailable.
func checkOwner(string, fs.FileInfo) error { return nil }

// openNoFollow opens path read-only; readPrivateFile still rejects a swapped
// file via os.SameFile.
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
