package live

import (
	"errors"
	"strings"
)

// controlRejectionText is what the voice model hears when a delegated request
// could not be handled. It always states the reason, because the voice model is the
// only channel that can tell the user what happened.
//
// It no longer repeats a wire form. Nothing instructs the model to address a
// request any more — the host routes every delegation itself — so there is no
// syntax to teach back.
func controlRejectionText(err error) string {
	message := "unknown error"
	if err != nil {
		if text := strings.TrimSpace(err.Error()); text != "" {
			message = text
		}
	}
	return "That request could not be handled: " + message + ". Tell the user plainly and let them ask again."
}

// controlFailure marks a request the router proved was session management and then
// could not finish.
//
// It is the opposite of a routing error. A routing error means triage never reached
// a verdict, so the request is still presumed to be work and falling open to the
// session lane is correct. This means the verdict was reached and acted on, so
// falling open would write a session-management request into the transcript this
// lane exists to keep clean — which is the bug the lane was built to fix.
type controlFailure struct{ err error }

func (f controlFailure) Error() string { return f.err.Error() }

func (f controlFailure) Unwrap() error { return f.err }

// FailControl tags err as a control request that ran and failed, so the host reports
// it to the voice model rather than retrying it as ordinary work.
func FailControl(err error) error {
	if err == nil {
		return nil
	}
	return controlFailure{err: err}
}

// isControlFailure reports whether err ended a request already known to be session
// management.
func isControlFailure(err error) bool {
	var failure controlFailure
	return errors.As(err, &failure)
}

// controlRefusal marks an error the host chose to refuse: the request was declined
// before anything ran. It is the same error the voice model hears, carrying the
// same message, but it is tagged so the controller can keep it off the browser's
// error channel — this text is guidance, not a user-facing failure.
type controlRefusal struct{ err error }

func (r controlRefusal) Error() string { return r.err.Error() }

func (r controlRefusal) Unwrap() error { return r.err }

// refuseControl tags err as a refusal of a request the host declined to accept.
// The controller uses it for the one refusal this lane still has — a request that
// arrived when the routing worker was already at capacity — so the browser sees a
// terminal, textless state rather than an error the user did not cause.
func refuseControl(err error) error {
	if err == nil {
		return nil
	}
	return controlRefusal{err: err}
}

// RefuseControl is refuseControl for hosts. A host that declines a delegation
// before anything runs — because it cannot start the work at all — tags the reason
// here and supplies the text the voice model hears.
//
// The routing lane does not use it: an unusable router falls through to the session
// lane with the original request instead, because a user's work must never be
// refused or lost because triage was unavailable.
func RefuseControl(err error) error { return refuseControl(err) }

// isControlRefusal reports whether err is a host refusal rather than a failed
// execution. An error an executor returned untagged is not one: something ran and
// went wrong, and the user benefits from hearing about it.
func isControlRefusal(err error) bool {
	var refusal controlRefusal
	return errors.As(err, &refusal)
}
