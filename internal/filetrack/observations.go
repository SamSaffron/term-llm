package filetrack

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const observationSchema = `
CREATE TABLE IF NOT EXISTS filesystem_observations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    run_id TEXT,
    event_seq INTEGER NOT NULL,
    tool_name TEXT,
    tool_call_id TEXT,
    classification TEXT NOT NULL CHECK (classification IN (
        'unclaimed_transition','materialized_output','unconfirmed_claim',
        'claim_mismatch','claim_conflict','observation_incomplete')),
    root TEXT,
    created_count INTEGER NOT NULL DEFAULT 0,
    modified_count INTEGER NOT NULL DEFAULT 0,
    deleted_count INTEGER NOT NULL DEFAULT 0,
    sampled_paths_json TEXT,
    samples_truncated INTEGER NOT NULL DEFAULT 0,
    coverage_status TEXT NOT NULL CHECK (coverage_status IN ('complete','truncated','unavailable')),
    details_json TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(session_id, event_seq)
);
CREATE INDEX IF NOT EXISTS idx_filesystem_obs_session_run ON filesystem_observations(session_id, run_id, event_seq);
CREATE INDEX IF NOT EXISTS idx_filesystem_obs_session_created ON filesystem_observations(session_id, created_at);
`

const (
	ObservationUnclaimed        = "unclaimed_transition"
	ObservationMaterialized     = "materialized_output"
	ObservationUnconfirmedClaim = "unconfirmed_claim"
	ObservationClaimMismatch    = "claim_mismatch"
	ObservationClaimConflict    = "claim_conflict"
	ObservationIncomplete       = "observation_incomplete"
)

type FilesystemObservation struct {
	ID               int64          `json:"id"`
	SessionID        string         `json:"-"`
	RunID            string         `json:"-"`
	EventSeq         int64          `json:"event_seq"`
	ToolName         string         `json:"-"`
	ToolCallID       string         `json:"-"`
	Classification   string         `json:"classification"`
	Root             string         `json:"root,omitempty"`
	CreatedCount     int            `json:"created_count"`
	ModifiedCount    int            `json:"modified_count"`
	DeletedCount     int            `json:"deleted_count"`
	SampledPaths     []string       `json:"sampled_paths,omitempty"`
	SamplesTruncated bool           `json:"samples_truncated,omitempty"`
	CoverageStatus   string         `json:"coverage_status"`
	Details          map[string]any `json:"details,omitempty"`
}

type OutputClaimDiagnostic struct {
	NormalizedPattern string `json:"normalized_pattern"`
	ClaimKind         string `json:"claim_kind"`
	Reason            string `json:"reason"`
	CoverageStatus    string `json:"coverage_status"`
	MatchingPathCount int    `json:"matching_path_count"`
	Message           string `json:"message,omitempty"`
}

type RunRecord struct {
	SessionID   string
	RunID       string
	StartedAt   time.Time
	CompletedAt time.Time
}

type SnapshotToken struct {
	AttributedEventSeq  int64 `json:"a"`
	ObservationEventSeq int64 `json:"o"`
	RunGeneration       int64 `json:"r"`
}

func (t SnapshotToken) Encode() string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeSnapshotToken(value string) (SnapshotToken, error) {
	var token SnapshotToken
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return token, err
	}
	if err := json.Unmarshal(b, &token); err != nil {
		return token, err
	}
	return token, nil
}

func openObservationDB(primaryPath string) (*sql.DB, string, error) {
	path := ":memory:"
	if primaryPath != ":memory:" {
		path = filepath.Join(filepath.Dir(primaryPath), "file_observations.db")
		if err := preparePrivateDBFile(path); err != nil {
			return nil, "", err
		}
	}
	dsn := path
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=auto_vacuum(2)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, "", fmt.Errorf("open filesystem observation database: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(observationSchema); err != nil {
		db.Close()
		return nil, "", fmt.Errorf("initialize filesystem observation schema: %w", err)
	}
	if path != ":memory:" {
		if err := chmodSQLiteFiles(path); err != nil {
			db.Close()
			return nil, "", err
		}
	}
	return db, path, nil
}

func validateChangeRecord(rec ChangeRecord, requireAttributed bool) error {
	validCoverage := rec.ClaimCoverage == CoverageComplete || rec.ClaimCoverage == CoverageTruncated || rec.ClaimCoverage == CoverageUnavailable
	validBaseline := rec.BaselineState == BaselineNormal || rec.BaselineState == BaselinePreexistingDirty || rec.BaselineState == BaselineUnknown
	if !validCoverage || !validBaseline {
		return fmt.Errorf("invalid attributed change evidence metadata")
	}
	switch rec.Provenance {
	case ProvenanceDirect:
		if rec.ClaimKind != "" {
			return fmt.Errorf("direct change must not carry a claim kind")
		}
	case ProvenanceDeclaredTransform:
		if rec.ClaimKind != ClaimTransform || rec.ClaimPattern == "" {
			return fmt.Errorf("declared transform requires its normalized transform claim")
		}
	case ProvenanceDeclaredGenerate:
		if rec.ClaimKind != ClaimGenerate || rec.ClaimPattern == "" {
			return fmt.Errorf("declared generate requires its normalized generate claim")
		}
	case ProvenanceLegacyUnverified:
		if requireAttributed {
			return fmt.Errorf("legacy_unverified is not attributable")
		}
		if rec.ClaimKind != "" {
			return fmt.Errorf("legacy row must not carry a claim kind")
		}
	default:
		return fmt.Errorf("missing or invalid change provenance %q", rec.Provenance)
	}
	return nil
}

func allocateEventSeqTx(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO filetrack_event_counters(session_id,next_event_seq) VALUES(?,2)
		ON CONFLICT(session_id) DO UPDATE SET next_event_seq=next_event_seq+1
		RETURNING next_event_seq-1`, sessionID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("allocate file tracking event sequence: %w", err)
	}
	return seq, nil
}

func (s *Store) allocateEventSeq(ctx context.Context, sessionID, runID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := ensureRunForChangeTx(ctx, tx, sessionID, runID); err != nil {
		return 0, err
	}
	seq, err := allocateEventSeqTx(ctx, tx, sessionID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

func validObservationClassification(v string) bool {
	switch v {
	case ObservationUnclaimed, ObservationMaterialized, ObservationUnconfirmedClaim, ObservationClaimMismatch, ObservationClaimConflict, ObservationIncomplete:
		return true
	}
	return false
}

func (s *Store) RecordFilesystemObservation(ctx context.Context, obs FilesystemObservation) (*FilesystemObservation, error) {
	if obs.SessionID == "" || !validObservationClassification(obs.Classification) {
		return nil, fmt.Errorf("invalid filesystem observation")
	}
	if obs.CoverageStatus == "" {
		obs.CoverageStatus = CoverageComplete
	}
	if obs.CoverageStatus != CoverageComplete && obs.CoverageStatus != CoverageTruncated && obs.CoverageStatus != CoverageUnavailable {
		return nil, fmt.Errorf("invalid observation coverage")
	}
	if len(obs.SampledPaths) > 100 {
		obs.SampledPaths = obs.SampledPaths[:100]
		obs.SamplesTruncated = true
	}
	for i := range obs.SampledPaths {
		obs.SampledPaths[i] = normalizePath(obs.SampledPaths[i])
	}
	pathsJSON, _ := json.Marshal(obs.SampledPaths)
	detailsJSON, err := json.Marshal(obs.Details)
	if err != nil {
		return nil, fmt.Errorf("encode observation details: %w", err)
	}
	obs.EventSeq, err = s.allocateEventSeq(ctx, obs.SessionID, obs.RunID)
	if err != nil {
		return nil, err
	}
	err = s.observationDB.QueryRowContext(ctx, `INSERT INTO filesystem_observations
		(session_id,run_id,event_seq,tool_name,tool_call_id,classification,root,created_count,modified_count,deleted_count,sampled_paths_json,samples_truncated,coverage_status,details_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`, obs.SessionID, nullString(obs.RunID), obs.EventSeq,
		obs.ToolName, obs.ToolCallID, obs.Classification, nullString(normalizePath(obs.Root)), obs.CreatedCount,
		obs.ModifiedCount, obs.DeletedCount, string(pathsJSON), boolInt(obs.SamplesTruncated), obs.CoverageStatus, string(detailsJSON)).Scan(&obs.ID)
	if err != nil {
		return nil, fmt.Errorf("insert filesystem observation: %w", err)
	}
	if err := s.pruneObservations(ctx, obs.SessionID); err != nil {
		return &obs, fmt.Errorf("prune filesystem observations: %w", err)
	}
	var exists bool
	if err := s.observationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM filesystem_observations WHERE id=?)`, obs.ID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("verify filesystem observation: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("filesystem observation exceeded its independent retention budget")
	}
	return &obs, nil
}

func (s *Store) pruneObservations(ctx context.Context, sessionID string) error {
	if s.maxObservationAgeDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -s.maxObservationAgeDays)
		if _, err := s.observationDB.ExecContext(ctx, `DELETE FROM filesystem_observations WHERE created_at < ?`, cutoff); err != nil {
			return err
		}
	}
	_, err := s.observationDB.ExecContext(ctx, `DELETE FROM filesystem_observations WHERE id IN (
		SELECT id FROM filesystem_observations WHERE session_id=? ORDER BY event_seq DESC LIMIT -1 OFFSET ?)`, sessionID, s.maxObservationSessionRows)
	if err != nil {
		return err
	}
	_, err = s.observationDB.ExecContext(ctx, `DELETE FROM filesystem_observations WHERE id IN (SELECT id FROM (
		SELECT id, SUM(LENGTH(COALESCE(root,''))+LENGTH(COALESCE(sampled_paths_json,''))+LENGTH(COALESCE(details_json,''))+128)
		OVER (ORDER BY event_seq DESC) AS used FROM filesystem_observations WHERE session_id=?) WHERE used>?)`, sessionID, s.maxObservationSessionBytes)
	if err != nil {
		return err
	}
	_, err = s.observationDB.ExecContext(ctx, `DELETE FROM filesystem_observations WHERE id IN (
		SELECT id FROM filesystem_observations ORDER BY created_at DESC,id DESC LIMIT -1 OFFSET ?)`, s.maxObservationRows)
	if err != nil {
		return err
	}
	_, err = s.observationDB.ExecContext(ctx, `DELETE FROM filesystem_observations WHERE id IN (SELECT id FROM (
		SELECT id, SUM(LENGTH(COALESCE(root,''))+LENGTH(COALESCE(sampled_paths_json,''))+LENGTH(COALESCE(details_json,''))+128)
		OVER (ORDER BY created_at DESC,id DESC) AS used FROM filesystem_observations) WHERE used>?)`, s.maxObservationBytes)
	return err
}

func (s *Store) RecordRunStart(ctx context.Context, run RunRecord) error {
	if run.SessionID == "" || run.RunID == "" {
		return nil
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file tracking run start: %w", err)
	}
	defer tx.Rollback()
	// A newly started run closes any abandoned predecessor. This keeps the run
	// lifecycle accurate after a caller fails to drain/Close a stream or after a
	// process crash.
	if _, err := tx.ExecContext(ctx, `UPDATE filetrack_runs SET completed_at=? WHERE session_id=? AND completed_at IS NULL AND run_id<>?`, run.StartedAt, run.SessionID, run.RunID); err != nil {
		return fmt.Errorf("close abandoned file tracking run: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO filetrack_runs(session_id,run_id,ordinal,started_at)
		VALUES(?,?,(SELECT COALESCE(MAX(ordinal),0)+1 FROM filetrack_runs WHERE session_id=?),?)
		ON CONFLICT(session_id,run_id) DO NOTHING`, run.SessionID, run.RunID, run.SessionID, run.StartedAt)
	if err != nil {
		return fmt.Errorf("record file tracking run start: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit file tracking run start: %w", err)
	}
	return nil
}

func (s *Store) RecordRunComplete(ctx context.Context, run RunRecord) error {
	if run.SessionID == "" || run.RunID == "" {
		return nil
	}
	if err := s.RecordRunStart(ctx, run); err != nil {
		return err
	}
	if run.CompletedAt.IsZero() {
		run.CompletedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE filetrack_runs SET completed_at=? WHERE session_id=? AND run_id=?`, run.CompletedAt, run.SessionID, run.RunID)
	if err != nil {
		return fmt.Errorf("record file tracking run completion: %w", err)
	}
	return nil
}

func ensureRunForChangeTx(ctx context.Context, tx *sql.Tx, sessionID, runID string) error {
	if runID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO filetrack_runs(session_id,run_id,ordinal,started_at,completed_at)
		VALUES(?,?,(SELECT COALESCE(MAX(ordinal),0)+1 FROM filetrack_runs WHERE session_id=?),CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)
		ON CONFLICT(session_id,run_id) DO NOTHING`, sessionID, runID, sessionID)
	return err
}

func (s *Store) ListRunObservations(ctx context.Context, sessionID string, runIDs []string) ([]FilesystemObservation, error) {
	if len(runIDs) == 0 {
		return []FilesystemObservation{}, nil
	}
	args := []any{sessionID}
	for _, id := range runIDs {
		args = append(args, id)
	}
	query := `SELECT id,run_id,event_seq,COALESCE(tool_name,''),COALESCE(tool_call_id,''),classification,COALESCE(root,''),created_count,modified_count,deleted_count,COALESCE(sampled_paths_json,'[]'),samples_truncated,coverage_status,COALESCE(details_json,'{}') FROM filesystem_observations WHERE session_id=? AND run_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",") + `) ORDER BY event_seq`
	rows, err := s.observationDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []FilesystemObservation{}
	for rows.Next() {
		var obs FilesystemObservation
		var pathsJSON, detailsJSON string
		if err := rows.Scan(&obs.ID, &obs.RunID, &obs.EventSeq, &obs.ToolName, &obs.ToolCallID, &obs.Classification, &obs.Root, &obs.CreatedCount, &obs.ModifiedCount, &obs.DeletedCount, &pathsJSON, &obs.SamplesTruncated, &obs.CoverageStatus, &detailsJSON); err != nil {
			return nil, err
		}
		obs.SessionID = sessionID
		_ = json.Unmarshal([]byte(pathsJSON), &obs.SampledPaths)
		_ = json.Unmarshal([]byte(detailsJSON), &obs.Details)
		result = append(result, obs)
	}
	return result, rows.Err()
}

func (s *Store) RecentRunIDs(ctx context.Context, sessionID string, limit int) ([]string, error) {
	window, err := s.latestRunWindow(ctx, sessionID, limit, 0)
	return window.runIDs, err
}

func (s *Store) CurrentSnapshotToken(ctx context.Context, sessionID string) string {
	var token SnapshotToken
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(event_seq),0) FROM file_changes WHERE session_id=?`, sessionID).Scan(&token.AttributedEventSeq)
	_ = s.observationDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(event_seq),0) FROM filesystem_observations WHERE session_id=?`, sessionID).Scan(&token.ObservationEventSeq)
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal),0) FROM filetrack_runs WHERE session_id=?`, sessionID).Scan(&token.RunGeneration)
	return token.Encode()
}

func (s *Store) gcObservations(ctx context.Context, sessionsDBPath string, maxAgeDays int) error {
	conn, err := s.observationDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire observation gc connection: %w", err)
	}
	defer conn.Close()
	if sessionsDBPath != "" && sessionsDBPath != ":memory:" {
		if _, statErr := os.Stat(sessionsDBPath); statErr == nil {
			uri := "file:" + filepath.ToSlash(sessionsDBPath) + "?mode=ro"
			if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS sess", uri); err != nil {
				return err
			}
			_, delErr := conn.ExecContext(ctx, "DELETE FROM filesystem_observations WHERE session_id NOT IN (SELECT id FROM sess.sessions)")
			_, detachErr := conn.ExecContext(ctx, "DETACH DATABASE sess")
			if delErr != nil {
				return fmt.Errorf("gc stale observations: %w", delErr)
			}
			if detachErr != nil {
				return detachErr
			}
		}
	}
	age := maxAgeDays
	if age <= 0 {
		age = s.maxObservationAgeDays
	}
	if age > 0 {
		cutoff := time.Now().AddDate(0, 0, -age)
		if _, err := conn.ExecContext(ctx, "DELETE FROM filesystem_observations WHERE created_at < ?", cutoff); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
		return err
	}
	return nil
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func worstCoverage(a, b string) string {
	rank := map[string]int{CoverageComplete: 0, CoverageTruncated: 1, CoverageUnavailable: 2}
	if rank[b] > rank[a] {
		return b
	}
	if a == "" {
		return b
	}
	return a
}
func cumulativeContentStatus(sp *pathSpan, kind string) string {
	if kind == KindDelete {
		return sp.firstContentStatus
	}
	if kind == KindCreate {
		return sp.lastContentStatus
	}
	if sp.firstContentStatus != ContentRetained && sp.firstContentStatus != ContentRetainedImage {
		return sp.firstContentStatus
	}
	return sp.lastContentStatus
}
