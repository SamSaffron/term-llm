package codemode

import (
	"fmt"
	"io"
	"os"
)

func readSnapshotFile(root *os.Root, path string, limit int) ([]byte, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	file, err := root.OpenFile(path, os.O_RDONLY|snapshotNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// Recheck the OPENED inode, not only the name: a racing rename must not
	// substitute a FIFO/device or bypass size limits after the initial Lstat.
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("file exceeds size limit (%d bytes): %s", limit, path)
	}
	b, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("file grew past size limit: %s", path)
	}
	return b, nil
}
