package session

import (
	"context"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session/authordb"
)

// Author is display-only metadata for a human sender.
type Author = authordb.Author

// Participants is the bounded ordered participant preview.
type Participants = authordb.Participants

// AuthorStore is the optional display-attribution persistence capability.
type AuthorStore interface {
	UpsertAuthor(context.Context, Author, bool) error
	HasAuthors(context.Context) (bool, error)
	GetAuthors(context.Context, []string) (map[string]Author, error)
	SessionParticipants(context.Context, []string) (map[string]Participants, error)
}

// AsAuthorStore discovers the optional directory capability through wrappers.
func AsAuthorStore(store Store) (AuthorStore, bool) {
	if logging, ok := store.(*LoggingStore); ok {
		return AsAuthorStore(logging.Store)
	}
	if s, ok := store.(AuthorStore); ok {
		return s, true
	}
	if s, ok := store.(interface{ Unwrap() Store }); ok {
		return AsAuthorStore(s.Unwrap())
	}
	return nil, false
}

func userAuthorID(role core.Role, id string) string {
	if role == core.RoleUser {
		return id
	}
	return ""
}
func (s *SQLiteStore) authorIDColumn() string {
	if s.hasAuthors {
		return "author_id"
	}
	return "''"
}
func (s *SQLiteStore) createdByColumn(alias string) string {
	if s.hasAuthors {
		return qualifiedMessageColumn(alias, "created_by")
	}
	return "''"
}
func (s *SQLiteStore) filterCreatedBy(query string, args []any, id string) (string, []any) {
	if id != "" {
		query += " AND " + s.createdByColumn("s") + " = ?"
		args = append(args, id)
	}
	return query, args
}

const authorSchemaV69 = authordb.SchemaV69

func migrateAuthorsV69(db schemaExecutor) error { return authordb.MigrateV69(db) }
func (s *SQLiteStore) UpsertAuthor(ctx context.Context, a Author, missing bool) error {
	return authordb.UpsertAuthor(ctx, s.db, a, missing)
}
func (s *SQLiteStore) GetAuthors(ctx context.Context, ids []string) (map[string]Author, error) {
	if !s.hasAuthors {
		return map[string]Author{}, nil
	}
	return authordb.GetAuthors(ctx, s.queryDB(), ids)
}
func (s *SQLiteStore) SessionParticipants(ctx context.Context, ids []string) (map[string]Participants, error) {
	if !s.hasAuthors {
		return map[string]Participants{}, nil
	}
	return authordb.SessionParticipants(ctx, s.queryDB(), ids)
}

// HasAuthors reports whether attribution has ever been stored in this database.
func (s *SQLiteStore) HasAuthors(ctx context.Context) (bool, error) {
	if !s.hasAuthors {
		return false, nil
	}
	var exists bool
	err := s.queryDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM authors)`).Scan(&exists)
	return exists, err
}
