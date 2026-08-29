//go:build unix

package chat

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func writeTerminalControlSequenceToTTY(sequence string) (int, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	defer tty.Close()
	if err := unix.SetNonblock(int(tty.Fd()), true); err != nil {
		return 0, fmt.Errorf("make terminal cleanup write nonblocking: %w", err)
	}
	return tty.WriteString(sequence)
}
