package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/sqliteutil"

	_ "modernc.org/sqlite"
)

func TestSessionMigrationListInvariants(t *testing.T) {
	list := make([]sqliteutil.Migration, len(migrations))
	for i, migration := range migrations {
		list[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(list, 1, schemaVersion, true); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMigrationFailureResumesFromPriorCommittedVersion(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(canonicalSessionSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES(0)`); err != nil {
		t.Fatal(err)
	}
	original := migrations
	defer func() { migrations = original }()
	migrations = append([]migration(nil), original...)
	migrations[1].up = func(schemaExecutor) error { return errors.New("injected migration 2 failure") }
	if err := initSchema(db); err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("marker after migration 2 failure = %d, want committed version 1", version)
	}
	migrations = original
	if err := initSchema(db); err != nil {
		t.Fatalf("retry from version 1: %v", err)
	}
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("final marker=%d err=%v", version, err)
	}
}

func TestSessionMigrationRejectsFutureVersionBeforeWrites(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
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

func TestSessionMigrationRejectsUnknownUnversionedSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE unrelated(id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err == nil || !strings.Contains(err.Error(), "unknown unversioned") {
		t.Fatalf("unknown schema error = %v", err)
	}
	var sessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='sessions'`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("unknown schema was modified: sessions=%d err=%v", sessions, err)
	}
}

func TestSessionMigrationNormalizesDuplicateIdenticalLegacyMarkers(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES(48),(48)`); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	var rows, version int
	if err := db.QueryRow(`SELECT COUNT(*),MAX(version) FROM schema_version`).Scan(&rows, &version); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || version != schemaVersion {
		t.Fatalf("marker rows=%d version=%d", rows, version)
	}
}

func TestSessionMigrationRejectsConflictingLegacyMarkers(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES(48),(49)`); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting marker error = %v", err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("marker rows=%d err=%v", rows, err)
	}
}

func TestSessionOldestSupportedUnversionedFixtureMigratesWithData(t *testing.T) {
	migrated, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	migrated.SetMaxOpenConns(1)
	defer migrated.Close()
	fixture := `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY, name TEXT, summary TEXT, provider TEXT NOT NULL, model TEXT NOT NULL,
		cwd TEXT, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, archived BOOLEAN DEFAULT FALSE,
		parent_id TEXT REFERENCES sessions(id)
	);
	CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		role TEXT NOT NULL CHECK(role IN ('user','assistant','system','tool')),
		parts TEXT NOT NULL, text_content TEXT, duration_ms INTEGER,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, sequence INTEGER NOT NULL
	);
	CREATE INDEX idx_sessions_updated_at ON sessions(updated_at DESC);
	CREATE INDEX idx_messages_session_id ON messages(session_id,sequence);
	CREATE VIRTUAL TABLE messages_fts USING fts5(text_content,content='messages',content_rowid='id');
	CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN INSERT INTO messages_fts(rowid,text_content) VALUES(new.id,new.text_content); END;
	CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN INSERT INTO messages_fts(messages_fts,rowid,text_content) VALUES('delete',old.id,old.text_content); END;
	CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN INSERT INTO messages_fts(messages_fts,rowid,text_content) VALUES('delete',old.id,old.text_content); INSERT INTO messages_fts(rowid,text_content) VALUES(new.id,new.text_content); END;
	INSERT INTO sessions(id,name,provider,model) VALUES('s','legacy','p','m');
	INSERT INTO messages(session_id,role,parts,text_content,sequence) VALUES('s','user','[]','hello',0),('s','assistant','[]','world',1);`
	if _, err := migrated.Exec(fixture); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(migrated); err != nil {
		t.Fatal(err)
	}
	var messageCount, persistedCount int
	if err := migrated.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id='s'`).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if err := migrated.QueryRow(`SELECT message_count FROM sessions WHERE id='s'`).Scan(&persistedCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 2 || persistedCount != 2 {
		t.Fatalf("legacy message rows=%d persisted count=%d", messageCount, persistedCount)
	}

	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	fresh.SetMaxOpenConns(1)
	defer fresh.Close()
	if err := initSchema(fresh); err != nil {
		t.Fatal(err)
	}
	freshSignature, err := sqliteutil.SchemaSignature(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	migratedSignature, err := sqliteutil.SchemaSignature(context.Background(), migrated)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(freshSignature, migratedSignature) {
		t.Fatalf("fresh and oldest supported unversioned session schemas differ")
	}
}

func TestSessionFreshAndVersion46MigratedSchemasEquivalent(t *testing.T) {
	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	fresh.SetMaxOpenConns(1)
	defer fresh.Close()
	if err := initSchema(fresh); err != nil {
		t.Fatal(err)
	}
	migrated := openProjectMigration46DB(t)
	if err := initSchema(migrated); err != nil {
		t.Fatal(err)
	}
	freshSignature, err := sqliteutil.SchemaSignature(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	migratedSignature, err := sqliteutil.SchemaSignature(context.Background(), migrated)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(freshSignature, migratedSignature) {
		t.Fatalf("fresh and version-46-migrated session schemas differ\n--- fresh ---\n%s\n--- migrated ---\n%s", strings.Join(freshSignature, "\n"), strings.Join(migratedSignature, "\n"))
	}
}

func TestSessionConcurrentBootstrapPublishesOneMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
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
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
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

func TestSessionFreshMarkerIsStructurallySingleton(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version(id,version) VALUES(2,?)`, schemaVersion); err == nil {
		t.Fatal("singleton marker accepted a second row")
	}
}
