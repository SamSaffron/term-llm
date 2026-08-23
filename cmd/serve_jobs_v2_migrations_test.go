package cmd

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

func TestJobsV2MigrationListInvariants(t *testing.T) {
	list := make([]sqliteutil.Migration, len(jobsV2Migrations))
	for i, migration := range jobsV2Migrations {
		list[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(list, 1, jobsV2SchemaVersion, true); err != nil {
		t.Fatal(err)
	}
}

func TestJobsV2FreshAndCurrentPathsRunZeroMigrationCallbacks(t *testing.T) {
	original := jobsV2Migrations
	jobsV2Migrations = append([]jobsV2Migration(nil), original...)
	callbacks := 0
	for i := range jobsV2Migrations {
		up := jobsV2Migrations[i].up
		jobsV2Migrations[i].up = func(tx sqliteutil.Executor) error {
			callbacks++
			return up(tx)
		}
	}
	defer func() { jobsV2Migrations = original }()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fast.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initJobsV2Schema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := initJobsV2Schema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 {
		t.Fatalf("fresh/current paths ran %d migration callbacks", callbacks)
	}
}

func TestJobsV2MigrationRejectsFutureVersionBeforeWrites(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := execJobsV2Schema(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE jobs_v2_schema_version SET version = ?`, jobsV2SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := initJobsV2Schema(context.Background(), db); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("future version error = %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM jobs_v2_schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != jobsV2SchemaVersion+1 {
		t.Fatalf("future marker changed to %d", version)
	}
}

func TestJobsV2MigrationFailureRollsBackSchemaAndMarker(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := execJobsV2Schema(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE jobs_v2_schema_version`); err != nil {
		t.Fatal(err)
	}
	original := jobsV2Migrations
	jobsV2Migrations = append([]jobsV2Migration(nil), original...)
	originalUp := jobsV2Migrations[0].up
	jobsV2Migrations[0].up = func(tx sqliteutil.Executor) error {
		if err := originalUp(tx); err != nil {
			return err
		}
		if _, err := tx.Exec(`CREATE TABLE partial_jobs_migration(id INTEGER)`); err != nil {
			return err
		}
		return errors.New("injected jobs migration failure")
	}
	defer func() { jobsV2Migrations = original }()
	if err := initJobsV2Schema(context.Background(), db); err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	var partial, marker int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='partial_jobs_migration'`).Scan(&partial); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='jobs_v2_schema_version'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if partial != 0 || marker != 0 {
		t.Fatalf("rollback left partial table=%d marker table=%d", partial, marker)
	}
}

func TestJobsV2ConcurrentBootstrapPublishesOneMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-jobs.db")
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
			errs <- initJobsV2Schema(context.Background(), db)
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
	if err := dbs[0].QueryRow(`SELECT COUNT(*),MAX(version) FROM jobs_v2_schema_version`).Scan(&rows, &version); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || version != jobsV2SchemaVersion {
		t.Fatalf("marker rows=%d version=%d", rows, version)
	}
}

func TestJobsV2MigrationRepairsEveryIndividualLegacyColumnHole(t *testing.T) {
	for _, column := range jobsV2LegacyColumns {
		t.Run(column.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hole.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := execJobsV2Schema(db); err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{
				`DROP TABLE jobs_v2_schema_version`,
				`DROP INDEX IF EXISTS idx_job_runs_v2_summary_by_job_created`,
				`DROP INDEX IF EXISTS idx_job_runs_v2_summary_created`,
				`ALTER TABLE job_runs_v2 DROP COLUMN "` + column.name + `"`,
			} {
				if _, err := db.Exec(statement); err != nil {
					t.Fatalf("prepare synthetic %s hole: %v", column.name, err)
				}
			}
			if err := initJobsV2Schema(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			var found int
			rows, err := db.Query(`PRAGMA table_info(job_runs_v2)`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var cid, notNull, pk int
				var name, typ string
				var defaultValue any
				if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
					t.Fatal(err)
				}
				if name == column.name {
					found++
				}
			}
			rows.Close()
			if found != 1 {
				t.Fatalf("reconciled column %s count=%d", column.name, found)
			}
		})
	}
}

func TestJobsV2MigrationRepairsNonPrefixLegacyColumnHoles(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy-jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	legacy := []string{
		jobsV2BootstrapStatements[0],
		`CREATE TABLE job_runs_v2 (
			id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs_v2(id) ON DELETE CASCADE,
			attempt INTEGER NOT NULL, trigger TEXT NOT NULL, scheduled_for TIMESTAMP NOT NULL,
			status TEXT NOT NULL, worker_id TEXT, session_id TEXT, started_at TIMESTAMP,
			finished_at TIMESTAMP, exit_code INTEGER, error TEXT, stdout TEXT, stderr TEXT,
			thinking TEXT, response TEXT, input_tokens INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		jobsV2BootstrapStatements[4],
		`CREATE INDEX idx_job_runs_v2_job_id ON job_runs_v2(job_id, created_at DESC)`,
		`CREATE INDEX idx_job_run_events_v2_run_id ON job_run_events_v2(run_id, created_at, id)`,
	}
	for _, statement := range legacy {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed legacy jobs schema: %v\n%s", err, statement)
		}
	}
	if _, err := db.Exec(`INSERT INTO jobs_v2(id,name,runner_type,runner_config,trigger_type,trigger_config) VALUES('j','job','program','{}','manual','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO job_runs_v2(id,job_id,attempt,trigger,scheduled_for,status,session_id,input_tokens) VALUES('r','j',1,'manual',CURRENT_TIMESTAMP,'succeeded','session',7)`); err != nil {
		t.Fatal(err)
	}
	if err := initJobsV2Schema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, column := range jobsV2LegacyColumns {
		rows, err := db.Query(`PRAGMA table_info(job_runs_v2)`)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			found = found || name == column.name
		}
		rows.Close()
		if !found {
			t.Fatalf("missing reconciled column %s", column.name)
		}
	}
	var sessionID string
	var inputTokens, truncated, turnCount, outputTokens int
	if err := db.QueryRow(`SELECT session_id,input_tokens,truncated,turn_count,output_tokens FROM job_runs_v2 WHERE id='r'`).Scan(&sessionID, &inputTokens, &truncated, &turnCount, &outputTokens); err != nil {
		t.Fatal(err)
	}
	if sessionID != "session" || inputTokens != 7 || truncated != 0 || turnCount != 0 || outputTokens != 0 {
		t.Fatalf("legacy values/defaults = %q,%d,%d,%d,%d", sessionID, inputTokens, truncated, turnCount, outputTokens)
	}
	for _, index := range []string{jobsV2RunSummaryIndexName, jobsV2RunGlobalSummaryIndexName, "idx_job_run_events_v2_run_id_id"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&count); err != nil || count != 1 {
			t.Fatalf("current index %s count=%d err=%v", index, count, err)
		}
	}

	fresh, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fresh-jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := execJobsV2Schema(fresh); err != nil {
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
		t.Fatalf("fresh and legacy-migrated jobs schemas differ\n--- fresh ---\n%s\n--- migrated ---\n%s", strings.Join(freshSignature, "\n"), strings.Join(migratedSignature, "\n"))
	}
}
