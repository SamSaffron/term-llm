package llm

import "fmt"

// StreamIncompleteError reports a streaming response that ended before the
// protocol terminal marker arrived. This must be treated as a failed model
// response, not a successful completion with truncated output.
type StreamIncompleteError struct {
	Transport string
	Terminal  string
	Err       error
}

func (e *StreamIncompleteError) Error() string {
	transport := e.Transport
	if transport == "" {
		transport = "stream"
	}
	terminal := e.Terminal
	if terminal == "" {
		terminal = "terminal event"
	}
	msg := fmt.Sprintf("%s closed before %s", transport, terminal)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *StreamIncompleteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
