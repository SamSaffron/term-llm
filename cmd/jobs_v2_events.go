package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func (m *jobsV2Manager) addRunEvent(runID, eventType, message string, payload any) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(eventType) == "" {
		return nil
	}
	var data string
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		data = string(encoded)
	}
	_, err := m.db.Exec(`INSERT INTO job_run_events_v2 (run_id, event_type, message, data, created_at) VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		runID, eventType, strings.TrimSpace(message), nullableString(data))
	return err
}

func (m *jobsV2Manager) ListRunEvents(runID string, sinceID int64, limit, offset int) ([]jobsV2RunEvent, int, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, 0, fmt.Errorf("run_id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	var where string
	args := make([]any, 0, 4)
	args = append(args, runID)
	if sinceID > 0 {
		where = "WHERE run_id = ? AND id > ?"
		args = append(args, sinceID)
	} else {
		where = "WHERE run_id = ?"
	}

	var total int
	countQuery := fmt.Sprintf(`SELECT COUNT(1) FROM job_run_events_v2 %s`, where)
	if err := m.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rowsArgs := append(append([]any{}, args...), limit, offset)
	// Event IDs are AUTOINCREMENT and match insertion order. Ordering by id lets
	// since_id polling seek into idx_job_run_events_v2_run_id_id instead of
	// walking the full per-run created_at timeline on every incremental poll.
	query := fmt.Sprintf(`SELECT id, run_id, event_type, message, data, created_at FROM job_run_events_v2 %s ORDER BY id ASC LIMIT ? OFFSET ?`, where)
	rows, err := m.db.Query(query, rowsArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	events := make([]jobsV2RunEvent, 0)
	for rows.Next() {
		var ev jobsV2RunEvent
		var message sql.NullString
		var data sql.NullString
		if err := rows.Scan(&ev.ID, &ev.RunID, &ev.EventType, &message, &data, &ev.CreatedAt); err != nil {
			return nil, 0, err
		}
		if message.Valid {
			ev.Message = message.String
		}
		if data.Valid {
			ev.Data = json.RawMessage(data.String)
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return events, total, nil
}
