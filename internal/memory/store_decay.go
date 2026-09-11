package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

func (s *Store) BumpAccess(ctx context.Context, fragmentID string) error {
	fragmentID = strings.TrimSpace(fragmentID)
	if fragmentID == "" {
		return fmt.Errorf("fragment_id is required")
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE memory_fragments
		SET accessed_at = ?,
		    access_count = access_count + 1
		WHERE id = ?`, time.Now(), fragmentID)
	if err != nil {
		return fmt.Errorf("bump fragment access: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("fragment %s not found", fragmentID)
	}
	return nil
}

const (
	DefaultDecayHalfLifeDays = 30.0
	MinDecayScore            = 0.04
	DefaultDecayGCThreshold  = 0.05
)

// DecayPreview is a non-persistent age-based decay calculation for a fragment.
type DecayPreview struct {
	ID           string
	Agent        string
	Path         string
	UpdatedAt    time.Time
	AccessedAt   *time.Time
	CurrentScore float64
	PreviewScore float64
}

// ComputeDecayScore returns the age-based decay score that would apply for the
// supplied timestamps. It is pure: callers decide whether to use it for ranking,
// reporting, or persistence.
func ComputeDecayScore(updatedAt time.Time, accessedAt *time.Time, halfLifeDays float64, now time.Time) float64 {
	if halfLifeDays <= 0 {
		halfLifeDays = DefaultDecayHalfLifeDays
	}
	if now.IsZero() {
		now = time.Now()
	}

	lastActive := updatedAt
	if accessedAt != nil && accessedAt.After(lastActive) {
		lastActive = *accessedAt
	}
	if lastActive.IsZero() || lastActive.After(now) {
		return 1.0
	}

	ageDays := now.Sub(lastActive).Hours() / 24.0
	if ageDays < 0 {
		ageDays = 0
	}
	decay := math.Pow(0.5, ageDays/halfLifeDays)
	return math.Max(decay, MinDecayScore)
}

// PreviewDecayScores calculates age-based decay scores without writing them to
// the database. Results are sorted from lowest preview score to highest.
func (s *Store) PreviewDecayScores(ctx context.Context, agent string, halfLifeDays float64, limit int) ([]DecayPreview, error) {
	agent = strings.TrimSpace(agent)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, path, updated_at, accessed_at, decay_score
		FROM memory_fragments
		WHERE pinned = 0
		  AND (? = '' OR agent = ?)`, agent, agent)
	if err != nil {
		return nil, fmt.Errorf("query fragments for decay preview: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	out := []DecayPreview{}
	for rows.Next() {
		var p DecayPreview
		var accessedAt sql.NullTime
		if err := rows.Scan(&p.ID, &p.Agent, &p.Path, &p.UpdatedAt, &accessedAt, &p.CurrentScore); err != nil {
			return nil, fmt.Errorf("scan fragment for decay preview: %w", err)
		}
		if accessedAt.Valid {
			at := accessedAt.Time
			p.AccessedAt = &at
		}
		p.PreviewScore = ComputeDecayScore(p.UpdatedAt, p.AccessedAt, halfLifeDays, now)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fragments for decay preview: %w", err)
	}

	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].PreviewScore-out[j].PreviewScore) < 1e-12 {
			if out[i].Agent == out[j].Agent {
				return out[i].Path < out[j].Path
			}
			return out[i].Agent < out[j].Agent
		}
		return out[i].PreviewScore < out[j].PreviewScore
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CountDecayCandidates counts fragments that would fall below threshold if
// age-based decay were applied, without mutating decay_score.
func (s *Store) CountDecayCandidates(ctx context.Context, agent string, halfLifeDays, threshold float64) (int, error) {
	if threshold <= 0 {
		threshold = DefaultDecayGCThreshold
	}
	previews, err := s.PreviewDecayScores(ctx, agent, halfLifeDays, 0)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, preview := range previews {
		if preview.PreviewScore < threshold {
			count++
		}
	}
	return count, nil
}

// RecalcDecayScores recalculates decay_score for non-pinned fragments.
// halfLifeDays defaults to 30 when <= 0.
// Deprecated: prefer PreviewDecayScores or ComputeDecayScore; routine memory
// maintenance should not rewrite decay scores.
func (s *Store) RecalcDecayScores(ctx context.Context, agent string, halfLifeDays float64) (int, error) {
	agent = strings.TrimSpace(agent)
	if halfLifeDays <= 0 {
		halfLifeDays = DefaultDecayHalfLifeDays
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin decay recalculation transaction: %w", err)
	}
	defer tx.Rollback()

	type decayUpdate struct {
		id    string
		score float64
	}
	updates := []decayUpdate{}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, updated_at, accessed_at
		FROM memory_fragments
		WHERE pinned = 0
		  AND (? = '' OR agent = ?)`, agent, agent)
	if err != nil {
		return 0, fmt.Errorf("query fragments for decay recalculation: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var id string
		var updatedAt time.Time
		var accessedAt sql.NullTime
		if err := rows.Scan(&id, &updatedAt, &accessedAt); err != nil {
			return 0, fmt.Errorf("scan fragment for decay recalculation: %w", err)
		}

		var accessedAtPtr *time.Time
		if accessedAt.Valid {
			at := accessedAt.Time
			accessedAtPtr = &at
		}
		finalDecay := ComputeDecayScore(updatedAt, accessedAtPtr, halfLifeDays, now)
		updates = append(updates, decayUpdate{id: id, score: finalDecay})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate fragments for decay recalculation: %w", err)
	}

	if len(updates) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit decay recalculation: %w", err)
		}
		return 0, nil
	}

	stmt, err := tx.PrepareContext(ctx, `UPDATE memory_fragments SET decay_score = ? WHERE id = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare decay recalculation update: %w", err)
	}
	defer stmt.Close()

	updatedCount := 0
	for _, update := range updates {
		res, err := stmt.ExecContext(ctx, update.score, update.id)
		if err != nil {
			return 0, fmt.Errorf("update decay score for fragment %s: %w", update.id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updatedCount += int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit decay recalculation: %w", err)
	}
	return updatedCount, nil
}

// CountGCCandidates counts fragments eligible for GC.
func (s *Store) CountGCCandidates(ctx context.Context, agent string) (int, error) {
	agent = strings.TrimSpace(agent)

	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM memory_fragments
		WHERE decay_score < 0.05
		  AND pinned = 0
		  AND (? = '' OR agent = ?)`, agent, agent).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count gc candidates: %w", err)
	}
	return count, nil
}

// GCFragments deletes decayed, non-pinned fragments and keeps FTS in sync.
func (s *Store) GCFragments(ctx context.Context, agent string) (int, error) {
	agent = strings.TrimSpace(agent)

	type gcCandidate struct {
		rowID   int64
		id      string
		agent   string
		path    string
		content string
	}
	candidates := []gcCandidate{}

	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid, id, agent, path, content
		FROM memory_fragments
		WHERE decay_score < 0.05
		  AND pinned = 0
		  AND (? = '' OR agent = ?)`, agent, agent)
	if err != nil {
		return 0, fmt.Errorf("query gc candidates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var c gcCandidate
		if err := rows.Scan(&c.rowID, &c.id, &c.agent, &c.path, &c.content); err != nil {
			return 0, fmt.Errorf("scan gc candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate gc candidates: %w", err)
	}

	if len(candidates) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin gc transaction: %w", err)
	}
	defer tx.Rollback()

	removed := 0
	for _, c := range candidates {
		frag := &Fragment{ID: c.id, Agent: c.agent, Path: c.path, Content: c.content}
		if err := syncFTSDelete(ctx, tx, c.rowID, frag); err != nil {
			return 0, fmt.Errorf("sync fts delete during gc for fragment %s: %w", c.id, err)
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM memory_fragments WHERE rowid = ?`, c.rowID)
		if err != nil {
			return 0, fmt.Errorf("delete fragment during gc %s: %w", c.id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit gc fragments: %w", err)
	}
	return removed, nil
}

// GetMeta returns a value from memory_meta. Missing keys return "", nil.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("meta key is required")
	}

	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM memory_meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get meta value: %w", err)
	}
	return value, nil
}

// SetMeta upserts a key/value pair in memory_meta.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("meta key is required")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_meta(key, value)
		VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set meta value: %w", err)
	}
	return nil
}

// ListStates returns all session mining states in one query.
func (s *Store) ListStates(ctx context.Context) ([]MiningState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, agent, last_mined_offset, mined_at
		FROM memory_mining_state`)
	if err != nil {
		return nil, fmt.Errorf("list mining states: %w", err)
	}
	defer rows.Close()

	states := make([]MiningState, 0)
	for rows.Next() {
		var state MiningState
		if err := rows.Scan(&state.SessionID, &state.Agent, &state.LastMinedOffset, &state.MinedAt); err != nil {
			return nil, fmt.Errorf("scan mining state: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mining states: %w", err)
	}
	return states, nil
}

// GetState returns mining state for a session.
func (s *Store) GetState(ctx context.Context, sessionID string) (*MiningState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT session_id, agent, last_mined_offset, mined_at
		FROM memory_mining_state
		WHERE session_id = ?`, sessionID)

	var st MiningState
	if err := row.Scan(&st.SessionID, &st.Agent, &st.LastMinedOffset, &st.MinedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get mining state: %w", err)
	}
	return &st, nil
}

// UpsertState inserts or updates mining state.
func (s *Store) UpsertState(ctx context.Context, st *MiningState) error {
	if st == nil {
		return fmt.Errorf("mining state is nil")
	}
	if strings.TrimSpace(st.SessionID) == "" {
		return fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(st.Agent) == "" {
		return fmt.Errorf("agent is required")
	}
	if st.MinedAt.IsZero() {
		st.MinedAt = time.Now()
	}

	// Mining cursors are monotonic. A stale concurrent miner must not replace
	// the cursor or its associated agent/timestamp metadata.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_mining_state(session_id, agent, last_mined_offset, mined_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			agent = CASE
				WHEN excluded.last_mined_offset > memory_mining_state.last_mined_offset THEN excluded.agent
				ELSE memory_mining_state.agent
			END,
			last_mined_offset = MAX(memory_mining_state.last_mined_offset, excluded.last_mined_offset),
			mined_at = CASE
				WHEN excluded.last_mined_offset > memory_mining_state.last_mined_offset THEN excluded.mined_at
				ELSE memory_mining_state.mined_at
			END`,
		st.SessionID, st.Agent, st.LastMinedOffset, st.MinedAt)
	if err != nil {
		return fmt.Errorf("upsert mining state: %w", err)
	}
	return nil
}

// InsightMinedAt returns the time a session's insights were last extracted,
// or (time.Time{}, nil) if the session has never been insight-mined.
func (s *Store) InsightMinedAt(ctx context.Context, sessionID string) (time.Time, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT mined_at FROM memory_insight_mining_state WHERE session_id = ?`,
		sessionID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	t, _ := parseFlexibleTime(raw)
	return t, nil
}

// MarkInsightMined records that insight extraction has run for a session.
func (s *Store) MarkInsightMined(ctx context.Context, sessionID, agent string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_insight_mining_state(session_id, agent, mined_at)
		VALUES(?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			agent    = excluded.agent,
			mined_at = excluded.mined_at`,
		sessionID, agent, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// FragmentCountsByAgent returns count(fragment) grouped by agent.
func (s *Store) FragmentCountsByAgent(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent, COUNT(*)
		FROM memory_fragments
		GROUP BY agent`)
	if err != nil {
		return nil, fmt.Errorf("query fragment counts: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var agent string
		var count int
		if err := rows.Scan(&agent, &count); err != nil {
			return nil, fmt.Errorf("scan fragment count: %w", err)
		}
		counts[agent] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// LastMinedByAgent returns MAX(mined_at) grouped by agent.
//
// SQLite aggregate functions like MAX() return the underlying value as a plain
// string, bypassing the driver's column-type inference that makes direct column
// scans into time.Time work. We therefore scan as a string and parse manually,
// accepting both RFC3339 (current format written by UpsertState) and the legacy
// Go time.String() format that older rows may contain.
func (s *Store) LastMinedByAgent(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent, MAX(mined_at)
		FROM memory_mining_state
		GROUP BY agent`)
	if err != nil {
		return nil, fmt.Errorf("query last mined: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var agent string
		var minedAtStr string
		if err := rows.Scan(&agent, &minedAtStr); err != nil {
			return nil, fmt.Errorf("scan last mined: %w", err)
		}
		t, err := parseFlexibleTime(minedAtStr)
		if err != nil {
			// Unparseable timestamp: skip rather than hard-fail.
			continue
		}
		out[agent] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseFlexibleTime parses a time string in either RFC3339 (the current
// UpsertState format) or the legacy Go time.String() layout that older rows
// may contain (e.g. "2006-01-02 15:04:05.999999999 -0700 MST m=+0.000").
func parseFlexibleTime(s string) (time.Time, error) {
	// RFC3339 / RFC3339Nano — written by current UpsertState.
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Go time.String() layout, with or without the monotonic "m=+…" suffix.
	// Strip the suffix first so the layout is fixed-width.
	cleaned := s
	if idx := strings.Index(s, " m="); idx != -1 {
		cleaned = s[:idx]
	}
	const goTimeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"
	if t, err := time.Parse(goTimeLayout, cleaned); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time format: %q", s)
}

// Close closes the underlying DB.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func scanFragment(scanner interface{ Scan(dest ...any) error }) (*Fragment, error) {
	var f Fragment
	var accessedAt sql.NullTime
	err := scanner.Scan(
		&f.ID,
		&f.Agent,
		&f.Path,
		&f.Content,
		&f.Source,
		&f.CreatedAt,
		&f.UpdatedAt,
		&accessedAt,
		&f.AccessCount,
		&f.DecayScore,
		&f.Pinned,
	)
	if err != nil {
		return nil, err
	}
	if accessedAt.Valid {
		f.AccessedAt = &accessedAt.Time
	}
	return &f, nil
}

func getFragmentByAgentPathTx(ctx context.Context, tx *sql.Tx, agent, path string) (*Fragment, int64, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT rowid, id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE agent = ? AND path = ?`,
		agent, path)

	var rowID int64
	var f Fragment
	var accessedAt sql.NullTime
	err := row.Scan(
		&rowID,
		&f.ID,
		&f.Agent,
		&f.Path,
		&f.Content,
		&f.Source,
		&f.CreatedAt,
		&f.UpdatedAt,
		&accessedAt,
		&f.AccessCount,
		&f.DecayScore,
		&f.Pinned,
	)
	if err == sql.ErrNoRows {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get fragment: %w", err)
	}
	if accessedAt.Valid {
		f.AccessedAt = &accessedAt.Time
	}
	return &f, rowID, nil
}

func syncFTSInsert(ctx context.Context, tx *sql.Tx, rowID int64, f *Fragment) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_fts(rowid, id, agent, path, content) VALUES(?, ?, ?, ?, ?)`,
		rowID, f.ID, f.Agent, f.Path, f.Content)
	return err
}

func syncFTSDelete(ctx context.Context, tx *sql.Tx, rowID int64, f *Fragment) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_fts(memory_fts, rowid, id, agent, path, content) VALUES('delete', ?, ?, ?, ?, ?)`,
		rowID, f.ID, f.Agent, f.Path, f.Content)
	return err
}

func newID() string {
	now := time.Now().Format("20060102-150405")
	randBytes := make([]byte, 3)
	_, _ = rand.Read(randBytes)
	return fmt.Sprintf("mem-%s-%s", now, hex.EncodeToString(randBytes))
}

// ── Fragment sources (L1→L2 backpointers) ────────────────────────────────────

// FragmentSource records a link between a memory fragment (L1) and the raw
// session turn range (L2) that the fragment was mined from.  One fragment can
// accumulate many sources over time as the miner revisits sessions.
type FragmentSource struct {
	ID        int64
	Agent     string
	Path      string
	SessionID string
	TurnStart int
	TurnEnd   int
	CreatedAt time.Time
}

// AddFragmentSource records that the fragment at (agent, path) was derived from
// messages [turnStart, turnEnd) of sessionID.  Duplicate rows (same agent,
// path, session, and turn range) are silently ignored.
func (s *Store) AddFragmentSource(ctx context.Context, agent, path, sessionID string, turnStart, turnEnd int) error {
	agent = strings.TrimSpace(agent)
	path = strings.TrimSpace(path)
	sessionID = strings.TrimSpace(sessionID)
	if agent == "" || path == "" || sessionID == "" {
		return fmt.Errorf("agent, path, and session_id are required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_fragment_sources (agent, path, session_id, turn_start, turn_end)
		SELECT ?, ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM memory_fragment_sources
			WHERE agent = ? AND path = ? AND session_id = ? AND turn_start = ? AND turn_end = ?
		)`,
		agent, path, sessionID, turnStart, turnEnd,
		agent, path, sessionID, turnStart, turnEnd,
	)
	if err != nil {
		return fmt.Errorf("add fragment source: %w", err)
	}
	return nil
}

// GetFragmentSources returns all source records for a given (agent, path)
// fragment, ordered oldest first.
func (s *Store) GetFragmentSources(ctx context.Context, agent, path string) ([]FragmentSource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, path, session_id, turn_start, turn_end, created_at
		FROM memory_fragment_sources
		WHERE agent = ? AND path = ?
		ORDER BY created_at ASC, id ASC`, agent, path)
	if err != nil {
		return nil, fmt.Errorf("get fragment sources: %w", err)
	}
	defer rows.Close()
	return scanFragmentSources(rows)
}

// GetSourcesForSession returns all fragment sources that were derived from a
// given session, ordered by path.  Useful for auditing what a session produced.
func (s *Store) GetSourcesForSession(ctx context.Context, sessionID string) ([]FragmentSource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, path, session_id, turn_start, turn_end, created_at
		FROM memory_fragment_sources
		WHERE session_id = ?
		ORDER BY path ASC, turn_start ASC, created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get sources for session: %w", err)
	}
	defer rows.Close()
	return scanFragmentSources(rows)
}

// deleteFragmentSources removes all source records for (agent, path).
// Called internally by DeleteFragment / DeleteFragmentByRowID.
func (s *Store) deleteFragmentSources(ctx context.Context, tx *sql.Tx, agent, path string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM memory_fragment_sources WHERE agent = ? AND path = ?`, agent, path)
	return err
}

func scanFragmentSources(rows *sql.Rows) ([]FragmentSource, error) {
	var out []FragmentSource
	for rows.Next() {
		var fs FragmentSource
		if err := rows.Scan(&fs.ID, &fs.Agent, &fs.Path, &fs.SessionID,
			&fs.TurnStart, &fs.TurnEnd, &fs.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan fragment source: %w", err)
		}
		out = append(out, fs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ── Image tracking ───────────────────────────────────────────────────────────

// ImageRecord is a record of a generated image.
type ImageRecord struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent"`
	SessionID  string    `json:"session_id"`
	Prompt     string    `json:"prompt"`
	OutputPath string    `json:"output_path"`
	MimeType   string    `json:"mime_type"`
	Provider   string    `json:"provider"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	FileSize   int       `json:"file_size"`
	CreatedAt  time.Time `json:"created_at"`
}

// ImageListOptions controls listing of generated images.
type ImageListOptions struct {
	Agent  string
	Limit  int
	Offset int
}

// RecordImage inserts a generated image record into the store.
// Non-fatal in callers: errors do not stop image generation.
