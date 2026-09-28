//go:build linux || windows

package tools

// Linux uses /proc for PID and start-time proof; Windows remains conservative.
func ownerPIDTerminated(int) bool { return false }
