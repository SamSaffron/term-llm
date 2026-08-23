# SQLite migrations

SQLite schema versions are authoritative on normal read-write startup. A current database must take a marker-only fast path; physical schema inspection is reserved for migration slow paths and explicit diagnostics.

## Adding a migration

1. Update the canonical fresh schema.
2. Increment the store's target version.
3. Append exactly one ordered migration with a positive version, a non-empty description, and a non-nil callback.
4. Add or update a faithful fixture from a released schema. Label synthetic missing-object fixtures as synthetic.
5. Compare fresh and migrated schemas with `sqliteutil.SchemaSignature`.
6. Test representative data preservation, callback failure rollback, and retry from the previous marker.
7. Write the marker only in the same `BEGIN IMMEDIATE` transaction as the change it publishes.
8. Never add startup reconciliation in place of a versioned migration.
9. Consider whether the immediately previous binary can still read the migrated file, and document any mixed-binary boundary.

Migration lists must be checked with `sqliteutil.ValidateMigrations` in package tests. Slow paths acquire `sqliteutil.WithImmediateMigrationTx`, then re-read their marker and schema state on that locked connection before writing. A future marker is rejected before writes. Released migration numbers and meanings are immutable; production repairs always use a new version.

## Common changes

### Add a column

Put the column in the canonical `CREATE TABLE`. In the migration, call `sqliteutil.ColumnExists` and issue an explicit `ALTER TABLE ... ADD COLUMN` only when absent. Validate known legacy variants; do not swallow arbitrary “already exists” errors.

### Add an index

Put the index in the canonical schema and create it in the migration that owns its first published version. A current startup must not run `CREATE INDEX IF NOT EXISTS` as reconciliation.

### Change a trigger

Keep released trigger SQL frozen. A new migration drops/recreates the trigger and performs any required data backfill transactionally. The canonical fresh schema contains the latest definition.

### Backfill data

Perform the backfill in the same migration transaction as its schema change and marker update. Test meaningful old rows, defaults, row counts, and rollback—not only object existence.

### Rebuild a table

Use version-named, immutable table SQL. Create the replacement table, copy an explicit column list, verify behavior in tests, restore version-owned indexes/triggers, and drop the old table inside one transaction. Include rollback and content-preservation coverage. Never point a historical rebuild at a mutable canonical table definition.

## Mixed-binary rollout notes

- Session v49 adds only the singleton marker key and reconciles already-supported tables, indexes, and triggers. The immediately previous binary's `SELECT version`/`UPDATE schema_version SET version = ?` statements remain valid; that binary may still accept v49 because it predates future-version rejection.
- Memory v12 canonicalizes the existing tables and owns the already-supported vector index. Its key/value marker remains readable by the previous binary.
- File-history v3 adds a singleton marker key while preserving `SELECT version` and whole-table `UPDATE` compatibility.
- Jobs v1 adopts the existing jobs-v2 shape and adds a marker unknown to older binaries; the reconciled columns and indexes remain compatible with the immediately previous jobs code.

A process running an older binary concurrently with a new migrator is not granted write coordination beyond SQLite locking. Deploy one write-capable version at a time during these first marker transitions.

## Errors and operations

Migration errors should identify the store, observed and supported versions, migration version and description, and failed operation. Callback failures leave the previously committed marker safe. Lock acquisition uses the configured SQLite `busy_timeout`; expiry returns a retryable contextual error rather than spinning indefinitely.

Unknown unversioned schemas fail closed with a structural description and recovery guidance, without user data. Full corruption checking belongs in a future explicit doctor command and must not be added to normal startup.
