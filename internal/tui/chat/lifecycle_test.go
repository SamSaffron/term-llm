package chat

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/lifecycle"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestLifecycleSnapshotMapping(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Model)
		state     lifecycle.State
		message   string
	}{
		{name: "idle", configure: func(*Model) {}, state: lifecycle.Idle},
		{name: "approval", configure: func(m *Model) { m.approvalModel = &tools.ApprovalModel{} }, state: lifecycle.Blocked, message: "Waiting for approval"},
		{name: "ask user", configure: func(m *Model) { m.askUserModel = &tools.AskUserModel{} }, state: lifecycle.Blocked, message: "Waiting for user input"},
		{name: "handover preview", configure: func(m *Model) { m.handoverPreview = &handoverPreviewModel{} }, state: lifecycle.Blocked, message: "Waiting for handover confirmation"},
		{name: "paused external UI", configure: func(m *Model) { m.pausedForExternalUI = true }, state: lifecycle.Blocked, message: "Waiting for external input"},
		{name: "streaming", configure: func(m *Model) { m.streaming = true }, state: lifecycle.Working, message: "Generating response"},
		{name: "direct shell", configure: func(m *Model) { m.directShellRun = &directShellRun{} }, state: lifecycle.Working, message: "Running shell command"},
		{name: "external process", configure: func(m *Model) { m.externalProcessActive = true }, state: lifecycle.Working, message: "Running external process"},
		{name: "worktree operation", configure: func(m *Model) { m.worktreeOperation = "switch" }, state: lifecycle.Working, message: "Running worktree operation"},
		{name: "side question", configure: func(m *Model) { m.sideQuestion.Running = true }, state: lifecycle.Working, message: "Answering side question"},
		{name: "branch context", configure: func(m *Model) { m.branchPathNotesRequest = &BranchPathNotesRequest{} }, state: lifecycle.Working, message: "Preparing branch context"},
		{name: "session transition", configure: func(m *Model) { m.sessionTransition = &sessionTransition{} }, state: lifecycle.Working, message: "Preparing session switch"},
		{name: "session switch pending", configure: func(m *Model) { m.sessionSwitchPending = true }, state: lifecycle.Working, message: "Preparing session switch"},
		{name: "active skill run", configure: func(m *Model) {
			m.skillRuns = map[string]*skillRunState{"skill-1": {Status: "running"}}
		}, state: lifecycle.Working, message: "Running skill"},
		{name: "cancelling skill run", configure: func(m *Model) {
			m.skillRuns = map[string]*skillRunState{"skill-1": {Status: "cancelling"}}
		}, state: lifecycle.Working, message: "Running skill"},
		{name: "transcript mutation", configure: func(m *Model) { m.transcriptMutationInFlight = true }, state: lifecycle.Working, message: "Updating transcript"},
		{name: "share", configure: func(m *Model) { m.shareInFlight = true }, state: lifecycle.Working, message: "Sharing session"},
		{name: "background title generation excluded", configure: func(m *Model) { m.titleGenerationInFlight = true }, state: lifecycle.Idle},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestChatModel(true)
			m.sess = &session.Session{ID: "session-a"}
			test.configure(m)
			want := lifecycle.Snapshot{State: test.state, SessionID: "session-a", Message: test.message}
			if got := m.LifecycleSnapshot(); got != want {
				t.Fatalf("LifecycleSnapshot() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestLifecycleSnapshotIncludesPreferredSessionTitle(t *testing.T) {
	m := newTestChatModel(true)
	m.sess = &session.Session{ID: "session-title", Name: "Testing", GeneratedShortTitle: "Generated fallback"}
	if got := m.LifecycleSnapshot().Title; got != "Testing" {
		t.Fatalf("LifecycleSnapshot().Title = %q, want Testing", got)
	}
}

func TestLifecycleSnapshotBlockedPrecedesWorking(t *testing.T) {
	blockers := []struct {
		name      string
		configure func(*Model)
		message   string
	}{
		{name: "approval", configure: func(m *Model) { m.approvalModel = &tools.ApprovalModel{} }, message: "Waiting for approval"},
		{name: "ask user", configure: func(m *Model) { m.askUserModel = &tools.AskUserModel{} }, message: "Waiting for user input"},
		{name: "handover preview", configure: func(m *Model) { m.handoverPreview = &handoverPreviewModel{} }, message: "Waiting for handover confirmation"},
		{name: "paused UI", configure: func(m *Model) { m.pausedForExternalUI = true }, message: "Waiting for external input"},
	}
	for _, blocker := range blockers {
		t.Run(blocker.name, func(t *testing.T) {
			m := newTestChatModel(true)
			m.streaming = true
			m.directShellRun = &directShellRun{}
			m.worktreeOperation = "new"
			m.sideQuestion.Running = true
			m.shareInFlight = true
			blocker.configure(m)
			got := m.LifecycleSnapshot()
			if got.State != lifecycle.Blocked || got.Message != blocker.message {
				t.Fatalf("LifecycleSnapshot() = %#v, want blocked %q", got, blocker.message)
			}
		})
	}
}

func TestLifecycleSnapshotExternalProcessOverridesPausedExternalUI(t *testing.T) {
	m := newTestChatModel(true)
	m.pausedForExternalUI = true
	m.externalProcessActive = true
	want := lifecycle.Snapshot{State: lifecycle.Working, SessionID: m.SessionID(), Message: "Running external process", CWD: m.effectiveWorkingDir()}
	if got := m.LifecycleSnapshot(); got != want {
		t.Fatalf("LifecycleSnapshot() = %#v, want %#v", got, want)
	}
}

func TestLifecycleSnapshotIncludesBoundWorkingDirectory(t *testing.T) {
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "session-cwd", CWD: "/root", WorktreeDir: "/work/tree"}
	got := m.LifecycleSnapshot()
	if got.CWD != "/work/tree" {
		t.Fatalf("LifecycleSnapshot().CWD = %q, want /work/tree", got.CWD)
	}
}

func TestLifecycleSnapshotIsPure(t *testing.T) {
	m := newTestChatModel(true)
	m.sess = &session.Session{ID: "session-a"}
	m.streaming = true
	first := m.LifecycleSnapshot()
	second := m.LifecycleSnapshot()
	if first != second || !m.streaming || m.SessionID() != "session-a" {
		t.Fatalf("snapshot mutated model: first=%#v second=%#v streaming=%v session=%q", first, second, m.streaming, m.SessionID())
	}
}
