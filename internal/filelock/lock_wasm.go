//go:build js || wasip1

package filelock

// Lock is a no-op on WebAssembly: a single-threaded host has no shared
// filesystem to coordinate with.
func Lock(path string) (func() error, error) {
	return func() error { return nil }, nil
}

// TryLock is a no-op on WebAssembly.
func TryLock(path string) (func() error, error) { return Lock(path) }
