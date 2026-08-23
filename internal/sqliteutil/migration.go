package sqliteutil

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Executor is the context-bound query surface exposed to a migration transaction.
// Implementations must keep every operation on the same SQLite connection.
type Executor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
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

func (e connExecutor) QueryRow(query string, args ...any) *sql.Row {
	return e.conn.QueryRowContext(e.ctx, query, args...)
}

const rollbackTimeout = 5 * time.Second

// WithImmediateMigrationTx runs fn on a dedicated connection in a SQLite
// BEGIN IMMEDIATE transaction. SQLite's configured busy_timeout governs lock
// acquisition; this function does not add an unbounded retry loop.
//
// Cleanup uses a short context independent of ctx, so cancellation cannot skip
// rollback. If rollback cannot be confirmed, database/sql's Raw hook returns
// driver.ErrBadConn, which tells the pool to discard rather than reuse the
// connection.
func WithImmediateMigrationTx(ctx context.Context, db *sql.DB, fn func(Executor) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate migration transaction: retryable SQLite lock acquisition failed (configured busy_timeout may have expired): %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		if _, rollbackErr := conn.ExecContext(cleanupCtx, "ROLLBACK"); rollbackErr != nil {
			poisonErr := conn.Raw(func(any) error { return driver.ErrBadConn })
			rollbackErr = fmt.Errorf("rollback migration transaction: %w", rollbackErr)
			if poisonErr != nil && !errors.Is(poisonErr, driver.ErrBadConn) {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("discard failed migration connection: %w", poisonErr))
			}
			err = errors.Join(err, rollbackErr)
		}
	}()

	if callbackErr := fn(connExecutor{ctx: ctx, conn: conn}); callbackErr != nil {
		return callbackErr
	}
	if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr != nil {
		return fmt.Errorf("commit migration transaction: %w", commitErr)
	}
	committed = true
	return nil
}

// Migration is the store-neutral description of one forward schema migration.
type Migration struct {
	Version     int
	Description string
	Up          func(Executor) error
}

// ValidateMigrations validates a migration list. When contiguous is true, the
// first version must equal firstVersion and every following version must be the
// next integer. A non-empty list must end at targetVersion.
func ValidateMigrations(migrations []Migration, firstVersion, targetVersion int, contiguous bool) error {
	if targetVersion < 0 {
		return fmt.Errorf("target migration version must not be negative: %d", targetVersion)
	}
	if len(migrations) == 0 {
		if targetVersion == 0 {
			return nil
		}
		return fmt.Errorf("migration list is empty but target version is %d", targetVersion)
	}
	previous := 0
	for i, migration := range migrations {
		if migration.Version <= 0 {
			return fmt.Errorf("migration at index %d has non-positive version %d", i, migration.Version)
		}
		if migration.Description == "" {
			return fmt.Errorf("migration %d has an empty description", migration.Version)
		}
		if migration.Up == nil {
			return fmt.Errorf("migration %d (%s) has a nil callback", migration.Version, migration.Description)
		}
		if i > 0 && migration.Version <= previous {
			return fmt.Errorf("migration %d (%s) is not strictly after version %d", migration.Version, migration.Description, previous)
		}
		if contiguous {
			want := firstVersion + i
			if migration.Version != want {
				return fmt.Errorf("migration at index %d has version %d, want contiguous version %d", i, migration.Version, want)
			}
		}
		previous = migration.Version
	}
	if previous != targetVersion {
		return fmt.Errorf("final migration version is %d, want target %d", previous, targetVersion)
	}
	return nil
}

var sqliteIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func quoteIdentifier(identifier string) (string, error) {
	if !sqliteIdentifier.MatchString(identifier) {
		return "", fmt.Errorf("invalid SQLite identifier %q", identifier)
	}
	return `"` + identifier + `"`, nil
}

// UserTableCount returns the number of non-SQLite-owned tables. It is useful
// only while classifying an unversioned database on a locked slow path.
func UserTableCount(exec Executor) (int, error) {
	var count int
	err := exec.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&count)
	return count, err
}

// TableExists reports whether a table or virtual table exists.
func TableExists(exec Executor, table string) (bool, error) {
	if _, err := quoteIdentifier(table); err != nil {
		return false, err
	}
	var exists int
	err := exec.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, table).Scan(&exists)
	return exists != 0, err
}

// ColumnExists reports whether table contains column.
func ColumnExists(exec Executor, table, column string) (bool, error) {
	quotedTable, err := quoteIdentifier(table)
	if err != nil {
		return false, err
	}
	if _, err := quoteIdentifier(column); err != nil {
		return false, err
	}
	rows, err := exec.Query(`PRAGMA table_info(` + quotedTable + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// IndexExists reports whether a named index exists.
func IndexExists(exec Executor, index string) (bool, error) {
	if _, err := quoteIdentifier(index); err != nil {
		return false, err
	}
	var exists int
	err := exec.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)`, index).Scan(&exists)
	return exists != 0, err
}

// TriggerExists reports whether a named trigger exists.
func TriggerExists(exec Executor, trigger string) (bool, error) {
	if _, err := quoteIdentifier(trigger); err != nil {
		return false, err
	}
	var exists int
	err := exec.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'trigger' AND name = ?)`, trigger).Scan(&exists)
	return exists != 0, err
}
