//go:build js

package procutil

// KillTaggedDescendants is a no-op on WebAssembly.
func KillTaggedDescendants(string, string) {}
