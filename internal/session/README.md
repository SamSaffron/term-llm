# Steering persistence and ownership

`internal/session` owns steering persistence, operation fencing, recovery state, and the session-side ownership boundary. User-facing steering documentation lives in the [usage guide](https://term-llm.com/guides/usage/) and the [web UI and API guide](https://term-llm.com/guides/web-ui-and-api/).

## Ownership and recovery

Server operations are session-scoped and keyed by a distinct stable request ID. Retrying the same ID returns the same immutable source and batch, including terminal results. Do not construct a fallback `/responses` request from rush-owned entries after a transport error.

SQLite stores accepted FIFO sequence, provenance and exclusive ownership, plus an operation ledger with full recovery payload. Initial input, pending-row removal and committed operation dispositions share one transaction. Stop and initial-input authorization compete through a fenced operation CAS. Database/HTTP errors before durable admission do not cancel the source.

Lock order is session admission boundary → runtime steering mutation guard → engine callback mutex. Admission installs the engine freeze before releasing these short locks for database I/O. No provider cancellation or settlement wait occurs under them. The published transition reservation blocks competing starts and session mutations during the gap.

The source's actual worker/persistence completion and active-session ownership release precede replacement admission. On restart, ambiguous operations are reported as blocked (`settlement_unknown`) rather than automatically replaying external work. Authorized state includes the latest operation, and operation lookup retains terminal metadata and recovery payload until session deletion. This has a retention/privacy cost: session deletion removes the ledger via foreign-key cascade.

The terminal manager owns the same freeze and settlement handoff. Without a session store, recovery is process-local: **Pending steering recovery is available only while this session is open.**

## Database upgrade / downgrade

Migration 56 renames the pending table/index, backfills deterministic historical `(created_at,id)` order, and adds monotonic acceptance sequence/provenance/ownership. Historical order backfill is best effort; it cannot recover true acceptance order from timestamp ties. Migration 57 adds the rush ledger. Migrations preserve pending structured payloads and foreign-key cascades. Fresh and upgraded schemas are checked for equivalence.

Do not run old and new binaries concurrently against this database. Older binaries are not supported after schema upgrade. Back up the database before upgrading; downgrade by stopping the server and restoring the backup, not by a destructive reverse migration.

