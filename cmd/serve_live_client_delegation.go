package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/live"
)

// liveClientDelegationResult is one terminal answer for a client-owned
// delegation. Exactly one of Output and Error is set; both are already trimmed,
// so a replay can be compared byte for byte against what was accepted.
type liveClientDelegationResult struct {
	Output string
	Error  string
}

// liveClientDelegation is one delegation this call published to its client.
//
// The entry outlives its waiter on purpose: a client retrying a result whose 200
// it never saw must be told "duplicate" rather than "no such delegation".
//
// Every field is read and written under liveSession.mu. done is closed exactly
// once, to release the waiter, and cleared in the same critical section, so it
// doubles as the guard against a second close.
type liveClientDelegation struct {
	id        string
	sessionID string
	input     string
	done      chan struct{}
	accepted  *liveClientDelegationResult
	abandoned bool
}

// abandonLocked releases a waiter without a result, because the call has ended.
func (e *liveClientDelegation) abandonLocked() {
	if e.done == nil {
		return
	}
	e.abandoned = true
	close(e.done)
	e.done = nil
}

// liveDelegationResultOutcome is how a POSTed result was resolved. The record
// decides; the handler alone maps it to HTTP.
type liveDelegationResultOutcome int

const (
	liveDelegationResultUnknown liveDelegationResultOutcome = iota
	liveDelegationResultAccepted
	liveDelegationResultDuplicate
	// liveDelegationResultConflict covers every known id that cannot take this
	// result: it contradicts one already accepted, names a different session, or
	// arrived when nobody was waiting for it any more.
	liveDelegationResultConflict
)

// completeClientDelegation resolves one POSTed result for delegationID.
//
// First writer wins, and the decision and the hand-off share one critical
// section, so two clients racing the same id cannot both be told they were
// accepted. sessionID is the client's optional consistency check; the binding the
// server captured when it published the request is the authority.
func (l *liveSession) completeClientDelegation(delegationID, sessionID string, result liveClientDelegationResult) liveDelegationResultOutcome {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clientDelegationLocked(delegationID)
	if entry == nil {
		return liveDelegationResultUnknown
	}
	if sessionID != "" && sessionID != entry.sessionID {
		return liveDelegationResultConflict
	}
	if entry.accepted != nil {
		if *entry.accepted == result {
			return liveDelegationResultDuplicate
		}
		return liveDelegationResultConflict
	}
	if entry.done == nil {
		// Nobody is waiting: the delegation timed out, was cancelled, or the call
		// ended. Absorbing the result would silently discard work the device
		// believes it delivered.
		return liveDelegationResultConflict
	}
	accepted := result
	entry.accepted = &accepted
	close(entry.done)
	entry.done = nil
	l.lastActivity = time.Now()
	return liveDelegationResultAccepted
}

// beginClientDelegation registers one pending delegation, bound to sessionID, and
// publishes its request event. It returns the channel closed when the delegation
// is resolved.
//
// Registration and publication share one critical section for the reason
// appendBindingEvent exists: the binding an event names is read under the lock
// that publishes it. sessionID is the caller's snapshot and must still be that
// binding here, so one value is the session checked for a running turn, the
// session the device is told to run against, and the session a client's
// consistency check is compared to.
func (l *liveSession) beginClientDelegation(delegationID, sessionID, input string) (<-chan struct{}, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return nil, errLiveCallEnded
	}
	if l.sessionID != sessionID {
		// The call was rebound between the caller's snapshot and this lock.
		// Publishing the snapshot would aim the device at the session the call just
		// left; publishing the new binding would mean the busy check ran against a
		// different one. Fail instead, and the voice model repeats the request.
		return nil, errors.New("the live call moved to another chat session before this request was sent")
	}
	if l.clientDelegationLocked(delegationID) != nil {
		// The controller claims each provider delegation id once per call, so this is
		// a host bug rather than a replayed provider event. Refusing keeps an earlier
		// waiter from being stranded and an earlier result from being reused.
		return nil, errors.New("this delegation is already known to the device")
	}
	entry := &liveClientDelegation{
		id: delegationID, sessionID: sessionID, input: input, done: make(chan struct{}),
	}
	// Delegations are strictly serial, so the newest entry is the only one that can
	// still have a waiter and dropping the oldest can never strand one.
	l.clientDelegations = append(l.clientDelegations, entry)
	if len(l.clientDelegations) > liveClientDelegationHistoryLimit {
		l.clientDelegations = l.clientDelegations[1:]
	}
	l.publishClientDelegationLocked(entry, false)
	return entry.done, nil
}

func (l *liveSession) clientDelegationLocked(delegationID string) *liveClientDelegation {
	for _, entry := range l.clientDelegations {
		if entry.id == delegationID {
			return entry
		}
	}
	return nil
}

// publishClientDelegationLocked emits one live.delegation_requested for entry.
// resend marks a repeat so a client that already has the id can drop it without
// re-executing the work.
func (l *liveSession) publishClientDelegationLocked(entry *liveClientDelegation, resend bool) {
	l.appendEventLocked(liveEventDelegationRequested, map[string]any{
		"session_id": entry.sessionID, "delegation_id": entry.id,
		"input": entry.input, "resend": resend,
	})
}

// resendClientDelegation re-publishes a still-pending request. Publishing counts
// as activity, so a long delegation with a quiet user is not reaped as idle.
func (l *liveSession) resendClientDelegation(delegationID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clientDelegationLocked(delegationID)
	if entry == nil || entry.done == nil {
		return
	}
	l.publishClientDelegationLocked(entry, true)
}

// finishClientDelegation retires the waiter for delegationID and reports how the
// delegation resolved. The entry and any accepted result stay for idempotent
// replay; only the waiter goes, so a later result is refused rather than
// delivered to nobody.
//
// Reading the outcome here, under the lock that accepted it, is what gives a
// result that landed in the same instant as a timeout or a cancellation
// precedence over the deadline: the device was already told 200, so the
// delegation must not also be spoken as failed.
func (l *liveSession) finishClientDelegation(delegationID string) (*liveClientDelegationResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clientDelegationLocked(delegationID)
	if entry == nil {
		return nil, false
	}
	entry.done = nil
	return entry.accepted, entry.abandoned
}

// serveLiveClientDelegator hands delegated work to the authenticated client that
// owns the call instead of running it as a chat turn.
//
// It implements live.Delegator only. Not implementing live.SteeringDelegator is
// the design: the controller then uses its serial lane, so there is at most one
// outstanding request on the device per call by construction, and a second spoken
// request waits in the controller queue rather than racing the first on a phone.
type serveLiveClientDelegator struct {
	server         *serveServer
	live           *liveSession
	resendInterval time.Duration
	timeout        time.Duration
}

// Run publishes one delegation to the client and waits for its result.
func (d *serveLiveClientDelegator) Run(ctx context.Context, request live.DelegationRequest, emit func(live.DelegationChunk)) error {
	input := strings.TrimSpace(request.Input)
	if input == "" {
		return errors.New("the voice delegation contained no request")
	}
	if d.server == nil || d.live == nil {
		return errors.New("the session runtime is unavailable")
	}
	delegationID := strings.TrimSpace(request.ID)
	if delegationID == "" {
		// Without an id the client cannot address a result back, and the host cannot
		// match one. Fail loudly rather than publish a request nobody can answer.
		return errors.New("the voice delegation has no identifier the device can answer")
	}
	sessionID := d.live.boundSession()
	if sessionID == "" {
		return errors.New("the live call is not bound to a chat session")
	}
	// Ask the host, not the device, whether the session is busy. The controller
	// already knows how to wait out a busy session — it speaks one notice and
	// retries — and letting the client discover a 409 from /v1/responses instead
	// would make client mode behave differently from server mode.
	if activeID := d.server.ensureResponseRuns().activeRunID(sessionID); activeID != "" {
		return live.ErrDelegationBusy
	}
	done, err := d.live.beginClientDelegation(delegationID, sessionID, input)
	if err != nil {
		return err
	}
	reason := d.wait(ctx, done, delegationID)
	accepted, abandoned := d.live.finishClientDelegation(delegationID)
	switch {
	case accepted != nil && accepted.Error != "":
		return errors.New(accepted.Error)
	case accepted != nil:
		emit(live.DelegationChunk{Channel: live.ChannelSpeakable, Text: accepted.Output})
		return nil
	case abandoned:
		return errors.New("the live call ended before the device answered")
	case reason != nil:
		return reason
	}
	// done only closes with a result or an abandonment recorded, so this is
	// unreachable — but reporting nil here would speak a silent success.
	return errors.New("the delegation ended without an answer")
}

// wait blocks until the delegation resolves, republishing the request meanwhile,
// and reports why the wait ended when it was not the device answering.
func (d *serveLiveClientDelegator) wait(ctx context.Context, done <-chan struct{}, delegationID string) error {
	resend := time.NewTicker(d.resendInterval)
	defer resend.Stop()
	timeout := time.NewTimer(d.timeout)
	defer timeout.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-resend.C:
			d.live.resendClientDelegation(delegationID)
		case <-timeout.C:
			return errors.New("the device did not answer this request in time")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// liveDelegationResultRequest is one terminal answer from the client.
type liveDelegationResultRequest struct {
	Output string `json:"output"`
	Error  string `json:"error"`
	// ResponseID is logged for correlation, never trusted, and ignored when a
	// replay is compared. SessionID is an optional consistency check against the
	// binding the server captured when it published the request.
	ResponseID string `json:"response_id"`
	SessionID  string `json:"session_id"`
}

// handleLiveDelegationResult accepts one client-executed delegation result.
//
// Authorization is the serve bearer token, applied by the same middleware as
// every other live route. live_id and delegation_id are correlation, not
// capabilities: delegation ids come from the provider and are broadcast on the
// call's SSE stream, so treating one as a secret would be false comfort.
func (s *serveServer) handleLiveDelegationResult(w http.ResponseWriter, r *http.Request, liveID, delegationID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	delegationID = strings.TrimSpace(delegationID)
	if delegationID == "" {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live delegation not found")
		return
	}
	record, ok := s.lookupLiveSession(liveID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
	var request liveDelegationResultRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, liveDelegationResultLimitBytes)).Decode(&request); err != nil {
		var overflow *http.MaxBytesError
		if errors.As(err, &overflow) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "the delegation result is too large")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	result, err := liveDelegationResult(request)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	switch record.completeClientDelegation(delegationID, strings.TrimSpace(request.SessionID), result) {
	case liveDelegationResultAccepted:
		if responseID := strings.TrimSpace(request.ResponseID); responseID != "" {
			log.Printf("[live] client delegation %s/%s answered by response %s", liveID, delegationID, responseID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case liveDelegationResultDuplicate:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true})
	case liveDelegationResultConflict:
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "this delegation is no longer accepting that result")
	default:
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live delegation not found")
	}
}

// liveDelegationResult validates one result body. An empty output is a failure,
// not a success with nothing to say, which matches how a delegated turn that
// produces no text is already treated.
//
// Trimming and capping happen here rather than at the comparison, so the accepted
// value and every later replay are normalised the same way: a retry that differs
// only in whitespace is the same answer, not a contradiction.
func liveDelegationResult(request liveDelegationResultRequest) (liveClientDelegationResult, error) {
	output, failure := strings.TrimSpace(request.Output), strings.TrimSpace(request.Error)
	switch {
	case output != "" && failure != "":
		return liveClientDelegationResult{}, errors.New("provide exactly one of output or error")
	case failure != "":
		// Unlike output, which the controller truncates before speaking, this text is
		// also published verbatim as the delegation's failure text, so it is capped
		// here rather than trusted at the 64 KiB body limit.
		if len(failure) > liveDelegationFailureLimitBytes {
			failure = strings.ToValidUTF8(failure[:liveDelegationFailureLimitBytes], "") + "…"
		}
		return liveClientDelegationResult{Error: failure}, nil
	case output != "":
		return liveClientDelegationResult{Output: output}, nil
	default:
		return liveClientDelegationResult{}, errors.New("provide exactly one of output or error, neither of them blank")
	}
}
