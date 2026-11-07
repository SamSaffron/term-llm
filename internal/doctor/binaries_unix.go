//go:build !windows

package doctor

func platformShellBinaries() []BinarySpec {
	return []BinarySpec{{Name: "sh", Missing: SeverityError, Purpose: "the shell tool and $(...) config value resolution"}}
}
