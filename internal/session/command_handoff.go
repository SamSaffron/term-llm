package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// CommandHandoff carries mode-owned UI state across an explicit self-exec. It is
// private session metadata, never argv or a synthetic user message.
type CommandHandoff struct {
	ID             string          `json:"id"`
	Service        string          `json:"service"`
	SourceInstance string          `json:"source_instance"`
	SessionID      string          `json:"session_id,omitempty"`
	Revision       int64           `json:"revision"`
	Created        time.Time       `json:"created"`
	Payload        json.RawMessage `json:"payload"`
}

type CommandHandoffStore interface {
	SaveCommandHandoff(context.Context, CommandHandoff) error
	ConsumeCommandHandoff(context.Context, string, string, string) (CommandHandoff, error)
	ReclaimCommandHandoff(context.Context, string, string, string) (CommandHandoff, error)
	DiscardCommandHandoff(context.Context, string) error
}

func AsCommandHandoffStore(store Store) (CommandHandoffStore, bool) {
	if wrapped, ok := store.(*LoggingStore); ok {
		return AsCommandHandoffStore(wrapped.Store)
	}
	if s, ok := store.(*SQLiteStore); ok && (s.cfg.ReadOnly || s.cfg.Path == ":memory:") {
		return nil, false
	}
	result, ok := store.(CommandHandoffStore)
	return result, ok
}

func (s *SQLiteStore) SaveCommandHandoff(ctx context.Context, h CommandHandoff) error {
	if h.ID == "" || h.Service == "" || h.SourceInstance == "" || !json.Valid(h.Payload) {
		return ErrExecHandoffConflict
	}
	h.Created = time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE metadata SET value=value WHERE key='serve_response_fencing_token'`); err != nil {
		return err
	}
	if h.SessionID != "" {
		if err = tx.QueryRowContext(ctx, `SELECT transcript_rev FROM sessions WHERE id=?`, h.SessionID).Scan(&h.Revision); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?)`, "process_handoff:"+h.ID, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ConsumeCommandHandoff(ctx context.Context, id, service, instance string) (CommandHandoff, error) {
	return s.takeCommandHandoff(ctx, id, service, instance, false)
}

// ReclaimCommandHandoff is the explicit failed-replacement path for the original
// instance. It retains the same service, revision, expiry and one-shot checks;
// unlike replacement adoption, it requires identity equality, not a fake boot ID.
func (s *SQLiteStore) ReclaimCommandHandoff(ctx context.Context, id, service, sourceInstance string) (CommandHandoff, error) {
	return s.takeCommandHandoff(ctx, id, service, sourceInstance, true)
}

func (s *SQLiteStore) takeCommandHandoff(ctx context.Context, id, service, instance string, reclaim bool) (CommandHandoff, error) {
	var h CommandHandoff
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return h, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE metadata SET value=value WHERE key='serve_response_fencing_token'`); err != nil {
		return h, err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, "process_handoff:"+id).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return h, ErrExecHandoffConflict
	} else if err != nil {
		return h, err
	}
	if err = json.Unmarshal([]byte(raw), &h); err != nil {
		return h, err
	}
	if h.ID != id || h.Service != service || instance == "" || (h.SourceInstance == instance) != reclaim || time.Since(h.Created) > 5*time.Minute || h.Created.After(time.Now()) {
		return h, ErrExecHandoffConflict
	}
	if h.SessionID != "" {
		var rev int64
		if err = tx.QueryRowContext(ctx, `SELECT transcript_rev FROM sessions WHERE id=?`, h.SessionID).Scan(&rev); err != nil {
			return h, err
		}
		if rev != h.Revision {
			return h, ErrExecHandoffConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, "process_handoff:"+id); err != nil {
		return h, err
	}
	return h, tx.Commit()
}

func (s *SQLiteStore) DiscardCommandHandoff(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, "process_handoff:"+id)
	return err
}
