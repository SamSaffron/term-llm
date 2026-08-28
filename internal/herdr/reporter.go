// Package herdr reports term-llm chat lifecycle changes to a containing Herdr
// pane. It deliberately has no dependency on Herdr's socket protocol: the
// release-matched Herdr CLI is the portable integration boundary.
package herdr

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	source = "custom:term-llm"
	agent  = "term-llm"

	commandTimeout = 2 * time.Second
	closeTimeout   = 500 * time.Millisecond
)

type commandRunner func(context.Context, string, ...string) error
type binaryFinder func(string) (string, error)

type eventKind uint8

const (
	reportEvent eventKind = iota
	releaseEvent
)

type event struct {
	kind      eventKind
	state     string
	sessionID string
}

// Reporter sends ordered, best-effort lifecycle reports to Herdr. It is only
// active when launched from a Herdr pane, so callers can use it unconditionally
// without making Herdr a runtime dependency.
type Reporter struct {
	binPath string
	paneID  string
	run     commandRunner

	mu        sync.Mutex
	closed    bool
	reported  bool
	lastState string
	lastID    string
	queue     []event
	wake      chan struct{}
	done      chan struct{}
}

// NewReporterFromEnv returns a reporter only for a process running in a
// Herdr-managed pane. Missing integration variables intentionally leave the
// caller with a nil reporter and no observable side effects.
func NewReporterFromEnv() *Reporter {
	return newReporterFromEnv(os.Getenv, exec.LookPath, runCommand)
}

func newReporterFromEnv(getenv func(string) string, lookPath binaryFinder, run commandRunner) *Reporter {
	if getenv("HERDR_ENV") != "1" {
		return nil
	}
	binPath := strings.TrimSpace(getenv("HERDR_BIN_PATH"))
	if binPath == "" && lookPath != nil {
		if found, err := lookPath("herdr"); err == nil {
			binPath = strings.TrimSpace(found)
		}
	}
	paneID := strings.TrimSpace(getenv("HERDR_PANE_ID"))
	if binPath == "" || paneID == "" {
		return nil
	}
	if run == nil {
		run = runCommand
	}
	r := &Reporter{
		binPath: binPath,
		paneID:  paneID,
		run:     run,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go r.loop()
	return r
}

// Report records the current chat state. Repeated reports for the same state
// and term-llm session are omitted. Calls never wait for Herdr's local socket.
func (r *Reporter) Report(state, sessionID string) {
	if r == nil || !validState(state) {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || (r.reported && r.lastState == state && r.lastID == sessionID) {
		return
	}
	r.reported = true
	r.lastState = state
	r.lastID = sessionID
	r.queue = append(r.queue, event{kind: reportEvent, state: state, sessionID: sessionID})
	r.notify()
}

// Close relinquishes lifecycle authority after pending reports. It waits only
// briefly because chat shutdown must never be held hostage by a local CLI
// failure. Calls are serialized, so Herdr receives state and release reports in
// order without sequence numbers that could become stale across process restarts.
func (r *Reporter) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	if r.reported {
		r.queue = append(r.queue, event{kind: releaseEvent})
	}
	r.notify()
	r.mu.Unlock()

	select {
	case <-r.done:
	case <-time.After(closeTimeout):
	}
}

func (r *Reporter) loop() {
	defer close(r.done)
	for range r.wake {
		for {
			event, ok, closed := r.nextEvent()
			if !ok {
				if closed {
					return
				}
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			if event.kind == releaseEvent {
				_ = r.run(ctx, r.binPath, "pane", "release-agent", r.paneID, "--source", source, "--agent", agent)
			} else {
				args := []string{"pane", "report-agent", r.paneID, "--source", source, "--agent", agent, "--state", event.state}
				if event.sessionID != "" {
					args = append(args, "--agent-session-id", event.sessionID)
				}
				_ = r.run(ctx, r.binPath, args...)
			}
			cancel()
		}
	}
}

// notify wakes the worker without ever waiting for it. The caller holds r.mu.
func (r *Reporter) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Reporter) nextEvent() (event, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 {
		return event{}, false, r.closed
	}
	next := r.queue[0]
	r.queue[0] = event{}
	r.queue = r.queue[1:]
	return next, true, false
}

func runCommand(ctx context.Context, path string, args ...string) error {
	return exec.CommandContext(ctx, path, args...).Run()
}

func validState(state string) bool {
	switch state {
	case "idle", "working", "blocked":
		return true
	default:
		return false
	}
}
