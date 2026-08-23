package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/sqliteutil"

	_ "modernc.org/sqlite"
)

func TestMemoryMigrationListInvariants(t *testing.T) {
	list := make([]sqliteutil.Migration, len(memoryMigrations))
	for i, migration := range memoryMigrations {
		list[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(list, 1, memorySchemaVersion, true); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCurrentFastPathDoesNotAcquireWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	store, err := NewStore(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
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
	conn, err := db1.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	if err := initSchema(db2); err != nil {
		t.Fatalf("current fast path attempted a write lock: %v", err)
	}
}

func TestMemoryFreshAndCurrentPathsRunZeroMigrationCallbacks(t *testing.T) {
	original := memoryMigrations
	memoryMigrations = append([]memoryMigration(nil), original...)
	callbacks := 0
	for i := range memoryMigrations {
		up := memoryMigrations[i].up
		memoryMigrations[i].up = func(tx schemaExecutor) error {
			callbacks++
			return up(tx)
		}
	}
	defer func() { memoryMigrations = original }()
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

func TestMemoryMigrationRejectsFutureVersionBeforeWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := NewStore(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE memory_meta SET value = ? WHERE key = ?`, memorySchemaVersion+1, memorySchemaVersionKey); err != nil {
		t.Fatal(err)
	}
	store.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = initSchema(db)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("future version error = %v", err)
	}
	var version string
	if err := db.QueryRow(`SELECT value FROM memory_meta WHERE key = ?`, memorySchemaVersionKey).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != strconv.Itoa(memorySchemaVersion+1) {
		t.Fatalf("future marker changed to %q", version)
	}
}

func TestMemoryFTSInitializationIsAtomicAndRetryable(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_meta(key,value) VALUES(?,?)`, memorySchemaVersionKey, strconv.Itoa(memorySchemaVersion)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_fragments(id,agent,path,content,created_at,updated_at) VALUES('f','a','p','atomicneedle',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_fts_marker BEFORE INSERT ON memory_meta WHEN new.key='fts_initialized' BEGIN SELECT RAISE(ABORT,'injected marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := ensureFTSInitialized(db); err == nil {
		t.Fatal("FTS initialization unexpectedly succeeded")
	}
	var matches int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_fts WHERE memory_fts MATCH 'atomicneedle'`).Scan(&matches); err != nil {
		t.Fatal(err)
	}
	if matches != 0 {
		t.Fatalf("failed initialization left %d FTS matches", matches)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_fts_marker`); err != nil {
		t.Fatal(err)
	}
	if err := ensureFTSInitialized(db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_fts WHERE memory_fts MATCH 'atomicneedle'`).Scan(&matches); err != nil {
		t.Fatal(err)
	}
	if matches != 1 {
		t.Fatalf("retried initialization produced %d FTS matches", matches)
	}
}

func TestMemoryConcurrentBootstrapPublishesOneMarker(t *testing.T) {
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
	var rows int
	var version string
	if err := dbs[0].QueryRow(`SELECT COUNT(*),MAX(value) FROM memory_meta WHERE key=?`, memorySchemaVersionKey).Scan(&rows, &version); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || version != strconv.Itoa(memorySchemaVersion) {
		t.Fatalf("marker rows=%d version=%q", rows, version)
	}
}

func TestMemoryFreshBootstrapPublishesCurrentMarkerAndIndex(t *testing.T) {
	store, err := NewStore(Config{Path: filepath.Join(t.TempDir(), "fresh.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version string
	if err := store.db.QueryRow(`SELECT value FROM memory_meta WHERE key = ?`, memorySchemaVersionKey).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != strconv.Itoa(memorySchemaVersion) {
		t.Fatalf("schema version = %q", version)
	}
	var indexCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_memory_embeddings_vector_search_cover'`).Scan(&indexCount); err != nil || indexCount != 1 {
		t.Fatalf("vector index count=%d err=%v", indexCount, err)
	}
}
