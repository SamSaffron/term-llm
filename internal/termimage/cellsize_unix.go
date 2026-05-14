//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package termimage

import (
	"os"

	"golang.org/x/sys/unix"
)

func terminalCellSizeFromTTY() (int, int, bool) {
	fds := []int{int(os.Stdout.Fd()), int(os.Stderr.Fd()), int(os.Stdin.Fd())}
	for _, fd := range fds {
		if w, h, ok := cellSizeFromFD(fd); ok {
			return w, h, true
		}
	}
	if tty, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0); err == nil {
		defer tty.Close()
		if w, h, ok := cellSizeFromFD(int(tty.Fd())); ok {
			return w, h, true
		}
	}
	return 0, 0, false
}

func cellSizeFromFD(fd int) (int, int, bool) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws == nil || ws.Col == 0 || ws.Row == 0 || ws.Xpixel == 0 || ws.Ypixel == 0 {
		return 0, 0, false
	}
	cellW := int(ws.Xpixel) / int(ws.Col)
	cellH := int(ws.Ypixel) / int(ws.Row)
	if cellW <= 0 || cellH <= 0 {
		return 0, 0, false
	}
	return cellW, cellH, true
}
