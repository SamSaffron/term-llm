//go:build unix

package grepsearch

import (
	"os"
	"syscall"
)

// openNonblock opens path read-only with O_NONBLOCK so a FIFO swapped in
// after the precheck cannot block waiting for a writer.
func openNonblock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
