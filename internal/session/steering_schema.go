package session

// Historical SQL is deliberately retained: deployed schemas must migrate forward.
const steeringMigrationV56 = `
DROP TABLE IF EXISTS session_pending_steering;
ALTER TABLE session_pending_interjections RENAME TO session_pending_steering;
DROP INDEX IF EXISTS idx_session_pending_interjections_order;
ALTER TABLE session_pending_steering ADD COLUMN acceptance_sequence INTEGER NOT NULL DEFAULT 0;
ALTER TABLE session_pending_steering ADD COLUMN origin TEXT NOT NULL DEFAULT 'legacy_unknown';
ALTER TABLE session_pending_steering ADD COLUMN owner_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE session_pending_steering ADD COLUMN owner_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_pending_steering ADD COLUMN owner_fence INTEGER NOT NULL DEFAULT 0;
UPDATE session_pending_steering AS p SET acceptance_sequence = (
 SELECT COUNT(*) FROM session_pending_steering q WHERE q.session_id = p.session_id
 AND (q.created_at < p.created_at OR (q.created_at = p.created_at AND q.id <= p.id))
);
CREATE UNIQUE INDEX idx_session_pending_steering_order ON session_pending_steering(session_id, acceptance_sequence);
CREATE TABLE IF NOT EXISTS session_steering_sequence (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 sequence INTEGER NOT NULL
);
INSERT INTO session_steering_sequence(session_id, sequence)
 SELECT session_id, MAX(acceptance_sequence) FROM session_pending_steering GROUP BY session_id
 ON CONFLICT(session_id) DO UPDATE SET sequence = MAX(sequence, excluded.sequence);
`

const rushSchemaV57 = `
CREATE TABLE IF NOT EXISTS session_rush_operations (
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 request_id TEXT NOT NULL,
 source_response_id TEXT NOT NULL,
 source_epoch INTEGER NOT NULL,
 status TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1,
 replacement_response_id TEXT,
 owner_fence INTEGER NOT NULL,
 reason TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
 PRIMARY KEY(session_id, request_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_session_rush_active ON session_rush_operations(session_id)
 WHERE status IN ('interrupting','waiting_for_settlement','starting');
CREATE UNIQUE INDEX IF NOT EXISTS idx_session_rush_replacement ON session_rush_operations(session_id, replacement_response_id)
 WHERE replacement_response_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS session_rush_entries (
 session_id TEXT NOT NULL,
 request_id TEXT NOT NULL,
 steering_id TEXT NOT NULL,
 acceptance_sequence INTEGER NOT NULL,
 payload TEXT NOT NULL,
 disposition TEXT NOT NULL DEFAULT 'claimed',
 PRIMARY KEY(session_id, request_id, steering_id),
 FOREIGN KEY(session_id, request_id) REFERENCES session_rush_operations(session_id, request_id) ON DELETE CASCADE
);
`
