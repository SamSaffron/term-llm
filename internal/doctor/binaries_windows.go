//go:build windows

package doctor

// platformShellBinaries is empty on Windows: PowerShell/COMSPEC ship with the
// OS and are used by the shell tool and config value resolution.
func platformShellBinaries() []BinarySpec { return nil }
