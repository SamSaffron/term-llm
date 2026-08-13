package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminaltext"
	"github.com/samsaffron/term-llm/internal/tools"
)

// WorkerStartRequest carries the durable child identity and its inherited run
// settings to the outer chat-owned jobs-v2 supervisor.
type WorkerStartRequest struct {
	ChildSessionID       string
	CoordinatorSessionID string
	Task                 string
	Cwd                  string
	Provider             string
	Model                string
	Tools                string
	Search               bool
	ApprovalMode         tools.ApprovalMode
}

type WorkerStartResult struct {
	JobID string
	RunID string
}

// WorkerController is implemented by cmd so the TUI does not depend on the
// jobs-v2 HTTP/server package.
type WorkerController interface {
	StartWorker(context.Context, WorkerStartRequest) (WorkerStartResult, error)
	CancelWorker(context.Context, string) error
}

type workerMailboxPollMsg struct {
	coordinatorID string
	unread        int
	edge          *session.WorkerEdge
}

func (m *Model) SetWorkerController(controller WorkerController) {
	m.workerController = controller
	m.refreshWorkerIdentity(context.Background())
}

func (m *Model) refreshWorkerIdentity(ctx context.Context) {
	m.workerEdge = nil
	if m == nil || m.store == nil || m.sess == nil {
		return
	}
	workerStore, ok := session.AsWorkerStore(m.store)
	if !ok {
		return
	}
	edge, err := workerStore.GetWorker(ctx, m.sess.ID)
	if err == nil {
		m.workerEdge = &edge
	}
}

func (m *Model) workerCoordinatorID() string {
	if m.workerEdge != nil {
		return m.workerEdge.CoordinatorSessionID
	}
	if m.sess != nil {
		return m.sess.ID
	}
	return ""
}

func (m *Model) workerChildReadOnly() bool {
	return m.workerEdge != nil && m.workerEdge.Status.Active()
}

func (m *Model) workerMailboxPollCmd() tea.Cmd {
	if m == nil || m.store == nil || m.sess == nil {
		return nil
	}
	workerStore, ok := session.AsWorkerStore(m.store)
	if !ok {
		return nil
	}
	sessionID := m.sess.ID
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		edge, edgeErr := workerStore.GetWorker(context.Background(), sessionID)
		coordinatorID := sessionID
		var edgePtr *session.WorkerEdge
		if edgeErr == nil {
			edgeCopy := edge
			edgePtr = &edgeCopy
			coordinatorID = edge.CoordinatorSessionID
		}
		unread, _ := workerStore.CountUnreadWorkerReports(context.Background(), coordinatorID)
		return workerMailboxPollMsg{coordinatorID: coordinatorID, unread: unread, edge: edgePtr}
	})
}

func (m *Model) handleWorkerMailboxPoll(msg workerMailboxPollMsg) tea.Cmd {
	m.workerUnreadReports = msg.unread
	m.workerEdge = msg.edge
	m.bumpContentVersion()
	return m.workerMailboxPollCmd()
}

func recordWorkerStartFailure(workerStore session.WorkerStore, edge session.WorkerEdge, title string, startErr error) {
	ctx := context.Background()
	_ = workerStore.UpdateWorkerStatus(ctx, edge.ChildSessionID, session.WorkerFailed)
	if hasResult, err := workerStore.HasWorkerReportKind(ctx, edge.ChildSessionID, session.WorkerReportResult); err == nil && hasResult {
		return
	}
	body := "Worker execution could not be started."
	if startErr != nil {
		body = startErr.Error()
	}
	_, _ = workerStore.AddWorkerReport(ctx, session.WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: edge.CoordinatorSessionID, Kind: session.WorkerReportResult,
		Title: title, Body: body, Origin: "terminal_synthesis",
	})
}

func (m *Model) cmdThread(task string) (tea.Model, tea.Cmd) {
	task = strings.TrimSpace(task)
	if task == "" {
		m.setTextareaValue("")
		return m.showFooterError("Usage: /thread TASK")
	}
	if m.workerController == nil {
		return m.showFooterError("Background workers are unavailable in this chat runtime.")
	}
	if m.store == nil || m.sess == nil {
		return m.showFooterError("A stored coordinator session is required.")
	}
	workerStore, ok := session.AsWorkerStore(m.store)
	if !ok {
		return m.showFooterError("Session storage does not support durable workers.")
	}
	if _, err := workerStore.GetWorker(context.Background(), m.sess.ID); err == nil {
		return m.showFooterWarning("Nested worker threads are not supported. Use /main first.")
	} else if !errors.Is(err, session.ErrNotFound) {
		return m.showFooterError(fmt.Sprintf("Check worker identity: %v", err))
	}

	edge, err := workerStore.CreateWorker(m.rootContext(), m.sess.ID, task)
	if err != nil {
		return m.showFooterWarning(fmt.Sprintf("Start worker: %v", err))
	}
	child, err := m.store.Get(m.rootContext(), edge.ChildSessionID)
	if err != nil {
		recordWorkerStartFailure(workerStore, edge, "Worker session could not be loaded", err)
		return m.showFooterError(fmt.Sprintf("Load worker session: %v", err))
	}
	result, err := m.workerController.StartWorker(m.rootContext(), WorkerStartRequest{
		ChildSessionID: edge.ChildSessionID, CoordinatorSessionID: edge.CoordinatorSessionID,
		Task: edge.Task, Cwd: child.CWD, Provider: child.ProviderKey, Model: child.Model,
		Tools: child.Tools, Search: child.Search, ApprovalMode: m.requestedApprovalMode,
	})
	if err != nil {
		recordWorkerStartFailure(workerStore, edge, "Worker failed to start", err)
		return m.showFooterError(fmt.Sprintf("Start worker execution: %v", err))
	}
	if err := workerStore.SetWorkerExecution(context.Background(), edge.ChildSessionID, result.JobID, result.RunID); err != nil {
		_ = m.workerController.CancelWorker(context.Background(), result.RunID)
		recordWorkerStartFailure(workerStore, edge, "Worker execution could not be saved", err)
		return m.showFooterError(fmt.Sprintf("Save worker execution: %v", err))
	}
	m.setTextareaValue("")
	return m.showFooterSuccess(fmt.Sprintf("Worker started · %s · use /tree to supervise", session.ShortID(edge.ChildSessionID)))
}

func (m *Model) cmdMain(args []string) (tea.Model, tea.Cmd) {
	if len(args) != 0 {
		return m.showFooterError("Usage: /main")
	}
	if m.store == nil || m.sess == nil {
		return m.showFooterMuted("Already in the main coordinator session.")
	}
	workerStore, ok := session.AsWorkerStore(m.store)
	if !ok {
		return m.showFooterMuted("Already in the main coordinator session.")
	}
	edge, err := workerStore.GetWorker(context.Background(), m.sess.ID)
	if errors.Is(err, session.ErrNotFound) {
		return m.showFooterMuted("Already in the main coordinator session.")
	}
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Find coordinator: %v", err))
	}
	return m.requestResumeSession(edge.CoordinatorSessionID)
}

func workerStatusLabel(status session.WorkerStatus) string {
	switch status {
	case session.WorkerQueued:
		return "queued"
	case session.WorkerRunning:
		return "running"
	case session.WorkerBlocked:
		return "blocked"
	case session.WorkerDone:
		return "done"
	case session.WorkerFailed:
		return "failed"
	case session.WorkerCancelled:
		return "cancelled"
	default:
		return string(status)
	}
}

func workerTreeItem(edge session.WorkerEdge, currentSessionID string) DialogItem {
	marker := "○"
	if edge.ChildSessionID == currentSessionID {
		marker = "●"
	}
	unread := ""
	if edge.UnreadReports > 0 {
		unread = fmt.Sprintf(" · %d unread", edge.UnreadReports)
	}
	preview := terminaltext.SanitizeSingleLine(edge.Task)
	return DialogItem{
		ID: "worker:" + edge.ChildSessionID, Label: fmt.Sprintf("%s %s · %s", marker, session.TruncateSummary(preview), workerStatusLabel(edge.Status)),
		Description: session.ShortID(edge.ChildSessionID) + unread, Category: "Workers",
	}
}

func (m *Model) openWorkerReports(childSessionID string) (tea.Model, tea.Cmd) {
	workerStore, ok := session.AsWorkerStore(m.store)
	if !ok {
		return m.showFooterError("Worker mailbox is unavailable.")
	}
	edge, err := workerStore.GetWorker(context.Background(), childSessionID)
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Load worker: %v", err))
	}
	reports, err := workerStore.ListWorkerReports(context.Background(), childSessionID)
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Load worker reports: %v", err))
	}
	m.workerTreeSelection = &edge
	m.workerReportChoices = make(map[int64]session.WorkerReport, len(reports))
	items := []DialogItem{{ID: "worker-open:" + childSessionID, Label: "Open worker session", Description: "Running workers open read-only"}}
	if edge.Status.Active() && edge.RunID != "" {
		items = append(items, DialogItem{ID: "worker-cancel:" + childSessionID, Label: "Cancel worker", Description: "Request jobs-v2 cancellation"})
	}
	for i := len(reports) - 1; i >= 0; i-- {
		report := reports[i]
		m.workerReportChoices[report.ID] = report
		state := "read"
		if report.ReadAt == nil {
			state = "unread"
		}
		if report.ImportedAt != nil {
			state = "imported"
		}
		items = append(items, DialogItem{
			ID: fmt.Sprintf("worker-report:%d", report.ID), Label: fmt.Sprintf("%s · %s", report.Kind, report.Title),
			Description: fmt.Sprintf("report %d · %s", report.Sequence+1, state), Category: "Reports",
		})
	}
	m.dialog.ShowWorkerReports(items, "Worker · "+session.ShortID(childSessionID))
	return m, nil
}

func (m *Model) handleWorkerReportsSelection(id string) (tea.Model, tea.Cmd) {
	switch {
	case strings.HasPrefix(id, "worker-open:"):
		return m.requestResumeSession(strings.TrimPrefix(id, "worker-open:"))
	case strings.HasPrefix(id, "worker-cancel:"):
		if m.workerTreeSelection == nil || m.workerController == nil {
			return m.showFooterError("Worker cancellation is unavailable.")
		}
		if err := m.workerController.CancelWorker(m.rootContext(), m.workerTreeSelection.RunID); err != nil {
			return m.showFooterError(fmt.Sprintf("Cancel worker: %v", err))
		}
		return m.showFooterMuted("Worker cancellation requested.")
	case strings.HasPrefix(id, "worker-report:"):
		var reportID int64
		if _, err := fmt.Sscanf(strings.TrimPrefix(id, "worker-report:"), "%d", &reportID); err != nil {
			return m.showFooterError("Invalid worker report selection.")
		}
		report, ok := m.workerReportChoices[reportID]
		if !ok {
			return m.showFooterError("Worker report is no longer available.")
		}
		if workerStore, ok := session.AsWorkerStore(m.store); ok {
			_ = workerStore.MarkWorkerReportRead(context.Background(), report.ID)
		}
		m.pendingWorkerReport = &report
		content := fmt.Sprintf("Kind: %s\nSource: %s\nOrigin: %s\n\n%s", report.Kind, session.ShortID(report.SourceSessionID), report.Origin, report.Body)
		m.dialog.ShowContent(report.Title, content)
		m.dialog.SetContentFooter("i import into coordinator as user · ↑/↓ scroll · esc close")
		return m, nil
	default:
		return m.showFooterError("Unknown worker action.")
	}
}

func (m *Model) importPendingWorkerReport() (tea.Model, tea.Cmd) {
	if m.pendingWorkerReport == nil {
		return m, nil
	}
	workerStore, ok := session.AsWorkerStore(m.store)
	if !ok {
		return m.showFooterError("Worker mailbox is unavailable.")
	}
	message, err := workerStore.ImportWorkerReport(m.rootContext(), m.pendingWorkerReport.ID)
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Import worker report: %v", err))
	}
	m.pendingWorkerReport = nil
	m.dialog.Close()
	if m.sess != nil && message != nil && message.SessionID == m.sess.ID {
		alreadyLoaded := false
		for i := range m.messages {
			alreadyLoaded = alreadyLoaded || m.messages[i].ID == message.ID
		}
		if !alreadyLoaded {
			m.messages = append(m.messages, *message)
			m.invalidateHistoryCache()
		}
	}
	if m.workerUnreadReports > 0 {
		m.workerUnreadReports--
	}
	return m.showFooterSuccess("Worker report imported as an attributed user message.")
}
