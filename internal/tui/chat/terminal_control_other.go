//go:build !unix

package chat

import "fmt"

func writeTerminalControlSequenceToTTY(string) (int, error) {
	return 0, fmt.Errorf("controlling terminal cleanup is unsupported on this platform")
}
