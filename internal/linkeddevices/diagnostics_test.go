package linkeddevices

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/peer"
)

func TestDiagnosticTimelineDisabledIsSilentAndDoesNotReadClock(t *testing.T) {
	clockCalls, sinkCalls := 0, 0
	observer := newDiagnosticTimeline(false, func() time.Time { clockCalls++; return time.Now() }, func(string) { sinkCalls++ })
	if observer != nil || clockCalls != 0 || sinkCalls != 0 {
		t.Fatalf("disabled observer=%v clockCalls=%d sinkCalls=%d", observer != nil, clockCalls, sinkCalls)
	}
}

func TestDiagnosticTimelineRedactsUnknownAndAdversarialFields(t *testing.T) {
	base := time.Unix(1000, 0)
	times := []time.Time{base, base.Add(5 * time.Millisecond), base.Add(-time.Hour)}
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		value := times[0]
		if len(times) > 1 {
			times = times[1:]
		}
		return value
	}
	var lines []string
	observer := newDiagnosticTimeline(true, now, func(line string) { lines = append(lines, line) })
	secret := "SECRET-token-host-id-device-id-query-body-header-sdp-192.0.2.1"
	observer(peer.DiagnosticEvent{
		Stage: peer.DiagnosticStage(secret), Outcome: peer.DiagnosticOutcome(secret), Reason: peer.DiagnosticReason(secret),
		Method: peer.DiagnosticMethod(secret), Route: peer.DiagnosticRoute("/v1/sessions/" + secret + "?token=" + secret),
		Candidate: peer.DiagnosticCandidateClass(secret), Generation: 1, Connection: 2, Request: 3,
		Status: 999, Bytes: ^uint64(0), Duration: -time.Second,
	})
	observer(peer.DiagnosticEvent{Stage: peer.DiagnosticRequestEnd, Outcome: peer.DiagnosticOutcomeFinished})
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, secret) || strings.Contains(joined, "192.0.2.1") || strings.Contains(joined, "token=") {
		t.Fatalf("diagnostics leaked adversarial input: %s", joined)
	}
	for _, want := range []string{
		"[linked-devices] +5ms stage=request_failure", "outcome=failed", "reason=send_failed",
		"method=OTHER", "route=other", "candidate=unknown", "bytes=1099511627776",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("first line missing %q: %s", want, lines[0])
		}
	}
	if !strings.HasPrefix(lines[1], "[linked-devices] +5ms ") {
		t.Fatalf("elapsed time moved backward: %s", lines[1])
	}
}

func TestDiagnosticCandidateClassIsFinite(t *testing.T) {
	for _, test := range []struct {
		stats peer.SelectedPairStats
		want  peer.DiagnosticCandidateClass
	}{
		{peer.SelectedPairStats{LocalCandidateType: "host", RemoteCandidateType: "host"}, peer.DiagnosticCandidateHost},
		{peer.SelectedPairStats{LocalCandidateType: "host", RemoteCandidateType: "srflx"}, peer.DiagnosticCandidateSrflx},
		{peer.SelectedPairStats{LocalCandidateType: "prflx", RemoteCandidateType: "host"}, peer.DiagnosticCandidatePrflx},
		{peer.SelectedPairStats{LocalCandidateType: "relay", RemoteCandidateType: "host"}, peer.DiagnosticCandidateRelay},
		{peer.SelectedPairStats{LocalCandidateType: "SECRET-address", RemoteCandidateType: "other"}, peer.DiagnosticCandidateUnknown},
	} {
		if got := diagnosticCandidateClass(test.stats); got != test.want {
			t.Errorf("diagnosticCandidateClass(%+v) = %q, want %q", test.stats, got, test.want)
		}
	}
}
