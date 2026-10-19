package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/sqliteutil"

	_ "modernc.org/sqlite"
)

func TestAgentRunMigrationFromV62PreservesRowsAndAddsNoDeadline(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	v62 := schema + projectsSchemaV47 + changeLogSchemaV52 + attentionSchemaV54 + rushSchemaV57 + modelUsageSchemaV58 + agentRunSchemaV62
	if _, err := db.Exec(v62); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (id INTEGER PRIMARY KEY CHECK (id=1),version INTEGER NOT NULL); INSERT INTO schema_version(id,version) VALUES(1,62); INSERT INTO session_agent_runs(child_session_id,parent_session_id,agent_name,prompt,run_status,owner_instance_id,updated_at) VALUES('child','parent','developer','work','interrupted','old-host:1:1',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	row, err := scanAgentRun(db.QueryRow(`SELECT ` + agentRunColumns + ` FROM session_agent_runs WHERE child_session_id='child'`))
	if err != nil || row.Status != "interrupted" || row.BaseDir != "" || row.NotifyWhenDone || len(row.Media) != 0 {
		t.Fatalf("migrated run=%+v %v", row, err)
	}
	// The v63 migration adds only cwd, delivery and media metadata. It must
	// never install an execution deadline or revive a child.
	columns, err := db.Query(`PRAGMA table_info(session_agent_runs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer columns.Close()
	for columns.Next() {
		var number int
		var name, kind string
		var notNull int
		var def sql.NullString
		var key int
		if err := columns.Scan(&number, &name, &kind, &notNull, &def, &key); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(name, "deadline") || strings.Contains(name, "execution_timeout") {
			t.Fatalf("unexpected child execution deadline: %s", name)
		}
	}
}

func TestSessionMigrationListInvariants(t *testing.T) {
	list := make([]sqliteutil.Migration, len(migrations))
	for i, migration := range migrations {
		list[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(list, 1, schemaVersion, true); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMigrationAddsIndexedChangeLog(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(schema + projectsSchemaV47); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL); INSERT INTO schema_version(id, version) VALUES(1, 51)`); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	var table string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'session_change_log'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id, provider, model) VALUES('external', 'test', 'model')`); err != nil {
		t.Fatal(err)
	}
	var kind, sessionID string
	if err := db.QueryRow(`SELECT kind, session_id FROM session_change_log ORDER BY sequence DESC LIMIT 1`).Scan(&kind, &sessionID); err != nil {
		t.Fatal(err)
	}
	if kind != StoreChangeSessionCreated || sessionID != "external" {
		t.Fatalf("change = %q/%q", kind, sessionID)
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

// Faithful v64 fresh schema: the frozen v54 attention table and all v64
// bootstrap indexes/triggers, including project ordering installed separately.
func openAttentionV64DB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(schema + projectsSchemaV47 + changeLogSchemaV52 + attentionSchemaV54 + rushSchemaV57 + modelUsageSchemaV58 + agentRunSchemaV63); err != nil {
		t.Fatal(err)
	}
	if err := ensureCurrentSessionIndexes(db); err != nil {
		t.Fatal(err)
	}
	if err := createMessageCountTriggersV27(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(pinOrderSchemaV60); err != nil {
		t.Fatal(err)
	}
	if err := migrateProjectOrderV61(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES(1,64);
		INSERT INTO sessions(id,provider,model) VALUES('old','test','test');
		INSERT INTO session_attention(session_id, latest_attention_seq, seen_through_seq, seen_at, updated_at) VALUES('old',17,17,123,123)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAttentionMigrationV65PreservesRowsAndMatchesFresh(t *testing.T) {
	db := openAttentionV64DB(t)
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	var latest, through, rev, seenAt int64
	var source sql.NullString
	if err := db.QueryRow(`SELECT latest_attention_seq, seen_through_seq, seen_rev, seen_source, seen_at FROM session_attention WHERE session_id='old'`).Scan(&latest, &through, &rev, &source, &seenAt); err != nil {
		t.Fatal(err)
	}
	if latest != 17 || through != 17 || rev != 0 || source.Valid || seenAt != 123 {
		t.Fatalf("migration lost data: %d/%d/%d/%v/%d", latest, through, rev, source, seenAt)
	}
	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := initSchema(fresh); err != nil {
		t.Fatal(err)
	}
	want, err := sqliteutil.SchemaSignature(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sqliteutil.SchemaSignature(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		for _, entry := range want {
			if !slices.Contains(got, entry) {
				t.Errorf("fresh only: %s", entry)
			}
		}
		for _, entry := range got {
			if !slices.Contains(want, entry) {
				t.Errorf("migrated only: %s", entry)
			}
		}
	}
}

func TestAttentionMigrationV65RollbackAndRetry(t *testing.T) {
	db := openAttentionV64DB(t)
	original := migrations
	defer func() { migrations = original }()
	migrations = append([]migration(nil), original...)
	last := 0
	for i := range migrations {
		if migrations[i].version == 65 {
			last = i
			break
		}
	}
	up := migrations[last].up
	migrations[last].up = func(db schemaExecutor) error {
		if err := up(db); err != nil {
			return err
		}
		return errors.New("injected v65 failure")
	}
	if err := initSchema(db); err == nil || !strings.Contains(err.Error(), "injected v65 failure") {
		t.Fatalf("failure=%v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 64 {
		t.Fatalf("marker=%d err=%v", version, err)
	}
	if exists, err := sqliteutil.ColumnExists(db, "session_attention", "seen_rev"); err != nil || exists {
		t.Fatalf("rolled-back column exists=%v err=%v", exists, err)
	}
	migrations = original
	if err := initSchema(db); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("retried marker=%d err=%v", version, err)
	}
}
