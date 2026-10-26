package linkeddevices

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/peer"
)

const maxDiagnosticDuration = 24 * time.Hour

// newDiagnosticTimeline returns nil without reading the clock or allocating a
// recorder when diagnostics are disabled.
func newDiagnosticTimeline(enabled bool, now func() time.Time, sink func(string)) peer.DiagnosticObserver {
	if !enabled || sink == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	timeline := &diagnosticTimeline{now: now, sink: sink, startedAt: now()}
	return timeline.observe
}

func stderrDiagnosticTimeline(now func() time.Time) peer.DiagnosticObserver {
	return newDiagnosticTimeline(true, now, func(line string) { log.Print(line) })
}

type diagnosticTimeline struct {
	mu          sync.Mutex
	now         func() time.Time
	sink        func(string)
	startedAt   time.Time
	lastElapsed time.Duration
}

func (d *diagnosticTimeline) observe(event peer.DiagnosticEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	elapsed := d.now().Sub(d.startedAt)
	if elapsed < 0 {
		elapsed = 0
	} else if elapsed > maxDiagnosticDuration {
		elapsed = maxDiagnosticDuration
	}
	if elapsed < d.lastElapsed {
		elapsed = d.lastElapsed
	}
	d.lastElapsed = elapsed
	event = sanitizeDiagnosticEvent(event)
	var line strings.Builder
	fmt.Fprintf(&line, "[linked-devices] +%dms stage=%s", elapsed/time.Millisecond, event.Stage)
	if event.Outcome != "" {
		fmt.Fprintf(&line, " outcome=%s", event.Outcome)
	}
	if event.Reason != "" {
		fmt.Fprintf(&line, " reason=%s", event.Reason)
	}
	if event.Generation != 0 {
		fmt.Fprintf(&line, " generation=%d", event.Generation)
	}
	if event.Connection != 0 {
		fmt.Fprintf(&line, " connection=%d", event.Connection)
	}
	if event.Request != 0 {
		fmt.Fprintf(&line, " request=%d", event.Request)
	}
	if event.Method != "" {
		fmt.Fprintf(&line, " method=%s", event.Method)
	}
	if event.Route != "" {
		fmt.Fprintf(&line, " route=%s", event.Route)
	}
	if event.Candidate != "" {
		fmt.Fprintf(&line, " candidate=%s", event.Candidate)
	}
	if event.Status != 0 {
		fmt.Fprintf(&line, " status=%d", event.Status)
	}
	if event.Bytes != 0 {
		fmt.Fprintf(&line, " bytes=%d", event.Bytes)
	}
	if event.Duration != 0 {
		fmt.Fprintf(&line, " duration_ms=%d", event.Duration/time.Millisecond)
	}
	d.sink(line.String())
}

func (r *Runtime) observeDiagnostic(event peer.DiagnosticEvent) {
	if r.opts.Diagnostics != nil {
		r.opts.Diagnostics(event)
	}
}

func (r *Runtime) beginDiagnosticConnection(generation uint64) uint64 {
	if r.opts.Diagnostics == nil {
		return 0
	}
	r.mu.Lock()
	r.diagnosticConnectionSeq++
	connection := r.diagnosticConnectionSeq
	r.mu.Unlock()
	r.observeDiagnostic(peer.DiagnosticEvent{
		Stage: peer.DiagnosticIdentityVerified, Outcome: peer.DiagnosticOutcomeConnected,
		Generation: generation, Connection: connection,
	})
	return connection
}

func diagnosticCandidateClass(stats peer.SelectedPairStats) peer.DiagnosticCandidateClass {
	classes := []string{stats.LocalCandidateType, stats.RemoteCandidateType}
	for _, want := range []peer.DiagnosticCandidateClass{
		peer.DiagnosticCandidateRelay, peer.DiagnosticCandidatePrflx,
		peer.DiagnosticCandidateSrflx, peer.DiagnosticCandidateHost,
	} {
		for _, class := range classes {
			if class == string(want) {
				return want
			}
		}
	}
	return peer.DiagnosticCandidateUnknown
}

func sanitizeDiagnosticEvent(event peer.DiagnosticEvent) peer.DiagnosticEvent {
	switch event.Stage {
	case peer.DiagnosticBrokerConnected, peer.DiagnosticBrokerDisconnected, peer.DiagnosticRecoveryAttempt,
		peer.DiagnosticIdentityVerified, peer.DiagnosticICEConnected, peer.DiagnosticChannelOpen,
		peer.DiagnosticRequestStart, peer.DiagnosticResponseHeaders, peer.DiagnosticRequestEnd,
		peer.DiagnosticRequestCancel, peer.DiagnosticRequestFailure:
	default:
		event.Stage = peer.DiagnosticRequestFailure
	}
	switch event.Outcome {
	case "", peer.DiagnosticOutcomeConnected, peer.DiagnosticOutcomeDisconnected, peer.DiagnosticOutcomeStarted,
		peer.DiagnosticOutcomeFinished, peer.DiagnosticOutcomeCanceled, peer.DiagnosticOutcomeTruncated,
		peer.DiagnosticOutcomeFailed:
	default:
		event.Outcome = peer.DiagnosticOutcomeFailed
	}
	switch event.Reason {
	case "", peer.DiagnosticReasonChannelClosed, peer.DiagnosticReasonPeerCanceled,
		peer.DiagnosticReasonContextCanceled, peer.DiagnosticReasonSendFailed, peer.DiagnosticReasonHandlerFailed:
	default:
		event.Reason = peer.DiagnosticReasonSendFailed
	}
	switch event.Method {
	case "", peer.DiagnosticMethodGET, peer.DiagnosticMethodPOST, peer.DiagnosticMethodPUT,
		peer.DiagnosticMethodPATCH, peer.DiagnosticMethodDELETE, peer.DiagnosticMethodHEAD,
		peer.DiagnosticMethodOPTIONS, peer.DiagnosticMethodOther:
	default:
		event.Method = peer.DiagnosticMethodOther
	}
	switch event.Route {
	case "", peer.DiagnosticRouteHealth, peer.DiagnosticRouteCapabilities, peer.DiagnosticRouteProviders,
		peer.DiagnosticRouteModels, peer.DiagnosticRouteSidebar, peer.DiagnosticRouteProjects,
		peer.DiagnosticRouteSessions, peer.DiagnosticRouteSessionStatus, peer.DiagnosticRouteSessionSearch,
		peer.DiagnosticRouteSession, peer.DiagnosticRouteResponses, peer.DiagnosticRouteResponse,
		peer.DiagnosticRouteEvents, peer.DiagnosticRouteEventsPoll, peer.DiagnosticRouteAttention,
		peer.DiagnosticRouteMentionsSearch, peer.DiagnosticRouteAsset, peer.DiagnosticRouteShell,
		peer.DiagnosticRouteOther:
	default:
		event.Route = peer.DiagnosticRouteOther
	}
	switch event.Candidate {
	case "", peer.DiagnosticCandidateHost, peer.DiagnosticCandidateSrflx, peer.DiagnosticCandidatePrflx,
		peer.DiagnosticCandidateRelay, peer.DiagnosticCandidateUnknown:
	default:
		event.Candidate = peer.DiagnosticCandidateUnknown
	}
	if event.Status < 100 || event.Status > 599 {
		event.Status = 0
	}
	if event.Bytes > 1<<40 {
		event.Bytes = 1 << 40
	}
	if event.Duration < 0 {
		event.Duration = 0
	} else if event.Duration > maxDiagnosticDuration {
		event.Duration = maxDiagnosticDuration
	}
	return event
}
