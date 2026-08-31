package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type AttentionCapability string

const (
	AttentionSupported   AttentionCapability = "supported"
	AttentionUnavailable AttentionCapability = "unavailable"
	AttentionLost        AttentionCapability = "lost"
)

type AttentionSync struct {
	NodeID          string              `json:"node_id"`
	StoreInstanceID string              `json:"store_instance_id,omitempty"`
	ETag            string              `json:"etag,omitempty"`
	Capability      AttentionCapability `json:"capability_state"`
	LastSuccessAt   time.Time           `json:"last_success_at,omitempty"`
	LastErrorAt     time.Time           `json:"last_error_at,omitempty"`
	LastError       string              `json:"last_error,omitempty"`
}

type SessionActivity struct {
	NodeID          string    `json:"node_id"`
	StoreInstanceID string    `json:"store_instance_id"`
	SessionID       string    `json:"session_id"`
	Kind            string    `json:"kind"`
	ResponseID      string    `json:"response_id,omitempty"`
	LifecycleState  string    `json:"lifecycle_state,omitempty"`
	AttentionSeq    int64     `json:"attention_seq,omitempty"`
	FinalRev        int64     `json:"final_rev,omitempty"`
	ShortTitle      string    `json:"short_title,omitempty"`
	LongTitle       string    `json:"long_title,omitempty"`
	ProjectID       string    `json:"project_id,omitempty"`
	Outcome         string    `json:"outcome,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	TerminalAt      time.Time `json:"terminal_at,omitempty"`
	LeaseExpiresAt  time.Time `json:"lease_expires_at,omitempty"`
	ObservedAt      time.Time `json:"observed_at"`
}

type AttentionProjectionStore struct{ db *sql.DB }

func OpenAttentionProjectionStore(path string) (*AttentionProjectionStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create Hub attention directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create Hub attention database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	schema := `
CREATE TABLE IF NOT EXISTS node_attention_sync (
 node_id TEXT PRIMARY KEY, store_instance_id TEXT NOT NULL DEFAULT '', etag TEXT NOT NULL DEFAULT '',
 capability_state TEXT NOT NULL DEFAULT 'unavailable', last_success_at INTEGER, last_error_at INTEGER,
 last_error TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS hub_session_activity (
 node_id TEXT NOT NULL, store_instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('running','terminal_unseen')), response_id TEXT NOT NULL DEFAULT '',
 lifecycle_state TEXT NOT NULL DEFAULT '', attention_seq INTEGER NOT NULL DEFAULT 0,
 final_rev INTEGER NOT NULL DEFAULT 0, short_title TEXT NOT NULL DEFAULT '', long_title TEXT NOT NULL DEFAULT '',
 project_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT '', started_at INTEGER,
 terminal_at INTEGER, lease_expires_at INTEGER, observed_at INTEGER NOT NULL,
 PRIMARY KEY(node_id, store_instance_id, session_id, kind)
);
CREATE INDEX IF NOT EXISTS hub_session_activity_inbox ON hub_session_activity(kind, terminal_at DESC)
 WHERE kind='terminal_unseen';
CREATE INDEX IF NOT EXISTS hub_session_activity_running ON hub_session_activity(kind, started_at)
 WHERE kind='running';`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize Hub attention database: %w", err)
	}
	return &AttentionProjectionStore{db: db}, nil
}

func (s *AttentionProjectionStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func nullableMillis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().UnixMilli()
}

func (s *AttentionProjectionStore) ReplaceNode(ctx context.Context, nodeID, storeID, etag string, activities []SessionActivity) error {
	if nodeID == "" || storeID == "" {
		return errors.New("hub: node and store instance IDs are required")
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previousStoreID string
	if err := tx.QueryRowContext(ctx, `SELECT store_instance_id FROM node_attention_sync WHERE node_id=?`, nodeID).Scan(&previousStoreID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM hub_session_activity WHERE node_id=?`, nodeID); err != nil {
		return err
	}
	for _, activity := range activities {
		if activity.SessionID == "" || (activity.Kind != "running" && activity.Kind != "terminal_unseen") {
			return errors.New("hub: invalid node attention activity")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_session_activity(node_id, store_instance_id, session_id, kind,
 response_id, lifecycle_state, attention_seq, final_rev, short_title, long_title, project_id, outcome,
 started_at, terminal_at, lease_expires_at, observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			nodeID, storeID, activity.SessionID, activity.Kind, activity.ResponseID, activity.LifecycleState,
			activity.AttentionSeq, activity.FinalRev, activity.ShortTitle, activity.LongTitle, activity.ProjectID,
			activity.Outcome, nullableMillis(activity.StartedAt), nullableMillis(activity.TerminalAt),
			nullableMillis(activity.LeaseExpiresAt), now.UnixMilli()); err != nil {
			return err
		}
	}
	lastError := ""
	var lastErrorAt any
	if previousStoreID != "" && previousStoreID != storeID {
		lastError = "node session store was replaced"
		lastErrorAt = now.UnixMilli()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO node_attention_sync(node_id,store_instance_id,etag,capability_state,last_success_at,last_error_at,last_error,updated_at)
 VALUES(?,?,?,'supported',?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET store_instance_id=excluded.store_instance_id,
 etag=excluded.etag, capability_state='supported', last_success_at=excluded.last_success_at,
 last_error_at=excluded.last_error_at, last_error=excluded.last_error, updated_at=excluded.updated_at`,
		nodeID, storeID, etag, now.UnixMilli(), lastErrorAt, lastError, now.UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AttentionProjectionStore) MarkSuccess(ctx context.Context, nodeID string) error {
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `UPDATE node_attention_sync SET capability_state='supported', last_success_at=?, last_error_at=NULL, last_error='', updated_at=? WHERE node_id=?`, now, now, nodeID)
	return err
}

func (s *AttentionProjectionStore) MarkUnavailable(ctx context.Context, nodeID string, lost bool) error {
	now := time.Now().UTC().UnixMilli()
	capability := AttentionUnavailable
	if lost {
		capability = AttentionLost
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO node_attention_sync(node_id,capability_state,updated_at) VALUES(?,?,?)
 ON CONFLICT(node_id) DO UPDATE SET capability_state=excluded.capability_state, updated_at=excluded.updated_at`, nodeID, capability, now)
	return err
}

func (s *AttentionProjectionStore) MarkError(ctx context.Context, nodeID, message string) error {
	runes := []rune(message)
	if len(runes) > 512 {
		message = string(runes[:512])
	}
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `INSERT INTO node_attention_sync(node_id,capability_state,last_error_at,last_error,updated_at)
 VALUES(?,'unavailable',?,?,?) ON CONFLICT(node_id) DO UPDATE SET last_error_at=excluded.last_error_at,
 last_error=excluded.last_error, updated_at=excluded.updated_at`, nodeID, now, message, now)
	return err
}

func (s *AttentionProjectionStore) GetSync(ctx context.Context, nodeID string) (AttentionSync, error) {
	var result AttentionSync
	var success, failure sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT node_id,store_instance_id,etag,capability_state,last_success_at,last_error_at,last_error
 FROM node_attention_sync WHERE node_id=?`, nodeID).Scan(&result.NodeID, &result.StoreInstanceID, &result.ETag,
		&result.Capability, &success, &failure, &result.LastError)
	if err != nil {
		return result, err
	}
	if success.Valid {
		result.LastSuccessAt = time.UnixMilli(success.Int64).UTC()
	}
	if failure.Valid {
		result.LastErrorAt = time.UnixMilli(failure.Int64).UTC()
	}
	return result, nil
}

func (s *AttentionProjectionStore) List(ctx context.Context) ([]SessionActivity, []AttentionSync, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id,store_instance_id,session_id,kind,response_id,lifecycle_state,
 attention_seq,final_rev,short_title,long_title,project_id,outcome,started_at,terminal_at,lease_expires_at,observed_at
 FROM hub_session_activity ORDER BY CASE kind WHEN 'terminal_unseen' THEN 0 ELSE 1 END, terminal_at DESC, started_at DESC`)
	if err != nil {
		return nil, nil, err
	}
	var activities []SessionActivity
	for rows.Next() {
		var item SessionActivity
		var started, terminal, lease sql.NullInt64
		var observed int64
		if err := rows.Scan(&item.NodeID, &item.StoreInstanceID, &item.SessionID, &item.Kind, &item.ResponseID,
			&item.LifecycleState, &item.AttentionSeq, &item.FinalRev, &item.ShortTitle, &item.LongTitle,
			&item.ProjectID, &item.Outcome, &started, &terminal, &lease, &observed); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if started.Valid {
			item.StartedAt = time.UnixMilli(started.Int64).UTC()
		}
		if terminal.Valid {
			item.TerminalAt = time.UnixMilli(terminal.Int64).UTC()
		}
		if lease.Valid {
			item.LeaseExpiresAt = time.UnixMilli(lease.Int64).UTC()
		}
		item.ObservedAt = time.UnixMilli(observed).UTC()
		activities = append(activities, item)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	syncRows, err := s.db.QueryContext(ctx, `SELECT node_id,store_instance_id,etag,capability_state,last_success_at,last_error_at,last_error FROM node_attention_sync ORDER BY node_id`)
	if err != nil {
		return nil, nil, err
	}
	defer syncRows.Close()
	var syncs []AttentionSync
	for syncRows.Next() {
		var item AttentionSync
		var success, failure sql.NullInt64
		if err := syncRows.Scan(&item.NodeID, &item.StoreInstanceID, &item.ETag, &item.Capability, &success, &failure, &item.LastError); err != nil {
			return nil, nil, err
		}
		if success.Valid {
			item.LastSuccessAt = time.UnixMilli(success.Int64).UTC()
		}
		if failure.Valid {
			item.LastErrorAt = time.UnixMilli(failure.Int64).UTC()
		}
		syncs = append(syncs, item)
	}
	return activities, syncs, syncRows.Err()
}

func (s *AttentionProjectionStore) HasActivity(ctx context.Context) (bool, error) {
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hub_session_activity LIMIT 1)`).Scan(&present); err != nil {
		return false, err
	}
	return present != 0, nil
}

func (s *AttentionProjectionStore) RemoveNode(ctx context.Context, nodeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM hub_session_activity WHERE node_id=?`, nodeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_attention_sync WHERE node_id=?`, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}
