//go:build unix

package pairing

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkOwner requires path to be owned by the effective user.
func checkOwner(path string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot determine owner", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s: owned by uid %d, not the current user", path, st.Uid)
	}
	return nil
}

// openNoFollow opens path read-only without following a final symlink.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
