//go:build windows

package procutil

// KillTaggedDescendants is a no-op on Windows: the owned Job Object reaps the
// process tree, so environment tagging is not needed.
func KillTaggedDescendants(string, string) {}
