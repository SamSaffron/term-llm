//go:build windows

package procutil

// KillTaggedDescendants is a no-op on Windows: the process tree is reaped with
// taskkill /T, which does not depend on environment tagging.
func KillTaggedDescendants(string, string) {}
