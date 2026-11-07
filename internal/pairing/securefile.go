package pairing

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/fileprivacy"
)

const maxStateFileBytes = 16 << 20

// ensurePrivateDir creates dir (and missing parents) with 0700 and then
// verifies the leaf: it must be a real directory (not a symlink), owned by the
// current user, and on Unix not accessible to group or others. Ancestors are
// not inspected, so shared parents such as /tmp remain usable.
func ensurePrivateDir(dir string) error {
	if err := fileprivacy.MkdirAll(dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s: state directory must be a real directory, not a symlink", dir)
	}
	if err := fileprivacy.CheckDir(info); err != nil {
		return fmt.Errorf("%s: directory permissions are too open (want 0700): %w", dir, err)
	}
	if err := checkOwner(dir, info); err != nil {
		return err
	}
	return nil
}

// testHookAfterLstat, when set by tests, runs between inspecting and opening
// a private file.
var testHookAfterLstat func(path string)

// readPrivateFile reads a regular, non-symlink file owned by the current user
// that is not accessible to group or others. The opened descriptor must be the
// same file that was inspected, so a swap between Lstat and Open is rejected.
// A missing file returns fs.ErrNotExist.
func readPrivateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := checkPrivateFile(path, info); err != nil {
		return nil, err
	}
	if testHookAfterLstat != nil {
		testHookAfterLstat(path)
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s: file changed while opening", path)
	}
	if err := checkPrivateFile(path, opened); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStateFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxStateFileBytes {
		return nil, fmt.Errorf("%s: file too large", path)
	}
	return data, nil
}

func checkPrivateFile(path string, info fs.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", path)
	}
	if err := fileprivacy.CheckFile(info); err != nil {
		return fmt.Errorf("%s: permissions are too open (want 0600): %w", path, err)
	}
	return checkOwner(path, info)
}

// writePrivateFile atomically replaces path with data using the shared
// private-file policy and durable atomic replacement.
func writePrivateFile(path string, data []byte) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := fileprivacy.WriteFileAtomic(path, data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// withFileLock runs fn while holding a cross-process lock beside path.
func withFileLock(path string, fn func() error) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	unlock, err := filelock.Lock(path + ".lock")
	if err != nil {
		return err
	}
	fnErr := fn()
	if uerr := unlock(); uerr != nil && fnErr == nil {
		return uerr
	}
	return fnErr
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
