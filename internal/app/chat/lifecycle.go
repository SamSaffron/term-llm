package chat

import tuichat "github.com/samsaffron/term-llm/internal/tui/chat"

// RestoreLifecycleOSC writes the reporter's restore sequence to the terminal.
func RestoreLifecycleOSC(reporter LifecycleReporter) {
	if reporter == nil {
		return
	}
	_, _ = tuichat.WriteTerminalControlSequence(reporter.RestoreOSC())
}

// RestoreLifecycleOSCToTTY restores the lifecycle OSC on the controlling TTY
// and closes the reporter.
func RestoreLifecycleOSCToTTY(reporter LifecycleReporter) {
	ForcedLifecycleCleanup(reporter, tuichat.WriteTerminalControlSequenceToTTY)
}

// ForcedLifecycleCleanup restores the OSC through write, then closes adapters.
func ForcedLifecycleCleanup(reporter LifecycleReporter, write func(string) (int, error)) {
	if reporter == nil {
		return
	}
	if write != nil {
		_, _ = write(reporter.RestoreOSC())
	}
	reporter.Close()
}
