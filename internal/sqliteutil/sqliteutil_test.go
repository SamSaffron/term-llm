package sqliteutil

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestWithImmediateMigrationTxCommitAndRollback(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migrations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := WithImmediateMigrationTx(ctx, db, func(exec Executor) error {
		_, err := exec.Exec("CREATE TABLE committed (id INTEGER PRIMARY KEY)")
		return err
	}); err != nil {
		t.Fatalf("commit migration: %v", err)
	}
	if _, err := db.Exec("INSERT INTO committed(id) VALUES (1)"); err != nil {
		t.Fatalf("committed table unavailable: %v", err)
	}

	wantErr := errors.New("stop migration")
	if err := WithImmediateMigrationTx(ctx, db, func(exec Executor) error {
		if _, err := exec.Exec("CREATE TABLE rolled_back (id INTEGER)"); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("rollback migration error = %v, want %v", err, wantErr)
	}
	if _, err := db.Exec("SELECT * FROM rolled_back"); err == nil {
		t.Fatal("rolled-back table still exists")
	}
	if err := WithImmediateMigrationTx(ctx, db, func(exec Executor) error {
		_, err := exec.Exec("CREATE TABLE reused_after_rollback (id INTEGER)")
		return err
	}); err != nil {
		t.Fatalf("reuse after rollback: %v", err)
	}
}

func TestWithImmediateMigrationTxCancellationRollsBack(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	wantErr := errors.New("callback stopped")
	err = WithImmediateMigrationTx(ctx, db, func(exec Executor) error {
		if _, err := exec.Exec("CREATE TABLE canceled (id INTEGER)"); err != nil {
			return err
		}
		cancel()
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want callback error", err)
	}
	if _, err := db.Exec("SELECT * FROM canceled"); err == nil {
		t.Fatal("canceled transaction was not rolled back")
	}
}

func TestWithImmediateMigrationTxBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	db1, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for _, db := range []*sql.DB{db1, db2} {
		if _, err := db.Exec(`PRAGMA busy_timeout = 10`); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db1.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = WithImmediateMigrationTx(ctx, db2, func(Executor) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "begin immediate migration transaction") {
		t.Fatalf("busy error = %v", err)
	}
}

func TestValidateMigrations(t *testing.T) {
	valid := []Migration{
		{Version: 1, Description: "one", Up: func(Executor) error { return nil }},
		{Version: 2, Description: "two", Up: func(Executor) error { return nil }},
	}
	if err := ValidateMigrations(valid, 1, 2, true); err != nil {
		t.Fatalf("valid migrations: %v", err)
	}
	cases := []struct {
		name       string
		migrations []Migration
		target     int
	}{
		{"duplicate", []Migration{valid[0], valid[0]}, 1},
		{"hole", []Migration{valid[0], {Version: 3, Description: "three", Up: valid[0].Up}}, 3},
		{"empty description", []Migration{{Version: 1, Up: valid[0].Up}}, 1},
		{"nil callback", []Migration{{Version: 1, Description: "one"}}, 1},
		{"wrong target", valid, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateMigrations(tc.migrations, 1, tc.target, true); err == nil {
				t.Fatal("validation unexpectedly succeeded")
			}
		})
	}
}

func TestSchemaPredicates(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "predicates.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = WithImmediateMigrationTx(context.Background(), db, func(exec Executor) error {
		if _, err := exec.Exec(`CREATE TABLE things (id INTEGER PRIMARY KEY, value TEXT)`); err != nil {
			return err
		}
		if _, err := exec.Exec(`CREATE INDEX idx_things_value ON things(value)`); err != nil {
			return err
		}
		if _, err := exec.Exec(`CREATE TRIGGER trg_things_insert AFTER INSERT ON things BEGIN UPDATE things SET value = NEW.value WHERE id = NEW.id; END`); err != nil {
			return err
		}
		checks := []struct {
			name string
			fn   func() (bool, error)
		}{
			{"table", func() (bool, error) { return TableExists(exec, "things") }},
			{"column", func() (bool, error) { return ColumnExists(exec, "things", "value") }},
			{"index", func() (bool, error) { return IndexExists(exec, "idx_things_value") }},
			{"trigger", func() (bool, error) { return TriggerExists(exec, "trg_things_insert") }},
		}
		for _, check := range checks {
			exists, err := check.fn()
			if err != nil || !exists {
				return errors.New(check.name + " predicate failed")
			}
		}
		if count, err := UserTableCount(exec); err != nil || count != 1 {
			return errors.New("user table count failed")
		}
		if _, err := ColumnExists(exec, "things; DROP TABLE things", "value"); err == nil {
			return errors.New("unsafe identifier accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeSchemaSQLPreservesCommasInStringDefaults(t *testing.T) {
	got := normalizeSchemaSQL(`CREATE TABLE example (z TEXT DEFAULT 'a,b', a INTEGER)`)
	want := `CREATE TABLE example(a INTEGER,z TEXT DEFAULT 'a,b')`
	if got != want {
		t.Fatalf("normalized SQL = %q, want %q", got, want)
	}
}

func TestResolveDBPathOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DB_DIR", "nested")
	defaultPath := func() (string, error) { return "default.db", nil }
	if got, err := ResolveDBPathOverride("", defaultPath); err != nil || got != "default.db" {
		t.Fatalf("empty override = %q, %v", got, err)
	}
	if got, err := ResolveDBPathOverride(":memory:", defaultPath); err != nil || got != ":memory:" {
		t.Fatalf("memory override = %q, %v", got, err)
	}
	got, err := ResolveDBPathOverride("~/$DB_DIR/store.db", defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "nested", "store.db")
	if got != want {
		t.Fatalf("expanded override = %q, want %q", got, want)
	}
}
