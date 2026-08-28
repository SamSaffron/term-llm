package chat

import (
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

type recordedLifecycleReporter struct {
	reports []lifecycleReport
}

type lifecycleReport struct {
	state     string
	sessionID string
}

func (r *recordedLifecycleReporter) Report(state, sessionID string) {
	r.reports = append(r.reports, lifecycleReport{state: state, sessionID: sessionID})
}

func TestLifecycleReporterTracksChatStates(t *testing.T) {
	m := newTestChatModel(true)
	m.sess = &session.Session{ID: "session-a"}
	reporter := &recordedLifecycleReporter{}
	m.SetLifecycleReporter(reporter)

	m.streaming = true
	m.reportLifecycleState()
	m.pausedForExternalUI = true
	m.reportLifecycleState()
	m.pausedForExternalUI = false
	m.streaming = false
	m.directShellRun = &directShellRun{}
	m.reportLifecycleState()
	m.directShellRun = nil
	m.reportLifecycleState()

	want := []lifecycleReport{
		{state: "idle", sessionID: "session-a"},
		{state: "working", sessionID: "session-a"},
		{state: "blocked", sessionID: "session-a"},
		{state: "working", sessionID: "session-a"},
		{state: "idle", sessionID: "session-a"},
	}
	if !reflect.DeepEqual(reporter.reports, want) {
		t.Fatalf("reports = %#v, want %#v", reporter.reports, want)
	}
}

func TestLifecycleReporterReportsStateAfterUpdate(t *testing.T) {
	m := newTestChatModel(true)
	m.sess = &session.Session{ID: "session-a"}
	reporter := &recordedLifecycleReporter{}
	m.SetLifecycleReporter(reporter)
	m.streaming = true

	updated, _ := m.Update(struct{}{})
	m = updated.(*Model)
	if got := reporter.reports[len(reporter.reports)-1]; got != (lifecycleReport{state: "working", sessionID: "session-a"}) {
		t.Fatalf("last report = %#v, want working state", got)
	}

	m.Update(struct{}{})
	if len(reporter.reports) != 2 {
		t.Fatalf("unchanged lifecycle produced %d reports, want 2", len(reporter.reports))
	}
}

func TestLifecycleReporterTreatsExternalProcessAsWorking(t *testing.T) {
	m := newTestChatModel(true)
	reporter := &recordedLifecycleReporter{}
	m.SetLifecycleReporter(reporter)

	m.pausedForExternalUI = true
	m.externalProcessActive = true
	m.reportLifecycleState()

	if got := reporter.reports[len(reporter.reports)-1]; got.state != "working" {
		t.Fatalf("external process state = %q, want working", got.state)
	}
}
