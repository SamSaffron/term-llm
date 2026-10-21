//go:build !unix

package grepsearch

import "os"

// openNonblock is the portable open; the post-open regular-file check in
// openRegularFile provides the safety net where O_NONBLOCK is unavailable.
func openNonblock(path string) (*os.File, error) {
	return os.Open(path)
}
