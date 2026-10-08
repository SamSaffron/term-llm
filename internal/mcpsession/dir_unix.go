//go:build unix

package mcpsession

import (
	"fmt"
	"os"
	"syscall"
)

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
