//go:build unix

package mcpsession

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

const supported = true

var errUnsupported = errors.New("MCP session sockets are not supported on this platform")

// checkPrivateDir refuses socket directories another user could tamper with.
func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat MCP session socket dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("MCP session socket dir %s is not a directory", dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("MCP session socket dir %s is not owned by the current user", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("MCP session socket dir %s must not be accessible by group or others (mode %v)", dir, info.Mode().Perm())
	}
	return nil
}

// Stale reports whether path is a socket file nobody listens on. Only a
// refused connection counts: timeouts or resource errors may come from a busy
// live listener, and non-socket files are never considered stale.
func Stale(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return false
	}
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}
