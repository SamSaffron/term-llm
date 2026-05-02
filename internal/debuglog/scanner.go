package debuglog

import (
	"bufio"
	"io"
)

const (
	// Most debug log lines are small event records; start with Scanner's
	// default-sized buffer and retain the old 1 MiB ceiling for large payloads.
	debugLogScannerInitialBuffer = 4 * 1024
	debugLogScannerMaxBuffer     = 1024 * 1024
)

func newDebugLogScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, debugLogScannerInitialBuffer), debugLogScannerMaxBuffer)
	return scanner
}
