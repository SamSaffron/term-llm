package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func (s *SQLiteStore) requireWorkers() error {
	if s == nil || !s.hasSessionWorkers {
		return ErrWorkersUnsupported
	}
	return nil
}

// CreateWorker creates an empty child chat session and its non-branch worker
// edge in one immediate transaction. Runtime settings and non-yolo workspace
// grants are inherited, but no transcript or provider state is copied.
func (s *SQLiteStore) CreateWorker(ctx context.Context, coordinatorSessionID, task string) (WorkerEdge, error) {
	return s.CreateWorkerOwned(ctx, coordinatorSessionID, "", task)
}

func canonicalWorkerCWD(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

// CreateWorkerOwned records the embedded chat runtime that owns the worker so
// another live chat cannot claim or clear it. A later runtime may explicitly
// adopt the edge after the owner's durable lease expires.
func (s *SQLiteStore) CreateWorkerOwned(ctx context.Context, coordinatorSessionID, ownerID, task string) (WorkerEdge, error) {
	if err := s.requireWorkers(); err != nil {
		return WorkerEdge{}, err
	}
	coordinatorSessionID = strings.TrimSpace(coordinatorSessionID)
	task, err := normalizeWorkerTask(task)
	if err != nil {
		return WorkerEdge{}, err
	}
	if coordinatorSessionID == "" {
		return WorkerEdge{}, ErrNotFound
	}

	var edge WorkerEdge
	err = retryOnBusy(ctx, 5, func() error {
		conn, err := s.beginImmediate(ctx)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				rollbackImmediate(conn)
				return
			}
			_ = conn.Close()
		}()

		var coordinatorCWD string
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(cwd, '') FROM sessions WHERE id = ?`, coordinatorSessionID).Scan(&coordinatorCWD); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("load coordinator workspace: %w", err)
		}
		coordinatorCWD = canonicalWorkerCWD(coordinatorCWD)
		rows, err := conn.QueryContext(ctx, `
			SELECT w.coordinator_session_id, COALESCE(child.cwd, '')
			FROM session_workers w
			JOIN sessions child ON child.id = w.child_session_id
			WHERE w.status IN (?, ?, ?)`, WorkerQueued, WorkerRunning, WorkerBlocked)
		if err != nil {
			return fmt.Errorf("check active workers: %w", err)
		}
		conflict := false
		for rows.Next() {
			var activeCoordinator, activeCWD string
			if err := rows.Scan(&activeCoordinator, &activeCWD); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan active worker: %w", err)
			}
			conflict = conflict || activeCoordinator == coordinatorSessionID ||
				(coordinatorCWD != "" && canonicalWorkerCWD(activeCWD) == coordinatorCWD)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan active workers: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close active workers: %w", err)
		}
		if conflict {
			return fmt.Errorf("a worker is already active for this coordinator or workspace; wait, cancel it in /tree, or use an isolated worktree")
		}

		now := time.Now().UTC()
		childID := NewID()
		result, err := conn.ExecContext(ctx, `
			INSERT INTO sessions (
				id, number, name, summary, provider, provider_key, model,
				reasoning_effort, reasoning_mode, mode, approval_mode, origin,
				agent, cwd, worktree_dir, created_at, updated_at, archived,
				pinned, parent_id, search, tools, mcp, status, compaction_seq
			)
			SELECT ?, (SELECT COALESCE(MAX(number), 0) + 1 FROM sessions), ?, ?,
			       provider, provider_key, model, reasoning_effort, reasoning_mode,
			       'chat', approval_mode, 'tui', 'developer', cwd, worktree_dir,
			       ?, ?, FALSE, FALSE, NULL, search, tools, mcp, 'active', -1
			FROM sessions WHERE id = ?`,
			childID, "Worker: "+TruncateSummary(task), task, now, now, coordinatorSessionID)
		if err != nil {
			return fmt.Errorf("create worker session: %w", err)
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrNotFound
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO session_workspace_grants (
				session_id, id, path, access, provenance, rationale, created_at, updated_at
			)
			SELECT ?, id, path, access, provenance, rationale, created_at, updated_at
			FROM session_workspace_grants
			WHERE session_id = ? AND LOWER(TRIM(provenance)) <> 'yolo'`, childID, coordinatorSessionID); err != nil {
			return fmt.Errorf("copy worker workspace grants: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO session_workers (
				child_session_id, coordinator_session_id, owner_id, task, status, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`, childID, coordinatorSessionID, strings.TrimSpace(ownerID), task, WorkerQueued, now, now); err != nil {
			return fmt.Errorf("create worker edge: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit worker creation: %w", err)
		}
		committed = true
		edge = WorkerEdge{
			ChildSessionID: childID, CoordinatorSessionID: coordinatorSessionID, OwnerID: strings.TrimSpace(ownerID),
			Task: task, Status: WorkerQueued, CreatedAt: now, UpdatedAt: now,
		}
		return nil
	})
	return edge, err
}

func (s *SQLiteStore) SetWorkerExecution(ctx context.Context, childSessionID, jobID, runID string) error {
	if err := s.requireWorkers(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_workers SET job_id = ?, run_id = ?, updated_at = ?
		WHERE child_session_id = ?`, strings.TrimSpace(jobID), strings.TrimSpace(runID), time.Now().UTC(), strings.TrimSpace(childSessionID))
	if err != nil {
		return fmt.Errorf("set worker execution: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) SetWorkerOwner(ctx context.Context, childSessionID, oldOwnerID, newOwnerID string) (bool, error) {
	if err := s.requireWorkers(); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_workers SET owner_id = ?
		WHERE child_session_id = ? AND owner_id = ?`, strings.TrimSpace(newOwnerID),
		strings.TrimSpace(childSessionID), strings.TrimSpace(oldOwnerID))
	if err != nil {
		return false, fmt.Errorf("set worker owner: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (s *SQLiteStore) UpdateWorkerStatus(ctx context.Context, childSessionID string, status WorkerStatus) error {
	if err := s.requireWorkers(); err != nil {
		return err
	}
	switch status {
	case WorkerQueued, WorkerRunning, WorkerBlocked, WorkerDone, WorkerFailed, WorkerCancelled:
	default:
		return fmt.Errorf("invalid worker status %q", status)
	}
	now := time.Now().UTC()
	var finished any
	if status.Terminal() {
		finished = now
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_workers SET status = ?, updated_at = ?,
			finished_at = CASE WHEN ? IS NULL THEN finished_at ELSE ? END
		WHERE child_session_id = ? AND status NOT IN (?, ?, ?)`, status, now, finished, finished,
		strings.TrimSpace(childSessionID), WorkerDone, WorkerFailed, WorkerCancelled)
	if err != nil {
		return fmt.Errorf("update worker status: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM session_workers WHERE child_session_id = ?`, strings.TrimSpace(childSessionID)).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("check worker status target: %w", err)
		}
	}
	return nil
}

// FinishWorker is idempotent: result insertion collapses concurrent terminal
// synthesis to one immutable result, then the edge advances to a terminal state.
func (s *SQLiteStore) FinishWorker(ctx context.Context, childSessionID string, status WorkerStatus, report WorkerReport) error {
	if !status.Terminal() {
		return fmt.Errorf("worker finish status %q is not terminal", status)
	}
	if strings.TrimSpace(report.ChildSessionID) == "" {
		report.ChildSessionID = strings.TrimSpace(childSessionID)
	}
	if report.Kind != WorkerReportResult {
		return fmt.Errorf("worker finish report must be a result")
	}
	if _, err := s.AddWorkerReport(ctx, report); err != nil {
		return err
	}
	return s.UpdateWorkerStatus(ctx, childSessionID, status)
}

const workerEdgeSelect = `
	SELECT w.child_session_id, w.coordinator_session_id, COALESCE(w.owner_id, ''), w.task, w.status,
	       COALESCE(w.job_id, ''), COALESCE(w.run_id, ''),
	       (SELECT COUNT(1) FROM session_worker_reports r
	        WHERE r.child_session_id = w.child_session_id AND r.read_at IS NULL),
	       w.created_at, w.updated_at, w.finished_at
	FROM session_workers w`

func scanWorkerEdge(scanner interface{ Scan(...any) error }) (WorkerEdge, error) {
	var edge WorkerEdge
	var status string
	var finished sql.NullTime
	if err := scanner.Scan(&edge.ChildSessionID, &edge.CoordinatorSessionID, &edge.OwnerID, &edge.Task, &status,
		&edge.JobID, &edge.RunID, &edge.UnreadReports, &edge.CreatedAt, &edge.UpdatedAt, &finished); err != nil {
		return WorkerEdge{}, err
	}
	edge.Status = WorkerStatus(status)
	if finished.Valid {
		t := finished.Time
		edge.FinishedAt = &t
	}
	return edge, nil
}

func (s *SQLiteStore) GetWorker(ctx context.Context, childSessionID string) (WorkerEdge, error) {
	if err := s.requireWorkers(); err != nil {
		return WorkerEdge{}, err
	}
	edge, err := scanWorkerEdge(s.db.QueryRowContext(ctx, workerEdgeSelect+` WHERE w.child_session_id = ?`, strings.TrimSpace(childSessionID)))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerEdge{}, ErrNotFound
	}
	if err != nil {
		return WorkerEdge{}, fmt.Errorf("get worker: %w", err)
	}
	return edge, nil
}

func (s *SQLiteStore) ListWorkers(ctx context.Context, coordinatorSessionID string) ([]WorkerEdge, error) {
	if err := s.requireWorkers(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, workerEdgeSelect+` WHERE w.coordinator_session_id = ? ORDER BY w.created_at, w.child_session_id`, strings.TrimSpace(coordinatorSessionID))
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer rows.Close()
	var workers []WorkerEdge
	for rows.Next() {
		edge, err := scanWorkerEdge(rows)
		if err != nil {
			return nil, fmt.Errorf("scan worker: %w", err)
		}
		workers = append(workers, edge)
	}
	return workers, rows.Err()
}

func (s *SQLiteStore) AddWorkerReport(ctx context.Context, report WorkerReport) (WorkerReport, error) {
	if err := s.requireWorkers(); err != nil {
		return WorkerReport{}, err
	}
	report, err := normalizeWorkerReport(report)
	if err != nil {
		return WorkerReport{}, err
	}
	err = retryOnBusy(ctx, 5, func() error {
		conn, err := s.beginImmediate(ctx)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				rollbackImmediate(conn)
				return
			}
			_ = conn.Close()
		}()
		var coordinator string
		if err := conn.QueryRowContext(ctx, `SELECT coordinator_session_id FROM session_workers WHERE child_session_id = ?`, report.ChildSessionID).Scan(&coordinator); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if report.SourceSessionID != report.ChildSessionID || report.DestinationSessionID != coordinator {
			return fmt.Errorf("worker report route does not match durable worker edge")
		}
		if report.Kind == WorkerReportResult {
			existing, err := scanWorkerReport(conn.QueryRowContext(ctx, workerReportSelect+` WHERE child_session_id = ? AND kind = ? ORDER BY sequence, id LIMIT 1`, report.ChildSessionID, WorkerReportResult))
			if err == nil {
				report = existing
				if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
					return err
				}
				committed = true
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("check existing worker result: %w", err)
			}
		}
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), -1) + 1 FROM session_worker_reports WHERE child_session_id = ?`, report.ChildSessionID).Scan(&report.Sequence); err != nil {
			return fmt.Errorf("allocate worker report sequence: %w", err)
		}
		report.CreatedAt = time.Now().UTC()
		if err := conn.QueryRowContext(ctx, `
			INSERT INTO session_worker_reports (
				child_session_id, source_session_id, destination_session_id,
				kind, title, body, metadata, origin, sequence, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			report.ChildSessionID, report.SourceSessionID, report.DestinationSessionID,
			report.Kind, report.Title, report.Body, string(report.Metadata), report.Origin,
			report.Sequence, report.CreatedAt).Scan(&report.ID); err != nil {
			return fmt.Errorf("insert worker report: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	})
	return report, err
}

const workerReportSelect = `
	SELECT id, child_session_id, source_session_id, destination_session_id,
	       kind, title, body, metadata, origin, sequence, read_at,
	       imported_at, COALESCE(imported_message_id, 0), created_at
	FROM session_worker_reports`

func scanWorkerReport(scanner interface{ Scan(...any) error }) (WorkerReport, error) {
	var report WorkerReport
	var kind, metadata string
	var readAt, importedAt sql.NullTime
	if err := scanner.Scan(&report.ID, &report.ChildSessionID, &report.SourceSessionID,
		&report.DestinationSessionID, &kind, &report.Title, &report.Body, &metadata,
		&report.Origin, &report.Sequence, &readAt, &importedAt,
		&report.ImportedMessageID, &report.CreatedAt); err != nil {
		return WorkerReport{}, err
	}
	report.Kind = WorkerReportKind(kind)
	report.Metadata = json.RawMessage(metadata)
	if readAt.Valid {
		t := readAt.Time
		report.ReadAt = &t
	}
	if importedAt.Valid {
		t := importedAt.Time
		report.ImportedAt = &t
	}
	return report, nil
}

func (s *SQLiteStore) ListWorkerReports(ctx context.Context, childSessionID string) ([]WorkerReport, error) {
	if err := s.requireWorkers(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, workerReportSelect+` WHERE child_session_id = ? ORDER BY sequence, id`, strings.TrimSpace(childSessionID))
	if err != nil {
		return nil, fmt.Errorf("list worker reports: %w", err)
	}
	defer rows.Close()
	var reports []WorkerReport
	for rows.Next() {
		report, err := scanWorkerReport(rows)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

func (s *SQLiteStore) MarkWorkerReportRead(ctx context.Context, reportID int64) error {
	if err := s.requireWorkers(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE session_worker_reports SET read_at = COALESCE(read_at, ?) WHERE id = ?`, time.Now().UTC(), reportID)
	if err != nil {
		return fmt.Errorf("mark worker report read: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

func workerImportText(report WorkerReport, edge WorkerEdge) string {
	return fmt.Sprintf("[Imported worker report]\nWorker session: %s\nKind: %s\nTitle: %s\nTask: %s\n\n%s",
		ShortID(report.SourceSessionID), report.Kind, report.Title, edge.Task, report.Body)
}

// ImportWorkerReport atomically appends an ordinary role=user message to the
// coordinator and records immutable report provenance through import metadata.
// Mailbox content is never serialized as developer/system authority.
func (s *SQLiteStore) ImportWorkerReport(ctx context.Context, reportID int64) (*Message, error) {
	if err := s.requireWorkers(); err != nil {
		return nil, err
	}
	var imported *Message
	var existingMessageID int64
	err := retryOnBusy(ctx, 5, func() error {
		conn, err := s.beginImmediate(ctx)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				rollbackImmediate(conn)
				return
			}
			_ = conn.Close()
		}()
		report, err := scanWorkerReport(conn.QueryRowContext(ctx, workerReportSelect+` WHERE id = ?`, reportID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if report.ImportedAt != nil && report.ImportedMessageID == 0 {
			return fmt.Errorf("worker report has already been imported; its attributed message no longer exists")
		}
		if report.ImportedMessageID != 0 {
			existingMessageID = report.ImportedMessageID
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			committed = true
			return nil
		}
		var task string
		if err := conn.QueryRowContext(ctx, `SELECT task FROM session_workers WHERE child_session_id = ? AND coordinator_session_id = ?`, report.ChildSessionID, report.DestinationSessionID).Scan(&task); err != nil {
			return ErrNotFound
		}
		edge := WorkerEdge{Task: task}
		text := workerImportText(report, edge)
		llmMessage := llm.UserText(text)
		message := NewMessage(report.DestinationSessionID, llmMessage, -1)
		message.CreatedAt = time.Now().UTC()
		partsJSON, err := message.PartsJSONForStorage(s.cfg.StripImageBase64)
		if err != nil {
			return err
		}
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), -1) + 1 FROM messages WHERE session_id = ?`, report.DestinationSessionID).Scan(&message.Sequence); err != nil {
			return err
		}
		if err := conn.QueryRowContext(ctx, `
			INSERT INTO messages (
				session_id, role, parts, text_content, duration_ms, turn_index,
				created_at, sequence, compaction_tail, client_message_id, response_id,
				assistant_segment_ordinal, segment_start_sequence, segment_end_sequence
			) VALUES (?, 'user', ?, ?, 0, 0, ?, ?, FALSE, '', '', -1, 0, 0)
			RETURNING id`, report.DestinationSessionID, partsJSON, text, message.CreatedAt, message.Sequence).Scan(&message.ID); err != nil {
			return fmt.Errorf("insert imported worker message: %w", err)
		}
		now := time.Now().UTC()
		if _, err := conn.ExecContext(ctx, `
			UPDATE sessions SET user_turns = user_turns + 1, updated_at = ?,
				last_user_message_at = ?, last_message_at = ? WHERE id = ?`,
			now, now, now, report.DestinationSessionID); err != nil {
			return err
		}
		if _, err := s.bumpTranscriptRev(ctx, conn, report.DestinationSessionID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE session_worker_reports
			SET read_at = COALESCE(read_at, ?), imported_at = ?, imported_message_id = ?
			WHERE id = ? AND imported_at IS NULL`, now, now, message.ID, reportID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		imported = message
		return nil
	})
	if err == nil && imported == nil && existingMessageID != 0 {
		imported, err = s.GetMessageByID(ctx, existingMessageID)
	}
	return imported, err
}

func (s *SQLiteStore) CountUnreadWorkerReports(ctx context.Context, coordinatorSessionID string) (int, error) {
	if err := s.requireWorkers(); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM session_worker_reports WHERE destination_session_id = ? AND read_at IS NULL`, strings.TrimSpace(coordinatorSessionID)).Scan(&count)
	return count, err
}
