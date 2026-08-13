package chat

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type fakeWorkerController struct {
	requests []WorkerStartRequest
	cancel   []string
	err      error
}

func (c *fakeWorkerController) OwnerID() string { return "owner-test" }

func (c *fakeWorkerController) StartWorker(_ context.Context, req WorkerStartRequest) (WorkerStartResult, error) {
	c.requests = append(c.requests, req)
	if c.err != nil {
		return WorkerStartResult{}, c.err
	}
	return WorkerStartResult{JobID: "job-1", RunID: "run-1"}, nil
}

func (c *fakeWorkerController) CancelWorker(_ context.Context, req WorkerCancelRequest) error {
	c.cancel = append(c.cancel, req.RunID)
	return c.err
}

func (c *fakeWorkerController) ReconcileWorkers(context.Context, string) error { return c.err }

func newWorkerChatTest(t *testing.T) (*Model, *session.SQLiteStore, *fakeWorkerController) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	coordinator := &session.Session{
		ID: "main", Provider: "mock", ProviderKey: "mock", Model: "model",
		Mode: session.ModeChat, Origin: session.OriginTUI, ApprovalMode: session.ApprovalModePrompt,
		CWD: t.TempDir(), CreatedAt: now, UpdatedAt: now, Status: session.StatusActive,
	}
	if err := store.Create(context.Background(), coordinator); err != nil {
		t.Fatal(err)
	}
	m := newCmdTestModel(store)
	m.sess = coordinator
	m.providerKey = coordinator.ProviderKey
	m.modelName = coordinator.Model
	m.toolsStr = "read_file"
	controller := &fakeWorkerController{}
	m.SetWorkerController(controller)
	return m, store, controller
}

func TestThreadStartsDurableWorkerWithoutMutatingCoordinatorTranscript(t *testing.T) {
	m, store, controller := newWorkerChatTest(t)
	updated, _ := m.ExecuteCommand("/thread inspect the harmless fixture")
	m = updated.(*Model)
	if len(controller.requests) != 1 || controller.requests[0].Task != "inspect the harmless fixture" || controller.requests[0].CoordinatorSessionID != m.sess.ID {
		t.Fatalf("worker request = %#v", controller.requests)
	}
	workers, err := store.ListWorkers(context.Background(), m.sess.ID)
	if err != nil || len(workers) != 1 || workers[0].RunID != "run-1" || workers[0].Status != session.WorkerQueued {
		t.Fatalf("workers = %#v, %v", workers, err)
	}
	messages, err := store.GetMessages(context.Background(), m.sess.ID, 0, 0)
	if err != nil || len(messages) != 0 {
		t.Fatalf("thread command mutated coordinator transcript: %#v, %v", messages, err)
	}
	if !strings.Contains(m.footerMessage, "Worker started") || !strings.Contains(m.footerMessage, "/tree") {
		t.Fatalf("thread footer = %q", m.footerMessage)
	}
}

func TestThreadStartFailureLeavesTerminalMailboxResult(t *testing.T) {
	m, store, controller := newWorkerChatTest(t)
	controller.err = fmt.Errorf("runner unavailable")
	updated, _ := m.ExecuteCommand("/thread harmless failure")
	m = updated.(*Model)
	workers, err := store.ListWorkers(context.Background(), m.sess.ID)
	if err != nil || len(workers) != 1 || workers[0].Status != session.WorkerFailed {
		t.Fatalf("failed worker = %#v, %v", workers, err)
	}
	reports, err := store.ListWorkerReports(context.Background(), workers[0].ChildSessionID)
	if err != nil || len(reports) != 1 || reports[0].Kind != session.WorkerReportResult || reports[0].Origin != "terminal_synthesis" || !strings.Contains(reports[0].Body, controller.err.Error()) {
		t.Fatalf("failure reports = %#v, %v", reports, err)
	}
	messages, err := store.GetMessages(context.Background(), m.sess.ID, 0, 0)
	if err != nil || len(messages) != 0 {
		t.Fatalf("start failure entered coordinator transcript: %#v, %v", messages, err)
	}
}

func TestTreeSupervisesWorkerReportsAndExplicitImportIsUser(t *testing.T) {
	m, store, controller := newWorkerChatTest(t)
	updated, _ := m.ExecuteCommand("/thread summarize the fixture")
	m = updated.(*Model)
	workers, _ := store.ListWorkers(context.Background(), m.sess.ID)
	edge := workers[0]
	report, err := store.AddWorkerReport(context.Background(), session.WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: edge.CoordinatorSessionID, Kind: session.WorkerReportResult,
		Title: "Fixture summary", Body: "The fixture is harmless.", Origin: "worker_tool",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, _ = m.cmdTree(nil)
	m = updated.(*Model)
	var workerID string
	for _, item := range m.dialog.items {
		if item.Category == "Workers" {
			workerID = item.ID
			if !strings.Contains(item.Description, "1 unread") || !strings.Contains(item.Label, "queued") {
				t.Fatalf("worker tree item = %#v", item)
			}
		}
	}
	if workerID == "" {
		t.Fatalf("tree has no Workers category: %#v", m.dialog.items)
	}
	m.dialog.Close()
	updated, _ = m.handleBranchTreeSelection(workerID)
	m = updated.(*Model)
	if m.dialog.Type() != DialogWorkerReports {
		t.Fatalf("worker selection dialog = %v", m.dialog.Type())
	}
	m.dialog.Close()
	updated, _ = m.handleWorkerReportsSelection("worker-report:" + fmtInt64(report.ID))
	m = updated.(*Model)
	if m.dialog.Type() != DialogContent || !strings.Contains(m.dialog.Content(), report.Body) || m.pendingWorkerReport == nil {
		t.Fatalf("report content dialog = type:%v content:%q pending:%#v", m.dialog.Type(), m.dialog.Content(), m.pendingWorkerReport)
	}
	updated, _ = m.importPendingWorkerReport()
	m = updated.(*Model)
	messages, err := store.GetMessages(context.Background(), edge.CoordinatorSessionID, 0, 0)
	if err != nil || len(messages) != 1 || messages[0].Role != llm.RoleUser || !strings.Contains(messages[0].TextContent, "Imported worker report") {
		t.Fatalf("imported transcript = %#v, %v", messages, err)
	}
	if len(controller.requests) != 1 {
		t.Fatalf("import unexpectedly started another worker: %#v", controller.requests)
	}
}

func fmtInt64(value int64) string {
	return fmt.Sprintf("%d", value)
}

func TestCompactedCoordinatorImportAppendsAfterBoundary(t *testing.T) {
	m, store, _ := newWorkerChatTest(t)
	ctx := context.Background()
	if err := store.AddMessage(ctx, m.sess.ID, session.NewMessage(m.sess.ID, llm.UserText("old prompt"), -1)); err != nil {
		t.Fatal(err)
	}
	summary := session.NewMessage(m.sess.ID, llm.UserText("[Context Compaction]\nsummary"), -1)
	ack := session.NewMessage(m.sess.ID, llm.AssistantText("Compacted context loaded."), -1)
	ack.CompactionTail = true
	if err := store.CompactMessages(ctx, m.sess.ID, []session.Message{*summary, *ack}); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.Get(ctx, m.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.sess = refreshed
	m.messages, m.compactionIdx, err = loadSessionMessagesForScrollback(ctx, store, refreshed)
	if err != nil {
		t.Fatal(err)
	}
	beforeBoundary := m.sess.CompactionSeq

	edge, err := store.CreateWorker(ctx, m.sess.ID, "compacted mailbox")
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.AddWorkerReport(ctx, session.WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: edge.CoordinatorSessionID, Kind: session.WorkerReportResult,
		Title: "Fresh result", Body: "post-compaction evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	m.pendingWorkerReport = &report
	updated, _ := m.importPendingWorkerReport()
	m = updated.(*Model)
	if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1].TextContent, report.Body) {
		t.Fatalf("UI compacted transcript missing imported tail: %#v", m.messages)
	}
	after, err := store.Get(ctx, m.sess.ID)
	if err != nil || after.CompactionSeq != beforeBoundary {
		t.Fatalf("compaction boundary changed: before=%d after=%#v err=%v", beforeBoundary, after, err)
	}
	active, err := session.LoadActiveMessages(ctx, store, after)
	if err != nil || len(active) == 0 || !strings.Contains(active[len(active)-1].TextContent, report.Body) {
		t.Fatalf("active compacted context missing report: %#v, %v", active, err)
	}
}

func TestThreadAndReportImportWaitForTranscriptMutationBoundary(t *testing.T) {
	for _, state := range []struct {
		name  string
		apply func(*Model)
	}{
		{name: "streaming", apply: func(m *Model) { m.streaming = true }},
		{name: "mutation", apply: func(m *Model) { m.transcriptMutationInFlight = true }},
	} {
		t.Run(state.name, func(t *testing.T) {
			m, store, controller := newWorkerChatTest(t)
			state.apply(m)
			updated, _ := m.ExecuteCommand("/thread must wait")
			m = updated.(*Model)
			if len(controller.requests) != 0 {
				t.Fatalf("busy /thread started worker: %#v", controller.requests)
			}
			workers, err := store.ListWorkers(context.Background(), m.sess.ID)
			if err != nil || len(workers) != 0 {
				t.Fatalf("busy /thread created edge: %#v, %v", workers, err)
			}
			if !strings.Contains(m.footerMessage, "conversation work is active") {
				t.Fatalf("busy /thread footer = %q", m.footerMessage)
			}

			// Build a mailbox report without going through /thread, then verify the
			// same boundary protects the transcript append and in-memory ordering.
			edge, err := store.CreateWorker(context.Background(), m.sess.ID, "report fixture")
			if err != nil {
				t.Fatal(err)
			}
			report, err := store.AddWorkerReport(context.Background(), session.WorkerReport{
				ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
				DestinationSessionID: edge.CoordinatorSessionID, Kind: session.WorkerReportResult,
				Title: "Deferred", Body: "Do not interleave me.",
			})
			if err != nil {
				t.Fatal(err)
			}
			m.pendingWorkerReport = &report
			before := len(m.messages)
			updated, _ = m.importPendingWorkerReport()
			m = updated.(*Model)
			if m.pendingWorkerReport == nil || len(m.messages) != before {
				t.Fatalf("busy import changed UI transcript: pending=%#v messages=%d", m.pendingWorkerReport, len(m.messages))
			}
			messages, err := store.GetMessages(context.Background(), m.sess.ID, 0, 0)
			if err != nil || len(messages) != 0 {
				t.Fatalf("busy import changed durable transcript: %#v, %v", messages, err)
			}
			if !strings.Contains(m.footerMessage, "Cannot import") {
				t.Fatalf("busy import footer = %q", m.footerMessage)
			}
		})
	}
}

func TestActiveWorkerAlwaysOffersSafeCancelOrReconcileAction(t *testing.T) {
	m, store, _ := newWorkerChatTest(t)
	edge, err := store.CreateWorker(context.Background(), m.sess.ID, "stale queued edge")
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.openWorkerReports(edge.ChildSessionID)
	m = updated.(*Model)
	found := false
	for _, item := range m.dialog.items {
		if item.ID == "worker-cancel:"+edge.ChildSessionID {
			found = strings.Contains(item.Label, "reconcile") && strings.Contains(item.Description, "stale")
		}
	}
	if !found {
		t.Fatalf("active edge without run has no safe action: %#v", m.dialog.items)
	}
}

func TestMainReturnsWorkerToCoordinatorAndActiveChildIsReadOnly(t *testing.T) {
	m, store, controller := newWorkerChatTest(t)
	_, _ = m.ExecuteCommand("/thread observe only")
	workers, _ := store.ListWorkers(context.Background(), m.sess.ID)
	edge := workers[0]
	if err := store.UpdateWorkerStatus(context.Background(), edge.ChildSessionID, session.WorkerRunning); err != nil {
		t.Fatal(err)
	}
	child, err := store.Get(context.Background(), edge.ChildSessionID)
	if err != nil {
		t.Fatal(err)
	}
	workerModel := newCmdTestModel(store)
	workerModel.sess = child
	workerModel.SetWorkerController(controller)
	updated, _ := workerModel.sendMessage("try to write")
	workerModel = updated.(*Model)
	if !strings.Contains(workerModel.footerMessage, "read-only") {
		t.Fatalf("running child send footer = %q", workerModel.footerMessage)
	}
	updated, cmd := workerModel.cmdMain(nil)
	workerModel = updated.(*Model)
	if cmd == nil || workerModel.RequestedResumeSessionID() != edge.CoordinatorSessionID {
		t.Fatalf("/main navigation = cmd:%v resume:%q", cmd != nil, workerModel.RequestedResumeSessionID())
	}

	updated, _ = m.cmdMain(nil)
	m = updated.(*Model)
	if !strings.Contains(m.footerMessage, "Already") {
		t.Fatalf("coordinator /main footer = %q", m.footerMessage)
	}
}

func TestWorkerMailboxPollUpdatesBadgeWithoutChangingFocus(t *testing.T) {
	m, store, _ := newWorkerChatTest(t)
	m.textarea.Focus()
	m.setTextareaValue("draft")
	edge, _ := store.CreateWorker(context.Background(), m.sess.ID, "poll")
	_, _ = store.AddWorkerReport(context.Background(), session.WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: edge.CoordinatorSessionID, Kind: session.WorkerReportProgress,
		Title: "Update", Body: "Still working.",
	})
	cmd := m.workerMailboxPollCmd()
	if cmd == nil {
		t.Fatal("mailbox poll command missing")
	}
	msg := cmd().(workerMailboxPollMsg)
	_ = m.handleWorkerMailboxPoll(msg)
	if m.workerUnreadReports != 1 || m.textarea.Value() != "draft" || !m.textarea.Focused() {
		t.Fatalf("poll state = unread:%d draft:%q focused:%v", m.workerUnreadReports, m.textarea.Value(), m.textarea.Focused())
	}
	footer := m.buildFooterLayout().view
	if !strings.Contains(footer, "1 unread worker report") {
		t.Fatalf("footer badge missing: %q", footer)
	}
}
