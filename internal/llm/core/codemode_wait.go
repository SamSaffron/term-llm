package core

import "context"

// WithCodeModeWait parks shared tool execution permits during a passive wait.
// The per-cell call budget remains charged, and permits must be reacquired
// before the caller continues. Direct calls have no parking hook.
func WithCodeModeWait[T any](ctx context.Context, wait func() (T, error)) (value T, err error) {
	// Direct callers retain their existing cancellation/result semantics.
	// Only script-owned waits need permit reacquisition and its failure path.
	pause, _ := ctx.Value(codeModePauseKey{}).(func() (func() error, error))
	if pause == nil {
		return wait()
	}
	resume, err := pause()
	if err != nil {
		return value, err
	}
	value, err = wait()
	if resumed := resume(); err == nil {
		err = resumed
	}
	if err == nil {
		err = ctx.Err()
	}
	return
}
