package sqliteutil

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

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
