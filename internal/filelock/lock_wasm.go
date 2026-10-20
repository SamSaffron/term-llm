//go:build js && wasm

package filelock

import (
	"errors"
	"fmt"
)

// Lock fails closed on js/wasm. These locks protect credentials, passkey state,
// and workspace trust; a browser host can provide a shared filesystem and run
// multiple instances, so pretending to hold an exclusive lock would be unsafe.
func Lock(path string) (func() error, error) {
	return nil, fmt.Errorf("filelock: lock %q unsupported on js/wasm: %w", path, errors.ErrUnsupported)
}

// TryLock fails closed on js/wasm; see Lock.
func TryLock(path string) (func() error, error) {
	return nil, fmt.Errorf("filelock: try lock %q unsupported on js/wasm: %w", path, errors.ErrUnsupported)
}
