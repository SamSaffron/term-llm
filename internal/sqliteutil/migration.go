package sqliteutil

import (
	"context"
	"database/sql"
	"fmt"
)

// Executor is the query surface exposed to a migration transaction.
type Executor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

type connExecutor struct {
	ctx  context.Context
	conn *sql.Conn
}

func (e connExecutor) Exec(query string, args ...any) (sql.Result, error) {
	return e.conn.ExecContext(e.ctx, query, args...)
}

func (e connExecutor) Query(query string, args ...any) (*sql.Rows, error) {
	return e.conn.QueryContext(e.ctx, query, args...)
}

// WithImmediateMigrationTx runs fn in a SQLite BEGIN IMMEDIATE transaction.
func WithImmediateMigrationTx(ctx context.Context, db *sql.DB, fn func(Executor) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate migration transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := fn(connExecutor{ctx: ctx, conn: conn}); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	committed = true
	return nil
}
