// Package authordb owns the additive display-attribution schema and batched directory queries.
package authordb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

// Author is display-only metadata for a human sender, never provider input.
type Author struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// Participants is a bounded first-message-ordered preview and the full count.
type Participants struct {
	IDs   []string
	Count int
}

const SchemaV69 = `
CREATE TABLE IF NOT EXISTS authors (
 id TEXT PRIMARY KEY,
 display_name TEXT NOT NULL,
 email TEXT NOT NULL DEFAULT '',
 updated_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_session_author ON messages(session_id, author_id) WHERE author_id <> '';
CREATE TRIGGER IF NOT EXISTS messages_created_by_insert AFTER INSERT ON messages
WHEN NEW.role = 'user' AND NEW.author_id <> '' AND COALESCE(NEW.compaction_tail, FALSE) = FALSE
BEGIN
 UPDATE sessions SET created_by = NEW.author_id
 WHERE id = NEW.session_id AND created_by = ''
 AND NOT EXISTS (SELECT 1 FROM messages WHERE session_id = NEW.session_id AND role = 'user' AND id <> NEW.id);
END;
`

func MigrateV69(db sqliteutil.Executor) error {
	for _, col := range []struct{ table, name string }{{"messages", "author_id"}, {"sessions", "created_by"}} {
		exists, err := sqliteutil.ColumnExists(db, col.table, col.name)
		if err != nil {
			return fmt.Errorf("inspect %s.%s: %w", col.table, col.name, err)
		}
		if !exists {
			if _, err := db.Exec("ALTER TABLE " + col.table + " ADD COLUMN " + col.name + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("add %s.%s: %w", col.table, col.name, err)
			}
		}
	}
	_, err := db.Exec(SchemaV69)
	if err != nil {
		return fmt.Errorf("create author directory: %w", err)
	}
	return nil
}

func UpsertAuthor(ctx context.Context, db *sql.DB, a Author, onlyMissing bool) error {
	if a.ID == "" {
		return nil
	}
	conflict := "DO UPDATE SET display_name=excluded.display_name, email=excluded.email, updated_at=excluded.updated_at"
	if onlyMissing {
		conflict = "DO NOTHING"
	}
	_, err := db.ExecContext(ctx, `INSERT INTO authors(id,display_name,email,updated_at) VALUES(?,?,?,?) ON CONFLICT(id) `+conflict, a.ID, a.Name, a.Email, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("upsert author: %w", err)
	}
	return nil
}

func authorQueryIDs(ids []string) (string, []any) {
	marks := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			marks = append(marks, "?")
			args = append(args, id)
		}
	}
	return strings.Join(marks, ","), args
}

func GetAuthors(ctx context.Context, db *sql.DB, ids []string) (map[string]Author, error) {
	out := map[string]Author{}
	marks, args := authorQueryIDs(ids)
	if marks == "" {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id,display_name,email FROM authors WHERE id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("get authors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a Author
		if err := rows.Scan(&a.ID, &a.Name, &a.Email); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

func SessionParticipants(ctx context.Context, db *sql.DB, ids []string) (map[string]Participants, error) {
	out := map[string]Participants{}
	marks, args := authorQueryIDs(ids)
	if marks == "" {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT session_id,author_id FROM messages WHERE session_id IN (`+marks+`) AND author_id <> '' AND role='user' GROUP BY session_id,author_id ORDER BY session_id,MIN(sequence),MIN(id)`, args...)
	if err != nil {
		return nil, fmt.Errorf("get participants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid, aid string
		if err := rows.Scan(&sid, &aid); err != nil {
			return nil, err
		}
		p := out[sid]
		p.Count++
		if len(p.IDs) < 5 {
			p.IDs = append(p.IDs, aid)
		}
		out[sid] = p
	}
	return out, rows.Err()
}
