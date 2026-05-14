//go:build !(linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris)

package termimage

func terminalCellSizeFromTTY() (int, int, bool) {
	return 0, 0, false
}
