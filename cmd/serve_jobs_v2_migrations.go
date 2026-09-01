package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

const jobsV2SchemaVersion = 2

const jobsV2MarkerSchema = `CREATE TABLE jobs_v2_schema_version (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	version INTEGER NOT NULL
)`

var jobsV2BootstrapStatements = []string{
	`CREATE TABLE jobs_v2 (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		enabled INTEGER NOT NULL DEFAULT 1,
		runner_type TEXT NOT NULL,
		runner_config TEXT NOT NULL,
		trigger_type TEXT NOT NULL,
		trigger_config TEXT NOT NULL,
		schedule_timezone TEXT,
		concurrency_policy TEXT NOT NULL DEFAULT 'forbid',
		max_concurrent_runs INTEGER NOT NULL DEFAULT 1,
		retry_policy TEXT,
		timeout_seconds INTEGER NOT NULL DEFAULT 300,
		misfire_policy TEXT NOT NULL DEFAULT 'skip',
		labels TEXT,
		next_run_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE job_runs_v2 (
		id TEXT PRIMARY KEY,
		job_id TEXT NOT NULL REFERENCES jobs_v2(id) ON DELETE CASCADE,
		attempt INTEGER NOT NULL,
		trigger TEXT NOT NULL,
		scheduled_for TIMESTAMP NOT NULL,
		status TEXT NOT NULL,
		worker_id TEXT,
		session_id TEXT,
		started_at TIMESTAMP,
		finished_at TIMESTAMP,
		exit_code INTEGER,
		error TEXT,
		stdout TEXT,
		stderr TEXT,
		thinking TEXT,
		response TEXT,
		exit_reason TEXT,
		truncated INTEGER NOT NULL DEFAULT 0,
		turn_count INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX idx_jobs_v2_next_run_at ON jobs_v2(next_run_at)`,
	`CREATE INDEX idx_job_runs_v2_status ON job_runs_v2(status, scheduled_for)`,
	`CREATE TABLE job_run_events_v2 (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL REFERENCES job_runs_v2(id) ON DELETE CASCADE,
		event_type TEXT NOT NULL,
		message TEXT,
		data TEXT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX idx_job_run_events_v2_run_id_id ON job_run_events_v2(run_id, id)`,
	jobsV2RunSummaryIndexSQL,
	jobsV2RunGlobalSummaryIndexSQL,
	jobsV2RunByIDSummaryIndexSQL,
	jobsV2MarkerSchema,
}

type jobsV2Migration struct {
	version     int
	description string
	up          func(sqliteutil.Executor) error
}

var jobsV2Migrations = []jobsV2Migration{
	{
		version:     1,
		description: "adopt legacy jobs schema and reconcile run metadata and indexes",
		up:          reconcileLegacyJobsV2Schema,
	},
	{
		version:     2,
		description: "cover run summaries looked up by id",
		up:          createJobsV2RunByIDSummaryIndex,
	},
}

func createJobsV2RunByIDSummaryIndex(tx sqliteutil.Executor) error {
	if _, err := tx.Exec(jobsV2RunByIDSummaryIndexSQL); err != nil {
		return fmt.Errorf("create by-id run summary covering index: %w", err)
	}
	return nil
}

var jobsV2LegacyColumns = []struct {
	name       string
	definition string
	typeName   string
	notNull    bool
	defaultSQL string
}{
	{name: "exit_reason", definition: "TEXT", typeName: "TEXT"},
	{name: "truncated", definition: "INTEGER NOT NULL DEFAULT 0", typeName: "INTEGER", notNull: true, defaultSQL: "0"},
	{name: "turn_count", definition: "INTEGER NOT NULL DEFAULT 0", typeName: "INTEGER", notNull: true, defaultSQL: "0"},
	{name: "input_tokens", definition: "INTEGER NOT NULL DEFAULT 0", typeName: "INTEGER", notNull: true, defaultSQL: "0"},
	{name: "output_tokens", definition: "INTEGER NOT NULL DEFAULT 0", typeName: "INTEGER", notNull: true, defaultSQL: "0"},
	{name: "session_id", definition: "TEXT", typeName: "TEXT"},
}

func reconcileLegacyJobsV2Schema(tx sqliteutil.Executor) error {
	for _, column := range jobsV2LegacyColumns {
		exists, err := sqliteutil.ColumnExists(tx, "job_runs_v2", column.name)
		if err != nil {
			return fmt.Errorf("inspect legacy job_runs_v2.%s: %w", column.name, err)
		}
		if exists {
			if err := validateJobsV2LegacyColumn(tx, column.name, column.typeName, column.notNull, column.defaultSQL); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE job_runs_v2 ADD COLUMN "` + column.name + `" ` + column.definition); err != nil {
			return fmt.Errorf("add legacy job_runs_v2.%s: %w", column.name, err)
		}
	}
	if err := canonicalizeLegacyJobsV2Tables(tx); err != nil {
		return err
	}
	operations := []struct {
		description string
		statement   string
	}{
		{"create jobs schedule index", `CREATE INDEX IF NOT EXISTS idx_jobs_v2_next_run_at ON jobs_v2(next_run_at)`},
		{"create job-scoped run summary covering index", jobsV2RunSummaryIndexSQL},
		{"create global run summary covering index", jobsV2RunGlobalSummaryIndexSQL},
		{"remove obsolete job run index", `DROP INDEX IF EXISTS idx_job_runs_v2_job_id`},
		{"create current job event index", `CREATE INDEX IF NOT EXISTS idx_job_run_events_v2_run_id_id ON job_run_events_v2(run_id, id)`},
		{"remove obsolete job event index", `DROP INDEX IF EXISTS idx_job_run_events_v2_run_id`},
	}
	for _, operation := range operations {
		if _, err := tx.Exec(operation.statement); err != nil {
			return fmt.Errorf("%s: %w", operation.description, err)
		}
	}
	return nil
}

func validateJobsV2LegacyColumn(tx sqliteutil.Executor, column, wantType string, wantNotNull bool, wantDefault string) error {
	rows, err := tx.Query(`PRAGMA table_info("job_runs_v2")`)
	if err != nil {
		return fmt.Errorf("inspect legacy job_runs_v2.%s definition: %w", column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typeName string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name != column {
			continue
		}
		gotDefault := ""
		if defaultValue.Valid {
			gotDefault = defaultValue.String
		}
		if !strings.EqualFold(typeName, wantType) || (notNull != 0) != wantNotNull || gotDefault != wantDefault {
			return fmt.Errorf("contradictory legacy jobs column job_runs_v2.%s: type=%s not_null=%t default=%q, want type=%s not_null=%t default=%q", column, typeName, notNull != 0, gotDefault, wantType, wantNotNull, wantDefault)
		}
		return nil
	}
	return rows.Err()
}

func canonicalizeLegacyJobsV2Tables(tx sqliteutil.Executor) error {
	statements := []string{
		`DROP INDEX IF EXISTS idx_job_runs_v2_job_id`,
		`DROP INDEX IF EXISTS idx_job_runs_v2_status`,
		`DROP INDEX IF EXISTS idx_job_runs_v2_summary_by_job_created`,
		`DROP INDEX IF EXISTS idx_job_runs_v2_summary_created`,
		`DROP INDEX IF EXISTS idx_job_run_events_v2_run_id`,
		`DROP INDEX IF EXISTS idx_job_run_events_v2_run_id_id`,
		`ALTER TABLE job_run_events_v2 RENAME TO job_run_events_v2_old`,
		`ALTER TABLE job_runs_v2 RENAME TO job_runs_v2_old`,
		`CREATE TABLE job_runs_v2 (
			id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL REFERENCES jobs_v2(id) ON DELETE CASCADE,
			attempt INTEGER NOT NULL,
			trigger TEXT NOT NULL,
			scheduled_for TIMESTAMP NOT NULL,
			status TEXT NOT NULL,
			worker_id TEXT,
			session_id TEXT,
			started_at TIMESTAMP,
			finished_at TIMESTAMP,
			exit_code INTEGER,
			error TEXT,
			stdout TEXT,
			stderr TEXT,
			thinking TEXT,
			response TEXT,
			exit_reason TEXT,
			truncated INTEGER NOT NULL DEFAULT 0,
			turn_count INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO job_runs_v2(id,job_id,attempt,trigger,scheduled_for,status,worker_id,session_id,started_at,finished_at,exit_code,error,stdout,stderr,thinking,response,exit_reason,truncated,turn_count,input_tokens,output_tokens,created_at,updated_at)
		 SELECT id,job_id,attempt,trigger,scheduled_for,status,worker_id,session_id,started_at,finished_at,exit_code,error,stdout,stderr,thinking,response,exit_reason,truncated,turn_count,input_tokens,output_tokens,created_at,updated_at FROM job_runs_v2_old`,
		`CREATE TABLE job_run_events_v2 (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL REFERENCES job_runs_v2(id) ON DELETE CASCADE,
			event_type TEXT NOT NULL,
			message TEXT,
			data TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO job_run_events_v2(id,run_id,event_type,message,data,created_at)
		 SELECT id,run_id,event_type,message,data,created_at FROM job_run_events_v2_old`,
		`DROP TABLE job_run_events_v2_old`,
		`DROP TABLE job_runs_v2_old`,
		`CREATE INDEX idx_job_runs_v2_status ON job_runs_v2(status, scheduled_for)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("canonicalize legacy jobs schema with %q: %w", statement, err)
		}
	}
	return nil
}

type jobsV2MarkerReader interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readJobsV2Version(db jobsV2MarkerReader) (int, error) {
	var rows, distinct, minVersion, maxVersion int
	err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0) FROM jobs_v2_schema_version`).Scan(
		&rows, &distinct, &minVersion, &maxVersion,
	)
	if err != nil {
		return 0, err
	}
	if rows != 1 || distinct != 1 || minVersion != maxVersion {
		return 0, fmt.Errorf("invalid jobs schema marker: rows=%d distinct_versions=%d range=%d..%d", rows, distinct, minVersion, maxVersion)
	}
	return maxVersion, nil
}

func initJobsV2Schema(ctx context.Context, db *sql.DB) error {
	version, err := readJobsV2Version(db)
	if err == nil {
		if version == jobsV2SchemaVersion {
			return nil
		}
		if version > jobsV2SchemaVersion {
			return fmt.Errorf("jobs database schema version %d is newer than supported version %d", version, jobsV2SchemaVersion)
		}
	}

	shared := make([]sqliteutil.Migration, len(jobsV2Migrations))
	for i, migration := range jobsV2Migrations {
		shared[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(shared, 1, jobsV2SchemaVersion, true); err != nil {
		return fmt.Errorf("validate jobs migrations: %w", err)
	}

	return sqliteutil.WithImmediateMigrationTx(ctx, db, func(tx sqliteutil.Executor) error {
		markerExists, err := sqliteutil.TableExists(tx, "jobs_v2_schema_version")
		if err != nil {
			return fmt.Errorf("inspect jobs schema marker: %w", err)
		}
		tableNames := []string{"jobs_v2", "job_runs_v2", "job_run_events_v2"}
		existing := 0
		for _, table := range tableNames {
			exists, err := sqliteutil.TableExists(tx, table)
			if err != nil {
				return fmt.Errorf("inspect jobs table %s: %w", table, err)
			}
			if exists {
				existing++
			}
		}

		if !markerExists && existing == 0 {
			userTables, err := sqliteutil.UserTableCount(tx)
			if err != nil {
				return fmt.Errorf("classify fresh jobs database: %w", err)
			}
			if userTables != 0 {
				return fmt.Errorf("unknown unversioned jobs schema: found %d unrelated tables; restore a backup or move the database aside to recreate it", userTables)
			}
			for _, statement := range jobsV2BootstrapStatements {
				if _, err := tx.Exec(statement); err != nil {
					return fmt.Errorf("bootstrap jobs schema: %w", err)
				}
			}
			if _, err := tx.Exec(`INSERT INTO jobs_v2_schema_version(id, version) VALUES(1, ?)`, jobsV2SchemaVersion); err != nil {
				return fmt.Errorf("publish jobs schema version %d: %w", jobsV2SchemaVersion, err)
			}
			return nil
		}
		if existing != len(tableNames) {
			return fmt.Errorf("unknown legacy jobs schema: found %d of %d required jobs tables; restore a backup or move the database aside to recreate it", existing, len(tableNames))
		}

		currentVersion := 0
		if markerExists {
			currentVersion, err = readJobsV2Version(tx)
			if err != nil {
				return fmt.Errorf("read locked jobs schema marker: %w", err)
			}
			if currentVersion > jobsV2SchemaVersion {
				return fmt.Errorf("jobs database schema version %d is newer than supported version %d", currentVersion, jobsV2SchemaVersion)
			}
			if currentVersion == jobsV2SchemaVersion {
				return nil
			}
		}

		for _, migration := range jobsV2Migrations {
			if migration.version <= currentVersion {
				continue
			}
			if err := migration.up(tx); err != nil {
				return fmt.Errorf("jobs migration %d (%s), prior version remains safely committed: %w", migration.version, migration.description, err)
			}
			if !markerExists {
				if _, err := tx.Exec(jobsV2MarkerSchema); err != nil {
					return fmt.Errorf("create jobs schema marker: %w", err)
				}
				if _, err := tx.Exec(`INSERT INTO jobs_v2_schema_version(id, version) VALUES(1, ?)`, migration.version); err != nil {
					return fmt.Errorf("publish jobs migration %d (%s): %w", migration.version, migration.description, err)
				}
				markerExists = true
			} else if _, err := tx.Exec(`UPDATE jobs_v2_schema_version SET version = ? WHERE id = 1`, migration.version); err != nil {
				return fmt.Errorf("publish jobs migration %d (%s): %w", migration.version, migration.description, err)
			}
			currentVersion = migration.version
		}
		if currentVersion != jobsV2SchemaVersion {
			return fmt.Errorf("jobs migrations ended at version %d, want %d", currentVersion, jobsV2SchemaVersion)
		}
		return nil
	})
}

// execJobsV2Schema remains a test helper for callers that need a complete
// current database. Production startup uses initJobsV2Schema directly.
func execJobsV2Schema(db *sql.DB) error {
	return initJobsV2Schema(context.Background(), db)
}
