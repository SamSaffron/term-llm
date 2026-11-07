// Package fileprivacy owns owner-only filesystem policy for application state:
// private file/directory modes, permission checks, and atomic private-file
// creation. Platform ACL specifics stay behind build-tagged helpers; callers
// retain their type, symlink, ownership, size, and same-file contracts.
package fileprivacy

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/atomicfile"
)

const (
	// FileMode is the mode for application-owned private files.
	FileMode fs.FileMode = 0o600
	// DirMode is the mode for application-owned private directories.
	DirMode fs.FileMode = 0o700
)

// MkdirAll creates missing private directories. Existing directories are not
// relaxed or silently tightened; callers that own an existing directory can call
// ChmodDir when adoption is intentional.
func MkdirAll(path string) error {
	return os.MkdirAll(path, DirMode)
}

// CreateTemp creates a private temporary file in dir and applies FileMode.
// The caller owns the file and is responsible for closing/removing it.
func CreateTemp(dir, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := ChmodFile(file.Name(), FileMode); err != nil {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
		return nil, err
	}
	return file, nil
}

// WriteFileAtomic replaces path with data by creating a private temporary file,
// syncing it, and using atomicfile.Replace for durable replacement. The parent
// directory must already exist and satisfy its owner's directory policy.
func WriteFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err = atomicfile.Replace(tmpPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	return nil
}
