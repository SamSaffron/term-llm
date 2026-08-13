package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WorkerStatus is the durable lifecycle state of a background worker.
type WorkerStatus string

const (
	WorkerQueued    WorkerStatus = "queued"
	WorkerRunning   WorkerStatus = "running"
	WorkerBlocked   WorkerStatus = "blocked"
	WorkerDone      WorkerStatus = "done"
	WorkerFailed    WorkerStatus = "failed"
	WorkerCancelled WorkerStatus = "cancelled"
)

func (s WorkerStatus) Terminal() bool {
	switch s {
	case WorkerDone, WorkerFailed, WorkerCancelled:
		return true
	default:
		return false
	}
}

func (s WorkerStatus) Active() bool {
	return s == WorkerQueued || s == WorkerRunning || s == WorkerBlocked
}

// WorkerReportKind identifies the mailbox event semantics.
type WorkerReportKind string

const (
	WorkerReportProgress WorkerReportKind = "progress"
	WorkerReportResult   WorkerReportKind = "result"
	WorkerReportBlocker  WorkerReportKind = "blocker"
)

const (
	MaxWorkerTaskRunes        = 16_000
	MaxWorkerReportTitleRunes = 200
	MaxWorkerReportBodyRunes  = 32_000
	MaxWorkerMetadataBytes    = 8_192
)

var ErrWorkersUnsupported = errors.New("session: workers unsupported")

// WorkerEdge is a separate durable relation. It intentionally does not use
// session_branches: workers have no transcript fork anchor or copied prefix.
type WorkerEdge struct {
	ChildSessionID       string       `json:"child_session_id"`
	CoordinatorSessionID string       `json:"coordinator_session_id"`
	Task                 string       `json:"task"`
	Status               WorkerStatus `json:"status"`
	JobID                string       `json:"job_id,omitempty"`
	RunID                string       `json:"run_id,omitempty"`
	UnreadReports        int          `json:"unread_reports"`
	CreatedAt            time.Time    `json:"created_at"`
	UpdatedAt            time.Time    `json:"updated_at"`
	FinishedAt           *time.Time   `json:"finished_at,omitempty"`
}

// WorkerReport is immutable mailbox content. Only read/import bookkeeping may
// be updated after insertion.
type WorkerReport struct {
	ID                   int64            `json:"id"`
	ChildSessionID       string           `json:"child_session_id"`
	SourceSessionID      string           `json:"source_session_id"`
	DestinationSessionID string           `json:"destination_session_id"`
	Kind                 WorkerReportKind `json:"kind"`
	Title                string           `json:"title"`
	Body                 string           `json:"body"`
	Metadata             json.RawMessage  `json:"metadata,omitempty"`
	Origin               string           `json:"origin"`
	Sequence             int              `json:"sequence"`
	ReadAt               *time.Time       `json:"read_at,omitempty"`
	ImportedAt           *time.Time       `json:"imported_at,omitempty"`
	ImportedMessageID    int64            `json:"imported_message_id,omitempty"`
	CreatedAt            time.Time        `json:"created_at"`
}

// WorkerStore is the optional durable worker/mailbox capability.
type WorkerStore interface {
	CreateWorker(ctx context.Context, coordinatorSessionID, task string) (WorkerEdge, error)
	SetWorkerExecution(ctx context.Context, childSessionID, jobID, runID string) error
	UpdateWorkerStatus(ctx context.Context, childSessionID string, status WorkerStatus) error
	GetWorker(ctx context.Context, childSessionID string) (WorkerEdge, error)
	ListWorkers(ctx context.Context, coordinatorSessionID string) ([]WorkerEdge, error)
	AddWorkerReport(ctx context.Context, report WorkerReport) (WorkerReport, error)
	ListWorkerReports(ctx context.Context, childSessionID string) ([]WorkerReport, error)
	MarkWorkerReportRead(ctx context.Context, reportID int64) error
	ImportWorkerReport(ctx context.Context, reportID int64) (*Message, error)
	CountUnreadWorkerReports(ctx context.Context, coordinatorSessionID string) (int, error)
	HasWorkerReportKind(ctx context.Context, childSessionID string, kind WorkerReportKind) (bool, error)
}

// AsWorkerStore resolves worker capability through the logging decorator.
func AsWorkerStore(store Store) (WorkerStore, bool) {
	if store == nil {
		return nil, false
	}
	workerStore, ok := store.(WorkerStore)
	return workerStore, ok
}

func normalizeWorkerText(value string, maxRunes int) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	runes := []rune(value)
	if maxRunes > 0 && len(runes) > maxRunes {
		value = string(runes[:maxRunes])
	}
	return value
}

func normalizeWorkerTask(task string) (string, error) {
	task = normalizeWorkerText(task, MaxWorkerTaskRunes)
	if task == "" {
		return "", fmt.Errorf("worker task is required")
	}
	return task, nil
}

func normalizeWorkerReport(report WorkerReport) (WorkerReport, error) {
	report.ChildSessionID = strings.TrimSpace(report.ChildSessionID)
	report.SourceSessionID = strings.TrimSpace(report.SourceSessionID)
	report.DestinationSessionID = strings.TrimSpace(report.DestinationSessionID)
	report.Title = normalizeWorkerText(report.Title, MaxWorkerReportTitleRunes)
	report.Body = normalizeWorkerText(report.Body, MaxWorkerReportBodyRunes)
	report.Origin = normalizeWorkerText(report.Origin, 64)
	if report.ChildSessionID == "" || report.SourceSessionID == "" || report.DestinationSessionID == "" {
		return WorkerReport{}, fmt.Errorf("worker report source and destination are required")
	}
	switch report.Kind {
	case WorkerReportProgress, WorkerReportResult, WorkerReportBlocker:
	default:
		return WorkerReport{}, fmt.Errorf("worker report kind must be progress, result, or blocker")
	}
	if report.Title == "" {
		report.Title = strings.ToUpper(string(report.Kind[:1])) + string(report.Kind[1:])
	}
	if report.Body == "" {
		return WorkerReport{}, fmt.Errorf("worker report body is required")
	}
	if report.Origin == "" {
		report.Origin = "worker_tool"
	}
	if len(report.Metadata) == 0 {
		report.Metadata = json.RawMessage(`{}`)
	}
	if len(report.Metadata) > MaxWorkerMetadataBytes {
		return WorkerReport{}, fmt.Errorf("worker report metadata exceeds %d bytes", MaxWorkerMetadataBytes)
	}
	var metadata any
	if err := json.Unmarshal(report.Metadata, &metadata); err != nil {
		return WorkerReport{}, fmt.Errorf("worker report metadata must be valid JSON: %w", err)
	}
	return report, nil
}
