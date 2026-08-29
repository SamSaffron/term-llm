package chat

import (
	"fmt"
	"os"
)

const terminalControlSequenceMaxBytes = 4096

// WriteTerminalControlSequence writes bounded shutdown/restore control output
// after Bubble Tea has released renderer ownership. Live lifecycle sequences
// are returned as tea.Raw commands by chatProgramModel instead.
func WriteTerminalControlSequence(sequence string) (int, error) {
	return writeTerminalControlSequence(sequence)
}

// WriteTerminalControlSequenceToTTY writes bounded forced-exit cleanup only to
// the controlling terminal. It deliberately avoids stdout while Bubble Tea may
// still own that stream.
func WriteTerminalControlSequenceToTTY(sequence string) (int, error) {
	if sequence == "" {
		return 0, nil
	}
	if len(sequence) > terminalControlSequenceMaxBytes {
		return 0, fmt.Errorf("terminal control sequence exceeds %d bytes", terminalControlSequenceMaxBytes)
	}
	return writeTerminalControlSequenceToTTY(sequence)
}

func writeTerminalControlSequence(sequence string) (int, error) {
	if sequence == "" {
		return 0, nil
	}
	if len(sequence) > terminalControlSequenceMaxBytes {
		return 0, fmt.Errorf("terminal control sequence exceeds %d bytes", terminalControlSequenceMaxBytes)
	}
	if written, err := WriteTerminalControlSequenceToTTY(sequence); err == nil {
		return written, nil
	}
	return os.Stdout.WriteString(sequence)
}
