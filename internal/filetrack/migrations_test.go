package filetrack

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/sqliteutil"

	_ "modernc.org/sqlite"
)

func TestFiletrackMigrationListInvariants(t *testing.T) {
	list := make([]sqliteutil.Migration, len(filetrackMigrations))
	for i, migration := range filetrackMigrations {
		list[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(list, 2, schemaVersion, true); err != nil {
		t.Fatal(err)
	}
}

func TestFiletrackFreshAndCurrentPathsRunZeroMigrationCallbacks(t *testing.T) {
	original := filetrackMigrations
	filetrackMigrations = append([]filetrackMigration(nil), original...)
	callbacks := 0
	for i := range filetrackMigrations {
		up := filetrackMigrations[i].up
		filetrackMigrations[i].up = func(tx sqliteutil.Executor) error {
			callbacks++
			return up(tx)
		}
	}
	defer func() { filetrackMigrations = original }()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fast.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 {
		t.Fatalf("fresh/current paths ran %d migration callbacks", callbacks)
	}
}

func TestFiletrackMigrationRejectsFutureVersionBeforeWrites(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_version SET version = ?`, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	err = initSchema(db)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("future version error = %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion+1 {
		t.Fatalf("future marker=%d err=%v", version, err)
	}
}

func TestFiletrackConcurrentBootstrapPublishesOneMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	dbs := make([]*sql.DB, 2)
	for i := range dbs {
		var err error
		dbs[i], err = sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer dbs[i].Close()
		if _, err := dbs[i].Exec(`PRAGMA busy_timeout=5000`); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	errs := make(chan error, len(dbs))
	var ready sync.WaitGroup
	ready.Add(len(dbs))
	for _, db := range dbs {
		go func(db *sql.DB) {
			ready.Done()
			<-start
			errs <- initSchema(db)
		}(db)
	}
	ready.Wait()
	close(start)
	for range dbs {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var rows, version int
	if err := dbs[0].QueryRow(`SELECT COUNT(*),MAX(version) FROM schema_version`).Scan(&rows, &version); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || version != schemaVersion {
		t.Fatalf("marker rows=%d version=%d", rows, version)
	}
}

func TestFiletrackMigrationNormalizesDuplicateMarkerAndPreservesData(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := `
	CREATE TABLE schema_version(version INTEGER NOT NULL);
	INSERT INTO schema_version VALUES(1),(1);
	CREATE TABLE blobs(hash TEXT PRIMARY KEY,size INTEGER NOT NULL,compression TEXT NOT NULL DEFAULT 'gzip',data BLOB NOT NULL,created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP);
	CREATE TABLE file_changes(
		id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, seq INTEGER NOT NULL,
		path TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('create','modify','delete')),
		tool_name TEXT, tool_call_id TEXT, before_hash TEXT, after_hash TEXT,
		before_size INTEGER NOT NULL DEFAULT 0, after_size INTEGER NOT NULL DEFAULT 0,
		adds INTEGER NOT NULL DEFAULT 0, dels INTEGER NOT NULL DEFAULT 0,
		truncated INTEGER NOT NULL DEFAULT 0, is_binary INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(session_id,seq)
	);
	INSERT INTO file_changes(session_id,seq,path,kind) VALUES('s',1,'a.txt','create');`
	if _, err := db.Exec(seed); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	var rows, version int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_version`).Scan(&rows, &version); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || version != schemaVersion {
		t.Fatalf("marker rows=%d version=%d", rows, version)
	}
	var path string
	var runID sql.NullString
	if err := db.QueryRow(`SELECT path,run_id FROM file_changes WHERE session_id='s'`).Scan(&path, &runID); err != nil {
		t.Fatal(err)
	}
	if path != "a.txt" || runID.Valid {
		t.Fatalf("preserved row path=%q run_id=%v", path, runID)
	}

	fresh, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := initSchema(fresh); err != nil {
		t.Fatal(err)
	}
	freshSignature, err := sqliteutil.SchemaSignature(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	migratedSignature, err := sqliteutil.SchemaSignature(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(freshSignature, migratedSignature) {
		t.Fatalf("fresh and v1-migrated file history schemas differ")
	}
}
